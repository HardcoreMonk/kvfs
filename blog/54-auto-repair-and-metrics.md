# Episode 54 — auto-repair scheduling + Prometheus surface on coord

> **Series**: kvfs — distributed object storage from scratch
> **Wave**: P8-15 (operational polish) · **Episode**: 8
> **연결**: [ADR-062](../docs/adr/ADR-062-auto-repair-and-metrics.md) · [demo-anti-entropy-auto-metrics](../scripts/demo-anti-entropy-auto-metrics.sh)

---

## P8-15: operator 가 자는 동안

ADR-055 의 audit ticker 가 `COORD_ANTI_ENTROPY_INTERVAL` 로 도입됐을 때
의도적 결정:

> auto-trigger 는 **audit 만**. repair 는 operator 의 명시 호출.

이유는 자연스러웠다 — 그 시점엔 ec-corrupt 가 아직 없었고 (ADR-058),
throttle 도 (ADR-059), 의 unrecoverable 알림도 (ADR-061). repair ticker
가 잘못 동작하면 cluster I/O 폭발 + silent 데이터 손실. operator 의
"이번 audit 결과 OK 인가" 검토 후 trigger 가 안전.

지금은 다르다. ADR-055 ~ 061 을 거치며 detect-action loop 가 닫혔다:

- corrupt-aware reconstruct (058)
- throttle / per-stripe precision (059)
- concurrent (060)
- persistent scrubber state (061-B)
- unrecoverable signal (061-C)

ticker 가 자동 self-heal 해도 안전한 surface 가 됐다. P8-15 가 그 짝을
도입.

## Part A — `COORD_AUTO_REPAIR_INTERVAL`

```
$ docker run kvfs-coord:dev \
    -e COORD_DN_IO=1 \
    -e COORD_AUTO_REPAIR_INTERVAL=1m \
    -e COORD_AUTO_REPAIR_MAX=100 \
    -e COORD_AUTO_REPAIR_CONCURRENCY=4 ...
```

세 env. interval 이 ON 스위치. max-repairs 가 burst cap (한 tick 의 DN I/O
한도). concurrency 가 worker pool 크기 (replication + EC 둘 다).

```go
func (s *Server) StartAutoRepairTicker(ctx context.Context, interval time.Duration, opts AntiEntropyRepairOpts) {
    if interval <= 0 { return }
    go func() {
        t := time.NewTicker(interval)
        defer t.Stop()
        for {
            select {
            case <-ctx.Done():
                return
            case <-t.C:
                if s.Elector != nil && !s.Elector.IsLeader() {
                    continue
                }
                s.recordAutoRepairRun()
                rep, err := s.runAntiEntropyRepair(ctx, opts)
                ...
            }
        }
    }()
}
```

leader-only 가 핵심. follower 의 ticker 도 살아있지만 IsLeader() 가 차단.
failover 시 새 leader 가 즉시 다음 tick 부터 인계 — 작업 누락 없음.

기본 동작: `corrupt: true, ec: true` — full self-heal. 운영자가 더 보수적
이면 max-repairs 를 작게.

## Part B — `internal/coord/metrics.go`

`internal/edge/metrics.go` 가 edge 의 metric 채널이라면, P8-15 에서 coord
도 동등한 surface 갖춤. Prometheus text-exposition format, atomic.Uint64
기반 lock-free, dependency 0.

```go
type metricsHandle struct {
    reg              *metrics.Registry
    audits           *metrics.Counter
    repairs          *metrics.Counter // labels: reason
    unrecoverable    *metrics.Counter
    throttled        *metrics.Counter
    autoRepairRuns   *metrics.Counter
}
```

5 counter:

| metric | 의미 |
|---|---|
| `kvfs_anti_entropy_audits_total` | audit 호출 (manual + ticker) |
| `kvfs_anti_entropy_repairs_total{reason}` | 성공한 repair (`missing`/`corrupt`/`ec`) |
| `kvfs_anti_entropy_unrecoverable_total` | 모든 replica 잃은 chunk 발견 |
| `kvfs_anti_entropy_throttled_total` | max_repairs 초과 skip |
| `kvfs_anti_entropy_auto_repair_runs_total` | auto-repair tick 발화 |

+2 gauge: election state, 메타 객체 수.

`recordX` helper 들이 모두 nil-safe — `SetupMetrics()` 호출 안 한 코드 패스
(unit test 등) 에서도 panic 0. `/metrics` route 도 항상 wired, SetupMetrics
미호출 시 200 + 빈 body — operator probe 가 fail-closed 안 됨.

## ADR-061 의 slog.Error 와 lockstep

ADR-061 Part C 가 `out.Skipped[Mode=no_source]` → `slog.Error("anti-entropy: unrecoverable chunk", ...)` 도입. P8-15 가 같은 사이트에 counter 도 추가:

```go
for _, sk := range out.Skipped {
    switch sk.Mode {
    case repairModeNoSource:
        s.Log.Error("anti-entropy: unrecoverable chunk", ...)
        s.recordUnrecoverable() // ADR-062
    case repairModeSkip:
        s.Log.Warn("anti-entropy: target DN unreachable", ...)
    case repairModeThrottled:
        s.recordThrottled() // ADR-062
    }
}
for _, oc := range out.Repairs {
    if !oc.OK || oc.Planned { continue }
    switch oc.Mode {
    case repairModeReplication:
        s.recordRepair(oc.Reason) // missing | corrupt
    case repairModeECInline:
        s.recordRepair("ec")
    }
}
```

slog 채널: per-event detail (chunk_id 등). counter 채널: rate / threshold
alert. 둘 다 같은 detection 으로부터 — 운영자가 어느 채널이든 하나라도
보면 됨.

## 측정

```
$ ./scripts/demo-anti-entropy-auto-metrics.sh

==> Part A: seed 3 objects, then sleep so auto-repair has nothing to do
    auto-repair runs after 3s wait: 2 (≥1 expected)
    repairs{reason=missing} baseline: 0

==> Part B: rm one chunk on dn1 → wait for next tick → verify metric advanced
    rm'd d7659632ee4e3366.. on dn1
    repairs{reason=missing} after auto-tick: 1
    ✓ counter advanced by 1 (auto-repair fired and counted)

==> Part C: kill all replicas of one chunk → verify unrecoverable counter
    unmade 80dce5bbb20ae235.. from every replica
    unrecoverable_total: 0 → 6
    ✓ unrecoverable counter advanced (slog.Error + metric in lockstep)

==> /metrics snapshot (anti-entropy slice)
kvfs_anti_entropy_audits_total 6
kvfs_anti_entropy_auto_repair_runs_total 6
kvfs_anti_entropy_repairs_total{reason="missing"} 1
kvfs_anti_entropy_throttled_total 0
kvfs_anti_entropy_unrecoverable_total 6

=== PASS: ADR-062 — auto-repair scheduling + Prometheus surface verified ===
```

Part C 의 `unrecoverable_total = 6` 이 흥미. interval=2s 이라 4s 동안 2~3
tick 발화 → 매 tick 마다 같은 lost chunk 가 재발견 → counter 누적. 절대
count 가 "고유 손실 chunk 수" 가 아니라 "발견 횟수" — Prometheus `rate()`
또는 `max_over_time` 으로 dashboard 가 자연 처리. ADR + metric HELP 에 그
의미를 명시.

## 두 운영 패턴

audit ticker (ADR-055) + repair ticker (ADR-062) 의 조합으로 두 운영
mode:

**Light**: `COORD_ANTI_ENTROPY_INTERVAL=5m` 만. audit 자동, repair 는 매번
operator 검토 후 수동. PII / 규제 / 검토 의무 환경.

**Continuous self-heal**: `COORD_AUTO_REPAIR_INTERVAL=1m` + DN_IO=1 +
max-repairs=100. ticker 가 self-heal 진행, dashboard 가 metric watch,
unrecoverable > 0 알림이 페이지 트리거.

같은 cluster 에서 둘 다 set 도 가능 — audit 빠른 cadence (1m), repair 느린
cadence (10m, low max-repairs).

## "왜 이전 ticker 와 합치지 않았나"

ADR-055 의 정의가 "audit-only". 합치면 backcompat 깨짐 — 운영자가
설정한 cluster 가 갑자기 자동 repair 도 시작. 별도 env 가 명시 (operator
가 의식적으로 켬) + 안전 (default off).

## 정량

| | |
|---|---|
| 코드 | metrics.go +130 LOC, anti_entropy.go +50 LOC ticker + 20 LOC counter walk, main.go +30 LOC env wiring |
| 새 envs | `COORD_AUTO_REPAIR_INTERVAL`, `COORD_AUTO_REPAIR_MAX`, `COORD_AUTO_REPAIR_CONCURRENCY`, `COORD_METRICS` |
| 새 endpoints | `GET /metrics` on coord |
| 새 metrics | 5 counter + 2 gauge |
| 데모 | 3 stage (ticker alive → auto-heal → unrecoverable counter) |
| 테스트 | 186 PASS (185 → 186, TestMetrics_NilSafeAndRenderAfterSetup +1) |

## P8 wave 의 진짜 끝?

P8-08 ~ P8-15 = 8 ADR (055~062). detect → action → policy → polish →
**continuous + observable** 모두 closed.

```
$ docker run -e COORD_DN_IO=1 \
    -e COORD_AUTO_REPAIR_INTERVAL=1m \
    -e COORD_ANTI_ENTROPY_INTERVAL=10s \
    kvfs-coord:dev ...
```

= cluster 가 자체 self-heal + audit 진행, operator 는 `/metrics` 만 watch.
"운영 가능한 분산 storage 의 reference" 라는 frame 1 헌장 기준에 도달.

남은 P8-16 후보:
- per-shard 정확도 (repair package refactor)
- adaptive burst cap (load-aware)
- chunk-level dedupe of unrecoverable
- multi-tier Merkle (depth ≥2)
- histogram metrics (latency)

진정한 한계 효용. P8 wave 가 자연스럽게 종결되는 시점.

## 다음

P8-16 또는 새 시즌 — 운영자 결정.
