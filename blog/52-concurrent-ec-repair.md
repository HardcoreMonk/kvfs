# Episode 52 — concurrent EC repair: 2× speedup with worker pool

> **Series**: kvfs — distributed object storage from scratch
> **Wave**: P8-13 (operational polish) · **Episode**: 6
> **연결**: [ADR-060](../docs/adr/ADR-060-concurrent-ec-repair.md) · [demo-anti-entropy-concurrent](../scripts/demo-anti-entropy-concurrent.sh)

---

## ADR-059 의 마지막 -항목

P8-12 의 ADR-059 가 per-stripe `repair.Run` 도입. operator 가 stripe 단위
정밀한 OK / Err 매핑 받게 됨. 그러나 -항목으로:

> 현재 serial loop. 큰 audit 의 wallclock = sum of per-stripe.

각 stripe 는 본질적으로 **independent** — 자기 K survivor 들에서 RS
Reconstruct + 자기 누락 shard PUT. 다른 stripe 와 무관. 자연스러운 worker
pool 후보.

## Up-front throttle partition

ADR-059 의 throttle counter 가 worker concurrent 에서는 race 위험. 해결:
schedule 결정을 **워커 시작 전** 에 하기.

```go
ecBudget := -1
if opts.MaxRepairs > 0 {
    ecBudget = opts.MaxRepairs - successCount  // 남은 budget
}

var scheduled []ecKey
for _, k := range ecKeys {
    if ecBudget == 0 || (ecBudget > 0 && len(scheduled) >= ecBudget) {
        markThrottled(k)
        continue
    }
    scheduled = append(scheduled, k)
}
// scheduled 만 워커에 dispatch — 정확히 budget 만큼
```

trade-off: 잡힌 stripe 가 fail 하면 그 budget slot 낭비. 그러나 over-cap 은
0 — operator 의 promise 보존.

## Worker pool

```go
sem := make(chan struct{}, conc)  // 동시 워커 cap
var mu sync.Mutex
var wg sync.WaitGroup

for _, k := range scheduled {
    sem <- struct{}{}
    wg.Add(1)
    go func(k ecKey) {
        defer wg.Done()
        defer func() { <-sem }()
        
        sr := ecPlanByKey[k]
        plan := repair.Plan{Repairs: []repair.StripeRepair{*sr}}
        stats := repair.Run(ctx, s.Coord, s.Store, plan, 1)
        
        mu.Lock()
        // outcome update + stats 누적
        mu.Unlock()
    }(k)
}
wg.Wait()
```

mu 의 critical section: int 누적 + slice indexed 쓰기. sub-microsecond.
contention 무시.

## 측정 — 2× 실속

```
$ ./scripts/demo-anti-entropy-concurrent.sh

==> stage 1: PUT 64 MiB EC (4+2) — 4 stripes
==> stage 2: rm 1 shard from each of 4 stripes
==> stage 3: serial repair (concurrency=1)
    elapsed=272ms, ec-inline OK=4

==> stage 4: re-corrupt
==> stage 5: concurrent repair (concurrency=4)
    elapsed=135ms, ec-inline OK=4

==> stage 6: GET via edge → sha256 intact
==> latency comparison
    serial    : 272ms
    concurrent: 135ms
    speedup   : 2.01× (concurrency=4 vs serial)
```

이론 최대 = 4×. 실측 2× 인 이유 (single-host demo):
- 6 DN 컨테이너가 같은 디스크 — I/O contention
- coord 자체 single-machine CPU
- worker dispatch + lock 미미하지만 0 아님

production multi-host (DN 들이 다른 host) 에서는 4× 에 더 가까움. demo 는
conservative 측정.

## 부수 정리 — httputil helper 추출

P8-12 simplify reviewer 가 deferred 한 항목: `?max_repairs=N` 같은 비음수
정수 query param 의 inline 파싱이 두 번째 use site (concurrency) 에서
다시 등장. 이번 ADR 에 합쳐 추출.

```go
// internal/httputil/query.go
func ParseNonNegIntQuery(q url.Values, name string, max int) (int, error) {
    v := q.Get(name)
    if v == "" { return 0, nil }
    n, err := strconv.Atoi(v)
    if err != nil { return 0, fmt.Errorf("%s: not an integer: %w", name, err) }
    if n < 0 { return 0, fmt.Errorf("%s=%d: must be non-negative", name, n) }
    if max > 0 && n > max { return 0, fmt.Errorf("%s=%d: exceeds upper bound %d", name, n, max) }
    return n, nil
}
```

callers: `max_repairs` (P8-12 의 inline 교체) + `concurrency` (이 ADR).
9 subtests cover (empty / valid / zero / upper bound / exceeds / max=0
disable / negative / non-numeric / empty value treated as missing).

다음 4번째 비음수 정수 query 가 추가될 때 같은 helper 재사용.

## "Replication path 는 왜 안 했나"

대안: replication 의 per-chunk copy 도 concurrent.

분석:
- replication copy 의 work = 1 chunk read + 1 chunk PUT (other DN). RTT-bound.
- DN side I/O 가 동일 — 같은 cluster, 같은 디스크.
- per-chunk 가 짧으니 dispatch overhead 가 비례적으로 큼.

EC 의 work 는 RS Reconstruct (CPU) + N shards PUT (parallel within stripe).
heavier work, parallelism gain 명확. ADR-060 은 EC 만 — replication 은
measurement 후 의미 있으면 별도 ADR.

## ADR-005 dividend, 마지막 회차?

repair worker 의 PUT 도 sha256 검증. concurrent 워커가 잘못해도 DN 이
거부. invariant 가 worker count 와 무관하게 보호.

ADR-005 (chunk_id = sha256) 가 ADR-008 부터 ADR-060 까지 거듭 dividend.
2 줄짜리 결정의 50+ 개월 가치. content-addressable 이 분산 storage 의
가장 근본적 invariant 였음을 retrospect 가 또 확정.

## 정량

| | |
|---|---|
| 코드 | anti_entropy.go +60 LOC (worker pool + up-front partition + concurrency wiring), httputil/query.go +20 LOC, cli +5 LOC |
| 새 helper | `httputil.ParseNonNegIntQuery` (9 subtests) |
| 새 endpoints / cli | `?concurrency=N` query · `--concurrency N` flag |
| 데모 | 6 stages, 64 MiB EC (4+2), 4-stripe corrupt, 2× speedup |
| 테스트 | 183 PASS (174 → 183, httputil +9) |

## P8 wave 의 거의 끝

P8-08 ~ P8-13 = 6 ADR (055~060) — anti-entropy 의 detection-action loop
완전 covered + operator-friendly polish (dry-run, throttle, concurrent,
precision) 모두 도입. 운영자 단일 명령:

```
$ kvfs-cli anti-entropy repair --coord URL \
    --corrupt --ec --max-repairs 1000 --concurrency 8
```

= 한 sweep 으로 cluster 의 모든 self-heal trigger + budget control + 병렬.

남은 P8-14 후보 (한계 효용):
- `repair.Run` 의 multi-stripe parallelism 활용
- Replication path concurrent
- Persistent scrubber state
- Multi-tier Merkle
- Notify on unrecoverable

P8 wave 가 충분히 production-friendly 한 영역에 도달. 자연스러운 정리
시점이 점점 가까워지는 중.

## 다음

자연 정리 또는 다른 polish. 운영자 결정.
