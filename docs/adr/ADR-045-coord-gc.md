# ADR-045 — GC plan + apply on coord (Season 6 Ep.3)

상태: Accepted (2026-04-27)

## 결정

ADR-044 의 `Coord` 필드 위에 GC 추가:

- `POST /v1/coord/admin/gc/plan?min-age=DURATION` — `gc.ComputePlan(ctx, s.Coord, s.Store, minAge)`
- `POST /v1/coord/admin/gc/apply?min-age=DURATION&concurrency=N` — plan + `gc.Run(ctx, s.Coord, plan, N)`

`s.Coord` 은 이미 `gc.Coordinator` 인터페이스 (DNs / ListChunks / DeleteChunkFrom) 만족.

cli `gc --plan/--apply --coord URL`. 기존 edge endpoint 와 평행.

## 비채택 대안

- coord 가 GC 만 하고 apply 는 background worker scheduler — 불필요한 복잡도. apply 가 이미 pull-based + plan-then-execute.

## 결과

+ GC 의 single source = coord (rebalance 와 동일)
+ ADR-044 의 DN I/O 인프라 재사용 — 새 코드 ~80 LOC
- edge 의 GC 코드 잠시 중복 (rebalance 와 동일 정리 패턴)
- min-age 를 edge 는 `min_age_seconds=60` (int), coord 는 `min-age=60s` (duration) 로 받음. cli 가 자동 변환

## 검증

데모 `demo-yod.sh` (히브리 י, 10번째 letter): 객체 PUT → 일부 chunk surplus 만들기 (DELETE meta 만, disk 남김) → cli `gc --plan --coord` → `gc --apply --coord` → re-plan empty.
