@../CLAUDE.md

# kvfs 프로젝트 작업 규약 (L4)

claude-zone L3 체인 상속. 여기는 **kvfs 고유**만.

## 프로젝트 정체성

- **이름**: kvfs — Key-Value File System (공개 오픈소스 데모)
- **출범**: 2026-04-25, clean-slate 독립 프로젝트로 출발. 상세: `docs/adr/ADR-001-independent-identity.md`
- **라이선스**: Apache 2.0
- **목적**: 분산 object storage 설계 원리를 **살아있는 데모**로 시연. 프로덕션 아님
- **성공 기준**: 학습성·재현성·명료함 (TPS·SLA 아님)

## 언어·런타임

- **Go 1.26** — 존 L3 최소(1.22+)를 넘어 최신 stable
- **CGO 없음** — `CGO_ENABLED=0`으로 정적 빌드 (Alpine·Distroless 친화)
- **외부 의존 최소** — 현재 `go.etcd.io/bbolt` 1개만. stdlib 우선

## 아키텍처 핵심

2-daemon MVP (상세: `docs/ARCHITECTURE.md`):

```
Client ──HTTP+UrlKey──▶ kvfs-edge ──HTTP REST──▶ kvfs-dn × 3
                           │                        │
                       bbolt meta               local files
```

- **kvfs-edge** (:8000): 게이트웨이 + 인라인 코디네이터
- **kvfs-dn** (:8080, ×3): 로컬 파일 chunk 저장

## 디렉토리 의미

| 경로 | 의미 |
|---|---|
| `cmd/` | 3개 바이너리 엔트리포인트 (edge, dn, cli) |
| `internal/` | 패키지 경계. **외부 import 금지** (Go 표준 `internal/` 규칙). 패키지별 ADR / 시즌-에피소드 매핑은 `docs/adr/README.md` 단일 소스 |
| `scripts/` | 클러스터 lifecycle + 데모 (bash, curl, docker, python3만) |
| `docs/adr/` | 아키텍처 의사결정 기록 (불변). 시즌·에피소드·연결 코드 모두 여기 |
| `docs/FOLLOWUP.md` | 우선순위별 pending 작업 단일 소스 |
| `blog/` | 공개 블로그 시리즈 초안 — episode별 1파일 |

> 패키지 단위 책임·ADR linkage 는 ADR README 표를 그대로 참조. CLAUDE.md 에 또 적지 않음 (drift 방지).

## 개발·테스트

| 명령 | 용도 |
|---|---|
| `make build` | `./bin/` 에 3개 바이너리 |
| `make test` | 단위 테스트 (CGO 없음) |
| `make test-race` | race 감지 (CGO 필요 — 로컬 Go 있을 때만) |
| `make fmt` / `make lint` | `gofmt -w .` / `go vet ./...` |
| `./scripts/up.sh` / `down.sh` | Docker 클러스터 기동·정리 (compose 플러그인 불필요) |
| `./scripts/demo-*.sh` | 시즌별 라이브 데모. greek letter 순으로 정렬 (alpha → omega 진행). episode ↔ 데모 ↔ ADR 매핑은 `docs/adr/README.md` |
| `./scripts/chaos-dn-killer.sh` | 주기적 random DN kill + GET 검증 (회귀 catch). 외부 cluster 가정 |
| `./scripts/chaos-coord-flap.sh` | 자체 3-coord HA → 주기적 coord kill+restart → PUT/GET hammer → 모든 200-PUT 검증 (ADR-038/039/040 invariant) |
| `./scripts/chaos-coord-quorum-loss.sh` | 자체 cluster → 2/3 coord kill → PUT 모두 503 검증 + bbolt drift 0 + post-restore phantom 0 (ADR-040 strict) |
| `./scripts/chaos-suite.sh [--quick] [--skip <name>]` | 위 chaos-* 일괄 실행 + 집계 |

Docker로 빌드 검증 (로컬 Go 없어도):

```bash
docker run --rm -v "$PWD:/src" -w /src golang:1.26-alpine \
  sh -c 'go build ./... && go test ./...'
```

## 코드 컨벤션

- **에러**: `fmt.Errorf("%s: %w", ctx, err)` — 래핑 체인. `errors.Is`·`errors.As`로 검사
- **로깅**: `log/slog` 구조화 (`slog.NewTextHandler`). `fmt.Printf`는 hot path에 한정 (dn·edge 미들웨어)
- **타임아웃**: 모든 HTTP call에 `context.WithTimeout`. 외부 I/O에 명시적 deadline
- **테스트**: `_test.go` 패키지 내부. mock server는 `httptest`, 외부 network 금지
- **Channels·goroutine**: `sync.WaitGroup` 으로 수명 관리. `context.Context` 전파
- **파일 권한**: 0644 (chunk) / 0600 (bbolt). Alpine 이미지에서 kvfs(UID 10001) 운영

## 블로그 시리즈 규칙

- `blog/NN-title.md` 번호 연속 증가 (0-padding 2자리)
- 1편 = 1주제 = 60분 독서 이하
- 실 데모 출력 포함 (스크린샷 또는 copy-paste 블록)
- 코드 인용 시 주석 달아 의도 명시

## Claude Code 작업 힌트

- 환경변수 전체 표는 `README.md` § "환경 변수" — 신규 env 추가 시 README + main.go 도움말 동기화
- 새 ADR 작성 시 `docs/adr/README.md` 표에 한 줄 추가 (시즌별 표, **번호 오름차순** 으로 삽입)
- 블로그 episode 완성 시 `blog/` 에 커밋 전 실 cluster 로 데모 재현
- 새 시즌 진입 시 README 의 시즌 표 갱신

## 금지·주의

- 🚫 `EDGE_SKIP_AUTH=1` 프로덕션 배포 (README·코드에 경고 있음)
- 🚫 `EDGE_URLKEY_SECRET` 하드코딩 repo 커밋 (현재 `demo-secret-change-me-in-production` 은 demo 전용임을 이름으로 명시)
- ⚠️ `chunk_id` 는 **content-addressable** (sha256) — 변경하면 ADR-005 supersede
