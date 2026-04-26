# ADR-041 — Edge routes placement decisions through coord

상태: Accepted (2026-04-27)
시즌: Season 5 Ep.6 · ADR-015 의 마지막 분리 단계

## 배경

ADR-015 (S5 Ep.1) 가 coord daemon 을 만들고 그 후 Ep.2~5 가 메타 commit/lookup/delete + HA + transactional commit 를 모두 coord 로 옮겼다. **placement 결정만 edge 인라인으로 남아있었다** — `/v1/coord/place` RPC 는 Ep.1 부터 존재했지만 edge 가 호출 안 했음.

문제:
- 멀티-edge 환경에서 각 edge 의 local placement 가 자기 DN list 에 의존 → topology drift 시 분기 가능
- edge 가 cluster 의 진실 (DN 목록) 을 알아야 함 → coord 의 single source of truth 원칙 미완성
- ADR-015 의 책임 분리 일관성 깨짐 (메타는 coord, placement 는 edge)

## 결정

`edge.CoordClient.PlaceN(ctx, key, n) ([]string, error)` 신규. coord 의 `/v1/coord/place` RPC 호출.

`Server.placeN(ctx, key, n)` helper 분기:
- `CoordClient` set → RPC 위임
- `CoordClient` nil → 기존 `s.Coord.PlaceN(...)` (인라인)

handler 변경:
- `writeChunkPreferClass`: 기존 `s.Coord.WriteChunk` (placement 인라인) → `placeN` + `WriteChunkToAddrs` (placement 분리)
- `handlePutECStream` 의 stripe placement: `s.Coord.PlaceN(stripeID, K+M)` → `placeN(ctx, stripeID, K+M)`

class subset placement (`PlacementPreferClass != ""`) 은 local 유지 — class 라벨 데이터 자체가 local store 에 있고 coord 가 아직 class-aware RPC 를 노출하지 않음. 후속 ep 의 일.

## 결과

+ **Single source of truth 완성**: 모든 placement 결정이 coord 한 곳에서. edge 가 멀티 인스턴스로 떠도 같은 chunk 는 같은 DN 으로 감.
+ **Topology drift 자동 해소**: coord 가 DN registry 의 진실. edge 가 `EDGE_DNS` 잘못 설정해도 placement 는 coord 가 결정.
+ **Edge 의 placement 코드 제거 가능 (미래)**: 본 ADR 에서는 `s.Coord` (=`internal/coordinator`) 의 placement 부분이 fallback 으로 남음 (`CoordClient nil` 시). 모든 운영이 coord-proxy 모드로 안정화되면 fallback 도 제거 가능.
- **Per-chunk placement RPC**: 매 chunk write 가 coord 에 1 RTT 추가. 큰 객체 (N chunks) PUT 은 N 번. mitigation: future ep 에서 batch PlaceN RPC 또는 short-TTL placement cache.
- **Edge 의 `EDGE_DNS` 가 더 이상 placement 에 영향 안 줌** (coord-proxy 모드에서). DN 목록은 여전히 chunk I/O target 으로 필요하지만 placement 는 coord 의 `COORD_DNS`. 운영자가 두 list 를 같게 유지해야 함 — 이는 coord 가 DN registry 를 직접 expose 하면 (다른 ep) 자연 해소.

## 비채택 대안

| 대안 | 기각 사유 |
|---|---|
| Edge 가 placement.Placer 완전히 제거 (Coord wrapper 도) | DN I/O target list 가 필요. Coord wrapper 의 PlaceNFromAddrs (class subset) 도 아직 사용됨 |
| Per-call placement cache (short TTL) | premature optimization. measurements 없이 캐시 도입 안 함 — kvfs는 micro-opt 보다 단순함 우선 |
| Batch PlaceMultiple RPC | useful for 큰 객체 (N chunks 한 번 RPC) 지만 streaming chunker 가 chunk 당 즉시 fanout 해야 해서 batching 하려면 chunker.Reader 의 lookahead 필요. 별도 ADR |

## 호환성

- `CoordClient nil` (기본 = legacy 인라인 모드) → 동작 0 변경.
- `CoordClient set` 인데 coord 의 `/v1/coord/place` 가 응답 안 하면 (네트워크/coord down) → handler 가 502/503 반환. 기존 인라인 모드는 placement 가 절대 fail 하지 않았음 — 이제 coord 가 SPOF 의 가능성 (HA mode 안 켜면). 하지만 commit/lookup 이 이미 coord 에 의존하고 있어 같은 risk.
- 모든 기존 demo (alpha~omega + ψ) 는 EDGE_COORD_URL 미설정 → 기존 path 그대로.

## 검증

`internal/edge/coord_client_test.go`:
- `TestCoordClient_PlaceN_ReturnsCoordsView` — coord 가 dn1/2/3 만 알고 있을 때 PlaceN 이 그 셋만 반환. 다른 addr 은 절대 안 옴 (proves the call hits coord, not edge's local placer).

`scripts/demo-vav.sh` (히브리 ו): COORD_DNS 가 dn1/2/3, EDGE_DNS 가 dn1/2/3/4. 차이가 결정적: edge inline 모드면 dn4 가 placement 후보, coord-proxy 모드면 NEVER. 객체 PUT → meta 의 chunk replicas 에 dn4 가 등장하지 않음 = coord 가 결정한 증거.

## 후속

- **P6-06**: kvfs-cli 가 coord 에 직접 admin (DN registry, class label) — coord 가 DN list 의 진실인데 cli 가 edge 통해 가는 것은 layer 위반.
- Class-aware placement 의 coord-side port (현재 local fallback). 큰 변경 아님 — `coord.PlaceFromAddrs` RPC 추가하면 끝.
- Placement cache (저우선) — measurements 보고.
