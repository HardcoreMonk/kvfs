# kvfs from scratch, Ep.7 — 운영자 호출 0번에 스스로 정렬되는 클러스터

> Season 3 · Episode 1. ADR-010 (rebalance) 와 ADR-012 (GC) 의 알고리즘 한 줄도 안 건드리고 trigger 만 추가. `time.Ticker` 두 개 + 기존 mutex 공유. ~200 LOC, 0 외부 의존, opt-in default.

## Season 2 의 미해결 — 손이 너무 많이 간다

지난 5 episode 동안 우리는 **분산 동기화의 5 기둥** 을 만들었습니다:

| Ep | 알고리즘 | 운영자가 누른 명령 |
|---|---|---|
| 2 | Rendezvous Hashing (placement) | (자동 — 매 PUT 시점) |
| 3 | Rebalance worker | `kvfs-cli rebalance --apply` |
| 4 | Surplus chunk GC | `kvfs-cli gc --apply` |
| 5 | Chunking | (자동 — 매 PUT 시점) |
| 6 | Reed-Solomon EC | (자동 — `X-KVFS-EC` 헤더 시) |

3, 4 가 **수동**. demo-eta / demo-theta 가 그대로 보여줬죠:

```
사용자가 dn4 추가 → 사용자가 rebalance 명령 → 사용자가 GC 명령
```

작은 클러스터에선 OK. 그러나 1년 운영하면:
- DN 추가/제거가 잦은 환경에선 매번 부담
- **잊기 쉬움** — 한 달 후 surplus 가 디스크 50% 차지하고 있음
- partial rebalance 가 자동 재시도 안 됨

ADR-013 의 답: **edge 안에 ticker 두 개**. 알고리즘 0 변경, trigger 만 추가.

## 가장 단순한 답이 정답

대안들은 다 더 복잡합니다:

| 방식 | 단점 |
|---|---|
| External cron (systemd / k8s CronJob) | 설정·관찰성 분산, 배포 복잡 |
| 이벤트 기반 (DN heartbeat 변경 감지) | DN ↔ edge push 인프라 필요 (별도 ADR) |
| Pull-based DN 광고 | 양방향 프로토콜 |
| **In-edge ticker** | (이번 ADR) — 추가 daemon 0, 기존 mutex 재활용 |

kvfs 의 single-edge MVP 에서는 **in-edge ticker** 가 압도적. 미래에 multi-edge 가 되면 ADR-022 (leader election) 가 받습니다 — 그건 그때.

## 핵심 코드 — `time.Ticker` 두 개

```go
// internal/edge/edge.go
func (s *Server) StartAuto(ctx context.Context) {
    if !s.AutoCfg.Enabled { return }    // opt-in
    ...
    go s.autoRebalanceLoop(ctx, rb, conc)
    go s.autoGCLoop(ctx, gci, minAge, conc)
}

func (s *Server) autoRebalanceLoop(ctx context.Context, interval time.Duration, conc int) {
    t := time.NewTicker(interval)
    defer t.Stop()
    for {
        select {
        case <-ctx.Done(): return       // signal로 깨끗히 종료
        case <-t.C:
            s.runAutoRebalance(ctx, conc)
        }
    }
}
```

`runAutoRebalance` 의 본체:

```go
func (s *Server) runAutoRebalance(ctx context.Context, conc int) {
    s.rebalanceMu.Lock()                // 수동 /apply 와 같은 mutex
    defer s.rebalanceMu.Unlock()

    plan, _ := rebalance.ComputePlan(s.Coord, s.Store)   // ADR-010 그대로
    if len(plan.Migrations) == 0 { return }              // fast-path
    stats := rebalance.Run(ctx, s.Coord, s.Store, plan, conc)  // ADR-010 그대로

    s.recordAuto(AutoRun{...})          // ring buffer 기록
}
```

**알고리즘 호출 한 줄.** Mutex 하나만 다른 작업이 잡고 있는지 확인. GC loop 도 동일 패턴.

이게 "trigger 만 추가" 의 진짜 의미입니다 — `rebalance.ComputePlan` 과 `rebalance.Run` 의 코드는 한 글자도 안 변했습니다.

## 안전 보장 — 같은 mutex 공유

```go
// 수동 /apply
func (s *Server) handleRebalanceApply(...) {
    ...
    s.rebalanceMu.Lock()
    defer s.rebalanceMu.Unlock()
    plan, _ := rebalance.ComputePlan(...)
    stats := rebalance.Run(...)
    ...
}

// 자동 cycle
func (s *Server) runAutoRebalance(...) {
    s.rebalanceMu.Lock()                // 같은 mutex
    defer s.rebalanceMu.Unlock()
    ...
}
```

**race 0**: 수동과 자동이 동시에 일어나면 두 번째가 첫 번째를 기다림. ADR-010 의 single-flight 약속이 자동 호출에도 그대로 유효.

GC 도 동일: `s.gcMu` 공유. `min-age` 안전망 (60s 기본) 이 freshly-PUT 청크 보호. **수동 `--apply --min-age 0` 같은 강제 옵션 외에는 자동이 데이터 손실을 일으킬 수 없습니다.**

## 설정 — opt-in default OFF

```
EDGE_AUTO                       1/0       default 0     ← 명시적으로 켜야 함
EDGE_AUTO_REBALANCE_INTERVAL    duration  default 5m
EDGE_AUTO_GC_INTERVAL           duration  default 15m
EDGE_AUTO_GC_MIN_AGE            duration  default 60s
EDGE_AUTO_CONCURRENCY           int       default 4
```

**왜 opt-in?** 자동 삭제 (GC) 가 포함됩니다. 운영자가 ADR-012 의 두 안전망 (claimed-set + min-age) 을 이해한 후 활성화하는 게 보수적. default off 가 정답.

## 관찰성 — `/v1/admin/auto/status`

```bash
$ kvfs-cli auto --status -v

⏱  Auto-trigger: ENABLED
   rebalance_interval: 10s
   gc_interval:        12s
   gc_min_age:         3s
   concurrency:        4
   last_rebalance:     2026-04-26T16:13:34Z
   last_gc:            2026-04-26T16:13:28Z
   next_rebalance:     2026-04-26T16:13:44Z
   next_gc:            2026-04-26T16:13:40Z

Recent 5 runs (most recent last):
  16:13:14  rebalance  plan=2  stats=map[bytes_copied:76 failed:0 migrated:2 scanned:4]  (20ms)
  16:13:16  gc         plan=2  stats=map[bytes_freed:76 claimed_keys:12 deleted:2 failed:0 min_age_sec:3 scanned:14]  (5ms)
  16:13:24  rebalance  no work  (0ms)
  16:13:28  gc         no work  (4ms)
  16:13:34  rebalance  no work  (0ms)
```

각 cycle 의 결과가 ring buffer (32 entries) 에 축적. 운영자가 한눈에 "최근 5 cycle 의 동작" 확인. slog 로 같은 정보가 stdout 에 구조화 로그로도 흐름.

## 라이브 데모 — `demo-lambda.sh`

```
=== λ demo: auto-trigger (ADR-013) — opt-in time-based loops ===

[1/7] Reset cluster + up edge with EDGE_AUTO=1
      (intervals: rebalance=10s, gc=12s, min-age=3s)
  ✅ 3 DNs + edge up (auto enabled)

[2/7] Seed 4 objects on the 3-DN cluster
  put seed-1.txt
  put seed-2.txt
  put seed-3.txt
  put seed-4.txt

[3/7] Add dn4 + restart edge with 4-DN config (auto still on)
  Disk before any auto cycle:
  dn1  chunk count = 4
  dn2  chunk count = 4
  dn3  chunk count = 4
  dn4  chunk count = 0     ← misplaced state
```

15초 sleep 후:

```
[4/7] Sleep 15s — auto-rebalance should have fired by then
Recent 2 runs:
  16:13:14  rebalance  plan=2  stats=map[bytes_copied:76 failed:0 migrated:2 scanned:4]  (20ms)
  16:13:16  gc         plan=2  stats=map[bytes_freed:76 claimed_keys:12 deleted:2 failed:0 min_age_sec:3 scanned:14]  (5ms)

  Disk after auto-rebalance:
  dn1  chunk count = 4
  dn2  chunk count = 3
  dn3  chunk count = 3
  dn4  chunk count = 2     ← dn4 received its share
```

**운영자가 한 번도 명령을 안 쳤습니다.** 그런데:
- 10초 후 첫 rebalance cycle: 2 청크 migrated (76 바이트)
- 12초 후 첫 GC cycle: 2 청크 deleted (같은 76 바이트, surplus 였음)
- 그 이후 cycle 들: "no work" (멱등)

```
[6/7] Verify cluster is in HRW-desired state without any manual call
📋 Rebalance plan — scanned 4 objects, 0 need migration
   ✅ Cluster is in HRW-desired state. No work to do.

🧹 GC plan — scanned 12 on-disk chunks across all DNs
   meta-claimed pairs (protected): 12
   sweeps proposed: 0
   ✅ No surplus to clean. Cluster disk = meta intent.

[7/7] Final disk = meta intent (4 objects × 3 replicas = 12 chunks)
  dn1  chunk count = 4
  dn2  chunk count = 3
  dn3  chunk count = 3
  dn4  chunk count = 2
  total: 12  (expected 12)
```

수동 `--plan` 도 동시 호출 가능 (같은 mutex 공유). 결과는 둘 다 0. **메타 = 디스크 정렬 완료**.

비교:
- **demo-eta + demo-theta** (manual): 운영자가 4개 명령을 명시적으로 호출
- **demo-lambda** (auto): 운영자 0 명령. 같은 결과

알고리즘이 같으니 결과도 같습니다. 차이는 **누가 trigger 를 누르는가**.

## 명시적 비범위 — 다음 ADR 들이 풀어야 할 것

1. **multi-edge leader election** — edge 가 N 개일 때 한 인스턴스만 auto loop 돌림. 현재 single-edge MVP 라 무관. ADR-022 후보
2. **이벤트 기반 즉시 반응** — DN 가입/탈퇴를 감지해 5분 안 기다리고 즉시. DN heartbeat 인프라 + push 모델 필요. ADR-014 (Meta backup/HA) 와 묶임
3. **EC stripe rebalance + auto re-encode** — ADR-008 의 EC 객체는 현재 rebalance 미적용 (stripe-level placement 변경 처리 필요). ADR-024 후보
4. **rate limiting / max-per-cycle** — 100만 객체 환경에선 한 cycle 의 max migrations cap 필요. ADR-023 후보
5. **alert / notification** — Failed > 0 시 운영자 알림. 현재 slog warn 만

이런 한계가 있다는 걸 알면서 ADR-013 한 페이지로 끝낸 이유는 같은 원칙:

> **하나의 결정 = 하나의 보장**

ADR-013 은 "수동 호출이 없어도 클러스터가 스스로 정렬" 만 약속. multi-edge, 이벤트 기반, EC 자동 재배치는 별개 약속.

## Season 3 가 시작됐다 — 무엇을 풀까

Season 2 가 **알고리즘** 을 닦았다면 (placement, rebalance, GC, chunking, EC), Season 3 는 **운영성** 을 닦습니다:

| Ep | ADR | 주제 |
|---|---|---|
| 7 (this) | 013 | Auto-trigger (시간 기반) |
| 8 후보 | 022 | multi-edge leader election |
| 9 후보 | 014 | Meta backup / HA (bbolt 손상 대비) |
| 10 후보 | 024 | EC stripe rebalance + 자동 재인코딩 |
| 11 후보 | 015 | Coordinator daemon 분리 |

운영성의 끝에는 **HA edge** 가 있습니다 — 단일 edge 가 죽어도 cluster 가 살아있도록.

## 다음 편 예고

- **Ep.8** *(완료)*: ADR-024 EC stripe rebalance — 6 DN EC 클러스터에 dn7 추가 시 EC 객체 자동 재배치, set-based 최소 이동. → [`blog/08-ec-rebalance.md`](08-ec-rebalance.md)
- **Ep.9 후보**: ADR-022 Multi-edge leader election — auto loop 의 single-instance 가정을 깨고 N edge 운용
- **Ep.10 후보**: ADR-025 EC repair queue — DN 영구 사망 시 reconstruct + 자동 복구
- **Ep.11 후보**: ADR-014 Meta backup/HA — bbolt 메타 손실 대비

## 직접 돌려보기

```bash
git clone https://github.com/HardcoreMonk/kvfs   # 공개 시점 기준
cd kvfs
docker build -t kvfs-edge:dev --target kvfs-edge .
docker build -t kvfs-dn:dev   --target kvfs-dn   .
docker run --rm -v "$PWD:/src" -w /src -e CGO_ENABLED=0 \
  golang:1.26-alpine sh -c 'go build -o ./bin/kvfs-cli ./cmd/kvfs-cli'

./scripts/demo-lambda.sh
```

매번 chunk_id 는 다릅니다. 그러나 항상:

- 자동 rebalance 가 ~10초 내 misplaced 청크 dn4 로 복사
- 자동 GC 가 ~12초 내 surplus 청크 정리
- 두 번째 cycle 부터는 "no work" (멱등)
- 최종 disk = 4 객체 × 3 replicas = 12

위 4 불변식이 ADR-013 의 약속과 일치.

## 참고 자료

- 이 ADR: [`docs/adr/ADR-013-auto-trigger.md`](../docs/adr/ADR-013-auto-trigger.md)
- 구현: [`internal/edge/edge.go`](../internal/edge/edge.go) `StartAuto` / `runAutoRebalance` / `runAutoGC`
- env 파싱: [`cmd/kvfs-edge/main.go`](../cmd/kvfs-edge/main.go) `EDGE_AUTO_*`
- CLI: [`cmd/kvfs-cli/main.go`](../cmd/kvfs-cli/main.go) `cmdAuto`
- 데모: [`scripts/demo-lambda.sh`](../scripts/demo-lambda.sh)
- 의존 알고리즘: ADR-010 (rebalance) + ADR-012 (GC)
- 이전 episode (EC): [`blog/06-erasure-coding.md`](06-erasure-coding.md)

---

*kvfs는 교육적 레퍼런스입니다. 프로덕션에 배포하지 마세요. Season 3 가 시작됐습니다 — 운영성 트랙. 다음 단계는 multi-edge HA, EC 자동 재배치, 메타 백업 — 모두 "혼자서도 동작하는 클러스터" 의 조각들.*
