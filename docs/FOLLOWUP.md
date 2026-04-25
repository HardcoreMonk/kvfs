# kvfs 후속 작업 (Follow-up)

`200.kvfs/` 의 **후속 작업 단일 소스**. 상태 업데이트는 이 파일만 수정한다.

문서 현행화 일자: **2026-04-26** · Season 2 Ep.4 (ADR-011 Chunking) 완료 시점 기준.

## 우선순위 맵

- **P0**: 차단·긴급. 즉시 처리 — 현재 **0건**
- **P1**: 명확한 스펙 존재, 실행 대기 — 현재 **2건**
- **P2**: 리뷰·개선 권고, 개인 프로젝트 여유 시 처리 — 현재 **9건**
- **P3**: 별도 스펙·사용자 결정 필요 — 현재 **5건**

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
- **옵션 A**: stdlib `slog` + `/metrics` 엔드포인트 (Prometheus-exposition 포맷 직접 작성)
- **옵션 B**: OpenTelemetry SDK 풀 통합 (traces · metrics · logs)
- **옵션 C**: 외부 stack 없이 현재 구조대로
- **개인 프로젝트 고려**: A 가 최소 변경. B 는 블로그 Ep.7~8 감

### [P3-03] 100.legacy DFS/legacy-modernized 의 legacy_client_py3 라이브 검증
- **배경**: 평가 레포 `/data/projects/claude-zone/100.legacy DFS/legacy-modernized/installer/legacy-coke-nn/script/legacy_client_py3.py` 는 mock server 단위 테스트 14개 PASS. 라이브 legacy meta tier NN 상대 tcpdump 검증은 미완
- **결정 필요**: 별도 작업 트랙으로 할지, kvfs 에 집중해 평가 레포는 동결 유지할지
- **현 추세**: 동결 방향. 이 항목은 **completeness 목록** 용

### [P3-04] Public 전환 타이밍
- **현**: `HardcoreMonk/kvfs` private (P1-01 완료 전제)
- **public 전환 기준** (사용자 결정 필요):
  - (a) 즉시 — 블로그 Ep.1 완성도 수용하면
  - (b) Season 2 Ep.1 블로그(ADR-009) 완성 후 — 2건 에피소드로 공개 단단함
  - (c) Season 2 전체 완성 후 — 풀 시즌 공개

### [P3-05] 블로그 Ep.3~5 주제 확정
- **Ep.3 후보**: dedup 내부 해부 (reference counting 필요 시점, 삭제의 어려움)
- **Ep.4 후보**: DN 영구 사망 시나리오 — repair queue 설계 (Season 2 프리뷰)
- **Ep.5 후보**: HTTP vs gRPC vs raw TCP 벤치마크
- 결정 시 P1 로 승격

### [P3-06] Season 3 로드맵 확정 시점
- Season 2 완료 후 어느 방향으로? gRPC/coordinator 분리/multi-region?
- **트리거 이벤트**: Season 2 Ep.5 완료 시점

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

---

## 현재 상태 요약 (2026-04-25)

- **Git**: main branch, 7 commits, no remote
- **클러스터**: `localhost:8000` running · edge(chunk_size=64KiB) + dn × 4 (demo-iota 가 down→up→4DN 까지 끝낸 상태)
- **테스트**: 7 placement + 5 urlkey + 8 rebalance + 9 gc + 13 chunker = **42 unit tests PASS**
- **데모**: α, ε, dedup, ζ, η, θ, ι 전부 라이브 통과
- **ADR**: 11건 (001~005 + 007 + 009~012, 006 superseded) Accepted
- **Blog**: Ep.1~Ep.5 완성 (Ep.5 = Chunking 라이브 데모)
- **LOC**: Go ~3,800 + 문서 ~4,000 + scripts ~700

## 업데이트 규칙

- 새 P0/P1/P2/P3 추가: 번호 이어서 부여 (재활용 금지)
- 항목 완료: ✅ + 완료일 표기 후 "완료된 주요 작업 기록" 테이블에 1줄 이관
- 우선순위 변경: P 라벨만 교체, 번호 유지
- 스펙 변경으로 작업 무의미해짐: `~~항목~~` 취소선 + 폐기 사유 한 줄
