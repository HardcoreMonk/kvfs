// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package edge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
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

// ADR-041 (S5 Ep.6): edge routes placement decisions through coord. The
// PlaceN RPC returns coord's HRW result against ITS DN list — which may
// differ from edge's local DN list. This test wires a coord with a DN
// subset distinct from what edge would pick locally; PlaceN must return
// only coord's subset, proving the call hit coord.
func TestCoordClient_PlaceN_ReturnsCoordsView(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "coord.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	// Coord knows only dn1, dn2, dn3. Edge calling PlaceN through this
	// client must NEVER receive any other addr.
	cs := &coord.Server{
		Store:  st,
		Placer: placement.New([]placement.Node{
			{ID: "dn1:8080", Addr: "dn1:8080"},
			{ID: "dn2:8080", Addr: "dn2:8080"},
			{ID: "dn3:8080", Addr: "dn3:8080"},
		}),
	}
	ts := httptest.NewServer(cs.Routes())
	defer ts.Close()

	cc := NewCoordClient(ts.URL)
	cc.Timeout = 2 * time.Second

	addrs, err := cc.PlaceN(context.Background(), "any-chunk-id", 3)
	if err != nil {
		t.Fatalf("PlaceN: %v", err)
	}
	if len(addrs) != 3 {
		t.Errorf("got %d addrs, want 3", len(addrs))
	}
	allowed := map[string]bool{"dn1:8080": true, "dn2:8080": true, "dn3:8080": true}
	for _, a := range addrs {
		if !allowed[a] {
			t.Errorf("got addr %q which coord doesn't know about", a)
		}
	}

	// Determinism — same key/n → same answer.
	addrs2, _ := cc.PlaceN(context.Background(), "any-chunk-id", 3)
	for i := range addrs {
		if addrs[i] != addrs2[i] {
			t.Errorf("PlaceN non-deterministic at %d: %q vs %q", i, addrs[i], addrs2[i])
		}
	}
}

// placeN dispatch: with no CoordClient, Server falls back to local
// Coord.PlaceN. With CoordClient set, the RPC result wins. This guards
// the dispatch logic in edge.go::placeN against accidental swaps.
func TestServer_placeN_DispatchesByCoordClient(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "coord.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	// Coord that reports a fixed 1-DN set ("from-coord:8080").
	cs := &coord.Server{
		Store:  st,
		Placer: placement.New([]placement.Node{{ID: "from-coord:8080", Addr: "from-coord:8080"}}),
	}
	ts := httptest.NewServer(cs.Routes())
	defer ts.Close()

	// Server with CoordClient → expect coord's view.
	srvProxy := &Server{CoordClient: NewCoordClient(ts.URL)}
	got, err := srvProxy.placeN(context.Background(), "k", 1)
	if err != nil {
		t.Fatalf("proxy placeN: %v", err)
	}
	if len(got) != 1 || got[0] != "from-coord:8080" {
		t.Errorf("proxy mode placeN = %v, want [from-coord:8080]", got)
	}
}

// P6-10: opt-in lookup cache. Hit short-circuits the RPC; CommitObject /
// DeleteObject invalidate the entry. Tests via a counting test server
// that bumps on every /v1/coord/lookup hit.
func TestCoordClient_LookupCache_HitInvalidate(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "coord.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if err := st.PutObject(&store.ObjectMeta{Bucket: "b", Key: "k", Size: 1}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var lookupHits int32
	cs := &coord.Server{Store: st, Placer: placement.New([]placement.Node{{ID: "dn1", Addr: "dn1"}})}
	mux := cs.Routes()
	wrapped := http.NewServeMux()
	wrapped.HandleFunc("GET /v1/coord/lookup", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&lookupHits, 1)
		mux.ServeHTTP(w, r)
	})
	// Mount everything else (commit, etc.) too so CommitObject still works.
	wrapped.HandleFunc("POST /v1/coord/commit", func(w http.ResponseWriter, r *http.Request) { mux.ServeHTTP(w, r) })
	wrapped.HandleFunc("POST /v1/coord/delete", func(w http.ResponseWriter, r *http.Request) { mux.ServeHTTP(w, r) })
	wrapped.HandleFunc("GET /v1/coord/healthz", func(w http.ResponseWriter, r *http.Request) { mux.ServeHTTP(w, r) })

	ts := httptest.NewServer(wrapped)
	defer ts.Close()

	cc := NewCoordClient(ts.URL)
	cc.SetLookupCache(2 * time.Second)
	ctx := context.Background()

	// Two consecutive Lookups → only ONE coord hit (second served from cache).
	if _, err := cc.LookupObject(ctx, "b", "k"); err != nil {
		t.Fatalf("lookup 1: %v", err)
	}
	if _, err := cc.LookupObject(ctx, "b", "k"); err != nil {
		t.Fatalf("lookup 2: %v", err)
	}
	if got := atomic.LoadInt32(&lookupHits); got != 1 {
		t.Errorf("after 2 lookups, coord hits = %d, want 1 (second cached)", got)
	}

	// Commit on the same client invalidates → next Lookup fetches anew.
	if err := cc.CommitObject(ctx, &store.ObjectMeta{Bucket: "b", Key: "k", Size: 2}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := cc.LookupObject(ctx, "b", "k"); err != nil {
		t.Fatalf("lookup 3: %v", err)
	}
	if got := atomic.LoadInt32(&lookupHits); got != 2 {
		t.Errorf("after commit + lookup, coord hits = %d, want 2 (cache invalidated)", got)
	}

	// SetLookupCache(0) disables.
	cc.SetLookupCache(0)
	if _, err := cc.LookupObject(ctx, "b", "k"); err != nil {
		t.Fatalf("lookup 4: %v", err)
	}
	if _, err := cc.LookupObject(ctx, "b", "k"); err != nil {
		t.Fatalf("lookup 5: %v", err)
	}
	if got := atomic.LoadInt32(&lookupHits); got != 4 {
		t.Errorf("after disable + 2 lookups, coord hits = %d, want 4 (no caching)", got)
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
