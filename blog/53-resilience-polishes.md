# Episode 53 — resilience polishes: replication concurrent + persistent scrubber + unrecoverable signal

> **Series**: kvfs — distributed object storage from scratch
> **Wave**: P8-14 (operational polish) · **Episode**: 7
> **연결**: [ADR-061](../docs/adr/ADR-061-resilience-polishes.md) · [demo-anti-entropy-resilience](../scripts/demo-anti-entropy-resilience.sh)

---

## P8-14: 한계 효용 3개를 한 번에

P8-13 의 끝에서 남긴 후속 목록:

- Replication path concurrent
- Persistent scrubber state
- Notify on unrecoverable
- (multi-tier Merkle, repair.Run multi-stripe — 더 큰 surgery)

앞 셋이 코드량 작고 패턴 비슷 → 한 wave 로 묶음. ADR-061 / Episode 53.

## Part A — Replication path concurrent

ADR-060 이 EC 만 worker-pool 화. 그때의 보류 사유:

> replication copy 는 RTT-bound (CPU 0). per-chunk 가 짧으니 dispatch
> overhead 가 비례적으로 큼.

맞는 말이지만, **큰 audit (수십 chunk 누락)** 에서는 wallclock 이 그대로
누적. measurement 가 하라고 한다.

같은 worker-pool 패턴, 같은 함수-스코프 `mu` 재사용:

```go
var repJobs []struct{ target, chunkID, reason string }
for _, e := range audit.DNs { ... append jobs ... }

if len(repJobs) > 0 {
    repSem := make(chan struct{}, repConc)
    var repWg sync.WaitGroup
    for _, j := range repJobs {
        repSem <- struct{}{}
        repWg.Add(1)
        go func(j struct{...}) {
            defer repWg.Done()
            defer func() { <-repSem }()
            repairOne(j.target, j.chunkID, j.reason)  // I/O OUTSIDE lock
        }(j)
    }
    repWg.Wait()
}
```

`repairOne` 은 ADR-060 와 동일: 잠금 밖에서 HTTP, 결과만 잠금 안에서
slice append.

### 측정

```
==> Part A: replication concurrent — measure 8-chunk audit
    seeded 8 objects
    concurrency=1 → repaired=8 elapsed=99ms
    concurrency=4 → repaired=8 elapsed=65ms
    speedup: 1.52× (concurrency=4 vs serial)
```

EC 의 2× 보다 작음 — RTT-bound 이라 각 작업이 짧고 dispatch overhead 가
상대적. 그래도 1.5× 의미 있음. multi-host production 에서는 더 클 듯.

## Part B — Persistent scrubber state

ADR-054 (S7 Ep.4) 의 scrubber 가 detect 한 corrupt set 이 in-memory only.
DN 재시작 시 잃음. 시나리오:

> 1. scrubber 가 chunk X 를 corrupt 로 flag.
> 2. coord 의 anti-entropy repair 가 다음 호출에서 X 를 정정.
> 3. 그러나 1) 직후, 2) 이전에 DN 이 OOM/restart 했다면 X 는 "잠시 잊혀짐"
>    — 새 scrubber pass 가 X 의 차례까지 도달해야 다시 발견.

운영자의 첫 repair 호출이 의미 있는 작업을 하기 전에 또 한 바퀴 기다려야
한다. 이를 제거하는 게 Part B.

### `scrub-state.json`

```go
const scrubStateFile = "scrub-state.json"
type scrubStatePersistedShape struct {
    Version int      `json:"version"`
    Corrupt []string `json:"corrupt"`
}
```

수명주기:
- `StartScrubber` 호출 시 `loadCorruptSet()` 로 init.
- `markCorrupt` / `scrubOne` healthy 분기에서 set mutation 시
  `persistCorruptSetLocked()` — atomic temp+rename.
- 쓰기 실패 silent — in-memory 가 truth, 디스크는 best-effort.

write amplification: scrubber 가 `interval` 당 한 chunk → 최악 10 writes/sec
(interval=100ms). corrupt set 자체도 보통 0..수개 → 파일 작음. 부담 0.

boot 순서: missing/parse-fail/version-mismatch 모두 빈 map 으로 graceful
fallback. DN start 가 실패할 일 없음.

### 검증

```
==> Part B: persistent scrubber state — corrupt → restart → still flagged
    overwrote f40c8770c822dad3.. on dn2; waiting 4s for scrubber...
    dn2 corrupt_count BEFORE restart: 1
    docker restart dn2
    dn2 corrupt_count AFTER restart:  1
    ✓ scrubber state survived restart
```

전에는 `AFTER restart: 0` 이었음 (다음 scan 까지 기다려야 함). 이제 즉시
재현. operator 의 첫 repair 가 즉시 의미 있는 작업 수행.

## Part C — Unrecoverable chunk slog.Error

coord 가 모든 replica 잃은 chunk 를 만나면 `out.Skipped` 에 `Mode=no_source`
로 추가하고 끝. 응답 JSON 에는 있지만 운영자가 polling 하지 않으면
silent.

해결: structured log signal:

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

`level=ERROR` + 일관된 `msg` 키 → log aggregator (ELK, Loki, Datadog)
의 alert rule 한 줄로 페이지 트리거 가능.

### 검증

```
==> Part C: unrecoverable notification — kill every replica of a chunk
    unmade 69952f239bcfb9e9.. from all replicas (truly unrecoverable)
    coord log captured: time=2026-04-27T13:08:57.669Z level=ERROR
        msg="anti-entropy: unrecoverable chunk"
        target_dn=dn1:8080 chunk_id=69952f239bcfb9e9...
    ✓ unrecoverable chunk surfaced via slog.Error
```

API/JSON 응답 변경 0. 로그-only 신호. silent failure 가 측정 가능한
ERROR 가 됨.

## "왜 metric 이 아닌 slog?"

운영자가 묻을 수 있는 질문. zone 의 표준이 slog 라 metric 인프라 (Prometheus
등) 도입 전에는 slog 가 자연 채널. 이후 ADR-062 가 coord `/metrics` 와
`unrecoverable_total` counter 를 추가했고, ADR-063 이 같은 chunk 반복 알림을
first-seen dedupe 로 줄였다.

slog ERROR 자체로도 alert 가능: 대부분의 aggregator 가 `level` 필터를
1급 시민으로 다룸.

## 정량

| | |
|---|---|
| 코드 | anti_entropy.go +50 LOC (replication worker pool + unrecoverable loop), scrubber.go +50 LOC (persist + load) |
| 새 파일 | `<DN_DATA_DIR>/scrub-state.json` (Version 1) |
| 새 endpoints / cli | 0 — 기존 `?concurrency` 가 replication 도 적용 |
| 데모 | 3 stage 한 스크립트 (concurrent + persist + unrecoverable) |
| 테스트 | 185 PASS (183 → 185, dn merkle_test +2: PersistsAcrossRestart, PersistedFileShape) |

## ADR-005 dividend (또)

scrubber 의 corrupt 검출 = sha256(file) ≠ chunk_id (= sha256(content)
기록). chunk_id 가 content-addressable 이라 disk drift 가 즉시 noisy
(invariant 위반). persist 한 corrupt set 도 chunk_id 를 그대로 저장 →
다른 DN 의 같은 chunk 와 join 가능 (anti-entropy 의 동작 원리).

ADR-005 의 2 줄짜리 결정이 P8 wave 의 거의 모든 episode 에 등장. 분산
storage 에서 가장 근본적인 invariant 였음을 retrospect 가 다시 확정.

## P8 wave 가 사실상 끝

ADR-055~061 = 7 ADR. anti-entropy 의 detect → action → policy → polish
loop 가 모두 닫힘:

| 영역 | 도입 |
|---|---|
| Detect | Merkle root, scrubber, Skipped 분류 |
| Action | replication repair, EC inline repair, corrupt force-overwrite |
| Policy | dry-run, throttle (max_repairs), concurrency, mode 분리 |
| Polish | concurrent (EC + replication), persist scrubber, unrecoverable signal |

운영자가 한 명령으로 cluster 전체 self-heal:

```
$ kvfs-cli anti-entropy repair --coord URL \
    --corrupt --ec --max-repairs 1000 --concurrency 8
```

후속 현황 (2026-04-28):

- Multi-tier hierarchical Merkle (256 bucket 평탄 → 깊이 ≥2)
- Scrubber rate adaptive (load 감지)
- Coord-side metric 은 ADR-062/063 으로 완료
- `repair.Run` 의 multi-stripe 자체 parallelism

이쯤에서 P8 wave 가 자연 종결 시점에 가깝다. 남은 것은 P8-17 저우선
polish 후보.

## 다음

P8-17 저우선 polish 또는 새 시즌. 자연 정리 시점.
