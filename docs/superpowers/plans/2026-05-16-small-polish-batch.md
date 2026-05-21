# Small Polish Batch Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close P6-12 and P8-05 by bounding the optional edge coord lookup cache and making quorum-loss chaos drift checks validate object identity.

**Architecture:** Keep coord as the metadata source of truth. The edge lookup cache remains an opt-in local optimization, now with an optional max-entry cap and LRU eviction. The quorum-loss chaos script keeps its existing count invariant and adds direct admin-object identity checks while quorum is still down.

**Tech Stack:** Go 1.26 standard library, existing `internal/edge` and `cmd/kvfs-edge` wiring, Bash, `jq`, existing documentation drift checks.

---

## Scope Check

This plan implements only:

- P6-12 `CoordClient` lookup cache size cap and LRU eviction.
- P8-05 `chaos-coord-quorum-loss.sh` Phase D identity checks.
- Documentation updates required by the new env var and closed follow-up items.

It does not implement P6-08, P8-07, P8-17, S3 compatibility, or anti-entropy algorithm changes.

## File Structure

- Modify `internal/edge/coord_client.go`: add `SetLookupCacheWithLimit`, cache usage ticks, expired-entry cleanup, and LRU eviction.
- Modify `internal/edge/coord_client_test.go`: add deterministic cache cap/LRU and expiry cleanup tests.
- Modify `cmd/kvfs-edge/main.go`: add `EDGE_COORD_LOOKUP_CACHE_MAX_ENTRIES` / `--coord-lookup-cache-max-entries` parsing and pass the cap into `CoordClient`.
- Modify `scripts/chaos-coord-quorum-loss.sh`: fetch survivor object JSON once in Phase D and validate Phase-A present / Phase-C absent by `(bucket,key)`.
- Modify `README.md`, `docs/GUIDE.md`, and `docs/guide.html`: document the new env var.
- Modify `docs/FOLLOWUP.md`: mark P6-12 and P8-05 complete after verification.

### Task 1: CoordClient Cache Cap Tests

**Files:**
- Modify: `internal/edge/coord_client_test.go`

- [ ] **Step 1: Write failing LRU and expiry tests**

Append these tests after `TestCoordClient_LookupCache_HitInvalidate` in `internal/edge/coord_client_test.go`:

```go
func TestCoordClient_LookupCache_MaxEntriesEvictsLeastRecentlyUsed(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "coord.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	for _, key := range []string{"k1", "k2", "k3"} {
		if err := st.PutObject(&store.ObjectMeta{Bucket: "b", Key: key, Size: 1}); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}

	var lookupHits int32
	cs := &coord.Server{Store: st, Placer: placement.New([]placement.Node{{ID: "dn1", Addr: "dn1"}})}
	mux := cs.Routes()
	wrapped := http.NewServeMux()
	wrapped.HandleFunc("GET /v1/coord/lookup", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&lookupHits, 1)
		mux.ServeHTTP(w, r)
	})
	wrapped.HandleFunc("POST /v1/coord/commit", func(w http.ResponseWriter, r *http.Request) { mux.ServeHTTP(w, r) })
	wrapped.HandleFunc("POST /v1/coord/delete", func(w http.ResponseWriter, r *http.Request) { mux.ServeHTTP(w, r) })
	wrapped.HandleFunc("GET /v1/coord/healthz", func(w http.ResponseWriter, r *http.Request) { mux.ServeHTTP(w, r) })
	ts := httptest.NewServer(wrapped)
	defer ts.Close()

	cc := NewCoordClient(ts.URL)
	cc.SetLookupCacheWithLimit(10*time.Second, 2)
	ctx := context.Background()

	for _, key := range []string{"k1", "k2"} {
		if _, err := cc.LookupObject(ctx, "b", key); err != nil {
			t.Fatalf("lookup %s: %v", key, err)
		}
	}
	if got := atomic.LoadInt32(&lookupHits); got != 2 {
		t.Fatalf("initial coord hits = %d, want 2", got)
	}

	if _, err := cc.LookupObject(ctx, "b", "k1"); err != nil {
		t.Fatalf("lookup k1 cached: %v", err)
	}
	if got := atomic.LoadInt32(&lookupHits); got != 2 {
		t.Fatalf("k1 cache hit should not call coord, hits = %d", got)
	}

	if _, err := cc.LookupObject(ctx, "b", "k3"); err != nil {
		t.Fatalf("lookup k3: %v", err)
	}
	if got := atomic.LoadInt32(&lookupHits); got != 3 {
		t.Fatalf("k3 miss should call coord once, hits = %d", got)
	}

	if _, err := cc.LookupObject(ctx, "b", "k1"); err != nil {
		t.Fatalf("lookup k1 after eviction: %v", err)
	}
	if got := atomic.LoadInt32(&lookupHits); got != 3 {
		t.Fatalf("recent k1 should remain cached, hits = %d", got)
	}

	if _, err := cc.LookupObject(ctx, "b", "k2"); err != nil {
		t.Fatalf("lookup k2 after eviction: %v", err)
	}
	if got := atomic.LoadInt32(&lookupHits); got != 4 {
		t.Fatalf("least-recent k2 should be evicted and fetched, hits = %d", got)
	}
}

func TestCoordClient_LookupCache_ExpiredEntriesPurgedBeforeCap(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "coord.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	for _, key := range []string{"old1", "old2", "fresh"} {
		if err := st.PutObject(&store.ObjectMeta{Bucket: "b", Key: key, Size: 1}); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}

	cs := &coord.Server{Store: st, Placer: placement.New([]placement.Node{{ID: "dn1", Addr: "dn1"}})}
	ts := httptest.NewServer(cs.Routes())
	defer ts.Close()

	cc := NewCoordClient(ts.URL)
	cc.SetLookupCacheWithLimit(30*time.Millisecond, 2)
	ctx := context.Background()
	if _, err := cc.LookupObject(ctx, "b", "old1"); err != nil {
		t.Fatalf("lookup old1: %v", err)
	}
	if _, err := cc.LookupObject(ctx, "b", "old2"); err != nil {
		t.Fatalf("lookup old2: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	if _, err := cc.LookupObject(ctx, "b", "fresh"); err != nil {
		t.Fatalf("lookup fresh: %v", err)
	}

	cc.mu.RLock()
	defer cc.mu.RUnlock()
	if got := len(cc.cache); got != 1 {
		t.Fatalf("cache entries after expiry purge = %d, want 1", got)
	}
	if _, ok := cc.cache[cc.cacheKey("b", "fresh")]; !ok {
		t.Fatalf("fresh entry missing after expiry purge: %#v", cc.cache)
	}
}
```

- [ ] **Step 2: Run the tests and verify they fail for the expected reason**

Run:

```bash
go test ./internal/edge -run 'TestCoordClient_LookupCache_(MaxEntriesEvictsLeastRecentlyUsed|ExpiredEntriesPurgedBeforeCap)' -count=1
```

Expected: FAIL because `SetLookupCacheWithLimit` is not defined.

- [ ] **Step 3: Commit nothing yet**

Do not commit the failing tests alone. Continue to Task 2 and commit tests plus implementation together after green.

### Task 2: CoordClient Cache Cap Implementation

**Files:**
- Modify: `internal/edge/coord_client.go`

- [ ] **Step 1: Add cache cap fields**

In `CoordClient`, replace the current cache field block:

```go
	cacheTTL time.Duration
	cache    map[string]cachedMeta
```

with:

```go
	cacheTTL        time.Duration
	cacheMaxEntries int
	cacheTick       uint64
	cache           map[string]cachedMeta
```

Replace `cachedMeta` with:

```go
type cachedMeta struct {
	meta     *store.ObjectMeta
	expiry   time.Time
	lastUsed uint64
}
```

- [ ] **Step 2: Replace cache setup with limited setup**

Replace `SetLookupCache` with:

```go
// SetLookupCache enables/disables the per-(bucket,key) read-through cache
// with no size cap, preserving the P6-10 compatibility behavior.
func (c *CoordClient) SetLookupCache(ttl time.Duration) {
	c.SetLookupCacheWithLimit(ttl, 0)
}

// SetLookupCacheWithLimit enables/disables the per-(bucket,key) read-through
// cache and optionally bounds it. maxEntries <= 0 means unbounded.
func (c *CoordClient) SetLookupCacheWithLimit(ttl time.Duration, maxEntries int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cacheTTL = ttl
	c.cacheMaxEntries = maxEntries
	c.cacheTick = 0
	if ttl > 0 {
		c.cache = make(map[string]cachedMeta)
	} else {
		c.cache = nil
		c.cacheMaxEntries = 0
	}
}
```

- [ ] **Step 3: Replace cache get/put internals**

Replace `cacheGet` and `cachePut` with:

```go
func (c *CoordClient) nextCacheTickLocked() uint64 {
	c.cacheTick++
	return c.cacheTick
}

func (c *CoordClient) cacheGet(bucket, key string) (*store.ObjectMeta, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache == nil {
		return nil, false
	}
	ck := c.cacheKey(bucket, key)
	e, ok := c.cache[ck]
	if !ok {
		return nil, false
	}
	if !time.Now().Before(e.expiry) {
		delete(c.cache, ck)
		return nil, false
	}
	e.lastUsed = c.nextCacheTickLocked()
	c.cache[ck] = e
	return e.meta, true
}

func (c *CoordClient) cachePut(bucket, key string, meta *store.ObjectMeta) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache == nil {
		return
	}
	now := time.Now()
	c.cacheRemoveExpiredLocked(now)
	c.cache[c.cacheKey(bucket, key)] = cachedMeta{
		meta:     meta,
		expiry:   now.Add(c.cacheTTL),
		lastUsed: c.nextCacheTickLocked(),
	}
	c.cacheEnforceLimitLocked()
}
```

Add these helper functions below `cachePut`:

```go
func (c *CoordClient) cacheRemoveExpiredLocked(now time.Time) {
	for k, e := range c.cache {
		if !now.Before(e.expiry) {
			delete(c.cache, k)
		}
	}
}

func (c *CoordClient) cacheEnforceLimitLocked() {
	if c.cacheMaxEntries <= 0 {
		return
	}
	for len(c.cache) > c.cacheMaxEntries {
		var evictKey string
		var evictTick uint64
		first := true
		for k, e := range c.cache {
			if first || e.lastUsed < evictTick {
				first = false
				evictKey = k
				evictTick = e.lastUsed
			}
		}
		if evictKey == "" {
			return
		}
		delete(c.cache, evictKey)
	}
}
```

- [ ] **Step 4: Run focused tests**

Run:

```bash
gofmt -w internal/edge/coord_client.go internal/edge/coord_client_test.go
go test ./internal/edge -run 'TestCoordClient_LookupCache' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit cache implementation**

Run:

```bash
git add internal/edge/coord_client.go internal/edge/coord_client_test.go
git commit -m "fix: bound coord lookup cache"
```

### Task 3: Edge Env Wiring For Cache Cap

**Files:**
- Modify: `cmd/kvfs-edge/main.go`

- [ ] **Step 1: Add the max-entry flag and parse failure path**

Add `strconv` to the import list:

```go
	"strconv"
```

Add this flag immediately after `flagLookupCache`:

```go
		flagLookupCacheMax = flag.String("coord-lookup-cache-max-entries", envOr("EDGE_COORD_LOOKUP_CACHE_MAX_ENTRIES", "0"), "P6-12: max entries for CoordClient lookup cache. 0 = unbounded; only used when coord lookup cache TTL is enabled.")
```

In the `if *flagCoordURL != ""` block, replace the P6-10 cache section with:

```go
		lookupCacheMax, perr := strconv.Atoi(*flagLookupCacheMax)
		if perr != nil {
			fatal("EDGE_COORD_LOOKUP_CACHE_MAX_ENTRIES parse: " + perr.Error())
		}
		if lookupCacheMax < 0 {
			fatal("EDGE_COORD_LOOKUP_CACHE_MAX_ENTRIES must be >= 0")
		}
		// P6-10/P6-12 opt-in cache.
		if *flagLookupCache != "" {
			ttl, perr := time.ParseDuration(*flagLookupCache)
			if perr != nil {
				fatal("EDGE_COORD_LOOKUP_CACHE_TTL parse: " + perr.Error())
			}
			coordClient.SetLookupCacheWithLimit(ttl, lookupCacheMax)
			log.Info("coord lookup cache enabled (P6-10/P6-12)", "ttl", ttl, "max_entries", lookupCacheMax)
		}
```

- [ ] **Step 2: Run build-focused tests**

Run:

```bash
gofmt -w cmd/kvfs-edge/main.go
go test ./cmd/kvfs-edge ./internal/edge
```

Expected: PASS.

- [ ] **Step 3: Commit env wiring**

Run:

```bash
git add cmd/kvfs-edge/main.go
git commit -m "feat: wire coord lookup cache cap"
```

### Task 4: Quorum-Loss Phase D Identity Checks

**Files:**
- Modify: `scripts/chaos-coord-quorum-loss.sh`

- [ ] **Step 1: Demonstrate the current count-only blind spot**

Run this one-off diagnostic:

```bash
jq -r 'length' <<'JSON'
[
  {"bucket":"quorum","key":"phase-a-1"},
  {"bucket":"quorum","key":"phase-c-1"}
]
JSON
```

Expected: prints `2`. This shows why count equality cannot prove the Phase-A set is intact or the Phase-C set is absent.

- [ ] **Step 2: Add JSON helpers**

Replace the current `coord_object_count` helper with:

```bash
coord_objects_json() {
  local coord_port="$1"
  curl -fsS --max-time 5 "http://localhost:${coord_port}/v1/coord/admin/objects"
}

coord_object_count() {
  local coord_port="$1"
  coord_objects_json "$coord_port" 2>/dev/null \
    | jq -r 'length' 2>/dev/null \
    || echo "ERROR"
}

object_json_has_key() {
  local json="$1" bucket="$2" key="$3"
  jq -e --arg bucket "$bucket" --arg key "$key" \
    'any(.[]; .bucket == $bucket and .key == $key)' \
    >/dev/null <<<"$json"
}
```

- [ ] **Step 3: Replace Phase D count-only logic**

Replace the Phase D block, from `DURING=$(coord_object_count "$SURVIVOR_PORT")` through the `DRIFT_FAIL` assignment, with:

```bash
DURING_JSON=$(coord_objects_json "$SURVIVOR_PORT" 2>/dev/null || true)
DURING=$(jq -r 'length' <<<"$DURING_JSON" 2>/dev/null || echo "ERROR")
echo "    survivor count: baseline=${BASELINE} during=${DURING}"
DRIFT_FAIL=0
IDENTITY_FAIL=0
if [ "$DURING" != "$BASELINE" ]; then
  DRIFT_FAIL=1
  echo "    ❌ DRIFT: survivor bbolt mutated during quorum loss → phantom commit"
fi
if [ "$DURING" = "ERROR" ] || [ -z "$DURING_JSON" ]; then
  IDENTITY_FAIL=1
  echo "    ❌ IDENTITY: could not read survivor object JSON"
else
  A_MISSING=0
  for entry in "${PHASE_A_KEYS[@]}"; do
    K="${entry%%|*}"
    if ! object_json_has_key "$DURING_JSON" "quorum" "$K"; then
      A_MISSING=$((A_MISSING + 1))
      [ "$A_MISSING" -le 3 ] && echo "    ❌ missing phase-A key on survivor: ${K}"
    fi
  done
  C_PRESENT=0
  for entry in "${PHASE_C_KEYS[@]}"; do
    K="${entry%%|*}"
    if object_json_has_key "$DURING_JSON" "quorum" "$K"; then
      C_PRESENT=$((C_PRESENT + 1))
      [ "$C_PRESENT" -le 3 ] && echo "    ❌ phase-C key present during quorum loss: ${K}"
    fi
  done
  if [ "$A_MISSING" -gt 0 ] || [ "$C_PRESENT" -gt 0 ]; then
    IDENTITY_FAIL=1
  fi
  echo "    identity: phase-A missing=${A_MISSING}, phase-C present=${C_PRESENT}"
fi
```

- [ ] **Step 4: Update summary and exit gate**

In the summary, add this line after the survivor drift line:

```bash
echo "   phase D (survivor identity):   $([ "$IDENTITY_FAIL" -eq 0 ] && echo 'phase-A present, phase-C absent' || echo 'IDENTITY DRIFT')"
```

Add this exit gate after the existing `DRIFT_FAIL` gate:

```bash
if [ "$IDENTITY_FAIL" -ne 0 ]; then
  echo "   ❌ ADR-040 VIOLATION: survivor object identity drifted during quorum loss"
  EXIT_CODE=1
fi
```

- [ ] **Step 5: Run script syntax check**

Run:

```bash
bash -n scripts/chaos-coord-quorum-loss.sh
```

Expected: PASS with no output.

- [ ] **Step 6: Commit script hardening**

Run:

```bash
git add scripts/chaos-coord-quorum-loss.sh
git commit -m "test: tighten quorum-loss drift check"
```

### Task 5: Documentation Updates

**Files:**
- Modify: `README.md`
- Modify: `docs/GUIDE.md`
- Modify: `docs/guide.html`
- Modify: `docs/FOLLOWUP.md`

- [ ] **Step 1: Update README env table**

In `README.md`, add this row immediately after `EDGE_COORD_LOOKUP_CACHE_TTL`:

```markdown
| `EDGE_COORD_LOOKUP_CACHE_MAX_ENTRIES` | 0 | — | P6-12 cache cap. `0` = unbounded; only applies when `EDGE_COORD_LOOKUP_CACHE_TTL` enables cache |
```

- [ ] **Step 2: Update GUIDE env table**

In `docs/GUIDE.md`, add this row immediately after `EDGE_COORD_LOOKUP_CACHE_TTL`:

```markdown
| `EDGE_COORD_LOOKUP_CACHE_MAX_ENTRIES` | 0 | P6-12: coord lookup cache entry cap. `0` = unbounded; TTL 이 켜졌을 때만 적용 |
```

- [ ] **Step 3: Mirror GUIDE env row into HTML**

In `docs/guide.html`, add this table row immediately after the `EDGE_COORD_LOOKUP_CACHE_TTL` row:

```html
<tr><td><code>EDGE_COORD_LOOKUP_CACHE_MAX_ENTRIES</code></td><td>0</td><td>P6-12: coord lookup cache entry cap. <code>0</code> = unbounded; TTL 이 켜졌을 때만 적용</td></tr>
```

- [ ] **Step 4: Update FOLLOWUP status**

In `docs/FOLLOWUP.md`:

- Replace the P6 priority-map line with:

```markdown
- **P6**: Season 5 (coord 분리) core — 완료. P6-08 helper polish 저우선 잔존
```

- Replace the P8 priority-map line with:

```markdown
- **P8**: Frame-1+2 100% wave — P8-01·02·03·04·05·06·08~16 DONE (Frame 1+2 = 100% + self-heal coverage 100% + operational polish + concurrent EC repair + replication concurrent + persistent scrubber + unrecoverable signal + continuous self-heal + Prometheus surface + observability completions + quorum-loss drift precision), P8-07·17 (한계효용 polish, 저우선) 잔존
```

- Replace the current-status residual line with:

```markdown
- **시즌**: S1·S2·S3·S4 closed. S5 closed (Ep.1~7). S6 Ep.1~7 done. P9 production MVP track opened with ADR-064. 저우선 잔존: P6-08, P8-07, P8-17
```

- Replace the open P6-12 section with a completed section:

```markdown
### ~~[P6-12] CoordClient 캐시 size cap / LRU eviction (저우선)~~
- **DONE 2026-05-16**: `CoordClient` lookup cache 에 optional
  max-entry cap + LRU eviction 추가. `SetLookupCacheWithLimit(ttl, maxEntries)`
  로 test/daemon wiring 이 cap 을 줄 수 있고, 기존 `SetLookupCache(ttl)` 는
  unbounded compatibility 동작 유지. `EDGE_COORD_LOOKUP_CACHE_MAX_ENTRIES`
  env/flag 추가 (`0` = unbounded).
```

- Replace the open P8-05 section with a completed section:

```markdown
### ~~[P8-05] Phase 1 chaos test 의 Phase D drift check 정확도 개선~~
- **DONE 2026-05-16**: `chaos-coord-quorum-loss.sh` Phase D 가
  count-only drift check 에서 survivor admin object JSON 기반 identity check
  로 확장됨. Quorum-loss 중 Phase-A key 존재와 Phase-C key 부재를 직접 검증.
```

- Add one compressed completed-work row:

```markdown
| 2026-05-16 | Low-priority polish close | P6-12 CoordClient lookup cache cap/LRU + P8-05 quorum-loss Phase D identity drift check 완료. |
```

- [ ] **Step 5: Run documentation checks**

Run:

```bash
./scripts/check-doc-drift.sh
git diff --check
stale_re='P4''-01|미''커밋|HE''AD [0-9a-f]|claude-''zone'
rg -n "$stale_re" -g '*.md' -g '*.html' -g '!AGENTS.md'
```

Expected:

- doc drift exits 0;
- diff check exits 0;
- stale-marker scan exits 1 with no matches.

- [ ] **Step 6: Commit docs**

Run:

```bash
git add README.md docs/GUIDE.md docs/guide.html docs/FOLLOWUP.md
git commit -m "docs: close cache and chaos polish followups"
```

### Task 6: Final Verification

**Files:**
- No planned source edits unless verification reveals a defect.

- [ ] **Step 1: Run focused tests**

Run:

```bash
go test ./internal/edge ./cmd/kvfs-edge
bash -n scripts/chaos-coord-quorum-loss.sh
```

Expected: PASS.

- [ ] **Step 2: Run full Go tests**

Run:

```bash
go test ./...
```

Expected: PASS.

- [ ] **Step 3: Run vet**

Run:

```bash
go vet ./...
```

Expected: PASS.

- [ ] **Step 4: Run documentation and worktree checks**

Run:

```bash
./scripts/check-doc-drift.sh
git diff --check
stale_re='P4''-01|미''커밋|HE''AD [0-9a-f]|claude-''zone'
rg -n "$stale_re" -g '*.md' -g '*.html' -g '!AGENTS.md'
git status --short --branch
```

Expected:

- doc drift exits 0;
- diff check exits 0;
- stale-marker scan exits 1 with no matches;
- status shows only expected local changes, or clean if all commits were made.

- [ ] **Step 5: Consider full chaos execution**

Run the full chaos scenario when Docker rebuild/container time is acceptable:

```bash
./scripts/chaos-coord-quorum-loss.sh --keys 3 --down-sec 6
```

Expected: PASS with Phase D summary reporting `phase-A present, phase-C absent`. If skipped, final handoff must state that the script was syntax-checked but not container-executed.

## Self-Review

- Spec coverage: P6-12 is covered by Tasks 1-3 and docs in Task 5. P8-05 is covered by Task 4 and docs in Task 5.
- Placeholder scan: no incomplete sections or undefined implementation decisions remain.
- Type consistency: `SetLookupCacheWithLimit(ttl time.Duration, maxEntries int)` is introduced in Task 2, used by tests in Task 1, and wired in Task 3.
- Verification coverage: focused tests, full tests, vet, doc drift, diff check, stale-marker scan, and chaos syntax/full-run paths are listed.
