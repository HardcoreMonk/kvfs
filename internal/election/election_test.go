// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package election

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestVoteRulesStaleTerm: stale candidate is rejected.
func TestVoteRulesStaleTerm(t *testing.T) {
	e := New(Config{SelfID: "self", Peers: []Peer{{ID: "self", URL: ""}}, Log: quietLog()})
	e.mu.Lock()
	e.currentTerm = 5
	e.mu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/?term=3&candidate=other", nil)
	rr := httptest.NewRecorder()
	e.HandleVote(rr, req)
	if !contains(rr.Body.String(), `"vote_granted":false`) {
		t.Errorf("stale term vote: want false, body=%s", rr.Body.String())
	}
	if !contains(rr.Body.String(), `"term":5`) {
		t.Errorf("response should expose currentTerm=5, body=%s", rr.Body.String())
	}
}

// TestVoteRulesGrantOnce: only one vote per term.
func TestVoteRulesGrantOnce(t *testing.T) {
	e := New(Config{SelfID: "self", Peers: []Peer{{ID: "self", URL: ""}}, Log: quietLog()})

	// First vote granted.
	req := httptest.NewRequest(http.MethodPost, "/?term=1&candidate=A", nil)
	rr := httptest.NewRecorder()
	e.HandleVote(rr, req)
	if !contains(rr.Body.String(), `"vote_granted":true`) {
		t.Fatalf("first vote: want true, body=%s", rr.Body.String())
	}

	// Same term, different candidate: rejected.
	req = httptest.NewRequest(http.MethodPost, "/?term=1&candidate=B", nil)
	rr = httptest.NewRecorder()
	e.HandleVote(rr, req)
	if !contains(rr.Body.String(), `"vote_granted":false`) {
		t.Errorf("second vote same term: want false, body=%s", rr.Body.String())
	}

	// Same candidate idempotent: allowed.
	req = httptest.NewRequest(http.MethodPost, "/?term=1&candidate=A", nil)
	rr = httptest.NewRecorder()
	e.HandleVote(rr, req)
	if !contains(rr.Body.String(), `"vote_granted":true`) {
		t.Errorf("re-vote same candidate: want true, body=%s", rr.Body.String())
	}
}

// TestHigherTermStepDown: receiving higher-term vote/HB resets state.
func TestHigherTermStepDown(t *testing.T) {
	e := New(Config{SelfID: "self", Peers: []Peer{{ID: "self", URL: ""}}, Log: quietLog()})
	e.mu.Lock()
	e.state = Leader
	e.currentTerm = 3
	e.leader = "self"
	e.mu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/?term=10&candidate=other", nil)
	rr := httptest.NewRecorder()
	e.HandleVote(rr, req)

	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.state != Follower {
		t.Errorf("state=%s want Follower (stepped down on higher term)", e.state)
	}
	if e.currentTerm != 10 {
		t.Errorf("currentTerm=%d want 10", e.currentTerm)
	}
	if e.votedFor != "other" {
		t.Errorf("votedFor=%q want other", e.votedFor)
	}
}

// TestHeartbeatResetsTimer: receiving HB updates lastHB and clears leader.
func TestHeartbeatResetsTimer(t *testing.T) {
	e := New(Config{SelfID: "self", Peers: []Peer{{ID: "self", URL: ""}}, Log: quietLog()})
	e.mu.Lock()
	e.lastHB = time.Now().Add(-1 * time.Hour)
	e.mu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/?term=1&leader=L", nil)
	rr := httptest.NewRecorder()
	e.HandleHeartbeat(rr, req)

	e.mu.RLock()
	defer e.mu.RUnlock()
	if time.Since(e.lastHB) > time.Second {
		t.Errorf("lastHB not reset: %v ago", time.Since(e.lastHB))
	}
	if e.leader != "L" {
		t.Errorf("leader=%q want L", e.leader)
	}
	if e.state != Follower {
		t.Errorf("state=%s want Follower", e.state)
	}
}

// TestStaleHeartbeatRejected: HB with old term doesn't change state.
func TestStaleHeartbeatRejected(t *testing.T) {
	e := New(Config{SelfID: "self", Peers: []Peer{{ID: "self", URL: ""}}, Log: quietLog()})
	e.mu.Lock()
	e.currentTerm = 5
	e.leader = "current"
	e.mu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/?term=3&leader=stale", nil)
	rr := httptest.NewRecorder()
	e.HandleHeartbeat(rr, req)

	if !contains(rr.Body.String(), `"vote_granted":false`) {
		t.Errorf("stale HB: want rejection, body=%s", rr.Body.String())
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.leader != "current" {
		t.Errorf("leader changed to %q (should remain 'current')", e.leader)
	}
}

// TestLiveCluster3 spins up 3 in-process electors via httptest and checks that
// exactly one becomes leader within a reasonable timeout.
func TestLiveCluster3(t *testing.T) {
	if testing.Short() {
		t.Skip("live cluster test")
	}

	servers := make([]*httptest.Server, 3)
	electors := make([]*Elector, 3)

	// Build URLs first so peers can refer to each other.
	urls := []string{}
	for i := 0; i < 3; i++ {
		s := httptest.NewServer(nil)
		t.Cleanup(s.Close)
		servers[i] = s
		urls = append(urls, s.URL)
	}
	peers := []Peer{}
	for i, u := range urls {
		peers = append(peers, Peer{ID: u, URL: u})
		_ = i
	}
	for i := 0; i < 3; i++ {
		e := New(Config{
			SelfID:             urls[i],
			Peers:              peers,
			HeartbeatInterval:  100 * time.Millisecond,
			ElectionTimeoutMin: 300 * time.Millisecond,
			ElectionTimeoutMax: 600 * time.Millisecond,
			Log:                quietLog(),
		})
		electors[i] = e
		mux := http.NewServeMux()
		mux.HandleFunc("POST /v1/election/vote", e.HandleVote)
		mux.HandleFunc("POST /v1/election/heartbeat", e.HandleHeartbeat)
		mux.HandleFunc("GET /v1/election/state", e.HandleState)
		servers[i].Config.Handler = mux
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, e := range electors {
		go e.Run(ctx)
	}

	// Wait up to 5s for exactly one leader.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		leaders := 0
		for _, e := range electors {
			if e.IsLeader() {
				leaders++
			}
		}
		if leaders == 1 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, e := range electors {
		t.Logf("final state: self=%s state=%s term=%d leader=%s",
			e.cfg.SelfID, e.State(), e.CurrentTerm(), e.Leader())
	}
	t.Fatalf("no single leader emerged within 5s")
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || (len(s) > 0 && substring(s, substr)))
}

func substring(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
