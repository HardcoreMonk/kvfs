# Episode 44 — degraded read: K survivors 의 약속을 GET 시점에 실현

> **Series**: kvfs — distributed object storage from scratch
> **Season**: 7 (textbook primitives) · **Episode**: 2
> **연결**: [ADR-052](../docs/adr/ADR-052-degraded-read.md) · [demo-ayin](../scripts/demo-ayin.sh)

---

## EC 의 약속, 그러나 read 는 받지 못한 약속

[Ep.6 erasure coding](06-erasure-coding.md) 의 핵심 슬로건:

> **"K+M shards 중 어떤 K 만 살아있어도 원본 복원 가능."**

[Ep.9 EC repair](09-ec-repair.md) 가 이 약속을 background 에서 realize —
worker 가 주기적으로 누락 shard 발견 → reconstruct → 재배포.

그런데 **read path** 는 이 약속을 활용 안 했다. handleGetEC 의 stripe loop:

```go
for si, sh := range stripe.Shards {     // ← 직렬
    data, _, err := s.Coord.ReadChunk(ctx, sh.ChunkID, sh.Replicas)
    if err != nil { continue }
    sum := sha256.Sum256(data)
    if hex.EncodeToString(sum[:]) != sh.ChunkID { continue }
    shards[si] = data
    survivors++
}
```

각 shard 를 한 번에 하나씩 시도. 한 DN 이 slow 하면 전체 stripe 가 그 DN
대기. 하나 죽었으면 conn-refused RTT 추가. 두 죽었으면 두 RTT 추가.

ADR-008 의 "any K" 가 read time 에는 적용 안 됨 — repair 시간까지 기다려야
했다. textbook 패턴은 **first-K-wins** — K+M shards 를 parallel 로 시작,
K 개 도착하면 즉시 reconstruct + 응답.

## 변경 — Inline parallel fetch

`parallelFetchShards(ctx, shards, K)` helper:

```go
func (s *Server) parallelFetchShards(ctx, shards []ChunkRef, quorum int) ([][]byte, int) {
    fetchCtx, cancel := context.WithCancel(ctx)
    defer cancel()
    ch := make(chan result, len(shards))   // buffered = goroutine 안 막힘
    for i, sh := range shards {
        go func(idx int, ref ChunkRef) {
            data, _, err := s.Coord.ReadChunk(fetchCtx, ...)
            // sha256 verify, send result
            ch <- result{idx, data, ok}
        }(i, sh)
    }
    survivors := 0; collected := 0
    for collected < total {
        r := <-ch
        collected++
        if r.ok && {survivors++; out[r.idx]=r.data; survivors == quorum} {
            cancel()       // ← 남은 fetch 즉시 중지
            drain(ch)      // ← in-flight result 흡수, goroutine leak 0
            return out, survivors
        }
    }
    return out, survivors  // < quorum: caller 가 GET 실패
}
```

handleGetEC 의 stripe loop 가 이 helper 한 호출로 교체. 알고리즘 변경 0,
fetch 전략 변경 1.

## ע ayin 데모

```
$ ./scripts/demo-ayin.sh
==> PUT 1 MB object with EC (4+2)    stripes: 1
    expected sha256: 8f4d42bf937971a2..

==> baseline GET (all 6 DN alive)
    baseline elapsed: 28 ms, sha verified ✓

==> kill 2 DN (dn5, dn6)
    alive: dn1, dn2, dn3, dn4 (4 of 6 = K survivors)

==> GET with 2/6 DN dead
    degraded elapsed: 44 ms, sha verified ✓

==> verify metric counter
    kvfs_ec_degraded_read_total = 1

=== ע PASS: GET via K survivors + RS Reconstruct, sha verified ===
```

evidence:
- 정상 GET 28ms, degraded GET 44ms — degraded 가 **약간 더 느림** (RS
  Reconstruct CPU + 2 conn-refused RTT 흡수). 그러나 **두 dead DN 의 timeout
  대기는 0** — first-K-wins 가 K=4 도착 후 즉시 cancel.
- metric `kvfs_ec_degraded_read_total` = 1 (degraded GET 에서만 증가, baseline
  은 카운트 안 됨). data shards 중 nil 있을 때만 카운트 → 의미 있는 시그널.

## Latency: parallel 의 증명

unit test `TestParallelFetchShards_AllSurvive` 가 가장 명료한 증명:

```go
// 6 fake DNs, 각각 50ms delay.
// Serial loop: 6 × 50ms = 300ms.
// Parallel + first-K-wins (K=4): K-th fastest = ~50ms 도착 시점.
// Test asserts elapsed < 200ms (충분한 margin).
```

실제 측정:
```
=== RUN   TestParallelFetchShards_AllSurvive
    parallel_fetch_test.go: elapsed 70ms (well under 200ms hard bound)
--- PASS
```

70ms = 50ms (DN delay) + ~20ms (test overhead). serial 이면 300ms+.

## Reconstruct 도 항상 실행

ADR-052 의 honest -항목: 현재 `enc.Reconstruct(shards)` 가 항상 호출됨. 모든
K data shards 가 fast-path 로 도착해도 RS 호출 — RS 자체는 그 경우 빠르게
no-op 같이 동작 (matrix is identity), 그러나 정확히 skip 하는 것이 작은
optimization. ADR-052 후속 항목.

## metric 의 의미

새 counter `kvfs_ec_degraded_read_total` 의 정의:
- **stripe 가 RS Reconstruct 가 진짜로 필요했을 때** 증가
- 즉 data shards (idx 0..K-1) 중 적어도 하나가 missing 일 때

다른 가능한 정의들:
- "survivors < K+M" — first-K-wins 에선 항상 true (early cancel) → 무의미
- "stripe 1 개당 1 increment" — GET 빈도 metric 과 중복

선택한 정의가 가장 actionable: dashboard 가 0 보다 크면 운영자가 "어느 stripe
들이 reconstruct 필요? repair worker 따라 잡고 있나?" 질문 가능.

## Replication 모드는?

Pure replication (R=3) 에는 EC parity 가 없으므로 "K survivors" 개념 없음.
대신 같은 chunk 의 R replicas 중 첫 응답 wins 가 적절한 패턴 — **hedged
read** (Dean & Barroso 2013 의 tail tolerance 기법).

ADR-052 는 **EC 만**. Replication-mode hedged read 는 별도 ADR 후보 (per-
replica conn 비용 ↑ vs read latency ↓ trade-off 다름).

## Auto-repair on degraded read?

degraded read 발생 시 missing shard 를 inline 으로 re-place + PUT 하면 좋다.
GET 자체에 부담 살짝 추가 (background queue 에 enqueue 가 더 흔한 패턴).
ADR-052 후속 항목으로 기록 — 본 ADR 의 단순 정리는 fetch 전략만.

## frame 2 진척

| primitive | 상태 |
|---|---|
| Failure domain hierarchy (Ep.43) | ✓ |
| **Degraded read** (이 글) | ✓ |
| Tunable consistency | 다음 (Ep.45) |
| Anti-entropy / Merkle tree | 다음다음 (Ep.46) |

S7 절반 도달. ADR-008 의 EC 약속이 이제 read time 에도 사용된다. 분산
storage 의 latency 곡선이 1 slow DN 이 아니라 K-th fastest 로 결정.

## 다음

Ep.45 — tunable consistency. Dynamo 의 W+R>N classic 을 per-request 로.
client 가 PUT 마다 `X-KVFS-W: 1`, GET 마다 `X-KVFS-R: 3` 같이 quorum 조정.
ADR-053. demo-pe (פ).

이게 일관성 모델의 textbook 마지막 piece — strict 부터 eventually 까지
스펙트럼.
