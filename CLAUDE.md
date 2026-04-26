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
| `internal/` | 패키지 경계. **외부 import 금지** (Go 표준 `internal/` 규칙) |
| `internal/placement/` | Rendezvous Hashing (ADR-009, Season 2 Ep.1). chunk→DN 선택 |
| `internal/coordinator/` | fanout + quorum. `placement.Placer` 사용. Season 2+ 에 별도 daemon 분리 예정 |
| `internal/rebalance/` | Rebalance worker (ADR-010, Season 2 Ep.2). copy-then-update, full-success 시 trim |
| `internal/gc/` | Surplus chunk GC (ADR-012, Season 2 Ep.3). claimed-set + min-age 두 안전망 |
| `internal/chunker/` | Chunking (ADR-011, Season 2 Ep.4, ADR-006 supersede). Split/Join 고정 크기 |
| `internal/reedsolomon/` | Reed-Solomon EC (ADR-008, Season 2 Ep.5). GF(2^8) + 행렬 + Encode/Reconstruct, from-scratch |
| `internal/edge/` `StartAuto` | Auto-trigger (ADR-013, Season 3 Ep.1). time.Ticker 두 개, 같은 mutex 공유 |
| `internal/rebalance/` `planEC`/`migrateShard` | EC stripe rebalance (ADR-024, Season 3 Ep.2). set-based 최소 이동 |
| `internal/repair/` | EC repair queue (ADR-025, Season 3 Ep.3). K survivors → Reed-Solomon Reconstruct |
| `internal/store/snapshot.go` | Metadata snapshot + Stats (ADR-014, Season 3 Ep.4). bbolt `tx.WriteTo` hot snapshot, restore = offline |
| `internal/heartbeat/` | DN liveness monitor (ADR-030, Season 3 Ep.5). pull-based ticker, Probe interface, Healthy=consec_fails<threshold |
| `internal/store/scheduler.go` | Auto-snapshot scheduler (ADR-016, Season 3 Ep.6). EDGE_SNAPSHOT_DIR/INTERVAL/KEEP, atomic temp+rename, mtime-based prune |
| `internal/edge/replica.go` | Multi-edge HA — read-replica follower (ADR-022, Season 3 Ep.7). EDGE_ROLE/PRIMARY_URL/PULL_INTERVAL, snapshot-pull + atomic.Pointer hot-swap, write reject 503+X-KVFS-Primary |
| `internal/chunker/stream.go` | Streaming PUT/GET (ADR-017, Season 4 Ep.1). io.Reader 기반 chunker.Reader, edge handler 메모리 = chunkSize 와 무관 (object size 무관) |
| `internal/chunker/cdc.go` | Content-defined chunking (ADR-018, Season 4 Ep.2). FastCDC + GearTable, EDGE_CHUNK_MODE=cdc 로 활성, shift-invariant dedup |
| `internal/election/` | Auto leader election (ADR-031, Season 4 Ep.3). Raft-style 3-state machine, term + voting invariants, HTTP vote/heartbeat RPC. EDGE_PEERS opt-in |
| `scripts/` | 클러스터 lifecycle + 데모 (bash, curl, docker, python3만) |
| `docs/adr/` | 아키텍처 의사결정 기록 (불변) |
| `docs/FOLLOWUP.md` | 우선순위별 pending 작업 단일 소스 |
| `blog/` | 공개 블로그 시리즈 초안 — episode별 1파일 |

## 개발·테스트

| 명령 | 용도 |
|---|---|
| `make build` | `./bin/` 에 3개 바이너리 |
| `make test` | 단위 테스트 (CGO 없음) |
| `make test-race` | race 감지 (CGO 필요 — 로컬 Go 있을 때만) |
| `make fmt` | `gofmt -w .` |
| `make lint` | `go vet ./...` |
| `./scripts/up.sh` | Docker 클러스터 기동 (compose 플러그인 불필요) |
| `./scripts/demo-alpha.sh` | 3-way replication 데모 |
| `./scripts/demo-epsilon.sh` | UrlKey 데모 |
| `./scripts/demo-zeta.sh` | 4-DN 추가 후 placement 분산 데모 (Season 2 Ep.1) |
| `./scripts/demo-eta.sh` | Rebalance worker 라이브 데모 (Season 2 Ep.2) |
| `./scripts/demo-theta.sh` | Surplus chunk GC 라이브 데모 (Season 2 Ep.3) |
| `./scripts/demo-iota.sh` | Chunking 라이브 데모 (Season 2 Ep.4) |
| `./scripts/demo-kappa.sh` | Reed-Solomon EC 라이브 데모 (Season 2 Ep.5) |
| `./scripts/demo-lambda.sh` | Auto-trigger 라이브 데모 (Season 3 Ep.1) |
| `./scripts/demo-mu.sh` | EC stripe rebalance 라이브 데모 (Season 3 Ep.2) |
| `./scripts/demo-nu.sh` | EC repair 라이브 데모 (Season 3 Ep.3) |
| `./scripts/demo-xi.sh` | Meta backup + offline restore 라이브 데모 (Season 3 Ep.4) |
| `./scripts/demo-omicron.sh` | DN heartbeat 라이브 데모 (Season 3 Ep.5) |
| `./scripts/demo-pi.sh` | Auto-snapshot scheduler 라이브 데모 (Season 3 Ep.6) |
| `./scripts/demo-rho.sh` | Multi-edge HA (read-replica) 라이브 데모 (Season 3 Ep.7, Season 3 close) |
| `./scripts/demo-sigma.sh` | Streaming PUT/GET 라이브 데모 (Season 4 Ep.1) — 64 MiB / edge mem 22 MiB |
| `./scripts/demo-tau.sh` | CDC chunking dedup 라이브 데모 (Season 4 Ep.2) — fixed 0% vs CDC 40% |
| `./scripts/demo-upsilon.sh` | Auto leader election 라이브 데모 (Season 4 Ep.3) — 3 edge, kill leader, ~4s 후 새 leader 자동 등극 |
| `./scripts/chaos-dn-killer.sh` | 주기적 random DN kill + GET 검증 (회귀 catch) |
| `./scripts/down.sh` | 정리 (dn1~dn8 + edge 포함) |

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
- 새 ADR 작성 시 `docs/adr/README.md` 표에 한 줄 추가
- 블로그 episode 완성 시 `blog/` 에 커밋 전 실 cluster 로 데모 재현
- 새 시즌 진입 시 README 의 시즌 표 갱신
- `internal/coordinator/` 독립 daemon 분리 시 ADR-002 supersede

## 금지·주의

- 🚫 `EDGE_SKIP_AUTH=1` 프로덕션 배포 (README·코드에 경고 있음)
- 🚫 `EDGE_URLKEY_SECRET` 하드코딩 repo 커밋 (현재 `demo-secret-change-me-in-production` 은 demo 전용임을 이름으로 명시)
- ⚠️ `chunk_id` 는 **content-addressable** (sha256) — 변경하면 ADR-005 supersede
