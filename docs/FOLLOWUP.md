# kvfs 후속 작업 (Follow-up)

`200.kvfs/` 의 **후속 작업 단일 소스**. 상태 업데이트는 이 파일만 수정한다.

문서 현행화 일자: **2026-04-26** · Season 4 close + P4-09 wave + 아키텍처 가이드 + doc-drift CI 까지 반영.

## 우선순위 맵

- **P0**: 차단·긴급 — 현재 **0건**
- **P1**: 명확한 스펙, 실행 대기 — 현재 **1건** (P1-02 사용자 직접)
- **P2**: 리뷰·개선 — 모두 완료 (P2-01~09 done)
- **P3**: 사용자 결정 필요 — 현재 **1건** (P3-02 OTel)
- **P5**: post-Season-4 wave — 모두 완료 (P5-01~09 done; FOLLOWUP 동기화 2026-04-27)
- **P6**: Season 5 (coord 분리) — Ep.1~7 모두 완료. 잔여: 3건 (P6-09 reserved, P6-10/11 저우선)
- **P7**: Season 6 (coord operational migration) — Ep.1~6 모두 완료. 잔여: 3건 (P7-07 저우선, P7-08 저우선, P7-09 중간)

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
- **DONE 2026-04-27**: opt-in `EDGE_COORD_LOOKUP_CACHE_TTL`. CoordClient 에 `cache map[bucket\x00key]cachedMeta` + RWMutex. CommitObject + DeleteObject 가 같은 client 의 entry invalidate (다른 edge 의 mutation 은 TTL 만료 대기 — 짧은 TTL 권장). 1 unit test (TestCoordClient_LookupCache_HitInvalidate). 측정 후 default 결정.

### ~~[P6-11] up.sh → lib/cluster.sh source~~
- **DONE 2026-04-27**: up.sh 가 `start_dns 3` + `start_edge edge 8000` + `wait_healthz` 사용. ~25 LOC 절약. up-tls.sh 는 TLS env 가 lib 의 surface 를 확장시켜서 그대로 보존 (별도 작업).

### [P6-08] scripts/lib/cluster.sh 추출
- **배경**: demo-bet/gimel/dalet 의 docker-build + DN-spawn boilerplate (~50줄) 가 반복. 기존 `scripts/lib/common.sh` 는 sign_url 만 제공. /simplify 가 발견.
- **스펙**: `start_dns N`, `start_coord <name> <port> [peers]`, `start_edge <coord_url>`, `wait_healthz <url>` helpers. S5 demos + 향후 S6 demos 에서 source.
- **부수 작업**: S5 demos 가 `lib/common.sh` 의 `sign_url` 사용 (현재는 `docker run kvfs-cli sign` 호출 — 무거움).

### [P6-09] internal/{cliutil,httputil} 추출 (저우선)
- **배경**: cmd/kvfs-{edge,coord}/main.go 의 envOr/splitCSV/fatal 중복 + internal/coord/coord.go 와 internal/edge/edge.go 의 writeJSON/writeError 중복. /simplify 발견.
- **스펙**: 5+ 바이너리 또는 4+ HTTP 패키지 가 생기면 진행. 현재 (4 binaries / 2 HTTP packages) 는 자체 보유가 더 readable. 트리거 조건 만족 시 진행.

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

---

## 현재 상태 요약 (2026-04-26)

- **Git**: main, GitHub `HardcoreMonk/kvfs` PUBLIC. 마지막 commit `f9a28b1` (doc-drift CI)
- **테스트**: **138 unit tests PASS** (chunker +3 / store +3 ADR-035 회귀 추가). `go vet` 클린
- **데모**: α β γ δ ε ζ η θ ι κ λ μ ν ξ ο π ρ σ τ υ φ χ ψ ω = 21개 (greek letter 전부) 라이브 PASS
- **ADR**: 35 Accepted (ADR-001~035, 미작성 015/020/021/023/026/036/037)
- **Blog**: Ep.1~17 완성. Ep.18~25 가 P5-06 punch list
- **시즌**: S1·S2·S3·S4 모두 closed. S5 진입은 P5-03 (Coordinator 분리) 채택 시 자동 트리거

## 업데이트 규칙

- 새 P0/P1/P2/P3/P5 추가: 각 시리즈 안에서 번호 이어서 부여 (재활용 금지)
- 항목 완료: ✅ + 완료일 표기 후 "완료된 주요 작업 기록" 표에 1줄 이관 (시즌 단위 압축 OK)
- 우선순위 변경: P 라벨만 교체, 번호 유지
- 스펙 변경으로 작업 무의미해짐: `~~항목~~` 취소선 + 폐기 사유 한 줄
- **본 파일 자체 갱신은 commit 별 작은 diff** 로. 시즌 close · ADR landing 시 즉시.
