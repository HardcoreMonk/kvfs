# ADR-049 — Edge urlkey.Signer propagation from coord (Season 6 Ep.7)

상태: Accepted (2026-04-27)

## 배경

ADR-048 (Ep.6) 가 URLKey 의 registry mutation 을 coord 로 옮겼다. 그러나 edge 의 in-memory `urlkey.Signer` 는 boot 시점 의 키 set 그대로 — coord 의 새 kid 는 edge 가 모름. coord-proxy 모드의 진정한 cli admin 완성을 위해 propagation 메커니즘이 필요.

ADR-048 본문 "비포함" 섹션 + FOLLOWUP P7-09 가 이 작업을 등록.

## 결정 — Polling

가장 단순한 접근: edge 가 `EDGE_COORD_URL` 설정 시 background goroutine 으로 주기적 (`EDGE_COORD_URLKEY_POLL_INTERVAL`, default 30s) coord 의 `/v1/coord/admin/urlkey` 호출 → 결과를 `Signer` 와 diff → Add/Remove/SetPrimary 적용.

`SyncURLKeys(signer, coord, failFn)` 가 unit-testable core. `Server.urlKeyPoller` 가 Ticker 위에서 호출. ctx 취소로 깨끗하게 종료.

```
EDGE_COORD_URL=http://coord:9000
EDGE_COORD_URLKEY_POLL_INTERVAL=30s
  ↓
edge boot →  coordClient.ListURLKeys() at every tick
            → SyncURLKeys diff:
               - new kid in coord, not in signer  → Add
               - kid in signer, not in coord       → Remove (skip primary, last)
               - primary changed                   → SetPrimary
```

## 명시적 비포함 (지금은 의도적, 후속 ep 가능)

- **Lazy 401-refresh**: verify 실패 시 즉시 한 번 sync + retry. 더 빠른 propagation 이지만 401 간 race condition + 리트라이 budget 복잡도. polling 으로 충분한 use case 가 90%+.
- **Edge 가 secret_hex 를 디스크에 캐시**: 매 부트 시 coord 에서 새로 받음 — coord down 시 edge 는 boot 시점 EDGE_URLKEY_SECRET 만으로 동작. 캐시는 misconfig 위험 (stale secret) 대비 미흡.
- **WAL replication 으로 키 propagation**: ADR-039 의 mechanism 재사용 가능. 단 edge 가 WAL apply 인프라 없음 (읽기 전용 follower coord 와 다름). 별도 작업.

## 구현 디테일

`SyncURLKeys` 의 순서:
1. 새 kid 들 Add (디코드 실패면 callback 으로 보고, 진행)
2. SetPrimary (Remove 가 primary 거부하므로 먼저)
3. Remove disappeared kids (`ErrPrimaryRemove` / `ErrLastKidRemove` 는 무시 — 안전장치)

Idempotent: 같은 coord state 로 재호출 시 변경 0, 에러 0.

Polling interval 의 lag = "rotation visible to edge" 의 RPO. 30s 면 운영 user 가 `cli urlkey rotate` 후 30s 안에 새 키로 서명한 URL verify 가능. 짧게 하면 coord rate ↑.

## 비채택 대안

| 대안 | 기각 사유 |
|---|---|
| edge 가 coord 에 SSE/long-poll | 양방향 connection 유지 — 운영 복잡도. polling 의 30s lag 가 acceptable |
| edge 가 자기 bbolt 에 URLKey 캐시 + WAL pull | edge.bbolt 가 coord-proxy 모드에서 unused. WAL pull 만으로 부분 복구하려면 인프라 ↑ |
| Push from coord (coord → edge POST) | edge 가 coord 의 client 인 패턴 깨짐. 현재 모든 outbound 는 edge → coord |

## 결과

+ Edge 가 coord 의 키 변경을 30s 안에 반영. 운영자가 `cli urlkey rotate --coord` 후 새 URL 즉시 사용 가능 (대부분의 경우).
+ Polling 만 — 추가 인프라 0. CoordClient 에 메서드 한 개 + edge 에 goroutine 한 개.
+ ADR-048 + 본 ADR 로 cli admin track 의 진정한 완성: registry + propagation 모두 자동.
- **30s lag** — fastest propagation 이 needed 한 use case 는 별도 옵션 검토 필요.
- **Coord down 시 edge 는 boot 시점 키로 동작**: signer 가 stale 가능. acceptable (coord down 자체가 더 큰 문제).

## 검증

`internal/edge/urlkey_sync_test.go`:
- `TestSyncURLKeys_AddSwapPrimaryRemove` — 보트 v1 → coord 가 v2 primary 추가 + v1 demoted → sync 후 signer 도 그렇게. v1 제거 후 sync → signer 도 v1 제거. Idempotent 검증.
- `TestSyncURLKeys_PreservesLastKid` — coord 가 빈 list 반환해도 signer 가 마지막 kid 유지 (Signer 의 `ErrLastKidRemove` 안전장치).

데모 `demo-nun.sh` (히브리 נ, 14번째 letter): coord + edge proxy. 작은 polling interval (3s) 로 설정. cli `urlkey rotate --coord` 후 ~3s 안에 edge.signer 에 새 키 등장 확인.

## 후속

- **ADR-050** (예상): Lazy 401-refresh 가 운영 needs 가 명확해질 때.
- **운영 측정**: polling 빈도 vs coord rate 의 trade-off 측정 후 default tuning.
