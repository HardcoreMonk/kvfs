# Episode 11 — DN heartbeat: edge가 클러스터를 알아채는 방법

> **Series**: kvfs — distributed object storage from scratch
> **Season**: 3 (operability) · **Episode**: 5
> **ADR**: [030](../docs/adr/ADR-030-dn-heartbeat.md) · **Demo**: `./scripts/demo-omicron.sh`

---

## 가시성 갭

지금까지 kvfs 운영자가 DN 죽음을 알 수 있는 경로:

1. `kvfs-cli rebalance --plan` 실행 → migration 수가 비정상적으로 많다
2. PUT 요청들이 종종 quorum 으로 떨어져 "느리다" 는 보고
3. `journalctl -u kvfs-edge` 에 `dial tcp: ... refused` 가 잔뜩
4. 외부 모니터링 툴 (Prometheus exporter 같은 게 있다면)

**모두 사후 indicator**. 운영자가 우연히 보고 발견한다. 그 동안 quorum 은 깨지고
사용자 요청은 timeout 으로 늘어진다.

ADR-027 동적 DN registry 는 운영자가 admin endpoint 호출로 add/remove 가능하게
했지만 — DN 이 갑자기 죽는 경우(kernel panic, OOM, 네트워크 단절)에는 운영자가
**먼저 알아채야** registry 에서 빼낼 수 있다.

ADR-030 의 목표는 이 비대칭을 메우는 것 — edge 가 알아서 보고하게 만든다.

## Pull vs Push

DN liveness 를 어떻게 알 수 있을까. 두 가지 방향:

| | **Pull** (edge → DN) | **Push** (DN → edge) |
|---|---|---|
| 인증 | 0 (DN 의 `/healthz` 는 이미 unauthenticated) | DN 이 edge 에 신뢰성 있게 알려야 — auth 추가 |
| 시간 동기화 | 불필요 (edge 본인 시각) | last_seen 비교 위해 양쪽 시간 sync |
| DN 코드 변경 | 0 | 모든 DN 에 heartbeat client 배포 |
| 부하 | edge 한 곳, fan-out tunable | DN N 개, N×outbound |
| ADR-027 와 정합 | edge 가 권위적 registry 보유 — 동일 모델 | DN 이 자기 active 알리는 모델 보강 필요 |

본 ep 는 **pull**. ADR-013 auto-trigger 의 in-edge ticker 패턴과도 일관 — 새
의존성 0, DN 코드 unchanged.

## 코드 — Monitor + Probe

```go
// internal/heartbeat/heartbeat.go
type Probe interface {
    Probe(ctx context.Context, addr string) (latency time.Duration, err error)
}

type Status struct {
    Addr        string
    Healthy     bool
    LastSeen    time.Time
    LatencyMS   int64
    ConsecFails int
}

type Monitor struct {
    mu            sync.RWMutex
    statuses      map[string]*Status
    probe         Probe
    failThreshold int
}

func (m *Monitor) Tick(ctx context.Context, addrs []string) {
    var wg sync.WaitGroup
    for _, addr := range addrs {
        wg.Add(1)
        go func(addr string) {
            defer wg.Done()
            pctx, cancel := context.WithTimeout(ctx, m.probeTimeout)
            defer cancel()
            lat, err := m.probe.Probe(pctx, addr)
            m.update(addr, lat, err)
        }(addr)
    }
    wg.Wait()
}
```

핵심 디자인 선택:

- **Probe 인터페이스로 격리** — 테스트는 fakeProbe 로 동기화 없이 검증, 프로덕션은
  HTTP/HTTPS probe 로 wire up
- **fan-out 병렬** — N 개 DN 에 동시 probe. 1s interval × 100 DN 도 부담 X
- **registry shrink 처리** — `Tick(addrs)` 가 들어오면 더 이상 addrs 에 없는
  status 는 자동 drop. ADR-027 dns admin remove 와 자연스럽게 연동

## Healthy 판정 — latch 없음, threshold 있음

```go
func (m *Monitor) update(addr string, lat time.Duration, err error) {
    st := m.statuses[addr] // or create
    st.TotalProbes++
    st.LatencyMS = lat.Milliseconds()
    if err == nil {
        st.LastSeen = time.Now().UTC()
        st.ConsecFails = 0
        st.Healthy = true
        return
    }
    st.ConsecFails++
    st.LastError = err.Error()
    if st.ConsecFails >= m.failThreshold {
        st.Healthy = false
    }
}
```

- **threshold (default 3)** — 1-2 회 jitter (네트워크 spike) 는 무시. 의미 있는
  패턴만 unhealthy
- **latch 없음** — 다음 성공 probe 가 즉시 healthy 로 회복. 운영자 수동 reset 불필요
- **wall clock based last_seen** — single edge 가정. multi-edge 는 ADR-022 과제

## HTTP probe — TLS 도 자연스럽게

`cmd/kvfs-edge/main.go` 의 `httpHealthProbe` factory:

```go
func httpHealthProbe(scheme string, tlsCfg *tls.Config) heartbeat.Probe {
    transport := &http.Transport{TLSClientConfig: tlsCfg}
    client := &http.Client{Transport: transport}
    return probeFn(func(ctx context.Context, addr string) (time.Duration, error) {
        start := time.Now()
        req, _ := http.NewRequestWithContext(ctx, "GET", scheme+"://"+addr+"/healthz", nil)
        resp, err := client.Do(req)
        // ...
    })
}
```

`scheme` + `tlsCfg` 는 ADR-029 가 빌드한 transport 와 동일 — TLS/mTLS 가 켜져
있으면 heartbeat probe 도 같은 인증서로 동작. 별도 cert 관리 X.

## Endpoint + CLI

`GET /v1/admin/heartbeat`:
```json
{
  "enabled": true,
  "statuses": [
    {"addr": "dn1:8080", "healthy": true, "last_seen": "...", "latency_ms": 0, "consec_fails": 0, "total_probes": 12},
    {"addr": "dn3:8080", "healthy": false, "consec_fails": 4, "last_error": "dial tcp: refused"}
  ]
}
```

`kvfs-cli heartbeat`:
```
ADDR                   HEALTHY  LAST_SEEN    LATENCY   CONSEC_FAILS LAST_ERROR
dn1:8080               ✅ yes     1s ago       0 ms      0            
dn2:8080               ✅ yes     1s ago       0 ms      0            
dn3:8080               ❌ NO      4s ago       3 ms      4            Get "http://dn3:8080/healthz": dial tcp: ... server misbehaving
```

## 라이브 데모 출력 (`./scripts/demo-omicron.sh`)

3 DN 클러스터 + heartbeat 1s interval + threshold 3:

```
--- step 2: heartbeat snapshot — all healthy ---
dn1:8080               ✅ yes     0s ago       0 ms      0            
dn2:8080               ✅ yes     0s ago       0 ms      0            
dn3:8080               ✅ yes     0s ago       0 ms      0            

--- step 3: kill dn3 ---
  dn3 killed

--- step 4: wait 4s for 3 consecutive fails ---

--- step 5: heartbeat snapshot — dn3 unhealthy ---
dn1:8080               ✅ yes     0s ago       0 ms      0            
dn2:8080               ✅ yes     0s ago       0 ms      0            
dn3:8080               ❌ NO      4s ago       3 ms      4            Get "...dn3:8080/healthz": ... server misbehaving

--- step 6: restart dn3 ---

--- step 7: heartbeat snapshot — dn3 recovered ---
dn1:8080               ✅ yes     0s ago       0 ms      0            
dn2:8080               ✅ yes     0s ago       0 ms      0            
dn3:8080               ✅ yes     0s ago       0 ms      0            

✅ ο demo PASS
```

10s default interval × 3 threshold = 30s detection latency. 1s × 3 = 3s 데모용.

## 비범위 — 본 ADR 가 명시적으로 하지 않은 것

- **Auto-eviction** — `Healthy=false` 됐다고 자동으로 placement 에서 빼지 않음.
  운영자 visibility 만. 자동 evict 는 후속 ep (ADR-022 multi-edge HA 와 함께
  다룰 가능성)
- **Placement-aware filtering** — `HealthyAddrs()` 헬퍼는 있지만 coordinator
  가 아직 사용 안 함. 활용은 후속
- **Multi-edge consensus** — single edge 의 wall clock 기준. ADR-022 과제

## 다음 ep ([P3-11](../docs/FOLLOWUP.md))

- **ADR-016 — WAL / incremental backup** — ADR-014 위에 분 단위 RPO
- **ADR-022 — Multi-edge leader election** (HA 본격 작업)
- **ADR-017 — Streaming PUT/GET**
- **ADR-018 — Content-defined chunking**

---

*kvfs 는 교육적 레퍼런스. 본 ep 의 heartbeat 패턴은 production 등급 health
checker 가 아니다 (지터 smoothing 없음, percentile latency 추적 없음). 학습용
시작점.*
