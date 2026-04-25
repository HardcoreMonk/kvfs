# kvfs 후속 작업 (Follow-up)

`200.kvfs/` 의 **후속 작업 단일 소스**. 상태 업데이트는 이 파일만 수정한다.

문서 현행화 일자: **2026-04-26** · Season 3 Ep.1 (ADR-013 Auto-trigger) 완료 시점 기준.

## 우선순위 맵

- **P0**: 차단·긴급. 즉시 처리 — 현재 **0건**
- **P1**: 명확한 스펙 존재, 실행 대기 — 현재 **2건**
- **P2**: 리뷰·개선 권고, 개인 프로젝트 여유 시 처리 — 현재 **9건**
- **P3**: 별도 스펙·사용자 결정 필요 — 현재 **4건** (P3-05/06 obsolete · 취소선)

---

## P1 — 실행 대기 (명확한 스펙)

### [P1-01] GitHub 레포 발행
- **상태**: Local `git init` + 2 commit 완료 (`main` 브랜치, `origin` 없음)
- **대기**: 사용자 직접 `gh repo create` 실행 (public/private 결정 후)
- **명령**:
  ```
  ! cd /data/projects/claude-zone/200.kvfs && gh repo create kvfs --private \
    --description "kvfs — Key-Value File System: an educational reference for distributed object storage (Go 1.26, Apache 2.0)" \
    --source=. --push --remote=origin
  ```
  (public 전환: `gh repo edit HardcoreMonk/kvfs --visibility public --accept-visibility-change-consequences`)

### [P1-02] Docker Compose plugin 설치
- **배경**: `./scripts/up.sh` 는 plain `docker run` 으로 대체 구현됨 (compose 미설치 환경 대응). `docker-compose.yml` 은 있으나 `docker compose` 명령 사용 불가
- **해결**: `sudo apt install docker-compose-plugin` — 사용자 직접
- **효과**: README 의 `docker compose up -d` 표준 명령이 동작. 현재는 `./scripts/up.sh` 권장

---

## P2 — 개선 권고 (개인 프로젝트 여유 시)

### [P2-01] LICENSE 헤더 .go 파일 추가
- 각 `.go` 상단에 SPDX-License-Identifier + Apache 2.0 short header
- 자동화: `make license` 타깃

### [P2-02] CONTRIBUTING.md · CODE_OF_CONDUCT.md
- 오픈소스 표준 템플릿. 개인 프로젝트라도 외부 기여 가능성 대비

### [P2-03] GitHub Actions CI
- `.github/workflows/ci.yml`
- PR·push 시: `go build ./...`, `go test ./...`, `go vet ./...`
- Go matrix: [1.25, 1.26] (하위호환 검증)
- 추가 고려: `staticcheck`, `govulncheck`

### [P2-04] placement-sim bar chart edge case
- 현재: 100% 초과 시 clamp. 이론적으로 없으나 방어 코드만 있음
- 작은 N (2~3 DN + R=3) 에서 일부 DN 100% 표시되는 것은 정상인데 시각적 오해 가능성. 주석 보강

### [P2-05] edge 동작 중 EDGE_DNS 변경 지원
- 현재: 시작 시 env var 1회 읽음. 변경하려면 재시작
- 개선: `/v1/admin/dns` POST 로 DN 동적 추가·제거 (재배치 로직은 ADR-010 가 담당)
- 기본적인 시스템 자체는 변경 최소

### [P2-06] Benchmark suite
- `internal/placement/placement_bench_test.go` — `BenchmarkPick` (N 별 lookup 레이턴시)
- `internal/urlkey/urlkey_bench_test.go` — `BenchmarkSign`, `BenchmarkVerify`
- README · blog 에서 숫자 인용 가능

### [P2-07] Chaos test — random DN kill
- `scripts/chaos-dn-killer.sh` — 주기적으로 random DN 중단·재시작
- 장시간 돌려 복구 경로 · 메타 일관성 검증

### [P2-08] Secret rotation (UrlKey `kid`)
- 현재: 단일 secret env var. 만료·누출 대응 어려움
- 개선: `urlkey_secrets` bucket 에 `kid → secret` 저장, URL 에 `kid=...` 추가
- 운영 측면 sophistication. MVP 에선 과함

### [P2-09] TLS / mTLS
- 현재: 모든 통신 평문 HTTP
- 개선: edge 에 TLS 서버 (cert-manager 또는 self-signed), edge↔DN mTLS
- 데모 순수성 때문에 Season 2 에서는 보류. Season 3+

---

## P3 — 사용자 결정·별도 스펙 필요

### [P3-02] 관찰성 스택 선택
- **현황**: ADR-013 으로 `slog` 구조화 로그 + `/v1/admin/auto/status` JSON endpoint 가 부분 도입됨. 클러스터-wide metrics 는 미정
- **옵션 A**: 기존 slog + `/metrics` 엔드포인트 추가 (Prometheus-exposition 포맷 직접 작성)
- **옵션 B**: OpenTelemetry SDK 풀 통합 (traces · metrics · logs)
- **옵션 C**: 외부 stack 없이 현재 구조 유지 (최소 변경)
- **개인 프로젝트 고려**: A 가 ADR-013 의 slog 흐름과 자연 연결. B 는 별도 Season 3 episode 감

### [P3-03] 100.legacy DFS/legacy-modernized 의 legacy_client_py3 라이브 검증
- **배경**: 평가 레포 `/data/projects/claude-zone/100.legacy DFS/legacy-modernized/installer/legacy-coke-nn/script/legacy_client_py3.py` 는 mock server 단위 테스트 14개 PASS. 라이브 legacy meta tier NN 상대 tcpdump 검증은 미완
- **결정 필요**: 별도 작업 트랙으로 할지, kvfs 에 집중해 평가 레포는 동결 유지할지
- **현 추세**: 동결 방향. 이 항목은 **completeness 목록** 용

### [P3-04] Public 전환 타이밍
- **현**: 아직 GitHub 발행 안 됨 (P1-01 대기 중). private 으로 만든 후 public 전환할지, 처음부터 public 으로 만들지 결정 필요
- **public 전환 기준** (사용자 결정 필요):
  - (a) 즉시 — Season 2 closed + Season 3 Ep.1 완료, 7 blog episode + 13 ADR + 10 demo 으로 첫 공개에 충분
  - (b) Season 3 Ep.2~3 완료 후 — 운영성 트랙 한두 개 더 채운 뒤 공개
  - (c) 후속 작업 (CI workflow P2-03, LICENSE 헤더 P2-01, CONTRIBUTING P2-02) 갖춘 후 공개

### ~~[P3-05] 블로그 Ep.3~5 주제 확정~~
- **OBSOLETE (2026-04-26)**: Ep.3~7 모두 완료 (rebalance · GC · chunking · EC · auto-trigger). 후보였던 dedup/repair/RPC 벤치 중 어느 것도 채택 안 됨 — 다른 방향이 더 나아서. 다음 Ep 결정은 신규 [P3-07] 가 받음

### ~~[P3-06] Season 3 로드맵 확정 시점~~
- **OBSOLETE (2026-04-26)**: Season 2 closed, Season 3 (운영성 트랙) Ep.1 (ADR-013 Auto-trigger) 으로 시작됨. 후속 ep 결정은 신규 [P3-07]

### [P3-07] Season 3 Ep.2 주제 결정
- **현**: Ep.1 Auto-trigger 완료. 운영성 트랙의 다음 ep 미정
- **후보** (ADR README 기준):
  - **ADR-024 — EC stripe rebalance**: ADR-008 의 EC 객체는 현재 rebalance 미적용. 6 DN 클러스터에 dn7 추가 시 stripe 재배치 동작이 빠짐. demo-zeta 와 동일한 갭 패턴 — 자연스러운 다음 ep
  - **ADR-022 — Multi-edge leader election**: ADR-013 의 single-edge 가정 깨기. auto loop 의 N edge 직렬화. coordinator daemon 분리 (ADR-015) 와 묶이는 큰 작업
  - **ADR-014 — Meta backup/HA**: bbolt 메타 손상 시 GC 의 자동 삭제가 위험. 안전망 강화
  - **ADR-017 — Streaming PUT/GET**: 현재 `io.ReadAll` 기반 (chunker 가 byte 슬라이스). 메모리 사용 상한 진짜 강제
  - **ADR-018 — Content-defined chunking**: 비정렬 데이터 dedup 효율 (rabin/buzhash)
  - **ADR-019 — SIMD-accelerated RS**: pure Go RS 의 ~50 MB/s → SIMD 1+ GB/s
  - **ADR-023 — Auto-trigger rate limiting**: 100만 객체 환경에서 한 cycle cap
- **추천 순서**: 024 (자연 후속 — 같은 알고리즘 패턴) → 014 (메타 안전성) → 022 (HA 본격)
- **결정 시 P1 로 승격**

---

## 완료된 주요 작업 기록

참고용. 상세는 각 commit · ADR 참조.

| 완료일 | 작업 | 결과 |
|---|---|---|
| 2026-04-25 | 기존 reference 평가 + 독자 프로젝트 identity | 17 KEEP/INHERIT 확정, `NAMING.md` 매핑 · ADR-001 |
| 2026-04-25 | Season 1 MVP 스캐폴딩 | 2-daemon · 22 파일 · 1,367 LOC · Apache 2.0 |
| 2026-04-25 | Season 1 α + ε 데모 라이브 통과 | 3-way replication 내구성 · UrlKey TTL 검증 |
| 2026-04-25 | Season 1 dedup 가설 검증 | 4 objects → 3 unique chunks (content-addressable 증명) |
| 2026-04-25 | CLAUDE.md + ADR 001~007 작성 | 7 ADR 불변 기록 · L3 체인 상속 |
| 2026-04-25 | git init + first commit (local) | `5cf3151 Initial commit — kvfs MVP` |
| 2026-04-25 | 블로그 Ep.1 실 데모 출력 embed | `304b991 blog(01): embed actual α/ε demo output` |
| 2026-04-25 | Season 2 Ep.1 — Rendezvous Hashing | `32d880a feat(placement)` · ADR-009 · 7 tests PASS |
| 2026-04-25 | placement-sim CLI 도구 | ASCII bar chart, 이동률 실측 표시 |
| 2026-04-25 | Blog Ep.2 작성 — Consistent Hashing | `blog/02-consistent-hashing.md` · placement-sim 3 케이스 실측 embed |
| 2026-04-25 | 4-DN 라이브 데모 스크립트 | `scripts/demo-zeta.sh` · seed 4 + add dn4 + new 8 쓰기, dn4 4/24 slots 실측, 기존 청크 정지 확인 |
| 2026-04-25 | Season 2 Ep.2 주제 결정 (P3-01 → P1-05) | ADR-010 Rebalance worker 채택, 010 → 011 → 008 로드맵 확정 |
| 2026-04-26 | Season 2 Ep.2 — ADR-010 Rebalance worker 구현 완료 | `internal/rebalance/` (8 tests PASS) + edge admin 엔드포인트 + `kvfs-cli rebalance` + `scripts/demo-eta.sh` (라이브 PASS) + `blog/03-rebalance.md` |
| 2026-04-26 | Season 2 Ep.3 — ADR-012 Surplus chunk GC 구현 완료 | `internal/gc/` (9 tests PASS) + DN `/chunks` list + edge admin + `kvfs-cli gc` + `scripts/demo-theta.sh` (라이브: 15→12 disk chunks, 메타↔디스크 정확 일치) + `blog/04-gc.md`. Rebalance trim-on-full-success 보정 동반 |
| 2026-04-26 | Season 2 Ep.4 — ADR-011 Chunking 구현 완료 (ADR-006 supersede) | `internal/chunker/` (13 tests PASS) + ObjectMeta 스키마 변경 (Chunks []ChunkRef + legacy adapter) + edge PUT/GET/DELETE 청크화 + rebalance/gc 청크 단위 갱신 + `EDGE_CHUNK_SIZE` env + `scripts/demo-iota.sh` (256 KiB → 4 청크 라이브 PASS) + `blog/05-chunking.md`. demo-eta/theta/alpha 회귀 fix 동반 |
| 2026-04-26 | Season 2 Ep.5 — ADR-008 Reed-Solomon EC 구현 완료 (Season 2 closed) | `internal/reedsolomon/` from-scratch (24 tests PASS): GF(2^8) + Vandermonde + Gauss-Jordan. ObjectMeta 에 ECParams + Stripes 추가, `Coordinator.PlaceN(stripeID, K+M)`, `X-KVFS-EC: K+M` 헤더로 per-object 모드, `internal/edge` 에 `handlePutEC` / `handleGetEC`. `scripts/demo-kappa.sh` (6 DN, 128 KiB / 4+2, dn5+dn6 kill 후 GET 복원 PASS) + `blog/06-erasure-coding.md`. GC `buildClaimedSet` 가 Stripes iterate |
| 2026-04-26 | Season 3 Ep.1 — ADR-013 Auto-trigger 구현 완료 | `internal/edge` 에 `AutoConfig` + `StartAuto(ctx)` + 두 개 `time.Ticker` 루프 (rebalance + GC). 기존 `rebalanceMu` / `gcMu` 공유로 수동/자동 직렬화. ring buffer 32 entries 의 `AutoRun` 기록 + `GET /v1/admin/auto/status` + `kvfs-cli auto --status`. `EDGE_AUTO=1` opt-in default. `scripts/demo-lambda.sh` (10s/12s interval, 운영자 명령 0번에 클러스터 정렬 PASS) + `blog/07-auto-trigger.md`. Season 3 운영성 트랙 시작 |
| 2026-04-26 | /simplify 리뷰 패스 — auto-trigger 코드 정리 | 3 agent 병렬 리뷰 후 6 항목 적용: `executeRebalance` / `executeGC` 헬퍼 추출 (handler+auto runner 공유), ring buffer no-op pollution fix (empty cycle은 lastCheck 만 갱신), redundant state 제거 (`autoLast/Next` → `autoLastCheck`), `autoLoop` 단일화, `parseAutoDur` / `mustGet` / `intQuery` 헬퍼, `AutoJob` typed enum. 246+/250- net -4 LOC, 0 behavior change, 66 tests PASS, demo-lambda 회귀 PASS |

---

## 현재 상태 요약 (2026-04-26)

- **Git**: main branch, 12 commits, no remote
- **클러스터**: `localhost:8000` running · edge(EDGE_AUTO=1) + dn × 4 (demo-lambda 가 down→up 까지 끝낸 상태)
- **테스트**: 7 placement + 5 urlkey + 8 rebalance + 9 gc + 13 chunker + 24 reedsolomon = **66 unit tests PASS**, `go vet` 클린
- **데모**: α, ε, dedup, ζ, η, θ, ι, κ, λ 전부 라이브 통과 (10종)
- **ADR**: 13건 (001~005 + 007~012 + 013, 006 superseded by 011) Accepted
- **Blog**: Ep.1~Ep.7 완성 (Ep.7 = Auto-trigger, Season 3 시작)
- **LOC**: Go ~5,300 + 문서 ~5,500 + scripts ~1,050
- **Season 2 closed**; **Season 3 (운영성 트랙) Ep.1 완료**, Ep.2 주제 미정 ([P3-07])

## 업데이트 규칙

- 새 P0/P1/P2/P3 추가: 번호 이어서 부여 (재활용 금지)
- 항목 완료: ✅ + 완료일 표기 후 "완료된 주요 작업 기록" 테이블에 1줄 이관
- 우선순위 변경: P 라벨만 교체, 번호 유지
- 스펙 변경으로 작업 무의미해짐: `~~항목~~` 취소선 + 폐기 사유 한 줄
