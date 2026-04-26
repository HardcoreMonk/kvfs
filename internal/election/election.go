// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Package election implements Raft-style leader election for multi-edge HA
// (ADR-031, follow-up to ADR-022). It does NOT include log replication —
// only the leader-election subset of Raft, which is enough to drive
// ADR-022's snapshot-pull replication automatically.
//
// 비전공자용 해설
// ──────────────
// ADR-022 가 follower edge 가 primary 의 snapshot 을 pull 하는 구조를 만들었지만
// failover 는 운영자 수동 (env 갱신 + restart). 본 패키지는 그 결정을 자동화한다.
//
// 동작:
//   - 모든 edge 가 같은 peer set (EDGE_PEERS) 을 알고 있음
//   - 한 명만 leader, 나머지는 follower
//   - Leader 가 주기적 heartbeat 보냄 → follower 의 election timer reset
//   - heartbeat 끊기면 (leader 죽음) follower 가 candidate 로 전환
//   - Candidate 가 term 올리고 peer 들에게 vote 요청
//   - quorum (N/2+1) 받으면 leader 등극, 못 받으면 follower 로 복귀
//
// State 전이:
//
//   Follower ──(timeout)──▶ Candidate ──(quorum)──▶ Leader
//      ▲                       │                       │
//      │                       │ (lost)                │
//      └───────(higher term)───┴───────────────────────┘
//
// Term 의 의미: 매 새 election 시 ++. 옛 term 의 vote/heartbeat 은 거부.
// 한 term 에 한 번만 vote 가능. 이 두 invariant 가 split-brain 방지.
//
// MVP 가정:
//   - peer set 은 정적 (env, 운영 중 변경 X)
//   - log replication 없음 — leadership 만 결정
//   - 메타 sync 는 ADR-022 snapshot-pull 그대로 (LeaderURL 가 동적으로 갱신됨)
package election

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	mathrand "math/rand"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// State is the current role in the election state machine.
type State int

const (
	Follower State = iota
	Candidate
	Leader
)

func (s State) String() string {
	switch s {
	case Follower:
		return "follower"
	case Candidate:
		return "candidate"
	case Leader:
		return "leader"
	}
	return "unknown"
}

// Peer is one edge endpoint in the cluster (including self).
type Peer struct {
	ID  string // stable identifier (typically the URL itself)
	URL string // base URL, e.g., "http://edge1:8000"
}

// Config bundles tunables. Defaults (zero) are reasonable for ~3-5 edges.
type Config struct {
	SelfID             string
	Peers              []Peer        // includes self
	HeartbeatInterval  time.Duration // leader → followers cadence (default 500ms)
	ElectionTimeoutMin time.Duration // follower→candidate trigger (default 1500ms)
	ElectionTimeoutMax time.Duration // (default 3000ms)
	HTTPClient         *http.Client  // for vote/heartbeat outbound calls
	Log                *slog.Logger
	// ReplicateTimeout is the per-call deadline for ReplicateEntry's quorum
	// wait (default 2s). Override only if peer RTT exceeds the default.
	ReplicateTimeout time.Duration
	// AppendEntryFn applies a pushed WAL entry to local state. Set at
	// construction; passed through to the AppendWAL HTTP handler.
	AppendEntryFn AppendEntryFunc
}

// Elector runs the election state machine.
type Elector struct {
	cfg        Config
	httpClient *http.Client
	rng        *mathrand.Rand

	// hbCh is signalled (non-blocking) when a valid heartbeat or vote-grant
	// arrives, waking the follower loop instead of polling. Buffer 1 so a
	// tight burst doesn't drop notifications below the wake threshold.
	hbCh chan struct{}

	// Mutable state — protected by mu.
	mu          sync.RWMutex
	state       State
	currentTerm uint64
	votedFor    string    // candidate ID this term (empty = none)
	leader      string    // current leader ID (empty = unknown)
	lastHB      time.Time // last heartbeat received (or sent if leader)
	appendFn    AppendEntryFunc
}

// New constructs an Elector. Peers must include self (matched by SelfID).
func New(cfg Config) *Elector {
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = 500 * time.Millisecond
	}
	if cfg.ElectionTimeoutMin <= 0 {
		cfg.ElectionTimeoutMin = 1500 * time.Millisecond
	}
	if cfg.ElectionTimeoutMax <= cfg.ElectionTimeoutMin {
		cfg.ElectionTimeoutMax = 3000 * time.Millisecond
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 1 * time.Second}
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.ReplicateTimeout <= 0 {
		cfg.ReplicateTimeout = 2 * time.Second
	}
	return &Elector{
		cfg:        cfg,
		httpClient: cfg.HTTPClient,
		rng:        mathrand.New(mathrand.NewSource(time.Now().UnixNano())),
		hbCh:       make(chan struct{}, 1),
		state:      Follower,
		lastHB:     time.Now(),
		appendFn:   cfg.AppendEntryFn,
	}
}

// Run blocks until ctx is cancelled, driving the state machine.
func (e *Elector) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		switch e.State() {
		case Follower:
			e.runFollower(ctx)
		case Candidate:
			e.runCandidate(ctx)
		case Leader:
			e.runLeader(ctx)
		}
	}
}

// State returns the current role.
func (e *Elector) State() State {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.state
}

// Leader returns the current leader's ID (empty if unknown).
func (e *Elector) Leader() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.leader
}

// LeaderURL returns the URL of the current leader (or "" if unknown).
func (e *Elector) LeaderURL() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.leader == "" {
		return ""
	}
	for _, p := range e.cfg.Peers {
		if p.ID == e.leader {
			return p.URL
		}
	}
	return ""
}

// IsLeader reports whether this edge is the current leader.
func (e *Elector) IsLeader() bool { return e.State() == Leader }

// CurrentTerm returns the current term (for diagnostics / /v1/election/state).
func (e *Elector) CurrentTerm() uint64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.currentTerm
}

func (e *Elector) randomElectionTimeout() time.Duration {
	min := e.cfg.ElectionTimeoutMin
	max := e.cfg.ElectionTimeoutMax
	delta := max - min
	return min + time.Duration(e.rng.Int63n(int64(delta)))
}

// runFollower waits for either a heartbeat (resetting the timer) or the
// election timeout (transitioning to candidate). Heartbeat resets are
// signalled via e.hbCh by HandleHeartbeat / HandleVote — no polling.
func (e *Elector) runFollower(ctx context.Context) {
	timeout := e.randomElectionTimeout()
	t := time.NewTimer(timeout)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// No heartbeat received within timeout window: become candidate.
			e.mu.Lock()
			if e.state != Follower {
				e.mu.Unlock()
				return
			}
			e.state = Candidate
			e.mu.Unlock()
			return
		case <-e.hbCh:
			// Reset election timer (drain pending tick first if needed).
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
			t.Reset(timeout)
		}
	}
}

// signalHeartbeat is called from HandleHeartbeat / HandleVote (after they
// update lastHB) to wake runFollower. Non-blocking — buffer 1 absorbs the
// burst case where multiple resets arrive while the follower is processing.
func (e *Elector) signalHeartbeat() {
	select {
	case e.hbCh <- struct{}{}:
	default:
	}
}

type voteResponse struct {
	VoteGranted bool   `json:"vote_granted"`
	Term        uint64 `json:"term"`
}

func (e *Elector) runCandidate(ctx context.Context) {
	e.mu.Lock()
	e.currentTerm++
	e.votedFor = e.cfg.SelfID
	e.leader = ""
	term := e.currentTerm
	e.mu.Unlock()

	e.cfg.Log.Info("election: campaigning",
		slog.String("self", e.cfg.SelfID), slog.Uint64("term", term))

	votes := 1 // self
	var voteMu sync.Mutex
	var wg sync.WaitGroup
	voteCtx, cancel := context.WithTimeout(ctx, e.randomElectionTimeout())
	defer cancel()

	for _, peer := range e.cfg.Peers {
		if peer.ID == e.cfg.SelfID {
			continue
		}
		wg.Add(1)
		go func(p Peer) {
			defer wg.Done()
			ok, peerTerm, err := e.requestVote(voteCtx, p, term)
			if err != nil {
				e.cfg.Log.Debug("election: vote request failed",
					slog.String("peer", p.ID), slog.String("err", err.Error()))
				return
			}
			if peerTerm > term {
				// Step down: someone is on a higher term.
				e.mu.Lock()
				if peerTerm > e.currentTerm {
					e.currentTerm = peerTerm
					e.votedFor = ""
					e.state = Follower
					e.leader = ""
				}
				e.mu.Unlock()
				return
			}
			if ok {
				voteMu.Lock()
				votes++
				voteMu.Unlock()
			}
		}(peer)
	}
	wg.Wait()

	if ctx.Err() != nil {
		return
	}
	quorum := len(e.cfg.Peers)/2 + 1
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state != Candidate || e.currentTerm != term {
		// Stepped down during voting (saw higher term).
		return
	}
	if votes >= quorum {
		e.state = Leader
		e.leader = e.cfg.SelfID
		e.cfg.Log.Info("election: WON, becoming leader",
			slog.String("self", e.cfg.SelfID),
			slog.Uint64("term", term),
			slog.Int("votes", votes), slog.Int("quorum", quorum))
		return
	}
	e.state = Follower
	e.lastHB = time.Now()
	e.cfg.Log.Info("election: lost, back to follower",
		slog.Uint64("term", term),
		slog.Int("votes", votes), slog.Int("quorum", quorum))
}

func (e *Elector) runLeader(ctx context.Context) {
	t := time.NewTicker(e.cfg.HeartbeatInterval)
	defer t.Stop()
	// Send an immediate heartbeat so followers learn of the new leader fast.
	e.broadcastHeartbeat(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if e.State() != Leader {
			return
		}
		e.broadcastHeartbeat(ctx)
	}
}

func (e *Elector) broadcastHeartbeat(ctx context.Context) {
	e.mu.RLock()
	term := e.currentTerm
	e.mu.RUnlock()
	for _, peer := range e.cfg.Peers {
		if peer.ID == e.cfg.SelfID {
			continue
		}
		go func(p Peer) {
			peerTerm, err := e.sendHeartbeat(ctx, p, term)
			if err != nil {
				e.cfg.Log.Debug("election: heartbeat failed",
					slog.String("peer", p.ID), slog.String("err", err.Error()))
				return
			}
			if peerTerm > term {
				e.mu.Lock()
				if peerTerm > e.currentTerm {
					e.currentTerm = peerTerm
					e.votedFor = ""
					e.state = Follower
					e.leader = ""
				}
				e.mu.Unlock()
			}
		}(peer)
	}
}

func (e *Elector) requestVote(ctx context.Context, peer Peer, term uint64) (bool, uint64, error) {
	url := fmt.Sprintf("%s/v1/election/vote?term=%d&candidate=%s", peer.URL, term, e.cfg.SelfID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return false, 0, err
	}
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return false, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, 0, fmt.Errorf("vote: HTTP %d", resp.StatusCode)
	}
	var v voteResponse
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return false, 0, err
	}
	return v.VoteGranted, v.Term, nil
}

func (e *Elector) sendHeartbeat(ctx context.Context, peer Peer, term uint64) (uint64, error) {
	url := fmt.Sprintf("%s/v1/election/heartbeat?term=%d&leader=%s", peer.URL, term, e.cfg.SelfID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("heartbeat: HTTP %d", resp.StatusCode)
	}
	var v voteResponse // reuse: success/term shape
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return 0, err
	}
	return v.Term, nil
}

// HandleVote serves POST /v1/election/vote?term=X&candidate=Y.
//
// Vote rules (Raft):
//   1. If incoming term < currentTerm → reject (stale candidate)
//   2. If incoming term > currentTerm → step down + reset votedFor
//   3. Vote granted iff (votedFor == "" || votedFor == candidate)
func (e *Elector) HandleVote(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	term, err := parseTerm(q.Get("term"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	candidate := q.Get("candidate")
	if candidate == "" {
		http.Error(w, "missing candidate", http.StatusBadRequest)
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if term < e.currentTerm {
		writeVote(w, false, e.currentTerm)
		return
	}
	if term > e.currentTerm {
		e.currentTerm = term
		e.votedFor = ""
		e.state = Follower
		e.leader = ""
	}
	if e.votedFor == "" || e.votedFor == candidate {
		e.votedFor = candidate
		e.lastHB = time.Now() // grant resets election timer
		writeVote(w, true, e.currentTerm)
		// Wake follower loop (non-blocking; buffer 1 absorbs the case where
		// the previous signal wasn't yet consumed).
		defer e.signalHeartbeat()
		return
	}
	writeVote(w, false, e.currentTerm)
}

// HandleHeartbeat serves POST /v1/election/heartbeat?term=X&leader=Y.
//
// Heartbeat rules:
//   1. If term < currentTerm → reject (stale leader)
//   2. If term >= currentTerm → accept; become follower; reset election timer
func (e *Elector) HandleHeartbeat(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	term, err := parseTerm(q.Get("term"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	leader := q.Get("leader")

	e.mu.Lock()
	defer e.mu.Unlock()
	if term < e.currentTerm {
		writeVote(w, false, e.currentTerm)
		return
	}
	if term > e.currentTerm {
		e.currentTerm = term
		e.votedFor = ""
	}
	e.state = Follower
	e.leader = leader
	e.lastHB = time.Now()
	writeVote(w, true, e.currentTerm)
	defer e.signalHeartbeat()
}

// AppendEntryFunc is called by HandleAppendWAL on followers to apply a
// received entry to local state. Set via Config.AppendEntryFn at New time.
type AppendEntryFunc func(entryBody []byte) error

// HandleAppendWAL serves POST /v1/election/append-wal?term=X with a JSON
// body = one WAL entry. Used by the leader to synchronously replicate
// writes to followers (ADR-031 follow-up — toward strong consistency).
//
// Rules:
//   1. Stale term → reject 409 (caller will downgrade or step down).
//   2. Higher term → adopt + step down to follower, then apply.
//   3. Same term → apply via the registered AppendEntryFunc.
func (e *Elector) HandleAppendWAL(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	term, err := parseTerm(q.Get("term"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	e.mu.Lock()
	if term < e.currentTerm {
		curTerm := e.currentTerm
		e.mu.Unlock()
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "term": curTerm, "reason": "stale_term"})
		return
	}
	if term > e.currentTerm {
		e.currentTerm = term
		e.votedFor = ""
		e.state = Follower
		e.lastHB = time.Now()
	}
	fn := e.appendFn
	e.mu.Unlock()

	if fn == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "reason": "no append fn"})
		return
	}
	if err := fn(body); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "reason": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "term": term})
}

// ReplicateEntry pushes one WAL entry body (raw JSON) to all peers in parallel.
// Returns nil if quorum (N/2+1 including self) acked within Config.ReplicateTimeout,
// else an error. Called by edge.Server's WAL hook on the leader.
//
// Best-effort semantics for now:
//   - Leader has already locally committed (bbolt + WAL append).
//   - This RPC informs peers; quorum-ack improves durability vs pure pull.
//   - On quorum failure, error is returned but the leader's local state is
//     unchanged — caller decides whether to surface to the client.
func (e *Elector) ReplicateEntry(ctx context.Context, entryBody []byte) error {
	if e.State() != Leader {
		return fmt.Errorf("not leader")
	}
	e.mu.RLock()
	term := e.currentTerm
	e.mu.RUnlock()
	pctx, cancel := context.WithTimeout(ctx, e.cfg.ReplicateTimeout)
	defer cancel()

	acks := 1 // self
	var ackMu sync.Mutex
	var wg sync.WaitGroup
	for _, peer := range e.cfg.Peers {
		if peer.ID == e.cfg.SelfID {
			continue
		}
		wg.Add(1)
		go func(p Peer) {
			defer wg.Done()
			if e.pushAppendWAL(pctx, p, term, entryBody) {
				ackMu.Lock()
				acks++
				ackMu.Unlock()
			}
		}(peer)
	}
	wg.Wait()

	quorum := len(e.cfg.Peers)/2 + 1
	if acks >= quorum {
		return nil
	}
	return fmt.Errorf("replicate: only %d/%d acks (need %d)", acks, len(e.cfg.Peers), quorum)
}

func (e *Elector) pushAppendWAL(ctx context.Context, peer Peer, term uint64, body []byte) bool {
	url := fmt.Sprintf("%s/v1/election/append-wal?term=%d", peer.URL, term)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// HandleState serves GET /v1/election/state for diagnostics.
func (e *Elector) HandleState(w http.ResponseWriter, r *http.Request) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	resp := map[string]any{
		"self":         e.cfg.SelfID,
		"state":        e.state.String(),
		"current_term": e.currentTerm,
		"voted_for":    e.votedFor,
		"leader":       e.leader,
		"last_hb":      e.lastHB,
		"peers":        e.cfg.Peers,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func parseTerm(s string) (uint64, error) {
	if s == "" {
		return 0, fmt.Errorf("missing term")
	}
	return strconv.ParseUint(s, 10, 64)
}

func writeVote(w http.ResponseWriter, granted bool, term uint64) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(voteResponse{VoteGranted: granted, Term: term})
}
