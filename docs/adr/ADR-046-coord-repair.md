# ADR-046 — EC repair on coord (Season 6 Ep.4)

상태: Accepted (2026-04-27)

## 결정

ADR-044/045 와 같은 패턴으로 EC repair 를 coord 로:

- `POST /v1/coord/admin/repair/plan` — `repair.ComputePlan(s.Coord, s.Store)`
- `POST /v1/coord/admin/repair/apply?concurrency=N` — plan + `repair.Run(...)` (Reed-Solomon Reconstruct + missing shard 재배포)

`s.Coord` 가 이미 `repair.Coordinator` 인터페이스 (DNs / PlaceN / ReadChunk / PutChunkTo) 만족.

cli `repair --plan/--apply --coord URL`. apply 는 K survivors 에서 reconstruct 후 누락 shard 를 다시 PUT.

## 결과

+ EC repair 의 single source = coord. ADR-025 의 worker 가 그대로 옮겨감.
+ Season 6 worker 트랙 3건 모두 같은 패턴 (rebalance / GC / repair) 으로 정리됨 — coord 가 실제 worker host.
- edge 의 repair endpoints 잠시 중복 보존.

## 검증

데모 demo-kaf.sh (히브리 כ, 11번째 letter): EC (4+2) PUT, DN 2개 kill (= 2 shards 손실, K survivors 정확히 4), cli `repair --apply --coord` → 누락 shard reconstruct + 재배포.
