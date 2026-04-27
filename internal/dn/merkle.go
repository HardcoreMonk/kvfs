// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Anti-entropy support (ADR-054, S7 Ep.4):
//
//   - GET /chunks/merkle             — flat Merkle: root + 256 bucket hashes
//   - GET /chunks/merkle/bucket?idx= — chunk_ids in one bucket
//   - GET /chunks/scrub-status       — bit-rot scrubber progress + corrupt set
//
// Single-tier Merkle (256 buckets keyed by chunk_id[0:2]) keeps the math
// trivial — each bucket carries an O(N/256) chunk-id list, root is
// sha256(concat(bucket_hashes)). For kvfs scale (≤ 10⁵ chunks/DN) this
// is a sweet spot: one HTTP round-trip resolves equality, the second
// trip enumerates the diverging bucket. Multi-tier hierarchical Merkle
// (Cassandra-style) is overkill until a DN holds 10⁷+ chunks.
package dn

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

// MerkleResponse: flat 256-bucket tree summary served by /chunks/merkle.
// `Root` lets the caller compare in O(32 bytes); on root mismatch the
// buckets array tells which slot diverged so the caller fetches only
// the affected bucket via /chunks/merkle/bucket?idx=N.
type MerkleResponse struct {
	DN      string         `json:"dn"`
	Root    string         `json:"root"`
	Total   int            `json:"total"`
	Buckets []MerkleBucket `json:"buckets"`
}

// MerkleBucket: one of 256 buckets keyed by first 2 hex chars of
// chunk_id. Empty buckets keep `Hash` = sha256("") so root math is
// stable even when many buckets are sparse — caller can tell empty
// from non-empty by Count == 0.
type MerkleBucket struct {
	Idx   int    `json:"idx"`   // 0..255
	Hash  string `json:"hash"`  // sha256 of sorted chunk_ids in bucket (newline-joined)
	Count int    `json:"count"` // chunks in this bucket
}

// computeMerkle scans the chunk directory once and returns the inventory
// arranged by 2-char prefix bucket. Pure function over the on-disk state
// — no internal counters used here so a corruption that leaves a chunk
// file but breaks chunkCount remains visible to anti-entropy.
func (s *Server) computeMerkle() (*MerkleResponse, error) {
	root := filepath.Join(s.dataDir, "chunks")
	buckets := make([][]string, 256)
	total := 0
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		dir := filepath.Base(filepath.Dir(p))
		base := filepath.Base(p)
		if len(dir) != 2 || len(base) != 62 {
			return nil
		}
		id := dir + base
		if !validChunkID(id) {
			return nil
		}
		idx, perr := strconv.ParseInt(dir, 16, 32)
		if perr != nil {
			return nil
		}
		buckets[idx] = append(buckets[idx], id)
		total++
		return nil
	})
	if err != nil {
		return nil, err
	}

	resp := &MerkleResponse{DN: s.id, Total: total, Buckets: make([]MerkleBucket, 256)}
	rootH := sha256.New()
	for i := 0; i < 256; i++ {
		ids := buckets[i]
		sort.Strings(ids)
		// Bucket hash = sha256 of newline-joined sorted ids. Empty
		// bucket → sha256(""). Any addition / removal / reorder changes
		// the bucket hash, which propagates to root.
		bh := sha256.New()
		for _, id := range ids {
			bh.Write([]byte(id))
			bh.Write([]byte{'\n'})
		}
		bsum := bh.Sum(nil)
		resp.Buckets[i] = MerkleBucket{
			Idx:   i,
			Hash:  hex.EncodeToString(bsum),
			Count: len(ids),
		}
		rootH.Write(bsum)
	}
	resp.Root = hex.EncodeToString(rootH.Sum(nil))
	return resp, nil
}

// listBucket returns the chunk_ids in one prefix bucket, sorted. Used
// by anti-entropy callers to enumerate the diverging slot identified
// by Merkle comparison.
func (s *Server) listBucket(idx int) ([]string, error) {
	if idx < 0 || idx > 255 {
		return nil, nil
	}
	prefix := []byte{hexNibble(idx >> 4), hexNibble(idx & 0xF)}
	dir := filepath.Join(s.dataDir, "chunks", string(prefix))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		base := e.Name()
		if len(base) != 62 {
			continue
		}
		id := string(prefix) + base
		if validChunkID(id) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func hexNibble(n int) byte {
	if n < 10 {
		return byte('0' + n)
	}
	return byte('a' + (n - 10))
}

// handleMerkle serves /chunks/merkle.
func (s *Server) handleMerkle(w http.ResponseWriter, _ *http.Request) {
	resp, err := s.computeMerkle()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleMerkleBucket serves /chunks/merkle/bucket?idx=N.
func (s *Server) handleMerkleBucket(w http.ResponseWriter, r *http.Request) {
	idxStr := r.URL.Query().Get("idx")
	idx, err := strconv.Atoi(idxStr)
	if err != nil || idx < 0 || idx > 255 {
		http.Error(w, "idx must be 0..255", http.StatusBadRequest)
		return
	}
	ids, err := s.listBucket(idx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"dn":     s.id,
		"idx":    idx,
		"count":  len(ids),
		"chunks": ids,
	})
}
