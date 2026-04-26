# kvfs 아키텍처

> Season 1 MVP 설계 + Season 2 진행 변경사항. 결정 근거는 `docs/adr/`.

## 한 장 요약

```
 Client ──HTTP + UrlKey──▶ kvfs-edge ──HTTP REST──▶ kvfs-dn × N
                              │  │                      │
                              │  └─ placement.Placer     │
                              │     (Rendezvous Hashing) │
                              │     ADR-009 (Season 2)   │
                              ▼                          ▼
                         bbolt (meta)              local fs
                         edge-data/edge.db         chunks/<id[:2]>/<id[2:]>
```

- **2 daemon**: `kvfs-edge` (게이트웨이 + 인라인 coordinator + placement) / `kvfs-dn` (스토리지 노드, ×N)
- **1 object = 1 chunk** (MVP): 객체 전체 = sha256 content-addressable 청크 1개
- **Placement**: chunkID → top-R DN 선택 (HRW score). DN 추가·제거 시 약 R/N 만 이동
- **Quorum write**: 선택된 R DN 에 병렬 PUT, `R/2+1` ack 시 성공 (3 DN → 2-of-3)
- **Read**: 메타에 저장된 replicas 순회, 성공하는 첫 DN 에서 제공

## 의사결정 이력

Season 1 브레인스토밍 Phase 1~6 + Season 2 Ep.1 기준. 각 행은 대응 ADR 로 연결.

| 시점 | 결정 | 대안 | 이유 | ADR |
|---|---|---|---|---|
| S1 Phase 1 | 공개 오픈소스 데모 | 개인 NAS, CI 저장소, 학술 | 학습성 · 재현성 우선 | [001](adr/ADR-001-independent-identity.md) |
| S1 Phase 2 | α(3-way 복제) + ε(UrlKey) 얼굴 | erasure coding, tiering, POSIX | 시각 임팩트 + 난이도 중 | — (Season 2 주제로) |
| S1 Phase 3 | (생략) α/ε가 얼굴 자산 확정 | — | Phase 2에서 자동 결정 | — |
| S1 Phase 4 | 2-daemon (edge + dn×N) | 3-daemon, 5-daemon | 복잡도 최소화 | [002](adr/ADR-002-two-daemon-architecture.md) |
| S1 Phase 5 | HTTP REST (client + 내부 모두) | gRPC, raw TCP binary | curl·tcpdump 친화 | [003](adr/ADR-003-http-rest-protocol.md) |
| S1 Phase 6 | raw files + bbolt + sha256 CA | SQLite, RocksDB, UUID | ls로 디버깅 가능 | [004](adr/ADR-004-bbolt-metadata.md), [005](adr/ADR-005-content-addressable-chunks.md), [006](adr/ADR-006-mvp-single-chunk.md) |
| S1 ε | HMAC-SHA256 UrlKey | S3 SigV4, JWT, APR1-MD5 + Base62 | stdlib only, timing-safe | [007](adr/ADR-007-urlkey-hmac-sha256.md) |
| **S2 Ep.1** | **Rendezvous Hashing (HRW)** | 고전 ring, 중앙 placement DB | **코드 ~20줄, 이동률 R/N 보장** | **[009](adr/ADR-009-consistent-hashing.md)** |

## 컴포넌트

### kvfs-edge (게이트웨이)

- **역할**: HTTP API · UrlKey 검증 · 코디네이터(인라인) · 메타 저장
- **포트**: `:8000`
- **외부 의존**: DN 3개 (HTTP)
- **상태 저장**: `edge-data/edge.db` (bbolt)
- **엔드포인트**:
  - `PUT /v1/o/{bucket}/{key...}?sig=...&exp=...`
  - `GET /v1/o/{bucket}/{key...}?sig=...&exp=...`
  - `DELETE /v1/o/{bucket}/{key...}?sig=...&exp=...`
  - `GET /v1/admin/objects` (디버그 · 인증 없음)
  - `GET /v1/admin/dns`
  - `GET /healthz`

### kvfs-dn (스토리지 노드)

- **역할**: 청크 저장·조회
- **포트**: `:8080` (컨테이너 내부)
- **외부 의존**: 없음
- **상태 저장**: `/var/lib/kvfs-dn/chunks/<id[:2]>/<id[2:]>`
- **엔드포인트**:
  - `PUT /chunk/{id}` (id = hex(sha256(body)), 서버가 검증)
  - `GET /chunk/{id}`
  - `DELETE /chunk/{id}`
  - `GET /healthz`

## 주요 데이터 흐름

### PUT (upload)

```
1. Client → edge: PUT /v1/o/b/k?sig=...&exp=...   [body: bytes]
2. edge: UrlKey 검증 (HMAC-SHA256, 만료 체크)
3. edge: chunk_id = hex(sha256(body))
4. edge: 병렬로 3 DN 에 PUT /chunk/<chunk_id>
5. 2-of-3 ack 수집 → quorum 충족
6. edge: bbolt 에 (b, k) → (chunk_id, [dn1,dn2,dn3], v1) 저장
7. edge → Client: 201 Created + {chunk_id, replicas, version}
```

### GET (download)

```
1. Client → edge: GET /v1/o/b/k?sig=...&exp=...
2. edge: UrlKey 검증
3. edge: bbolt 에서 (b, k) → chunk_id, replicas 조회
4. edge: replicas[0]에 GET /chunk/<chunk_id>
5. 성공 → edge: sha256 재검증 → Client에 전달
   실패 → replicas[1], replicas[2] 순차 시도
6. edge → Client: 200 + body (+ X-KVFS-Served-By, X-KVFS-Chunk-ID 헤더)
```

### 장애 시나리오: dn1 죽음 후 GET

```
1. edge → dn1: GET /chunk/<id>   → connection refused
2. edge → dn2: GET /chunk/<id>   → 200 OK
3. edge → Client: 200 + body     (X-KVFS-Served-By: dn2:8080)
```

## 패키지 구조

```
cmd/kvfs-edge/main.go       edge 엔트리포인트
cmd/kvfs-dn/main.go          dn 엔트리포인트
cmd/kvfs-cli/main.go         sign / inspect / placement-sim CLI

internal/urlkey/             HMAC-SHA256 signer · verifier
internal/store/              bbolt 래퍼 (ObjectMeta / DNInfo)
internal/dn/                 DN HTTP 서버 · 디스크 저장
internal/placement/          Rendezvous Hashing — chunk→DN 선택 (Season 2 Ep.1)
internal/coordinator/        fanout · quorum · 재시도 (placement 사용, edge 안 library)
internal/edge/               edge HTTP 서버 · 핸들러 · 인증 미들웨어
```

## Season 1 (MVP) 범위 — 완료 ✅

- 단일 object = 단일 chunk (최대 ~64MB 권장)
- 3-way replication + 2-of-3 quorum
- HMAC-SHA256 URL 서명 · 만료
- bbolt 재시작 후 복원
- Docker 4-node 테스트 환경 (`./scripts/up.sh`)
- α · ε 데모 스크립트
- Go 1.26 · CGO 없음 · 7 ADR
- 33 파일 · 1,367 LOC

## Season 2 (진행 중) ▶

**Ep.1 — Consistent Hashing (완료 ✅)**:
- `internal/placement` — Rendezvous Hashing (HRW), ~220 LOC (주석 100+ 줄)
- 7 tests: 결정성 · 균등분포 · 변동률 (실측 R/N 이론값 일치)
- Coordinator 통합 — N DN 에서 top-R 선택
- `kvfs-cli placement-sim` — ASCII bar chart 로 분포·이동률 시각화
- ADR-009

**Ep.2+ 예정** — `docs/FOLLOWUP.md` 참조:
- Rebalance worker (ADR-010)
- Chunking (ADR-011)
- Reed-Solomon EC (ADR-008)
- Coordinator daemon 분리 + Raft HA (ADR-012)
- gRPC 마이그레이션 결정 (ADR-013)

## 배제 (Season 3+ 또는 판단 유보)

- Hot/Cold tier 자동 이동
- NFS/SMB 게이트웨이 확장
- Multi-region active-active

## 시험 가능한 가설 (블로그 글감)

| # | 가설 | 검증 |
|---|---|---|
| 1 | 3-way replication 은 복잡하지 않다 | `coordinator.WriteChunk` 60줄 — Ep.1 블로그 증명 ✅ |
| 2 | HMAC URL 서명은 2페이지면 충분 | `internal/urlkey` 90 LOC — ε 데모 통과 ✅ |
| 3 | Content-addressable chunk 는 dedup 을 "무료"로 준다 | α 후속 실험 — 4 objects → 3 chunks 실측 확인 ✅ |
| 4 | bbolt 로 충분하다 (SQLite 필요 없다) | MVP 스코프에서 bbolt KV 패턴이 자연스러움 — 동작 확인 ✅ |
| 5 | 5분 `./scripts/up.sh` 데모는 실제로 5분 안에 동작 | 로컬 빌드+기동 ~3분 관찰 — CI 측정 미완 ⏳ |
| 6 | **Rendezvous Hashing 은 "consistent"를 수학적으로 지킨다** | 실측 R/N ≈ 이론값 확인 (27.4% ≈ 27.3%, 29.1% ≈ 30%) ✅ |

## 참고

- 후속 작업 단일 소스: [`FOLLOWUP.md`](FOLLOWUP.md)
- ADR 전체: [`adr/`](adr/)
- Apache 2.0 라이선스
