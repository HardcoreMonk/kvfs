# Episode 38 — GC on coord

> **Series**: kvfs — distributed object storage from scratch
> **Season**: 6 (operational migration) · **Episode**: 3
> **연결**: [ADR-045](../docs/adr/ADR-045-coord-gc.md) · [demo-yod](../scripts/demo-yod.sh)

---

## 같은 패턴, 다른 worker

Ep.37 (rebalance apply) 가 coord 에 DN I/O 를 도입했다. 그 인프라를 재활용하면
GC 는 거의 공짜로 옮길 수 있다.

```
ADR-044: coord.Server.Coord (DN client) → rebalance.Run
ADR-045: coord.Server.Coord (DN client) → gc.Run
```

새 코드 ~80 LOC. ADR 본문이 짧은 이유.

## endpoints

```
POST /v1/coord/admin/gc/plan?min-age=DURATION
POST /v1/coord/admin/gc/apply?min-age=DURATION&concurrency=N
```

`gc.Coordinator` 인터페이스:

```go
type Coordinator interface {
    DNs() []string
    ListChunks(ctx, addr) ([]ChunkInfo, error)
    DeleteChunkFrom(ctx, addr, id) error
}
```

`coord.Server.Coord` (= `internal/coordinator.Coordinator`) 가 이미 만족.
어댑터 0줄.

## min-age 의 트릭한 detail

`min-age` 는 GC 의 핵심 안전망 ([Ep.4](04-gc.md)) — chunk 가 disk 에 N 분 미만으로
있으면 보호. PUT 직후 메타 commit 직전의 race 방지.

edge 의 기존 endpoint:
```
POST /v1/admin/gc/plan?min_age_seconds=60     ← int seconds
```

coord 의 새 endpoint:
```
POST /v1/coord/admin/gc/plan?min-age=60s       ← duration
```

cli 가 자동 변환. duration parsing 이 더 future-proof (1m, 30s, 2h 등). 두
스펙 동등 — 운영자가 어느 쪽 쓰든 같은 결과.

## י yod 데모

```
$ ./scripts/demo-yod.sh
==> 3 DN + coord (DN_IO) + edge
==> PUT object → DELETE → orphan chunk on disk
==> gc --plan --coord --min-age 1s
    plan: 1 surplus chunk (sha256=abc...)

==> gc --apply --coord --min-age 1s --concurrency 2
    deleted: 1 chunk

==> verify DN /chunks 에서 사라짐
    dn1: chunk count 0 (was 1)
    dn2: chunk count 0 (was 1)
    dn3: chunk count 0 (was 1)

=== י PASS: GC runs on coord, surplus chunks cleaned ===
```

evidence: GC apply 가 coord 에서 실행, 실제 byte 가 DN 디스크에서 사라짐.

## edge 측의 잔재

ADR-045 의 -항목:
> edge 의 GC 코드 잠시 중복 (rebalance 와 동일 정리 패턴)

Season 6 동안 edge 의 GC endpoint 도 working — backward compat. 운영자가
점진 migrate. S6 끝난 후 별도 cleanup ep 에서 edge 의 GC code 제거 가능 (또는
지금처럼 fallback 으로 영구 보존 — single-coord-mode 운영 시 edge inline 이
필요).

## Trade-off 가 작다

ADR-043 (rebalance plan) 은 새 책임 (계산 위치 이동) 이 있고, ADR-044 (rebalance
apply) 는 큰 책임 (DN I/O 도입) 이 있다. ADR-045 (GC) 는 둘 다 있는 인프라
재활용 — incremental cost 거의 0.

이게 좋은 abstraction 의 또 다른 증거. `Coordinator` 인터페이스가 rebalance,
GC, repair 셋 모두 만족 → 한 번 만든 객체로 셋 다 옮기기 가능.

## 다음

Ep.39 — EC repair 도 같은 패턴. ADR-046. K survivors → Reed-Solomon
Reconstruct → 누락 shard 재배포. demo-kaf (כ).
