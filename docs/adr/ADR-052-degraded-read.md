# ADR-052 — Degraded read (parallel shard fetch + first-K-wins)

상태: Accepted (2026-04-27)
시즌: Season 7 Ep.2 — frame 2 (textbook primitives)

## 배경

Reed-Solomon EC (ADR-008) 의 약속: K+M shards 중 **어떤 K** 만 살아있어도
원본 복원 가능. ADR-025 의 EC repair worker 가 이 약속을 **백그라운드** 에서
realize — 사후에 누락 shard 를 reconstruct.

그러나 **read path** 는 그 약속을 활용 안 했다. handleGetEC (S4 Ep.6 follow-
up) 의 stripe loop:

```go
for si, sh := range stripe.Shards {              // ← 직렬
    data, _, ferr := s.Coord.ReadChunk(ctx, sh.ChunkID, sh.Replicas)
    ...
}
```

각 shard 를 한 번에 하나씩 시도. 한 DN 이 slow 하면 전체 stripe 가 그 DN
대기. 하나 죽었으면 conn-refused RTT 추가. 두 죽었으면 두 RTT 추가. 이게
production 분산 storage 의 read tail latency 의 큰 부분.

textbook 패턴은 "first K wins": K+M shards 를 **parallel** 로 fetch, K 개가
도착하면 즉시 reconstruct + 응답. 나머지 fetch 는 cancel.

## 결정

`Server.parallelFetchShards(ctx, shards, K)` helper. handleGetEC 의 stripe
loop 가 이걸 호출.

```
1. K+M goroutine 동시 launch (per shard)
2. 각 goroutine: ReadChunk + sha256 verify, buffered channel 에 result 송신
3. main: K 성공 수집할 때까지 channel 에서 receive
4. K 도달 → cancel context (남은 fetches 자발 종료) + drain (goroutine leak 방지)
5. enc.Reconstruct(shards) → 복원
```

핵심 invariant:
- **Latency**: K-th 가장 빠른 shard 의 도착 시간으로 결정. 죽은 DN, slow DN
  은 그 K 안에 못 들어오면 cancel.
- **Correctness**: 어느 K 가 도착하든 RS Reconstruct 는 결정적 — 같은 결과.
- **No goroutine leak**: cancel 후 모든 goroutine drain (buffered channel
  size = total → 절대 block 하지 않음).

새 metric `kvfs_ec_degraded_read_total` — stripe 가 RS Reconstruct 를 진짜로
**필요했을 때** 증가 (data shards `[0..K-1]` 중 nil 있을 때). 모든 데이터
shard 가 fast-path 로 도착한 healthy GET 은 카운트 안 됨 — 운영자가 dashboard
로 "EC 가 평소엔 reconstruct 안 한다 vs 지금 어떤 stripe 들이 reconstruct
해야 한다" 를 시각화.

## 알고리즘 detail

```go
func (s *Server) parallelFetchShards(ctx, shards []ChunkRef, quorum int) ([][]byte, int) {
    fetchCtx, cancel := context.WithCancel(ctx)
    defer cancel()
    ch := make(chan result, len(shards))   // buffered = no goroutine block
    for i, sh := range shards {
        go func(idx int, ref ChunkRef) {
            data, _, err := s.Coord.ReadChunk(fetchCtx, ref.ChunkID, ref.Replicas)
            // verify sha256, send result (ok=true|false)
            ch <- result{idx, data, ok}
        }(i, sh)
    }
    survivors := 0; collected := 0
    for collected < total {
        r := <-ch
        collected++
        if r.ok {
            out[r.idx] = r.data
            survivors++
            if survivors == quorum {
                cancel()                    // ← stop remaining fetches
                drain(ch, total - collected) // ← absorb in-flight results
                return out, survivors
            }
        }
    }
    return out, survivors  // < quorum: caller fails the GET
}
```

## 비채택 대안

| 대안 | 기각 사유 |
|---|---|
| 순서대로 K shards 만 fetch (skip M parity) | M parity shards 가 더 빨리 도착할 수 있음. data + parity 차별 없이 first-K 가 더 robust |
| Hedged read (per-shard 내부 replica parallel) | 별도 결정. 본 ADR 은 stripe-level parallelism 만; replica-level hedged read 는 미래 ADR (per-replica conn 비용 ↑) |
| 모든 shard 끝까지 기다리기 (현재 동작 유지) + metric 만 추가 | core 문제 (latency) 미해결. metric 만으로는 가치 부족 |
| Streaming reconstruct (수신 즉시 incremental) | RS 알고리즘은 K shards 를 한꺼번에 입력 받음 (matrix invert). incremental 불가 |
| Coord 가 결정 (PlaceN 결과의 일부만 advise) | edge 가 K+M 모두 알아도 first-K 결정은 read-time 정보. coord routing 추가 의미 없음 |

## 결과

+ **Tail latency 단축**: 1 slow DN 이 read 를 가두지 않음. K-th fastest 만 영향.
+ **Failure 내성 read-time 실현**: ADR-008 의 "any K" 약속이 GET 즉시 활용.
+ **Degraded read 가시성**: 새 metric `kvfs_ec_degraded_read_total` 로 자가
  치유 빈도 모니터.
+ **No design 변경**: 같은 K+M shards 분포, 같은 RS encoder, 같은 stripe
  schema. fetch 전략만 교체.
+ **3 unit tests + demo PASS**: AllSurvive (parallel proof — 50ms × 6 shards
  serial 이면 300ms 인데 한 RTT 안에 끝남), KSurvivors (4 alive + 2 fail →
  성공), BelowQuorum (3 alive → 실패 검증).
- **DN 부담 일시 ↑**: K+M goroutines 동시 fetch (early cancel 로 평균
  K-fetch + cancel-overhead). serial 보다 약간 더 많은 traffic — pre-existing
  rebalance 같은 background load 와 충돌 가능. measurement 기반 후속 결정.
- **Memory 일시**: 한 stripe 의 K+M shards 가 잠시 동시 메모리. shardSize ×
  (K+M) ≈ pre-S7 와 같음 (전엔 sequential reuse, 지금은 동시). EC 의 shard
  size 가 보통 작아 (수십 KiB ~ 수 MiB) 실용적 한계 도달 어려움.
- **Replication mode 미적용**: 본 ADR 은 EC stripe parallelism 만. replication
  의 multi-replica 는 hedged read 패턴이 적합하나 conn 비용/이득 trade-off
  이 다름 — 별도 ADR 후보.
- **Reconstruct 항상 실행**: shards[0..K-1] 이 이미 모두 도착해도 enc.Reconstruct
  호출. RS 가 그 경우 빠르게 no-op 처럼 동작하지만, 진짜 skip 도 가능 — 후속
  optimization.

## 호환성

- handleGetEC API 무변경. body shape, header, trim 로직 모두 그대로.
- 새 metric `kvfs_ec_degraded_read_total` — 기본 0, scrape 받는 측 영향 0.
- 새 helper `parallelFetchShards` — 내부, public API 영향 0.

## 검증

- `internal/edge/parallel_fetch_test.go` 3 신규 unit test:
  - **AllSurvive** — 6 fake DNs each 50ms delay, K=4. parallel 이라 ~50ms,
    serial 이면 300ms. 200ms hard upper bound assertion.
  - **KSurvivors** — 4 alive + 2 fail, K=4 → survivors ≥ 4, 죽은 슬롯들은 nil.
  - **BelowQuorum** — 3 alive + 3 fail, K=4 → survivors=3, caller 가 GET fail.
- `scripts/demo-ayin.sh` — 6 DN + EC (4+2) live, baseline GET vs 2-DN-down
  GET. baseline ≈ 28ms, degraded ≈ 44ms (reconstruct CPU + 2 conn-refused
  RTT의 일부 흡수). metric counter 가 baseline 0, degraded 1 → 의미 검증.
- 전체 test suite **167 PASS** (parallel_fetch +3).

## 후속

- **Replication-mode hedged read**: chunk's replicas[0/1/2] 동시 발사, 첫
  응답 win. 별도 ADR. trade-off (DN load 증가 vs read latency 단축).
- **Reconstruct skip when all data shards present**: shards[0..K-1] 모두
  non-nil 이면 RS skip — 평균 GET CPU ↓.
- **Auto-repair on degraded read**: GET 시 missing shard 발견하면 inline
  으로 re-place + PUT (또는 background queue 에 enqueue). degraded 가 누적
  되지 않게 self-healing.
