# ADR-036 — WAL group commit observability

상태: Accepted (2026-04-26)

## 배경

ADR-035 의 group commit (`EDGE_WAL_BATCH_INTERVAL=5ms`) 은 throughput 을 ~15× 끌어올린다고 주장하지만, 운영 중에는 그 batch 가 실제로 어느 정도 모이는지·얼마나 오래 대기하는지 알 길이 없다. flusher 가 1 entry 짜리 batch 만 계속 만든다면 이론값이 거짓말이고, 100 entry 짜리 batch 가 5ms 가 아니라 100ms 씩 대기한다면 RPO 가 망가진 것.

## 결정

`internal/store/wal.go` 가 두 가지 atomic 카운터를 유지하고, edge 의 `/metrics` 가 gauge 두 개로 노출.

- **`kvfs_wal_batch_size`** — 마지막 fsync cycle 의 entry 수 (= `pending - prevDurable`).
  - inline mode 에서는 0.
  - 0 이 자주 보이면 group commit 이 효과 없는 환경 (트래픽 적음).
- **`kvfs_wal_durable_lag_seconds`** — 가장 오래된 미-fsync entry 의 age, 초 단위.
  - 정상이면 batchInterval 근처. 증가하면 디스크 stall 또는 fsync 지연.
  - 0 = pending 없음.

구현:

```go
// WAL struct (atomic, lock-free):
lastBatchSize    atomic.Int64
oldestUnsyncedNs atomic.Int64

// Append (batched path):
w.oldestUnsyncedNs.CompareAndSwap(0, time.Now().UnixNano())

// flushOnce, fsync 성공 후:
w.lastBatchSize.Store(pending - w.durableSeq)
w.oldestUnsyncedNs.Store(0)
```

CAS 패턴이 핵심: 첫 Append 만 oldest 를 마킹, 이후 Append 는 같은 batch 의 일원이므로 마킹 안 함. flusher 가 reset.

`internal/edge/metrics.go` 에 callback gauge 두 개 등록 — `/metrics` scrape 마다 cheap atomic load.

## 결과

+ group commit 효과를 외부에서 검증 가능.
+ Pool, dependency 추가 0 — 기존 metrics 인프라 재사용.
+ Inline mode 에서 둘 다 0 → operator 가 mode 구분 가능.
- High write rate 에서 oldestUnsyncedNs 가 wall-clock time 이라 NTP drift 영향 받음 (microbenchmark 의미만).
- batch 분포 (p50 / p99) 가 아니라 마지막 값만 — histogram 미지원 (ADR-035 micro-opt 정책 일관성).

## 호환성

- WAL 에 `LastBatchSize() int64` / `OldestUnsyncedAge() time.Duration` 메서드 추가 (외부 contract 확장).
- 기존 사용자 영향 0 — inline mode 동작 무변화.
- Truncate 가 두 카운터도 reset.
