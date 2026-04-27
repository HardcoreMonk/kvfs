# ADR-059 — Anti-entropy repair throttle + per-stripe precision

상태: Accepted (2026-04-27)
시즌: P8-12 (operational polish)

## 배경

ADR-055~058 이 self-heal coverage 100% 도달. functional 한 빈 칸 0. 이제
operational quality 차원의 polish:

1. **Per-stripe precision**: ADR-057 의 EC pass 가 `repair.Run` 을 한 번에
   bulk plan 으로 호출 → `RunStats.Errors` 가 flat 하니 어느 stripe 가
   실패했는지 매핑 brittle. operator 가 진단 어려움.

2. **Repair throttle**: 큰 audit 결과 (수만 chunks missing) 에 대해 한 번에
   repair 시도 시 cluster 부담 ↑↑. operator 가 "한 번에 N 개만" 할 수 있는
   safety knob 부재.

ADR-058 본문이 후속 후보로 명시한 둘. 본 ADR 이 정리.

## 결정

### Part 1 — Per-stripe Run

기존 ADR-057 의 EC pass:
```go
plan := repair.Plan{Repairs: [모든 affected stripe]}
stats := repair.Run(ctx, ..., plan, 1)  // 한 번 호출
// stats.Errors 가 flat — 어느 stripe 인지 매핑 brittle
```

ADR-059:
```go
for each stripe in plan:
    plan := repair.Plan{Repairs: [그 stripe 하나만]}
    stats := repair.Run(ctx, ..., plan, 1)
    ok := stats.Repaired == 1 && len(stats.Errors) == 0
    // 이 stripe 의 모든 ec-inline outcome 의 OK 정확하게 set
```

각 호출의 stats 가 그 stripe 만 가리킴 — error message 파싱 0. 운영자가
per-shard `OK` / `Err` 보고 해당 stripe 만 진단.

부수 효과: ec-summary 의 totals 도 정확. 여러 stripe 의 stats 누적이
ambiguous 안 됨.

**Concurrent execution 의 자연 삽입점**: 현재 serial loop 이지만 future
worker pool 도입 시 같은 loop 위에서 `parallel for` 로 격상 가능 (ADR-060
후보).

### Part 2 — max_repairs throttle

`?max_repairs=N` query param. 0 / unset = unlimited (back-compat).

```go
type AntiEntropyRepairOpts struct {
    ...
    MaxRepairs int  // ADR-059
}
```

closure-shared `successCount` counter. 매 successful repair 후 ++. 다음
candidate 처리 전 `throttled() = MaxRepairs > 0 && successCount >= MaxRepairs`
체크. throttle 도달 시 `Skipped` 에 `mode: "throttled"` 로 기록.

Replication + EC 양쪽 path 모두 적용:
- replication: per-chunk
- EC: per-stripe (one stripe = one repair unit toward count)

cli flag: `--max-repairs N`.

## 운영 시나리오

```
# 큰 audit 발견. dry-run 으로 영향 미리 보기 (ADR-056)
$ kvfs-cli anti-entropy repair --coord URL --dry-run
{ "repairs": [...10000 candidates...] }

# 한 번에 100 만 repair. 부담 분산.
$ kvfs-cli anti-entropy repair --coord URL --max-repairs 100
{ "repairs": [100 OK], "skipped": [9900 throttled] }

# 측정 후 cluster 부담 OK 면 다음 100, ...반복
```

operator 가 자기 cluster 의 IO budget / time window 따라 sweep size 조절.

## 비채택 대안

| 대안 | 기각 사유 |
|---|---|
| Bytes-based throttle (`max_bytes=N`) | chunk size 분포 알기 어려움. count 가 더 직관적 |
| Rate limit (`repairs_per_sec`) | sleep 도입 — 처리 시간 늘어나고 dispatch 복잡. count cap 으로 충분 |
| Per-DN throttle (`max_per_dn`) | 한 DN 만 outage 후 모두 그 DN 으로 repair 몰릴 때 유용하지만 use case 좁음. global count 가 우선 |
| Stripe 단위 repair.Run 자체 격상 (repair package 변경) | 본 ADR 은 anti-entropy 측만 — repair package 의 bulk 호출 path 그대로 보존. ADR-046 cli 가 동일 worker 사용 |
| Errors 매핑 정밀화 (regex 로 stats.Errors 파싱) | brittle. 메시지 포맷 변경 시 깨짐. per-stripe call 이 cleaner |

## 결과

+ **per-stripe `OK` 정확도**: 각 ec-inline outcome 이 진짜로 그 stripe 의
  성공 / 실패 반영. operator 가 어느 stripe 가 망가졌는지 정확히 식별.
+ **operator-controlled sweep size**: `max_repairs` 로 큰 cluster 의 repair
  를 phase-split. cluster IO budget 안에서 운영 가능.
+ **back-compat**: 두 변경 모두 default off (max_repairs=0) 또는 outcome
  필드 정밀화 (기존 client 가 `OK==true` 만 보면 동일).
+ **throttled outcomes 가 next-pass repair 후보**: 한 번 throttled 된 chunks
  는 다음 audit 에 또 missing 으로 등장 → operator 가 다음 invocation 에서
  처리. 자연 resumability.
+ **demo 6 stages PASS**: 5 chunks rm → max_repairs=2 → 정확히 2 repaired
  + 3 throttled → 두 번째 invocation 에서 나머지 → audit clean.
- **EC pass 의 wallclock 변화 0**: per-stripe Run 도 결국 같은 RS reconstruct
  + PUT 연산. loop overhead 만 추가 — RS 가 dominant 비용이라 무시.
- **per-shard 정밀도는 여전히 stripe 단위**: repair.repairStripe 가 atomic
  reconstruct + multi-PUT — 그 안에서 일부 PUT 만 실패해도 stripe 전체
  failure. 정말 per-shard 정밀도 (PUT N 중 어느 것?) 는 repair package
  refactor 필요. ADR-060 후보.
- **MaxRepairs 의 fairness**: 현재 iteration 순서 (DN order × audit order)
  대로 처리. 어떤 DN/chunk 가 먼저인지 deterministic 이지만 operator 가
  control 못 함. priority queue 후보 (저우선).

## 호환성

- `?max_repairs` 미설정 시 동작 0 변경 (back-compat).
- `OK` field 의 의미 정밀화 — 기존 ADR-057 client 의 `OK==true` chunks
  는 여전히 `OK==true` (정밀화 후 더 정확하지만 직접 영향 없음).
- ec-summary 의 stats 누적 → 더 정확한 totals.
- repair package API 무변경.

## 검증

- `scripts/demo-anti-entropy-throttle.sh` 6 stages live:
  1. PUT 6 distinct objects
  2. rm 5 chunk files from dn1
  3. `?max_repairs=2` → 2 repaired, 3 throttled
  4. anti-entropy run → dn1 missing=3 (throttle 작동 입증)
  5. retry without cap → 3 repaired
  6. final audit → root_match=true on all DN
- 전체 test suite **174 PASS** (회귀 0).

## 후속

- **Concurrent EC repair** (ADR-060 후보): per-stripe loop 에 worker pool.
  measurement 후 결정.
- **Per-shard precision in repair package**: repair.repairStripe 의 PUT
  loop 가 per-shard outcome 반환하도록 격상.
- **Bytes-based throttle**: count 가 충분치 않은 경우 (다양한 chunk size).
- **Fair priority**: oldest divergence 우선, 또는 per-DN load balance.
- **Auto-retry on throttle**: ticker 가 throttled outcomes 다음 호출에
  자동 처리.
