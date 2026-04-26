# kvfs 후속 작업 (Follow-up)

`200.kvfs/` 의 **후속 작업 단일 소스**. 상태 업데이트는 이 파일만 수정한다.

문서 현행화 일자: **2026-04-26** · Season 3 Ep.1 (ADR-013 Auto-trigger) 완료 시점 기준.

## 우선순위 맵

- **P0**: 차단·긴급. 즉시 처리 — 현재 **0건**
- **P1**: 명확한 스펙 존재, 실행 대기 — 현재 **2건**
- **P2**: 리뷰·개선 권고, 개인 프로젝트 여유 시 처리 — 현재 **0건** (P2-01~09 모두 완료)
- **P3**: 별도 스펙·사용자 결정 필요 — 현재 **3건** (P3-05/06 obsolete · 취소선; P3-07 ADR-024 완료 → P3-09 신규)

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

### ~~[P2-01] LICENSE 헤더~~
- **DONE 2026-04-26** (`6c18c51`): `scripts/add-license-headers.sh` (idempotent + trailing-newline 보존), 23 .go SPDX 헤더, `make license`

### ~~[P2-02] CONTRIBUTING + CODE_OF_CONDUCT~~
- **DONE 2026-04-26** (`6c18c51` + 후속): `CONTRIBUTING.md` (개발 흐름·ADR 규칙·커밋 컨벤션), `CODE_OF_CONDUCT.md` (Contributor Covenant 2.1 by reference)

### ~~[P2-03] GitHub Actions CI~~
- **DONE 2026-04-26** (`6c18c51`): `.github/workflows/ci.yml` 3 jobs (build/vet/test on Go 1.26 + staticcheck + govulncheck). /simplify 가 잡은 1.25 vs go.mod 불일치 fix 동반

### ~~[P2-04] placement-sim bar chart edge case~~
- **DONE 2026-04-26**: cmdPlacementSim 패키지 주석 + R=N 케이스 런타임 ℹ️  notice + barChart 함수 주석 추가

### ~~[P2-05] edge 동작 중 EDGE_DNS 변경 지원~~
- **DONE 2026-04-26 (ADR-027)**: `Coordinator.UpdateNodes` (RWMutex), `dns_runtime` bbolt 버킷 영속, `POST /v1/admin/dns` + `DELETE /v1/admin/dns?addr=`, `kvfs-cli dns list/add/remove`. 라이브 검증: dn4 add → edge restart → bbolt 영속 유지 → remove 즉시 적용. `EDGE_DNS_RESET=1` env 가 disaster recovery

### ~~[P2-06] Benchmark suite~~
- **DONE 2026-04-26**: 4 패키지 `*_bench_test.go` 신규
  - placement: `BenchmarkPick` (N=3..1000) — N=10 ~1.4 µs, N=1000 ~178 µs (O(N) 그대로)
  - urlkey: Sign 737 ns, BuildURL 859 ns, Verify 1053 ns
  - reedsolomon: (4+2) Encode/Reconstruct ~515 MB/s, (10+4) ~264 MB/s, gfMul 0.44 ns/op
  - chunker: Split ~2350 MB/s (sha256 한계)
- ADR-008 / blog Ep.6 의 "~50 MB/s" 추정치를 실측 기준으로 갱신 (실제 ~10배 빠름, log/exp 테이블 L1 cache-friendly 덕)

### ~~[P2-07] Chaos test — random DN kill~~
- **DONE 2026-04-26**: `scripts/chaos-dn-killer.sh` (--duration/--interval/--downtime/--get-rate/--dns flags). quorum (R/2+1) 보장 위해 alive > 2 일 때만 kill, 죽은 DN 자동 restore. 라이브 검증: 30s/3 kill cycle, 69 GETs 0 fail PASS

### ~~[P2-08] Secret rotation (UrlKey `kid`)~~
- **DONE 2026-04-26 (ADR-028)**: multi-key `urlkey.Signer` (Add/Remove/SetPrimary/Kids), `urlkey_secrets` bbolt 버킷 영속, URL `?kid=` 파라미터, kid 누락 시 try-all-kids fallback (legacy/shell 클라이언트 호환). `POST /v1/admin/urlkey/rotate` + `DELETE /v1/admin/urlkey?kid=`. `kvfs-cli urlkey list/rotate/remove --kid X`. 라이브 검증: rotate 후 옛 v1-signed URL 정상 verify, v1 remove 후 401. 11 unit tests PASS (5 기존 + 6 rotation)

### ~~[P2-09] TLS / mTLS~~
- **DONE 2026-04-26 (ADR-029)**: env-driven opt-in TLS. `EDGE_TLS_CERT/KEY` (HTTPS server), `EDGE_DN_TLS_CA + CLIENT_CERT/KEY` (edge → DN mTLS), `DN_TLS_CERT/KEY + CLIENT_CA` (DN HTTPS + mTLS). Coordinator 에 DNScheme + TLSConfig 추가, URL build 시 scheme 적용. `scripts/gen-tls-certs.sh` (self-signed CA + edge/dn/client certs), `scripts/up-tls.sh` (TLS 클러스터 데모). 라이브: healthz with CA → 200, without CA → curl exit 60 (verify fail), TLS PUT/GET round-trip 일치, edge→DN mTLS 로그 확인. 11 기존 데모는 plain HTTP 그대로 (TLS env 없으면 평문)

---

## P3 — 사용자 결정·별도 스펙 필요

### [P3-02] 관찰성 스택 선택
- **현황**: ADR-013 으로 `slog` 구조화 로그 + `/v1/admin/auto/status` JSON endpoint 가 부분 도입됨. 클러스터-wide metrics 는 미정
- **옵션 A**: 기존 slog + `/metrics` 엔드포인트 추가 (Prometheus-exposition 포맷 직접 작성)
- **옵션 B**: OpenTelemetry SDK 풀 통합 (traces · metrics · logs)
- **옵션 C**: 외부 stack 없이 현재 구조 유지 (최소 변경)
- **개인 프로젝트 고려**: A 가 ADR-013 의 slog 흐름과 자연 연결. B 는 별도 Season 3 episode 감

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

### ~~[P3-07] Season 3 Ep.2 주제 결정~~
- **OBSOLETE (2026-04-26)**: ADR-024 (EC stripe rebalance) 채택 + 구현 완료. 다음 ep 결정은 신규 [P3-09]

### ~~[P3-09] Season 3 Ep.3 주제 결정~~
- **OBSOLETE (2026-04-26)**: ADR-025 EC repair queue 채택 + 구현 완료. 다음 ep 결정은 신규 [P3-10]

### [P3-10] Season 3 Ep.4 주제 결정
- **현**: Ep.3 ADR-025 완료. 운영성 트랙의 다음 ep 미정
- **후보**:
  - **ADR-022 — Multi-edge leader election**: ADR-013/024/025/027/028 모두 single-edge 가정. multi-edge 시 동기화 필요. 큰 작업
  - **ADR-014 — Meta backup/HA**: bbolt 메타 손상 시 안전망. 단순한 snapshot/replay
  - **ADR-030 — DN heartbeat**: registry-removal 외 자동 dead 감지
  - **ADR-017 — Streaming PUT/GET**: io.ReadAll 기반 → io.Reader 진짜 streaming
  - **ADR-018 — Content-defined chunking**: rabin/buzhash 비정렬 dedup
- **추천 순서**: 014 (메타 안전성, 작은 ADR) → 022 (HA 본격, 큰 작업) → 030 (heartbeat) → 017/018 (성능/효율)
- **결정 시 P1 로 승격**

---

## 완료된 주요 작업 기록

참고용. 상세는 각 commit · ADR 참조.

| 완료일 | 작업 | 결과 |
|---|---|---|
| 2026-04-25 | 독자 프로젝트 identity 출범 | clean-slate 정의 · ADR-001 |
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
| 2026-04-26 | P1-01 GitHub 발행 + push | `https://github.com/HardcoreMonk/kvfs` private, 12 commits push (`076ba27..main`) |
| 2026-04-26 | P2-03 CI workflow + P2-01 LICENSE 헤더 + P2-02 CONTRIBUTING (부분) | `.github/workflows/ci.yml` (3 jobs: test/staticcheck/govulncheck, Go 1.26 fix), `scripts/add-license-headers.sh` (idempotent + trailing-newline 보존), 23 .go 파일 SPDX 헤더, `Makefile` `license`/`bench` targets, `CONTRIBUTING.md`. /simplify 후 trailing newline 보존 fix 동반 |
| 2026-04-26 | Season 3 Ep.2 — ADR-024 EC stripe rebalance 구현 완료 | `internal/rebalance/` 확장 (planEC + migrateShard, set-based 최소 이동), Migration 에 `Kind`/`StripeIndex`/`ShardIndex`/`OldAddr`/`NewAddr` 추가, `Coordinator.PlaceN` + `ObjectStore.UpdateShardReplicas` 인터페이스 확장, CLI `[shard]` 렌더링. EC 전용 7 tests PASS (총 73). `scripts/demo-mu.sh` 라이브: 6 DN + dn7 추가 → 정확히 stripe 당 1 migration → GET sha256 일치 + 멱등 + `blog/08-ec-rebalance.md`. ADR-013 auto-trigger 와 자동 통합 |
| 2026-04-26 | P2-02 마무리 (CODE_OF_CONDUCT) + P2-06 Benchmark suite | `CODE_OF_CONDUCT.md` (Contributor Covenant 2.1 by reference, content filter 회피 위해 본문 link). 4 패키지 bench: placement Pick O(N), urlkey Sign/Verify ~µs, RS Encode (4+2) 515 MB/s · (10+4) 264 MB/s, chunker Split 2350 MB/s. ADR-008/blog Ep.6 의 ~50 MB/s 추정치를 실측으로 정정 |

---

## 현재 상태 요약 (2026-04-26)

- **Git**: main branch, 15+ commits, **GitHub `HardcoreMonk/kvfs` private** (latest pushed)
- **클러스터**: `localhost:8000` running · edge + dn × 7 (demo-mu 가 down→up→6DN→dn7 까지 끝낸 상태)
- **테스트**: 7 placement + 5 urlkey + 15 rebalance (8 chunk + 7 EC) + 9 gc + 13 chunker + 24 reedsolomon = **73 unit tests PASS**, `go vet` 클린
- **데모**: α, ε, dedup, ζ, η, θ, ι, κ, λ, μ 전부 라이브 통과 (11종)
- **ADR**: 14건 (001~005 + 007~013 + 024, 006 superseded by 011) Accepted
- **Blog**: Ep.1~Ep.8 완성 (Ep.8 = EC stripe rebalance)
- **LOC**: Go ~5,500 + 문서 ~6,000 + scripts ~1,200
- **CI**: GitHub Actions (build/vet/test + staticcheck + govulncheck) — public 전환 [P3-04] 대기
- **Season 2 closed**; **Season 3 Ep.2 완료** (auto-trigger + EC rebalance 결합), Ep.3 주제 미정 ([P3-09])

## 업데이트 규칙

- 새 P0/P1/P2/P3 추가: 번호 이어서 부여 (재활용 금지)
- 항목 완료: ✅ + 완료일 표기 후 "완료된 주요 작업 기록" 테이블에 1줄 이관
- 우선순위 변경: P 라벨만 교체, 번호 유지
- 스펙 변경으로 작업 무의미해짐: `~~항목~~` 취소선 + 폐기 사유 한 줄
