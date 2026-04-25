// Package edge is the kvfs-edge HTTP gateway: UrlKey verification + coordinator.
package edge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/HardcoreMonk/kvfs/internal/chunker"
	"github.com/HardcoreMonk/kvfs/internal/coordinator"
	"github.com/HardcoreMonk/kvfs/internal/gc"
	"github.com/HardcoreMonk/kvfs/internal/rebalance"
	"github.com/HardcoreMonk/kvfs/internal/store"
	"github.com/HardcoreMonk/kvfs/internal/urlkey"
)

// Server assembles the edge HTTP handlers over a metadata store, a coordinator,
// and a UrlKey signer.
type Server struct {
	Store  *store.MetaStore
	Coord  *coordinator.Coordinator
	Signer *urlkey.Signer

	// ChunkSize is bytes-per-chunk for PUT splitting (ADR-011).
	// Zero / negative falls back to chunker.DefaultChunkSize.
	ChunkSize int

	// If true, skip UrlKey verification (for demos/tests). Never enable in public.
	SkipAuth bool

	// rebalanceMu serializes /v1/admin/rebalance/apply so two concurrent
	// triggers don't race on the same chunks. /plan is read-only and excluded.
	rebalanceMu sync.Mutex

	// gcMu serializes /v1/admin/gc/apply for the same reason.
	gcMu sync.Mutex
}

func (s *Server) chunkSize() int {
	if s.ChunkSize <= 0 {
		return chunker.DefaultChunkSize
	}
	return s.ChunkSize
}

// Routes builds the HTTP mux.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/o/{bucket}/{key...}", s.handlePut)
	mux.HandleFunc("GET /v1/o/{bucket}/{key...}", s.handleGet)
	mux.HandleFunc("DELETE /v1/o/{bucket}/{key...}", s.handleDelete)
	mux.HandleFunc("GET /v1/admin/objects", s.handleList)
	mux.HandleFunc("GET /v1/admin/dns", s.handleDNs)
	mux.HandleFunc("POST /v1/admin/rebalance/plan", s.handleRebalancePlan)
	mux.HandleFunc("POST /v1/admin/rebalance/apply", s.handleRebalanceApply)
	mux.HandleFunc("POST /v1/admin/gc/plan", s.handleGCPlan)
	mux.HandleFunc("POST /v1/admin/gc/apply", s.handleGCApply)
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

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "empty body")
		return
	}

	// Split body into fixed-size chunks (ADR-011). Each chunk gets its own
	// content-addressable id and is placed independently via Rendezvous Hashing.
	pieces := chunker.Split(body, s.chunkSize())

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	chunkRefs := make([]store.ChunkRef, len(pieces))
	for i, p := range pieces {
		replicas, werr := s.Coord.WriteChunk(ctx, p.ID, p.Data)
		if werr != nil {
			// Partial-success orphan chunks remain on disk; GC (ADR-012) cleans
			// them after min-age. We do NOT commit metadata, so reads can't see
			// a half-written object.
			writeError(w, http.StatusBadGateway,
				fmt.Sprintf("chunk %d/%d (%s): %v", i+1, len(pieces), p.ID[:16], werr))
			return
		}
		chunkRefs[i] = store.ChunkRef{
			ChunkID:  p.ID,
			Size:     p.Size,
			Replicas: replicas,
		}
	}

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	meta := &store.ObjectMeta{
		Bucket:      bucket,
		Key:         key,
		Size:        int64(len(body)),
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
		"size":       len(body),
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

	// Build per-chunk fetch index so chunker.Join can verify each piece.
	specs := make([]chunker.JoinSpec, len(meta.Chunks))
	chunkReplicas := make(map[string][]string, len(meta.Chunks))
	for i, c := range meta.Chunks {
		specs[i] = chunker.JoinSpec{ChunkID: c.ChunkID, Size: c.Size}
		chunkReplicas[c.ChunkID] = c.Replicas
	}

	body, err := chunker.Join(specs, meta.Size, func(chunkID string) ([]byte, error) {
		data, _, ferr := s.Coord.ReadChunk(ctx, chunkID, chunkReplicas[chunkID])
		return data, ferr
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	w.Header().Set("Content-Type", meta.ContentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", meta.Size))
	w.Header().Set("X-KVFS-Chunks", fmt.Sprintf("%d", len(meta.Chunks)))
	w.Header().Set("X-KVFS-Version", fmt.Sprintf("%d", meta.Version))
	_, _ = w.Write(body)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
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
	// best-effort per-chunk delete; continue even if some replicas fail (dead DN)
	for _, c := range meta.Chunks {
		_ = s.Coord.DeleteChunk(ctx, c.ChunkID, c.Replicas)
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

// handleRebalanceApply computes a fresh plan and runs it.
// Serialized via s.rebalanceMu — second concurrent call waits.
//
// Query: ?concurrency=N (default 4)
func (s *Server) handleRebalanceApply(w http.ResponseWriter, r *http.Request) {
	concurrency := 4
	if q := r.URL.Query().Get("concurrency"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			concurrency = n
		}
	}

	s.rebalanceMu.Lock()
	defer s.rebalanceMu.Unlock()

	plan, err := rebalance.ComputePlan(s.Coord, s.Store)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	planSize := len(plan.Migrations)

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	stats := rebalance.Run(ctx, s.Coord, s.Store, plan, concurrency)

	writeJSON(w, http.StatusOK, map[string]any{
		"scanned":      plan.Scanned,
		"plan_size":    planSize,
		"concurrency":  concurrency,
		"migrated":     stats.Migrated,
		"failed":       stats.Failed,
		"bytes_copied": stats.BytesCopied,
		"errors":       stats.Errors,
	})
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

// handleGCApply runs the GC plan with the given concurrency.
// Serialized via s.gcMu.
//
// Query:
//
//	?min_age_seconds=N   (default 60s)
//	?concurrency=N       (default 4)
func (s *Server) handleGCApply(w http.ResponseWriter, r *http.Request) {
	minAge := parseMinAge(r)
	concurrency := 4
	if q := r.URL.Query().Get("concurrency"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			concurrency = n
		}
	}

	s.gcMu.Lock()
	defer s.gcMu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	plan, err := gc.ComputePlan(ctx, s.Coord, s.Store, minAge)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	planSize := len(plan.Sweeps)
	stats := gc.Run(ctx, s.Coord, plan, concurrency)

	writeJSON(w, http.StatusOK, map[string]any{
		"scanned":      plan.Scanned,
		"claimed_keys": plan.ClaimedKeys,
		"plan_size":    planSize,
		"concurrency":  concurrency,
		"min_age_sec":  int(minAge.Seconds()),
		"deleted":      stats.Deleted,
		"failed":       stats.Failed,
		"bytes_freed":  stats.BytesFreed,
		"errors":       stats.Errors,
	})
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
