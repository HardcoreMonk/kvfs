# Episode 36 — Season 6 시작: rebalance plan on coord

> **Series**: kvfs — distributed object storage from scratch
> **Season**: 6 (operational migration) · **Episode**: 1
> **연결**: [ADR-043](../docs/adr/ADR-043-coord-rebalance-plan.md) · [demo-chet](../scripts/demo-chet.sh)

---

## S6 의 정체

S5 끝 시점 (Ep.35) 의 그림: coord 가 메타·placement·HA 의 owner. 그러나
**operational admin** 은 여전히 edge 가 host:

- rebalance / GC / repair worker — `internal/{rebalance,gc,repair}/` 가 edge 의
  `s.Coord` 위에서 돈다
- DN registry mutation (`POST /v1/admin/dns`) — edge 의 admin
- URLKey rotation — edge 의 admin

cli 가 admin 하려면 edge 통과. layer 어색함.

**Season 6 = 이 두꺼운 책임들을 coord 로 이전**. 7 episodes (Hebrew chet ח ~
nun נ).

## plan 부터, apply 는 다음

Rebalance 의 자연 분리: plan-then-apply.

- **Plan** — 현재 placement 와 ideal placement 의 diff. 순수 계산. DN I/O 0.
- **Apply** — plan 따라 chunk copy. DN HTTP 필요.

이 ep (Ep.1) 가 **plan 만** 옮긴다. apply 는 Ep.2 (ADR-044). 분리 이유: plan
은 placement 결정만 필요하고 그건 이미 coord 책임. DN I/O 라는 새 위험 (coord
가 DN HTTP 클라이언트 host) 는 다음 ep 에서.

## 핵심: rebalance.Coordinator 인터페이스

`internal/rebalance/` 가 cluster 와 대화하는 방식:

```go
type Coordinator interface {
    DNs() []string
    QuorumWrite() int
    PlaceChunk(chunkID string) []string
    PlaceN(key string, n int) []string
    PlaceNFromAddrs(key string, n int, addrs []string) []string
    ReadChunk(...) ([]byte, ...)
    PutChunkTo(...) error
}
```

plan 단계는 **Place* 메소드만** 호출. ReadChunk/PutChunkTo 는 apply 가 사용.

ADR-043 의 `rebalancePlanCoord` 어댑터:

```go
type rebalancePlanCoord struct { coordPlacer *placement.Placer; store ... }

func (a *rebalancePlanCoord) PlaceChunk(id string) []string {
    return a.coordPlacer.PlaceN(id, 3)  // R from store config
}
func (a *rebalancePlanCoord) PlaceN(key, n) []string { ... }
func (a *rebalancePlanCoord) ReadChunk(...) error { return errors.New("not supported") }
func (a *rebalancePlanCoord) PutChunkTo(...) error { return errors.New("not supported") }
```

ReadChunk/PutChunkTo 는 명시적 에러 — apply 시도 시 분명한 신호. plan 만 OK.

## endpoint

```
POST /v1/coord/admin/rebalance/plan
```

cli:
```
$ kvfs-cli rebalance --plan --coord http://coord1:9000
```

`--coord` 와 `--edge` 는 mutually exclusive. apply 는 Ep.2 까지는 여전히 `--edge`.

## ח chet 데모

```
$ ./scripts/demo-chet.sh
==> setup: 3 DN + coord + edge
==> PUT 5 objects
==> add 4th DN (dn4) to runtime registry via cli (still edge endpoint)
==> POST /v1/coord/admin/rebalance/plan
    plan: 7 chunks misplaced
      chunk_id abc... currently {dn1,dn2,dn3} → ideal {dn4,dn1,dn2}
      ...
==> verify same plan via edge (legacy path)
    plan: 7 chunks (identical)
=== ח PASS: rebalance plan computed by coord matches edge plan ===
```

evidence: 같은 cluster state 에서 coord-computed plan == edge-computed plan.
**계산 위치만 옮기고 결과 동일** — 분리의 정상 조건.

## 두 측면의 미래

ADR-043 의 + 항목:
- single-source plan 으로 multi-edge drift 방지
- ADR-015 의 read-side 분리 일관성 확장

- 항목:
- edge 의 rebalance endpoint 잠시 중복 (legacy path 보존)
- apply 는 다음 ep 에서 — 그때까지 cli 는 plan 만 coord, apply 는 edge 통과하는
  과도기

이런 점진성이 분산 시스템 변경의 미덕. 한 번에 모두 옮기지 않고 단계별 검증.

## 다음

Ep.37 — coord 에 DN I/O 노출. ADR-044. `COORD_DN_IO=1` env. coord 가 비로소
chunk 를 직접 copy. apply 가 coord 로 옮겨감. demo-tet (ט).
