// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Anti-entropy worker (ADR-054, S7 Ep.4):
//
// For each DN in the live registry, builds the *expected* chunk_id set
// from coord's ObjectMeta (replicas + EC shards), fetches the DN's
// *actual* Merkle tree (256 buckets × bucket hash), and reports per-
// DN diffs: chunks that should be on the DN but are not (missing —
// repair candidate), and chunks the DN holds but coord doesn't track
// (extra — orphan, GC candidate; informational, since ADR-012's GC
// already handles the actual reclamation).
//
// Performance: O(B + ΔN) where B = 256 buckets and ΔN = chunks in
// diverging buckets. Healthy clusters with rare divergence pay almost
// only the 256-byte root + bucket hash compare. Single-tier Merkle is
// ample for kvfs scale; larger systems would extend to deeper trees.
//
// Repair: this ADR ships *detection* only. The diff report can drive
// existing repair workers (rebalance for replication, ADR-046 for EC)
// or future inline auto-repair — explicitly out of scope here so each
// step lands separately.
package coord

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HardcoreMonk/kvfs/internal/repair"
	"github.com/HardcoreMonk/kvfs/internal/store"
)

// AntiEntropyReport is the JSON body returned by
// POST /v1/coord/admin/anti-entropy/run. One entry per live DN.
type AntiEntropyReport struct {
	StartedAt time.Time         `json:"started_at"`
	Duration  string            `json:"duration"`
	DNs       []DNAuditEntry    `json:"dns"`
}

// DNAuditEntry: per-DN summary. RootMatch == true means the on-disk
// inventory exactly matches what coord expects (no missing, no extra).
type DNAuditEntry struct {
	DN              string   `json:"dn"`
	Reachable       bool     `json:"reachable"`
	ExpectedTotal   int      `json:"expected_total"`
	ActualTotal     int      `json:"actual_total"`
	RootMatch       bool     `json:"root_match"`
	MissingFromDN   []string `json:"missing_from_dn,omitempty"`
	ExtraOnDN       []string `json:"extra_on_dn,omitempty"`
	BucketsExamined int      `json:"buckets_examined"`
	Note            string   `json:"note,omitempty"`
}

// runAntiEntropy is the worker. Top-down structure:
//
//  1. Compute expected: for every ObjectMeta, walk replicas + EC shard
//     replicas to bucket each chunk_id by which DN it should live on.
//  2. For each live DN, fetch its actual Merkle (root + 256 buckets).
//     Compare per bucket; only fetch the chunks of buckets that
//     actually differ.
//  3. Diff per DN → DNAuditEntry. Bundle into AntiEntropyReport.
func (s *Server) runAntiEntropy(ctx context.Context) (*AntiEntropyReport, error) {
	startedAt := time.Now().UTC()

	// (1) Expected map: dn_addr → bucket idx (0..255) → sorted chunk_ids.
	expected, err := s.expectedChunksByDN(ctx)
	if err != nil {
		return nil, fmt.Errorf("compute expected: %w", err)
	}

	dns, err := s.Store.ListRuntimeDNs()
	if err != nil {
		return nil, fmt.Errorf("list runtime DNs: %w", err)
	}

	report := &AntiEntropyReport{StartedAt: startedAt}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, dn := range dns {
		wg.Add(1)
		go func(dnAddr string) {
			defer wg.Done()
			entry := s.auditOneDN(ctx, dnAddr, expected[dnAddr])
			mu.Lock()
			report.DNs = append(report.DNs, entry)
			mu.Unlock()
		}(dn)
	}
	wg.Wait()

	sort.Slice(report.DNs, func(i, j int) bool { return report.DNs[i].DN < report.DNs[j].DN })
	report.Duration = time.Since(startedAt).String()
	return report, nil
}

// expectedChunksByDN walks every object's metadata once and returns a
// nested map: dn_addr → bucket(0..255) → sorted chunk_ids that should
// live on that DN. Both replication and EC shards contribute. EC
// shard.Replicas[0] is treated as the "primary" target — this matches
// what handleGetEC reads from. Future ADR may model multi-replica
// shards explicitly.
func (s *Server) expectedChunksByDN(_ context.Context) (map[string]map[int][]string, error) {
	objs, err := s.Store.ListObjects()
	if err != nil {
		return nil, err
	}
	out := make(map[string]map[int][]string)
	add := func(dn, id string) {
		if dn == "" || !validHex64(id) {
			return
		}
		idx, perr := strconv.ParseInt(id[:2], 16, 32)
		if perr != nil {
			return
		}
		if out[dn] == nil {
			out[dn] = make(map[int][]string, 16)
		}
		out[dn][int(idx)] = append(out[dn][int(idx)], id)
	}
	for _, obj := range objs {
		// Replication: ChunkRef.Replicas — every DN holds the chunk.
		for _, ch := range obj.Chunks {
			for _, dn := range ch.Replicas {
				add(dn, ch.ChunkID)
			}
		}
		// EC: each Stripe has K+M Shards, each Shard has its own
		// Replicas. Walk all of them for completeness.
		for _, st := range obj.Stripes {
			for _, sh := range st.Shards {
				for _, dn := range sh.Replicas {
					add(dn, sh.ChunkID)
				}
			}
		}
	}
	for _, byBucket := range out {
		for idx := range byBucket {
			sort.Strings(byBucket[idx])
		}
	}
	return out, nil
}

// auditOneDN: fetch actual Merkle, compare bucket hashes vs expected,
// enumerate diverging buckets to enumerate the actual chunk set, then
// take symmetric difference.
func (s *Server) auditOneDN(ctx context.Context, dnAddr string, expBuckets map[int][]string) DNAuditEntry {
	entry := DNAuditEntry{DN: dnAddr}
	// expected total
	for _, ids := range expBuckets {
		entry.ExpectedTotal += len(ids)
	}

	rootResp, err := fetchDNMerkle(ctx, dnAddr)
	if err != nil {
		entry.Reachable = false
		entry.Note = "merkle fetch: " + err.Error()
		return entry
	}
	entry.Reachable = true
	entry.ActualTotal = rootResp.Total

	expRoot := computeExpectedRoot(expBuckets)
	if expRoot == rootResp.Root {
		entry.RootMatch = true
		return entry
	}

	// Bucket-level walk: only fetch chunks of buckets whose hashes
	// differ. Healthy single-DN drift typically touches one bucket so
	// the bandwidth saved by the Merkle round trip is real.
	for _, b := range rootResp.Buckets {
		expHash := bucketHash(expBuckets[b.Idx])
		if expHash == b.Hash {
			continue
		}
		entry.BucketsExamined++
		actual, ferr := fetchDNBucket(ctx, dnAddr, b.Idx)
		if ferr != nil {
			entry.Note = fmt.Sprintf("bucket %d fetch: %v", b.Idx, ferr)
			continue
		}
		exp := expBuckets[b.Idx]
		miss, extra := symmetricDiff(exp, actual)
		entry.MissingFromDN = append(entry.MissingFromDN, miss...)
		entry.ExtraOnDN = append(entry.ExtraOnDN, extra...)
	}
	sort.Strings(entry.MissingFromDN)
	sort.Strings(entry.ExtraOnDN)
	return entry
}

// computeExpectedRoot mirrors DN.computeMerkle's hash convention:
// per-bucket hash = sha256("\n"-joined sorted ids), root = sha256(concat
// of 256 bucket hashes in idx order). Empty bucket → sha256("").
func computeExpectedRoot(byBucket map[int][]string) string {
	rh := sha256.New()
	for i := 0; i < 256; i++ {
		bh := sha256.New()
		for _, id := range byBucket[i] {
			bh.Write([]byte(id))
			bh.Write([]byte{'\n'})
		}
		rh.Write(bh.Sum(nil))
	}
	return hex.EncodeToString(rh.Sum(nil))
}

func bucketHash(ids []string) string {
	bh := sha256.New()
	for _, id := range ids {
		bh.Write([]byte(id))
		bh.Write([]byte{'\n'})
	}
	return hex.EncodeToString(bh.Sum(nil))
}

// symmetricDiff returns (a − b, b − a). Both inputs are sorted; uses
// merge-style two-pointer walk for O(n+m).
func symmetricDiff(a, b []string) (onlyA, onlyB []string) {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			onlyA = append(onlyA, a[i])
			i++
		case a[i] > b[j]:
			onlyB = append(onlyB, b[j])
			j++
		default:
			i++
			j++
		}
	}
	onlyA = append(onlyA, a[i:]...)
	onlyB = append(onlyB, b[j:]...)
	return onlyA, onlyB
}

// fetchDNMerkle: GET http://<dn>/chunks/merkle.
func fetchDNMerkle(ctx context.Context, dnAddr string) (*dnMerkle, error) {
	url := "http://" + dnAddr + "/chunks/merkle"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out dnMerkle
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// fetchDNBucket: GET http://<dn>/chunks/merkle/bucket?idx=N.
func fetchDNBucket(ctx context.Context, dnAddr string, idx int) ([]string, error) {
	url := fmt.Sprintf("http://%s/chunks/merkle/bucket?idx=%d", dnAddr, idx)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var body struct {
		Chunks []string `json:"chunks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Chunks, nil
}

type dnMerkle struct {
	Root    string `json:"root"`
	Total   int    `json:"total"`
	Buckets []struct {
		Idx   int    `json:"idx"`
		Hash  string `json:"hash"`
		Count int    `json:"count"`
	} `json:"buckets"`
}

// validHex64 — same constraint as dn.validChunkID, inlined here to
// avoid importing internal/dn from internal/coord (which would create
// a layering inversion: dn shouldn't depend on coord, coord shouldn't
// depend on dn either).
func validHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// handleAntiEntropyRun serves POST /v1/coord/admin/anti-entropy/run.
// One-shot — returns the report when finished. For continuous audit
// the operator schedules the cli command externally (cron, k8s job)
// or sets COORD_ANTI_ENTROPY_INTERVAL on the coord daemon (ADR-055).
// Leader-only because it walks the authoritative ObjectMeta; followers
// would race on stale state.
func (s *Server) handleAntiEntropyRun(w http.ResponseWriter, r *http.Request) {
	if s.requireLeader(w) {
		return
	}
	rep, err := s.runAntiEntropy(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// ChunkRepairOutcome.Mode values. Centralised so a typo at any one
// call site can't silently produce a JSON shape divergence — the
// outcomes are part of the operator-facing API surface.
const (
	repairModeReplication = "replication"   // chunk copy from healthy replica
	repairModeECDeferred  = "ec-deferred"   // EC chunk found but ec=1 not set
	repairModeECInline    = "ec-inline"     // ADR-057 per-stripe reconstruct stub
	repairModeECSummary   = "ec-summary"    // synthetic per-call EC stats row
	repairModeThrottled   = "throttled"     // ADR-059 max_repairs cap reached
	repairModeNoSource    = "no-source"     // every owner unreachable / corrupt
	repairModeSkip        = "skip"          // target DN unreachable
)

// ChunkRepairOutcome.Reason values.
const (
	repairReasonMissing = "missing" // audit's MissingFromDN (ADR-054)
	repairReasonCorrupt = "corrupt" // scrubber's corrupt set (ADR-056)
)

// AntiEntropyRepairReport: result of POST /v1/coord/admin/anti-entropy/repair.
// Bundles the original audit + per-DN repair attempts (ADR-055).
// ADR-056 added: corrupt-chunk repair (sourced from /chunks/scrub-status)
// and DryRun mode (preview without copying bytes).
type AntiEntropyRepairReport struct {
	Audit     *AntiEntropyReport   `json:"audit"`
	DryRun    bool                 `json:"dry_run"`
	Repairs   []ChunkRepairOutcome `json:"repairs"`
	Skipped   []ChunkRepairOutcome `json:"skipped"` // EC + unreachable + no-source
	StartedAt time.Time            `json:"started_at"`
	Duration  string               `json:"duration"`
}

// ChunkRepairOutcome: one row per (dn, chunk_id) the worker tried to fix.
//
// Reason ("missing" or "corrupt") tells operator WHY repair was needed:
//   - missing: chunk file absent on the DN (inventory drift, ADR-054)
//   - corrupt: scrubber found sha mismatch on the DN's copy (bit-rot, ADR-054)
type ChunkRepairOutcome struct {
	TargetDN string `json:"target_dn"`
	ChunkID  string `json:"chunk_id"`
	SourceDN string `json:"source_dn,omitempty"` // empty if Skipped
	Mode     string `json:"mode"`                // "replication" or "ec-deferred"
	Reason   string `json:"reason"`              // "missing" | "corrupt"
	OK       bool   `json:"ok"`
	Planned  bool   `json:"planned,omitempty"`   // DryRun: would have run
	Err      string `json:"err,omitempty"`
}

// runAntiEntropyRepair: ADR-055. detection (runAntiEntropy) → for each
// `missing_from_dn` chunk on a DN, find another DN that holds it per
// ObjectMeta + per the audit's actual inventory, copy, write to target.
//
// Scope: replication-mode chunks only. EC stripes deliberately skipped
// because the proper recovery path is ADR-046 (Reed-Solomon Reconstruct
// + redistribute) — running it inline here would either duplicate logic
// or invoke the existing EC repair worker. Either way, EC is its own
// beast; we report skipped and let the operator run `repair --apply`
// separately. Same module of work, different worker.
//
// Requires Coord (DN-IO) — without it we have no way to read/write
// chunks from coord. Returns 503 at the handler level if missing.
//
// ADR-056 extends the original ADR-055 design with two flags packed
// into a single AntiEntropyRepairOpts struct:
//   - Corrupt: also fetch each DN's /chunks/scrub-status, treat
//     scrubber-flagged chunk_ids as repair candidates (overwrite the
//     bad bytes with a healthy replica's copy). DN's PUT handler
//     validates sha256 = chunk_id, so a healthy source guarantees the
//     overwrite restores correctness.
//   - DryRun: skip the actual ReadChunk/PutChunkTo, mark each outcome
//     Planned=true so operator can preview impact before committing.
//
// ADR-057 (P8-10) adds:
//   - EC: inline EC missing repair. Anti-entropy's per-shard missing
//     set is grouped by stripe, packed into repair.StripeRepair, and
//     handed to the existing ADR-046 repair.Run. Default off — when
//     unset, EC chunks remain "ec-deferred" (back-compat with ADR-055).
//
// ADR-059 (P8-12) adds:
//   - MaxRepairs: hard cap on total successful repairs per call. 0 =
//     unlimited (back-compat). Once the cap is hit subsequent
//     candidates land in Skipped with mode="throttled". Lets operators
//     run cautious, bounded repair sweeps on big clusters.
type AntiEntropyRepairOpts struct {
	Corrupt    bool
	DryRun     bool
	EC         bool
	MaxRepairs int
}

func (s *Server) runAntiEntropyRepair(ctx context.Context, opts AntiEntropyRepairOpts) (*AntiEntropyRepairReport, error) {
	startedAt := time.Now().UTC()
	audit, err := s.runAntiEntropy(ctx)
	if err != nil {
		return nil, err
	}
	out := &AntiEntropyRepairReport{Audit: audit, DryRun: opts.DryRun, StartedAt: startedAt}

	// Build chunk_id → []dn (which DNs are supposed to have it). Used
	// as the source candidate list per repair attempt.
	objs, err := s.Store.ListObjects()
	if err != nil {
		return nil, fmt.Errorf("list objects: %w", err)
	}
	chunkOwners := make(map[string][]string)
	ecChunks := make(map[string]struct{})
	for _, o := range objs {
		for _, c := range o.Chunks {
			chunkOwners[c.ChunkID] = append(chunkOwners[c.ChunkID], c.Replicas...)
		}
		for _, st := range o.Stripes {
			for _, sh := range st.Shards {
				ecChunks[sh.ChunkID] = struct{}{}
				chunkOwners[sh.ChunkID] = append(chunkOwners[sh.ChunkID], sh.Replicas...)
			}
		}
	}

	// Build "actually has" map: dn → set of chunk_ids it actually holds.
	// Derived from the audit's per-DN ExpectedTotal/ActualTotal isn't
	// granular enough — we use audit's MissingFromDN inversely (a chunk
	// missing on dn means it's NOT actually there) plus assume the rest
	// of expected[dn] IS there (audit confirms via root_match or by not
	// listing in MissingFromDN). For exhaustive correctness we'd re-read
	// each DN's full bucket of any divergent slot; the audit already did
	// that and recorded missing.
	dnHas := make(map[string]map[string]struct{})
	for _, e := range audit.DNs {
		if !e.Reachable {
			continue
		}
		// Start with "everything expected" then subtract MissingFromDN.
		miss := make(map[string]struct{}, len(e.MissingFromDN))
		for _, m := range e.MissingFromDN {
			miss[m] = struct{}{}
		}
		dnHas[e.DN] = miss // we'll invert during lookup
	}

	// ADR-056: also gather scrubber-detected corrupt chunks per DN. We
	// fetch /chunks/scrub-status from each reachable DN and treat its
	// `corrupt` ids as additional repair candidates. The audit DOESN'T
	// see these (the file is on disk so MissingFromDN doesn't list it),
	// only the scrubber knows the bytes are wrong.
	corruptByDN := make(map[string][]string)
	if opts.Corrupt {
		corruptByDN = s.collectCorruptByDN(ctx, audit.DNs)
	}

	// ADR-057: build per-(bucket, key, stripe_idx) → StripeRepair when
	// EC inline mode is on. Groups all missing shards of the same stripe
	// together so repair.Run reconstructs once per stripe, not once per
	// shard. Uses the SAME chunkID-keyed lookup as the rest of the audit.
	//
	// ADR-058 (this commit): when opts.Corrupt is also set, scrubber-
	// detected corrupt EC shards join the DeadShards list with Force=true.
	// repair.repairStripe routes those to PutChunkToForce so the corrupt
	// on-disk file gets overwritten instead of skipped by the idempotent
	// PUT path. Survivor selection also excludes shards on DNs whose
	// scrubber flagged the same chunk_id as corrupt — we won't read from
	// a known-bad source.
	type ecKey struct{ Bucket, Key string; Stripe int }
	ecPlanByKey := make(map[ecKey]*repair.StripeRepair)
	if opts.EC {
		for _, o := range objs {
			if !o.IsEC() {
				continue
			}
			for si, st := range o.Stripes {
				k, m := o.EC.K, o.EC.M
				key := ecKey{o.Bucket, o.Key, si}
				rep := &repair.StripeRepair{
					Bucket: o.Bucket, Key: o.Key, StripeIndex: si,
					K: k, M: m,
				}
				for shi, sh := range st.Shards {
					if len(sh.Replicas) == 0 {
						continue
					}
					addr := sh.Replicas[0]
					missingHere := false
					if dnHas[addr] != nil {
						if _, gone := dnHas[addr][sh.ChunkID]; gone {
							missingHere = true
						}
					}
					corruptHere := opts.Corrupt && hasCorrupt(corruptByDN[addr], sh.ChunkID)
					switch {
					case missingHere:
						rep.DeadShards = append(rep.DeadShards, repair.DeadShard{
							ShardIndex: shi, ChunkID: sh.ChunkID,
							OldAddr: addr, NewAddr: addr,
							Size: sh.Size,
						})
					case corruptHere:
						rep.DeadShards = append(rep.DeadShards, repair.DeadShard{
							ShardIndex: shi, ChunkID: sh.ChunkID,
							OldAddr: addr, NewAddr: addr,
							Size:  sh.Size,
							Force: true, // ADR-058: bytes wrong on disk, must overwrite
						})
					case dnHas[addr] != nil:
						// Survivor candidate. Exclude if scrubber on its DN
						// reported THIS chunk as corrupt — the bytes there are
						// wrong, can't trust as a Reconstruct source.
						if !hasCorrupt(corruptByDN[addr], sh.ChunkID) {
							rep.Survivors = append(rep.Survivors, repair.SurvivorRef{
								ShardIndex: shi, ChunkID: sh.ChunkID, Addr: addr,
							})
						}
					}
				}
				if len(rep.DeadShards) > 0 {
					ecPlanByKey[key] = rep
				}
			}
		}
	}

	successCount := 0
	throttled := func() bool {
		return opts.MaxRepairs > 0 && successCount >= opts.MaxRepairs
	}

	repairOne := func(targetDN, chunkID, reason string) {
		if throttled() {
			out.Skipped = append(out.Skipped, ChunkRepairOutcome{
				TargetDN: targetDN, ChunkID: chunkID, Reason: reason,
				Mode: repairModeThrottled,
				Err:  fmt.Sprintf("max_repairs=%d limit reached (ADR-059)", opts.MaxRepairs),
			})
			return
		}
		if _, isEC := ecChunks[chunkID]; isEC {
			if !opts.EC {
				out.Skipped = append(out.Skipped, ChunkRepairOutcome{
					TargetDN: targetDN, ChunkID: chunkID, Reason: reason,
					Mode: repairModeECDeferred,
					Err:  "EC chunk — pass ec=1 to inline-repair (ADR-057), or use kvfs-cli repair --apply --coord (ADR-046)",
				})
				return
			}
			// EC reconstruct happens in the per-stripe pass below; this
			// stub lets the caller see the chunk was claimed. The pass
			// flips OK / Err once the stripe's repair.Run returns.
			oc := ChunkRepairOutcome{
				TargetDN: targetDN, ChunkID: chunkID, Reason: reason,
				Mode: repairModeECInline,
			}
			if opts.DryRun {
				oc.Planned = true
			}
			out.Repairs = append(out.Repairs, oc)
			return
		}
		// Find a healthy source: a DN that owns the chunk per ObjectMeta,
		// is reachable, doesn't have it listed in its own MissingFromDN,
		// AND (for corrupt-mode) doesn't itself report the chunk as corrupt.
		var srcDN string
		for _, owner := range chunkOwners[chunkID] {
			if owner == targetDN {
				continue
			}
			ownerMissing := dnHas[owner]
			if ownerMissing == nil {
				continue // owner unreachable
			}
			if _, gone := ownerMissing[chunkID]; gone {
				continue
			}
			if hasCorrupt(corruptByDN[owner], chunkID) {
				continue
			}
			srcDN = owner
			break
		}
		if srcDN == "" {
			out.Skipped = append(out.Skipped, ChunkRepairOutcome{
				TargetDN: targetDN, ChunkID: chunkID, Reason: reason,
				Mode: repairModeNoSource,
				Err:  "no healthy replica found",
			})
			return
		}
		if opts.DryRun {
			out.Repairs = append(out.Repairs, ChunkRepairOutcome{
				TargetDN: targetDN, ChunkID: chunkID, SourceDN: srcDN,
				Mode: repairModeReplication, Reason: reason, Planned: true,
			})
			return
		}
		oc := s.copyChunk(ctx, srcDN, targetDN, chunkID, reason == repairReasonCorrupt)
		oc.Reason = reason
		if oc.OK {
			successCount++
		}
		out.Repairs = append(out.Repairs, oc)
	}

	for _, e := range audit.DNs {
		if !e.Reachable {
			for _, id := range e.MissingFromDN {
				out.Skipped = append(out.Skipped, ChunkRepairOutcome{
					TargetDN: e.DN, ChunkID: id, Reason: repairReasonMissing,
					Err: "target DN unreachable", Mode: repairModeSkip,
				})
			}
			continue
		}
		for _, missID := range e.MissingFromDN {
			repairOne(e.DN, missID, repairReasonMissing)
		}
		// Scrubber-detected corrupt is independent of audit's MissingFromDN
		// — file IS on disk, bytes wrong (sha mismatch). Force-overwrite path.
		for _, corruptID := range corruptByDN[e.DN] {
			repairOne(e.DN, corruptID, repairReasonCorrupt)
		}
	}
	// Per-stripe repair.Run (vs bulk) so each call's stats unambiguously
	// refers to that single stripe — caller's per-shard Err can quote
	// stats.Errors directly without parsing back from a flat bulk list.
	if opts.EC && len(ecPlanByKey) > 0 && !opts.DryRun {
		var attemptedStripes, totalRepaired, totalFailed int
		var totalBytes int64
		var allErrors []string

		ecKeys := make([]ecKey, 0, len(ecPlanByKey))
		for k := range ecPlanByKey {
			ecKeys = append(ecKeys, k)
		}
		sort.Slice(ecKeys, func(i, j int) bool {
			if ecKeys[i].Bucket != ecKeys[j].Bucket {
				return ecKeys[i].Bucket < ecKeys[j].Bucket
			}
			if ecKeys[i].Key != ecKeys[j].Key {
				return ecKeys[i].Key < ecKeys[j].Key
			}
			return ecKeys[i].Stripe < ecKeys[j].Stripe
		})

		ecOutcomeIdx := make(map[string]int, len(ecPlanByKey))
		for i, oc := range out.Repairs {
			if oc.Mode == repairModeECInline {
				ecOutcomeIdx[oc.ChunkID] = i
			}
		}

		for _, k := range ecKeys {
			sr := ecPlanByKey[k]
			if throttled() {
				for _, d := range sr.DeadShards {
					if i, ok := ecOutcomeIdx[d.ChunkID]; ok {
						out.Repairs[i].Mode = repairModeThrottled
						out.Repairs[i].Err = fmt.Sprintf("max_repairs=%d reached before this stripe (ADR-059)", opts.MaxRepairs)
					}
				}
				continue
			}
			attemptedStripes++
			plan := repair.Plan{Repairs: []repair.StripeRepair{*sr}}
			stats := repair.Run(ctx, s.Coord, s.Store, plan, 1)
			totalRepaired += stats.Repaired
			totalFailed += stats.Failed
			totalBytes += stats.BytesWritten
			allErrors = append(allErrors, stats.Errors...)

			ok := stats.Repaired == 1 && len(stats.Errors) == 0
			for _, d := range sr.DeadShards {
				if i, found := ecOutcomeIdx[d.ChunkID]; found {
					if ok {
						out.Repairs[i].OK = true
					} else {
						out.Repairs[i].Err = strings.Join(stats.Errors, "; ")
					}
				}
			}
			if ok {
				successCount++
			}
		}

		// Emit summary only when at least one stripe actually ran;
		// otherwise OK=true with zero totals would read as success
		// rather than "all stripes throttled before their turn".
		if attemptedStripes > 0 {
			summary := ChunkRepairOutcome{
				TargetDN: "(ec-summary)",
				Mode:     repairModeECSummary,
				Reason:   "ec-stats",
				OK:       len(allErrors) == 0,
			}
			if len(allErrors) > 0 {
				summary.Err = fmt.Sprintf("ec stripes attempted=%d repaired=%d failed=%d bytes=%d errs=%v",
					attemptedStripes, totalRepaired, totalFailed, totalBytes, allErrors)
			}
			out.Repairs = append(out.Repairs, summary)
		}
	}

	out.Duration = time.Since(startedAt).String()
	return out, nil
}

// collectCorruptByDN fetches /chunks/scrub-status from each reachable DN
// and returns a map of dn_addr → list of corrupt chunk_ids. Best-effort:
// unreachable DNs / fetch errors yield an empty entry (already handled
// upstream — those DNs were marked unreachable in the audit).
func (s *Server) collectCorruptByDN(ctx context.Context, dns []DNAuditEntry) map[string][]string {
	out := make(map[string][]string)
	for _, e := range dns {
		if !e.Reachable {
			continue
		}
		corrupt, err := fetchDNCorrupt(ctx, e.DN)
		if err != nil {
			s.Log.Debug("scrub-status fetch failed", "dn", e.DN, "err", err)
			continue
		}
		out[e.DN] = corrupt
	}
	return out
}

// fetchDNCorrupt: GET http://<dn>/chunks/scrub-status, return .corrupt.
func fetchDNCorrupt(ctx context.Context, dnAddr string) ([]string, error) {
	url := "http://" + dnAddr + "/chunks/scrub-status"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var body struct {
		Corrupt []string `json:"corrupt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Corrupt, nil
}

// hasCorrupt: linear scan, fine for small lists (corrupt sets are
// expected to be small in healthy clusters; binary search would be
// premature optimization).
func hasCorrupt(list []string, id string) bool {
	for _, c := range list {
		if c == id {
			return true
		}
	}
	return false
}

// copyChunk: read chunkID from src, PUT to target, log the outcome.
// `force` triggers ADR-056 overwrite path (used for corrupt-repair where
// the target already has a bad file; idempotent PUT would skip).
func (s *Server) copyChunk(ctx context.Context, src, target, chunkID string, force bool) ChunkRepairOutcome {
	oc := ChunkRepairOutcome{
		TargetDN: target, ChunkID: chunkID, SourceDN: src, Mode: repairModeReplication,
	}
	body, _, err := s.Coord.ReadChunk(ctx, chunkID, []string{src})
	if err != nil {
		oc.Err = "read from " + src + ": " + err.Error()
		return oc
	}
	put := s.Coord.PutChunkTo
	if force {
		put = s.Coord.PutChunkToForce
	}
	if perr := put(ctx, target, chunkID, body); perr != nil {
		oc.Err = "put to " + target + ": " + perr.Error()
		return oc
	}
	oc.OK = true
	return oc
}

// handleAntiEntropyRepair serves POST /v1/coord/admin/anti-entropy/repair.
// Audits + auto-fixes replication chunks. EC paths return skipped.
// 503 if Coord (DN-IO) is unset — repair needs to actually move bytes.
//
// Query parameters (ADR-056):
//   - corrupt=1 : also repair scrubber-detected corrupt chunks per DN
//                 (default off — keeps repair scope to inventory-missing
//                  unless operator opts in).
//   - dry_run=1 : preview-only; mark each candidate Planned=true and
//                 do not call ReadChunk/PutChunkTo. No bytes moved.
func (s *Server) handleAntiEntropyRepair(w http.ResponseWriter, r *http.Request) {
	if s.requireLeader(w) {
		return
	}
	if s.Coord == nil {
		writeErr(w, http.StatusServiceUnavailable,
			fmt.Errorf("anti-entropy repair requires COORD_DN_IO=1 (ADR-044)"))
		return
	}
	q := r.URL.Query()
	opts := AntiEntropyRepairOpts{
		Corrupt: q.Get("corrupt") == "1",
		DryRun:  q.Get("dry_run") == "1",
		EC:      q.Get("ec") == "1",
	}
	// ADR-059: max_repairs throttle. 0 / unset = unlimited.
	if v := q.Get("max_repairs"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("max_repairs: must be non-negative integer, got %q", v))
			return
		}
		opts.MaxRepairs = n
	}
	rep, err := s.runAntiEntropyRepair(r.Context(), opts)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// StartAntiEntropyTicker (ADR-055): if interval > 0, runs anti-entropy
// AUDIT (no auto-repair — too risky as a default ticker side-effect)
// every interval on the leader. Logs results; does not enqueue repair.
// Operator calls /repair explicitly when ready. ctx cancel stops.
func (s *Server) StartAntiEntropyTicker(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if s.Elector != nil && !s.Elector.IsLeader() {
					continue
				}
				rep, err := s.runAntiEntropy(ctx)
				if err != nil {
					s.Log.Warn("scheduled anti-entropy failed", "err", err)
					continue
				}
				divergent := 0
				totalMissing := 0
				for _, e := range rep.DNs {
					if !e.RootMatch {
						divergent++
					}
					totalMissing += len(e.MissingFromDN)
				}
				s.Log.Info("scheduled anti-entropy",
					"divergent_dns", divergent,
					"total_missing", totalMissing,
					"duration", rep.Duration)
			}
		}
	}()
}

// Avoid an unused-import warning when building without store referenced
// elsewhere in the file (defensive — store is used in expectedChunksByDN).
var _ = store.ObjectMeta{}
var _ = errors.New
