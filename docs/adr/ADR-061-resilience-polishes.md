# ADR-061 — Anti-entropy resilience polishes (replication concurrent + persistent scrubber + unrecoverable notify)

상태: Accepted (2026-04-27)
시즌: P8-14 (operational polish)

## 배경

ADR-060 의 후속 (P8-14 후보 목록) 에서 세 항목이 한 wave 로 묶일 만한 결합도:

1. **Replication path concurrent** — ADR-060 이 EC 만 concurrent 화. replication
   pass 는 RTT-bound 이라 미루었지만, 큰 audit (수십 chunk 누락) 에서는
   wallclock 이 그대로 누적. EC 와 같은 worker-pool 패턴 재사용 가능.
2. **Persistent scrubber state** — ADR-054 의 scrubber 가 detect 한 corrupt 집합
   이 in-memory only. DN 재시작 시 잃어버려, 다음 anti-entropy 호출이
   "다시 한 바퀴 돌 때까지" 기다려야 같은 corrupt 를 재발견. 운영 관점
   불편 (예: 스크럽이 발견한 직후 DN 이 OOM/restart 했다면 corrupt 가 잠시
   "잊혀짐").
3. **Unrecoverable chunk notification** — coord 가 모든 replica 잃은 chunk 를
   `Skipped[Mode=no_source]` 로 silent 처리. 외부 log aggregator 에 알릴
   structured ERROR signal 부재.

세 항목 모두 **operational polish** 로 코드량 작고 테스트 패턴 유사 →
하나의 ADR / Episode 로 bundle.

## 결정

### Part A — Replication path concurrent dispatch

`internal/coord/anti_entropy.go` `runAntiEntropyRepair` 의 replication for-loop
를 EC pass 와 같은 worker-pool 로 교체. ADR-060 의 함수-스코프 `mu` 를
재사용 — replication 도 같은 `out.Repairs / out.Skipped` 에 누적.

```go
var repJobs []struct{ target, chunkID, reason string }
for _, e := range audit.DNs { ... append jobs ... }

repConc := opts.Concurrency
if repConc < 1 { repConc = 1 }
if repConc > len(repJobs) && len(repJobs) > 0 { repConc = len(repJobs) }
if len(repJobs) > 0 {
    repSem := make(chan struct{}, repConc)
    var repWg sync.WaitGroup
    for _, j := range repJobs {
        repSem <- struct{}{}
        repWg.Add(1)
        go func(j struct{...}) {
            defer repWg.Done()
            defer func() { <-repSem }()
            repairOne(j.target, j.chunkID, j.reason)
        }(j)
    }
    repWg.Wait()
}
```

`repairOne` 의 HTTP I/O 는 lock 밖, slice append + throttle 검사만 lock 안.
EC pass 와 동일 invariant.

Throttle 의 best-effort over-cap (worker 가 in-flight 중 임계 도달) 는 ADR-060
에서 이미 인정한 trade-off — replication 에도 그대로 적용.

### Part B — Persistent scrubber state (`scrub-state.json`)

`internal/dn/scrubber.go` 에 corrupt set 의 디스크 영속 추가:

```go
const scrubStateFile = "scrub-state.json"
type scrubStatePersistedShape struct {
    Version int      `json:"version"`
    Corrupt []string `json:"corrupt"`
}
```

수명주기:
- `StartScrubber` 호출 시 `loadCorruptSet()` 로 init (no-file → 빈 map).
- `markCorrupt` / `scrubOne` healthy 분기에서 set 의 add/delete 마다
  `persistCorruptSetLocked()` 호출 — atomic temp+rename.
- 쓰기 실패는 silent — in-memory 가 truth, 디스크는 best-effort. 다음
  mutation 이 재시도.

Write amplification 우려: scrubber 가 `interval` 당 한 chunk 처리 → 최악
10 writes/sec (interval=100ms) → 일반 SSD 부담 0. 운영에서 corrupt set 은
보통 0..수개 라 파일 크기도 작음.

Boot 순서: load 가 graceful (missing/parse-fail/version-mismatch → 빈 map)
→ DN start 절대 실패 안 함. 잘못된 파일을 만나면 다음 scrub pass 가 다시
build.

### Part C — Unrecoverable chunk slog.Error

`runAntiEntropyRepair` 의 마지막 `return out, nil` 직전에 `out.Skipped` 순회:

```go
for _, sk := range out.Skipped {
    switch sk.Mode {
    case repairModeNoSource:
        s.Log.Error("anti-entropy: unrecoverable chunk",
            "target_dn", sk.TargetDN, "chunk_id", sk.ChunkID,
            "reason", sk.Reason, "err", sk.Err)
    case repairModeSkip:
        s.Log.Warn("anti-entropy: target DN unreachable", ...)
    }
}
```

slog 의 structured key/value 그대로 노출 → log aggregator (ELK, Loki) 의
`level=ERROR` + `msg="anti-entropy: unrecoverable chunk"` 필터로 즉시
alert 가능.

API/JSON 응답 shape 변경 없음 — 로그-only 신호. 자동 페이지 트리거는
operator 가 alert rule 로 바인딩.

## 측정

`scripts/demo-anti-entropy-resilience.sh` 3 stage:

- **Part A**: 8-object PUT, 8-chunk rm from dn1, repair concurrency=1 vs
  concurrency=4. 결과: 99ms → 65ms, **1.52× speedup**. RTT-bound 이라
  EC 의 2× 보다 작음 — single-host docker, DN I/O 동일 디스크. 그래도
  유의미.
- **Part B**: bit-rot dn2 → scrubber 가 4초 안에 flag → `docker restart dn2`
  → 재시작 후 corrupt_count 그대로 1. **survives restart**.
- **Part C**: 한 chunk 의 모든 replica rm → repair 호출 → coord 로그에
  `level=ERROR msg="anti-entropy: unrecoverable chunk" target_dn=dn1:8080
  chunk_id=69952f23...` 출현 검증.

## 비채택 대안

| 대안 | 기각 사유 |
|---|---|
| Replication concurrent 를 별도 ADR | 코드 패턴이 EC 와 동일 → 분리할 가치 < documentation 비용 |
| Replication 도 up-front partition 으로 throttle race-free | replication throttle 은 successCount 를 매 작업 후 lock 안에서 읽음. up-front 는 EC 의 stripe-level granularity 에서 의미 — replication 의 per-chunk 는 best-effort 로 충분 |
| Scrubber state 를 bbolt 에 통합 | DN 은 stateless-by-design (bbolt 없음). JSON 파일이 가장 단순 + grep 가능 |
| Scrubber 마다 SQLite | 단일 파일 + atomic rename 으로 충분. SQLite 는 dependency |
| Unrecoverable 을 metric counter 로 (slog 대신) | 당시 metric 인프라 없음 — slog 가 zone 의 표준. 이후 ADR-062/063 이 coord metric + dedupe 를 추가 |
| Unrecoverable 시 페이지 직접 호출 | coord 가 외부 alert 시스템 알면 안 됨. log → aggregator → alert 가 깔끔 |
| Persist 를 mutation 마다 (현재) 가 아니라 throttled flush | 작성 빈도 낮음 (sec 당 ≤ 10) — 복잡도 추가 가치 0 |

## 결과

+ **Replication 1.52× speedup** (single-host demo). production multi-host
  에서 더 큼.
+ **Scrubber persistence**: DN restart 시 corrupt set 보존. 운영자의 첫
  repair 가 즉시 의미 있는 작업 수행.
+ **Unrecoverable structured ERROR**: log aggregator alert 가능. silent
  failure 제거.
+ **185 unit tests PASS** (+2 신규: persistence-restart, persisted-shape).
+ 코드 작음: ~80 LoC anti_entropy.go + ~50 LoC scrubber.go.
- **Replication throttle 의 best-effort over-cap** 그대로 — concurrent context
  에서 budget 의 strict 정확도는 trade-off 인정.
- **Scrubber persist 가 mutation 동기적** — 디스크 hang 시 scrubLoop 도 hang.
  허용 가능: 같은 디스크가 hang 이면 chunk 자체가 위험.
- **Unrecoverable 알림이 로그-only** — alert 자동화는 operator 책임.

## 호환성

- `Concurrency` 의 replication path 적용은 동일 query param. 미설정 시
  serial. 기존 behavior 0 변화.
- `scrub-state.json` 새 파일. 기존 DN 시작 시 missing → 빈 set load. drift 0.
- slog ERROR 추가 — log volume 증가는 unrecoverable 이 발생할 때만. 정상
  운영에서는 0.

## 검증

- `internal/dn/merkle_test.go` 신규 2 테스트: `TestScrubber_CorruptStatePersistsAcrossRestart`,
  `TestScrubber_PersistedFileShape`. fresh Server 로 reload + JSON Version=1
  검증.
- `scripts/demo-anti-entropy-resilience.sh` 3 stage 모두 PASS (2026-04-27 실행).
- 전체 test suite **185 PASS**.

## 후속 현황 (2026-04-28)

- Replication 의 throttle 도 EC 처럼 up-front partition (필요 시).
- Multi-tier hierarchical Merkle (256 bucket 평탄 트리 → 깊이 ≥2).
- Scrubber rate adaptive (load 감지 시 slowdown).
- Coord-side unrecoverable counter metric 은 ADR-062 로 완료.
- Unrecoverable first-seen dedupe 는 ADR-063 으로 완료.
