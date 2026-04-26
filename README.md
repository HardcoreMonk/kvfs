# kvfs — Key-Value File System

> **분산 object storage 설계 원리를 살아있는 데모로** 보여주는 오픈소스 레퍼런스.
> **Go 1.26 · Apache 2.0 · 24 ADR · 15 blog episode · 17 라이브 데모 · 117 unit test**

## 이것은 무엇인가

분산 object storage 의 **핵심 원리**를 작고 이해 가능한 코드로 구현한다. 각 원리마다
ADR(설계 결정) + 블로그 episode + 라이브 데모로 검증.

| Season | 트랙 | 상태 | 핵심 |
|---|---|---|---|
| **1** | MVP | ✅ closed | 2-daemon · 3-way replication · UrlKey · CA chunk |
| **2** | 분산 알고리즘 | ✅ closed | HRW placement · rebalance · GC · chunking · EC |
| **3** | 운영성 | ✅ closed | auto-trigger · EC repair · meta backup · heartbeat · multi-edge HA |
| **4** | 성능·효율 | ▶ Ep.2 (CDC dedup) | streaming · CDC chunking · WAL · auto leader election |

이것이 Ceph·MinIO·S3 가 하는 일의 **단순화된 핵심**. 목표는 production 이 아니라
**이해 가능한 레퍼런스**.

## 5분 데모

```bash
git clone https://github.com/HardcoreMonk/kvfs
cd kvfs

# 1. 클러스터 기동 (edge × 1 + dn × 3)
./scripts/up.sh

# 2. α — 3-way replication 내구성 (DN-1 죽여도 GET 성공)
./scripts/demo-alpha.sh

# 3. ε — UrlKey presigned URL (만료 후 401)
./scripts/demo-epsilon.sh
```

전체 15개 데모 라이브 PASS — Season 별 매핑은 아래 ADR 표 참조 (`scripts/demo-*.sh`).

## 아키텍처

```
   Client ─HTTP+UrlKey─► kvfs-edge ─HTTP REST─► kvfs-dn (× N)
                            │                      │
                            ├─ placement (HRW)     └─ chunks/<sha256[0:2]>/<rest>
                            ├─ chunker / EC
                            ├─ rebalance / GC / repair
                            ├─ heartbeat (DN liveness)
                            ├─ snapshot scheduler (auto backup)
                            └─ bbolt meta (object → chunk[s] / shards)
```

상세: [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)

## 설계 결정 (ADR 전문)

각 결정은 [`docs/adr/`](docs/adr/) 의 독립 문서로 박힘 — 불변 기록.

### Season 1 (MVP)

| ADR | 주제 | Blog |
|---|---|---|
| [001](docs/adr/ADR-001-independent-identity.md) | 독자적 프로젝트 identity (clean-slate) | [01](blog/01-hello-kvfs.md) |
| [002](docs/adr/ADR-002-two-daemon-architecture.md) | 2-daemon MVP (edge + dn × 3) | — |
| [003](docs/adr/ADR-003-http-rest-protocol.md) | HTTP REST 통신 (curl·tcpdump 친화) | — |
| [004](docs/adr/ADR-004-bbolt-metadata.md) | bbolt 메타 (pure Go, 외부 의존 1) | — |
| [005](docs/adr/ADR-005-content-addressable-chunks.md) | sha256 content-addressable | — |
| [006](docs/adr/ADR-006-mvp-single-chunk.md) | 1 object = 1 chunk (superseded by 011) | — |
| [007](docs/adr/ADR-007-urlkey-hmac-sha256.md) | UrlKey HMAC-SHA256 | — |

### Season 2 (분산 알고리즘)

| ADR | 주제 | Demo | Blog |
|---|---|---|---|
| [009](docs/adr/ADR-009-consistent-hashing.md) | Rendezvous Hashing (HRW) | ζ | [02](blog/02-consistent-hashing.md) |
| [010](docs/adr/ADR-010-rebalance-worker.md) | Rebalance worker (copy-then-update) | η | [03](blog/03-rebalance.md) |
| [012](docs/adr/ADR-012-surplus-gc.md) | Surplus chunk GC (claimed-set + min-age) | θ | [04](blog/04-gc.md) |
| [011](docs/adr/ADR-011-chunking.md) | Chunking (ADR-006 supersede) | ι | [05](blog/05-chunking.md) |
| [008](docs/adr/ADR-008-reed-solomon-ec.md) | Reed-Solomon EC (GF(2^8) from-scratch) | κ | [06](blog/06-erasure-coding.md) |

### Season 3 (운영성)

| ADR | 주제 | Demo | Blog |
|---|---|---|---|
| [013](docs/adr/ADR-013-auto-trigger.md) | Auto-trigger (in-edge ticker) | λ | [07](blog/07-auto-trigger.md) |
| [024](docs/adr/ADR-024-ec-stripe-rebalance.md) | EC stripe rebalance (set-based 최소 이동) | μ | [08](blog/08-ec-rebalance.md) |
| [025](docs/adr/ADR-025-ec-repair.md) | EC repair queue (K survivors → reconstruct) | ν | [09](blog/09-ec-repair.md) |
| [014](docs/adr/ADR-014-meta-backup.md) | Meta backup (snapshot + offline restore) | ξ | [10](blog/10-meta-backup.md) |
| [030](docs/adr/ADR-030-dn-heartbeat.md) | DN heartbeat (in-edge pull) | ο | [11](blog/11-dn-heartbeat.md) |
| [016](docs/adr/ADR-016-auto-snapshot-scheduler.md) | Auto-snapshot scheduler (ticker + retention) | π | [12](blog/12-auto-snapshot.md) |
| [022](docs/adr/ADR-022-multi-edge-ha.md) | Multi-edge HA (read-replica + atomic.Pointer hot-swap) | ρ | [13](blog/13-multi-edge-ha.md) |

### Season 4 (성능·효율, 진행 중)

| ADR | 주제 | Demo | Blog |
|---|---|---|---|
| [017](docs/adr/ADR-017-streaming-put-get.md) | Streaming PUT/GET (io.Reader 기반) | σ | [14](blog/14-streaming.md) |
| [018](docs/adr/ADR-018-content-defined-chunking.md) | Content-defined chunking (FastCDC, opt-in) | τ | [15](blog/15-cdc.md) |

### 운영 보강 (Accepted)

| ADR | 주제 |
|---|---|
| [027](docs/adr/ADR-027-dynamic-dn-registry.md) | Dynamic DN registry (admin endpoint + bbolt 영속) |
| [028](docs/adr/ADR-028-urlkey-rotation.md) | UrlKey kid rotation (multi-key Signer) |
| [029](docs/adr/ADR-029-optional-tls.md) | Optional TLS / mTLS (env-driven opt-in) |

### 예정 ([P4-02](docs/FOLLOWUP.md))

ADR-018 CDC chunking · ADR-019 WAL/incremental · ADR-031 자동 leader election

## 환경 변수

| Env | Default | ADR | 용도 |
|---|---|---|---|
| `EDGE_ADDR` | `:8000` | 002 | HTTP bind |
| `EDGE_DNS` | required | 002 | comma-sep DN addrs |
| `EDGE_DATA_DIR` | `./edge-data` | 004 | bbolt 디렉토리 |
| `EDGE_URLKEY_SECRET` | required | 007 | HMAC 시크릿 |
| `EDGE_QUORUM_WRITE` | auto | 002 | write quorum (0=auto) |
| `EDGE_CHUNK_SIZE` | 4 MiB | 011 | bytes per chunk (fixed mode) |
| `EDGE_CHUNK_MODE` | `fixed` | 018 | `fixed` \| `cdc` (FastCDC, replication only) |
| `EDGE_AUTO` | 0 | 013 | auto rebalance/GC opt-in |
| `EDGE_AUTO_REBALANCE_INTERVAL` | 5m | 013 | — |
| `EDGE_AUTO_GC_INTERVAL` | 15m | 013 | — |
| `EDGE_HEARTBEAT_INTERVAL` | 10s | 030 | DN probe interval (`0s` = off) |
| `EDGE_HEARTBEAT_FAIL_THRESHOLD` | 3 | 030 | 연속 실패 → unhealthy |
| `EDGE_SNAPSHOT_DIR` | (off) | 016 | auto-snapshot 디렉토리 |
| `EDGE_SNAPSHOT_INTERVAL` | 1h | 016 | — |
| `EDGE_SNAPSHOT_KEEP` | 7 | 016 | retention |
| `EDGE_ROLE` | primary | 022 | `primary` \| `follower` |
| `EDGE_PRIMARY_URL` | (follower-only) | 022 | follower → primary URL |
| `EDGE_FOLLOWER_PULL_INTERVAL` | 30s | 022 | snapshot pull 주기 |
| `EDGE_TLS_CERT/KEY`, `EDGE_DN_TLS_CA/CLIENT_CERT/KEY` | (off) | 029 | TLS / mTLS |
| `EDGE_SKIP_AUTH` | 0 | — | DEMO 전용 (production 금지) |
| `DN_DATA_DIR`, `DN_TLS_CERT/KEY/CLIENT_CA` | — | 002/029 | DN 측 |

## 다음 작업

[`docs/FOLLOWUP.md`](docs/FOLLOWUP.md) — 우선순위별 pending 작업 단일 소스.

## 기여

Apache 2.0. PR 환영. **교육적 가치**를 최상위 기준으로 코드 리뷰합니다. 자세한
워크플로우는 [`CONTRIBUTING.md`](CONTRIBUTING.md).
