# kvfs 후속 작업 (Follow-up)

`200.kvfs/` 의 **후속 작업 단일 소스**. 상태 업데이트는 이 파일만 수정한다.

문서 현행화 일자: **2026-04-28** · P8-16 observability completions (ADR-063, blog Ep.55, demo-anti-entropy-observability) + README/GUIDE/ADR/blog env·count drift 정리까지 반영.

## 우선순위 맵

- **P0**: 차단·긴급 — 현재 **0건**
- **P1**: 명확한 스펙, 실행 대기 — 현재 **1건** (P1-02 사용자 직접)
- **P2**: 리뷰·개선 — 모두 완료
- **P3**: 사용자 결정 필요 — 현재 **0건**
- **P5**: post-Season-4 wave — 모두 완료
- **P6**: Season 5 (coord 분리) + helper extraction + meta cache — 모두 완료
- **P7**: Season 6 (coord operational migration) — Ep.1~7 모두 완료
- **P8**: Frame-1+2 100% wave — P8-01·02·03·04·06·08~16 DONE (Frame 1+2 = 100% + self-heal coverage 100% + operational polish + concurrent EC repair + replication concurrent + persistent scrubber + unrecoverable signal + continuous self-heal + Prometheus surface + observability completions), P8-05·07·17 (한계효용 polish, 저우선) 잔존

> ※ P4-* 모두 완료. P3-02 close, P5-03 ADR-015 Accept (S5 진입). 신규 항목은 P6-* 부터.

---

## P1 — 실행 대기 (사용자 직접)

### [P1-02] Docker Compose plugin 설치
- **배경**: `./scripts/up.sh` 가 plain `docker run` 으로 우회 구현됨. `docker-compose.yml` 은 있으나 `docker compose` 명령 사용 불가
- **해결**: `sudo apt install docker-compose-plugin`
- **효과**: README 의 `docker compose up -d` 표준 명령 동작

---

## P3 — 사용자 결정

### ~~[P3-02] OTel 통합 여부~~
- **CLOSED 2026-04-26**: 옵션 A (slog + Prometheus `/metrics`) 채택 확정. OTel 풀 통합은 trace 수집 needs 가 명확해질 때까지 무기한 deferred. 운영 관찰성은 Prometheus + ADR-036 의 WAL gauges + ADR-037 의 pool gauge + 기존 slog 구조화 로그로 충분.

---

## P5 — Post-Season-4 wave 후속

### ~~[P5-01] ADR-036 — WAL batch metrics~~
- **DONE 2026-04-26**: ADR-036 작성 + 구현. `kvfs_wal_batch_size` + `kvfs_wal_durable_lag_seconds` gauges. WAL.LastBatchSize / OldestUnsyncedAge accessors. `internal/edge/metrics.go` 에 wired.

### ~~[P5-02] ADR-037 — chunker pool cap 정책~~
- **DONE 2026-04-26**: ADR-037 작성 + 구현. `chunker.SetPoolCap` + `EDGE_CHUNKER_POOL_CAP_BYTES` env. `kvfs_chunker_pool_bytes` gauge.

### ~~[P5-03] ADR-015 — Coordinator daemon 분리~~
- **ACCEPTED 2026-04-26**: ADR-015 Proposed → Accepted. ADR-002 supersede. **Season 5 진입**. 후속 작업은 P6-* 시리즈로 이전.

### ~~[P5-04] Hot/Cold rebalance 통합~~
- **DONE 2026-04-26**: `ObjectMeta.Class` field, `rebalance.ClassResolver` interface, `ComputePlan(coord, store, class)` 시그니처. class subset 으로 placement 한정. `MetaStore.ListRuntimeDNsByClass` resolver 가 R 미만이면 fallback (deadlock 방지). 2 unit tests.

### ~~[P5-05] WAL group commit × transactional Raft 상호작용 검증~~
- **DONE 2026-04-26**: `TestWALBatched_HookFiresExactlyOnceUnderConcurrency` — 30 concurrent Append + group commit + WAL hook. 모든 entry 가 hook 을 정확히 1번 firing, 모든 seq 디스크에 unique. 핵심 race window 검증.

---

## P6 — Season 5 (coord 분리)

### ~~[P6-01] Season 5 Ep.1 — kvfs-coord skeleton~~
- **DONE 2026-04-26**: `cmd/kvfs-coord/main.go` + `internal/coord/` (4 RPC: place/commit/lookup/delete + healthz). 2 unit tests PASS. demo-aleph.sh 라이브. Dockerfile + Makefile + down.sh 갱신. backward compat 100% (edge 코드 0 변경).

### ~~[P6-02] Edge → coord client 통합~~
- **DONE 2026-04-27**: `internal/edge/coord_client.go` (CommitObject/LookupObject/DeleteObject/Healthz). edge.Server 에 `CoordClient` 필드, `commitPutMeta`/`lookupMeta`/`deleteMeta` 헬퍼 분기. handleHead/handleGet/handleDelete 모두 lookupMeta 경유. cmd/kvfs-edge/main.go: `EDGE_COORD_URL` env, boot 시 healthz fail-fast, coord-proxy 모드면 auto-trigger 자동 비활성. 데모 demo-bet.sh: edge.db vs coord.db 크기 비교로 단일 source 검증. 2 unit tests (round-trip + healthz fast-fail). 기존 동작 0 변경 (env unset 시 인라인).

### ~~[P6-03] coord 자체의 Raft (peer set, election 재사용)~~
- **DONE 2026-04-27**: ADR-038 작성 + 구현. `internal/election` 그대로 재사용 (새 state machine 0). `coord.Server.Elector` field, mounting at `/v1/election/{vote,heartbeat}`. follower coord 가 mutating RPC 받으면 503 + `X-COORD-LEADER`. edge.CoordClient 가 transparent 1-hop redirect (`MaxLeaderRedirects=1` default). `COORD_PEERS`/`COORD_SELF_URL` env. 데모 demo-gimel.sh (히브리 ג): 3-coord election + leader kill + new leader. **알려진 갭**: 새 leader bbolt 비어있음 (P6-04 가 sync). 1 unit test (TestRequireLeader_FollowerRejectsWritesPropagatesLeaderHint).

### ~~[P6-04] coord 간 메타 sync (WAL replication 재사용)~~
- **DONE 2026-04-27**: ADR-039 작성 + 구현. coord daemon 이 `COORD_WAL_PATH` 설정 시 `store.OpenWAL` + leader-side `MetaStore.SetWALHook` (Elector.ReplicateEntry 호출) + follower-side `Elector.AppendEntryFn` (MetaStore.ApplyEntry 호출). `coord.Server.Routes()` 가 `/v1/election/append-wal` 마운트. 새 메커니즘 0 — ADR-031 follow-up (Ep.8) 의 패턴 그대로 location 만 이동. ~50 LOC + ADR + demo-dalet.sh (히브리 ד) — pre-failover write 가 새 leader 에서 200 으로 복원 (gimel 의 404 갭 메움). 146 tests PASS.

### ~~[P6-07] coord transactional commit (ADR-034 port)~~
- **DONE 2026-04-27**: ADR-040 작성 + 구현. `coord.Server.{TransactionalCommit, ReplicateTimeout}` field, `commit()` helper 가 분기. `COORD_TRANSACTIONAL_RAFT=1` env (Elector + WAL 둘 다 필수, mismatch 시 startup fatal). 2 unit tests: quorum failure → bbolt 무변화 + WAL.LastSeq 무변화, prerequisite 누락 시 fallback. 데모 demo-he.sh (히브리 ה) — 2/3 coord kill 후 PUT 503 + lookup 404 + 복구 후 PUT 200. 148 tests PASS.

---

## P8 — Frame-1+2 마감 wave (chaos suite + textbook primitives)

목표: 헌장 기준(frame 1) + 학부 텍스트북 기준(frame 2) 모두 100%. 약 5주 작업으로 추정.
2026-04-27 진입. 순서: chaos suite → S5/S6 blog backfill → Season 7 (textbook primitives).

### ~~[P8-01] Phase 1 chaos suite 시드~~
- **DONE 2026-04-27**: 3개 시나리오 (chaos-coord-flap, chaos-coord-quorum-loss, chaos-suite orchestrator) + scripts/lib/cluster.sh 에 `rebuild_images` 헬퍼.
- 발견 (test 의 self-discovery): `ensure_images` 가 "한 번만 빌드" 라서 stale-image 가능. P7-07/P7-08 이 들어오기 전에 빌드된 edge image 가 잡혀있어 chaos test 가 거짓 phantom 을 보고했음. 신규 `rebuild_images` (Docker BuildKit 캐시 의존) 로 chaos/CI 는 항상 fresh 보장. ensure_images 는 demo 용으로 보존 (속도 우선).
- ADR-040 transactional commit invariant 가 chaos 하에서 holds — 5/5 PUT 503 차단 + bbolt drift 0 + phantom 0 검증.
- ADR-038/039 (coord HA + WAL repl) 도 holds — 90s 동안 random coord kill, 모든 200-PUT 복원.

### ~~[P8-02] Phase 1 chaos suite 확장 (partition + mixed)~~
- **DONE 2026-04-27**: `chaos-coord-partition.sh` + `chaos-mixed.sh` 추가, `chaos-suite.sh` 에 등록.
- partition 테스트: `docker network disconnect` 로 1 coord 격리 → split-brain count == 0 (ADR-038 invariant 검증) + 재연결 후 elected leader 정상.
- mixed 테스트: DN + coord 동시 flap → **architectural finding 발생** (P8-06 참조). 확률적 — 같은 명령으로 한 번은 PASS, 한 번은 2/24 데이터 손실.
- WAL corrupt + disk-full 시나리오는 P8-07 로 이연 (구현 비용 > Frame-1+2 100% 진척 비율).

### ~~[P8-06] Simplified Raft 강화~~
- **DONE 2026-04-27**: ADR-050 작성 + 양 갈래 구현.
  - **Part 1**: `election.Config.LastLogSeqFn` 신규. `HandleVote` 가 voter.lastLogSeq > candidate.lastLogSeq 면 vote 거부 (Raft §5.4.1). coord daemon 이 `WAL.LastSeq` 으로 wire. unit test `TestVoteRejectStaleLog`.
  - **Part 2**: coord 측 `GET /v1/coord/admin/meta/snapshot` 신규 endpoint. coord daemon boot 시 `bootstrapFromPeer` (leader 우선 probe → snapshot fetch → atomic Reload). 모든 peer unreachable 시 best-effort warn + 진행.
- **검증**: chaos-mixed 5회 연속 실행 모두 `FINAL_FAIL=0` (이전엔 평균 5회 중 1회 violation). hard durability invariant 가 일관 holds.
- 161 unit tests PASS · `go vet` 클린.
- frame 2 (textbook Raft) 의 진척 — log-up-to-date invariant 만큼은 정통 Raft 동등.

### [P8-07] chaos suite 확장 wave 2 (저우선)
- `chaos-wal-corrupt.sh` — WAL tail truncate / mid-line garble → replay recover. WAL 의 corruption tolerance 검증.
- `chaos-disk-full.sh` — DN tmpfs cap → edge graceful 5xx, 회복 후 repair worker 자동 healing.
- 추정: 3~5일.

### ~~[P8-03] S5/S6 blog backfill~~
- **DONE 2026-04-27**: 14 episode 작성 (Ep.29~42). S5 (29-coord-skeleton ~ 35-cli-coord-admin) + S6 (36-coord-rebalance-plan ~ 42-edge-urlkey-sync). aleph→nun demo + ADR-015·038~049 본문 모두 cover.
- S5 Ep.2 의 ADR 결번 backfill 은 **별도 ADR 추가 안 함** — 본 episode (Ep.30) 본문에 결정 기록 (env wiring 자체가 ADR-015 의 follow-up 이고 별개 결정 부재). ADR README 의 "Season 5 Ep.2 는 ADR 없이 wiring 만" 노트와 정합.
- 평균 episode 길이 ~120 LOC, 합계 ~1700 LOC. 기존 28 episode 의 평균 (~120 LOC) 와 동일한 톤.
- 본 작업 마감으로 **frame 1 (헌장 기준 100%)** 도달.

### ~~[P8-04] Season 7 — textbook primitives~~
- ~~**Ep.1 (ADR-051)**: Failure domain hierarchy~~ — **DONE 2026-04-27**. `placement.Node.Domain` + `PickByDomain` greedy walk + admin endpoint + cli `dns domain` + demo-samekh (3 racks × 2 DN, R=3 spread + EC 4+2 per-stripe spread). 3 unit tests + ADR-051 + blog Ep.43.
- ~~**Ep.2 (ADR-052)**: Degraded read~~ — **DONE 2026-04-27**. `Server.parallelFetchShards` (K+M goroutine, first-K-wins, cancel + drain) + `kvfs_ec_degraded_read_total` metric (data shards missing 시) + demo-ayin (1MB EC 4+2, 2 DN kill, GET 정상). 3 unit tests + ADR-052 + blog Ep.44.
- ~~**Ep.3 (ADR-053)**: Tunable consistency~~ — **DONE 2026-04-27**. `X-KVFS-W` + `X-KVFS-R` 헤더 + `WriteChunkToAddrsW` + `readChunkAgreement` (sha256 agreement = free integrity probe) + `kvfs_tunable_quorum_total{op,value}` metric + demo-pe (9 stages: default·invalid·strong·weak·R=3 agreement). 3 unit tests + ADR-053 + blog Ep.45. **부수 fix**: handleGet 의 phantom Content-Length 정정 (502 응답에 잘못된 CL 빼기).
- ~~**Ep.4 (ADR-054)**: Anti-entropy / Merkle tree~~ — **DONE 2026-04-27**. DN `/chunks/merkle` (256-bucket flat tree) + `/chunks/merkle/bucket` + `/chunks/scrub-status` + 백그라운드 scrubber (`DN_SCRUB_INTERVAL`). Coord `/v1/coord/admin/anti-entropy/run` worker (expected vs actual per-DN diff via Merkle root → bucket → enumerate). cli `anti-entropy run --coord URL`. demo-tsadi 6 stages (clean / inventory drift / bit-rot 모두 detect). 4 unit tests + ADR-054 + blog Ep.46. **S7 close → frame 2 = 100%**.

### P8-04 마감 — Frame 1 + Frame 2 = 100%
- Frame 1 (헌장 — 살아있는 reference): **100%** (P8-03 마감)
- Frame 2 (textbook primitives): **100%** (P8-04 4 ep 모두 마감)

### ~~[P8-08] Anti-entropy auto-repair + scheduled audit (ADR-054 후속)~~
- **DONE 2026-04-27**: ADR-055.
  - `POST /v1/coord/admin/anti-entropy/repair` — leader-only, COORD_DN_IO 필수. detection (ADR-054) + 자동 복구 — replication chunks 만 (EC 는 ADR-046 worker 위임, "ec-deferred" 메시지로 skip).
  - `Server.runAntiEntropyRepair`: ObjectMeta walk → chunkOwners 맵 → audit 의 `MissingFromDN` 별 healthy source 찾아 `Coord.ReadChunk` + `Coord.PutChunkTo`.
  - `COORD_ANTI_ENTROPY_INTERVAL` env opt-in: leader 가 ticker 로 주기 audit (auto-repair 는 별도 — ticker 가 자동 트리거 안 함, operator agency 보존).
  - cli `kvfs-cli anti-entropy repair --coord URL`.
  - demo-anti-entropy-repair 7 stages: clean → rm chunk → audit detect → repair 1 → audit clean → GET 정상.
- detection (ADR-054) → action (ADR-055) loop 닫힘. cluster self-heal.

### ~~[P8-09] Anti-entropy 후속 — corrupt repair + dry-run~~
- **DONE 2026-04-27**: ADR-056.
  - Scrubber-detected corrupt 자동 repair: `?corrupt=1` flag → 각 DN 의 `/chunks/scrub-status` 수집 → healthy source 에서 force overwrite.
  - DN handlePut 의 idempotent skip 우회 위해 `?force=1` query param 추가. body 의 sha256(body) == chunk_id 검증은 그대로 — invariant 보존.
  - `Coordinator.PutChunkToForce` 신규 (원본 `PutChunkTo` 영향 0).
  - Dry-run flag (`?dry_run=1`): preview without byte movement. ChunkRepairOutcome 에 `Planned: true` 표시.
  - ChunkRepairOutcome 에 `Reason` 필드 (`"missing"` | `"corrupt"`) — operator 가 detection channel 식별.
  - cli flags `--corrupt` / `--dry-run`.
  - demo-anti-entropy-repair-corrupt 7 stages: bit-rot inject → scrubber detect → dry-run (no byte movement 검증) → real repair → next scrub clean.
- detection (ADR-054) → action (ADR-055/056) 두 갈래 (missing + corrupt) 모두 closed.

### ~~[P8-10] Anti-entropy EC inline repair~~
- **DONE 2026-04-27**: ADR-057.
  - `?ec=1` flag → ObjectMeta walk → 영향 받은 stripe 단위 `repair.StripeRepair` 빌드 (Survivors = anti-entropy 가 healthy 라고 mark, DeadShards = missing) → 기존 `repair.Run` 위임.
  - ADR-046 worker 의 reconstruct 코드 재사용. 새 reconstruct 로직 0 LOC.
  - `mode: "ec-inline"` (per-shard) + `mode: "ec-summary"` (집계) outcomes.
  - DryRun 동작: `repair.Run` 호출 안 함, ec-inline 만 Planned 표시.
  - cli `--ec` flag.
  - demo-anti-entropy-repair-ec 6 stages: 1MB EC (4+2) PUT → shard rm → ec-deferred (default) → ec=1 inline reconstruct → audit clean → GET sha256 정상.
- replication missing (ADR-055) + replication corrupt (ADR-056) + EC missing (ADR-057) 모두 anti-entropy 단일 명령으로 처리.
- 남은 빈 칸: EC corrupt (P8-11 후보).

### ~~[P8-11] EC corrupt repair (self-heal coverage 마지막 빈 칸)~~
- **DONE 2026-04-27**: ADR-058.
  - `repair.DeadShard.Force bool` 필드 + `repair.Coordinator.PutChunkToForce` 인터페이스 메서드 추가. `repairStripe` 가 `d.Force` 분기로 PUT 메서드 선택.
  - anti-entropy 의 EC plan 빌더가 corrupt switch case 추가 (`corruptHere → DeadShard{Force: true}`). Survivor 후보에서 corrupt 제외 (자기 source 가 corrupt 면 사용 X).
  - 새 endpoint / cli flag 0 — 기존 `--corrupt --ec` 조합으로 활성.
  - demo-anti-entropy-repair-ec-corrupt 7 stages: 1MB EC (4+2) → corrupt inject → scrubber detect → ec=1 alone fail → ec=1+corrupt=1 reconstruct + force overwrite → next scrub clean → GET sha256 정상.
- **Self-heal coverage 100%** — 4 detection 채널 (replication missing/corrupt + EC missing/corrupt) 모두 anti-entropy 단일 명령 (`--corrupt --ec`) 으로 처리.

### ~~[P8-12] Repair throttle + per-stripe precision~~
- **DONE 2026-04-27**: ADR-059.
  - per-stripe `repair.Run` 호출로 정밀한 OK/Err 매핑. ec-summary totals 도 정확.
  - `AntiEntropyRepairOpts.MaxRepairs` + `?max_repairs=N` query + cli `--max-repairs N`. successCount counter closure-shared between replication + EC paths.
  - throttled outcomes 는 stateless resumable — 다음 repair 호출에서 audit 다시 잡힘.
  - demo-anti-entropy-throttle 6 stages: 5 chunks rm → max_repairs=2 → 2 repaired + 3 throttled → 두 번째 호출에서 나머지 → audit clean.

### ~~[P8-13] Concurrent EC repair + httputil helper~~
- **DONE 2026-04-27**: ADR-060.
  - `AntiEntropyRepairOpts.Concurrency` + `?concurrency=N` query + cli `--concurrency N`.
  - up-front throttle partition (워커 시작 전 schedule 결정) → counter race 0. 워커 pool 은 deterministic 잡 큐만 처리.
  - mu-guarded outcome update + stats. critical section sub-microsecond.
  - `httputil.ParseNonNegIntQuery` 추출 (P8-12 reviewer deferred 항목): max_repairs + concurrency 두 use site 가 같은 helper. 9 subtests cover (empty/valid/zero/upper/exceeds/max=0/negative/non-numeric/empty-value).
  - demo-anti-entropy-concurrent 6 stages: 64 MiB EC (4+2) → 4-stripe corrupt → serial 272ms vs concurrent=4 135ms = **2.01× speedup** (single-host demo, multi-host 더 큼).

### ~~[P8-14] Resilience polishes (replication concurrent + persistent scrubber + unrecoverable signal)~~
- **DONE 2026-04-27**: ADR-061.
  - **Part A — Replication concurrent**: `runAntiEntropyRepair` replication for-loop 을 EC pass 와 같은 worker-pool 로 교체. 함수-스코프 `mu` 재사용 (HTTP I/O 는 잠금 밖, slice append/throttle 검사만 잠금 안). `?concurrency=N` 이 EC + replication 모두 적용. demo 측정: 8-chunk audit 99ms (serial) → 65ms (conc=4) = 1.52× speedup.
  - **Part B — Persistent scrubber state**: `<DN_DATA_DIR>/scrub-state.json` (Version 1, atomic temp+rename). `markCorrupt`/`scrubOne` healthy 분기에서 mutation 마다 `persistCorruptSetLocked()`. `StartScrubber` 가 `loadCorruptSet()` 으로 init (missing/parse-fail/version-mismatch → 빈 map graceful fallback). DN restart 시 corrupt set 즉시 재현 — 이전엔 다음 scan 까지 대기.
  - **Part C — Unrecoverable slog.Error**: `out.Skipped[Mode=no_source]` → `slog.Error("anti-entropy: unrecoverable chunk", target_dn, chunk_id, reason, err)`. log aggregator (ELK/Loki/Datadog) 의 `level=ERROR` + `msg` 키 한 줄로 alert rule 가능. API/JSON 응답 변경 0.
  - demo-anti-entropy-resilience 3 stage 모두 PASS. dn merkle_test +2 신규 (TestScrubber_CorruptStatePersistsAcrossRestart, TestScrubber_PersistedFileShape).

### ~~[P8-15] Auto-repair scheduling + coord /metrics surface~~
- **DONE 2026-04-27**: ADR-062.
  - **Part A — `COORD_AUTO_REPAIR_INTERVAL`**: ADR-055 의 audit-only ticker 의 자연 짝. 새 `StartAutoRepairTicker` (leader-only, DN_IO prerequisite) + 3 env (`INTERVAL`, `MAX`, `CONCURRENCY`). 기본 corrupt+ec=true 로 full self-heal, max-repairs cap 으로 burst 안전. failover 시 새 leader 가 즉시 인계.
  - **Part B — Coord `/metrics`**: `internal/coord/metrics.go` (edge pattern 미러). 5 counter (`audits_total`, `repairs_total{reason}`, `unrecoverable_total`, `throttled_total`, `auto_repair_runs_total`) + 2 gauge (`election_state`, `objects`). `recordX` helpers nil-safe (SetupMetrics 미호출 시 no-op). `/metrics` route 항상 wired, default on (`COORD_METRICS=0` 으로만 off).
  - ADR-061 의 slog.Error 와 lockstep — unrecoverable 발견 시 `slog.Error` + `recordUnrecoverable()` 동시 발화. operator 가 어느 채널이든 watch 하면 됨.
  - demo-anti-entropy-auto-metrics 3 stage: ticker alive (interval=2s 후 runs ≥1) → 1 chunk rm → auto-tick 후 `repairs{reason="missing"} = 1` → 모든 replica rm → `unrecoverable_total` advance. 마지막에 `/metrics` 의 anti-entropy slice dump.
  - 코드: ~130 LOC metrics.go + ~50 LOC ticker + ~30 LOC main wiring.
  - 운영 패턴 두 가지 가능: light (audit-only ticker) / continuous self-heal (둘 다 set).

### ~~[P8-16] Anti-entropy observability completions~~
- **DONE 2026-04-27**: ADR-063.
  - **Histogram metrics**: `internal/metrics.Histogram` 추가. Prometheus cumulative bucket + `_sum` + `_count`. coord 에 `kvfs_anti_entropy_audit_duration_seconds`, `kvfs_anti_entropy_repair_duration_seconds` 연결.
  - **Skipped counter**: `kvfs_anti_entropy_skipped_total{mode}` (`skip`, `ec-deferred`). 성공/불가/throttle 외 deferred outcome 도 Prometheus 에 남김.
  - **Unrecoverable dedupe**: process-local `unrecoverableSeen` set. 같은 lost chunk 가 auto-repair tick 마다 재발견되어도 첫 발견만 `slog.Error` + `unrecoverable_total` 증가. 100k cap reset 으로 memory bound.
  - demo-anti-entropy-observability 3 stage PASS: histogram dump → EC deferred counter → unrecoverable dedupe.
  - blog Ep.55 작성.
  - 신규 tests: `TestHistogram_CumulativeBucketsSumCount`, `TestHistogram_NegativeClampsToZero`, `TestMetrics_HistogramsAndSkippedRender`, `TestUnrecoverableDedupe_FirstSeenOnceThenSilent`.

### [P8-17] Anti-entropy 남은 한계효용 polish (저우선)
- per-shard 정확한 success/failure (repair package refactor — 현재는 per-stripe)
- `repair.Run` 자체 multi-stripe parallelism 활용
- Multi-tier hierarchical Merkle (256-bucket flat → depth ≥2)
- Peer-to-peer DN self-heal
- Adaptive auto-repair (load 감지 시 burst cap 자동 축소)
- Scrubber rate adaptive (load 감지 시 slowdown)
- ADR 번호: 본래 050~053 예정이었으나 P8-06 (ADR-050) 가 ADR-050 을 가져가 → S7 은 **051~054** 사용.

### [P8-05] Phase 1 chaos test 의 Phase D drift check 정확도 개선
- **현황**: `coord_object_count` 가 admin/objects 응답 (raw JSON array) 의 length 를 jq 로 read. P8-01 에서 fix 됨.
- **잔여 보강**: 단순 length 가 아니라 specific phase-A 키 존재 / phase-C 키 부재까지 검증하면 더 강력. 우선순위 낮음 — Phase F 가 이미 phantom 검증을 cover 하므로 중복.

---

## P7 — Season 6 (coord operational migration)

### ~~[P7-01] Season 6 Ep.1 — rebalance plan on coord~~
- **DONE 2026-04-27**: ADR-043 작성 + 구현. coord `/v1/coord/admin/rebalance/plan`. `rebalancePlanCoord` 어댑터 (placer + Store 위에 rebalance.Coordinator 인터페이스 충족, ReadChunk/PutChunkTo는 명시적 에러). cli `rebalance --plan --coord URL`. 1 unit test (TestRebalancePlan_DetectsMisplacedChunk). 데모 demo-chet.sh (히브리 ח). 153 tests PASS.

### ~~[P7-02] Season 6 Ep.2 — rebalance apply on coord~~
- **DONE 2026-04-27**: ADR-044. `coord.Server.Coord *coordinator.Coordinator` (COORD_DN_IO=1). `/v1/coord/admin/rebalance/apply`. cli `--apply --coord`. demo-tet (히브리 ט).

### ~~[P7-03] Season 6 Ep.3 — GC plan + apply on coord~~
- **DONE 2026-04-27**: ADR-045. `/v1/coord/admin/gc/{plan,apply}`. cli `gc --coord`. min-age=DURATION 형식 (edge 의 min_age_seconds 와 다름, cli 가 변환). demo-yod (히브리 י).

### ~~[P7-04] Season 6 Ep.4 — EC repair on coord~~
- **DONE 2026-04-27**: ADR-046. `/v1/coord/admin/repair/{plan,apply}`. cli `repair --coord`. K survivors → Reed-Solomon Reconstruct → 누락 shard 재배포. demo-kaf (히브리 כ).

### ~~[P7-05] Season 6 Ep.5 — DN registry mutation on coord~~
- **DONE 2026-04-27**: ADR-047. `/v1/coord/admin/dns` POST/DELETE + `/v1/coord/admin/dns/class` PUT. 모두 requireLeader gate. cli `dns add/remove/class --coord`. urlkey rotation 은 본 ADR 비포함 (별도 ep). 1 unit test. demo-lamed (히브리 ל).

### ~~[P7-06] coord-side URLKey rotation~~
- **DONE 2026-04-27**: ADR-048. coord `/v1/coord/admin/urlkey` (list/rotate/remove). cli `urlkey --coord` 모든 subcommand 지원. `RotateURLKeyRequest` 에 `is_primary` 필드 (cli default true). 1 unit test (rotate v1 → rotate v2 demotes v1 → remove v1 → remove unknown 404). 데모 demo-mem.sh (히브리 מ). **명시적 비포함**: edge in-memory Signer 의 propagation (다음 작업 P7-09 대기).

### ~~[P7-09] Edge in-memory Signer propagation (URLKey 변경 사후 동기화)~~
- **DONE 2026-04-27**: ADR-049. Polling 채택 (`EDGE_COORD_URLKEY_POLL_INTERVAL`, default 30s). `internal/edge/urlkey_sync.go` 의 `SyncURLKeys` (Add/SetPrimary/Remove diff). `Server.StartURLKeyPolling` background goroutine. 2 unit tests. 데모 demo-nun.sh (히브리 נ 14번째). Lazy 401-refresh 는 미채택 — needs 명확해질 때 ADR-050 후보.

### ~~[P7-07] coord placer + Coord embedded — dual placer 정리~~
- **DONE 2026-04-27**: handlePlace 가 Coord != nil 일 때 `s.Coord.PlaceN` 사용. nil 일 때만 raw Placer fallback. drift 가능성 제거.

### ~~[P7-08] CANDIDATE 상태 retry 예산~~
- **DONE 2026-04-27**: CoordClient.do() 가 503 + 빈 X-COORD-LEADER 보면 200ms backoff + retry (maxRedirects 가 cap). Election convergence (~3s) 안에 transparent 회복.

---

### ~~[P6-10] Edge-side meta cache (per-bucket-key, short-TTL)~~
- **DONE 2026-04-27**: opt-in `EDGE_COORD_LOOKUP_CACHE_TTL`. CoordClient 에 `cache map[bucket\x00key]cachedMeta` + RWMutex. CommitObject + DeleteObject 가 같은 client 의 entry invalidate (다른 edge 의 mutation 은 TTL 만료 대기 — 짧은 TTL 권장). 1 unit test. 측정 후 default 결정.

### [P6-12] CoordClient 캐시 size cap / LRU eviction (저우선)
- **배경**: P6-10 의 `cache map` 은 unbounded — TTL 만료 후 read 까지는 메모리 유지. 수백만 unique key workload 에서 monotonic growth. /simplify (2026-04-27) 발견.
- **스펙**: max-entries cap (e.g. 10k) 또는 주기적 sweeper (cheap — TTL 초 단위). 측정 후 결정.
- **우선순위**: 저우선 (현재 demo / 운영 규모에서 안 보임).

### ~~[P6-11] up.sh → lib/cluster.sh source~~
- **DONE 2026-04-27**: up.sh 가 `start_dns 3` + `start_edge edge 8000` + `wait_healthz` 사용. ~25 LOC 절약. up-tls.sh 는 TLS env 가 lib 의 surface 를 확장시켜서 그대로 보존 (별도 작업).

### [P6-08] scripts/lib/cluster.sh 추출
- **배경**: demo-bet/gimel/dalet 의 docker-build + DN-spawn boilerplate (~50줄) 가 반복. 기존 `scripts/lib/common.sh` 는 sign_url 만 제공. /simplify 가 발견.
- **스펙**: `start_dns N`, `start_coord <name> <port> [peers]`, `start_edge <coord_url>`, `wait_healthz <url>` helpers. S5 demos + 향후 S6 demos 에서 source.
- **부수 작업**: S5 demos 가 `lib/common.sh` 의 `sign_url` 사용 (현재는 `docker run kvfs-cli sign` 호출 — 무거움).

### ~~[P6-09] internal/{cliutil,httputil} 추출~~
- **DONE 2026-04-27**: `internal/cliutil` (EnvOr / AtoiOr / SplitCSV / Fatal) + `internal/httputil` (WriteJSON / WriteError / WriteErr). cmd/kvfs-{edge,coord,dn}/main.go + internal/{edge,coord} 모두 thin shims 로 교체. 트리거 임계 (4 binaries / 2 HTTP packages) 거의 도달했고 P6-10/P7-09 가 같은 패턴 또 늘릴 가능성에 선제 정리.

### ~~[P6-05] edge 의 placement 코드 완전 제거~~
- **DONE 2026-04-27**: ADR-041 작성 + 구현. `CoordClient.PlaceN` 추가, `Server.placeN` helper 분기, `writeChunkPreferClass` 와 `handlePutECStream` 모두 placeN 경유. 1 unit test (TestCoordClient_PlaceN_ReturnsCoordsView). 데모 demo-vav.sh (히브리 ו) — coord 가 dn1/2/3 만, edge 가 dn1/2/3/4 알 때 chunks 가 dn4 에 절대 안 감 (proof of routing). 149 tests PASS. `internal/coordinator/` 의 placement 부분은 fallback 용 그대로 (CoordClient nil 일 때). 완전 제거는 후속 정리.

### ~~[P6-06] kvfs-cli 가 coord 직접 admin~~
- **DONE 2026-04-27**: ADR-042 (read-only inspect). coord 에 `/v1/coord/admin/{objects,dns}` 추가, cli `inspect` 에 `--coord URL` 플래그. 1 unit test (TestAdminEndpoints_ListObjectsAndDNs). 데모 demo-zayin.sh (히브리 ז) — coord 직접 인스펙트 + edge.db 비어있음 대비. mutating admin (rebalance/gc/urlkey/dns) 의 coord 이전은 후속 ADR-043/044 예상.

---

### ~~[P5-09] 블로그 episode 23~28 (P4-09 wave 정리)~~
- **DONE 2026-04-26**: 6편 완료. Ep.23 Prometheus metrics · Ep.24 SIMD-style RS · Ep.25 Hot/Cold tier (placement bias + P5-04 rebalance 통합) · Ep.26 NFS gateway deferred (ADR-032 회고) · Ep.27 WAL log compaction · Ep.28 Strict vs Transactional 비교. 합계 743 줄. blog 22 → 28 편.

### ~~[P5-06] 블로그 episode 18~22 작성~~
- **DONE 2026-04-26**: 5편 완료. Ep.18 (EC streaming) · Ep.19 (CDC × EC) · Ep.20 (sync replication) · Ep.21 (transactional Raft) · Ep.22 (micro-opts bundle). 합계 736 줄.
- **잔여 (= 신규 P5-09)**: P4-09 wave 의 자체 episode (Prometheus /metrics, SIMD-style RS, Hot/Cold tier, NFS deferred, log compaction, strict-vs-transactional 비교). 코드는 있으나 글 미작성.

---

## 완료된 주요 작업 기록

상세는 각 commit · ADR · blog episode 참조. 본 표는 **시즌 단위로 압축** — 2026-04-26 하루 (collapse 된 활동 기간) 동안 진행된 모든 작업을 시즌 별로 묶어둠.

| 시점 | 시즌 / 트랙 | 결과 |
|---|---|---|
| 2026-04-25 | 출범 + Season 1 MVP | clean-slate identity (ADR-001) · 2-daemon (ADR-002) · HTTP REST (ADR-003) · bbolt 메타 (ADR-004) · CA chunks (ADR-005) · UrlKey (ADR-007) · 첫 데모 α/ε PASS · 첫 commit |
| 2026-04-25 | Season 2 Ep.1 | Rendezvous Hashing (ADR-009) · placement-sim CLI · demo-zeta · blog Ep.2 |
| 2026-04-26 | Season 2 Ep.2~5 (closed) | Rebalance worker (ADR-010) · Surplus GC (ADR-012) · Chunking (ADR-011, supersedes ADR-006) · Reed-Solomon EC from-scratch (ADR-008). demo-eta/theta/iota/kappa · blog Ep.3~6 |
| 2026-04-26 | Season 3 Ep.1~7 (closed) | Auto-trigger (ADR-013) · EC stripe rebalance (ADR-024) · EC repair queue (ADR-025) · Meta backup (ADR-014) · DN heartbeat (ADR-030) · Auto-snapshot scheduler (ADR-016) · Multi-edge HA read-replica (ADR-022). + 운영 보강: Dynamic DN registry (ADR-027) · UrlKey rotation (ADR-028) · Optional TLS (ADR-029). demo-lambda/mu/nu/xi/omicron/pi/rho · blog Ep.7~13 |
| 2026-04-26 | Season 4 Ep.1~8 (closed) | Streaming PUT/GET (ADR-017) · Content-defined chunking (ADR-018) · Auto leader election (ADR-031) · WAL (ADR-019) · Follower WAL auto-pull (Ep.5 follow-up) · EC streaming (Ep.6 follow-up) · EC+CDC combined (Ep.7 follow-up) · Synchronous Raft-style WAL replication (Ep.8 follow-up). demo-sigma/tau/upsilon/phi/chi/psi/omega |
| 2026-04-26 | P4-09 wave (post-Season-4) | Prometheus /metrics · SIMD-style RS (mul-by-constant + 8-byte unroll, 2.3× ↑) · Hot/Cold tier (admin label + placement bias) · NFS gateway deferred (ADR-032) · WAL log compaction · Strict replication informational (ADR-033) · Transactional Raft commit-before-quorum (ADR-034) · S3-compatible API (HEAD + ListBucket) |
| 2026-04-26 | ADR-035 micro-opt bundle | WAL group commit (`EDGE_WAL_BATCH_INTERVAL=5ms`, ~15× write throughput) · 3-region FastCDC (`MaskBitsRelax > 0`, MaxSize cap 빈도 ↓) · chunker sync.Pool (alloc churn ↓). +/simplify pass: Truncate-during-wait deadlock fix · flushOnce mu-during-fsync release · pool oversize cap reject |
| 2026-04-26 | Doc·CI 정리 | CLAUDE.md L4 (-28% 줄, drift 제거) · ADR README 번호 오름차순 정렬 + missing 4건 추가 (032/033/034/035) · 아키텍처 가이드 GUIDE.md + guide.html (13장 walkthrough, mermaid 다이어그램, 자체 포함 HTML) · doc-drift CI (`scripts/check-doc-drift.sh` + `.github/workflows/doc-drift.yml`, 월별 cron + path-trigger). FOLLOWUP.md 자체 갱신 (본 commit) |
| 2026-04-26 | OSS 발행 | repo private→public + history rewrite + repo recreate (옛 SHA 영구 폐기) · LICENSE 헤더 23 .go 파일 · CONTRIBUTING + CODE_OF_CONDUCT · CI workflow (build/vet/test on Go 1.26, staticcheck, govulncheck) · benchmark suite 4 패키지 · chaos-dn-killer |
| 2026-04-27 | P8 anti-entropy polish | P8-08~16: auto-repair · corrupt/dry-run · EC inline/corrupt · throttle · concurrent EC/replication · persistent scrubber · continuous self-heal · coord Prometheus surface · histogram/skipped/dedupe observability completions. ADR-055~063 · blog Ep.47~55 · anti-entropy special demos 9개 |

---

## 현재 상태 요약 (2026-04-28)

- **Git**: main, GitHub `HardcoreMonk/kvfs` PUBLIC. HEAD `93a0a8f` (P8-15 auto-repair scheduling + /metrics), 워킹트리 P8-16 observability completions + 문서 현행화 반영 완료(미커밋)
- **테스트**: **190 unit test PASS target** (P8-16 +4). Docker `golang:1.26-alpine` 기준 `go test ./...` + `go vet ./...` PASS
- **데모**: 그리스 α~ω (S1~S4, 21개) + 히브리 aleph~nun (S5~S6, 14개) + S7 samekh~tsadi (Ep.1~4, 4개) + P8-08~16 anti-entropy demos (9개) = **48개**. 신규 `demo-anti-entropy-observability` PASS
- **ADR**: **59 Accepted** — ADR-001~063 중 020/021/023/026 4개 결번. post-S4: 032~037, S5: 015·038~042, S6: 043~049, P8: 050·055~063, S7: 051~054
- **Blog**: Ep.1~55 완성. S5/S6 blog backfill (P8-03) + S7 Ep.1~4 (Ep.43~46) + P8-08~16 (Ep.47~55)
- **시즌**: S1·S2·S3·S4 closed. S5 closed (Ep.1~7). S6 Ep.1~7 done (P6-12 만 저우선 잔존)
- **Chaos suite**: chaos-coord-{flap,quorum-loss,partition} + chaos-mixed + chaos-suite 오케스트레이터 — P8-06 fix 후 모두 안정 PASS

## 업데이트 규칙

- 새 P0/P1/P2/P3/P5 추가: 각 시리즈 안에서 번호 이어서 부여 (재활용 금지)
- 항목 완료: ✅ + 완료일 표기 후 "완료된 주요 작업 기록" 표에 1줄 이관 (시즌 단위 압축 OK)
- 우선순위 변경: P 라벨만 교체, 번호 유지
- 스펙 변경으로 작업 무의미해짐: `~~항목~~` 취소선 + 폐기 사유 한 줄
- **본 파일 자체 갱신은 commit 별 작은 diff** 로. 시즌 close · ADR landing 시 즉시.
