# kvfs 아키텍처

> Phase 1~6 브레인스토밍 결과의 요약. 의사결정 추적 용도.

## 한 장 요약

```
 Client ──HTTP + UrlKey──▶ kvfs-edge ──HTTP REST──▶ kvfs-dn × 3
                              │                         │
                              ▼                         ▼
                         bbolt (meta)              local fs
                         edge-data/edge.db         chunks/<id[:2]>/<id[2:]>
```

- **2 daemon**: `kvfs-edge` (게이트웨이 + 인라인 코디네이터) / `kvfs-dn` (스토리지 노드, ×3)
- **1 object = 1 chunk** (MVP): 객체 전체 = sha256 content-addressable 청크 1개
- **Quorum write**: 3 DN 에 병렬 PUT, 2-of-3 ack 시 성공
- **Read**: replicas 중 성공하는 첫 DN에서 제공

## 의사결정 이력

| Phase | 결정 | 대안 | 이유 |
|---|---|---|---|
| 1 | 공개 오픈소스 데모 | 개인 NAS, CI 저장소, 학술 | 학습성 · 재현성 우선 |
| 2 | α(3-way 복제) + ε(UrlKey) 얼굴 | erasure coding, tiering, POSIX | 시각 임팩트 + 난이도 중 |
| 3 | (생략) α/ε가 얼굴 자산 확정 | — | Phase 2에서 자동 결정 |
| 4 | 2-daemon (edge + dn×3) | 3-daemon, 5-daemon | 복잡도 최소화 |
| 5 | HTTP REST (client + 내부 모두) | gRPC, raw TCP binary | curl·tcpdump 친화 |
| 6 | raw files + bbolt + sha256 CA | SQLite, RocksDB, UUID | ls로 디버깅 가능 |

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
cmd/kvfs-edge/main.go       엔트리포인트
cmd/kvfs-dn/main.go          엔트리포인트
cmd/kvfs-cli/main.go         sign / inspect CLI

internal/urlkey/             HMAC-SHA256 signer · verifier
internal/store/              bbolt 래퍼 (ObjectMeta / DNInfo)
internal/dn/                 DN HTTP 서버 · 디스크 저장
internal/coordinator/        fanout · quorum · 재시도 (현재는 edge 안 library)
internal/edge/               edge HTTP 서버 · 핸들러 · 인증 미들웨어
```

## Season 1 (MVP) 범위

**포함**:
- 단일 object = 단일 chunk (최대 ~64MB 권장)
- 3-way replication + 2-of-3 quorum
- HMAC-SHA256 URL 서명 · 만료
- bbolt 재시작 후 복원
- Docker Compose 4-node 테스트 환경
- α · ε 데모 스크립트
- Go 1.26 · CGO 없음

**배제** (Season 2+):
- 청크 분할 (> 64MB 객체)
- Reed-Solomon 이레이저 코딩 (KEEP-03)
- Hot/Cold tier 자동 이동 (KEEP-01, KEEP-06)
- Consistent hashing · 동적 rebalance (INHERIT-05)
- Coordinator daemon 분리 · Raft HA (INHERIT-01)
- gRPC 마이그레이션 (INHERIT-02)
- NFS/SMB 확장

## 시험 가능한 가설 (블로그 글감)

- **가설 1**: 3-way replication 은 복잡하지 않다 → `coordinator.WriteChunk` 60줄로 시연
- **가설 2**: HMAC URL 서명은 2페이지면 충분 → `internal/urlkey` 100줄 이내
- **가설 3**: Content-addressable chunk 는 dedup 을 "무료"로 준다 → 같은 body 두 번 PUT 시 DN 디스크 사용량 1배
- **가설 4**: bbolt 로 충분하다 (SQLite 필요 없다) → MVP 스코프에서 bbolt KV 패턴이 자연스러움
- **가설 5**: 5분 `docker compose up` 데모는 실제로 5분 안에 동작한다 → CI로 시간 측정

## 참고

- 기존 reference 매핑: `NAMING.md`
- Apache 2.0 라이선스
