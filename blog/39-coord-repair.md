# Episode 39 — EC repair on coord

> **Series**: kvfs — distributed object storage from scratch
> **Season**: 6 (operational migration) · **Episode**: 4
> **연결**: [ADR-046](../docs/adr/ADR-046-coord-repair.md) · [demo-kaf](../scripts/demo-kaf.sh)

---

## 세 번째 worker

S6 의 worker 트랙 = rebalance / GC / repair. 패턴은 셋 다 같다:

```
endpoint:        POST /v1/coord/admin/<worker>/{plan,apply}
prerequisite:    coord.Server.Coord (= COORD_DN_IO=1)
internal pkg:    internal/<worker>/{plan,run}
cli:             kvfs-cli <worker> --coord URL
```

ADR-044 (rebalance), ADR-045 (GC), 본 ADR-046 (repair). 셋 다 같은 모양.

## EC repair 의 회상

[Ep.9 EC repair](09-ec-repair.md) 가 다룬 알고리즘:

```
for each stripe:
  alive shards 수 점검
  if K <= alive < K+M:
    fetch K shards
    Reed-Solomon Reconstruct → all K data shards
    encode → all M parity shards
    누락된 shard 들을 PlaceN 의 위치로 다시 PUT
```

이 worker 가 edge 에서 coord 로 옮긴다. 알고리즘 변경 0.

## repair.Coordinator 인터페이스

```go
type Coordinator interface {
    DNs() []string
    PlaceN(key, n) []string
    ReadChunk(ctx, id, replicas) ([]byte, ...)
    PutChunkTo(ctx, addr, id, data) error
}
```

`coord.Server.Coord` (= `internal/coordinator.Coordinator`) 가 이미 만족.

ADR-044/045 의 인프라 그대로 재활용.

## endpoints

```
POST /v1/coord/admin/repair/plan
POST /v1/coord/admin/repair/apply?concurrency=N
```

cli:
```
$ kvfs-cli repair --plan/--apply --coord URL
```

## כ kaf 데모 — 진짜 데이터 손실 시뮬

```
$ ./scripts/demo-kaf.sh
==> setup: 3 DN + coord + edge
==> add dn4, dn5, dn6 (총 6 DN)
==> PUT 1 GB object with X-KVFS-EC: 4+2
    splits into N stripes, each (4 data + 2 parity = 6 shards)

==> kill dn5 + dn6 (= 2 shards lost per stripe; K survivors = 4)
==> verify GET still works
    1 GB read OK ← K survivors → reconstruct on read

==> repair --plan --coord
    stripes needing repair: 47
    missing shards per stripe: 2

==> repair --apply --coord --concurrency 4
    repaired: 47 stripes
    new shards placed on remaining DNs

==> bring dn5/dn6 back, verify they're empty (repair already redistributed)
    dn5: 0 chunks
    dn6: 0 chunks
==> verify all data still readable (post-repair healthy)
    1 GB read OK ✓

=== כ PASS: EC repair runs on coord, missing shards reconstructed ===
```

evidence: 2 of 6 shards 손실해도 데이터 무손실, repair 가 coord 에서 실행되어
새 shards 가 alive DNs 으로 재배포됨.

## 알고리즘은 변하지 않았다

ADR-046 의 핵심 인사이트: **위치 이동, 알고리즘 동일**. Reed-Solomon Reconstruct
는 [Ep.6](06-erasure-coding.md) 의 GF(2^8) 산술 그대로. shard layout 도 그대로.
변경된 건 worker 가 어느 daemon 안에 있냐 뿐.

이게 좋은 추상화의 가치. `repair` 패키지가 `Coordinator` 인터페이스에 의존
(implementation 에 의존 X), 그 인터페이스를 만족하는 객체가 어느 process 안에
있든 무관.

## S6 worker 트랙 close

Ep.36~39 으로 worker 3개가 모두 coord 로:

| Worker | ADR | Episode | demo |
|---|---|---|---|
| Rebalance plan | ADR-043 | Ep.36 | chet ח |
| Rebalance apply | ADR-044 | Ep.37 | tet ט |
| GC | ADR-045 | Ep.38 | yod י |
| Repair | ADR-046 | Ep.39 | kaf כ |

남은 S6 episode 3개 = registry mutation (DN, URLKey) + edge propagation.

## 다음

Ep.40 — DN registry 의 add/remove/class 가 coord 에. ADR-047. cli `dns add/
remove/class --coord`. demo-lamed (ל).
