// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package coord

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

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
