# kvfs — Key-Value File System

> **분산 object storage 설계 원리를 살아있는 데모로** 보여주는 오픈소스 레퍼런스.
> **Go 1.26 · Apache 2.0 · Season 2 진행 중 · 주말 프로젝트**

## 이것은 무엇인가

kvfs는 **분산 object storage의 핵심 원리**를 작고 이해 가능한 코드로 구현합니다. 각 원리마다 **시즌별 Episode** 로 추가:

### Season 1 (MVP 완료 ✅)
1. **3-way replication + quorum 쓰기** — 1 노드 죽어도 데이터 지속 (ADR-002)
2. **URL 서명 (presigned URL)** — 오리진 트래픽 없이 엣지에서 권한 검증 (ADR-007)
3. **Content-addressable chunk** — sha256 기반 자동 dedup + 무료 무결성 (ADR-005)

### Season 2 (진행 중)
4. **Consistent hashing (Rendezvous / HRW)** ← **Ep.1 완료** — 노드 추가·제거 시 최소 이동 (ADR-009)
5. Rebalance worker · chunking · Reed-Solomon EC — 예정

이것이 Ceph·MinIO·S3가 하는 일의 **단순화된 핵심**입니다. 목표는 production이 아니라 **이해 가능한 레퍼런스**.

## 5분 데모

```bash
git clone https://github.com/HardcoreMonk/kvfs
cd kvfs

# 1. 전체 기동 (edge × 1 + dn × 3)
./scripts/up.sh

# 2. α 데모: 3-way replication durability
./scripts/demo-alpha.sh
#    → PUT object → 3개 DN에 청크 분산 확인
#    → DN-1 강제 종료 → GET 여전히 성공
#    → "✅ 3-way replication verified"

# 3. ε 데모: UrlKey presigned URL
./scripts/demo-epsilon.sh
#    → CLI로 서명 URL 생성
#    → 해당 URL로 PUT 성공
#    → 만료 후 401 거부
#    → "✅ UrlKey presigned URL verified"

# 4. placement-sim: Consistent hashing 동작 눈으로 확인 (Season 2)
docker run --rm -v "$PWD:/src" -w /src golang:1.26-alpine \
  go run ./cmd/kvfs-cli placement-sim --nodes 10 --chunks 10000 --remove 1
#    → 노드 1개 제거 시 약 30% 만 이동 (이론 R/N 일치) 출력
```

## 아키텍처 (2 daemon)

```
   Client ─HTTP+UrlKey─► kvfs-edge ─HTTP REST─► kvfs-dn (×3)
                            │                      │
                            │                      └─ 로컬 파일 저장
                            │                         chunks/<sha256[0:2]>/<rest>
                            │
                            └─ bbolt meta DB
                               object_key → [chunk_id, replicas]
```

상세: [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)

## 기원

kvfs는 **과거 운영 사례에서 한 분산 파일 시스템의 사전 연구**에서 파생되었습니다. 기존 reference의 설계 자산 중 **2026년에도 현역인 9건 + 아이디어 계승 8건**을 선별해 새 코드베이스로 구현 중.

기존 reference 매핑: [`NAMING.md`](NAMING.md)

## 설계 결정 (ADR 전문)

각 결정은 [`docs/adr/`](docs/adr/) 의 독립 문서로 박힘 — 불변 기록.

### Season 1 (Accepted)

| ADR | 주제 | 핵심 |
|---|---|---|
| [001](docs/adr/ADR-001-independent-identity.md) | 독자적 프로젝트 identity | 기존 reference 브랜드 제거, `NAMING.md` 매핑 유지 |
| [002](docs/adr/ADR-002-two-daemon-architecture.md) | 2-daemon MVP | edge(인라인 coordinator) + dn × 3 |
| [003](docs/adr/ADR-003-http-rest-protocol.md) | HTTP REST 통신 | curl·tcpdump 디버그 가능 |
| [004](docs/adr/ADR-004-bbolt-metadata.md) | bbolt 메타 | pure Go, 트랜잭션, 외부 의존 1개 |
| [005](docs/adr/ADR-005-content-addressable-chunks.md) | sha256 content-addressable | dedup + integrity 무료 |
| [006](docs/adr/ADR-006-mvp-single-chunk.md) | 1 object = 1 chunk | chunking은 Season 2 |
| [007](docs/adr/ADR-007-urlkey-hmac-sha256.md) | UrlKey HMAC-SHA256 | Go stdlib only, timing-safe |

### Season 2 (Accepted)

| ADR | 주제 | 핵심 |
|---|---|---|
| [009](docs/adr/ADR-009-consistent-hashing.md) | Rendezvous Hashing | `internal/placement/`. 이동률 R/N (실측 일치) |

### Season 2+ 예정 (docs/FOLLOWUP.md)

- ADR-010 Rebalance worker · ADR-011 Chunking · ADR-008 Reed-Solomon EC
- ADR-012 Coordinator daemon 분리 · ADR-013 gRPC 이행 결정

## 로드맵

```
Season 1 (MVP 완료 ✅)  α + ε 데모 · 7 ADR · 33 파일 · 1,367 LOC
Season 2 (진행 중 ▶)  ADR-009 ✅ → 010 → 011 → 008 → 012 → ...
Season 3 (미정)       gRPC · Coordinator daemon · Raft · multi-region
Season 4 (장기)       NFS/SMB (기존 reference의 legacy NFS gateway 경로 참조)
```

## 다음 작업

[`docs/FOLLOWUP.md`](docs/FOLLOWUP.md) — 우선순위별 pending 작업 단일 소스.

## 기여

Apache 2.0. PR 환영. **교육적 가치**를 최상위 기준으로 코드 리뷰합니다.

## 블로그

- [`blog/01-hello-kvfs.md`](blog/01-hello-kvfs.md) — 프로젝트 소개 + α 데모 walkthrough
