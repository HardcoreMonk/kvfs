# kvfs Architectural Decision Records

각 ADR은 **한 번 내린 결정**을 기록. 한 번 **Accepted** 된 ADR은 불변 — 바꿀 때는 **새 ADR로 supersede**.

## 형식

- 번호: `ADR-NNN` 3자리 0-padded
- 상태: `Proposed` → `Accepted` → (`Superseded by ADR-MMM` | `Deprecated`)
- 파일명: `ADR-NNN-short-slug.md`
- 한국어·한 페이지 이내

## 템플릿

```markdown
# ADR-NNN — 제목

## 상태
Accepted · YYYY-MM-DD

## 맥락
왜 이 결정이 필요했나. 제약·대안·기존 상태.

## 결정
무엇을 결정했나. 한 문단이면 충분.

## 결과
+ 긍정·얻은 것
- 부정·포기한 것
- 트레이드오프·미래 숙제
```

> **표 정렬 규칙**: 각 시즌 내부는 **ADR 번호 오름차순**. 작성 일자(시즌별 진행 순) 는 우측 상태 컬럼.

## Season 1 — MVP (closed)

| # | 제목 | 상태 | 연결 |
|---|---|---|---|
| [001](ADR-001-independent-identity.md) | 독자적 프로젝트 identity | Accepted | — |
| [002](ADR-002-two-daemon-architecture.md) | 2-daemon MVP (edge + dn) | Accepted | `docs/ARCHITECTURE.md` §컴포넌트 |
| [003](ADR-003-http-rest-protocol.md) | 내·외부 통신 HTTP REST | Accepted | — |
| [004](ADR-004-bbolt-metadata.md) | 메타데이터 저장 bbolt | Accepted | `internal/store/` |
| [005](ADR-005-content-addressable-chunks.md) | Content-addressable sha256 chunks | Accepted | `internal/dn/` `internal/edge/` |
| [006](ADR-006-mvp-single-chunk.md) | MVP: 1 object = 1 chunk (Season 2 ADR-011 으로 supersede) | Accepted → Superseded | — |
| [007](ADR-007-urlkey-hmac-sha256.md) | UrlKey = HMAC-SHA256 | Accepted | `internal/urlkey/` |

## Season 2 — placement · rebalance · dedup · EC (closed, Ep.1~5)

| # | 제목 | 상태 | 연결 · demo |
|---|---|---|---|
| [008](ADR-008-reed-solomon-ec.md) | Reed-Solomon EC (from-scratch GF(2^8)) | Accepted · 2026-04-26 | `internal/reedsolomon/` · demo-kappa (Ep.5) |
| [009](ADR-009-consistent-hashing.md) | Consistent Hashing (Rendezvous/HRW) | Accepted · 2026-04-25 | `internal/placement/` · demo-zeta (Ep.1) |
| [010](ADR-010-rebalance-worker.md) | Rebalance worker (copy-then-update, no-delete MVP) | Accepted · 2026-04-25 | `internal/rebalance/` · demo-eta (Ep.2) |
| [011](ADR-011-chunking.md) | Chunking (고정 크기, ADR-006 supersede) | Accepted · 2026-04-26 | `internal/chunker/` · demo-iota (Ep.4) |
| [012](ADR-012-surplus-gc.md) | Surplus chunk GC (claimed-set + min-age 안전망) | Accepted · 2026-04-26 | `internal/gc/` · demo-theta (Ep.3) |

## Season 3 — operability (closed, Ep.1~7)

| # | 제목 | 상태 | 연결 · demo |
|---|---|---|---|
| [013](ADR-013-auto-trigger.md) | Auto-trigger policy (in-edge ticker, opt-in) | Accepted · 2026-04-26 | `internal/edge/` auto loops · demo-lambda (Ep.1) |
| [014](ADR-014-meta-backup.md) | Metadata backup (snapshot + offline restore) | Accepted · 2026-04-26 | `internal/store/snapshot.go` + `kvfs-cli meta` · demo-xi (Ep.4) |
| [016](ADR-016-auto-snapshot-scheduler.md) | Auto-snapshot scheduler (ticker + retention) | Accepted · 2026-04-26 | `internal/store/scheduler.go` · demo-pi (Ep.6) |
| [022](ADR-022-multi-edge-ha.md) | Multi-edge HA (read-replica via snapshot-pull) | Accepted · 2026-04-26 | `internal/edge/replica.go` · demo-rho (Ep.7, season close) |
| [024](ADR-024-ec-stripe-rebalance.md) | EC stripe rebalance (set-based, min-migration) | Accepted · 2026-04-26 | `internal/rebalance/` 확장 · demo-mu (Ep.2) |
| [025](ADR-025-ec-repair.md) | EC repair queue (Reed-Solomon Reconstruct K survivors) | Accepted · 2026-04-26 | `internal/repair/` + `kvfs-cli repair` · demo-nu (Ep.3) |
| [027](ADR-027-dynamic-dn-registry.md) | Dynamic DN registry (admin endpoint + bbolt 영속) | Accepted · 2026-04-26 | `internal/coordinator/` UpdateNodes |
| [028](ADR-028-urlkey-rotation.md) | UrlKey kid rotation (multi-key Signer + bbolt 영속) | Accepted · 2026-04-26 | `internal/urlkey/` multi-key |
| [029](ADR-029-optional-tls.md) | Optional TLS / mTLS (env-driven opt-in) | Accepted · 2026-04-26 | `cmd/kvfs-{edge,dn}/` + `scripts/gen-tls-certs.sh` |
| [030](ADR-030-dn-heartbeat.md) | DN heartbeat (in-edge pull-based liveness) | Accepted · 2026-04-26 | `internal/heartbeat/` · demo-omicron (Ep.5) |

## Season 4 — streaming · CDC · Raft · WAL (closed, Ep.1~8)

| # | 제목 | 상태 | 연결 · demo |
|---|---|---|---|
| [017](ADR-017-streaming-put-get.md) | Streaming PUT/GET (io.Reader 기반) | Accepted · 2026-04-26 | `internal/chunker/stream.go` · demo-sigma (Ep.1). EC streaming = Ep.6 follow-up |
| [018](ADR-018-content-defined-chunking.md) | Content-defined chunking (FastCDC, opt-in) | Accepted · 2026-04-26 | `internal/chunker/cdc.go` · demo-tau (Ep.2). EC+CDC = Ep.7 follow-up (demo-psi) |
| [019](ADR-019-wal-incremental-backup.md) | WAL of metadata mutations | Accepted · 2026-04-26 | `internal/store/wal.go` · demo-phi (Ep.4). Follower auto-pull = Ep.5 follow-up |
| [031](ADR-031-auto-leader-election.md) | Auto leader election (Raft-style 3-state) | Accepted · 2026-04-26 | `internal/election/` · demo-upsilon (Ep.3). Sync WAL push = Ep.8 follow-up (demo-omega) |

## Post-Season-4 — P4-09 wave (closed)

| # | 제목 | 상태 | 연결 |
|---|---|---|---|
| [032](ADR-032-nfs-gateway-deferred.md) | NFS gateway (deferred — scope evaluation) | Accepted · 2026-04-26 | — (defer 사유 기록) |
| [033](ADR-033-strict-replication.md) | Strict replication (informational; bbolt commits regardless) | Accepted · 2026-04-26 | `EDGE_STRICT_REPL=1` |
| [034](ADR-034-transactional-raft.md) | Transactional Raft (replicate-then-commit, true strict) | Accepted · 2026-04-26 | `EDGE_TRANSACTIONAL_RAFT=1` + `MarshalPutObjectEntry` |
| [035](ADR-035-micro-optimizations.md) | WAL group commit · 3-region CDC · chunker sync.Pool | Accepted · 2026-04-26 | `EDGE_WAL_BATCH_INTERVAL`, `CDCConfig.MaskBitsRelax`, `chunker.scratchPool` |
| [036](ADR-036-wal-batch-metrics.md) | WAL group commit observability (batch_size + durable_lag gauges) | Accepted · 2026-04-26 | `WAL.LastBatchSize` / `OldestUnsyncedAge` + `internal/edge/metrics.go` |
| [037](ADR-037-chunker-pool-cap.md) | Chunker scratch-pool soft cap (memory pressure 대응) | Accepted · 2026-04-26 | `chunker.SetPoolCap` + `EDGE_CHUNKER_POOL_CAP_BYTES` |

## Season 5 — coord 분리 (in progress, Ep.1~)

| # | 제목 | 상태 | 연결 · demo |
|---|---|---|---|
| [015](ADR-015-coordinator-daemon-split.md) | Coordinator daemon 분리 (ADR-002 **supersede**) | Accepted · 2026-04-26 | `cmd/kvfs-coord/` + `internal/coord/` · demo-aleph (Ep.1) · demo-bet (Ep.2) |
| [038](ADR-038-coord-ha-via-raft.md) | Coord HA via Raft (election + leader-redirect, ADR-031 reuse) | Accepted · 2026-04-27 | `coord.Server.Elector` + `X-COORD-LEADER` redirect · demo-gimel (Ep.3) |
| [039](ADR-039-coord-wal-replication.md) | Coord-to-coord WAL replication (ADR-038 gap closer) | Accepted · 2026-04-27 | `COORD_WAL_PATH` + leader push hook + follower ApplyEntry · demo-dalet (Ep.4) |
| [040](ADR-040-coord-transactional-commit.md) | Coord transactional commit (ADR-034 port; closes phantom-write window) | Accepted · 2026-04-27 | `COORD_TRANSACTIONAL_RAFT` + commit() helper · demo-he (Ep.5) |
| [041](ADR-041-edge-placement-via-coord.md) | Edge routes placement decisions through coord (single source of truth) | Accepted · 2026-04-27 | `CoordClient.PlaceN` + `Server.placeN` helper · demo-vav (Ep.6) |
| [042](ADR-042-cli-direct-coord-admin.md) | kvfs-cli direct coord admin (read-only inspect) | Accepted · 2026-04-27 | `coord /v1/coord/admin/{objects,dns}` + cli `inspect --coord` · demo-zayin (Ep.7) |

## Season 6 — coord operational migration (in progress, Ep.1~)

| # | 제목 | 상태 | 연결 · demo |
|---|---|---|---|
| [043](ADR-043-coord-rebalance-plan.md) | Rebalance plan computed on coord | Accepted · 2026-04-27 | `coord /v1/coord/admin/rebalance/plan` + cli `rebalance --plan --coord` · demo-chet (Ep.1) |
| [044](ADR-044-coord-rebalance-apply.md) | Rebalance apply on coord (DN I/O introduced) | Accepted · 2026-04-27 | `COORD_DN_IO` + `coord.Server.Coord` + `/v1/coord/admin/rebalance/apply` · demo-tet (Ep.2) |
| [045](ADR-045-coord-gc.md) | GC plan + apply on coord | Accepted · 2026-04-27 | `/v1/coord/admin/gc/{plan,apply}` + cli `gc --coord` · demo-yod (Ep.3) |
| [046](ADR-046-coord-repair.md) | EC repair on coord | Accepted · 2026-04-27 | `/v1/coord/admin/repair/{plan,apply}` + cli `repair --coord` · demo-kaf (Ep.4) |
| [047](ADR-047-coord-dns-admin.md) | DN registry mutation on coord (add/remove/class) | Accepted · 2026-04-27 | `/v1/coord/admin/dns{,/class}` (POST/DELETE/PUT) + cli `dns --coord` · demo-lamed (Ep.5) |
| [048](ADR-048-coord-urlkey-admin.md) | URLKey kid registry on coord (rotate/list/remove) | Accepted · 2026-04-27 | `/v1/coord/admin/urlkey{,/rotate}` + cli `urlkey --coord` · demo-mem (Ep.6) |

> Hebrew letters continue (chet = ח, 8th letter).

## 미작성 ADR (예상)

- Season 5 Ep.2~5 의 별도 ADR 필요 시 추가 (현재로 ADR-015 이 마스터 결정).

작성 시점: 해당 기능 설계 직전.
