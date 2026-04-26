# ADR-044 — Rebalance apply on coord (Season 6 Ep.2)

상태: Accepted (2026-04-27)

## 배경

ADR-043 (Ep.1) 가 plan 만 옮겼다. apply 는 chunk Read/Put 이 필요해서 coord 가 DN HTTP 클라이언트를 가져야 함. 본 ADR 이 그것을 한다.

## 결정

`coord.Server.Coord *coordinator.Coordinator` 신규 필드 (옵셔널). `COORD_DN_IO=1` env 로 활성. 활성 시 coord daemon 이 자기 DN list 위에 `coordinator.New(...)` 인스턴스 생성 — 기존 edge 의 coordinator 와 같은 객체.

새 endpoint:
- `POST /v1/coord/admin/rebalance/apply?concurrency=N`
- Coord 가 nil 이면 503 (apply 불가)
- 그렇지 않으면 `rebalance.ComputePlan(s.Coord, store, store)` + `rebalance.Run(ctx, s.Coord, store, plan, N)` 그대로

`handleRebalancePlan` 도 Coord 가 있으면 그쪽을 사용 (이전엔 placer-only adapter 였음). 이유: plan 의 `PlaceChunk` 가 `Coordinator.PlaceChunk` 와 100% 동일한 답을 줘야 apply 가 그 답대로 옮길 수 있음 — 한 객체 사용으로 일관성 보장.

cli `kvfs-cli rebalance --apply --coord URL` — `--apply --edge` 와 동등.

## 비채택 대안

- coord 가 plan 만 하고 edge 가 apply: 이미 Ep.1 의 모드. Ep.2 의 목적은 그 cross-daemon 호출 제거.
- coord-as-master 만, edge 가 영구 cli proxy: 너무 큼. ADR-015 의 점진적 분리 정신.

## 결과

+ rebalance 의 plan + apply 가 하나의 daemon (coord) 위에서 — view 일관성 강. cli `--apply --coord` 로 운영자가 cross-host 운영.
+ coord 가 DN I/O 가질 수 있음을 입증 — Ep.3/4 (GC, repair) 가 같은 인프라 재사용.
- Edge 가 같은 코드 path 보유 (`/v1/admin/rebalance/apply`) — 잠시 중복. coord-proxy 모드 운영 안정화 후 정리.
- coord 가 DN 들을 직접 알아야 함: `COORD_DNS` 가 정확해야. 부정확 시 잘못된 DN 호출. operator 책임.

## 검증

내부: `coordinator.Coordinator` 의 정상성은 Season 1+2 에서 이미 입증됨. coord 안에서 같은 객체를 빌드만 하면 됨.

데모: `scripts/demo-tet.sh` — 4 DN + coord with `COORD_DN_IO=1`. 일부러 misplaced 객체 만든 후 cli `rebalance --apply --coord` 한 번 호출 → `rebalance --plan --coord` 가 0 migrations 반환 (cluster 정렬됨).
