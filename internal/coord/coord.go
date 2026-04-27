// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Package coord is the kvfs-coord daemon's HTTP RPC surface.
//
// 비전공자용 해설
// ──────────────
// ADR-015 가 Season 5 의 architectural pivot: edge 가 게이트웨이 + coordinator
// + meta-store 3역 하던 걸 coord 라는 별도 daemon 으로 분리. edge 는 thin
// gateway 가 됨. cmd/kvfs-coord/main.go 의 top doc 에 책임 분담 + boot env
// 매트릭스. 본 패키지는 그 daemon 의 HTTP handler 모음.
//
// Season 5 (메타 분리) — Ep.1~7
//   Ep.1 ADR-015: skeleton + place/commit/lookup/delete RPC
//   Ep.2 ADR-038-앞: edge → coord meta proxy (CoordClient, EDGE_COORD_URL)
//   Ep.3 ADR-038: coord HA via Raft (election + leader redirect)
//   Ep.4 ADR-039: coord-to-coord WAL replication (best-effort)
//   Ep.5 ADR-040: transactional commit (replicate-then-commit, true strict)
//   Ep.6 ADR-041: edge → coord placement RPC (single source of truth)
//   Ep.7 ADR-042: cli direct coord admin (read-only inspect)
//
// Season 6 (운영 분리) — Ep.1~7
//   Ep.1 ADR-043: rebalance plan on coord (no DN I/O)
//   Ep.2 ADR-044: rebalance apply (COORD_DN_IO=1 → coordinator embed)
//   Ep.3 ADR-045: GC plan + apply
//   Ep.4 ADR-046: EC repair (Reed-Solomon Reconstruct)
//   Ep.5 ADR-047: DN registry mutation (add/remove/class)
//   Ep.6 ADR-048: URLKey kid registry (rotate/list/remove)
//   Ep.7 ADR-049: edge urlkey.Signer propagation (polling)
//
// HTTP route 카탈로그 (현재 코드의 Routes() 와 1:1 일치):
//   POST /v1/coord/place                                  Ep.1 — placement
//   POST /v1/coord/commit                                 Ep.1 — meta write
//   GET  /v1/coord/lookup?bucket=&key=                    Ep.1 — meta read
//   POST /v1/coord/delete                                 Ep.1 — meta delete
//   GET  /v1/coord/healthz                                Ep.1 — liveness + role
//   POST /v1/election/vote                                Ep.3 — Raft vote RPC
//   POST /v1/election/heartbeat                           Ep.3 — Raft heartbeat
//   POST /v1/election/append-wal                          Ep.4 — WAL push
//   GET  /v1/coord/admin/objects                          Ep.7 — bulk meta dump
//   GET  /v1/coord/admin/dns                              Ep.7 — DN list (read)
//   POST /v1/coord/admin/rebalance/plan                   S6 Ep.1 — plan
//   POST /v1/coord/admin/rebalance/apply?concurrency=N    S6 Ep.2 — apply (DN I/O)
//   POST /v1/coord/admin/gc/{plan,apply}?min-age=         S6 Ep.3
//   POST /v1/coord/admin/repair/{plan,apply}?concurrency= S6 Ep.4
//   POST   /v1/coord/admin/dns?addr=                      S6 Ep.5 — leader-only
//   DELETE /v1/coord/admin/dns?addr=                      S6 Ep.5 — leader-only
//   PUT    /v1/coord/admin/dns/class?addr=&class=         S6 Ep.5 — leader-only
//   GET    /v1/coord/admin/urlkey                         S6 Ep.6 — kid list
//   POST   /v1/coord/admin/urlkey/rotate                  S6 Ep.6 — leader-only
//   DELETE /v1/coord/admin/urlkey?kid=                    S6 Ep.6 — leader-only
//
// 두 가지 gate:
//   requireLeader  — HA mode (Elector 활성) 의 mutating RPC 가 follower 에 오면
//                    503 + X-COORD-LEADER 헤더로 리다이렉트
//   requireDNIO    — apply 계열 (rebalance/gc/repair) 이 COORD_DN_IO 미설정 시
//                    503 (coord 가 DN HTTP 클라이언트 없음)
//
// Coord field 는 옵셔널 (COORD_DN_IO=1 일 때만 wired). Plan 계열은 어댑터
// (rebalancePlanCoord) 로 placer-only fallback. 자세한 거 Server 정의 + 각
// handler 위 ADR 코멘트.
package coord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/HardcoreMonk/kvfs/internal/coordinator"
	"github.com/HardcoreMonk/kvfs/internal/election"
	"github.com/HardcoreMonk/kvfs/internal/gc"
	"github.com/HardcoreMonk/kvfs/internal/httputil"
	"github.com/HardcoreMonk/kvfs/internal/placement"
	"github.com/HardcoreMonk/kvfs/internal/rebalance"
	"github.com/HardcoreMonk/kvfs/internal/repair"
	"github.com/HardcoreMonk/kvfs/internal/store"
)

// Server bundles the coord daemon's owned state (placement + meta store)
// and serves the HTTP RPC surface defined by ADR-015 / Season 5 Ep.1.
//
// Placer is replaced (not mutated in-place) on every dns admin change
// so handlePlace / rebalanceCoord stay coherent under concurrent
// reads. dnsAdminMu serializes the persist+refresh flow in
// refreshDNTopology — gates the swap so two concurrent adds don't
// stomp each other's view.
type Server struct {
	Store  *store.MetaStore
	Placer *placement.Placer
	Log    *slog.Logger

	dnsAdminMu sync.Mutex

	// Coord is the optional DN-I/O backend (Season 6 Ep.2, ADR-044).
	// When non-nil, rebalance/gc/repair APPLY paths can run on coord
	// directly instead of going back through edge. PLAN paths only need
	// Placer + Store and work without it.
	Coord *coordinator.Coordinator

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
	// ADR-042 (Season 5 Ep.7): bulk read-only admin endpoints so kvfs-cli
	// (and any operator script) can talk to coord directly instead of
	// going through edge. Coord owns the truth — the cli should ask the
	// owner.
	mux.HandleFunc("GET /v1/coord/admin/objects", s.handleAdminObjects)
	mux.HandleFunc("GET /v1/coord/admin/dns", s.handleAdminDNs)
	// P8-06: snapshot endpoint mirroring edge's /v1/admin/meta/snapshot
	// (ADR-014). A coord that just booted with empty/stale bbolt can pull
	// the current state from any peer (preferably leader). Closes the
	// follower-bootstrap-fetch gap chaos-mixed surfaced.
	mux.HandleFunc("GET /v1/coord/admin/meta/snapshot", s.handleAdminSnapshot)
	// ADR-043 (Season 6 Ep.1): rebalance plan computed by coord directly.
	// Read-only — apply still on edge until coord grows DN I/O capability.
	mux.HandleFunc("POST /v1/coord/admin/rebalance/plan", s.handleRebalancePlan)
	// ADR-044 (Season 6 Ep.2): rebalance apply via coord's own DN I/O.
	// Requires Coord field (which embeds the DN HTTP client). 503 if unset.
	mux.HandleFunc("POST /v1/coord/admin/rebalance/apply", s.handleRebalanceApply)
	// ADR-045 (Season 6 Ep.3): GC. Plan needs Coord (for ListChunks),
	// apply needs Coord (for DeleteChunkFrom). Both 503 if Coord nil.
	mux.HandleFunc("POST /v1/coord/admin/gc/plan", s.handleGCPlan)
	mux.HandleFunc("POST /v1/coord/admin/gc/apply", s.handleGCApply)
	// ADR-046 (Season 6 Ep.4): EC repair. Both 503 if Coord nil.
	mux.HandleFunc("POST /v1/coord/admin/repair/plan", s.handleRepairPlan)
	mux.HandleFunc("POST /v1/coord/admin/repair/apply", s.handleRepairApply)
	// ADR-047 (Season 6 Ep.5): mutating registry admin on coord.
	// DN add/remove/class. coord owns the registry — cli should mutate
	// it directly rather than going through edge.
	mux.HandleFunc("POST /v1/coord/admin/dns", s.handleAdminAddDN)
	mux.HandleFunc("DELETE /v1/coord/admin/dns", s.handleAdminRemoveDN)
	mux.HandleFunc("PUT /v1/coord/admin/dns/class", s.handleAdminSetDNClass)
	// ADR-051 (Season 7 Ep.1): failure-domain label per DN. Mirror of
	// the class endpoint above. Empty domain query value clears the label.
	// Triggers refreshDNTopology so the next placement uses the new view.
	mux.HandleFunc("PUT /v1/coord/admin/dns/domain", s.handleAdminSetDNDomain)
	// ADR-048 (Season 6 Ep.6): URLKey rotation on coord. Read endpoints
	// (list) — anyone. Mutate (rotate, remove) — leader-only.
	mux.HandleFunc("GET /v1/coord/admin/urlkey", s.handleAdminListURLKeys)
	mux.HandleFunc("POST /v1/coord/admin/urlkey/rotate", s.handleAdminRotateURLKey)
	mux.HandleFunc("DELETE /v1/coord/admin/urlkey", s.handleAdminRemoveURLKey)
	// ADR-054 (Season 7 Ep.4): one-shot anti-entropy audit. Leader-only
	// because it walks the authoritative ObjectMeta. Returns per-DN diff
	// of expected vs actual chunk inventory using DN-side Merkle trees.
	mux.HandleFunc("POST /v1/coord/admin/anti-entropy/run", s.handleAntiEntropyRun)
	// ADR-055: detect-and-repair. Reads from a healthy replica, PUTs to
	// the affected DN. Replication chunks only; EC stripes are reported
	// as skipped (use the existing repair worker — ADR-046).
	mux.HandleFunc("POST /v1/coord/admin/anti-entropy/repair", s.handleAntiEntropyRepair)
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

// requireDNIO is the gate for handlers that need coord-side DN I/O
// (rebalance/gc/repair apply). Returns true if the request was rejected
// (caller must return immediately). 503 + a message naming the missing
// COORD_DN_IO env so operators get a clear path to fix.
func (s *Server) requireDNIO(w http.ResponseWriter, op string) bool {
	if s.Coord != nil {
		return false
	}
	writeErr(w, http.StatusServiceUnavailable,
		fmt.Errorf("coord %s: COORD_DN_IO not enabled", op))
	return true
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
	// P7-07 cleanup: when Coord is wired (DN-IO mode), route through it
	// so PlaceN / PlaceChunk all see the same placer instance (apply
	// uses Coord; plan uses this). Two parallel placers used to drift
	// silently after refresh — single source of truth.
	var addrs []string
	if s.Coord != nil {
		addrs = s.Coord.PlaceN(req.Key, req.N)
	} else {
		// PickByDomain auto-fastpaths to legacy HRW when no Domain set
		// (ADR-051). Both branches behave identically pre-domain-tagging.
		nodes := s.Placer.PickByDomain(req.Key, req.N)
		addrs = make([]string, 0, len(nodes))
		for _, n := range nodes {
			addrs = append(addrs, n.Addr)
		}
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
// handleAdminObjects returns every object's metadata. Same shape as
// edge's GET /v1/admin/objects but served by coord (the owner). For
// MVP no pagination; large clusters will need ?limit=&since= later.
func (s *Server) handleAdminObjects(w http.ResponseWriter, _ *http.Request) {
	objs, err := s.Store.ListObjects()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, objs)
}

// handleAdminSnapshot streams the bbolt snapshot to the response body
// (P8-06). A booting coord can pull this from any peer to bootstrap
// its state before joining the election — closes the follower-restart
// catch-up gap. Mirror of edge's /v1/admin/meta/snapshot (ADR-014),
// same Store.Snapshot path so behavior is identical.
//
// No leader gate — followers are happy to serve the snapshot, and the
// caller may not yet know who is leader. Auth: same admin trust model
// as the rest of /v1/coord/admin/* (cluster-internal network).
func (s *Server) handleAdminSnapshot(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/octet-stream")
	if _, err := s.Store.Snapshot(w); err != nil {
		// Snapshot streaming may have already written headers; log and
		// give up gracefully. Caller will see truncated body and retry.
		s.Log.Warn("handleAdminSnapshot stream failed", "err", err)
	}
}

// handleAdminDNs returns the runtime DN list (addrs + class labels)
// from coord's bbolt registry. Mirrors edge's GET /v1/admin/dns.
// handleAdminDNs returns the runtime DN registry. Default body shape is
// []string (back-compat with kvfs-cli inspect that expects a flat array).
// Add `?detailed=1` for [{addr, domain}] — used by demo-samekh to verify
// failure-domain placement.
func (s *Server) handleAdminDNs(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("detailed") == "1" {
		withDomain, err := s.Store.ListRuntimeDNsWithDomain()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		// Stable sort for deterministic output (test fixtures, golden tests).
		addrs := make([]string, 0, len(withDomain))
		for a := range withDomain {
			addrs = append(addrs, a)
		}
		sort.Strings(addrs)
		out := make([]map[string]string, 0, len(addrs))
		for _, a := range addrs {
			out = append(out, map[string]string{"addr": a, "domain": withDomain[a]})
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	dns, err := s.Store.ListRuntimeDNs()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, dns)
}

// handleRebalancePlan computes a rebalance plan from coord's authoritative
// metadata + placement. Read-only: no DN I/O happens. When Coord is set
// (Ep.2+), uses the real Coordinator directly — its PlaceN/PlaceChunk
// answers match what apply would use. When Coord is nil (Ep.1 single-coord
// mode), falls back to the placer-only adapter.
func (s *Server) handleRebalancePlan(w http.ResponseWriter, _ *http.Request) {
	rc := s.rebalanceCoord()
	plan, err := rebalance.ComputePlan(rc, s.Store, s.Store)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

// handleRebalanceApply executes a freshly-computed rebalance plan using
// coord's DN I/O. Requires Coord != nil; without it, the apply path
// can't move chunks (placement-only adapter errors on Read/PutChunkTo).
//
// Concurrency knob via ?concurrency=N (default 4, matches edge default).
func (s *Server) handleRebalanceApply(w http.ResponseWriter, r *http.Request) {
	if s.requireDNIO(w, "rebalance apply") {
		return
	}
	concurrency := 4
	if v := r.URL.Query().Get("concurrency"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			concurrency = n
		}
	}
	plan, err := rebalance.ComputePlan(s.Coord, s.Store, s.Store)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	stats := rebalance.Run(r.Context(), s.Coord, s.Store, plan, concurrency)
	writeJSON(w, http.StatusOK, stats)
}

// handleGCPlan + handleGCApply: ADR-045 Season 6 Ep.3. GC needs DN
// inventory (ListChunks) so both endpoints require Coord != nil.
//
// Query params:
//   ?min-age=DURATION  default 5m (mirror of edge's EDGE_AUTO_GC_MIN_AGE)
//   ?concurrency=N     apply only; default 4
func (s *Server) handleGCPlan(w http.ResponseWriter, r *http.Request) {
	if s.requireDNIO(w, "gc plan") {
		return
	}
	minAge := parseDurationQuery(r, "min-age", 5*time.Minute)
	plan, err := gc.ComputePlan(r.Context(), s.Coord, s.Store, minAge)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) handleGCApply(w http.ResponseWriter, r *http.Request) {
	if s.requireDNIO(w, "gc apply") {
		return
	}
	minAge := parseDurationQuery(r, "min-age", 5*time.Minute)
	concurrency := parseIntQuery(r, "concurrency", 4)
	plan, err := gc.ComputePlan(r.Context(), s.Coord, s.Store, minAge)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	stats := gc.Run(r.Context(), s.Coord, plan, concurrency)
	writeJSON(w, http.StatusOK, stats)
}

// handleAdminAddDN / RemoveDN / SetDNClass: ADR-047 Season 6 Ep.5.
// Coord owns the runtime DN registry; mutations go straight to
// MetaStore.AddRuntimeDN / RemoveRuntimeDN / SetRuntimeDNClass which
// already journal through the WAL (so multi-coord HA picks up changes
// via Ep.4 WAL replication).
//
// In HA mode (Ep.3) only the leader accepts these — same requireLeader
// gate as commit/delete.
func (s *Server) handleAdminAddDN(w http.ResponseWriter, r *http.Request) {
	if s.requireLeader(w) {
		return
	}
	addr := r.URL.Query().Get("addr")
	if addr == "" {
		writeErr(w, http.StatusBadRequest, errors.New("addr query param required"))
		return
	}
	if err := s.Store.AddRuntimeDN(addr); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.refreshDNTopology(); err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("refresh placer: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"added": addr})
}

func (s *Server) handleAdminRemoveDN(w http.ResponseWriter, r *http.Request) {
	if s.requireLeader(w) {
		return
	}
	addr := r.URL.Query().Get("addr")
	if addr == "" {
		writeErr(w, http.StatusBadRequest, errors.New("addr query param required"))
		return
	}
	if err := s.Store.RemoveRuntimeDN(addr); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.refreshDNTopology(); err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("refresh placer: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"removed": addr})
}

// refreshDNTopology rebuilds the in-memory placement view from the
// authoritative bbolt registry. Without this, s.Placer / s.Coord stay
// frozen at boot-time COORD_DNS even after dns add/remove mutations
// — placement decisions go to the wrong DNs forever.
//
// Edge (internal/edge/edge.go::applyDNChange) does the equivalent atomic
// persist+refresh in one helper. Coord persists via Store first (so HA
// followers see it via WAL) and then refreshes the local view here.
//
// Note: there's a small inconsistency window — between Store.Add* and
// refreshDNTopology(), placement still sees the old set. Acceptable for
// admin operations (low frequency). For higher consistency a writer-mu
// would help; deferred.
func (s *Server) refreshDNTopology() error {
	s.dnsAdminMu.Lock()
	defer s.dnsAdminMu.Unlock()
	// Pull addr→domain map so the rebuilt Placer knows failure-domain
	// labels (ADR-051). Falls back to plain ListRuntimeDNs on legacy
	// stores (impossible in normal flow but cheap defensiveness).
	withDomain, err := s.Store.ListRuntimeDNsWithDomain()
	if err != nil {
		return err
	}
	addrs := make([]string, 0, len(withDomain))
	for addr := range withDomain {
		addrs = append(addrs, addr)
	}
	sort.Strings(addrs) // deterministic placement seed
	nodes := make([]placement.Node, 0, len(addrs))
	for _, addr := range addrs {
		nodes = append(nodes, placement.Node{ID: addr, Addr: addr, Domain: withDomain[addr]})
	}
	s.Placer = placement.New(nodes)
	if s.Coord != nil {
		if err := s.Coord.UpdateNodes(nodes); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) handleAdminSetDNClass(w http.ResponseWriter, r *http.Request) {
	if s.requireLeader(w) {
		return
	}
	addr := r.URL.Query().Get("addr")
	class := r.URL.Query().Get("class")
	if addr == "" {
		writeErr(w, http.StatusBadRequest, errors.New("addr query param required"))
		return
	}
	if err := s.Store.SetRuntimeDNClass(addr, class); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"addr": addr, "class": class})
}

// handleAdminSetDNDomain (ADR-051) — sets/clears the failure-domain label
// for a registered DN. Empty domain unlabels (DN goes back to the
// "default" pool). Triggers refreshDNTopology so the next handlePlace
// sees the new layout — without that the Placer's in-memory Domain
// stays at boot value forever.
func (s *Server) handleAdminSetDNDomain(w http.ResponseWriter, r *http.Request) {
	if s.requireLeader(w) {
		return
	}
	addr := r.URL.Query().Get("addr")
	domain := r.URL.Query().Get("domain")
	if addr == "" {
		writeErr(w, http.StatusBadRequest, errors.New("addr query param required"))
		return
	}
	if err := s.Store.SetRuntimeDNDomain(addr, domain); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.refreshDNTopology(); err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("refresh: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"addr": addr, "domain": domain})
}

// handleAdminListURLKeys / Rotate / Remove: ADR-048 Season 6 Ep.6.
// URLKey kid registry mutation owned by coord. Mirrors the dns admin
// pattern from Ep.5 — list is open, mutate requires leader, all journal
// to bbolt (and thus to follower coords via WAL replication).
func (s *Server) handleAdminListURLKeys(w http.ResponseWriter, _ *http.Request) {
	keys, err := s.Store.ListURLKeys()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, keys)
}

// RotateURLKeyRequest is the body of POST /v1/coord/admin/urlkey/rotate.
// kid + secret_hex are operator-supplied (typically generated by `kvfs-cli
// urlkey rotate` which auto-generates if not given). When IsPrimary=true,
// store atomically clears the old primary flag.
type RotateURLKeyRequest struct {
	Kid       string `json:"kid"`
	SecretHex string `json:"secret_hex"`
	IsPrimary bool   `json:"is_primary"`
}

func (s *Server) handleAdminRotateURLKey(w http.ResponseWriter, r *http.Request) {
	if s.requireLeader(w) {
		return
	}
	var req RotateURLKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("decode: %w", err))
		return
	}
	if req.Kid == "" || req.SecretHex == "" {
		writeErr(w, http.StatusBadRequest, errors.New("kid and secret_hex required"))
		return
	}
	if err := s.Store.PutURLKey(req.Kid, req.SecretHex, req.IsPrimary); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kid":        req.Kid,
		"is_primary": req.IsPrimary,
	})
}

func (s *Server) handleAdminRemoveURLKey(w http.ResponseWriter, r *http.Request) {
	if s.requireLeader(w) {
		return
	}
	kid := r.URL.Query().Get("kid")
	if kid == "" {
		writeErr(w, http.StatusBadRequest, errors.New("kid query param required"))
		return
	}
	if err := s.Store.DeleteURLKey(kid); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"removed": kid})
}

// handleRepairPlan + handleRepairApply: ADR-046 Season 6 Ep.4. EC repair
// scans every stripe for missing/unresponsive shards and (apply path)
// reconstructs them via Reed-Solomon. Both 503 if Coord nil.
func (s *Server) handleRepairPlan(w http.ResponseWriter, r *http.Request) {
	if s.requireDNIO(w, "repair plan") {
		return
	}
	plan, err := repair.ComputePlan(s.Coord, s.Store)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) handleRepairApply(w http.ResponseWriter, r *http.Request) {
	if s.requireDNIO(w, "repair apply") {
		return
	}
	concurrency := parseIntQuery(r, "concurrency", 4)
	plan, err := repair.ComputePlan(s.Coord, s.Store)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	stats := repair.Run(r.Context(), s.Coord, s.Store, plan, concurrency)
	writeJSON(w, http.StatusOK, stats)
}

func parseDurationQuery(r *http.Request, key string, def time.Duration) time.Duration {
	if v := r.URL.Query().Get(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func parseIntQuery(r *http.Request, key string, def int) int {
	if v := r.URL.Query().Get(key); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// rebalanceCoord picks the rebalance.Coordinator implementation based on
// what's wired: real Coord if available, placer-only adapter otherwise.
// Plan path can use either; apply path only works with real Coord.
func (s *Server) rebalanceCoord() rebalance.Coordinator {
	if s.Coord != nil {
		return s.Coord
	}
	return &rebalancePlanCoord{placer: s.Placer}
}

// rebalancePlanCoord adapts coord's Placer to rebalance.Coordinator for
// the PLAN path only. ReadChunk/PutChunkTo panic with a clear message —
// they would only be hit by Run (apply), which Ep.1 keeps on edge.
//
// `r` (replication factor) for PlaceChunk: rebalance only calls PlaceChunk
// when planChunks falls back from the class-subset path; in that case it
// passes len(chunk.Replicas) which == R per the existing chunk's own
// recorded count. For a coord-side stub we mirror placement.Pick with the
// chunk's existing R — captured by the call site, not us. The interface
// signature doesn't take an n though, so we hardcode the conventional R=3
// here. Future eps can wire EDGE_REPLICATION_FACTOR equivalent.
type rebalancePlanCoord struct{ placer *placement.Placer }

func (a *rebalancePlanCoord) PlaceChunk(chunkID string) []string {
	const defaultR = 3
	nodes := a.placer.Pick(chunkID, defaultR)
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Addr)
	}
	return out
}

func (a *rebalancePlanCoord) PlaceN(key string, n int) []string {
	nodes := a.placer.Pick(key, n)
	out := make([]string, 0, len(nodes))
	for _, nd := range nodes {
		out = append(out, nd.Addr)
	}
	return out
}

func (a *rebalancePlanCoord) PlaceNFromAddrs(key string, n int, addrs []string) []string {
	nodes := make([]placement.Node, 0, len(addrs))
	for _, addr := range addrs {
		nodes = append(nodes, placement.Node{ID: addr, Addr: addr})
	}
	picked := placement.PickFromNodes(key, n, nodes)
	out := make([]string, 0, len(picked))
	for _, nd := range picked {
		out = append(out, nd.Addr)
	}
	return out
}

func (a *rebalancePlanCoord) ReadChunk(_ context.Context, _ string, _ []string) ([]byte, string, error) {
	return nil, "", errors.New("coord rebalance: ReadChunk not implemented (apply path lives on edge in Ep.1)")
}

func (a *rebalancePlanCoord) PutChunkTo(_ context.Context, _, _ string, _ []byte) error {
	return errors.New("coord rebalance: PutChunkTo not implemented (apply path lives on edge in Ep.1)")
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	body := map[string]string{"status": "ok"}
	if s.Elector != nil {
		body["role"] = s.Elector.State().String()
		body["leader_url"] = s.Elector.LeaderURL()
	}
	writeJSON(w, http.StatusOK, body)
}

// Local shims to avoid touching every callsite. The helpers themselves
// live in internal/httputil for cross-daemon reuse.
func writeJSON(w http.ResponseWriter, status int, body any) { httputil.WriteJSON(w, status, body) }
func writeErr(w http.ResponseWriter, status int, err error) { httputil.WriteErr(w, status, err) }

