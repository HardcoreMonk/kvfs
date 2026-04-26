// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Package edge is the kvfs-edge HTTP gateway: UrlKey verification + coordinator.
//
// 비전공자용 해설
// ──────────────
// edge = 클러스터의 단일 진입점. 사용자 HTTP 요청을 받아서 1) 인증 (UrlKey,
// ADR-007/028) 2) 청크/EC stripe 분할 (ADR-008/011) 3) DN 으로 fanout 후
// quorum 으로 응답.
//
// Server 구조체에 모든 운영성 모듈이 모인다:
//   - Coord   (coordinator)           : 청크 → DN 라우팅 + quorum
//   - Store   (bbolt 메타)            : 객체 ↔ chunk/stripe 매핑
//   - Signer  (urlkey)                : URL 서명 검증
//   - AutoCfg/autoRuns                : ADR-013 in-edge ticker (rebalance/GC)
//   - Heartbeat                       : ADR-030 DN liveness probe
//   - SnapshotScheduler               : ADR-016 주기 backup
//   - Role/followerSt                 : ADR-022 multi-edge HA
//
// 라우팅 (Routes 함수):
//   - 데이터: PUT/GET/DELETE /v1/o/{bucket}/{key...}  (UrlKey 필수)
//   - 관리:   /v1/admin/*                              (auth 없음 — admin 망 가정)
//
// 핵심 mutex 들 (rebalanceMu/gcMu/repairMu/dnsAdminMu/urlkeyAdminMu/autoMu)
// 은 각 admin endpoint 와 auto-trigger 가 같은 자원을 동시에 건드리지 못하게
// 직렬화. 데이터 path (PUT/GET/DELETE) 는 mutex 안 잡음 — 동시 처리.
package edge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HardcoreMonk/kvfs/internal/chunker"
	"github.com/HardcoreMonk/kvfs/internal/coordinator"
	"github.com/HardcoreMonk/kvfs/internal/election"
	"github.com/HardcoreMonk/kvfs/internal/gc"
	"github.com/HardcoreMonk/kvfs/internal/heartbeat"
	"github.com/HardcoreMonk/kvfs/internal/placement"
	"github.com/HardcoreMonk/kvfs/internal/rebalance"
	"github.com/HardcoreMonk/kvfs/internal/reedsolomon"
	"github.com/HardcoreMonk/kvfs/internal/repair"
	"github.com/HardcoreMonk/kvfs/internal/store"
	"github.com/HardcoreMonk/kvfs/internal/urlkey"
)

// AutoConfig controls the in-edge automatic rebalance + GC loops (ADR-013).
//
// Default zero value: Enabled=false (opt-in). When Enabled, two background
// goroutines run on Server.StartAuto(ctx). Each respects the same mutex used
// by manual /v1/admin/{rebalance,gc}/apply so manual + auto interleave safely.
type AutoConfig struct {
	Enabled           bool
	RebalanceInterval time.Duration // default 5m
	GCInterval        time.Duration // default 15m
	GCMinAge          time.Duration // default gc.DefaultMinAge (60s)
	Concurrency       int           // default 4
}

// AutoJob is a typed enum of the two auto-trigger jobs.
type AutoJob string

const (
	JobRebalance AutoJob = "rebalance"
	JobGC        AutoJob = "gc"
)

// AutoRun records one auto-trigger cycle that did work or errored.
// Empty (no-op) cycles are NOT recorded — they would evict meaningful history
// on long-running idle clusters. Use the lastCheck timestamps for liveness.
type AutoRun struct {
	Job        AutoJob        `json:"job"`
	StartedAt  time.Time      `json:"started_at"`
	DurationMS int64          `json:"duration_ms"`
	PlanSize   int            `json:"plan_size"`
	Stats      map[string]any `json:"stats,omitempty"`
	Error      string         `json:"error,omitempty"`
}

const autoRunHistory = 32

// Server assembles the edge HTTP handlers over a metadata store, a coordinator,
// and a UrlKey signer.
type Server struct {
	Store  *store.MetaStore
	Coord  *coordinator.Coordinator
	Signer *urlkey.Signer
	Log    *slog.Logger

	// ChunkSize is bytes-per-chunk for fixed-mode PUT splitting (ADR-011).
	// Zero / negative falls back to chunker.DefaultChunkSize.
	ChunkSize int

	// CDCEnabled selects content-defined chunking (ADR-018) over fixed.
	// When true, replication-mode handlePut uses chunker.NewCDCReader with
	// CDCConfig (CDCConfig zero value → DefaultCDCConfig). EC mode is always
	// fixed (encoder needs uniform shard sizes).
	CDCEnabled bool

	// CDCConfig tunes the FastCDC parameters (min/normal/max + masks).
	// Ignored when CDCEnabled is false. Zero value falls back to defaults.
	CDCConfig chunker.CDCConfig

	// If true, skip UrlKey verification (for demos/tests). Never enable in public.
	SkipAuth bool

	// AutoCfg controls the auto-trigger loops (ADR-013). See StartAuto.
	AutoCfg AutoConfig

	// rebalanceMu serializes /v1/admin/rebalance/apply so two concurrent
	// triggers don't race on the same chunks. /plan is read-only and excluded.
	rebalanceMu sync.Mutex

	// gcMu serializes /v1/admin/gc/apply for the same reason.
	gcMu sync.Mutex

	// dnsAdminMu serializes /v1/admin/dns POST/DELETE so concurrent
	// add/remove don't race the bbolt persist + Coord.UpdateNodes pair.
	dnsAdminMu sync.Mutex

	// urlkeyAdminMu serializes /v1/admin/urlkey/* so persist + Signer
	// mutation stay consistent.
	urlkeyAdminMu sync.Mutex

	// repairMu serializes /v1/admin/repair/apply (ADR-025).
	repairMu sync.Mutex

	// autoMu protects autoRuns + lastCheck timestamps.
	// Empty cycles update lastCheck only (not autoRuns) so the ring buffer
	// keeps actionable history; lastCheck still proves the loop is alive.
	autoMu                  sync.Mutex
	autoRuns                []AutoRun
	autoLastCheckRebalance  time.Time
	autoLastCheckGC         time.Time

	// Heartbeat is the per-DN liveness monitor (ADR-030). nil disables it.
	// StartHeartbeat wires up the periodic probe loop.
	Heartbeat *heartbeat.Monitor

	// SnapshotScheduler is the in-edge auto-backup ticker (ADR-016). nil
	// disables it. StartSnapshotScheduler wires up the loop.
	SnapshotScheduler *store.SnapshotScheduler

	// Role is "primary" (default) or "follower" (ADR-022 manual mode).
	// When Elector != nil (ADR-031 election mode), this field is ignored —
	// effectiveRole() consults the elector instead.
	Role Role

	// followerSt holds follower mode state. Initialized when Role == follower
	// via SetFollowerConfig. Nil otherwise.
	followerSt *followerState

	// Elector enables automatic leader election (ADR-031). When non-nil,
	// effectiveRole() returns Primary when Elector.IsLeader(), else Follower.
	// The follower sync loop, when present, queries Elector.LeaderURL()
	// dynamically so failover automatically rewires snapshot-pull source.
	Elector *election.Elector
}

// SetFollowerConfig switches Role to follower and registers the sync config.
// Call once before StartFollowerSync (ADR-022 manual mode).
func (s *Server) SetFollowerConfig(cfg FollowerConfig) {
	s.Role = RoleFollower
	s.followerSt = &followerState{cfg: cfg}
}

// SetElectionFollowerSync configures the follower sync loop for election
// mode (ADR-031). PrimaryURL is intentionally empty — it's queried from the
// Elector at each tick so leader changes propagate automatically.
func (s *Server) SetElectionFollowerSync(dataDir string, pullInterval time.Duration) {
	if pullInterval <= 0 {
		pullInterval = 30 * time.Second
	}
	s.followerSt = &followerState{cfg: FollowerConfig{
		DataDir:      dataDir,
		PullInterval: pullInterval,
	}}
}

func (s *Server) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Server) chunkSize() int {
	if s.ChunkSize <= 0 {
		return chunker.DefaultChunkSize
	}
	return s.ChunkSize
}

// pieceReader is the common interface for both fixed and CDC streaming
// chunkers. handlePut treats them uniformly.
type pieceReader interface {
	Next() (*chunker.StreamPiece, error)
}

// newPutReader picks the chunker mode (fixed vs CDC, ADR-018) based on
// Server.CDCEnabled.
func (s *Server) newPutReader(src io.Reader) pieceReader {
	if s.CDCEnabled {
		return chunker.NewCDCReader(src, s.CDCConfig)
	}
	return chunker.NewReader(src, s.chunkSize())
}

// Routes builds the HTTP mux.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/o/{bucket}/{key...}", s.handlePut)
	mux.HandleFunc("GET /v1/o/{bucket}/{key...}", s.handleGet)
	mux.HandleFunc("DELETE /v1/o/{bucket}/{key...}", s.handleDelete)
	mux.HandleFunc("GET /v1/admin/objects", s.handleList)
	mux.HandleFunc("GET /v1/admin/dns", s.handleDNs)
	mux.HandleFunc("POST /v1/admin/dns", s.handleAddDN)
	mux.HandleFunc("DELETE /v1/admin/dns", s.handleRemoveDN)
	mux.HandleFunc("GET /v1/admin/urlkey", s.handleListURLKeys)
	mux.HandleFunc("POST /v1/admin/urlkey/rotate", s.handleRotateURLKey)
	mux.HandleFunc("DELETE /v1/admin/urlkey", s.handleRemoveURLKey)
	mux.HandleFunc("POST /v1/admin/rebalance/plan", s.handleRebalancePlan)
	mux.HandleFunc("POST /v1/admin/rebalance/apply", s.handleRebalanceApply)
	mux.HandleFunc("POST /v1/admin/gc/plan", s.handleGCPlan)
	mux.HandleFunc("POST /v1/admin/gc/apply", s.handleGCApply)
	mux.HandleFunc("POST /v1/admin/repair/plan", s.handleRepairPlan)
	mux.HandleFunc("POST /v1/admin/repair/apply", s.handleRepairApply)
	mux.HandleFunc("GET /v1/admin/auto/status", s.handleAutoStatus)
	mux.HandleFunc("GET /v1/admin/meta/snapshot", s.handleMetaSnapshot)
	mux.HandleFunc("GET /v1/admin/meta/info", s.handleMetaInfo)
	mux.HandleFunc("GET /v1/admin/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("GET /v1/admin/snapshot/history", s.handleSnapshotHistory)
	mux.HandleFunc("GET /v1/admin/role", s.handleRole)
	mux.HandleFunc("GET /v1/admin/wal", s.handleWAL)
	mux.HandleFunc("GET /v1/admin/wal/info", s.handleWALInfo)
	if s.Elector != nil {
		mux.HandleFunc("POST /v1/election/vote", s.Elector.HandleVote)
		mux.HandleFunc("POST /v1/election/heartbeat", s.Elector.HandleHeartbeat)
		mux.HandleFunc("GET /v1/election/state", s.Elector.HandleState)
	}
	mux.HandleFunc("GET /healthz", s.handleHealth)
	return logRequests(mux)
}

func (s *Server) verifyAuth(r *http.Request) error {
	if s.SkipAuth {
		return nil
	}
	return s.Signer.Verify(r.Method, r.URL.Path, r.URL.Query(), time.Now())
}

func (s *Server) handlePut(w http.ResponseWriter, r *http.Request) {
	if s.rejectIfFollowerWrite(w, r) {
		return
	}
	if err := s.verifyAuth(r); err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	bucket := r.PathValue("bucket")
	key := r.PathValue("key")
	if bucket == "" || key == "" {
		writeError(w, http.StatusBadRequest, "bucket and key required")
		return
	}

	// EC mode (ADR-008) — streaming per-stripe (ADR-017 follow-up).
	// Encoder needs uniform K data shards per stripe, so we still pull
	// stripeBytes worth of bytes at a time, but never the whole object.
	if ecHdr := r.Header.Get("X-KVFS-EC"); ecHdr != "" {
		s.handlePutECStream(w, r, bucket, key, ecHdr)
		return
	}

	// Replication mode: stream per chunk (ADR-017). Memory bound = chunkSize
	// regardless of object size. Each chunk gets its own content-addressable id
	// and is placed independently via Rendezvous Hashing.
	//
	// Two chunker modes (ADR-018): fixed (default) or CDC (content-defined).
	// CDC gives shift-invariant dedup at the cost of variable chunk sizes.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	reader := s.newPutReader(r.Body)
	var chunkRefs []store.ChunkRef
	var totalSize int64
	for i := 0; ; i++ {
		piece, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("read chunk %d: %v", i+1, err))
			return
		}
		replicas, werr := s.Coord.WriteChunk(ctx, piece.ID, piece.Data)
		if werr != nil {
			// Partial-success orphan chunks remain on disk; GC (ADR-012) cleans
			// them after min-age. We do NOT commit metadata, so reads can't see
			// a half-written object.
			writeError(w, http.StatusBadGateway,
				fmt.Sprintf("chunk %d (%s): %v", i+1, piece.ID[:16], werr))
			return
		}
		chunkRefs = append(chunkRefs, store.ChunkRef{
			ChunkID:  piece.ID,
			Size:     piece.Size,
			Replicas: replicas,
		})
		totalSize += piece.Size
	}
	if len(chunkRefs) == 0 {
		writeError(w, http.StatusBadRequest, "empty body")
		return
	}

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	meta := &store.ObjectMeta{
		Bucket:      bucket,
		Key:         key,
		Size:        totalSize,
		Chunks:      chunkRefs,
		ContentType: contentType,
	}
	if err := s.Store.PutObject(meta); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"bucket":     bucket,
		"key":        key,
		"size":       totalSize,
		"chunk_size": s.chunkSize(),
		"chunks":     chunkRefs,
		"version":    meta.Version,
	})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	if err := s.verifyAuth(r); err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	bucket := r.PathValue("bucket")
	key := r.PathValue("key")

	meta, err := s.Store.GetObject(bucket, key)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "object not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	// EC-mode objects: reconstruct each stripe from any K surviving shards.
	if meta.IsEC() {
		s.handleGetEC(w, r, ctx, meta)
		return
	}

	// Replication mode: stream each chunk to the response writer (ADR-017).
	// Memory bound = chunkSize per iteration, regardless of object size.
	//
	// Header order matters: once we Write the first byte, headers are flushed.
	// If a mid-stream chunk fetch fails, we cannot change status — only abort
	// the connection so the client sees a truncated body.
	w.Header().Set("Content-Type", meta.ContentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", meta.Size))
	w.Header().Set("X-KVFS-Chunks", fmt.Sprintf("%d", len(meta.Chunks)))
	w.Header().Set("X-KVFS-Version", fmt.Sprintf("%d", meta.Version))

	for i, c := range meta.Chunks {
		data, _, ferr := s.Coord.ReadChunk(ctx, c.ChunkID, c.Replicas)
		if ferr != nil {
			if i == 0 {
				writeError(w, http.StatusBadGateway, ferr.Error())
				return
			}
			s.logger().Error("GET stream aborted",
				slog.String("bucket", bucket), slog.String("key", key),
				slog.Int("chunk_index", i), slog.String("err", ferr.Error()))
			return
		}
		if int64(len(data)) != c.Size {
			s.logger().Error("GET chunk size mismatch",
				slog.String("chunk_id", c.ChunkID),
				slog.Int("got", len(data)), slog.Int64("want", c.Size))
			return
		}
		if _, werr := w.Write(data); werr != nil {
			// Client disconnect — log and stop.
			return
		}
	}
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if s.rejectIfFollowerWrite(w, r) {
		return
	}
	if err := s.verifyAuth(r); err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	bucket := r.PathValue("bucket")
	key := r.PathValue("key")

	meta, err := s.Store.GetObject(bucket, key)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "object not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	// best-effort per-chunk/per-shard delete; continue even if some fail (dead DN)
	for _, c := range meta.Chunks {
		_ = s.Coord.DeleteChunk(ctx, c.ChunkID, c.Replicas)
	}
	for _, st := range meta.Stripes {
		for _, sh := range st.Shards {
			_ = s.Coord.DeleteChunk(ctx, sh.ChunkID, sh.Replicas)
		}
	}

	if err := s.Store.DeleteObject(bucket, key); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	objs, err := s.Store.ListObjects()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, objs)
}

func (s *Server) handleDNs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"dns":          s.Coord.DNs(),
		"quorum_write": s.Coord.QuorumWrite(),
	})
}

// dnsAdminMu serializes admin DN registry mutations so two concurrent add/
// remove calls don't race on the bbolt bucket + Coord.UpdateNodes pair.
// (Reads via handleDNs are unaffected.)
//
// handleAddDN adds a DN addr to the runtime registry (ADR-027).
// Body: {"addr":"dn7:8080"}.
// Returns 200 + the new DN list. Idempotent (re-add updates registered_at only).
func (s *Server) handleAddDN(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Addr string `json:"addr"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Addr == "" {
		writeError(w, http.StatusBadRequest, "missing addr")
		return
	}
	if err := s.applyDNChange(func(addrs []string) ([]string, error) {
		for _, a := range addrs {
			if a == body.Addr {
				return addrs, nil // idempotent
			}
		}
		return append(addrs, body.Addr), nil
	}, body.Addr, "add"); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"action": "add", "addr": body.Addr, "dns": s.Coord.DNs(),
	})
}

// handleRemoveDN removes a DN addr from the runtime registry (ADR-027).
// Query: ?addr=dn7:8080. Returns 200 + the new DN list.
//
// CAUTION: This does NOT migrate data off the removed DN. Operator should
// run rebalance --apply BEFORE removing a DN that holds chunks.
func (s *Server) handleRemoveDN(w http.ResponseWriter, r *http.Request) {
	addr := r.URL.Query().Get("addr")
	if addr == "" {
		writeError(w, http.StatusBadRequest, "missing addr query param")
		return
	}
	if err := s.applyDNChange(func(addrs []string) ([]string, error) {
		out := make([]string, 0, len(addrs))
		for _, a := range addrs {
			if a != addr {
				out = append(out, a)
			}
		}
		if len(out) == len(addrs) {
			return addrs, nil // not present, idempotent
		}
		if len(out) < s.Coord.ReplicationFactor() {
			return nil, fmt.Errorf("would leave %d DNs < ReplicationFactor %d",
				len(out), s.Coord.ReplicationFactor())
		}
		return out, nil
	}, addr, "remove"); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"action": "remove", "addr": addr, "dns": s.Coord.DNs(),
	})
}

// applyDNChange runs the mutator on the current DN list, persists the result
// to bbolt, then atomically swaps the Coordinator's placer. mutator returns
// (new_addrs, err). Serialized via dnsAdminMu.
func (s *Server) applyDNChange(mutate func([]string) ([]string, error), addr, action string) error {
	s.dnsAdminMu.Lock()
	defer s.dnsAdminMu.Unlock()

	current := s.Coord.DNs()
	next, err := mutate(current)
	if err != nil {
		return err
	}
	// Persist first; if persist fails, do NOT update placer (avoid
	// in-memory state diverging from bbolt across crash).
	if err := s.Store.SeedRuntimeDNs(next); err != nil {
		return fmt.Errorf("persist DN registry: %w", err)
	}
	nodes := make([]placement.Node, len(next))
	for i, a := range next {
		nodes[i] = placement.Node{ID: a, Addr: a}
	}
	if err := s.Coord.UpdateNodes(nodes); err != nil {
		return fmt.Errorf("update placer: %w", err)
	}
	s.logger().Info("dns registry changed", "action", action, "addr", addr,
		"new_count", len(next))
	return nil
}

// ─── ADR-028 UrlKey rotation handlers ───

func (s *Server) handleListURLKeys(w http.ResponseWriter, r *http.Request) {
	if s.Signer == nil {
		writeJSON(w, http.StatusOK, map[string]any{"kids": []any{}, "primary": ""})
		return
	}
	entries, err := s.Store.ListURLKeys()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]any{
			"kid":        e.Kid,
			"is_primary": e.IsPrimary,
			"created_at": e.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kids":    out,
		"primary": s.Signer.Primary(),
	})
}

// handleRotateURLKey adds a new (kid, secret_hex) and sets it primary.
// Body: {"kid":"v2","secret_hex":"<hex>"}.
func (s *Server) handleRotateURLKey(w http.ResponseWriter, r *http.Request) {
	if s.Signer == nil {
		writeError(w, http.StatusBadRequest, "signer disabled (skip-auth)")
		return
	}
	var body struct {
		Kid       string `json:"kid"`
		SecretHex string `json:"secret_hex"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Kid == "" || body.SecretHex == "" {
		writeError(w, http.StatusBadRequest, "kid + secret_hex required")
		return
	}
	secret, err := hex.DecodeString(body.SecretHex)
	if err != nil {
		writeError(w, http.StatusBadRequest, "secret_hex: "+err.Error())
		return
	}

	s.urlkeyAdminMu.Lock()
	defer s.urlkeyAdminMu.Unlock()

	if err := s.Store.PutURLKey(body.Kid, body.SecretHex, true); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.Signer.Add(body.Kid, secret); err != nil && err != urlkey.ErrDuplicateKid {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.Signer.SetPrimary(body.Kid); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logger().Info("urlkey rotated", "new_primary", body.Kid)
	writeJSON(w, http.StatusOK, map[string]any{
		"action":  "rotate",
		"primary": body.Kid,
		"kids":    s.Signer.Kids(),
	})
}

// handleRemoveURLKey deletes a non-primary kid. Query: ?kid=v1.
func (s *Server) handleRemoveURLKey(w http.ResponseWriter, r *http.Request) {
	if s.Signer == nil {
		writeError(w, http.StatusBadRequest, "signer disabled (skip-auth)")
		return
	}
	kid := r.URL.Query().Get("kid")
	if kid == "" {
		writeError(w, http.StatusBadRequest, "missing kid query param")
		return
	}

	s.urlkeyAdminMu.Lock()
	defer s.urlkeyAdminMu.Unlock()

	if err := s.Signer.Remove(kid); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Store.DeleteURLKey(kid); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logger().Info("urlkey removed", "kid", kid)
	writeJSON(w, http.StatusOK, map[string]any{
		"action": "remove",
		"kid":    kid,
		"kids":   s.Signer.Kids(),
	})
}

// ─── ADR-025 EC repair handlers ───

// handleRepairPlan computes (read-only) the EC stripe repair plan.
func (s *Server) handleRepairPlan(w http.ResponseWriter, r *http.Request) {
	plan, err := repair.ComputePlan(s.Coord, s.Store)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

// repairOutcome bundles a repair cycle's result. Mirrors rebalance/gc shape so
// future auto-repair (Season 3+) and the manual handler can't drift.
type repairOutcome struct {
	Scanned        int
	RepairsPlanned int
	Unrepairable   int
	Stripes        int
	Repaired       int
	Failed         int
	BytesWritten   int64
	Errors         []string
}

func (o repairOutcome) toMap() map[string]any {
	return map[string]any{
		"scanned":         o.Scanned,
		"repairs_planned": o.RepairsPlanned,
		"unrepairable":    o.Unrepairable,
		"stripes":         o.Stripes,
		"repaired":        o.Repaired,
		"failed":          o.Failed,
		"bytes_written":   o.BytesWritten,
		"errors":          o.Errors,
	}
}

// executeRepair computes + (if non-empty) runs an EC repair plan under
// s.repairMu. Used by handleRepairApply (and future auto-repair).
func (s *Server) executeRepair(ctx context.Context, concurrency int) (repairOutcome, error) {
	s.repairMu.Lock()
	defer s.repairMu.Unlock()

	plan, err := repair.ComputePlan(s.Coord, s.Store)
	if err != nil {
		return repairOutcome{}, fmt.Errorf("ComputePlan: %w", err)
	}
	out := repairOutcome{
		Scanned:        plan.Scanned,
		RepairsPlanned: len(plan.Repairs),
		Unrepairable:   len(plan.Unrepairable),
	}
	if out.RepairsPlanned == 0 {
		return out, nil
	}
	stats := repair.Run(ctx, s.Coord, s.Store, plan, concurrency)
	out.Stripes = stats.Stripes
	out.Repaired = stats.Repaired
	out.Failed = stats.Failed
	out.BytesWritten = stats.BytesWritten
	out.Errors = stats.Errors
	return out, nil
}

// handleRepairApply runs the repair plan with the given concurrency.
// Serialized via s.repairMu. ?concurrency=N (default 4).
func (s *Server) handleRepairApply(w http.ResponseWriter, r *http.Request) {
	concurrency := intQuery(r, "concurrency", 4)
	// Repair fetches K survivors per stripe + Reconstruct + PUT — 10min covers ~1k stripes.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	out, err := s.executeRepair(ctx, concurrency)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := out.toMap()
	resp["concurrency"] = concurrency
	writeJSON(w, http.StatusOK, resp)
}

// handleRebalancePlan computes (read-only) the migration plan and returns it.
// Safe to call concurrently — no side effects.
func (s *Server) handleRebalancePlan(w http.ResponseWriter, r *http.Request) {
	plan, err := rebalance.ComputePlan(s.Coord, s.Store)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	plan.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	writeJSON(w, http.StatusOK, plan)
}

// rebalanceOutcome bundles a rebalance cycle's result. Shared by handler +
// auto-runner so they can't drift on field shapes.
type rebalanceOutcome struct {
	Scanned     int
	PlanSize    int
	Migrated    int
	Failed      int
	BytesCopied int64
	Errors      []string
}

func (o rebalanceOutcome) toMap() map[string]any {
	return map[string]any{
		"scanned":      o.Scanned,
		"plan_size":    o.PlanSize,
		"migrated":     o.Migrated,
		"failed":       o.Failed,
		"bytes_copied": o.BytesCopied,
		"errors":       o.Errors,
	}
}

// executeRebalance computes a fresh plan and (if non-empty) runs it under
// s.rebalanceMu. Used by handleRebalanceApply and the auto-rebalance loop.
func (s *Server) executeRebalance(ctx context.Context, concurrency int) (rebalanceOutcome, error) {
	s.rebalanceMu.Lock()
	defer s.rebalanceMu.Unlock()

	plan, err := rebalance.ComputePlan(s.Coord, s.Store)
	if err != nil {
		return rebalanceOutcome{}, fmt.Errorf("ComputePlan: %w", err)
	}
	out := rebalanceOutcome{Scanned: plan.Scanned, PlanSize: len(plan.Migrations)}
	if out.PlanSize == 0 {
		return out, nil
	}
	stats := rebalance.Run(ctx, s.Coord, s.Store, plan, concurrency)
	out.Migrated = stats.Migrated
	out.Failed = stats.Failed
	out.BytesCopied = stats.BytesCopied
	out.Errors = stats.Errors
	return out, nil
}

// handleRebalanceApply: POST /v1/admin/rebalance/apply?concurrency=N
func (s *Server) handleRebalanceApply(w http.ResponseWriter, r *http.Request) {
	concurrency := intQuery(r, "concurrency", 4)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	out, err := s.executeRebalance(ctx, concurrency)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := out.toMap()
	resp["concurrency"] = concurrency
	writeJSON(w, http.StatusOK, resp)
}

// handleGCPlan computes the surplus chunk plan (read-only).
// Query: ?min_age_seconds=N (default uses gc.DefaultMinAge = 60s)
func (s *Server) handleGCPlan(w http.ResponseWriter, r *http.Request) {
	minAge := parseMinAge(r)
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	plan, err := gc.ComputePlan(ctx, s.Coord, s.Store, minAge)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	plan.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	writeJSON(w, http.StatusOK, plan)
}

type gcOutcome struct {
	Scanned     int
	ClaimedKeys int
	PlanSize    int
	MinAgeSec   int
	Deleted     int
	Failed      int
	BytesFreed  int64
	Errors      []string
}

func (o gcOutcome) toMap() map[string]any {
	return map[string]any{
		"scanned":      o.Scanned,
		"claimed_keys": o.ClaimedKeys,
		"plan_size":    o.PlanSize,
		"min_age_sec":  o.MinAgeSec,
		"deleted":      o.Deleted,
		"failed":       o.Failed,
		"bytes_freed":  o.BytesFreed,
		"errors":       o.Errors,
	}
}

// executeGC computes + (if non-empty) runs a GC plan under s.gcMu.
// Used by handleGCApply and the auto-GC loop.
func (s *Server) executeGC(ctx context.Context, minAge time.Duration, concurrency int) (gcOutcome, error) {
	s.gcMu.Lock()
	defer s.gcMu.Unlock()

	plan, err := gc.ComputePlan(ctx, s.Coord, s.Store, minAge)
	if err != nil {
		return gcOutcome{}, fmt.Errorf("ComputePlan: %w", err)
	}
	out := gcOutcome{
		Scanned: plan.Scanned, ClaimedKeys: plan.ClaimedKeys,
		PlanSize: len(plan.Sweeps), MinAgeSec: int(minAge.Seconds()),
	}
	if out.PlanSize == 0 {
		return out, nil
	}
	stats := gc.Run(ctx, s.Coord, plan, concurrency)
	out.Deleted = stats.Deleted
	out.Failed = stats.Failed
	out.BytesFreed = stats.BytesFreed
	out.Errors = stats.Errors
	return out, nil
}

// handleGCApply: POST /v1/admin/gc/apply?min_age_seconds=N&concurrency=N
func (s *Server) handleGCApply(w http.ResponseWriter, r *http.Request) {
	minAge := parseMinAge(r)
	concurrency := intQuery(r, "concurrency", 4)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	out, err := s.executeGC(ctx, minAge, concurrency)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := out.toMap()
	resp["concurrency"] = concurrency
	writeJSON(w, http.StatusOK, resp)
}

// intQuery reads ?name=N from the request, returning def on missing/invalid.
func intQuery(r *http.Request, name string, def int) int {
	if q := r.URL.Query().Get(name); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func parseMinAge(r *http.Request) time.Duration {
	if q := r.URL.Query().Get("min_age_seconds"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n >= 0 {
			return time.Duration(n) * time.Second
		}
	}
	return gc.DefaultMinAge
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"dns":    s.Coord.DNs(),
	})
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusRecorder{ResponseWriter: w, status: 200}
		h.ServeHTTP(rw, r)
		fmt.Printf("[edge] %s %s -> %d (%s)\n", r.Method, r.URL.Path, rw.status, time.Since(start))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// ─── auto-trigger (ADR-013) ───

// StartAuto launches background goroutines that periodically run rebalance
// and GC. They reuse Server.rebalanceMu / gcMu so manual /apply and auto
// runs interleave safely. Stops on ctx.Done().
//
// Returns immediately if AutoCfg.Enabled is false.
func (s *Server) StartAuto(ctx context.Context) {
	if !s.AutoCfg.Enabled {
		return
	}
	rb := s.AutoCfg.RebalanceInterval
	if rb <= 0 {
		rb = 5 * time.Minute
	}
	gci := s.AutoCfg.GCInterval
	if gci <= 0 {
		gci = 15 * time.Minute
	}
	minAge := s.AutoCfg.GCMinAge
	if minAge <= 0 {
		minAge = gc.DefaultMinAge
	}
	conc := s.AutoCfg.Concurrency
	if conc <= 0 {
		conc = 4
	}

	s.logger().Info("auto-trigger starting",
		"rebalance_interval", rb, "gc_interval", gci,
		"gc_min_age", minAge, "concurrency", conc)

	go s.autoLoop(ctx, JobRebalance, rb, func() { s.runAutoRebalance(ctx, conc) })
	go s.autoLoop(ctx, JobGC, gci, func() { s.runAutoGC(ctx, minAge, conc) })
}

// autoLoop is the auto-trigger ticker driver. Wraps tickerLoop with a
// per-job stop log.
func (s *Server) autoLoop(ctx context.Context, job AutoJob, interval time.Duration, exec func()) {
	tickerLoop(ctx, interval, exec)
	s.logger().Info("auto loop stopping", "job", job)
}

// tickerLoop is the bare ticker driver shared by auto-trigger, heartbeat,
// and follower-sync goroutines. exec runs once per tick; loop exits when
// ctx is cancelled.
func tickerLoop(ctx context.Context, interval time.Duration, exec func()) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			exec()
		}
	}
}

func (s *Server) runAutoRebalance(ctx context.Context, conc int) {
	start := time.Now()
	rctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	out, err := s.executeRebalance(rctx, conc)
	dur := time.Since(start).Milliseconds()

	if err != nil {
		s.recordAuto(JobRebalance, AutoRun{
			Job: JobRebalance, StartedAt: start, DurationMS: dur,
			Error: err.Error(),
		})
		s.logger().Warn("auto-rebalance failed", "err", err)
		return
	}
	if out.PlanSize == 0 {
		s.markCheck(JobRebalance, start)
		return
	}
	s.recordAuto(JobRebalance, AutoRun{
		Job: JobRebalance, StartedAt: start, DurationMS: dur,
		PlanSize: out.PlanSize, Stats: out.toMap(),
	})
	s.logger().Info("auto-rebalance cycle",
		"plan_size", out.PlanSize, "migrated", out.Migrated,
		"failed", out.Failed, "bytes", out.BytesCopied, "duration_ms", dur)
}

func (s *Server) runAutoGC(ctx context.Context, minAge time.Duration, conc int) {
	start := time.Now()
	gctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	out, err := s.executeGC(gctx, minAge, conc)
	dur := time.Since(start).Milliseconds()

	if err != nil {
		s.recordAuto(JobGC, AutoRun{
			Job: JobGC, StartedAt: start, DurationMS: dur,
			Error: err.Error(),
		})
		s.logger().Warn("auto-gc failed", "err", err)
		return
	}
	if out.PlanSize == 0 {
		s.markCheck(JobGC, start)
		return
	}
	s.recordAuto(JobGC, AutoRun{
		Job: JobGC, StartedAt: start, DurationMS: dur,
		PlanSize: out.PlanSize, Stats: out.toMap(),
	})
	s.logger().Info("auto-gc cycle",
		"plan_size", out.PlanSize, "deleted", out.Deleted,
		"failed", out.Failed, "bytes", out.BytesFreed, "duration_ms", dur)
}

// recordAuto appends to the ring buffer and updates lastCheck atomically.
func (s *Server) recordAuto(job AutoJob, r AutoRun) {
	s.autoMu.Lock()
	defer s.autoMu.Unlock()
	s.autoRuns = append(s.autoRuns, r)
	if len(s.autoRuns) > autoRunHistory {
		s.autoRuns = s.autoRuns[len(s.autoRuns)-autoRunHistory:]
	}
	s.setLastCheckLocked(job, r.StartedAt)
}

// markCheck updates only lastCheck — used for empty cycles to keep
// liveness visible without polluting the ring buffer.
func (s *Server) markCheck(job AutoJob, t time.Time) {
	s.autoMu.Lock()
	defer s.autoMu.Unlock()
	s.setLastCheckLocked(job, t)
}

func (s *Server) setLastCheckLocked(job AutoJob, t time.Time) {
	switch job {
	case JobRebalance:
		s.autoLastCheckRebalance = t
	case JobGC:
		s.autoLastCheckGC = t
	}
}

func (s *Server) handleAutoStatus(w http.ResponseWriter, r *http.Request) {
	s.autoMu.Lock()
	runs := append([]AutoRun(nil), s.autoRuns...)
	lastCheckRb := s.autoLastCheckRebalance
	lastCheckGC := s.autoLastCheckGC
	s.autoMu.Unlock()

	// Derive next-fire from lastCheck + interval (ticker fires at fixed cadence).
	nextRb := zeroOr(lastCheckRb, s.AutoCfg.RebalanceInterval)
	nextGC := zeroOr(lastCheckGC, s.AutoCfg.GCInterval)

	writeJSON(w, http.StatusOK, map[string]any{
		"config": map[string]any{
			"enabled":            s.AutoCfg.Enabled,
			"rebalance_interval": s.AutoCfg.RebalanceInterval.String(),
			"gc_interval":        s.AutoCfg.GCInterval.String(),
			"gc_min_age":         s.AutoCfg.GCMinAge.String(),
			"concurrency":        s.AutoCfg.Concurrency,
		},
		"last_check_rebalance": lastCheckRb,
		"last_check_gc":        lastCheckGC,
		"next_rebalance":       nextRb,
		"next_gc":              nextGC,
		"history_size":         len(runs),
		"history_capacity":     autoRunHistory,
		"runs":                 runs,
	})
}

func zeroOr(t time.Time, d time.Duration) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return t.Add(d)
}

// ─── ADR-014 meta backup/HA handlers ───

// handleMetaSnapshot streams a consistent point-in-time copy of the metadata
// bbolt file as application/octet-stream. Suggested filename via
// Content-Disposition. Safe while writers are active (bbolt single-writer +
// many-readers).
//
// Header X-KVFS-WAL-Seq carries the WAL seq at snapshot time (ADR-019) so
// followers can fetch only the delta since this snapshot via /v1/admin/wal.
func (s *Server) handleMetaSnapshot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="kvfs-meta-snapshot.bbolt"`)
	if wal := s.Store.WAL(); wal != nil {
		w.Header().Set("X-KVFS-WAL-Seq", fmt.Sprintf("%d", wal.LastSeq()))
	}
	if _, err := s.Store.Snapshot(w); err != nil {
		// Headers already sent — log only; client sees truncated body.
		s.Log.Error("meta snapshot failed", slog.String("err", err.Error()))
	}
}

// handleMetaInfo returns aggregate counts (object/EC/chunk/stripe/shard/DN/key)
// + bbolt logical size for capacity planning.
func (s *Server) handleMetaInfo(w http.ResponseWriter, r *http.Request) {
	stats, err := s.Store.Stats()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// ─── ADR-030 heartbeat handlers ───

// handleHeartbeat returns the current per-DN liveness snapshot. Returns an
// empty list when the monitor isn't running.
func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if s.Heartbeat == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "statuses": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":  true,
		"statuses": s.Heartbeat.Snapshot(),
	})
}

// handleWAL streams WAL entries since the given seq as JSON-lines (ADR-019).
// Query: ?since=<int> (default 0 = full WAL).
//
// Followers use this for incremental catch-up between snapshot pulls.
func (s *Server) handleWAL(w http.ResponseWriter, r *http.Request) {
	wal := s.Store.WAL()
	if wal == nil {
		writeError(w, http.StatusNotImplemented, "WAL not enabled (set EDGE_WAL_DIR)")
		return
	}
	sinceSeq := int64(intQuery(r, "since", 0))
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-KVFS-WAL-Last-Seq", fmt.Sprintf("%d", wal.LastSeq()))
	if _, err := wal.WriteSinceTo(sinceSeq, w); err != nil {
		s.logger().Error("WAL stream failed", slog.String("err", err.Error()))
	}
}

// handleWALInfo returns last_seq + path + a small recent-tail (last 10
// entries) without streaming the whole file. Diagnostic endpoint.
func (s *Server) handleWALInfo(w http.ResponseWriter, r *http.Request) {
	wal := s.Store.WAL()
	if wal == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	last := wal.LastSeq()
	from := last - 10
	if from < 0 {
		from = 0
	}
	tail, _ := wal.Since(from)
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":  true,
		"last_seq": last,
		"recent":   tail,
	})
}

// handleSnapshotHistory returns the auto-snapshot scheduler's stats + the
// rotating snapshot directory listing (ADR-016). When the scheduler is nil
// (env not set), returns enabled:false.
func (s *Server) handleSnapshotHistory(w http.ResponseWriter, r *http.Request) {
	if s.SnapshotScheduler == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, s.SnapshotScheduler.Stats())
}

// StartSnapshotScheduler runs the auto-snapshot loop in a goroutine. No-op if
// s.SnapshotScheduler is nil. The scheduler stops on ctx.Done.
func (s *Server) StartSnapshotScheduler(ctx context.Context) {
	if s.SnapshotScheduler == nil {
		return
	}
	go s.SnapshotScheduler.Run(ctx.Done())
}

// handleRole returns the current role + (follower) sync stats (ADR-022).
func (s *Server) handleRole(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.roleStatus())
}

// StartFollowerSync runs the snapshot-pull loop in a goroutine. Runs when
// either ADR-022 manual follower (Role == RoleFollower) or ADR-031 election
// mode is configured (followerSt is set in either case). The sync function
// itself short-circuits when self-is-leader.
func (s *Server) StartFollowerSync(ctx context.Context) {
	if s.followerSt == nil {
		return
	}
	go s.runFollowerSync(ctx)
}

// StartElector runs the election state machine. No-op if Elector is nil.
func (s *Server) StartElector(ctx context.Context) {
	if s.Elector == nil {
		return
	}
	go s.Elector.Run(ctx)
}

// StartHeartbeat runs a goroutine that probes all runtime DNs every
// `interval`. No-op if s.Heartbeat is nil. Stops on ctx.Done.
func (s *Server) StartHeartbeat(ctx context.Context, interval time.Duration) {
	if s.Heartbeat == nil {
		return
	}
	if interval <= 0 {
		interval = 10 * time.Second
	}
	tick := func() { s.Heartbeat.Tick(ctx, s.Coord.DNs()) }
	go func() {
		// Immediate probe so the first /v1/admin/heartbeat call is non-empty
		// even if the user hits it before the first tick fires.
		tick()
		tickerLoop(ctx, interval, tick)
	}()
}

// ─── EC mode handlers (ADR-008) ───

// parseEC parses headers like "4+2" → (4, 2). Returns error if malformed or
// out-of-range.
func parseEC(hdr string) (k, m int, err error) {
	parts := strings.SplitN(hdr, "+", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("X-KVFS-EC must be K+M (got %q)", hdr)
	}
	k, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	m, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || k <= 0 || m <= 0 {
		return 0, 0, fmt.Errorf("X-KVFS-EC must be K+M positive ints (got %q)", hdr)
	}
	return k, m, nil
}

// handlePutEC stores body using Reed-Solomon (K+M).
//
// Steps per ADR-008:
//  1. Pad body up to multiple of K * shardSize
//  2. Split into stripes of K data shards each
//  3. RS encode → M parity shards per stripe
//  4. For each stripe: stripe_id = sha256(K data shards concat),
//     desired DNs = Pick(stripe_id, K+M) → K+M distinct DN addresses,
//     PUT each shard to its assigned DN
//  5. Persist ObjectMeta with EC params + Stripes
// handlePutECStream is the streaming EC PUT (ADR-017 follow-up to ADR-008).
// Reads stripeBytes (= K × shardSize) at a time from r.Body, encodes to
// K+M shards, fans out, and appends one stripe per iteration. Memory bound
// = stripeBytes regardless of object size.
func (s *Server) handlePutECStream(w http.ResponseWriter, r *http.Request, bucket, key, ecHdr string) {
	k, m, err := parseEC(ecHdr)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	totalDNs := len(s.Coord.DNs())
	if k+m > totalDNs {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("EC %d+%d requires %d DNs, cluster has %d", k, m, k+m, totalDNs))
		return
	}
	enc, err := reedsolomon.NewEncoder(k, m)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	shardSize := s.chunkSize()
	stripeBytes := k * shardSize

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	stripeBuf := make([]byte, stripeBytes)
	var stripes []store.Stripe
	var dataLen int64

	for stripeIdx := 0; ; stripeIdx++ {
		n, rerr := io.ReadFull(r.Body, stripeBuf)
		if rerr == io.EOF {
			break // exact stripe boundary
		}
		// Last stripe: short read → pad with zeros to stripeBytes.
		if rerr == io.ErrUnexpectedEOF {
			for i := n; i < stripeBytes; i++ {
				stripeBuf[i] = 0
			}
		} else if rerr != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("read stripe %d: %v", stripeIdx, rerr))
			return
		}
		dataLen += int64(n)

		// Slice K data shards (defensive copies — encoder/PutChunkTo may retain).
		dataShards := make([][]byte, k)
		for di := 0; di < k; di++ {
			ds := make([]byte, shardSize)
			copy(ds, stripeBuf[di*shardSize:(di+1)*shardSize])
			dataShards[di] = ds
		}
		parityShards, encErr := enc.Encode(dataShards)
		if encErr != nil {
			writeError(w, http.StatusInternalServerError,
				fmt.Sprintf("stripe %d encode: %v", stripeIdx, encErr))
			return
		}
		hsh := sha256.New()
		for _, ds := range dataShards {
			hsh.Write(ds)
		}
		stripeID := hex.EncodeToString(hsh.Sum(nil))

		dnAddrs := s.Coord.PlaceN(stripeID, k+m)
		if len(dnAddrs) < k+m {
			writeError(w, http.StatusBadGateway,
				fmt.Sprintf("stripe %d: only %d DNs available for %d shards", stripeIdx, len(dnAddrs), k+m))
			return
		}

		all := make([][]byte, 0, k+m)
		all = append(all, dataShards...)
		all = append(all, parityShards...)

		shardRefs := make([]store.ChunkRef, k+m)
		for si := 0; si < k+m; si++ {
			sum := sha256.Sum256(all[si])
			shardID := hex.EncodeToString(sum[:])
			addr := dnAddrs[si]
			if perr := s.Coord.PutChunkTo(ctx, addr, shardID, all[si]); perr != nil {
				writeError(w, http.StatusBadGateway,
					fmt.Sprintf("stripe %d shard %d (%s) → %s: %v", stripeIdx, si, shardID[:16], addr, perr))
				return
			}
			shardRefs[si] = store.ChunkRef{
				ChunkID:  shardID,
				Size:     int64(len(all[si])),
				Replicas: []string{addr},
			}
		}
		stripes = append(stripes, store.Stripe{StripeID: stripeID, Shards: shardRefs})

		if rerr == io.ErrUnexpectedEOF {
			break // last (padded) stripe processed
		}
	}

	if dataLen == 0 {
		writeError(w, http.StatusBadRequest, "empty body")
		return
	}

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	meta := &store.ObjectMeta{
		Bucket:      bucket,
		Key:         key,
		Size:        dataLen,
		ContentType: contentType,
		EC: &store.ECParams{
			K:         k,
			M:         m,
			ShardSize: shardSize,
			DataSize:  dataLen,
		},
		Stripes: stripes,
	}
	if err := s.Store.PutObject(meta); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"bucket":      bucket,
		"key":         key,
		"size":        dataLen,
		"ec":          meta.EC,
		"stripes":     stripes,
		"num_stripes": len(stripes),
		"version":     meta.Version,
	})
}

// handleGetEC reconstructs an EC-stored object.
//
// For each stripe: try fetching all K+M shards from their assigned DNs.
// If at least K succeed, run reedsolomon.Reconstruct to rebuild missing
// data shards. Concat all data shards (across stripes) and trim to
// EC.DataSize (removes the last-stripe padding).
func (s *Server) handleGetEC(w http.ResponseWriter, r *http.Request, ctx context.Context, meta *store.ObjectMeta) {
	enc, err := reedsolomon.NewEncoder(meta.EC.K, meta.EC.M)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	k, m := meta.EC.K, meta.EC.M

	// Streaming GET (ADR-017 follow-up): per-stripe reconstruct, write data
	// shards directly to ResponseWriter. Memory bound = (K+M) × shardSize per
	// iteration regardless of object size. Last stripe is trimmed to DataSize
	// (drops padding) by tracking remaining bytes.
	w.Header().Set("Content-Type", meta.ContentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", meta.Size))
	w.Header().Set("X-KVFS-EC", fmt.Sprintf("%d+%d", k, m))
	w.Header().Set("X-KVFS-Stripes", fmt.Sprintf("%d", len(meta.Stripes)))
	w.Header().Set("X-KVFS-Version", fmt.Sprintf("%d", meta.Version))

	remaining := meta.EC.DataSize
	for stripeIdx, stripe := range meta.Stripes {
		shards := make([][]byte, k+m)
		survivors := 0
		for si, sh := range stripe.Shards {
			data, _, ferr := s.Coord.ReadChunk(ctx, sh.ChunkID, sh.Replicas)
			if ferr != nil {
				continue
			}
			sum := sha256.Sum256(data)
			if hex.EncodeToString(sum[:]) != sh.ChunkID {
				continue
			}
			shards[si] = data
			survivors++
		}
		if survivors < k {
			if stripeIdx == 0 {
				writeError(w, http.StatusServiceUnavailable,
					fmt.Sprintf("stripe %d: only %d of %d shards survived (need >= %d)", stripeIdx, survivors, k+m, k))
				return
			}
			s.logger().Error("EC GET stream aborted",
				slog.Int("stripe", stripeIdx), slog.Int("survivors", survivors))
			return
		}
		if rerr := enc.Reconstruct(shards); rerr != nil {
			if stripeIdx == 0 {
				writeError(w, http.StatusInternalServerError,
					fmt.Sprintf("stripe %d reconstruct: %v", stripeIdx, rerr))
				return
			}
			s.logger().Error("EC GET reconstruct failed mid-stream", slog.Int("stripe", stripeIdx))
			return
		}
		// Write K data shards in order, trimmed by remaining for the last stripe.
		for di := 0; di < k && remaining > 0; di++ {
			toWrite := shards[di]
			if int64(len(toWrite)) > remaining {
				toWrite = toWrite[:remaining]
			}
			if _, werr := w.Write(toWrite); werr != nil {
				return // client disconnect
			}
			remaining -= int64(len(toWrite))
		}
	}
}
