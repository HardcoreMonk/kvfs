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

## 목록 (Season 1 MVP)

| # | 제목 | 상태 | 연결 |
|---|---|---|---|
| [001](ADR-001-independent-identity.md) | 독자적 프로젝트 identity | Accepted | — |
| [002](ADR-002-two-daemon-architecture.md) | 2-daemon MVP (edge + dn) | Accepted | `docs/ARCHITECTURE.md` §컴포넌트 |
| [003](ADR-003-http-rest-protocol.md) | 내·외부 통신 HTTP REST | Accepted | — |
| [004](ADR-004-bbolt-metadata.md) | 메타데이터 저장 bbolt | Accepted | `internal/store/` |
| [005](ADR-005-content-addressable-chunks.md) | Content-addressable sha256 chunks | Accepted | `internal/dn/` `internal/edge/` |
| [006](ADR-006-mvp-single-chunk.md) | MVP: 1 object = 1 chunk | Accepted | chunking Season 2 |
| [007](ADR-007-urlkey-hmac-sha256.md) | UrlKey = HMAC-SHA256 | Accepted | `internal/urlkey/` |

## Season 2

| # | 제목 | 상태 | 연결 |
|---|---|---|---|
| [009](ADR-009-consistent-hashing.md) | Consistent Hashing (Rendezvous/HRW) | Accepted · 2026-04-25 | `internal/placement/` |
| [010](ADR-010-rebalance-worker.md) | Rebalance worker (copy-then-update, no-delete MVP) | Accepted · 2026-04-25 | `internal/rebalance/` |
| [012](ADR-012-surplus-gc.md) | Surplus chunk GC (claimed-set + min-age 안전망) | Accepted · 2026-04-26 | `internal/gc/` |
| [011](ADR-011-chunking.md) | Chunking (고정 크기, ADR-006 supersede) | Accepted · 2026-04-26 | `internal/chunker/` |
| [008](ADR-008-reed-solomon-ec.md) | Reed-Solomon EC (from-scratch GF(2^8)) | Accepted · 2026-04-26 | `internal/reedsolomon/` |

## Season 3 (in progress)

| # | 제목 | 상태 | 연결 |
|---|---|---|---|
| [013](ADR-013-auto-trigger.md) | Auto-trigger policy (in-edge ticker, opt-in) | Accepted · 2026-04-26 | `internal/edge/` auto loops |
| [024](ADR-024-ec-stripe-rebalance.md) | EC stripe rebalance (set-based, min-migration) | Accepted · 2026-04-26 | `internal/rebalance/` 확장 |
| [027](ADR-027-dynamic-dn-registry.md) | Dynamic DN registry (admin endpoint + bbolt 영속) | Accepted · 2026-04-26 | `internal/coordinator/` UpdateNodes + `dns_runtime` 버킷 |
| [028](ADR-028-urlkey-rotation.md) | UrlKey kid rotation (multi-key Signer + bbolt 영속) | Accepted · 2026-04-26 | `internal/urlkey/` multi-key + `urlkey_secrets` 버킷 |
| [029](ADR-029-optional-tls.md) | Optional TLS / mTLS (env-driven opt-in) | Accepted · 2026-04-26 | `cmd/kvfs-{edge,dn}/` TLS env + `scripts/gen-tls-certs.sh` |
| [025](ADR-025-ec-repair.md) | EC repair queue (Reed-Solomon Reconstruct K survivors) | Accepted · 2026-04-26 | `internal/repair/` + `kvfs-cli repair` |
| [014](ADR-014-meta-backup.md) | Metadata backup (snapshot + offline restore) | Accepted · 2026-04-26 | `internal/store/snapshot.go` + `kvfs-cli meta` |
| [030](ADR-030-dn-heartbeat.md) | DN heartbeat (in-edge pull-based liveness) | Accepted · 2026-04-26 | `internal/heartbeat/` + `kvfs-cli heartbeat` |
| [016](ADR-016-auto-snapshot-scheduler.md) | Auto-snapshot scheduler (ticker + retention) | Accepted · 2026-04-26 | `internal/store/scheduler.go` + `kvfs-cli meta history` |

## Season 3+ 예상 ADR (pending)
- ADR-015: Coordinator daemon 분리 + Raft HA
- ADR-017: Streaming PUT/GET (io.Reader 기반)
- ADR-018: Content-defined chunking
- ADR-019: WAL / incremental backup (분 단위 RPO, ADR-016 후속)
- ADR-022: Multi-edge leader election

작성 시점: 해당 기능 설계 직전.
