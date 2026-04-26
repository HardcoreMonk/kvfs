// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Package coord is the kvfs-coord daemon's HTTP RPC surface.
//
// Season 5 Ep.1 (ADR-015) — first separation of placement + metadata
// ownership out of edge into a dedicated daemon. Edge becomes a thin
// gateway that calls coord via HTTP for every metadata mutation and
// every placement decision.
//
// 비전공자용 해설
// ──────────────
// Season 1~4 의 kvfs-edge 는 게이트웨이 + coordinator + meta-store 의 3역.
// 단일 edge 의 bbolt single-writer 가 메타 throughput 의 천장이었고, 멀티-edge
// HA 도 snapshot-pull / Raft 같은 우회로 풀어왔다.
//
// ADR-015 의 결정: coord 를 떼낸다. 책임 재배치:
//
//   kvfs-edge   — HTTP 종단, UrlKey 검증, chunker/EC encoder, DN I/O
//   kvfs-coord  — placement (HRW), 메타 (bbolt), 결정의 단일 출처
//   kvfs-dn     — chunk byte 저장 (변동 없음)
//
// Ep.1 의 minimal RPC:
//   POST /v1/coord/place   {key, n}    → {addrs:[...]}
//   POST /v1/coord/commit  {meta}      → {ok:true, version:N}
//   GET  /v1/coord/lookup?bucket&key   → {meta}  (or 404)
//   POST /v1/coord/delete  {bucket,key} → {ok:true}
//   GET  /v1/coord/healthz
//
// 이후 episode 가 PlaceN, EC stripe placement, runtime DN registry 추가.
package coord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/HardcoreMonk/kvfs/internal/election"
	"github.com/HardcoreMonk/kvfs/internal/placement"
	"github.com/HardcoreMonk/kvfs/internal/store"
)

// Server bundles the coord daemon's owned state (placement + meta store)
// and serves the HTTP RPC surface defined by ADR-015 / Season 5 Ep.1.
type Server struct {
	Store  *store.MetaStore
	Placer *placement.Placer
	Log    *slog.Logger

	// Elector is optional. When set (Season 5 Ep.3, ADR-038), this coord
	// participates in a multi-coord election and only the current leader
	// accepts mutating RPCs. Followers return 503 with a X-COORD-LEADER
	// header pointing at the leader's URL — clients (edge.CoordClient)
	// transparently follow.
	Elector *election.Elector

	// TransactionalCommit (Season 5 Ep.5, ADR-040) flips PutObject from
	// "commit-then-push" (best-effort, ADR-039) to "replicate-then-commit"
	// (true Raft-style). Closes the leader-loss-mid-write phantom-write
	// window: if quorum push fails, the leader's bbolt is NOT touched.
	// Requires Elector + WAL to be active. Default false (Ep.4 behavior).
	TransactionalCommit bool
}

// transactionalCommitTimeout caps each transactional commit's quorum wait.
// Matches election.Config's default; not exposed as a field until a real
// operator request shows up.
const transactionalCommitTimeout = 2 * time.Second

// HeaderCoordLeader names the redirect header used when a follower coord
// returns 503 on a write — value is the current leader's base URL (from
// COORD_PEERS). Empty when no leader is known yet (election in progress).
const HeaderCoordLeader = "X-COORD-LEADER"

// Routes returns a configured ServeMux. Caller wires it into http.Server.
// When Elector is set, election RPCs are mounted on /v1/election/* (same
// paths the elector hardcodes for outbound calls).
func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/coord/place", s.handlePlace)
	mux.HandleFunc("POST /v1/coord/commit", s.handleCommit)
	mux.HandleFunc("GET /v1/coord/lookup", s.handleLookup)
	mux.HandleFunc("POST /v1/coord/delete", s.handleDelete)
	mux.HandleFunc("GET /v1/coord/healthz", s.handleHealthz)
	if s.Elector != nil {
		mux.HandleFunc("POST /v1/election/vote", s.Elector.HandleVote)
		mux.HandleFunc("POST /v1/election/heartbeat", s.Elector.HandleHeartbeat)
		// ADR-039: leader pushes each WAL entry to followers via this RPC.
		// Wired only when election is active; the inbound side runs on every
		// coord (followers receive, leader's own pushes go to peers).
		mux.HandleFunc("POST /v1/election/append-wal", s.Elector.HandleAppendWAL)
	}
	return mux
}

// requireLeader is the gate for mutating RPCs in HA mode. Returns true if
// the request was rejected (caller must return immediately). When Elector
// is nil (single-coord mode), always returns false (allow).
func (s *Server) requireLeader(w http.ResponseWriter) bool {
	if s.Elector == nil {
		return false
	}
	if s.Elector.IsLeader() {
		return false
	}
	if leader := s.Elector.LeaderURL(); leader != "" {
		w.Header().Set(HeaderCoordLeader, leader)
	}
	writeErr(w, http.StatusServiceUnavailable, errors.New("not the coord leader"))
	return true
}

// PlaceRequest is the body of POST /v1/coord/place.
type PlaceRequest struct {
	Key string `json:"key"` // chunk_id or stripe_id
	N   int    `json:"n"`   // how many DNs to pick (R for replication, K+M for EC)
}

// PlaceResponse is the response body.
type PlaceResponse struct {
	Addrs []string `json:"addrs"`
}

// handlePlace returns the top-N DN addresses for a key, computed by the
// coord's authoritative Placer. Edge calls this once per chunk/stripe.
func (s *Server) handlePlace(w http.ResponseWriter, r *http.Request) {
	var req PlaceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("decode: %w", err))
		return
	}
	if req.Key == "" || req.N <= 0 {
		writeErr(w, http.StatusBadRequest, errors.New("key and n>0 required"))
		return
	}
	nodes := s.Placer.Pick(req.Key, req.N)
	addrs := make([]string, 0, len(nodes))
	for _, n := range nodes {
		addrs = append(addrs, n.Addr)
	}
	writeJSON(w, http.StatusOK, PlaceResponse{Addrs: addrs})
}

// CommitRequest is the body of POST /v1/coord/commit.
type CommitRequest struct {
	Meta *store.ObjectMeta `json:"meta"`
}

// CommitResponse echoes the version assigned by the store.
type CommitResponse struct {
	OK      bool  `json:"ok"`
	Version int64 `json:"version"`
}

// handleCommit persists object metadata. Coord owns the bbolt write, so
// this is the single point of serialization in the cluster — a property
// edge multi-instance setups didn't have. In HA mode (Ep.3), only the
// current leader accepts this; followers reply 503 + X-COORD-LEADER.
//
// Two commit paths (selected by Server.TransactionalCommit):
//
//	default (best-effort, ADR-039):  PutObject → bbolt commit → walHook
//	                                 (push to peers) → respond. Phantom
//	                                 write possible if leadership lost
//	                                 between commit and push.
//
//	transactional (ADR-040):         MarshalPutObjectEntry → ReplicateEntry
//	                                 (wait for quorum ack) → only on
//	                                 success, PutObjectAfterReplicate
//	                                 (commits + writes WAL with hook
//	                                 suppressed since peers already have
//	                                 the entry). Quorum failure → 503,
//	                                 NO local commit. Closes the phantom-
//	                                 write window.
func (s *Server) handleCommit(w http.ResponseWriter, r *http.Request) {
	if s.requireLeader(w) {
		return
	}
	var req CommitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("decode: %w", err))
		return
	}
	if req.Meta == nil || req.Meta.Bucket == "" || req.Meta.Key == "" {
		writeErr(w, http.StatusBadRequest, errors.New("meta.bucket+key required"))
		return
	}
	if err := s.commit(r.Context(), req.Meta); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, CommitResponse{OK: true, Version: req.Meta.Version})
}

// commit dispatches between the best-effort and transactional commit paths.
// Both paths require leadership (already checked by handleCommit). The
// transactional path additionally requires Elector + WAL — without WAL
// there's nothing to replicate, so it falls back to the legacy path.
//
// cmd/kvfs-coord/main.go fatals at startup when TransactionalCommit is
// enabled without both prerequisites; this nil-check is a defensive
// guard for programmatic Server construction (tests, future embedders).
func (s *Server) commit(ctx context.Context, meta *store.ObjectMeta) error {
	if !s.TransactionalCommit || s.Elector == nil || s.Store.WAL() == nil {
		return s.Store.PutObject(meta)
	}
	body, err := s.Store.MarshalPutObjectEntry(meta)
	if err != nil {
		return fmt.Errorf("marshal entry: %w", err)
	}
	repCtx, cancel := context.WithTimeout(ctx, transactionalCommitTimeout)
	defer cancel()
	if err := s.Elector.ReplicateEntry(repCtx, body); err != nil {
		return fmt.Errorf("transactional replicate: %w", err)
	}
	return s.Store.PutObjectAfterReplicate(meta)
}

// handleLookup returns the ObjectMeta for (bucket, key) or 404.
func (s *Server) handleLookup(w http.ResponseWriter, r *http.Request) {
	bucket := r.URL.Query().Get("bucket")
	key := r.URL.Query().Get("key")
	if bucket == "" || key == "" {
		writeErr(w, http.StatusBadRequest, errors.New("bucket and key required"))
		return
	}
	meta, err := s.Store.GetObject(bucket, key)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, meta)
}

// DeleteRequest is the body of POST /v1/coord/delete.
type DeleteRequest struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if s.requireLeader(w) {
		return
	}
	var req DeleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("decode: %w", err))
		return
	}
	if req.Bucket == "" || req.Key == "" {
		writeErr(w, http.StatusBadRequest, errors.New("bucket and key required"))
		return
	}
	if err := s.Store.DeleteObject(req.Bucket, req.Key); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleHealthz returns liveness + (when election is configured) the
// coord's current role and known leader URL. Demos use the role field
// to find the current leader without a side-effecting probe write.
//
//	{"status":"ok"}                                        — single-coord mode
//	{"status":"ok","role":"leader","leader_url":"..."}    — HA mode
//	{"status":"ok","role":"follower","leader_url":"..."}
//	{"status":"ok","role":"candidate","leader_url":""}
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	body := map[string]string{"status": "ok"}
	if s.Elector != nil {
		body["role"] = s.Elector.State().String()
		body["leader_url"] = s.Elector.LeaderURL()
	}
	writeJSON(w, http.StatusOK, body)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

