// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package edge

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/HardcoreMonk/kvfs/internal/coord"
	"github.com/HardcoreMonk/kvfs/internal/placement"
	"github.com/HardcoreMonk/kvfs/internal/store"
)

// Wires CoordClient against a real coord.Server (in-process via httptest).
// Verifies CommitObject + LookupObject + DeleteObject + Healthz round-trip
// against the actual coord daemon — no mocking. Guards the Ep.2 contract:
// edge handler routing decision is "if CoordClient != nil, use it."
func TestCoordClient_RoundTripAgainstRealCoord(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "coord.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	cs := &coord.Server{
		Store:  st,
		Placer: placement.New([]placement.Node{{ID: "dn1", Addr: "dn1"}, {ID: "dn2", Addr: "dn2"}}),
	}
	ts := httptest.NewServer(cs.Routes())
	defer ts.Close()

	cc := NewCoordClient(ts.URL)
	cc.Timeout = 2 * time.Second

	ctx := context.Background()
	if err := cc.Healthz(ctx); err != nil {
		t.Fatalf("healthz: %v", err)
	}

	meta := &store.ObjectMeta{
		Bucket: "b",
		Key:    "ep2-test",
		Size:   7,
		Chunks: []store.ChunkRef{
			{ChunkID: "c1", Size: 7, Replicas: []string{"dn1", "dn2"}},
		},
	}

	if err := cc.CommitObject(ctx, meta); err != nil {
		t.Fatalf("commit: %v", err)
	}

	got, err := cc.LookupObject(ctx, "b", "ep2-test")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.Bucket != "b" || got.Key != "ep2-test" || got.Size != 7 {
		t.Errorf("lookup payload mismatch: %+v", got)
	}
	if len(got.Chunks) != 1 || got.Chunks[0].ChunkID != "c1" {
		t.Errorf("lookup chunks wrong: %+v", got.Chunks)
	}

	// Lookup of missing key → store.ErrNotFound (preserved across HTTP boundary).
	_, err = cc.LookupObject(ctx, "b", "no-such-key")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing lookup err = %v, want ErrNotFound", err)
	}

	if err := cc.DeleteObject(ctx, "b", "ep2-test"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err = cc.LookupObject(ctx, "b", "ep2-test")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("post-delete lookup err = %v, want ErrNotFound", err)
	}
}

// Healthz against an unreachable coord must return a non-nil error within
// the per-call timeout — used by edge boot to fail fast on misconfig.
func TestCoordClient_HealthzFailsFastOnUnreachable(t *testing.T) {
	cc := NewCoordClient("http://127.0.0.1:1") // port 1 = always refused
	cc.Timeout = 500 * time.Millisecond

	start := time.Now()
	err := cc.Healthz(context.Background())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("want error, got nil")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Healthz took %v, expected to fail well under 2s", elapsed)
	}
}
