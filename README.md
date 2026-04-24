# kvfs — Key-Value File System

> **분산 object storage 설계 원리를 살아있는 데모로** 보여주는 오픈소스 레퍼런스.
> **Go 1.26 · Apache 2.0 · ~3,000 LOC · 주말 프로젝트**

## 이것은 무엇인가

kvfs는 **분산 object storage의 핵심 원리 2가지**를 작고 이해 가능한 코드로 구현합니다:

1. **3-way replication + quorum 쓰기** — 1 노드 죽어도 데이터 지속
2. **URL 서명 (presigned URL)** — 오리진 트래픽 없이 엣지에서 권한 검증

이것이 Ceph·MinIO·S3가 하는 일의 **단순화된 핵심**입니다. 목표는 production이 아니라 **이해 가능한 레퍼런스**.

## 5분 데모

```bash
git clone https://github.com/HardcoreMonk/kvfs
cd kvfs

# 1. 전체 기동 (edge × 1 + dn × 3)
docker compose up -d

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

## 설계 결정 (브레인스토밍 기록)

| # | 결정 | 이유 |
|---|---|---|
| Phase 1 | **공개 오픈소스 데모**가 목적 | 프로덕션이 아님. 학습성·재현성·명료함 우선 |
| Phase 2 | **α(3-way 복제) + ε(UrlKey)**를 "얼굴" | 시각적 임팩트 + 적당한 난이도 |
| Phase 4 | **2-daemon** (edge inline coordinator + dn×3) | Coordinator HA는 Season 2 |
| Phase 5 | **HTTP REST** 통신 | curl·tcpdump로 학습 가능 |
| Phase 6 | **raw files 샤딩 + bbolt 메타 + sha256 CA** | ls·cat으로 디버깅 가능 |

## MVP에 들어있지 않은 것 (Season 2+)

- 청크 분할 (>64MB 객체)
- Reed-Solomon 이레이저 코딩
- Hot/Cold tier 자동 이동
- Consistent hashing rebalance
- Coordinator daemon 분리 · Raft HA
- gRPC 마이그레이션
- Multi-region

각각 블로그 시리즈 시즌 2의 독립 주제.

## 로드맵

```
Season 1 (MVP)     ─►  현재 (α+ε 데모)
Season 2           ─►  erasure coding, consistent hashing, tiering
Season 3           ─►  gRPC 마이그레이션, Coordinator 분리
Season 4 (장기)    ─►  NFS/SMB 확장 (기존 reference의 legacy NFS gateway 경로 참조)
```

## 기여

Apache 2.0. PR 환영. **교육적 가치**를 최상위 기준으로 코드 리뷰합니다.

## 블로그

- [`blog/01-hello-kvfs.md`](blog/01-hello-kvfs.md) — 프로젝트 소개 + α 데모 walkthrough
