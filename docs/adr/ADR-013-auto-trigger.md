# ADR-013 — Auto-trigger Policy (운영자 개입 없이 클러스터 스스로 정렬)

## 상태
Accepted · 2026-04-26 · Season 3 Ep.1 첫 결정

## 맥락

ADR-010 (rebalance) 와 ADR-012 (GC) 가 만든 `kvfs-cli rebalance --apply` /
`kvfs-cli gc --apply` 는 운영자가 **명시적으로** 호출해야 한다. demo-eta /
demo-theta 가 이 패턴을 시연:

```
사용자가 dn4 추가 → 사용자가 rebalance 명령 → 사용자가 GC 명령
```

작은 클러스터에선 OK. 그러나:

1. **DN 추가/제거가 빈번** 한 환경에서는 매번 손으로 호출하는 게 부담
2. **잊기 쉬움** — 한 달 후 surplus 가 디스크 50% 차지하고 있는 걸 발견
3. **rebalance 의 partial failure** — 다음 run 이 자동으로 재시도 안 되면 영구 미해결

분산 스토리지의 **운영 부담 감소** 가 다음 설계 단계.

### 대안 검토

| 방식 | 장점 | 단점 |
|---|---|---|
| **In-edge time-based loops (이번 ADR)** | 추가 daemon 0, 기존 mutex 재활용, 단순 cron 모델 | edge HA / 다중 인스턴스 시 중복 실행 (현재 single-edge 라 무관) |
| External cron 호출 | edge 코드 변경 0, 설정 파일 분리 | systemd / k8s / ECS 별 별도 배포, 관찰성 분산 |
| Event-driven (DN heartbeat 변경 감지) | 즉각 반응 | DN 등록·heartbeat 인프라 필요 (별도 ADR) |
| Pull-based DN 광고 | DN 자체가 의도 알림 | DN ↔ edge 양방향 프로토콜 필요 |
| Manual only (현 상태 유지) | 운영자가 통제 | 위 3 부담 그대로 |

kvfs 의 single-edge MVP 에서는 **in-edge ticker** 가 압도적으로 단순. 외부 cron 도 가능하지만 설정·관찰성 분산. 미래에 multi-edge 가 되면 ADR-022 (leader election) 이 받음.

## 결정

### 핵심 모델

edge 안에 **2 개의 background goroutine** (rebalance ticker, GC ticker). 각 ticker 가 주기적으로:

```
rebalance loop (every RebalanceInterval):
    s.rebalanceMu.Lock()
    plan = rebalance.ComputePlan(...)
    if plan.Migrations > 0:
        stats = rebalance.Run(...)
        record(AutoRun{Job:"rebalance", ...})
    s.rebalanceMu.Unlock()

gc loop (every GCInterval):
    s.gcMu.Lock()
    plan = gc.ComputePlan(..., minAge)
    if plan.Sweeps > 0:
        stats = gc.Run(...)
        record(AutoRun{Job:"gc", ...})
    s.gcMu.Unlock()
```

**결정적 안전 보장**:
- 동일한 `rebalanceMu` / `gcMu` 사용 → 수동 `--apply` 와 자동이 **완벽히 직렬화** (race 0)
- min-age = 60s (기본) → auto GC 의 자동 실행이 freshly-PUT 청크 절대 안 건드림
- 둘 다 plan 0 이면 fast-path (Run 호출 안 함, mutex 만 잡았다 놓음)

### 설정 (env vars)

```
EDGE_AUTO                       1/0      default 0  — kill switch
EDGE_AUTO_REBALANCE_INTERVAL    duration default 5m  (e.g. "30s", "10m")
EDGE_AUTO_GC_INTERVAL           duration default 15m
EDGE_AUTO_GC_MIN_AGE            duration default 60s (= gc.DefaultMinAge)
EDGE_AUTO_CONCURRENCY           int      default 4
```

기본값 OFF. 운영자가 명시적으로 `EDGE_AUTO=1` 설정해야 자동화 활성. **opt-in** 보수적 default.

### 관찰성

신규 엔드포인트:
- `GET /v1/admin/auto/status` — 현재 config + 최근 N 회 run 결과 (ring buffer 길이 32)

각 `AutoRun`:
```go
type AutoRun struct {
    Job        string    // "rebalance" | "gc"
    StartedAt  time.Time
    DurationMS int64
    PlanSize   int       // migrations / sweeps planned
    Stats      map[string]any  // job-specific
    Error      string    // empty = success
}
```

slog 로 시작/끝 로그 (job, duration, plan_size, applied/failed, bytes).

CLI:
```
kvfs-cli auto --status                  # 마지막 run / config 표시
kvfs-cli auto --status -v               # 최근 32 run 전체
```

### 명시적 비범위

- **multi-edge leader election** — ADR-022 후보. 현재 single-edge 가정
- **이벤트 기반 즉시 반응** — DN heartbeat 인프라 + push 모델은 ADR-014 (Meta backup/HA) 와 함께 풀 주제
- **EC re-encoding** — DN 변경 시 EC stripe 재배치는 rebalance 확장 + Stripes 처리 필요. 별도 ADR-024 후보 (현재 EC rebalance 미구현 상태에서 자동화도 무의미)
- **rate limiting / max-per-cycle** — 현재 ComputePlan 이 한 번에 모든 misplaced 처리. 100만 객체 환경에선 cap 필요. ADR-023 후보
- **alert / notification** — Failed > 0 시 운영자 알림 메커니즘. slog warn 만으로 시작

## 결과

### 긍정

- **운영 부담 0 (default off, opt-in)** — `EDGE_AUTO=1` 한 줄로 하루 종일 클러스터 스스로 정렬
- **수동/자동 안전 공존** — 동일 mutex 사용, 운영자가 언제든 `--apply` 호출 가능
- **알고리즘 0 변경** — rebalance / GC 의 코드 한 줄도 안 건드림. trigger 만 추가
- **추가 의존 0** — Go stdlib `time.Ticker` + `context.Context` + slog
- **관찰 단순** — `/v1/admin/auto/status` 한 엔드포인트 + slog
- **fail-soft** — 한 cycle 의 error 가 다음 cycle 막지 않음. 자동 재시도

### 부정

- **multi-edge 이중 실행** — 두 edge 인스턴스가 같은 메타에 동시 auto-rebalance 호출 시 두 번째가 mutex 안 걸려 race. **single-edge MVP 가정** 으로 current scope OK
- **고정 cadence** — load 가 높을 때 일시 멈춤 같은 적응적 동작 없음. 단순 ticker
- **edge restart 시 history 소실** — ring buffer in-memory. 영속 history 가 필요하면 별도 store
- **시간 정확성 의존** — wall clock 변경 시 ticker 가 점프 가능. `time.Ticker` 의 monotonic 특성에 의존

### 트레이드오프 인정

- "왜 이벤트 기반 안 함?" — DN heartbeat / push 인프라가 ADR-014 와 묶임. 시간 기반이 압도적으로 단순. 이벤트 기반은 후속
- "왜 default off?" — 자동 삭제 (GC) 가 포함됨. opt-in 이 보수적 default. 운영자가 ADR-012 의 안전망 (claimed-set + min-age) 을 이해한 후 활성
- "왜 rebalance interval 5분?" — DN 변경이 빈번해도 5분 내 정렬. 30초로 줄여도 OK (cycle CPU 비용 무시 가능). 5분이 보수적 default
- "왜 GC interval 15분?" — min-age 60s 와 분리된 concept. GC 의 비용은 list /chunks × DN 수 + delete 호출. 5분마다는 너무 잦을 수 있음

## 데모 시나리오 (`scripts/demo-lambda.sh`)

```
1. ./scripts/down.sh + start dn1~dn3 + edge with EDGE_AUTO=1 +
   EDGE_AUTO_REBALANCE_INTERVAL=10s + EDGE_AUTO_GC_INTERVAL=15s +
   EDGE_AUTO_GC_MIN_AGE=5s (demo 용 짧게)
2. PUT 4 seed objects on 3 DN
3. start dn4 + restart edge (with EDGE_DNS expanded + AUTO 그대로)
4. 청크 디스크 분포 확인 (dn4=0)
5. 10초 sleep → 자동 rebalance 가 misplaced 청크 dn4 로 복사
6. 10초 sleep → 자동 GC 가 surplus 청크 (이제 메타에서 빠진 dn1~dn3 청크) 삭제
7. /v1/admin/auto/status 로 history 확인
8. 디스크 분포 = 메타 의도 (자동으로!)
```

## 관련

- ADR-010 — Rebalance worker (이 ADR 가 자동 호출하는 알고리즘)
- ADR-012 — Surplus chunk GC (이 ADR 가 자동 호출하는 알고리즘 + min-age 안전망 의존)
- ADR-009 — Rendezvous Hashing (rebalance 의 desired 계산)
- `internal/edge/` — `Server.StartAuto(ctx)` + auto loops (후속)
- `cmd/kvfs-edge/` — env 파싱 (후속)
- `cmd/kvfs-cli/` — `auto` 서브커맨드 (후속)
- `scripts/demo-lambda.sh` — 시연 (후속)
- `blog/07-auto-trigger.md` — narrative episode (후속)

## 후속 ADR 예상

- **ADR-022 — Leader election for multi-edge auto**: edge 가 N 개일 때 한 인스턴스만 auto loop 돌림
- **ADR-023 — Auto-trigger rate limiting**: 한 cycle 의 max migrations / sweeps cap
- **ADR-024 — EC stripe rebalance + auto re-encode**: EC 객체의 자동 재배치 (ADR-008 의 stripe-level placement 변경 처리)
- **ADR-025 — Adaptive cadence**: load / pending work 에 따라 interval 조절
