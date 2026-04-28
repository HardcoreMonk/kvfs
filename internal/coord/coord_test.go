// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package coord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

// ADR-042 (Season 5 Ep.7): bulk admin endpoints let kvfs-cli read coord
// state directly without touching edge or opening bbolt files. Verifies
// objects + dns endpoints return what's actually in the store.
func TestAdminEndpoints_ListObjectsAndDNs(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "coord.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	if err := st.PutObject(&store.ObjectMeta{
		Bucket: "b", Key: "k1",
		Chunks: []store.ChunkRef{{ChunkID: "c1", Replicas: []string{"dn1:8080"}}},
	}); err != nil {
		t.Fatalf("seed put: %v", err)
	}
	if err := st.AddRuntimeDN("dn4:8080"); err != nil {
		t.Fatalf("seed dn: %v", err)
	}

	srv := &Server{
		Store:  st,
		Placer: placement.New([]placement.Node{{ID: "dn1", Addr: "dn1"}}),
	}
	hs := httptest.NewServer(srv.Routes())
	defer hs.Close()

	resp, err := http.Get(hs.URL + "/v1/coord/admin/objects")
	if err != nil {
		t.Fatalf("admin/objects: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("admin/objects status %d", resp.StatusCode)
	}
	var objs []store.ObjectMeta
	if err := json.NewDecoder(resp.Body).Decode(&objs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if len(objs) != 1 || objs[0].Bucket != "b" || objs[0].Key != "k1" {
		t.Errorf("admin/objects body wrong: %+v", objs)
	}

	resp, err = http.Get(hs.URL + "/v1/coord/admin/dns")
	if err != nil {
		t.Fatalf("admin/dns: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("admin/dns status %d", resp.StatusCode)
	}
	var dns []string
	if err := json.NewDecoder(resp.Body).Decode(&dns); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	found := false
	for _, a := range dns {
		if a == "dn4:8080" {
			found = true
		}
	}
	if !found {
		t.Errorf("admin/dns missing dn4:8080: %v", dns)
	}
}

// ADR-043 (Season 6 Ep.1): coord computes the rebalance plan directly
// against its own placement + meta. Seed an object whose recorded
// replicas don't match HRW's current pick → the endpoint must return
// at least one migration entry referencing that chunk.
func TestRebalancePlan_DetectsMisplacedChunk(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "coord.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	// 5 DNs. Seed the chunk with only 1 replica → HRW top-3 always
	// proposes 2 migrations (the missing set is non-empty by construction,
	// regardless of which DN HRW happens to rank first).
	nodes := []placement.Node{
		{ID: "dn1:8080", Addr: "dn1:8080"},
		{ID: "dn2:8080", Addr: "dn2:8080"},
		{ID: "dn3:8080", Addr: "dn3:8080"},
		{ID: "dn4:8080", Addr: "dn4:8080"},
		{ID: "dn5:8080", Addr: "dn5:8080"},
	}
	srv := &Server{
		Store:  st,
		Placer: placement.New(nodes),
	}

	if err := st.PutObject(&store.ObjectMeta{
		Bucket: "b", Key: "k",
		Chunks: []store.ChunkRef{
			{ChunkID: "fixed-chunk", Size: 100, Replicas: []string{"dn1:8080"}},
		},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	hs := httptest.NewServer(srv.Routes())
	defer hs.Close()

	resp, err := http.Post(hs.URL+"/v1/coord/admin/rebalance/plan", "application/json", nil)
	if err != nil {
		t.Fatalf("plan request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var plan struct {
		Scanned    int `json:"scanned"`
		Migrations []struct {
			Bucket  string   `json:"bucket"`
			Key     string   `json:"key"`
			ChunkID string   `json:"chunk_id"`
			Missing []string `json:"missing"`
		} `json:"migrations"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&plan); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if plan.Scanned != 1 {
		t.Errorf("scanned=%d, want 1", plan.Scanned)
	}
	if len(plan.Migrations) == 0 {
		t.Fatalf("expected at least one migration (single-replica chunk vs R=3 desired)")
	}
	if plan.Migrations[0].ChunkID != "fixed-chunk" {
		t.Errorf("migration chunk_id = %q, want fixed-chunk", plan.Migrations[0].ChunkID)
	}
}

// ADR-047 (Season 6 Ep.5): mutating registry admin on coord. add → list
// → class → remove round-trip via the new admin endpoints. No leader
// (no Elector wired) so requireLeader is a no-op gate.
func TestAdminMutateDNRegistry(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "coord.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	srv := &Server{Store: st, Placer: placement.New([]placement.Node{{ID: "dn1", Addr: "dn1"}})}
	hs := httptest.NewServer(srv.Routes())
	defer hs.Close()

	// add
	resp, err := http.Post(hs.URL+"/v1/coord/admin/dns?addr=dn9:8080", "application/json", nil)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("add status %d", resp.StatusCode)
	}
	resp.Body.Close()

	// list — should now include dn9
	resp, _ = http.Get(hs.URL + "/v1/coord/admin/dns")
	var list []string
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	resp.Body.Close()
	found := false
	for _, a := range list {
		if a == "dn9:8080" {
			found = true
		}
	}
	if !found {
		t.Errorf("after add, list = %v, want dn9:8080 present", list)
	}

	// class
	req, _ := http.NewRequest(http.MethodPut,
		hs.URL+"/v1/coord/admin/dns/class?addr=dn9:8080&class=hot", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("class: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("class status %d", resp.StatusCode)
	}
	resp.Body.Close()

	got, err := st.RuntimeDNClass("dn9:8080")
	if err != nil {
		t.Fatalf("read class: %v", err)
	}
	if got != "hot" {
		t.Errorf("class = %q, want hot", got)
	}

	// remove
	req, _ = http.NewRequest(http.MethodDelete, hs.URL+"/v1/coord/admin/dns?addr=dn9:8080", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("remove status %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp, _ = http.Get(hs.URL + "/v1/coord/admin/dns")
	list = nil
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	for _, a := range list {
		if a == "dn9:8080" {
			t.Errorf("after remove, dn9:8080 still in list: %v", list)
		}
	}
}

// ADR-048 (Season 6 Ep.6): URLKey rotation on coord. Round-trip the
// rotate/list/remove endpoints. Verifies primary-flag handling (a fresh
// rotate should clear the previous primary) and the not-found delete
// path returns ErrNotFound (404).
func TestAdminURLKeyRotateListRemove(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "coord.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	srv := &Server{Store: st, Placer: placement.New([]placement.Node{{ID: "dn1", Addr: "dn1"}})}
	hs := httptest.NewServer(srv.Routes())
	defer hs.Close()

	// rotate v1 (primary)
	body, _ := json.Marshal(RotateURLKeyRequest{Kid: "v1", SecretHex: "deadbeef00", IsPrimary: true})
	resp, err := http.Post(hs.URL+"/v1/coord/admin/urlkey/rotate", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("rotate v1: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("rotate v1 status %d", resp.StatusCode)
	}

	// rotate v2 (primary) — store should clear v1's primary flag
	body, _ = json.Marshal(RotateURLKeyRequest{Kid: "v2", SecretHex: "feedface00", IsPrimary: true})
	resp, err = http.Post(hs.URL+"/v1/coord/admin/urlkey/rotate", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("rotate v2: %v", err)
	}
	resp.Body.Close()

	// list — should show 2 kids, only v2 primary
	resp, _ = http.Get(hs.URL + "/v1/coord/admin/urlkey")
	var keys []store.URLKeyEntry
	if err := json.NewDecoder(resp.Body).Decode(&keys); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	resp.Body.Close()
	if len(keys) != 2 {
		t.Fatalf("list = %d kids, want 2", len(keys))
	}
	primaries := 0
	for _, k := range keys {
		if k.IsPrimary {
			primaries++
		}
	}
	if primaries != 1 {
		t.Errorf("primaries = %d, want exactly 1", primaries)
	}

	// remove v1 (was primary, now secondary)
	req, _ := http.NewRequest(http.MethodDelete, hs.URL+"/v1/coord/admin/urlkey?kid=v1", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("remove v1: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("remove v1 status %d", resp.StatusCode)
	}

	// remove unknown kid → 404
	req, _ = http.NewRequest(http.MethodDelete, hs.URL+"/v1/coord/admin/urlkey?kid=v99", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("remove v99: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("remove unknown kid status %d, want 404", resp.StatusCode)
	}
}

// /simplify regression: dns add MUST refresh the in-memory Placer (and
// Coord.UpdateNodes when wired). Without this fix, all subsequent
// placement decisions used the boot-time DN set forever — adding a
// runtime DN would persist to bbolt but never actually receive any
// chunks (HRW didn't know it existed).
func TestAdminAddDN_RefreshesInMemoryPlacer(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "coord.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	// Seed the boot-time DN list (matches what main.go would do).
	for _, addr := range []string{"dn1:8080", "dn2:8080"} {
		if err := st.AddRuntimeDN(addr); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	bootNodes := []placement.Node{
		{ID: "dn1:8080", Addr: "dn1:8080"},
		{ID: "dn2:8080", Addr: "dn2:8080"},
	}
	srv := &Server{Store: st, Placer: placement.New(bootNodes)}
	hs := httptest.NewServer(srv.Routes())
	defer hs.Close()

	// Sanity: before add, Pick over a chunk_id should return only dn1+dn2.
	pre := srv.Placer.Pick("any-chunk", 3)
	if len(pre) != 2 {
		t.Fatalf("pre-add Pick should return 2 (only 2 DNs known), got %d", len(pre))
	}

	// Admin add dn3.
	resp, err := http.Post(hs.URL+"/v1/coord/admin/dns?addr=dn3:8080", "application/json", nil)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("add status %d", resp.StatusCode)
	}

	// Post-fix: srv.Placer must have been refreshed; Pick should now
	// see dn3 as a candidate.
	post := srv.Placer.Pick("any-chunk", 3)
	if len(post) != 3 {
		t.Errorf("post-add Pick = %d nodes, want 3 (dn3 should be picked)", len(post))
	}
	hasDN3 := false
	for _, n := range post {
		if n.Addr == "dn3:8080" {
			hasDN3 = true
		}
	}
	if !hasDN3 {
		t.Errorf("post-add Pick = %v, want one to be dn3:8080", post)
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

// ADR-062 (P8-15): /metrics surface and the per-event recorders. nil-safe
// when SetupMetrics hasn't run; counters render in Prometheus text format
// after it has, with the labels we feed in.
func TestMetrics_NilSafeAndRenderAfterSetup(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "coord.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	srv := &Server{Store: st, Placer: placement.New([]placement.Node{{ID: "dn1:8080", Addr: "dn1:8080"}})}

	// Before SetupMetrics: every recorder must no-op (no panic, no allocation
	// surfaces). Hit the /metrics endpoint and expect 200 + empty body.
	srv.recordAudit()
	srv.recordRepair("missing")
	srv.recordUnrecoverable()
	srv.recordThrottled()
	srv.recordAutoRepairRun()

	hs := httptest.NewServer(srv.Routes())
	defer hs.Close()
	resp, err := http.Get(hs.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics pre-setup: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("pre-setup status %d, want 200", resp.StatusCode)
	}

	// Now wire metrics + bump several counters, then re-render.
	srv.SetupMetrics()
	srv.recordAudit()
	srv.recordAudit()
	srv.recordRepair("missing")
	srv.recordRepair("corrupt")
	srv.recordRepair("ec")
	srv.recordUnrecoverable()
	srv.recordThrottled()
	srv.recordThrottled()
	srv.recordAutoRepairRun()

	resp2, err := http.Get(hs.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics post-setup: %v", err)
	}
	defer resp2.Body.Close()
	body := make([]byte, 32*1024)
	n, _ := resp2.Body.Read(body)
	out := string(body[:n])

	// Spot-check: each metric name appears with the expected value. The
	// renderer is exercised in internal/metrics tests; here we just confirm
	// the Server-level wiring + label tuples land correctly.
	wants := []string{
		"kvfs_anti_entropy_audits_total 2",
		`kvfs_anti_entropy_repairs_total{reason="missing"} 1`,
		`kvfs_anti_entropy_repairs_total{reason="corrupt"} 1`,
		`kvfs_anti_entropy_repairs_total{reason="ec"} 1`,
		"kvfs_anti_entropy_unrecoverable_total 1",
		"kvfs_anti_entropy_throttled_total 2",
		"kvfs_anti_entropy_auto_repair_runs_total 1",
	}
	for _, w := range wants {
		if !bytes.Contains([]byte(out), []byte(w)) {
			t.Errorf("missing %q in /metrics output:\n%s", w, out)
		}
	}
}

// ADR-063 (P8-16): unrecoverable dedupe — markUnrecoverableFirstSeen must
// return true only the first time a chunk_id is observed, then false on
// re-occurrence. Cap-induced reset is also exercised.
func TestUnrecoverableDedupe_FirstSeenOnceThenSilent(t *testing.T) {
	srv := &Server{}
	if !srv.markUnrecoverableFirstSeen("aaa") {
		t.Fatal("first call must return true")
	}
	if srv.markUnrecoverableFirstSeen("aaa") {
		t.Error("second call for same id must return false")
	}
	if !srv.markUnrecoverableFirstSeen("bbb") {
		t.Error("first call for different id must return true")
	}
	// Force the cap reset path: stuff cap-many unique entries then verify
	// the map cleared and re-alerts on a previously-seen chunk_id. Uses
	// fmt-formatted ids to guarantee uniqueness without rune-encoding traps.
	for i := 0; i < unrecoverableSeenCap+1; i++ {
		srv.markUnrecoverableFirstSeen(fmt.Sprintf("filler-%d", i))
	}
	if !srv.markUnrecoverableFirstSeen("aaa") {
		t.Error("after cap reset, re-discovered chunks must re-alert")
	}
}

// ADR-063: histograms + skipped counter render correctly via /metrics.
// Ties together the new SetupMetrics counters/histograms and the recordX
// helpers — confirms wiring + Prometheus shape.
func TestMetrics_HistogramsAndSkippedRender(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "coord.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	srv := &Server{Store: st, Placer: placement.New([]placement.Node{{ID: "dn1:8080", Addr: "dn1:8080"}})}
	srv.SetupMetrics()

	srv.recordAuditDuration(150 * time.Millisecond)
	srv.recordRepairDuration(2500 * time.Millisecond)
	srv.recordSkipped("skip")
	srv.recordSkipped("ec-deferred")
	srv.recordSkipped("skip")

	hs := httptest.NewServer(srv.Routes())
	defer hs.Close()
	resp, err := http.Get(hs.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	body := make([]byte, 32*1024)
	n, _ := resp.Body.Read(body)
	out := string(body[:n])

	wants := []string{
		`kvfs_anti_entropy_skipped_total{mode="skip"} 2`,
		`kvfs_anti_entropy_skipped_total{mode="ec-deferred"} 1`,
		`kvfs_anti_entropy_audit_duration_seconds_bucket{le="0.25"} 1`,
		`kvfs_anti_entropy_audit_duration_seconds_count 1`,
		`kvfs_anti_entropy_repair_duration_seconds_bucket{le="2.5"} 1`,
		`kvfs_anti_entropy_repair_duration_seconds_bucket{le="+Inf"} 1`,
	}
	for _, w := range wants {
		if !bytes.Contains([]byte(out), []byte(w)) {
			t.Errorf("missing %q in /metrics output", w)
		}
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
