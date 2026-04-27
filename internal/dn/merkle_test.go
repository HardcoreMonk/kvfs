// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package dn

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// putTestChunk writes a body to the DN's data dir using the chunk_id
// =sha256(body) layout. Used by the Merkle tests to seed inventory
// without going through the HTTP PUT path (which validates content).
func putTestChunk(t *testing.T, dataDir string, body string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(body))
	id := hex.EncodeToString(sum[:])
	dir := filepath.Join(dataDir, "chunks", id[:2])
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, id[2:]), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return id
}

// TestMerkle_RootStableUnderSameInventory: identical chunk sets on two
// fresh DNs must produce identical Merkle roots. This is the core
// invariant — without it, anti-entropy's "compare roots" optimization
// is meaningless.
func TestMerkle_RootStableUnderSameInventory(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	srvA, err := NewServer("dn-a", dirA)
	if err != nil {
		t.Fatalf("NewServer A: %v", err)
	}
	srvB, err := NewServer("dn-b", dirB)
	if err != nil {
		t.Fatalf("NewServer B: %v", err)
	}
	for _, body := range []string{"alpha", "beta", "gamma", "delta"} {
		putTestChunk(t, dirA, body)
		putTestChunk(t, dirB, body)
	}
	mA, err := srvA.computeMerkle()
	if err != nil {
		t.Fatalf("computeMerkle A: %v", err)
	}
	mB, err := srvB.computeMerkle()
	if err != nil {
		t.Fatalf("computeMerkle B: %v", err)
	}
	if mA.Root != mB.Root {
		t.Errorf("root mismatch on identical inventories: A=%s B=%s", mA.Root[:16], mB.Root[:16])
	}
	if mA.Total != 4 || mB.Total != 4 {
		t.Errorf("total: A=%d B=%d, want 4 each", mA.Total, mB.Total)
	}
}

// TestMerkle_RootDivergesOnAddRemove: a single chunk diff must change
// both the affected bucket's hash and the root. Critical for
// detecting divergence in O(constant work) before drilling into the
// bucket level.
func TestMerkle_RootDivergesOnAddRemove(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	srvA, _ := NewServer("dn-a", dirA)
	srvB, _ := NewServer("dn-b", dirB)
	for _, b := range []string{"alpha", "beta", "gamma"} {
		putTestChunk(t, dirA, b)
		putTestChunk(t, dirB, b)
	}
	// Extra on A only.
	missingID := putTestChunk(t, dirA, "delta")

	mA, _ := srvA.computeMerkle()
	mB, _ := srvB.computeMerkle()
	if mA.Root == mB.Root {
		t.Fatalf("roots should differ when one DN holds an extra chunk")
	}
	// Find the bucket of the missing chunk; that bucket must be the
	// one whose hash differs.
	idx := mustHexNibble(missingID[0])*16 + mustHexNibble(missingID[1])
	if mA.Buckets[idx].Hash == mB.Buckets[idx].Hash {
		t.Errorf("bucket %d hash should differ (missing=%s..)", idx, missingID[:8])
	}
	// All OTHER buckets must still match — divergence is localized.
	for i := 0; i < 256; i++ {
		if i == idx {
			continue
		}
		if mA.Buckets[i].Hash != mB.Buckets[i].Hash {
			t.Errorf("bucket %d should match but differs (idx=%02x)", i, i)
		}
	}
}

// TestMerkle_HTTPRoundTrip: end-to-end via the registered HTTP routes.
// Catches integration bugs (handler wiring, JSON shape) that
// computeMerkle alone wouldn't.
func TestMerkle_HTTPRoundTrip(t *testing.T) {
	dir := t.TempDir()
	srv, _ := NewServer("dn-1", dir)
	for _, b := range []string{"x", "y"} {
		putTestChunk(t, dir, b)
	}
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/chunks/merkle")
	if err != nil {
		t.Fatalf("GET merkle: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d", resp.StatusCode)
	}
	// Cheap shape verification — the JSON contains "root" and "buckets":[256].
	buf := make([]byte, 64*1024)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])
	if !strings.Contains(body, `"root":`) || !strings.Contains(body, `"buckets":`) {
		t.Errorf("response missing root/buckets: %s", body[:200])
	}
}

// TestScrubber_DetectsCorruptChunk: write a real chunk, then hand-edit
// the on-disk file to mismatch its sha256. Run a single scrub pass and
// assert the chunk is in the corrupt set + ChunksScrubbed counter
// advanced.
func TestScrubber_DetectsCorruptChunk(t *testing.T) {
	dir := t.TempDir()
	srv, _ := NewServer("dn-1", dir)
	id := putTestChunk(t, dir, "real-payload")
	// Corrupt the file in-place — simulates bit rot.
	path := srv.chunkPath(id)
	if err := os.WriteFile(path, []byte("FLIPPED-bytes-not-real-payload"), 0o644); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}
	srv.scrubOne(path, id)
	srv.scrubMu.Lock()
	_, found := srv.scrub.corrupt[id]
	cnt := srv.scrub.chunksScrubbed
	srv.scrubMu.Unlock()
	if !found {
		t.Errorf("expected %s in corrupt set after sha mismatch", id[:8])
	}
	if cnt < 1 {
		t.Errorf("ChunksScrubbed=%d, want ≥ 1", cnt)
	}
}

// TestScrubber_CorruptStatePersistsAcrossRestart (ADR-061): a corrupt
// chunk flagged on one Server instance should reappear in the corrupt
// set when a fresh Server starts on the same dataDir, without waiting
// for a new scrub pass to detect it.
func TestScrubber_CorruptStatePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	srv1, _ := NewServer("dn-1", dir)
	id := putTestChunk(t, dir, "real-payload")
	// Corrupt on disk + manually flag (simulates a scrub pass that found it).
	path := srv1.chunkPath(id)
	if err := os.WriteFile(path, []byte("flipped-bytes-mismatch-now"), 0o644); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}
	srv1.scrubOne(path, id)
	srv1.scrubMu.Lock()
	_, found := srv1.scrub.corrupt[id]
	srv1.scrubMu.Unlock()
	if !found {
		t.Fatalf("scrubOne should have flagged %s as corrupt", id[:8])
	}

	// Simulate restart: a fresh Server on the SAME dataDir.
	srv2, _ := NewServer("dn-1", dir)
	loaded := srv2.loadCorruptSet()
	if _, ok := loaded[id]; !ok {
		t.Errorf("loadCorruptSet should have restored %s after restart", id[:8])
	}
}

// TestScrubber_PersistedFileShape: minimal proof the on-disk shape
// matches the documented schema (Version 1 + Corrupt array). Catches
// the case where someone bumps the format without updating the
// version field.
func TestScrubber_PersistedFileShape(t *testing.T) {
	dir := t.TempDir()
	srv, _ := NewServer("dn-1", dir)
	srv.MarkCorruptForTest("a" + strings.Repeat("0", 63))
	srv.MarkCorruptForTest("b" + strings.Repeat("0", 63))

	data, err := os.ReadFile(filepath.Join(dir, scrubStateFile))
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var st scrubStatePersistedShape
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if st.Version != 1 {
		t.Errorf("Version = %d, want 1", st.Version)
	}
	if len(st.Corrupt) != 2 {
		t.Errorf("Corrupt len = %d, want 2", len(st.Corrupt))
	}
}

// helper used in TestMerkle_RootDivergesOnAddRemove. lowercase only
// since chunk_ids are hex.
func mustHexNibble(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c - 'a' + 10)
	}
	panic(fmt.Sprintf("not a hex nibble: %c", c))
}
