# kvfs — Key-Value File System

> **분산 object storage 설계 원리를 살아있는 데모로** 보여주는 오픈소스 레퍼런스.
> **Go 1.26 · Apache 2.0 · Season 3 진행 중 · 주말 프로젝트**

## 이것은 무엇인가

kvfs는 **분산 object storage의 핵심 원리**를 작고 이해 가능한 코드로 구현합니다. 각 원리마다 **시즌별 Episode** 로 추가:

### Season 1 (MVP 완료 ✅)
1. **3-way replication + quorum 쓰기** — 1 노드 죽어도 데이터 지속 (ADR-002)
2. **URL 서명 (presigned URL)** — 오리진 트래픽 없이 엣지에서 권한 검증 (ADR-007)
3. **Content-addressable chunk** — sha256 기반 자동 dedup + 무료 무결성 (ADR-005)

### Season 2 (진행 중)
4. **Consistent hashing (Rendezvous / HRW)** ← **Ep.1 완료** — 노드 추가·제거 시 최소 이동 (ADR-009)
5. **Rebalance worker** ← **Ep.2 완료** — 정지된 청크를 desired DN 으로 이사 (copy-then-update · ADR-010)
6. **Surplus chunk GC** ← **Ep.3 완료** — claimed-set + min-age 두 안전망으로 디스크↔메타 정렬 (ADR-012)
7. **Chunking** ← **Ep.4 완료** — 큰 객체를 고정 크기 청크로 자르기 (ADR-011, ADR-006 supersede)
8. **Reed-Solomon EC** ← **Ep.5 완료** — K+M shard 중 어느 K개로도 복원, 디스크 75% 절감 (ADR-008, from-scratch GF(2^8))

### Season 3 (운영성 트랙, 진행 중)
9. **Auto-trigger** ← **Ep.1 완료** — 운영자 호출 0번에 클러스터 스스로 정렬 (ADR-013, opt-in time-based loops)
10. **EC stripe rebalance** ← **Ep.2 완료** — EC 객체도 DN 추가 시 자동 정렬, set-based 최소 이동 (ADR-024)
11. **EC repair queue** ← **Ep.3 완료** — dead DN 의 shard 를 K survivors 로 Reed-Solomon Reconstruct (ADR-025)
12. **Meta backup** ← **Ep.4 완료** — bbolt hot snapshot + offline restore, `kvfs-cli meta` (ADR-014)
13. **DN heartbeat** ← **Ep.5 완료** — edge in-process ticker로 모든 DN /healthz pull, threshold 후 unhealthy 표시 (ADR-030)
14. Multi-edge HA · WAL · Streaming · CDC chunking — 예정

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

# 5. ζ 데모: 4번째 DN 추가 — 신규 쓰기는 분산, 기존 청크는 정지 (Season 2)
./scripts/demo-zeta.sh
#    → seed 4 객체 (3-DN 시점) → dn4 추가 + edge 재시작 → new 8 객체
#    → dn4 가 새 쓰기에 참여하지만 기존 청크는 dn1/2/3 에 그대로
#    → ADR-010 (Rebalance worker) 의 동기 부여

# 6. η 데모: rebalance worker — 정지된 청크를 desired DN으로 이사 (Season 2 Ep.2)
./scripts/demo-eta.sh
#    → 위 ζ 와 동일 setup → kvfs-cli rebalance --plan → --apply
#    → misplaced 청크가 dn4 로 복사 + 메타 갱신 + 두 번째 plan = 0 (멱등)
#    → never-delete 규칙: dn1/2/3 디스크의 surplus 는 그대로 (GC 가 청소)

# 7. θ 데모: Surplus chunk GC — 메타 = 디스크 정렬 (Season 2 Ep.3)
./scripts/demo-theta.sh
#    → 위 η 까지 한 흐름 → kvfs-cli gc --plan → --apply
#    → claimed-set + min-age 두 안전망으로 surplus 만 안전 삭제
#    → 최종: disk chunk 수 = 객체 × R (메타 의도와 정확히 일치)

# 8. ι 데모: Chunking — 256 KiB 객체가 4 청크로 분산 (Season 2 Ep.4)
./scripts/demo-iota.sh
#    → EDGE_CHUNK_SIZE=64KiB 로 클러스터 기동
#    → 256 KiB 랜덤 PUT → 정확히 4 청크 + 각각 독립 HRW 배치
#    → GET 라운드트립 sha256 일치 확인

# 9. κ 데모: Reed-Solomon EC — 디스크 50% overhead 로 R=3 동일 내구성 (Season 2 Ep.5)
./scripts/demo-kappa.sh
#    → 6 DN 클러스터 + X-KVFS-EC: 4+2 헤더로 PUT
#    → 128 KiB → 2 stripes × 6 shards = 12 shards (6 DN 에 균등)
#    → dn5+dn6 kill (M=2 한계) → GET 이 GF(2^8) Reconstruct 로 복원

# 10. λ 데모: Auto-trigger — 운영자 호출 0번에 자동 정렬 (Season 3 Ep.1)
./scripts/demo-lambda.sh
#    → EDGE_AUTO=1 + 짧은 interval (10s/12s) 로 클러스터 기동
#    → seed → dn4 추가 → 가만히 sleep → cluster 가 스스로 rebalance + GC
#    → kvfs-cli auto --status 로 cycle 결과 추적

# 11. μ 데모: EC stripe rebalance — dn7 추가 시 EC shard 이동 (Season 3 Ep.2)
./scripts/demo-mu.sh
#    → 6 DN EC(4+2) PUT → dn7 추가 → rebalance --plan/--apply
#    → 정확히 stripe 당 1 shard 이동 (set-based 최소 이동) → GET sha256 일치

# 12. ν 데모: EC repair — dn5+dn6 영구 사망 → K survivors 로 reconstruct (Season 3 Ep.3)
./scripts/demo-nu.sh
#    → 8 DN EC(4+2) PUT → dn5+dn6 kill+remove from registry
#    → repair --plan: dead shards 정확 식별 (sorted unused desired)
#    → repair --apply: Reed-Solomon Reconstruct → GET sha256 일치 + 멱등
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

## 설계 결정 (ADR 전문)

각 결정은 [`docs/adr/`](docs/adr/) 의 독립 문서로 박힘 — 불변 기록.

### Season 1 (Accepted)

| ADR | 주제 | 핵심 |
|---|---|---|
| [001](docs/adr/ADR-001-independent-identity.md) | 독자적 프로젝트 identity | clean-slate Apache 2.0 코드베이스 |
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
| [010](docs/adr/ADR-010-rebalance-worker.md) | Rebalance worker | `internal/rebalance/`. copy-then-update, full-success 시 trim |
| [012](docs/adr/ADR-012-surplus-gc.md) | Surplus chunk GC | `internal/gc/`. claimed-set + min-age 두 안전망 |
| [011](docs/adr/ADR-011-chunking.md) | Chunking (ADR-006 supersede) | `internal/chunker/`. 고정 크기, EDGE_CHUNK_SIZE |
| [008](docs/adr/ADR-008-reed-solomon-ec.md) | Reed-Solomon EC (from-scratch) | `internal/reedsolomon/`. GF(2^8) + Vandermonde + Gauss-Jordan |

### Season 3 (Accepted)

| ADR | 주제 | 핵심 |
|---|---|---|
| [013](docs/adr/ADR-013-auto-trigger.md) | Auto-trigger (in-edge ticker, opt-in) | `time.Ticker` 두 개, 같은 mutex 공유, ring buffer 32 |
| [024](docs/adr/ADR-024-ec-stripe-rebalance.md) | EC stripe rebalance (set-based) | `internal/rebalance/` 확장, Migration `Kind` 분기 |
| [027](docs/adr/ADR-027-dynamic-dn-registry.md) | Dynamic DN registry | `Coordinator.UpdateNodes` + `dns_runtime` bbolt + admin endpoint |
| [028](docs/adr/ADR-028-urlkey-rotation.md) | UrlKey kid rotation | multi-key Signer + `urlkey_secrets` bbolt |
| [029](docs/adr/ADR-029-optional-tls.md) | Optional TLS / mTLS | env-driven opt-in, gen-tls-certs.sh |
| [025](docs/adr/ADR-025-ec-repair.md) | EC repair queue | `internal/repair/` Reed-Solomon Reconstruct |

### Season 3+ 예정 (docs/FOLLOWUP.md)

- ADR-014 Meta backup/HA · ADR-015 Coordinator daemon 분리
- ADR-022 Multi-edge leader election · ADR-030 DN heartbeat
- ADR-017 Streaming PUT/GET · ADR-018 CDC chunking
- ADR-019 SIMD-accelerated RS · ADR-020 Hybrid storage

## 로드맵

```
Season 1 (MVP 완료 ✅)  α + ε 데모 · 7 ADR · 33 파일 · 1,367 LOC
Season 2 (진행 중 ▶)  ADR-009 ✅ → 010 → 011 → 008 → 012 → ...
Season 3 (미정)       gRPC · Coordinator daemon · Raft · multi-region
Season 4 (장기)       NFS/SMB 게이트웨이
```

## 다음 작업

[`docs/FOLLOWUP.md`](docs/FOLLOWUP.md) — 우선순위별 pending 작업 단일 소스.

## 기여

Apache 2.0. PR 환영. **교육적 가치**를 최상위 기준으로 코드 리뷰합니다.

## 블로그

- [`blog/01-hello-kvfs.md`](blog/01-hello-kvfs.md) — 프로젝트 소개 + α 데모 walkthrough
- [`blog/02-consistent-hashing.md`](blog/02-consistent-hashing.md) — Rendezvous Hashing 실측 (3→4 / 5→6 / 10→9 시나리오)
- [`blog/03-rebalance.md`](blog/03-rebalance.md) — Rebalance worker · copy-then-update · trim-on-full-success (ADR-010)
- [`blog/04-gc.md`](blog/04-gc.md) — Surplus chunk GC · claimed-set + min-age 두 안전망 (ADR-012)
- [`blog/05-chunking.md`](blog/05-chunking.md) — Chunking · 256 KiB → 4 청크 라이브 (ADR-011)
- [`blog/06-erasure-coding.md`](blog/06-erasure-coding.md) — Reed-Solomon EC · GF(2^8) from-scratch · dn5+dn6 kill 후 복원 (ADR-008)
- [`blog/07-auto-trigger.md`](blog/07-auto-trigger.md) — Auto-trigger · 운영자 호출 0번에 자동 정렬 (ADR-013, Season 3 Ep.1)
- [`blog/08-ec-rebalance.md`](blog/08-ec-rebalance.md) — EC stripe rebalance · set-based 최소 이동 (ADR-024, Season 3 Ep.2)
- [`blog/09-ec-repair.md`](blog/09-ec-repair.md) — EC repair · K survivors → Reed-Solomon Reconstruct (ADR-025, Season 3 Ep.3)
