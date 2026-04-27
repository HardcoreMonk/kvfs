# Episode 51 — repair throttle + per-stripe precision

> **Series**: kvfs — distributed object storage from scratch
> **Wave**: P8-12 (operational polish) · **Episode**: 5
> **연결**: [ADR-059](../docs/adr/ADR-059-throttle-and-precision.md) · [demo-anti-entropy-throttle](../scripts/demo-anti-entropy-throttle.sh)

---

## Functional 100% → operational quality

ADR-055~058 가 self-heal coverage 100% 도달. functional 빈 칸 0. 이제
operational quality polish:

1. **Per-stripe precision**: ec-summary 의 `ok` 가 ambiguous — operator
   가 어느 stripe 망가졌는지 진단 어려움
2. **Repair throttle**: 큰 audit 의 byte movement 한계 두기. cluster
   IO budget 보호

작은 polish 두 갈래. 한 ADR (ADR-059) 묶음.

## Per-stripe precision

ADR-057 의 EC pass 가 `repair.Run` 한 번에 bulk plan 호출:

```go
plan := repair.Plan{Repairs: [모든 affected stripes]}
stats := repair.Run(ctx, ..., plan, 1)
// stats.Errors 가 flat — 어느 stripe 인지 매핑 brittle
```

문제: `stats.Errors` 가 `[]string` flat list. 각 메시지에 Bucket/Key/StripeIndex
포함되지만 그걸로 구조화된 outcome 매핑은 brittle (메시지 포맷 변경 시 깨짐).

ADR-059 — **per-stripe Run**:

```go
for each stripe in plan:
    plan := repair.Plan{Repairs: [그 stripe 하나만]}
    stats := repair.Run(ctx, ..., plan, 1)
    ok := stats.Repaired == 1 && len(stats.Errors) == 0
    // 이 stripe 의 모든 ec-inline outcome 의 OK / Err 정확히 set
```

각 호출의 stats 가 그 stripe 만 가리킴. error message 파싱 0. operator 가
per-shard outcome 보고 정확한 진단.

부수 효과: ec-summary 의 totals 도 정확. 누적 시 ambiguous 안 됨.

또: per-stripe loop 이 **future concurrent execution 의 자연 삽입점**.
worker pool 도입 시 같은 loop 위에서 `parallel for`. ADR-060 후보.

## max_repairs throttle

`?max_repairs=N` query param. 0 / unset = unlimited (back-compat).

```go
type AntiEntropyRepairOpts struct {
    ...
    MaxRepairs int  // ADR-059
}

successCount := 0
throttled := func() bool {
    return opts.MaxRepairs > 0 && successCount >= opts.MaxRepairs
}

repairOne := func(...) {
    if throttled() {
        out.Skipped += { mode: "throttled" }
        return
    }
    // ... 실제 repair
    if oc.OK { successCount++ }
}
```

Replication + EC 양쪽 path 모두. 매 successful repair 후 ++.

## 운영 시나리오

```bash
# 큰 audit 결과
$ kvfs-cli anti-entropy repair --coord URL --dry-run
{ "repairs": [...10000 candidates...] }

# 한 번에 100 만 repair. byte movement 분산.
$ kvfs-cli anti-entropy repair --coord URL --max-repairs 100
{ "repairs": [100 OK], "skipped": [9900 throttled] }

# IO budget OK 면 다음 100, 또 다음 ...
```

operator 가 sweep size 직접 조절. cluster 의 IO budget / time window 안에서
운영.

## 데모

```
$ ./scripts/demo-anti-entropy-throttle.sh

==> stage 1: PUT 6 distinct objects
==> stage 2: rm 5 chunk files from dn1
==> stage 3: anti-entropy/repair?max_repairs=2
    {"repaired":2,"throttled_skipped":3}
    ✓ throttle stopped at 2, 3 marked throttled
==> stage 4: anti-entropy run → dn1 still missing 3
==> stage 5: retry without cap
    {"repaired":3}
==> stage 6: final audit → all root_match=true

=== PASS ===
```

6 stages, 정확한 throttle 동작 + resumability (next call 이 throttled
이전 결과를 다시 처리).

## "왜 bytes-based throttle 안 했나"

대안: `?max_bytes=N` — N bytes 까지 만 byte movement.

장점: byte movement = 진짜 cluster 부담의 metric.
단점: chunk size 분포 알기 어려움. operator 가 추정해야 — N MiB 면
몇 chunks?

count-based 가 더 직관적: "10 chunks 만 옮겨줘" = clear. byte budget 안
되도록 측정 (chunk size × count) 가 operator 의 일.

count + 작은 size limit (예: streaming chunk 4MiB) 면 wallclock predictable.

## "Resumability 의 미덕"

throttled outcomes 가 자동 resumable:

```
첫 호출: max_repairs=100 → 100 repaired + 9900 throttled
두 번째: max_repairs=100 → 다음 100 처리 (audit 가 다시 실행, 그들이 missing 으로 잡힘)
...
100 번째: max_repairs=100 → 마지막 100, audit clean
```

각 호출이 stateless. throttled state 가 cluster 에 저장 안 됨 — 단지 audit
가 매번 fresh. **idempotent + resumable** = production-friendly.

## ADR-058 후 polish 의 한계 효용

ADR-055~058 의 functional 4채널 (replication missing/corrupt + EC missing/
corrupt) 은 운영 cluster 의 self-heal 핵심. ADR-059 의 polish 는 그
위의 quality:
- precision: 진단 친화
- throttle: 운영 안전

이 둘이 production-readiness 의 큰 부분 — functional 위의 operational layer.
하지만 polish 의 한계 효용은 점점 줄어듦. ADR-060+ 후보 (concurrent,
persistent state 등) 도 더 작은 절대적 이득.

자연스러운 wave 정리 시점.

## 정량

| | |
|---|---|
| 코드 | anti_entropy.go +50 LOC (per-stripe loop + throttle counter + ec-summary 정확도), cli +5 LOC, ADR + blog |
| 새 endpoints / cli flags | `?max_repairs=N` query / `--max-repairs N` |
| 데모 | 6 stages, full throttle behavior |
| 테스트 | 회귀 0 (174 PASS) |

## 다음

P8-12 가 wave 의 자연 정리 시점. 다음 후보:
- 더 큰 polish (concurrent repair, persistent scrubber state)
- 운영 자료 정비 (final state announce, README polish)
- 새로운 wave / 시즌 / fork

각 결정은 operator 의 영역.
