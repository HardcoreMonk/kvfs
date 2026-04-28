# ADR-063 — Anti-entropy observability completions

상태: Accepted (2026-04-27)
시즌: P8-16 (operational polish)

## 배경

ADR-062 로 coord `/metrics` 가 생겼지만 anti-entropy 운영 화면에는 세 빈칸이
남았다.

1. audit / repair 가 얼마나 오래 걸리는지 duration distribution 이 없음.
2. `skip` / `ec-deferred` outcome 은 JSON 응답에만 보이고 Prometheus counter
   로 집계되지 않음.
3. unrecoverable chunk 는 auto-repair tick 마다 재발견되어
   `kvfs_anti_entropy_unrecoverable_total` 과 `slog.Error` 가 계속 증가.

P8-16 은 새 self-heal 기능이 아니라 **관찰성의 한계효용 polish** 다. 이미
있는 anti-entropy loop 의 의미를 바꾸지 않고 operator 가 noisy / blind
spot 없이 볼 수 있게 한다.

## 결정

### Part A — stdlib histogram

`internal/metrics` 에 `Histogram` 을 추가한다. 외부 Prometheus client 의존은
계속 도입하지 않는다.

- fixed bucket ladder
- cumulative `_bucket{le="..."}`
- `_sum`, `_count`
- label 없음 (필요하면 metric 을 분리)
- observation 단위는 seconds

coord 는 두 histogram 을 등록한다.

| metric | 의미 |
|---|---|
| `kvfs_anti_entropy_audit_duration_seconds` | `runAntiEntropy` wallclock |
| `kvfs_anti_entropy_repair_duration_seconds` | `runAntiEntropyRepair` wallclock |

bucket 은 1ms ~ 30s 의 고정 ladder. Prometheus 에서는
`histogram_quantile()` 로 p95 / p99 를 계산한다.

### Part B — skipped counter

`kvfs_anti_entropy_skipped_total{mode}` counter 를 추가한다.

| mode | 의미 |
|---|---|
| `skip` | target DN 이 repair 시점에 unreachable |
| `ec-deferred` | EC shard 문제를 발견했지만 `ec=1` opt-in 없이 호출 |

`no_source` 는 기존 `unrecoverable_total`, `throttled` 는 기존
`throttled_total` 이 담당하므로 중복하지 않는다.

### Part C — unrecoverable dedupe

coord process 안에 `unrecoverableSeen map[chunk_id]struct{}` 를 둔다. 같은
chunk_id 가 auto-repair tick 으로 반복 발견되어도 첫 발견만 `slog.Error` 와
`recordUnrecoverable()` 를 발생시킨다.

bounded set 이다. 100k 개에 도달하면 map 을 비우고 다음 발견부터 다시 알린다.
메모리를 무한히 쓰지 않는 쪽이 우선이고, 대규모 손실 상황에서 re-alert 는
fail-safe 다.

## 결과

+ audit / repair latency 가 Prometheus histogram 으로 보인다.
+ skip / EC opt-in 누락도 counter 로 집계된다.
+ unrecoverable alert noise 가 "tick 횟수" 에서 "고유 chunk 첫 발견" 으로
  바뀐다.
+ ADR-062 의 `/metrics` surface 를 확장하는 작은 변경으로 끝난다.
- dedupe 는 process-local 이다. coord 재시작 후 같은 chunk 는 다시 first-seen
  으로 처리된다.
- 100k cap reset 후에는 재알림이 가능하다. 의도된 fail-safe.
- histogram 은 label-less. DN 별 / mode 별 latency breakdown 은 하지 않는다.

## 검증

- `TestHistogram_CumulativeBucketsSumCount`
- `TestHistogram_NegativeClampsToZero`
- `TestMetrics_HistogramsAndSkippedRender`
- `TestUnrecoverableDedupe_FirstSeenOnceThenSilent`
- Docker `golang:1.26-alpine` 기준 `go test ./...` PASS
- Docker `golang:1.26-alpine` 기준 `go vet ./...` PASS
- `scripts/demo-anti-entropy-observability.sh` PASS
