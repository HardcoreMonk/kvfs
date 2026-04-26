# Episode 23 — Prometheus metrics: stdlib 만으로

> **Series**: kvfs — distributed object storage from scratch
> **Season**: P4-09 wave (post-Season-4) · **Episode**: 10
> **연결**: `internal/metrics/` · `internal/edge/metrics.go`

---

## 외부 lib 없는 Prometheus

`prometheus/client_golang` 은 표준이지만 의존 트리 수십 개. kvfs 의 "stdlib + bbolt only" 원칙과 충돌. 그렇다고 관찰성을 포기할 순 없음.

해결: Prometheus **text-exposition format** 만 직접 출력. 이게 spec — `https://prometheus.io/docs/instrumenting/exposition_formats/`. 한 페이지 짜리 grammar 라 ~100 LOC 면 Counter + Gauge 두 종류는 충분.

```
# HELP kvfs_put_total Total PUT requests
# TYPE kvfs_put_total counter
kvfs_put_total{mode="replication"} 42
kvfs_put_total{mode="ec"} 7
```

각 줄: name + (optional labels) + value. type/help 주석. 끝.

## Registry + 두 종류 metric

```go
type Registry struct {
    mu      sync.RWMutex
    metrics []metric
}

type Counter struct {  // monotonic uint64, optional labels
    name, help string
    labelKeys  []string
    values     map[string]*atomic.Uint64  // key = strings.Join(labels, "\x00")
}

type Gauge struct {    // callback-driven
    name, help string
    fn         func() int64
}
```

Counter 는 `WithLabels("replication").Add(1)`. Gauge 는 callback — scrape 마다 호출:

```go
reg.Gauge("kvfs_objects", "Total objects", func() int64 {
    st, _ := server.Store.Stats()
    return int64(st.Objects)
})
```

Histogram 은 안 만든다 — kvfs scope 에서 latency 분포 정밀도가 필요한 hot path 가 없고, sum + count + 적당한 bucket 으로 흉내내면 코드 ×3.

## kvfs-edge 의 metric 카탈로그

`internal/edge/metrics.go` 가 전부:

```
kvfs_put_total{mode}          counter
kvfs_get_total{mode}          counter
kvfs_delete_total             counter
kvfs_objects                  gauge   (callback: Stats.Objects)
kvfs_dns_runtime              gauge   (callback: ListRuntimeDNs len)
kvfs_dns_healthy              gauge   (callback: Heartbeat.HealthyAddrs len)
kvfs_wal_last_seq             gauge   (callback: WAL.LastSeq)
kvfs_wal_batch_size           gauge   (ADR-036, 그룹커밋 effect)
kvfs_wal_durable_lag_seconds  gauge   (ADR-036, 디스크 stall detection)
kvfs_chunker_pool_bytes       gauge   (ADR-037, scratch pool memory)
kvfs_election_state           gauge   (0/1/2 = follower/candidate/leader)
kvfs_election_term            gauge
kvfs_election_term_changes_total counter (failover 빈도)
```

13 metric. cardinality 작음 (mode 2 가지, election state 3 가지). high-cardinality (per-bucket, per-key) 는 의도적으로 미지원 — Prometheus 의 표준 함정 회피.

## scrape

`/metrics` endpoint 가 위 텍스트를 그대로 plaintext 로 emit:

```go
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
    if s.Metrics == nil { http.Error(w, "metrics not enabled", 503); return }
    w.Header().Set("Content-Type", "text/plain; version=0.0.4")
    s.Metrics.reg.Render(w)
}
```

Prometheus 가 매 scrape interval (보통 15-60s) 마다 GET. cost = `sort metrics + Counter atomic load × labels + Gauge callback`. Stats() callback 이 가장 비싸지만 (full bucket scan) 60s 마다 한 번이라 무시.

## 활성화

`EDGE_METRICS=1` (default on). off 하려면 `EDGE_METRICS=0`. metrics 자체가 instrumented code 의 hot path 비용은 atomic.Add 한 번 — 거의 0.

## 이걸로 무엇을 보나

| 알고 싶은 것 | metric |
|---|---|
| 트래픽 mix | `kvfs_put_total{mode}` 비율 |
| 메타 압박 | `kvfs_objects` 추이 |
| WAL group commit 효과 | `kvfs_wal_batch_size` (1 나오면 효과 없음) |
| 디스크 stall | `kvfs_wal_durable_lag_seconds` (batch interval 보다 크면 의심) |
| Failover 빈도 | `kvfs_election_term_changes_total` rate |
| Chunker 메모리 | `kvfs_chunker_pool_bytes` |
| DN 죽음 | `kvfs_dns_healthy < kvfs_dns_runtime` |

Grafana dashboard 1장이면 운영 대시보드 완성. OTel 같은 풀스택은 trace 가 필요해질 때.

## 다음

[Ep.24 SIMD-style RS](24-simd-rs.md): Reed-Solomon 의 hot loop 을 SIMD 어셈블리 없이 2× 빠르게.
