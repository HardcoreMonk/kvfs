// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package edge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/HardcoreMonk/kvfs/internal/coordinator"
	"github.com/HardcoreMonk/kvfs/internal/placement"
	"github.com/HardcoreMonk/kvfs/internal/store"
)

// fakeDN is a tiny in-process DN that serves a fixed (chunkID -> body)
// map via GET /chunk/{id}. Optional fail or delay per shard simulates
// slow / dead nodes for the parallel-fetch tests.
type fakeDN struct {
	chunks map[string][]byte
	fail   bool
	delay  time.Duration
}

func (f *fakeDN) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.fail {
			http.Error(w, "simulated failure", http.StatusInternalServerError)
			return
		}
		if f.delay > 0 {
			select {
			case <-time.After(f.delay):
			case <-r.Context().Done():
				return
			}
		}
		// /chunk/{id}
		id := strings.TrimPrefix(r.URL.Path, "/chunk/")
		body, ok := f.chunks[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
}

func mkChunk(payload string) (string, []byte) {
	body := []byte(payload)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), body
}

// helper: build a Server wired to a fresh in-memory MetaStore + a Coord
// pointing at the listed DN addresses (host:port from each httptest.URL).
func newTestServer(t *testing.T, dnAddrs []string) *Server {
	t.Helper()
	dir := t.TempDir()
	ms, err := store.Open(dir + "/edge.db")
	if err != nil {
		t.Fatalf("OpenMetaStore: %v", err)
	}
	t.Cleanup(func() { _ = ms.Close() })

	nodes := make([]placement.Node, len(dnAddrs))
	for i, a := range dnAddrs {
		nodes[i] = placement.Node{ID: a, Addr: a}
	}
	c, err := coordinator.New(coordinator.Config{
		Nodes:             nodes,
		ReplicationFactor: 1, // each shard placed on exactly one DN in this test
		QuorumWrite:       1,
	})
	if err != nil {
		t.Fatalf("coordinator.New: %v", err)
	}
	return &Server{
		Store: ms,
		Coord: c,
		Log:   slog.New(slog.NewTextHandler(testLogWriter{t}, &slog.HandlerOptions{Level: slog.LevelError})),
	}
}

type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(b []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(b)))
	return len(b), nil
}

// startFakeDNs: spin up one httptest server per spec and return their
// host:port addresses + the raw test servers (for cleanup). Each spec
// is preloaded with a single (chunkID, payload) pair.
func startFakeDNs(t *testing.T, dns []*fakeDN) []string {
	t.Helper()
	addrs := make([]string, len(dns))
	for i, dn := range dns {
		ts := httptest.NewServer(dn.handler())
		t.Cleanup(ts.Close)
		// strip "http://" → "127.0.0.1:NNN"
		addrs[i] = strings.TrimPrefix(ts.URL, "http://")
	}
	return addrs
}

// TestParallelFetchShards_AllSurvive: 6 fast DNs all return → 6 survivors,
// returns in well under any per-shard delay would imply sequentially.
func TestParallelFetchShards_AllSurvive(t *testing.T) {
	const N = 6
	chunkIDs := make([]string, N)
	bodies := make([][]byte, N)
	dns := make([]*fakeDN, N)
	for i := 0; i < N; i++ {
		id, body := mkChunk(fmt.Sprintf("shard-%d", i))
		chunkIDs[i] = id
		bodies[i] = body
		dns[i] = &fakeDN{
			chunks: map[string][]byte{id: body},
			delay:  50 * time.Millisecond, // each DN takes 50ms
		}
	}
	addrs := startFakeDNs(t, dns)
	srv := newTestServer(t, addrs)

	shards := make([]store.ChunkRef, N)
	for i := range dns {
		shards[i] = store.ChunkRef{ChunkID: chunkIDs[i], Replicas: []string{addrs[i]}}
	}

	start := time.Now()
	got, survivors := srv.parallelFetchShards(context.Background(), shards, 4)
	elapsed := time.Since(start)

	if survivors != 4 {
		t.Errorf("survivors = %d, want 4 (returned at K, not K+M)", survivors)
	}
	// Parallel: should finish in ~one delay (50ms), not 6×50ms = 300ms.
	if elapsed > 200*time.Millisecond {
		t.Errorf("elapsed=%v, parallel fetch should finish in ~50ms not the serial total", elapsed)
	}
	// At least 4 of the returned slots must hold the right body.
	matched := 0
	for i, b := range got {
		if b == nil {
			continue
		}
		if string(b) != string(bodies[i]) {
			t.Errorf("slot %d: body mismatch", i)
		}
		matched++
	}
	if matched < 4 {
		t.Errorf("matched=%d, want >= 4", matched)
	}
}

// TestParallelFetchShards_KSurvivors: 4 alive + 2 dead, quorum=4 → succeeds.
// Verifies the "any K" property of EC GET — no preference for which K
// happen to survive.
func TestParallelFetchShards_KSurvivors(t *testing.T) {
	const N = 6
	chunkIDs := make([]string, N)
	bodies := make([][]byte, N)
	dns := make([]*fakeDN, N)
	for i := 0; i < N; i++ {
		id, body := mkChunk(fmt.Sprintf("shard-%d", i))
		chunkIDs[i] = id
		bodies[i] = body
		dns[i] = &fakeDN{chunks: map[string][]byte{id: body}}
	}
	dns[1].fail = true
	dns[4].fail = true
	addrs := startFakeDNs(t, dns)
	srv := newTestServer(t, addrs)

	shards := make([]store.ChunkRef, N)
	for i := range dns {
		shards[i] = store.ChunkRef{ChunkID: chunkIDs[i], Replicas: []string{addrs[i]}}
	}

	got, survivors := srv.parallelFetchShards(context.Background(), shards, 4)
	if survivors < 4 {
		t.Errorf("survivors = %d, want >= 4 (4 DNs alive)", survivors)
	}
	if survivors > 6 {
		t.Errorf("survivors = %d, total only 6", survivors)
	}
	// Slots 1 and 4 must be nil (those DNs failed).
	if got[1] != nil || got[4] != nil {
		t.Errorf("failed-DN slots should be nil: got[1]=%v got[4]=%v", got[1], got[4])
	}
}

// TestParallelFetchShards_BelowQuorum: only 3 alive of 6, quorum=4 → returns
// survivors=3 (cluster cannot reconstruct, caller fails the GET).
func TestParallelFetchShards_BelowQuorum(t *testing.T) {
	const N = 6
	chunkIDs := make([]string, N)
	bodies := make([][]byte, N)
	dns := make([]*fakeDN, N)
	for i := 0; i < N; i++ {
		id, body := mkChunk(fmt.Sprintf("shard-%d", i))
		chunkIDs[i] = id
		bodies[i] = body
		dns[i] = &fakeDN{chunks: map[string][]byte{id: body}}
	}
	for _, d := range []int{0, 2, 3} {
		dns[d].fail = true
	}
	addrs := startFakeDNs(t, dns)
	srv := newTestServer(t, addrs)

	shards := make([]store.ChunkRef, N)
	for i := range dns {
		shards[i] = store.ChunkRef{ChunkID: chunkIDs[i], Replicas: []string{addrs[i]}}
	}

	_, survivors := srv.parallelFetchShards(context.Background(), shards, 4)
	if survivors != 3 {
		t.Errorf("survivors = %d, want 3 (3 alive DNs)", survivors)
	}
}
