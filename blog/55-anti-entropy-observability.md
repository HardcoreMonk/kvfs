# Episode 55 — anti-entropy observability completions

> **Series**: kvfs — distributed object storage from scratch
> **Wave**: P8-16 (operational polish) · **Episode**: 9
> **연결**: [ADR-063](../docs/adr/ADR-063-anti-entropy-observability-completions.md) · [demo-anti-entropy-observability](../scripts/demo-anti-entropy-observability.sh)

---

## P8-16: 잘 고치는 것 다음은 잘 보이는 것

P8-15 에서 coord `/metrics` 가 생겼다. auto-repair ticker 도 생겨서 operator
가 직접 누르지 않아도 missing / corrupt / EC 문제를 주기적으로 고친다.

그 다음 남은 문제는 "고쳤는가" 가 아니라 "운영자가 무엇을 볼 수 있는가"다.

세 빈칸이 있었다.

- audit / repair duration histogram 이 없다.
- `skip` / `ec-deferred` outcome 이 counter 로 보이지 않는다.
- unrecoverable chunk 가 auto-repair tick 마다 같은 alert 를 반복한다.

ADR-063 은 이 셋만 닫는다. 새 repair 알고리즘은 없다. 관찰성의 신호 품질을
올리는 작은 polish 다.

## Part A — histogram

`internal/metrics` 는 dependency-free Prometheus text exporter 다. Counter /
Gauge 만 있었고, ADR-063 에서 Histogram 을 추가했다.

```go
type Histogram struct {
    name, help string
    buckets    []float64
    counts     []*atomic.Uint64
    sum        *atomic.Uint64
}
```

출력은 Prometheus convention 그대로다.

```
kvfs_anti_entropy_audit_duration_seconds_bucket{le="0.1"} 4
kvfs_anti_entropy_audit_duration_seconds_bucket{le="+Inf"} 4
kvfs_anti_entropy_audit_duration_seconds_sum 0.183
kvfs_anti_entropy_audit_duration_seconds_count 4
```

coord 에 붙은 metric 은 둘:

- `kvfs_anti_entropy_audit_duration_seconds`
- `kvfs_anti_entropy_repair_duration_seconds`

operator 는 `histogram_quantile(0.99, rate(..._bucket[5m]))` 로 p99 를 본다.

## Part B — skipped counter

repair outcome 에서 성공 / throttle / unrecoverable 은 이미 counter 가 있었다.
하지만 "operator 선택으로 deferred 된 것" 은 없었다.

```go
case repairModeSkip:
    s.recordSkipped("skip")
case repairModeECDeferred:
    s.recordSkipped("ec-deferred")
```

`ec-deferred` 는 특히 중요하다. anti-entropy 가 EC shard 손상을 봤지만
operator 가 `ec=1` 을 주지 않으면 reconstruct 를 하지 않는다. JSON 응답을
놓쳐도 Prometheus 에 남아야 한다.

## Part C — unrecoverable dedupe

P8-15 의 `unrecoverable_total` 은 발견 횟수였다. auto-repair interval 이 2초면
같은 lost chunk 하나가 1분에 30번 카운트될 수 있다.

ADR-063 은 process-local seen set 을 둔다.

```go
if s.markUnrecoverableFirstSeen(sk.ChunkID) {
    s.Log.Error("anti-entropy: unrecoverable chunk", ...)
    s.recordUnrecoverable()
}
```

이제 counter 와 error log 는 같은 chunk 에 대해 첫 발견 때만 올라간다.
coord 재시작 후에는 다시 알릴 수 있다. 100k cap 에 도달하면 set 을 비우고
재알림을 허용한다. alert noise 보다 bounded memory 가 우선이다.

## 데모

```
$ ./scripts/demo-anti-entropy-observability.sh

==> Part A: duration histograms — manual repair then dump _bucket lines
    audit + repair counts via histogram:
    ...

==> Part B: skipped counter — PUT an EC object, rm one shard, repair without ec=1
    skipped_total{mode=ec-deferred}: 1

==> Part C: unrecoverable dedupe — auto-repair tick rediscovers, counter doesn't
    unrecoverable_total: 0 -> 1
    auto_repair_runs_total: 4
```

중요한 invariant 는 마지막 줄이다. tick 은 여러 번 돌았지만
`unrecoverable_total` 은 같은 chunk 에 대해 한 번만 오른다.

## 정리

ADR-063 은 self-heal 의 능력을 늘리지 않는다. 이미 닫힌 loop 를 운영자가
덜 시끄럽고 더 정확하게 볼 수 있게 한다.

P8 의 남은 저우선 후보는 더 큰 구조 변화다: per-shard precision,
multi-tier Merkle, peer-to-peer DN self-heal, adaptive scheduling. ADR-063 은
그 전에 싸고 확실한 observability gap 만 닫았다.

