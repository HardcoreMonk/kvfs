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
| [001](ADR-001-independent-identity.md) | 독자적 프로젝트 identity | Accepted | `NAMING.md` |
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
| [011](ADR-011-chunking.md) | Chunking (고정 크기, ADR-006 supersede) | Accepted · 2026-04-26 | `internal/chunker/` (후속) |

## Season 2+ 예상 ADR (pending)

- ADR-008: Reed-Solomon erasure coding 도입
- ADR-013: Auto-trigger policy (rebalance + GC 자동화)
- ADR-014: Meta backup/HA (bbolt 메타 손실 시 복구)
- ADR-015: Coordinator daemon 분리 + Raft HA
- ADR-016: gRPC 마이그레이션 (or 유지 결정)
- ADR-017: Streaming PUT/GET (io.Reader 기반)
- ADR-018: Content-defined chunking

작성 시점: 해당 기능 설계 직전.
