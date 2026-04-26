# ADR-048 — URLKey kid registry on coord (Season 6 Ep.6)

상태: Accepted (2026-04-27)

## 결정

ADR-047 (Ep.5) 와 같은 패턴으로 URLKey kid registry 를 coord 로:

- `GET    /v1/coord/admin/urlkey` — list (open, follower OK)
- `POST   /v1/coord/admin/urlkey/rotate` body `{kid, secret_hex, is_primary}` — leader-only
- `DELETE /v1/coord/admin/urlkey?kid=X` — leader-only, primary kid 거부 (Store-side check)

`MetaStore.PutURLKey / DeleteURLKey / ListURLKeys` 가 이미 존재 — coord 가 그대로 호출. WAL 에 기록되어 ADR-039 sync 로 follower coord 에 자동 전파.

cli `kvfs-cli urlkey list/rotate/remove --coord URL`. 기존 `--edge` 와 평행. List response shape 두 개 지원 (edge `{kids, primary}` vs coord `[]URLKeyEntry` raw) — cli 가 자동 분기.

## 명시적 비포함 (= 후속 ADR 또는 Season 7)

> **Edge 의 in-memory Signer 갱신 미포함.**

coord-proxy 모드에서 edge 가 URLKey 를 verify 하려면 자기 in-memory `urlkey.Signer` 가 새 키를 알아야 한다. 본 ADR 은 **registry 의 owner** 만 옮긴다 — edge 가 그 변경을 어떻게 따라잡는지는 별도 작업:

옵션 후보:
1. **Edge 가 coord 에 polling** (e.g. 30s 마다 GET /v1/coord/admin/urlkey)
2. **WAL replication (ADR-039)** 에 URLKey op 추가 → follower edge 가 자동 ApplyEntry
3. **401 시 lazy refresh** — verify 실패하면 한 번 coord 에서 가져와 retry

운영 use case 결정 필요. 현재 edge 는 자기 `EDGE_URLKEY_SECRET` env 또는 `urlkey_secrets` bbolt 버킷에서 boot 시점 로드 — coord-proxy 모드에서는 이 bbolt 가 비어있음.

요약: 본 ADR 은 cli admin track 의 마지막 piece (kid registry mutation 이 coord 에서 됨), edge 가 그 변경을 propagation 하는 mechanism 은 다음 작업.

## 비채택 대안

| 대안 | 기각 사유 |
|---|---|
| 본 ADR 에서 edge propagation 까지 같이 | 위에 적은 옵션 결정이 운영 use case 의존. 분리해서 박는 게 깨끗 |
| primary kid 자동 결정 (timestamp-based) | 현재 cli 가 `--primary=true` default 로 명시적. 같은 secret 으로 rotate 시 의도 모호 — 명시 유지 |
| Rotate 가 자동 random secret 생성 | cli 가 `--secret-hex` 빈 시 32-byte random 생성 — operator 가 명시적으로 control 가능. 서버 side 자동 생성은 audit 어려움 |

## 결과

+ Cli admin track 완성: inspect / rebalance / gc / repair / dns / urlkey 모두 `--coord URL` 지원
+ HA mode 일관성 자동 (WAL → follower coord)
+ 코드 변경 ~80 LOC + 1 unit test
- Edge 의 actual key sync 는 후속 (P7-09 예상)
- Edge admin endpoints 잠시 중복

## 검증

`internal/coord/coord_test.go::TestAdminURLKeyRotateListRemove` — rotate v1 → rotate v2 (v1 primary cleared) → list (1 primary) → remove v1 (200) → remove v99 (404).

데모 demo-mem.sh (히브리 מ, 13번째 letter): coord-only cluster. cli `urlkey rotate --coord` → list 에 새 kid 표시 → remove 동작 검증. **Edge 의 actual signing key 갱신은 본 demo 범위 외** — 명시적으로 노트.
