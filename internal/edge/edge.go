// Package edge is the kvfs-edge HTTP gateway: UrlKey verification + coordinator.
package edge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/HardcoreMonk/kvfs/internal/coordinator"
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

	// If true, skip UrlKey verification (for demos/tests). Never enable in public.
	SkipAuth bool

	// rebalanceMu serializes /v1/admin/rebalance/apply so two concurrent
	// triggers don't race on the same chunks. /plan is read-only and excluded.
	rebalanceMu sync.Mutex
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

	// Content-addressable: chunk_id = hex(sha256(body))
	sum := sha256.Sum256(body)
	chunkID := hex.EncodeToString(sum[:])

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	replicas, err := s.Coord.WriteChunk(ctx, chunkID, body)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	meta := &store.ObjectMeta{
		Bucket:      bucket,
		Key:         key,
		Size:        int64(len(body)),
		ChunkID:     chunkID,
		Replicas:    replicas,
		ContentType: contentType,
	}
	if err := s.Store.PutObject(meta); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"bucket":   bucket,
		"key":      key,
		"chunk_id": chunkID,
		"size":     len(body),
		"replicas": replicas,
		"version":  meta.Version,
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

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	body, servedBy, err := s.Coord.ReadChunk(ctx, meta.ChunkID, meta.Replicas)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	// Integrity: verify bytes match chunk_id
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != meta.ChunkID {
		writeError(w, http.StatusBadGateway, "chunk integrity check failed")
		return
	}

	w.Header().Set("Content-Type", meta.ContentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", meta.Size))
	w.Header().Set("X-KVFS-Served-By", servedBy)
	w.Header().Set("X-KVFS-Chunk-ID", meta.ChunkID)
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

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	// best-effort chunk delete; continue even if some replicas fail (e.g. dead DN)
	_ = s.Coord.DeleteChunk(ctx, meta.ChunkID, meta.Replicas)

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
