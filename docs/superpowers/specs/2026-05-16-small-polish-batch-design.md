# Small Polish Batch Design

## Purpose

Close two low-risk follow-up items that harden existing behavior without
changing public object-storage semantics:

- P6-12: bound the optional edge `CoordClient` lookup cache.
- P8-05: make quorum-loss chaos drift checks prove specific key presence and
  absence, not only object-count stability.

## Scope

In scope:

- Add a size cap to the opt-in `CoordClient` lookup cache used when
  `EDGE_COORD_LOOKUP_CACHE_TTL` is set.
- Keep cache behavior disabled by default.
- Preserve same-client invalidation on `CommitObject` and `DeleteObject`.
- Add focused unit coverage for cap-driven eviction and expired-entry cleanup.
- Strengthen `scripts/chaos-coord-quorum-loss.sh` Phase D so it validates that
  Phase-A keys remain present on the survivor and Phase-C keys remain absent
  while quorum is down.
- Update `docs/FOLLOWUP.md` after verification.

Out of scope:

- Enabling the lookup cache by default.
- Cross-edge cache invalidation.
- New cache metrics.
- Reworking the demo helper library beyond what P6-12 and P8-05 need.
- Adding new chaos scenarios for WAL corruption or disk-full behavior. Those
  remain P8-07.
- Anti-entropy algorithm changes. Those remain P8-17.

## Domain Architecture

`CoordClient` is the edge-side HTTP client for coord metadata and placement
RPCs. Its lookup cache is an edge-local, read-through optimization for
`LookupObject(bucket, key)`. The authoritative metadata owner remains coord.

The cache contract after this change:

- Disabled when TTL is zero.
- Enabled by `SetLookupCache(ttl)` with an effectively unbounded compatibility
  cap, matching current behavior unless callers opt into a stricter cap.
- Enabled by `SetLookupCacheWithLimit(ttl, maxEntries)` when callers need a
  memory bound.
- Stores at most `maxEntries` entries when `maxEntries > 0`.
- Evicts the least recently used entry when inserting over cap.
- Treats cache hits as use, so a hot key is less likely to be evicted.
- Drops expired entries opportunistically during reads and inserts.

`chaos-coord-quorum-loss.sh` is an executable safety test for ADR-040
transactional commit. Phase D currently proves that the survivor's object count
does not change during quorum loss. The strengthened contract additionally
checks object identity:

- Every Phase-A key seeded before quorum loss is still visible in the survivor's
  direct coord admin object list.
- Every Phase-C key attempted during quorum loss is absent from that same list
  while quorum is still down.

## Design

### P6-12 CoordClient Cache Cap

Extend `CoordClient` with small cache bookkeeping:

- `cacheMaxEntries int`
- `cacheTick uint64`
- `lastUsed uint64` on each cached entry

The cache remains map-backed because the expected cap is small and this avoids a
new container dependency. On insert:

1. Remove expired entries.
2. Insert or update the requested key.
3. If `cacheMaxEntries > 0` and the map is still over cap, scan for the lowest
   `lastUsed` value and delete that entry.

This is O(n) on insertion only. That is acceptable for a 10k default-style cap
and keeps the implementation easy to audit. `LookupObject` is still dominated
by HTTP I/O on misses.

Command wiring should add `EDGE_COORD_LOOKUP_CACHE_MAX_ENTRIES` and a matching
flag. It only matters when `EDGE_COORD_LOOKUP_CACHE_TTL` enables the cache. A
zero value keeps the existing unbounded behavior for compatibility, while docs
can recommend a bounded value for real workloads.

### P8-05 Chaos Drift Precision

Add a helper to `scripts/chaos-coord-quorum-loss.sh` that fetches
`/v1/coord/admin/objects` from the survivor and evaluates the raw JSON array
with `jq`.

Phase D should report three facts:

- count matches the Phase-A baseline;
- Phase-A keys are present;
- Phase-C keys are absent.

The summary should fail the script if any of these checks fail. Phase F remains
as the end-to-end edge-visible phantom check after restore.

## Error Handling

Cache configuration errors should fail edge startup before serving traffic:

- invalid TTL keeps the existing `EDGE_COORD_LOOKUP_CACHE_TTL parse` fatal path;
- invalid max-entry value should use the same fatal style;
- negative max-entry values are invalid;
- max-entry values without TTL are allowed but inert, avoiding surprising
  startup failures for unused env.

Chaos script failures should print enough context for operators:

- survivor count baseline and during values;
- first few missing Phase-A keys;
- first few unexpected Phase-C keys;
- existing coord/edge log tails on failure.

## Testing

Focused tests:

- `go test ./internal/edge`
  - existing cache hit/invalidate behavior remains green;
  - new cap test proves least-recently-used eviction;
  - new expiration test proves expired entries are removed before cap eviction.
- `go test ./cmd/kvfs-edge` or `go test ./...` verifies command wiring builds.
- `bash -n scripts/chaos-coord-quorum-loss.sh` verifies script syntax.

Full verification for the batch:

- focused Go tests first;
- `go test ./...`;
- `go vet ./...`;
- `./scripts/check-doc-drift.sh`;
- `git diff --check`;
- stale-marker scan from `AGENTS.md`;
- `git status --short --branch`.

Running the full chaos script is useful but may be expensive because it rebuilds
Docker images and manipulates containers. If skipped, record the reason and keep
the syntax check plus unit tests as the minimum verification.

## Documentation

Update `docs/FOLLOWUP.md` only after implementation and verification:

- mark P6-12 complete with the chosen cache contract;
- mark P8-05 complete with the new Phase D identity checks;
- add one compressed entry to the completed work table;
- update the current-status low-priority residual list.

No ADR is required because this is a follow-up hardening patch for previously
accepted behavior, not a new architecture decision.

## Success Criteria

- The lookup cache can be bounded without changing default behavior.
- LRU behavior is covered by deterministic unit tests.
- Same-client commit/delete invalidation still works.
- Quorum-loss Phase D catches identity drift, not just count drift.
- Documentation no longer lists P6-12 or P8-05 as open after verification.
