# kvfs — Key-Value File System

> **분산 object storage 설계 원리를 살아있는 데모로** 보여주는 오픈소스 레퍼런스.
> **Go 1.26 · Apache 2.0 · 59 ADR · 55 blog episode · 48 라이브 데모 · 190 unit test**

## 이것은 무엇인가

분산 object storage 의 **핵심 원리**를 작고 이해 가능한 코드로 구현한다. 각 원리마다
ADR(설계 결정) + 블로그 episode + 라이브 데모로 검증.

| Season | 트랙 | 상태 | 핵심 |
|---|---|---|---|
| **1** | MVP | ✅ closed | 2-daemon · 3-way replication · UrlKey · CA chunk |
| **2** | 분산 알고리즘 | ✅ closed | HRW placement · rebalance · GC · chunking · EC |
| **3** | 운영성 | ✅ closed | auto-trigger · EC repair · meta backup · heartbeat · multi-edge HA |
| **4** | 성능·효율 | ✅ closed | streaming · CDC · WAL · election · sync repl · transactional Raft · micro-opts |
| **5** | coord 분리 | ✅ closed (Ep.1~7) | ADR-015·038~042. skeleton → edge meta client → HA → WAL sync → txn commit → placement RPC → cli inspect |
| **6** | coord operational migration | ✅ Ep.1~7 done | ADR-043~049. rebalance · GC · repair · DN registry · URLKey rotation/propagation — all on coord |
| **7** | textbook primitives | ✅ closed (Ep.1~4) | ADR-051~054. failure domain · degraded read · tunable consistency · anti-entropy/Merkle |
| **P8** | self-heal polish | ✅ P8-16 done | ADR-050·055~063. chaos hardening · 4채널 self-heal · continuous repair · Prometheus observability |

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

전체 48개 데모 라이브 PASS — Season 별 매핑은 아래 ADR 표 참조 (`scripts/demo-*.sh`). 그리스 letter (α~ω, 21개) = S1~S4, 히브리 letter (aleph~nun, 14개) = S5~S6, S7 + P8 anti-entropy specials 포함.

## 아키텍처

```
                                   ┌─ kvfs-coord (× N) ──┐  (S5 이후 옵션)
                                   │  placement·메타 owner │
                                   │  Raft + WAL replication │
   Client ─HTTP+UrlKey─► kvfs-edge ─┴─ HTTP REST ──► kvfs-dn (× N)
                            │                          │
                            ├─ thin gateway              └─ chunks/<sha256[0:2]>/<rest>
                            ├─ chunker / EC encoder
                            ├─ urlkey verify (multi-kid)
                            └─ (coord-proxy 모드 OR 인라인 coordinator)
```

- **2-daemon 모드** (S1~S4 호환): edge 가 coordinator 인라인 — placement·rebalance·GC·repair 모두 edge 안에서.
- **3-daemon 모드** (S5~): `EDGE_COORD_URL` 설정 시 edge 는 thin gateway, coord 가 placement·메타·일관성 owner. coord 는 Raft (`COORD_PEERS`) + WAL replication (`COORD_WAL_PATH`) + transactional commit (`COORD_TRANSACTIONAL_RAFT`) 으로 HA + zero-RPO 가능. cli 는 coord 에 직접 admin (`--coord URL`).

- 처음 읽기: [`docs/GUIDE.md`](docs/GUIDE.md) (또는 브라우저용 [`docs/guide.html`](docs/guide.html)) — 13개 챕터 walkthrough
- 짧은 reference: [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)

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

### Season 4 (성능·효율)

| ADR | 주제 | Demo | Blog |
|---|---|---|---|
| [017](docs/adr/ADR-017-streaming-put-get.md) | Streaming PUT/GET (io.Reader 기반) | σ | [14](blog/14-streaming.md) |
| [018](docs/adr/ADR-018-content-defined-chunking.md) | Content-defined chunking (FastCDC, opt-in) | τ | [15](blog/15-cdc.md) |
| [031](docs/adr/ADR-031-auto-leader-election.md) | Auto leader election (Raft-style, multi-edge HA) | υ | [16](blog/16-leader-election.md) |
| [019](docs/adr/ADR-019-wal-incremental-backup.md) | WAL of metadata mutations (audit + replay) | φ | [17](blog/17-wal.md) |

EC streaming = Ep.6 follow-up (demo-χ). EC+CDC = Ep.7 follow-up (demo-ψ). Sync WAL push = Ep.8 follow-up (demo-ω).

### 운영 보강 + post-S4 wave (Accepted)

| ADR | 주제 |
|---|---|
| [027](docs/adr/ADR-027-dynamic-dn-registry.md) | Dynamic DN registry (admin endpoint + bbolt 영속) |
| [028](docs/adr/ADR-028-urlkey-rotation.md) | UrlKey kid rotation (multi-key Signer) |
| [029](docs/adr/ADR-029-optional-tls.md) | Optional TLS / mTLS (env-driven opt-in) |
| [032](docs/adr/ADR-032-nfs-gateway-deferred.md) | NFS gateway — deferred (scope 평가) |
| [033](docs/adr/ADR-033-strict-replication.md) | Strict replication (informational) |
| [034](docs/adr/ADR-034-transactional-raft.md) | Transactional Raft (replicate-then-commit) |
| [035](docs/adr/ADR-035-micro-optimizations.md) | WAL group commit · 3-region CDC · chunker pool |
| [036](docs/adr/ADR-036-wal-batch-metrics.md) | WAL group commit observability gauges |
| [037](docs/adr/ADR-037-chunker-pool-cap.md) | Chunker scratch-pool soft cap |

### Season 5 (coord 분리)

| ADR | 주제 | Demo | Blog |
|---|---|---|---|
| [015](docs/adr/ADR-015-coordinator-daemon-split.md) | Coordinator daemon 분리 (ADR-002 supersede) | aleph (Ep.1) | [29](blog/29-coord-skeleton.md) |
| — | edge → coord meta client (`EDGE_COORD_URL`) | bet (Ep.2) | [30](blog/30-coord-client.md) |
| [038](docs/adr/ADR-038-coord-ha-via-raft.md) | Coord HA via Raft (ADR-031 reuse) | gimel (Ep.3) | [31](blog/31-coord-ha.md) |
| [039](docs/adr/ADR-039-coord-wal-replication.md) | Coord-to-coord WAL replication | dalet (Ep.4) | [32](blog/32-coord-wal-repl.md) |
| [040](docs/adr/ADR-040-coord-transactional-commit.md) | Coord transactional commit (ADR-034 port) | he (Ep.5) | [33](blog/33-coord-txn-commit.md) |
| [041](docs/adr/ADR-041-edge-placement-via-coord.md) | Edge → coord placement RPC (single source of truth) | vav (Ep.6) | [34](blog/34-coord-placement.md) |
| [042](docs/adr/ADR-042-cli-direct-coord-admin.md) | kvfs-cli direct coord admin (read-only inspect) | zayin (Ep.7) | [35](blog/35-cli-coord-admin.md) |

### Season 6 (coord operational migration)

| ADR | 주제 | Demo | Blog |
|---|---|---|---|
| [043](docs/adr/ADR-043-coord-rebalance-plan.md) | Rebalance plan on coord | chet (Ep.1) | [36](blog/36-coord-rebalance-plan.md) |
| [044](docs/adr/ADR-044-coord-rebalance-apply.md) | Rebalance apply on coord (`COORD_DN_IO`) | tet (Ep.2) | [37](blog/37-coord-rebalance-apply.md) |
| [045](docs/adr/ADR-045-coord-gc.md) | GC plan + apply on coord | yod (Ep.3) | [38](blog/38-coord-gc.md) |
| [046](docs/adr/ADR-046-coord-repair.md) | EC repair on coord | kaf (Ep.4) | [39](blog/39-coord-repair.md) |
| [047](docs/adr/ADR-047-coord-dns-admin.md) | DN registry mutation on coord | lamed (Ep.5) | [40](blog/40-coord-dns-admin.md) |
| [048](docs/adr/ADR-048-coord-urlkey-admin.md) | URLKey kid registry on coord | mem (Ep.6) | [41](blog/41-coord-urlkey.md) |
| [049](docs/adr/ADR-049-edge-urlkey-propagation.md) | Edge urlkey.Signer polling propagation | nun (Ep.7) | [42](blog/42-edge-urlkey-sync.md) |

### Season 7 (textbook primitives)

| ADR | 주제 | Demo | Blog |
|---|---|---|---|
| [051](docs/adr/ADR-051-failure-domain-hierarchy.md) | Failure domain hierarchy | samekh (Ep.1) | [43](blog/43-failure-domain-hierarchy.md) |
| [052](docs/adr/ADR-052-degraded-read.md) | Degraded read | ayin (Ep.2) | [44](blog/44-degraded-read.md) |
| [053](docs/adr/ADR-053-tunable-consistency.md) | Tunable consistency | pe (Ep.3) | [45](blog/45-tunable-consistency.md) |
| [054](docs/adr/ADR-054-anti-entropy-merkle.md) | Anti-entropy / Merkle + scrubber | tsadi (Ep.4) | [46](blog/46-anti-entropy-merkle.md) |

### P8 (chaos + anti-entropy self-heal polish)

| ADR | 주제 | Demo | Blog |
|---|---|---|---|
| [050](docs/adr/ADR-050-raft-stale-log-protection.md) | Raft stale-log protection + coord bootstrap | chaos-mixed | — |
| [055](docs/adr/ADR-055-anti-entropy-auto-repair.md) | Anti-entropy auto-repair + scheduled audit | anti-entropy-repair | [47](blog/47-anti-entropy-auto-repair.md) |
| [056](docs/adr/ADR-056-corrupt-repair-and-dry-run.md) | Corrupt repair + dry-run | anti-entropy-repair-corrupt | [48](blog/48-corrupt-repair-and-dry-run.md) |
| [057](docs/adr/ADR-057-ec-inline-repair.md) | EC inline repair | anti-entropy-repair-ec | [49](blog/49-ec-inline-repair.md) |
| [058](docs/adr/ADR-058-ec-corrupt-repair.md) | EC corrupt repair | anti-entropy-repair-ec-corrupt | [50](blog/50-ec-corrupt-repair.md) |
| [059](docs/adr/ADR-059-throttle-and-precision.md) | Repair throttle + precision | anti-entropy-throttle | [51](blog/51-throttle-and-precision.md) |
| [060](docs/adr/ADR-060-concurrent-ec-repair.md) | Concurrent EC repair | anti-entropy-concurrent | [52](blog/52-concurrent-ec-repair.md) |
| [061](docs/adr/ADR-061-resilience-polishes.md) | Resilience polishes | anti-entropy-resilience | [53](blog/53-resilience-polishes.md) |
| [062](docs/adr/ADR-062-auto-repair-and-metrics.md) | Auto-repair scheduling + coord metrics | anti-entropy-auto-metrics | [54](blog/54-auto-repair-and-metrics.md) |
| [063](docs/adr/ADR-063-anti-entropy-observability-completions.md) | Anti-entropy observability completions | anti-entropy-observability | [55](blog/55-anti-entropy-observability.md) |

## 환경 변수

| Env | Default | ADR | 용도 |
|---|---|---|---|
| `EDGE_ADDR` | `:8000` | 002 | HTTP bind |
| `EDGE_DNS` | required | 002 | comma-sep DN addrs |
| `EDGE_DNS_RESET` | 0 | 027 | bbolt `dns_runtime` 을 `EDGE_DNS` 로 재시드 |
| `EDGE_DATA_DIR` | `./edge-data` | 004 | bbolt 디렉토리 |
| `EDGE_URLKEY_SECRET` | required | 007 | HMAC 시크릿 |
| `EDGE_URLKEY_PRIMARY_KID` | (off) | 028 | primary URLKey kid override |
| `EDGE_QUORUM_WRITE` | auto | 002 | write quorum (0=auto) |
| `EDGE_CHUNK_SIZE` | 4 MiB | 011 | bytes per chunk (fixed mode) |
| `EDGE_CHUNK_MODE` | `fixed` | 018 | `fixed` \| `cdc` (FastCDC, replication only) |
| `EDGE_AUTO` | 0 | 013 | auto rebalance/GC opt-in |
| `EDGE_AUTO_REBALANCE_INTERVAL` | 5m | 013 | — |
| `EDGE_AUTO_GC_INTERVAL` | 15m | 013 | — |
| `EDGE_AUTO_GC_MIN_AGE` | 60s | 013 | GC safety window |
| `EDGE_AUTO_CONCURRENCY` | 4 | 013 | auto worker concurrency |
| `EDGE_HEARTBEAT_INTERVAL` | 10s | 030 | DN probe interval (`0s` = off) |
| `EDGE_HEARTBEAT_FAIL_THRESHOLD` | 3 | 030 | 연속 실패 → unhealthy |
| `EDGE_SNAPSHOT_DIR` | (off) | 016 | auto-snapshot 디렉토리 |
| `EDGE_SNAPSHOT_INTERVAL` | 1h | 016 | — |
| `EDGE_SNAPSHOT_KEEP` | 7 | 016 | retention |
| `EDGE_ROLE` | primary | 022 | `primary` \| `follower` |
| `EDGE_PRIMARY_URL` | (follower-only) | 022 | follower → primary URL |
| `EDGE_FOLLOWER_PULL_INTERVAL` | 30s | 022 | snapshot pull 주기 |
| `EDGE_PEERS` | (off) | 031 | comma-sep peer URLs (election opt-in) |
| `EDGE_SELF_URL` | (req for election) | 031 | this edge's own peer URL |
| `EDGE_ELECTION_HB_INTERVAL` | 500ms | 031 | leader heartbeat 주기 |
| `EDGE_ELECTION_TIMEOUT_MIN/MAX` | 1500ms / 3000ms | 031 | follower election timer (jitter range) |
| `EDGE_WAL_PATH` | (off) | 019 | metadata mutation WAL file (opt-in audit log) |
| `EDGE_WAL_BATCH_INTERVAL` | (off) | 035 | group commit interval (e.g. `5ms`) |
| `EDGE_STRICT_REPL` | 0 | 033 | informational strict replication 503 on quorum miss |
| `EDGE_TRANSACTIONAL_RAFT` | 0 | 034 | replicate-then-commit (PutObject only) |
| `EDGE_PLACEMENT_PREFER` | (off) | — | DN class bias (e.g. `hot`) |
| `EDGE_METRICS` | 1 | 036/037 | `/metrics` Prometheus endpoint |
| `EDGE_CHUNKER_POOL_CAP_BYTES` | (off) | 037 | chunker scratch-pool soft cap |
| `EDGE_TLS_CERT/KEY` | (off) | 029 | edge HTTPS server |
| `EDGE_DN_TLS_CA`, `EDGE_DN_TLS_CLIENT_CERT/KEY` | (off) | 029 | edge → DN TLS / mTLS client |
| `EDGE_SKIP_AUTH` | 0 | — | DEMO 전용 (production 금지) |
| `EDGE_COORD_URL` | (off) | 015 | coord proxy mode 활성 (메타·placement 위임) |
| `EDGE_COORD_URLKEY_POLL_INTERVAL` | 30s | 049 | coord urlkey 변경 polling 주기 |
| `EDGE_COORD_LOOKUP_CACHE_TTL` | (off) | — | opt-in coord lookup 결과 캐시 TTL (e.g. `2s`) |
| `COORD_ADDR` | `:9000` | 015 | coord HTTP bind |
| `COORD_DATA_DIR` | `./coord-data` | 015 | coord bbolt 디렉토리 |
| `COORD_DNS` | required | 015 | coord 가 알 DN addrs (comma-sep) |
| `COORD_PEERS` / `COORD_SELF_URL` | (off) | 038 | coord-side election peer set |
| `COORD_WAL_PATH` | (off) | 039 | coord-to-coord WAL replication |
| `COORD_TRANSACTIONAL_RAFT` | 0 | 040 | coord replicate-then-commit (Elector + WAL 필수) |
| `COORD_DN_IO` | 0 | 044 | coord 가 chunk I/O — rebalance/gc/repair apply on coord |
| `COORD_ANTI_ENTROPY_INTERVAL` | (off) | 055 | leader-only scheduled audit |
| `COORD_AUTO_REPAIR_INTERVAL` | (off) | 062 | leader-only scheduled self-heal |
| `COORD_AUTO_REPAIR_MAX` | 100 | 062 | auto-repair tick 당 max repairs |
| `COORD_AUTO_REPAIR_CONCURRENCY` | 4 | 062 | auto-repair worker pool size |
| `COORD_METRICS` | 1 | 062/063 | coord `/metrics` Prometheus endpoint |
| `DN_ADDR`, `DN_DATA_DIR`, `DN_ID` | `:8080`, required, required | 002 | DN 측 |
| `DN_SCRUB_INTERVAL` | (off) | 054 | bit-rot scrubber pacing |
| `DN_TLS_CERT/KEY/CLIENT_CA` | (off) | 029 | DN-side TLS / mTLS |

## 다음 작업

[`docs/FOLLOWUP.md`](docs/FOLLOWUP.md) — 우선순위별 pending 작업 단일 소스.

## 기여

Apache 2.0. PR 환영. **교육적 가치**를 최상위 기준으로 코드 리뷰합니다. 자세한
워크플로우는 [`CONTRIBUTING.md`](CONTRIBUTING.md).
