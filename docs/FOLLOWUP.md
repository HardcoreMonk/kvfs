# kvfs 후속 작업 (Follow-up)

`200.kvfs/` 의 **후속 작업 단일 소스**. 상태 업데이트는 이 파일만 수정한다.

문서 현행화 일자: **2026-04-26** · Season 4 close + P4-09 wave + 아키텍처 가이드 + doc-drift CI 까지 반영.

## 우선순위 맵

- **P0**: 차단·긴급. 즉시 처리 — 현재 **0건**
- **P1**: 명확한 스펙 존재, 실행 대기 — 현재 **1건** (P1-02 사용자 직접)
- **P2**: 리뷰·개선 권고 — 현재 **0건** (P2-01~09 모두 완료)
- **P3**: 사용자 결정 필요 — 현재 **1건** (P3-02, OTel 도입 여부)
- **P5**: post-Season-4 wave 후속 — 모두 완료 (P5-01~09 done)
- **P6**: Season 5 (coord 분리) — 현재 **1건** (P6-01 Ep.1 skeleton 완료, Ep.2~5 대기)

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

### [P5-01] ADR-036 — WAL batch metrics
- **배경**: ADR-035 group commit 이 실제 throughput 을 올리고 있는지 운영 중 관측 불가
- **스펙**: `kvfs_wal_batch_size` (gauge — 마지막 batch 의 entry 수), `kvfs_wal_durable_lag_seconds` (gauge — 가장 오래 대기 중인 미-fsync entry 의 age)
- **연결**: `internal/store/wal.go` flusher 에서 update, `internal/edge/metrics.go` 에 등록
- **출처**: ADR-035 본문 "후속" 섹션

### [P5-02] ADR-037 — chunker pool cap 정책
- **배경**: `internal/chunker/stream.go` `scratchPool` 은 정상 GC 회수에 의존. 메모리 압박 시명시적 evict 없음
- **스펙**: pool 의 슬랩 수 상한, 또는 cap 합계 상한. 초과 시 가장 오래된 것부터 drop
- **연결**: `getScratch`/`putScratch`. 측정용 카운터 (P5-01 과 묶어 처리 가능)
- **출처**: ADR-035 본문 "후속" 섹션

### ~~[P5-03] ADR-015 — Coordinator daemon 분리~~
- **ACCEPTED 2026-04-26**: ADR-015 Proposed → Accepted. ADR-002 supersede. **Season 5 진입**. 후속 작업은 P6-* 시리즈로 이전.

### [P5-04] Hot/Cold rebalance 통합
- **배경**: P4-09 wave 의 hot/cold tier 는 신규 PUT 만 hot 으로 bias. 기존 cold 데이터의 hot→cold 자동 migration 미구현
- **스펙**: `internal/rebalance/` 가 chunk 의 `Class` 라벨을 인식, ideal placement set 계산 시 class 필터 적용
- **출처**: ADR-035 follow-up commit message ("Out of scope: rebalance scope by class — defer to follow-up")

### [P5-05] WAL group commit × transactional Raft 상호작용 검증
- **배경**: `EDGE_WAL_BATCH_INTERVAL=5ms` + `EDGE_TRANSACTIONAL_RAFT=1` 동시 활성 시 동작 미검증
- **위험**: transactional Raft 는 `MarshalPutObjectEntry` 로 entry 미리 push 한 뒤 quorum 받으면 commit. group commit 이 그 commit 을 batch 로 묶으면 quorum-then-batch 순서가 어긋날 가능성
- **스펙**: integration test — 두 옵션 동시 활성으로 PUT × N concurrent → bbolt + 모든 follower bbolt 일치 검증

---

## P6 — Season 5 (coord 분리)

### ~~[P6-01] Season 5 Ep.1 — kvfs-coord skeleton~~
- **DONE 2026-04-26**: `cmd/kvfs-coord/main.go` + `internal/coord/` (4 RPC: place/commit/lookup/delete + healthz). 2 unit tests PASS. demo-aleph.sh 라이브. Dockerfile + Makefile + down.sh 갱신. backward compat 100% (edge 코드 0 변경).

### [P6-02] Edge → coord client 통합
- **다음 단계**: edge 가 `EDGE_COORD_URL` 보면 placement + commit RPC 를 coord 로 위임. inline 모드 fallback 유지.
- **스코프**: `internal/edge/coord_client.go` + `commitPutMeta` 분기 추가. 데모 demo-bet.sh.

### [P6-03] coord 자체의 Raft (peer set, election 재사용)
- **다음 단계**: coord HA. ADR-031 의 Elector 를 coord 가 가져와 다중 coord 구성. ADR 신규 (예상 ADR-038).

### [P6-04] coord 간 메타 sync (WAL replication 재사용)
- **다음 단계**: ADR-019 WAL + ADR-031 sync push 를 coord 간에. leader coord 가 follower coord 에 push.

### [P6-05] edge 의 placement 코드 완전 제거
- **다음 단계**: `internal/coordinator/` 의 placement 부분이 coord client wrapper 만 남음. ADR-002 기존 paragraph deprecate.

### [P6-06] kvfs-cli 가 coord 직접 admin
- **다음 단계**: `kvfs-cli coord-* ...` subcommand. admin 작업이 edge 통과 안 함.

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
