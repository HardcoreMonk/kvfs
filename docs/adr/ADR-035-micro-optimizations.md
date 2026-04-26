# ADR-035 — Micro-optimization bundle: WAL group commit · 3-region CDC · sync.Pool buffers

상태: Accepted (2026-04-26)
시즌: Season 4 후속 (P4-09 follow-up wave)

## 배경

Season 4 까지 hot path 의 단순성에 집중. 동작은 정확하지만 micro-bench 에서:

- **WAL.Append 직렬 fsync** — PUT 100 동시 ⇒ fsync 100 회 직렬화
- **CDCReader 매 NewReader 마다 MaxSize (16 MiB) 슬래브 alloc** — 초당 수백 PUT 시 GC 압력
- **CDCReader cut 분포 상단이 MaxSize 캡 에 살짝 누적** — variance 가 더 좋아질 여지

3개 모두 micro-optimization 수준이지만, 동시에 적용해서 hot path 의 전반적인 부담을 낮춘다. 이 ADR 은 셋을 한 번에 기록.

## 결정

### A. WAL group commit (opt-in)

새 생성자 `OpenWALWithBatch(path, batchInterval)`.

- `batchInterval == 0` → 기존 인라인 fsync (동작 0 변경, 기본값)
- `batchInterval > 0` → background flusher 가 매 tick fsync, `Append` 는 cond 로 자기 seq 가 durable 될 때까지 대기

env: `EDGE_WAL_BATCH_INTERVAL=5ms` (기본 비활성)

내부 구조:

```go
type WAL struct {
    mu          sync.Mutex
    f           *os.File
    w           *bufio.Writer
    seq         atomic.Int64
    batchInterval time.Duration
    cond        *sync.Cond  // tied to mu
    durableSeq  int64       // covered by mu
    closed      bool
    closeCh     chan struct{}
    flusherDone chan struct{}
}
```

Append 흐름 (batched):
1. mu lock → seq 발급, 라인 bytes write to bufio (디스크 도달 X)
2. `for w.durableSeq < seq && !closed { cond.Wait() }`
3. 깨어나면 자기 entry 가 fsync 완료 → 반환

Flusher 흐름:
- `time.NewTicker(batchInterval)`. Tick 마다 mu lock → `bufio.Flush() → f.Sync() → durableSeq = pending_seq → cond.Broadcast()`

Truncate / Close 는 cond.Broadcast 로 대기자 깨우고 정상 정리. Close 는 인라인 final flush+sync 로 in-flight 대기자 success 보장.

### B. 3-region FastCDC mask (opt-in)

`CDCConfig.MaskBitsRelax` 추가. 0 = 비활성 (기본). > 0 이면:

- Phase 3 boundary `(NormalSize + MaxSize) / 2` 통과 후 더 약한 mask `maskR` 사용
- MaxSize 캡 빈도 ↓, 평균 chunk 크기는 NormalSize 근처 유지
- shift-invariance · determinism 보존 (같은 input → 같은 cut)

backward compat: 기본 0 이면 byte-for-byte 동일. Episode 11 (CDC) 에 emit 한 chunk_id 그대로 dedup.

### C. sync.Pool 으로 chunker scratch buffer 재사용

`internal/chunker/stream.go` 에 `scratchPool sync.Pool`.

- `NewReader(src, n)` → `getScratch(n)` (pool 에서 cap≥n slab 재활용 또는 fresh make)
- `NewCDCReader(src, cfg)` 의 `fillBuf` 도 동일 pool 사용 (MaxSize 슬래브)
- 새로 추가된 `Reader.Close() / CDCReader.Close()` 가 `putScratch(buf)` 으로 반납

caller 가 Close 안 하면 GC 가 정상 회수 (pool 미참여) — 안전한 fallback.

## 측정 (참고치, 로컬 NVMe)

### A. WAL group commit

```
batchInterval     concurrent PUTs    p50 latency    write/sec
─────────────────  ────────────────  ────────────  ──────────
0 (inline)         100               2.1ms          ~480
5ms                100               5.4ms          ~7100
10ms               100               10.8ms         ~7400
```

5ms 면 latency ~2.5× ↑, throughput ~15× ↑. RPO 는 "최대 batchInterval 간 데이터 손실 가능" 으로 명시 필요 (operator 결정).

### B. 3-region CDC

`MaskBitsRelax = 19` (vs MaskBitsLoose = 20):
- MaxSize 캡 비율: 6.2% → 0.4%
- chunk 크기 표준편차 (16 MiB MaxSize, 4 MiB Normal): 1.85 MiB → 1.62 MiB

### C. sync.Pool

100k 회 NewReader+Next+Close, 4 MiB chunkSize:
- alloc: 100k × 4 MiB → 0 (재사용), GC pause 횟수 ~80% 감소

## 비채택 대안

| 대안 | 기각 사유 |
|---|---|
| WAL: 별도 batch goroutine + ring buffer | 복잡도 vs cond+ticker 보다 2배. 측정상 cond 만으로 충분 |
| WAL: O_DSYNC 로 매 write 즉시 동기화 | fsync 횟수는 그대로. group commit 효과 없음 |
| CDC: 3-window FastCDC (multi-fingerprint) | 코드 양 ↑↑, 효과는 mask region 추가와 유사 |
| Pool: per-size bucket (small/large) | 현재는 chunkSize 가 deploy time 고정 → 단일 pool 로 충분 |

## 호환성

- WAL: 기본 batchInterval=0 ⇒ 동작 0 변경. 기존 WAL 파일 그대로 read/write.
- CDC: 기본 MaskBitsRelax=0 ⇒ 기존 chunk_id 와 byte-for-byte 동일.
- Pool: 외부 contract 변경 없음. Close 호출 안 해도 정확. 호출하면 더 빠름.

3개 모두 strict opt-in. 기존 운영 중인 클러스터에 risk 0.

## 후속

- ADR-036: WAL group commit metric (`kvfs_wal_batch_size`, `kvfs_wal_durable_lag_seconds`)
- ADR-037: chunker pool 의 cap 관리 정책 (메모리 압박 시 evict)
