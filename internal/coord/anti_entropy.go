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
	"sync"
	"time"

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
// the operator schedules the cli command externally (cron, k8s job).
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

// Avoid an unused-import warning when building without store referenced
// elsewhere in the file (defensive — store is used in expectedChunksByDN).
var _ = store.ObjectMeta{}
var _ = errors.New
