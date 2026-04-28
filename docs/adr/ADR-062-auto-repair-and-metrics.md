# ADR-062 — Auto-repair scheduling + coord /metrics surface

상태: Accepted (2026-04-27)
시즌: P8-15 (operational polish — Prometheus / continuous self-heal)

## 배경

ADR-055 가 `COORD_ANTI_ENTROPY_INTERVAL` 로 audit 의 ticker 만 도입. repair
는 의도적으로 operator 의 명시적 호출에 맡김 — "ticker 가 자동 자기힐"
이 부작용 위험. 그러나 이후 ADR-061 까지 anti-entropy 의 detect-action
loop 가 operator 가 감독 없이도 안전하게 닫힐 만큼 성숙 (corrupt-aware
reconstruct, throttle, concurrent, persistent state, unrecoverable
notification 모두 covered).

ADR-061 의 unrecoverable 알림은 `slog.Error` 만. 운영자가 ELK/Loki/Datadog
의 log query 로 알람을 만들 수 있지만, **Prometheus 가 표준인 환경**에서는
counter rate / threshold alert 이 더 자연. coord 자체는 `/metrics` surface
가 부재 — edge 만 있음 (`internal/edge/metrics.go`, `EDGE_METRICS=1`).

P8-15 는 두 갭을 묶음:

1. **`COORD_AUTO_REPAIR_INTERVAL`** — audit-only ticker 의 자연 짝. 운영자
   가 옵트인 → leader 가 주기적으로 `runAntiEntropyRepair` 실행 → 클러스터
   self-heal 이 사람 개입 없이 진행.
2. **Coord `/metrics`** — `kvfs_anti_entropy_*` counter 다섯 + election
   state / object count gauge. ADR-061 의 slog.Error 와 짝지어 alert
   policy 가능.

## 결정

### Part A — `StartAutoRepairTicker` + envs

`internal/coord/anti_entropy.go`:

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

leader-only — follower coords 의 ticker 도 살아있지만 IsLeader() 체크에
가로막힘. failover 시 새 leader 가 즉시 다음 tick 부터 자동 self-heal 인계.

`cmd/kvfs-coord/main.go` 에 3 env:

| env | default | 의미 |
|---|---|---|
| `COORD_AUTO_REPAIR_INTERVAL` | "" (off) | duration; tick 간격 |
| `COORD_AUTO_REPAIR_MAX` | 100 | 한 tick 당 max repairs (burst cap) |
| `COORD_AUTO_REPAIR_CONCURRENCY` | 4 | worker pool size (replication + EC) |

기본 동작은 `corrupt: true, ec: true` — full self-heal. 운영자가 더 보수적
으로 가려면 `MaxRepairs` 를 낮추거나 ticker 자체를 disable. burst cap 이
DN I/O fan-out 의 폭발을 막아 줌.

`COORD_DN_IO=1` 이 prerequisite — auto-repair 는 apply path. unset 시
warn + skip (boot 자체는 진행).

### Part B — `internal/coord/metrics.go`

`internal/edge/metrics.go` 패턴 그대로 옮김. counters:

| metric | type | labels | 의미 |
|---|---|---|---|
| `kvfs_anti_entropy_audits_total` | counter | — | audit 호출 수 (manual + ticker) |
| `kvfs_anti_entropy_repairs_total` | counter | reason | 성공한 repair (`missing`/`corrupt`/`ec`) |
| `kvfs_anti_entropy_unrecoverable_total` | counter | — | 모든 replica 잃은 chunk 발견 |
| `kvfs_anti_entropy_throttled_total` | counter | — | max_repairs 초과로 skip |
| `kvfs_anti_entropy_auto_repair_runs_total` | counter | — | auto-repair ticker 발화 |

gauges:

- `kvfs_coord_election_state` (0/1/2 = follower/candidate/leader)
- `kvfs_coord_objects` (메타 store 의 객체 수)

추가로 `recordX` helpers 가 nil-safe — `SetupMetrics` 호출 안 한 테스트
서버에서도 `recordAudit()` 등이 그냥 no-op. 기존 unit test 영향 0.

`/metrics` route 는 `Routes()` 에 항상 wired. handleMetrics 가 nil 체크
후 200 + 빈 body — 운영자 probe 가 절대 fail-closed 안 함.

### 두 변경의 결합

ADR-055 의 audit ticker + ADR-062 의 repair ticker = 운영 패턴 두 가지:

1. **Light**: `COORD_ANTI_ENTROPY_INTERVAL=5m` 만 → audit 만 자동, repair
   는 operator 가 검토 후 `kvfs-cli anti-entropy repair`. PII / 규제 환경.
2. **Continuous self-heal**: `COORD_AUTO_REPAIR_INTERVAL=1m` + DN_IO=1 →
   repair 가 백그라운드 진행. metric dashboard + alert (`unrecoverable >
   0` 페이지) 로 모니터.

같은 클러스터에서 둘 다 set 도 가능 — audit 은 detection-only 빠른 cadence,
repair 는 더 느린 cadence + low max_repairs.

## 측정

`scripts/demo-anti-entropy-auto-metrics.sh` 3 stage:

- **Part A — ticker alive**: 3 objects PUT, 3s 후 `kvfs_anti_entropy_auto_repair_runs_total = 2` (interval=2s 이라 1~2 tick).
- **Part B — auto-heal**: 1 chunk rm → 4s 대기 → `repairs_total{reason="missing"}` 0 → 1.
- **Part C — unrecoverable**: 한 chunk 의 모든 replica rm → `unrecoverable_total` 0 → 6 (auto-repair tick 마다 같은 chunk 가 재발견되며 카운트 증가, ADR-061 의 slog.Error 와 lockstep).

마지막에 `/metrics` 의 anti-entropy slice 출력:
```
kvfs_anti_entropy_audits_total 6
kvfs_anti_entropy_auto_repair_runs_total 6
kvfs_anti_entropy_repairs_total{reason="missing"} 1
kvfs_anti_entropy_throttled_total 0
kvfs_anti_entropy_unrecoverable_total 6
```

## 비채택 대안

| 대안 | 기각 사유 |
|---|---|
| Auto-repair 를 ADR-055 ticker 안에 통합 | `COORD_ANTI_ENTROPY_INTERVAL` 의 정의 (audit-only) 를 변경 → backcompat 깨짐. 별도 env 가 명시 + 안전 |
| Auto-repair default on | 위험: 새 cluster 가 의도와 무관하게 DN I/O 발생. opt-in 이 zone 의 일관 패턴 |
| `recordX` 에 sync.Once | atomic.Add 자체가 lock-free + counter 만 — 추가 lock 불필요 |
| Counter 를 `out.Repairs` 길이로 set (gauge) | 시간 정보 손실. `_total` counter 가 Prometheus rate 함수와 호환 (표준) |
| Counter 를 ec-summary outcomes 도 포함 | summary 는 synthetic per-call row — 청크 수 != counter 증가량 → operator 혼란. inline outcomes 만 카운트 |
| `unrecoverable_total` 을 chunk_id 별로 dedupe | ADR-062 시점에는 발견 횟수 (frequency) 로 채택. 이후 ADR-063 이 operator alert noise 감소를 위해 process-local first-seen dedupe 로 변경 |
| edge 처럼 `/metrics` 를 flag 로 default on | 따랐음 (`COORD_METRICS=0` 으로만 off). edge 와 패턴 일관 |

## 결과

+ **continuous self-heal**: leader 가 주기적으로 cluster 정정. operator
  agency 보존 (env opt-in + DN_IO prerequisite + max-repairs cap).
+ **observability surface**: 5 counter + 2 gauge. unrecoverable 알림이
  slog.Error (ADR-061) + counter 두 채널.
+ **edge 와 패턴 일관**: `internal/coord/metrics.go` 가 `internal/edge/metrics.go`
  의 thin mirror. registry / Counter API 는 `internal/metrics` 공유.
+ **186 unit tests PASS** (TestMetrics_NilSafeAndRenderAfterSetup +1).
+ 코드 작음: ~130 LoC metrics.go + ~50 LoC ticker + ~30 LoC main wiring.
- **counter 는 발견 frequency, 고유 chunk 수 아님** — auto-repair tick 마다
  unrecoverable 이 재발견되며 증가. dashboard query 측면에선 `rate()` 가
  자연스럽게 처리하지만, 절대 count 를 "고유 손실 chunk" 로 잘못 읽으면
  inflate. ADR 본문 + metric HELP 에 명시.
- **auto-repair 의 burst cap 이 max_repairs 절대값** — 클러스터 크기 비례
  scaling 미지원. 운영자가 알아서 조정. 향후 adaptive 가 P8-17 후보.
- **/metrics public 노출 시 정보** — coord 의 객체 수 / election state 노출.
  민감 환경에서는 `--metrics=false` 로 off 또는 ingress 에서 차단.

## 호환성

- env unset 시 기존 동작 0 변화. ticker 미가동, /metrics 가 wired 되어
  있지만 SetupMetrics 미호출 시 200 + 빈 body.
- `Server.metrics` field 추가. zero value safe — 기존 테스트 코드 영향 0.
- 새 `recordX` 는 internal — 외부 caller 영향 0.

## 검증

- `internal/coord/coord_test.go` 신규 `TestMetrics_NilSafeAndRenderAfterSetup`:
  pre-SetupMetrics no-op + post 5 counter / 1 label-tuple-set / Prometheus
  텍스트 output 확인.
- `scripts/demo-anti-entropy-auto-metrics.sh` 3 stage 모두 PASS — auto-repair
  ticker 발화 + counter 증가 + unrecoverable signal.
- 전체 test suite **186 PASS** (P8-14: 185 → +1).

## 후속 현황 (2026-04-28)

- `_total` 외 histogram (repair latency, audit duration) 은 ADR-063 으로 완료.
- `kvfs_anti_entropy_skipped_total{mode}` (skip / ec-deferred 도 카운트) 는 ADR-063 으로 완료.
- Adaptive auto-repair (load 감지 시 burst cap 자동 축소).
- `unrecoverable` 의 chunk-level dedupe — 첫 발견에만 알림 — ADR-063 으로 완료.
- per-shard 정확한 success / failure (repair package refactor).
- Multi-tier hierarchical Merkle (256-bucket flat → depth ≥2).
