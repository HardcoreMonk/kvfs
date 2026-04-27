# Episode 37 — apply 도 coord 로: COORD_DN_IO 의 의미

> **Series**: kvfs — distributed object storage from scratch
> **Season**: 6 (operational migration) · **Episode**: 2
> **연결**: [ADR-044](../docs/adr/ADR-044-coord-rebalance-apply.md) · [demo-tet](../scripts/demo-tet.sh)

---

## plan 의 다음 — apply

Ep.36 가 plan 까지만. apply 는 chunk byte 를 옮겨야 해서 coord 가 DN HTTP
클라이언트를 host 해야 함.

DN HTTP 라는 새 책임 = 새 위험. ADR-044 의 trade-off: latency · 보안 · 실패
모드 전부 새로 검토. 그래서 **opt-in env** 로 도입.

```
COORD_DN_IO=1   # coord 가 chunk read/write 를 직접 함
                # 미설정 시 coord 는 plan 만 (Ep.1 그대로)
```

## coord.Server.Coord — 그 한 필드

```go
type Server struct {
    Store               *store.MetaStore
    Placer              *placement.Placer
    Elector             *election.Elector
    TransactionalCommit bool
    Coord               *coordinator.Coordinator   // ← Ep.2 신규
    ...
}
```

`internal/coordinator` 가 무엇을 들고 있나? quorum write fanout, DN HTTP
client, DN scheme (HTTP/HTTPS) — edge 의 인라인 coordinator 와 같은 객체. 한
번 잘 만든 패키지 재사용.

main.go:
```go
if *flagDNIO {
    dnCoord, err = coordinator.New(coordinator.Config{Nodes: nodes})
    if err != nil { fatal(...) }
    log.Info("kvfs-coord DN I/O enabled (Ep.2, ADR-044)")
}
srv := &coord.Server{ ..., Coord: dnCoord }
```

## endpoint

```
POST /v1/coord/admin/rebalance/apply?concurrency=N
```

`Coord == nil` (Ep.1 only) 이면 503 + "DN I/O not enabled". 명시적.

cli:
```
$ kvfs-cli rebalance --apply --coord URL
```

`--apply --edge` 와 동등 (legacy path).

## handlePlan 의 통합

Ep.1 의 `rebalancePlanCoord` 어댑터는 ReadChunk/PutChunkTo 가 명시적 에러.
이제 `Coord` 가 진짜 Coordinator 면 그쪽 사용. handlePlan 도 통합:

```go
func (s *Server) handleRebalancePlan(...) {
    ...
    if s.Coord != nil {
        plan, err = rebalance.ComputePlan(s.Coord, s.Store, s.Store)
    } else {
        plan, err = rebalance.ComputePlan(s.adapter, s.Store, s.Store)
    }
    ...
}
```

이유: plan 의 PlaceChunk 가 apply 의 PlaceChunk 와 100% 일치해야 apply 가
plan 대로 옮길 수 있음. 같은 객체 사용 = 일관성 보장.

## ט tet 데모

```
$ ./scripts/demo-tet.sh
==> 3 DN + 3-coord HA (transactional, COORD_DN_IO=1) + edge
==> PUT 8 objects
==> add dn4 to coord runtime registry
==> rebalance --plan --coord
    plan: 11 chunks misplaced

==> rebalance --apply --coord --concurrency 4
    chunks moved: 11
    ✓ no inflight errors

==> verify: GET all 8 objects → 200 (data intact)
==> verify: dn4 has chunks (was empty before)

=== ט PASS: rebalance apply runs on coord, data preserved ===
```

evidence: rebalance 가 coord 에서 실행됨. data integrity 유지. dn4 가
실제로 chunk 를 받음.

## ADR-044 의 회귀 위험

DN HTTP I/O 가 coord 에 들어오면 새 실패 모드:
- coord ↔ DN 네트워크 분할 시 apply 중단
- DN 응답 지연이 coord 의 throughput 에 영향
- coord 의 quorum write logic 이 edge 의 그것과 분기 (현재는 같은 패키지지만)

mitigation: ADR-044 의 -항목이 honestly 명시. apply 가 운영 부담을 coord 로
옮기는 것 — 그 부담을 받아들이는 결정.

## "왜 한 ep 에 plan + apply 같이 안 했나"

ADR-043+044 가 두 ADR 로 나뉘었다. 한 PR 로 끝낼 수도 있었음.

이유: **plan 은 순수 계산, apply 는 I/O**. 위험 카테고리가 다름. 분리해서:
- plan ep 는 빠르게 PASS (계산 정확성만)
- apply ep 는 carefully (DN I/O 의 새 책임)

만약 apply 에 버그 생기면 plan 은 영향 없음. 분리 = 회귀 격리.

이게 P8-01 chaos suite 가 보여준 패턴 (작은 단위 검증) 의 mini 버전.

## 다음

Ep.38 — GC 도 같은 패턴으로 coord 에. ADR-045. plan + apply 한 ep 에 묶음
(GC 는 rebalance 처럼 plan/apply 분리가 약하고 간단). demo-yod (י).
