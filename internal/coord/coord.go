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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/HardcoreMonk/kvfs/internal/placement"
	"github.com/HardcoreMonk/kvfs/internal/store"
)

// Server bundles the coord daemon's owned state (placement + meta store)
// and serves the HTTP RPC surface defined by ADR-015 / Season 5 Ep.1.
type Server struct {
	Store  *store.MetaStore
	Placer *placement.Placer
	Log    *slog.Logger
}

// Routes returns a configured ServeMux. Caller wires it into http.Server.
func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/coord/place", s.handlePlace)
	mux.HandleFunc("POST /v1/coord/commit", s.handleCommit)
	mux.HandleFunc("GET /v1/coord/lookup", s.handleLookup)
	mux.HandleFunc("POST /v1/coord/delete", s.handleDelete)
	mux.HandleFunc("GET /v1/coord/healthz", s.handleHealthz)
	return mux
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
// edge multi-instance setups didn't have.
func (s *Server) handleCommit(w http.ResponseWriter, r *http.Request) {
	var req CommitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("decode: %w", err))
		return
	}
	if req.Meta == nil || req.Meta.Bucket == "" || req.Meta.Key == "" {
		writeErr(w, http.StatusBadRequest, errors.New("meta.bucket+key required"))
		return
	}
	if err := s.Store.PutObject(req.Meta); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, CommitResponse{OK: true, Version: req.Meta.Version})
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

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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

// readBody is a small helper for tests/clients that want raw bytes.
func readBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(r.Body)
}

var _ = readBody // currently unused; kept for the test helper to call.
