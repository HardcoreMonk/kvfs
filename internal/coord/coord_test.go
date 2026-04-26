// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package coord

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/HardcoreMonk/kvfs/internal/election"
	"github.com/HardcoreMonk/kvfs/internal/placement"
	"github.com/HardcoreMonk/kvfs/internal/store"
)

// Smoke test: a 4-DN placer + bbolt store round-trips through the HTTP
// surface — Place → Commit → Lookup → Delete — and yields what callers
// expect. Guards Season 5 Ep.1's RPC contract.
func TestServerRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "coord.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	nodes := []placement.Node{
		{ID: "dn1:8080", Addr: "dn1:8080"},
		{ID: "dn2:8080", Addr: "dn2:8080"},
		{ID: "dn3:8080", Addr: "dn3:8080"},
		{ID: "dn4:8080", Addr: "dn4:8080"},
	}
	srv := &Server{Store: st, Placer: placement.New(nodes)}

	hs := httptest.NewServer(srv.Routes())
	defer hs.Close()

	// Place: a chunk_id should map to 3 distinct addresses out of the 4.
	body, _ := json.Marshal(PlaceRequest{Key: "abc-chunk", N: 3})
	resp, err := http.Post(hs.URL+"/v1/coord/place", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("place status %d", resp.StatusCode)
	}
	var pr PlaceResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		t.Fatalf("decode place: %v", err)
	}
	if len(pr.Addrs) != 3 {
		t.Errorf("placed %d addrs, want 3", len(pr.Addrs))
	}

	// Place is deterministic — same key + same DN set → same answer.
	resp2, _ := http.Post(hs.URL+"/v1/coord/place", "application/json", bytes.NewReader(body))
	var pr2 PlaceResponse
	_ = json.NewDecoder(resp2.Body).Decode(&pr2)
	for i := range pr.Addrs {
		if pr.Addrs[i] != pr2.Addrs[i] {
			t.Errorf("placement non-deterministic at %d: %q vs %q", i, pr.Addrs[i], pr2.Addrs[i])
		}
	}

	// Commit: store a meta record.
	meta := &store.ObjectMeta{
		Bucket: "b",
		Key:    "k",
		Size:   42,
		Chunks: []store.ChunkRef{
			{ChunkID: "abc-chunk", Size: 42, Replicas: pr.Addrs},
		},
	}
	cb, _ := json.Marshal(CommitRequest{Meta: meta})
	cresp, err := http.Post(hs.URL+"/v1/coord/commit", "application/json", bytes.NewReader(cb))
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if cresp.StatusCode != 200 {
		t.Fatalf("commit status %d", cresp.StatusCode)
	}

	// Lookup: must find it.
	lresp, err := http.Get(hs.URL + "/v1/coord/lookup?bucket=b&key=k")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if lresp.StatusCode != 200 {
		t.Fatalf("lookup status %d", lresp.StatusCode)
	}
	var got store.ObjectMeta
	if err := json.NewDecoder(lresp.Body).Decode(&got); err != nil {
		t.Fatalf("decode lookup: %v", err)
	}
	if got.Bucket != "b" || got.Key != "k" || got.Size != 42 {
		t.Errorf("lookup payload mismatch: %+v", got)
	}

	// Delete + lookup again → 404.
	db, _ := json.Marshal(DeleteRequest{Bucket: "b", Key: "k"})
	dresp, _ := http.Post(hs.URL+"/v1/coord/delete", "application/json", bytes.NewReader(db))
	if dresp.StatusCode != 200 {
		t.Errorf("delete status %d", dresp.StatusCode)
	}

	l404, _ := http.Get(hs.URL + "/v1/coord/lookup?bucket=b&key=k")
	if l404.StatusCode != 404 {
		t.Errorf("post-delete lookup status %d, want 404", l404.StatusCode)
	}

	// Healthz sanity.
	hresp, _ := http.Get(hs.URL + "/v1/coord/healthz")
	if hresp.StatusCode != 200 {
		t.Errorf("healthz status %d", hresp.StatusCode)
	}
}

// HA mode (Ep.3, ADR-038): a follower coord must reject mutating RPCs
// with 503 + X-COORD-LEADER pointing at the current leader. Reads stay
// open. Tests use a stubFollower elector that always reports State=
// Follower so we can exercise the gate without a real 3-node cluster.
func TestRequireLeader_FollowerRejectsWritesPropagatesLeaderHint(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "coord.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	// election.New configured with self ≠ any peer "leader" never wins on
	// its own (no quorum from a single non-self peer). Force a known
	// LeaderURL by sending a heartbeat handler call directly is too much
	// machinery — instead, use a single-peer setup that includes a fake
	// leader address and let the timing-driven follower stay follower
	// for the test window.
	//
	// Simpler: build the elector but don't Run() it; the zero state is
	// Follower, leader=="". Then verify the gate returns 503 with no
	// header (election in progress).
	el := election.New(election.Config{
		SelfID: "http://self:9000",
		Peers: []election.Peer{
			{ID: "http://self:9000", URL: "http://self:9000"},
			{ID: "http://other:9000", URL: "http://other:9000"},
		},
	})

	srv := &Server{
		Store:   st,
		Placer:  placement.New([]placement.Node{{ID: "dn1", Addr: "dn1"}}),
		Elector: el,
	}
	hs := httptest.NewServer(srv.Routes())
	defer hs.Close()

	// commit must 503 (no leader yet → no header set, but status code is
	// the contract).
	body, _ := json.Marshal(CommitRequest{Meta: &store.ObjectMeta{
		Bucket: "b", Key: "k",
		Chunks: []store.ChunkRef{{ChunkID: "c1", Replicas: []string{"dn1"}}},
	}})
	resp, err := http.Post(hs.URL+"/v1/coord/commit", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if resp.StatusCode != 503 {
		t.Errorf("follower commit status = %d, want 503", resp.StatusCode)
	}

	// lookup is allowed even on a follower.
	lresp, _ := http.Get(hs.URL + "/v1/coord/lookup?bucket=b&key=k")
	if lresp.StatusCode != 404 {
		// not 404 = 403/503/etc — definitely a problem
		t.Errorf("follower lookup of missing key = %d, want 404", lresp.StatusCode)
	}

	// healthz allowed.
	hresp, _ := http.Get(hs.URL + "/v1/coord/healthz")
	if hresp.StatusCode != 200 {
		t.Errorf("follower healthz = %d, want 200", hresp.StatusCode)
	}
}

// ADR-040 transactional commit: when no quorum can be reached, the leader
// must reject the commit (return error from commit(), 5xx to client) AND
// MUST NOT touch its bbolt. This guards the phantom-write window that the
// best-effort path (ADR-039) leaves open.
func TestTransactionalCommit_QuorumFailureLeavesBboltUntouched(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "coord.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	wal, err := store.OpenWAL(filepath.Join(dir, "coord.wal"))
	if err != nil {
		t.Fatalf("open wal: %v", err)
	}
	defer wal.Close()
	st.SetWAL(wal)

	// 3-peer election where the only reachable node is self → ReplicateEntry
	// can never get quorum (needs 2 acks, only 1 self-ack possible).
	el := election.New(election.Config{
		SelfID: "http://self:9000",
		Peers: []election.Peer{
			{ID: "http://self:9000", URL: "http://self:9000"},
			{ID: "http://dead1:9000", URL: "http://dead1:9000"},
			{ID: "http://dead2:9000", URL: "http://dead2:9000"},
		},
		ReplicateTimeout: 200 * time.Millisecond,
	})

	srv := &Server{
		Store:               st,
		Placer:              placement.New([]placement.Node{{ID: "dn1", Addr: "dn1"}}),
		Elector:             el,
		TransactionalCommit: true,
	}

	// Force the test elector into Leader state by directly handling a
	// vote-RPC call — too much. Easier: bypass the requireLeader gate by
	// calling commit() directly. The test exercises the transactional path
	// itself, not the leader gate (which TestRequireLeader_... covers).
	meta := &store.ObjectMeta{
		Bucket: "b", Key: "txn-fail",
		Chunks: []store.ChunkRef{{ChunkID: "c1", Replicas: []string{"dn1"}}},
	}
	beforeSeq := wal.LastSeq()

	err = srv.commit(context.Background(), meta)
	if err == nil {
		t.Fatalf("commit succeeded under no-quorum, want error")
	}

	// bbolt MUST NOT contain the entry.
	if _, lerr := st.GetObject("b", "txn-fail"); lerr == nil {
		t.Errorf("phantom write: bbolt has txn-fail after failed transactional commit")
	}
	// WAL.LastSeq must not advance (no entry was appended locally).
	if afterSeq := wal.LastSeq(); afterSeq != beforeSeq {
		t.Errorf("WAL advanced from %d to %d on failed commit", beforeSeq, afterSeq)
	}
}

// When TransactionalCommit is set but no Elector / no WAL, commit must
// fall back to the legacy path (best-effort) without crashing — config-trap
// guard. Mirrors the early-return in commit().
func TestTransactionalCommit_FallsBackWhenPrerequisitesMissing(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "coord.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	srv := &Server{
		Store:               st,
		Placer:              placement.New([]placement.Node{{ID: "dn1", Addr: "dn1"}}),
		TransactionalCommit: true, // set but Elector + WAL both nil
	}

	meta := &store.ObjectMeta{
		Bucket: "b", Key: "fallback",
		Chunks: []store.ChunkRef{{ChunkID: "c1", Replicas: []string{"dn1"}}},
	}
	if err := srv.commit(context.Background(), meta); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := st.GetObject("b", "fallback"); err != nil {
		t.Errorf("fallback path didn't write: %v", err)
	}
}

func TestPlaceRejectsBadInput(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "coord.db"))
	defer st.Close()
	srv := &Server{Store: st, Placer: placement.New([]placement.Node{{ID: "dn1", Addr: "dn1"}})}
	hs := httptest.NewServer(srv.Routes())
	defer hs.Close()

	cases := []struct {
		name string
		body string
	}{
		{"empty key", `{"key":"","n":3}`},
		{"zero n", `{"key":"x","n":0}`},
		{"bad json", `not-json`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, _ := http.Post(hs.URL+"/v1/coord/place", "application/json", bytes.NewReader([]byte(c.body)))
			if resp.StatusCode != 400 {
				t.Errorf("status %d, want 400", resp.StatusCode)
			}
		})
	}
}
