# ADR-043 — Rebalance plan computed on coord (Season 6 Ep.1)

상태: Accepted (2026-04-27)
시즌: **Season 6 — coord operational migration** (open)

## Background — Season 6 의 시작

Season 5 (ADR-015) 가 메타·placement 의 read 측을 coord 로 옮겼다. mutating admin 과 worker (rebalance / GC / repair) 는 여전히 edge 가 host. 결과:

- edge 가 thin gateway 가 됐다고 광고하는데 admin 측면에서는 여전히 두꺼움
- rebalance plan 이 edge 의 placement view 로 계산됨 → 멀티-edge 시 view drift 가능
- coord 의 single-source 약속이 read 까지만

**Season 6** = 이 두꺼운 책임들을 coord 로 정리하는 트랙. plan-then-apply 분리가 자연스러우므로 plan 부터.

## 결정 (Ep.1)

`coord.Server` 가 `POST /v1/coord/admin/rebalance/plan` 를 노출. 내부 동작:

1. `rebalancePlanCoord` 어댑터 — coord 의 `Placer` 위에 `rebalance.Coordinator` 인터페이스를 충족. PlaceChunk / PlaceN / PlaceNFromAddrs 만 진짜 구현, ReadChunk / PutChunkTo 는 명시적 에러 (apply 경로 미지원)
2. `rebalance.ComputePlan(adapter, coord.Store, coord.Store)` 호출 — coord 의 MetaStore 가 ObjectStore + ClassResolver 인터페이스 둘 다 만족
3. 결과 plan 을 JSON 응답

cli `kvfs-cli rebalance --plan --coord URL` — 기존 `--edge` 와 mutually exclusive. apply 는 여전히 `--edge` (worker 가 edge 에 있음).

## 명시적 비포함 (= Ep.2)

- **apply (실제 chunk copy) 는 edge** — coord 가 DN I/O 를 안 가짐. apply 를 coord 로 옮기려면 coord 가 `internal/coordinator` 인스턴스 (DN HTTP 클라이언트) 를 들어야 함. 큰 변화라 별도 ep.
- **GC, repair plan 의 coord 이전** — 같은 패턴. 의도적으로 plan 만 분리해서 한 번에 하나씩 진행.

이 분할은 한 ADR 의 변경을 작게 유지 + 각 단계의 검증 가능성을 명확히.

## Why Plan Only Pays Off Already

- **드리프트 검증**: 멀티-edge 환경에서 두 edge 가 서로 다른 plan 을 계산할 때 어느 게 옳은지 알 수 없음. coord 의 plan 이 기준점 — operator 가 `--coord` 로 호출하면 진실.
- **CLI host 자유도**: 운영자가 edge filesystem 에 접근할 필요 없음. coord URL 만 알면 plan 을 받음.
- **Pattern 박힘**: GC / repair plan 도 같은 모양 — 다음 ep 들이 mechanical.

## 비채택 대안

| 대안 | 기각 사유 |
|---|---|
| coord 가 plan + apply 한 번에 | 큰 변화 (DN I/O 도입) — 검증 어려움. 단계별로 |
| edge 가 coord plan 호출해서 캐시 | edge 가 plan 의 owner 가 아닌데 캐시 책임 — layer 위반 + invalidation 복잡 |
| Plan 응답에 ETag/version, cli 가 캐시 | YAGNI. plan 은 작고 stateless |

## 결과

+ rebalance plan 의 single source = coord
+ ADR-042 의 read-only inspect 패턴 정확히 따름 (`/v1/coord/admin/*` + cli `--coord` flag)
+ Edge 의 plan 코드는 그대로 — 코드 중복이지만 fallback / debugging 도구로 유지 (Season 6 끝날 때 정리)
- **Apply 미이전**: cli 가 plan 은 coord 로, apply 는 edge 로 — 약간 어색. operator 가 두 base URL 다 알아야 함. Ep.2 에서 통합
- **Replication factor 하드코딩**: 어댑터의 `rebalancePlanCoord.PlaceChunk` 가 `defaultR=3` 사용. coord 가 R 을 env 로 받지 않음 (현재 EDGE_REPLICATION_FACTOR 만 존재). 후속 ep 가 `COORD_REPLICATION_FACTOR` 추가하거나, coord 가 edge 와 같은 env 공유

## 호환성

- 새 endpoint 추가만, 기존 사용자 영향 0
- cli `--coord` 는 새 flag — 미설정 시 기존 `--edge` 경로 동일

## 검증

`internal/coord/coord_test.go::TestRebalancePlan_DetectsMisplacedChunk` — 5 DN, R=3, 단일 replica 로 seed → plan 이 ≥1 migration 반환. set-based 비교 아니라 missing 보장.

`scripts/demo-chet.sh` (히브리 ח, 8th letter): coord + edge proxy + 4 DN. PUT 객체 → coord 가 dn4 추가됨 (runtime registry) → cli `rebalance --plan --coord` 가 migration 반환. cli `rebalance --plan --edge` 도 호출해서 두 plan 의 migration 수 비교 (둘 다 같은 placer 면 일치).

## 후속 (Season 6 episodes)

- **ADR-044** (Ep.2): rebalance apply on coord. coord 에 `internal/coordinator` 임베드 → DN I/O 가능.
- **ADR-045** (Ep.3): GC plan + apply on coord.
- **ADR-046** (Ep.4): repair on coord (EC reconstruct 위한 DN I/O 필요).
- **ADR-047+** (Ep.5+): urlkey rotation, dns admin 의 coord 이전.
