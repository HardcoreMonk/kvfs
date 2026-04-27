# Episode 34 — placement 결정도 coord 에

> **Series**: kvfs — distributed object storage from scratch
> **Season**: 5 (coord 분리) · **Episode**: 6
> **연결**: [ADR-041](../docs/adr/ADR-041-edge-placement-via-coord.md) · [demo-vav](../scripts/demo-vav.sh)

---

## 마지막 분리

Ep.29~33 이 메타 (commit/lookup/delete) + HA + transactional commit 을 모두
coord 로 옮겼다. 그런데 **placement 결정만** edge 인라인으로 남아 있었다.

- `/v1/coord/place` RPC 는 Ep.1 부터 존재
- 그러나 edge 가 호출 안 함. edge 의 인라인 `s.Coord.PlaceN(...)` 사용.

이번 ep 가 마지막 분리. edge 의 placement 코드 호출 자리를 RPC 로 교체.

## 왜 굳이

세 이유:

1. **Topology drift 방지**. multi-edge 환경에서 각 edge 가 자기 `EDGE_DNS` 보고
   placement 계산 → 두 edge 의 DN list 가 잠깐 다르면 같은 chunk 가 다른 DN
   세트로 갈 가능성.
2. **Single source of truth 완성**. 메타도, placement 도, DN registry 도 모두
   coord. ADR-015 의 책임 분리 일관성.
3. **운영 단순화**. coord 가 DN list 의 진실. edge 는 thin gateway.

## 코드 변경 — 한 헬퍼 + 두 호출 부위

`internal/edge/edge.go`:

```go
// Server.placeN — placement decision dispatcher
func (s *Server) placeN(ctx context.Context, key string, n int) ([]string, error) {
    if s.CoordClient != nil {
        return s.CoordClient.PlaceN(ctx, key, n)  // ADR-041, this ep
    }
    return s.Coord.PlaceN(key, n), nil  // legacy 인라인
}
```

호출 부위 두 곳:
- `writeChunkPreferClass` — replication mode 의 chunk placement
- `handlePutECStream` — EC mode 의 stripe placement

기존 `s.Coord.PlaceN(...)` 직접 호출이 `placeN(ctx, ...)` 헬퍼 경유로 바뀜.

## class 라벨은 잠시 local

`PlacementPreferClass` (hot/cold tier, [Ep.25](25-hot-cold-tier.md)) 의 placement
는 class subset 데이터에 의존하고, class subset 은 local store 에 있음. coord
가 아직 class-aware RPC 를 노출하지 않음.

→ 본 ADR 에서는 class subset placement 는 인라인 fallback 유지. 후속 ep 에서
coord 의 `/v1/coord/place` 가 class 파라미터 받게 확장.

이게 분할 정복의 자연스러운 사례. 한 번에 다 못 하면 우선 핵심 80% 옮기고,
나머지 20% 는 follow-up.

## ו vav 데모 — 결정적 evidence

데모의 영리함:

```
COORD_DNS=dn1:8080,dn2:8080,dn3:8080         ← coord 가 아는 3개
EDGE_DNS=dn1:8080,dn2:8080,dn3:8080,dn4:8080 ← edge 가 아는 4개 (1개 더)
```

```
$ ./scripts/demo-vav.sh
==> PUT /v1/o/vav/test
==> verify chunks placement
    chunk_id abc123... replicas: ["dn3:8080","dn1:8080","dn2:8080"]

==> assert dn4 is NEVER in placement (proof of routing)
    ✓ dn4 not in any chunk's replicas

=== ו PASS: coord governs placement (edge's DN view is irrelevant) ===
```

**dn4 가 절대 등장하지 않음**. edge 가 dn4 를 알고 있는데도. coord 가 결정한
증거.

만약 edge inline 모드라면 dn4 가 반드시 candidate. coord-proxy 모드면 NEVER.
이 차이가 architectural 분리의 실증.

## per-chunk RPC 의 비용

ADR-041 의 -항목:

> 매 chunk write 가 coord 에 1 RTT 추가. 큰 객체 (N chunks) PUT 은 N 번.

1 GiB 객체 (256 chunks of 4 MiB) 면 256 RTT. 로컬 LAN 1ms × 256 = 256ms 추가.

두 mitigation 후보:
- **Batch PlaceMultiple RPC** — N chunks 를 한 번에. streaming chunker 와의
  interplay 가 복잡 (lookahead 필요).
- **Short-TTL placement cache** — chunk_id 가 같으면 같은 결과 (HRW 가
  deterministic). per-chunk RPC 회피 가능.

ADR-041 는 둘 다 미채택. **measurement 없이 캐시 도입 안 함** — kvfs 의 단순함
우선 원칙.

P6-10 (블로그에는 안 다룸) 가 LookupObject 의 short-TTL 캐시는 도입 — placement
와 별개.

## Topology drift 의 실제

production 에서 발생할 수 있는 시나리오:

```
t=0  edge1, edge2 둘 다 EDGE_DNS=dn1,dn2,dn3
t=1  운영자가 dn4 추가. coord 는 dns_runtime 에 등록.
t=2  edge1 의 EDGE_DNS env 갱신 (재시작) — edge1 은 dn1,2,3,4
t=3  edge2 는 아직 미갱신 — edge2 는 dn1,2,3
t=4  같은 chunk_id 에 대해:
       edge1 placement → {dn4, dn2, dn3} (dn4 가 top-3 에 들어감)
       edge2 placement → {dn1, dn2, dn3}
     같은 chunk_id 의 데이터가 두 edge 가 다른 DN 으로 보냄 → divergence
```

ADR-041 후엔:
```
t=4  edge1 placement → coord.PlaceN → {top-3 from coord's DN list}
     edge2 placement → coord.PlaceN → 같은 결과
```

drift 자동 해소.

## Edge 의 EDGE_DNS 의 의미 변화

ADR-041 후, coord-proxy 모드에서:
- `EDGE_DNS` = chunk I/O target list (PUT 할 DN 주소)
- `COORD_DNS` = placement 결정의 input (어느 DN 들이 후보)

두 list 가 같아야 정상 동작. 다르면 placement 가 dn 을 골랐는데 edge 가
모르거나 (PUT 실패), edge 가 아는 DN 이 placement 에 안 와서 idle.

운영 부담? 지금 시점에서는 yes. ADR-041 자체 인정. 후속 ep (ADR-047) 가
DN registry 의 coord 이전 + cli 의 coord-direct admin 으로 자연 해소.

## 다음

Ep.35 — kvfs-cli 가 coord 에 직접 admin (read-only). ADR-042. cli 의 layer
violation 정리. demo-zayin (ז). S5 의 마지막 ep.

S5 가 끝나면 cluster 는 진짜 3-daemon — coord 가 모든 결정의 owner, edge 는
thin gateway, dn 은 byte container. ADR-015 의 분리 비전 완성.
