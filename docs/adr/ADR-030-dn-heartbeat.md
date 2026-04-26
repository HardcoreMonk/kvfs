# ADR-030 — DN heartbeat (in-edge pull-based liveness monitor)

## 상태
Accepted · 2026-04-26

## 맥락

ADR-027 이 동적 DN registry 를 줬지만 add/remove 가 **운영자 admin endpoint
호출**로만 일어난다. dn3 가 kernel panic 으로 그냥 죽으면:

- `ListRuntimeDNs()` 는 여전히 dn3 반환
- coordinator 가 dn3 에 PUT/GET 시도 → 실패 → 매 요청마다 timeout 후 quorum
- 운영자는 사용자 보고서 / Grafana 대시보드 / journalctl 보고 알아챈 다음 수동 remove

운영성 트랙(Season 3)의 다음 단계는 이 "**자동 dead 감지**" 의 가장 단순한
형태부터 추가한다. ADR-013 auto-trigger 패턴을 그대로 차용 (in-edge ticker
+ opt-in env, sub-process 신규 X).

## 결정

### Pull-based 선택 이유

| 옵션 | pull (edge → DN) | push (DN → edge) |
|---|---|---|
| 인증 surface | 0 (DN 은 unauthenticated `/healthz` 이미 노출) | DN 이 edge 신뢰 + auth 로직 추가 필요 |
| 시간 동기화 | 불필요 (edge 본인 시각 사용) | last_seen 비교 위해 DN ↔ edge 시간 정확 |
| DN 변경 영향 | 0 (DN 코드 unchanged) | 모든 DN 에 heartbeat 클라이언트 배포 |
| 부하 위치 | edge (한 곳, fan-out tunable) | DN N 개 (N 마다 outbound) |
| ADR-027 와 정합 | 동일 (edge 가 권위적 registry 보유) | DN 이 자기 active 알리는 모델은 보강 필요 |

`pull` 은 ADR-013 (in-edge ticker) 패턴과도 일관. 추가 의존성·코드 0.

### 모듈 구조

`internal/heartbeat/` — 새 패키지 (~150 LOC):
- `Probe` 인터페이스 (testable)
- `Monitor` (RWMutex + per-DN `Status`)
- `Tick(ctx, addrs)` — 모든 addr 병렬 probe, registry 변경 시 자동 status drop
- `Snapshot()`, `HealthyAddrs()`

`cmd/kvfs-edge/main.go` — `httpHealthProbe` (실 HTTP/HTTPS), TLS 가 켜져있으면
edge↔DN 과 동일 transport 사용 (ADR-029 재사용).

`internal/edge/edge.go`:
- `Server.Heartbeat *heartbeat.Monitor` (nil 허용)
- `StartHeartbeat(ctx, interval)` — ticker 루프 (ADR-013 과 동일 패턴)
- `GET /v1/admin/heartbeat` — `{"enabled": bool, "statuses": [...]}` JSON

### 환경 변수

- `EDGE_HEARTBEAT_INTERVAL` — default `10s`. `0s` 면 monitor 비활성 (모듈 로드 X)
- `EDGE_HEARTBEAT_FAIL_THRESHOLD` — default `3` (3 연속 실패 → `Healthy=false`)

### Healthy 판정

`consec_fails >= threshold` 일 때 `Healthy=false`. 일시적 jitter (1-2 회 fail)
는 무시. 복구 시 다음 성공 probe 가 즉시 `Healthy=true` 로 회복 (latch X).

### Auto-eviction

**본 ADR 범위 외**. `HealthyAddrs()` 가 placement 에 통합되거나 auto-trigger
가 unhealthy DN 을 자동 remove 하는 건 후속 ADR (ADR-022 multi-edge HA 와 함께
다룰 가능성). 현재는 운영자 visibility 만 제공 — admin 이 보고 결정.

## 결과

**긍정**:
- 운영자가 `kvfs-cli heartbeat` 한 번으로 클러스터 보건 즉시 파악
- ADR-013 / ADR-027 와 동일 패턴 — 학습 곡선 X
- 추가 의존성 0, 의존성 영향 0 (DN 코드 unchanged)
- Probe 인터페이스로 테스트 격리, 6 unit tests PASS (healthy / threshold /
  recovery / shrink / 빈 입력)

**부정**:
- 운영자 시각화에 머묾 — 자동 placement 회피·자동 evict 는 별도 작업
- 10s interval = max 10s 의 stale view (RPO 와 trade-off)
- N 개 DN × 10s = N/10 RPS 부하. N=100 시 10 RPS — 무시할 수준이지만 N=1000+
  에서는 interval 늘리거나 sampling 도입 후보

**트레이드오프**:
- `Healthy=false` 후 즉시 placement 에서 빠지지 않음 — coordinator 가 PUT 실패
  하고 quorum 깨질 수 있음. 안전 default 는 "sleep on it" (운영자가 결정)
- `last_seen` 시각이 edge wall clock 기준 — multi-edge 는 ADR-022 과제

## 관련

- `internal/heartbeat/heartbeat.go` — Monitor + Probe (~150 LOC)
- `internal/heartbeat/heartbeat_test.go` — 6 tests PASS
- `internal/edge/edge.go` — `StartHeartbeat` + `handleHeartbeat`
- `cmd/kvfs-edge/main.go` — `httpHealthProbe` factory
- `cmd/kvfs-cli/main.go` — `heartbeat` 서브커맨드
- `scripts/demo-omicron.sh` — DN kill → unhealthy 표시 라이브
