# Episode 22 — Micro-optimization 3종 묶음

> **Series**: kvfs — distributed object storage from scratch
> **Season**: 4 (performance/efficiency) · **Episode**: 9 (post-Season-4 wrap)
> **ADR**: [035](../docs/adr/ADR-035-micro-optimizations.md) (+ [036](../docs/adr/ADR-036-wal-batch-metrics.md), [037](../docs/adr/ADR-037-chunker-pool-cap.md))

---

## 3개 한 묶음

큰 결정 ([Ep.20](20-sync-replication.md), [Ep.21](21-transactional-raft.md)) 이 끝나면 남는 건 **profile 결과의 빨간 막대들**. 세 가지 micro-opt 가 한 commit 으로 들어갔다 — 모두 opt-in, 모두 backward compat.

1. **WAL group commit** — 매 PUT 마다 fsync 하던 걸 batch 로
2. **3-region FastCDC** — chunk 크기 분포의 상단 tail 줄이기
3. **chunker sync.Pool** — alloc churn 제거

각각 작지만 hot path 의 다른 막대.

## 1. WAL group commit

### 문제

```
goroutine A: WAL.Append → flush → fsync (~1 ms)
goroutine B: WAL.Append → 기다림 (mu) → flush → fsync (~1 ms)
goroutine C: ...
```

100 동시 PUT = 100 fsync 직렬. 직렬화된 fsync 가 throughput 의 천장.

### 해결

매 5 ms 마다 background flusher 가 한 번만 fsync. 그 사이에 들어온 모든 Append 가 그 한 fsync 를 공유.

```go
type WAL struct {
    ...
    batchInterval time.Duration
    cond          *sync.Cond
    durableSeq    int64
}

func (w *WAL) Append(...) {
    w.mu.Lock()
    seq := w.seq.Add(1)
    w.w.Write(line)            // bufio 까지만
    if w.batchInterval == 0 {  // inline mode
        w.w.Flush(); w.f.Sync()
        w.mu.Unlock()
        return seq, line, nil
    }
    // batched: cond 으로 대기
    for w.durableSeq < seq && !w.closed {
        w.cond.Wait()
    }
    w.mu.Unlock()
    return seq, line, nil
}

func (w *WAL) flusher() {
    t := time.NewTicker(w.batchInterval)
    for range t.C {
        w.mu.Lock()
        pending := w.seq.Load()
        if pending > w.durableSeq {
            w.w.Flush()
            // mu release: fsync 동안 다음 batch 가 bufio 채울 수 있게
            w.mu.Unlock()
            w.f.Sync()
            w.mu.Lock()
            w.durableSeq = pending
            w.cond.Broadcast()
        }
        w.mu.Unlock()
    }
}
```

핵심 디테일: **fsync 동안 mu release**. 그래야 다음 batch 의 Append 들이 bufio 에 글을 쓸 수 있다. ADR-035 의 /simplify pass 에서 잡힌 hot 경로.

### 측정 (로컬 NVMe)

| batch interval | concurrent PUTs | p50 latency | write/sec |
|---|---|---|---|
| 0 (inline) | 100 | 2.1 ms | ~480 |
| 5 ms | 100 | 5.4 ms | ~7100 |
| 10 ms | 100 | 10.8 ms | ~7400 |

5 ms 면 latency ~2.5× 비용으로 throughput **15×**. 5 ms 가 RPO 의 추가 손실 한도 — 대부분의 use case 에서 받아들일 수 있는 거래.

### Truncate-during-wait 함정

snapshot 이 WAL.Truncate 를 부르면 seq 가 0 으로 reset. 그런데 어떤 Append 가 이미 seq=5 받고 cond 에 park 했다면? `w.durableSeq < 5` 가 영원히 true → deadlock.

해결: `epoch` counter. Truncate 시 bump → waiter 가 자기 epoch 와 비교, 다르면 error 로 깨어남.

```go
// Append (batched):
myEpoch := w.epoch
for w.durableSeq < seq && !w.closed && w.epoch == myEpoch {
    w.cond.Wait()
}
if w.epoch != myEpoch {
    return 0, nil, fmt.Errorf("wal: truncated while waiting for fsync")
}

// Truncate:
w.epoch++
w.cond.Broadcast()
```

### 관측 (ADR-036)

`/metrics`:
- `kvfs_wal_batch_size` — 마지막 fsync 가 묶은 entry 수
- `kvfs_wal_durable_lag_seconds` — 가장 오래 대기 중인 entry 의 age

batch_size = 1 만 보이면 group commit 이 효과 없는 환경 (트래픽 적음). lag 가 batchInterval 보다 커지면 fsync 가 밀리고 있음 (디스크 stall 의심).

## 2. 3-region FastCDC

### 문제

[Ep.15 CDC](15-cdc.md) 의 2-mask 알고리즘은 NormalSize 이후 looser mask 로 cut 시도. 그러나 운 나쁘면 MaxSize 까지 cut 안 남 → MaxSize 캡 chunk. 분포 상단이 MaxSize 에 누적.

### 해결

3rd region: `(NormalSize + MaxSize) / 2` 통과 후 더 약한 mask. MaxSize 도달 직전에 거의 확실히 cut.

```go
type CDCConfig struct {
    MinSize        int
    NormalSize     int
    MaxSize        int
    MaskBitsStrict int   // pre-NormalSize
    MaskBitsLoose  int   // post-NormalSize
    MaskBitsRelax  int   // post-(Normal+Max)/2 — 0 = disabled, backward compat
}

func cutpoint(data []byte) int {
    // Phase 1: MinSize 까지 fingerprint warm-up (cut 시도 X)
    // Phase 2: MinSize..NormalSize: maskS (strict)
    // Phase 3: NormalSize..(N+M)/2: maskL (loose)
    // Phase 4: (N+M)/2..MaxSize: maskR (very loose) — 옵션
    // 캡: MaxSize
}
```

`MaskBitsRelax = 0` (default) 이면 Phase 4 건너뜀 → 기존 동작 byte-for-byte 동일. 활성하면:

| MaskBitsRelax | MaxSize-cap chunk 비율 | std dev |
|---|---|---|
| 0 (off) | 6.2% | 1.85 MiB |
| 19 | 0.4% | 1.62 MiB |

dedup 효율 살짝 ↑, variance ↓.

### 미묘함: shift-invariance 보존

mask region 이 늘어도 같은 input → 같은 cut 보장 — 핵심 dedup 성질 유지. mask 는 fp 의 함수이고 fp 는 byte stream 의 deterministic 함수.

## 3. chunker sync.Pool

### 문제

```go
func NewReader(src io.Reader, chunkSize int) *Reader {
    return &Reader{src: src, chunkSize: chunkSize, buf: make([]byte, chunkSize)}
}
```

`make([]byte, 4MiB)` 가 매 PUT 마다 한 번. 초당 수백 PUT 시 GC 가 청소 못 따라가서 RSS 가 부푼다.

### 해결

scratch buffer 를 sync.Pool 로 재사용:

```go
var scratchPool sync.Pool

func getScratch(n int) []byte {
    for i := 0; i < 2; i++ {
        v := scratchPool.Get()
        if v == nil { break }
        b := v.([]byte)
        if cap(b) >= n && cap(b) <= 2*n { return b[:n] }
        // 너무 큼 — pin 방지로 drop
    }
    return make([]byte, n)
}

func putScratch(b []byte) {
    scratchPool.Put(b[:0])
}

// Reader.Close() 가 putScratch 호출 — 호출 안 해도 GC 안전, 호출하면 더 빠름.
```

### Cap 정책 (ADR-037)

sync.Pool 자체는 GC round 사이 무한 누적 가능. `EDGE_CHUNKER_POOL_CAP_BYTES` soft cap:

```go
var (
    scratchPoolBytes atomic.Int64
    poolCapBytes     atomic.Int64
)

func putScratch(b []byte) {
    c := int64(cap(b))
    limit := poolCapBytes.Load()
    if limit > 0 && scratchPoolBytes.Load()+c > limit {
        return  // over cap; let GC reclaim
    }
    scratchPoolBytes.Add(c)
    scratchPool.Put(b[:0])
}
```

cgroup memory limit 친화. `kvfs_chunker_pool_bytes` gauge 로 관측.

### 측정

100k × NewReader+Next+Close, 4 MiB chunkSize:
- alloc: 100k × 4 MiB → ~0 (재사용)
- GC pause 횟수 ~80% 감소

## 묶음의 의미

각각은 작은 변경. 함께 적용하면:
- WAL: 480 → 7400 ops/s (15×)
- CDC: chunk size variance ↓
- alloc: GC churn ↓

큰 architectural change ([Ep.20](20-sync-replication.md), [Ep.21](21-transactional-raft.md)) 으로 이미 RPO·일관성을 살린 위에, **같은 hardware 에서 더 많이 처리** — micro-opt 의 본질.

## Season 4 의 마무리

Season 4 (성능·효율) 이 마무리. 누적:

| Ep | 내용 | 핵심 |
|---|---|---|
| 14 | streaming PUT/GET | edge memory ≈ chunkSize |
| 15 | CDC chunking | shift-invariant dedup |
| 16 | leader election | Raft state machine |
| 17 | WAL | RPO 분 → 초 |
| 18 | EC streaming | stripe 단위 pump |
| 19 | CDC × EC | variable stripe |
| 20 | sync replication | RPO 초 → ms |
| 21 | transactional Raft | RPO ms → 0 |
| 22 | micro-opts | throughput 곡선 한 칸 |

## 다음

Season 5 진입은 [ADR-015 (Proposed)](../docs/adr/ADR-015-coordinator-daemon-split.md) 사용자 채택에 달렸다. 그때까지는 Season 4 운영 관찰 + 후속 ADR 들 (036, 037) 의 지표 검증.
