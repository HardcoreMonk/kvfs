# ADR-060 — Anti-entropy concurrent EC repair + httputil query helper

상태: Accepted (2026-04-27)
시즌: P8-13 (operational polish)

## 배경

ADR-059 의 per-stripe `repair.Run` 루프가 serial. 한 stripe 의 reconstruct
+ 멀티 PUT 이 끝나야 다음 stripe 시작 → 큰 audit (수십 stripe missing) 에서
wallclock 누적.

각 stripe 의 repair 는 본질적으로 **independent**:
- 입력: 자기 stripe 의 K survivor shards (다른 stripe 와 무관)
- 작업: Reed-Solomon Reconstruct (CPU)
- 출력: 누락 shard 의 PUT (DN I/O, 보통 다른 DN 들에)

→ 자연스러운 parallelization 후보. ADR-060 이 worker pool 도입.

부수 정리: ADR-053/059 가 비음수 정수 query param 파싱을 두 곳에 inline 으로
가짐 (max_repairs, X-KVFS-W/R). ADR-060 이 세 번째 (concurrency) 를 추가하므로
P8-12 simplify reviewer 가 deferred 한 helper 추출도 본 ADR 에 합침.

## 결정

### Part 1 — `httputil.ParseNonNegIntQuery`

```go
// internal/httputil/query.go
func ParseNonNegIntQuery(q url.Values, name string, max int) (int, error)
```

semantics:
- 빈 값 / 미설정 → `(0, nil)` (caller 가 default 사용)
- 음수 → 400-mappable error
- max > 0 시 N > max 면 error
- max == 0 시 upper bound off

callers:
- `handleAntiEntropyRepair` 의 `max_repairs` (P8-12 의 inline 패턴 교체)
- `handleAntiEntropyRepair` 의 `concurrency` (본 ADR)

기존 `parseQuorumHeader` (edge 의 X-KVFS-W/R 헤더) 는 헤더 path 라 별도 — query
에 query helper, 헤더에 헤더 helper. naming 혼선 회피.

### Part 2 — Concurrent EC repair

새 opt:
```go
type AntiEntropyRepairOpts struct {
    ...
    Concurrency int  // 0/1 = serial; ≥2 = worker pool size
}
```

`?concurrency=N` query param. cli `--concurrency N` flag.

Algorithm:

1. **Up-front throttle partition** (ADR-059 와 호환): 남은 budget 만큼만
   `scheduled` 슬라이스에 추가. 나머지는 즉시 `repairModeThrottled` 로
   mark. → 워커는 deterministic schedule 만 처리 — counter race 없음.
2. **Worker pool**: `sem := make(chan struct{}, conc)` semaphore. 매
   stripe 마다 `wg.Add(1)`, `sem <- struct{}{}`, goroutine 시작 (defer
   `<-sem`).
3. **Critical section** (mu): out.Repairs index update + stats 누적
   (attemptedStripes, totalRepaired, totalFailed, totalBytes, allErrors)
   + successCount.
   - 모두 ints + slice 의 indexed 쓰기 — sub-microsecond. 워커 간 contention
     무시 가능.
4. `wg.Wait()` 후 ec-summary append (with concurrency 표시).

Replication path 는 serial 그대로 — per-chunk 작업이 RTT-bound (CPU 0)
이라 pool 효과 작음. (operational measure 결과 보고 미래 ADR 가능.)

### Throttle 의 race 회피

ADR-059 의 closure-counter 가 worker concurrent context 에서는 race 위험.
해결: 워커 시작 **전** 에 schedule 결정. 각 stripe 가 reserved budget 만
받음. 워커가 늦게 끝나도 over-cap 0.

trade-off: 모든 잡힌 stripe 가 succeed 해야만 정확히 budget 만큼 진행. 일부
fail 시 useful 한 budget 슬롯이 낭비될 수 있음 — 그러나 이는 fail 도 "tried
once" 의 비용 인정 (재시도 정책은 caller 의 일).

## 측정

`scripts/demo-anti-entropy-concurrent.sh` 6 stages:
- 64 MiB EC (4+2) PUT → 4 stripes
- 4 stripes 각각 1 shard rm
- serial (concurrency=1) repair: 272ms
- concurrent (concurrency=4) repair: 135ms
- speedup: **2.01×**

이론 최대 = 4× (4 worker × 1 stripe each in parallel). 실측 2× 인 이유:
- DN I/O 가 같은 docker host 의 같은 디스크에 contention
- coord 자체의 single-machine CPU
- worker pool dispatch + lock contention 미미하지만 0 아님

production cluster (DN 들이 별 host) 에서는 4× 에 가까울 가능성. demo 는
single-host 라 conservative 결과.

## 비채택 대안

| 대안 | 기각 사유 |
|---|---|
| `successCount` 를 atomic.Int64 로 + CAS reserve | reserve 후 fail 시 over-count 또는 release 복잡. up-front partition 이 더 단순 + 결정적 |
| Replication path 도 concurrent | 전체 작업 RTT-bound, CPU 한 갑 안 둠. measurement 후 미래 ADR |
| `repair.Run(plan, concurrency)` 의 기존 concurrency 인자 사용 | 그건 한 plan 안의 stripes 동시 처리. 본 ADR 은 per-stripe Run 별 dispatch — outcome 매핑 정밀도 (ADR-059) 위해 필수 |
| Default concurrency > 1 | back-compat 깨짐. opt-in 이 안전 |
| Worker count 의 자동 조정 (`runtime.NumCPU()`) | operator 의 cluster size / IO budget 와 무관. 명시 지정이 정직 |

## 결과

+ **2× speedup** on 4-stripe demo (single host, single disk). production
  multi-host 에서 더 큼.
+ **Correctness 무변경**: per-stripe Run 의 invariant 그대로. 출력 shape
  도 동일.
+ **httputil 추출**: `ParseNonNegIntQuery` 1 helper, 2 use site (max_repairs
  + concurrency). 미래 4번째 use site 등장 시 같은 helper 재사용.
+ **183 unit tests PASS** (httputil +9 subtests, 회귀 0).
- **Replication path 미적용**: 본 ADR 은 EC만. replication 의 concurrent
  copy 는 별도 trade-off 분석 후.
- **Single-machine demo**: 이론 4× 대비 2× — operator 가 자기 환경에서
  measure 권장.
- **mu 의 critical section 단순화 가능성**: `out.Repairs` 의 index-쓰기는
  sync.Map 또는 lock-free 가능. 측정 후 의미 있을 때 refactor.

## 호환성

- `?concurrency` 미설정 → 0 → serial (back-compat with ADR-059).
- `Concurrency` field 추가, zero value 안전.
- `ec-summary` Err 메시지에 `concurrency=N` 추가 — 새 정보, 기존 parser
  영향 0.
- `httputil` 새 함수 — 기존 import 영향 0.

## 검증

- `internal/httputil/query_test.go` 신규: TestParseNonNegIntQuery 9 subtests
  (empty, valid, zero, upper bound, exceeds, max=0 disable, negative,
  non-numeric, empty value).
- `scripts/demo-anti-entropy-concurrent.sh` 6 stages: 64MB EC PUT →
  4-shard rm → serial 272ms → 동일 corrupt 재현 → concurrent=4 135ms →
  GET sha256 intact → speedup 2.01×.
- 전체 test suite **183 PASS**.

## 후속 (P8-14 후보)

- `repair.Run` 자체의 concurrency 인자 활용 (한 plan 의 multi-stripe).
- Replication path concurrent.
- Persistent scrubber state (DN 재시작 시 corrupt set 보존).
- Multi-tier hierarchical Merkle.
- Notify on unrecoverable.
