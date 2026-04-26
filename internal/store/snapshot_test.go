// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package store

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotRoundTrip(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bbolt")
	st, err := Open(src)
	if err != nil {
		t.Fatalf("open src: %v", err)
	}
	defer st.Close()

	// Seed: 2 plain objects + 1 EC object.
	for _, key := range []string{"alpha", "beta"} {
		obj := &ObjectMeta{
			Bucket: "b1", Key: key, Size: 16, ContentType: "text/plain",
			Chunks: []ChunkRef{{ChunkID: "c-" + key, Size: 16, Replicas: []string{":8001"}}},
		}
		if err := st.PutObject(obj); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	ec := &ObjectMeta{
		Bucket: "b1", Key: "gamma", Size: 64,
		EC: &ECParams{K: 2, M: 1},
		Stripes: []Stripe{{
			StripeID: "s0",
			Shards:   []ChunkRef{{ChunkID: "g0", Size: 32, Replicas: []string{":8001"}}, {ChunkID: "g1", Size: 32, Replicas: []string{":8002"}}, {ChunkID: "g2", Size: 32, Replicas: []string{":8003"}}},
		}},
	}
	if err := st.PutObject(ec); err != nil {
		t.Fatalf("put ec: %v", err)
	}

	// Snapshot to buffer.
	var buf bytes.Buffer
	n, err := st.Snapshot(&buf)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if n == 0 || int64(buf.Len()) != n {
		t.Fatalf("snapshot bytes mismatch: returned=%d buf=%d", n, buf.Len())
	}

	// Write snapshot to a new file and re-open as restored store.
	dst := filepath.Join(dir, "restored.bbolt")
	if err := os.WriteFile(dst, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write restored: %v", err)
	}
	rst, err := Open(dst)
	if err != nil {
		t.Fatalf("open restored: %v", err)
	}
	defer rst.Close()

	// Restored store must have all 3 objects with identical content.
	for _, key := range []string{"alpha", "beta", "gamma"} {
		o, err := rst.GetObject("b1", key)
		if err != nil {
			t.Fatalf("get %s from restored: %v", key, err)
		}
		if o.Key != key {
			t.Errorf("restored key mismatch: got %s want %s", o.Key, key)
		}
	}
	gamma, _ := rst.GetObject("b1", "gamma")
	if gamma == nil || !gamma.IsEC() || len(gamma.Stripes) != 1 || len(gamma.Stripes[0].Shards) != 3 {
		t.Errorf("EC object not restored intact: %+v", gamma)
	}
}

func TestStats(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "stats.bbolt"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	// Seed: 2 plain (3 chunks each) + 1 EC (2 stripes × 4 shards) + 2 DNs + 1 runtime DN + 1 urlkey.
	for _, key := range []string{"a", "b"} {
		obj := &ObjectMeta{
			Bucket: "x", Key: key,
			Chunks: []ChunkRef{
				{ChunkID: "c1-" + key, Replicas: []string{":1"}},
				{ChunkID: "c2-" + key, Replicas: []string{":1"}},
				{ChunkID: "c3-" + key, Replicas: []string{":1"}},
			},
		}
		if err := st.PutObject(obj); err != nil {
			t.Fatal(err)
		}
	}
	ec := &ObjectMeta{
		Bucket: "x", Key: "ec",
		EC: &ECParams{K: 2, M: 2},
		Stripes: []Stripe{
			{StripeID: "s0", Shards: make([]ChunkRef, 4)},
			{StripeID: "s1", Shards: make([]ChunkRef, 4)},
		},
	}
	if err := st.PutObject(ec); err != nil {
		t.Fatal(err)
	}
	_ = st.PutDN(&DNInfo{ID: "dn1", Addr: ":8001"})
	_ = st.PutDN(&DNInfo{ID: "dn2", Addr: ":8002"})
	_ = st.AddRuntimeDN(":8003")
	_ = st.PutURLKey("v1", "deadbeef", true)

	s, err := st.Stats()
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if s.Objects != 3 {
		t.Errorf("Objects=%d want 3", s.Objects)
	}
	if s.ECObjects != 1 {
		t.Errorf("ECObjects=%d want 1", s.ECObjects)
	}
	if s.ChunkCount != 6 {
		t.Errorf("ChunkCount=%d want 6", s.ChunkCount)
	}
	if s.StripeCount != 2 {
		t.Errorf("StripeCount=%d want 2", s.StripeCount)
	}
	if s.ShardCount != 8 {
		t.Errorf("ShardCount=%d want 8", s.ShardCount)
	}
	if s.DNs != 2 {
		t.Errorf("DNs=%d want 2", s.DNs)
	}
	if s.RuntimeDNs != 1 {
		t.Errorf("RuntimeDNs=%d want 1", s.RuntimeDNs)
	}
	if s.URLKeys != 1 {
		t.Errorf("URLKeys=%d want 1", s.URLKeys)
	}
	if s.BBoltBytes <= 0 {
		t.Errorf("BBoltBytes=%d want > 0", s.BBoltBytes)
	}
}
