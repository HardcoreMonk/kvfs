// Package dn is the kvfs-dn data-node storage layer.
//
// On-disk layout:
//
//	<data_dir>/chunks/<id[0:2]>/<id[2:]>       # chunk bytes
//	<data_dir>/meta.json                       # DN self-state
//
// Chunks are content-addressable: id = hex(sha256(body)). This gives:
//   - Automatic dedup across objects with identical content
//   - Free integrity verification on read
//   - git / IPFS / Restic mental model
package dn

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

var (
	ErrNotFound        = errors.New("dn: chunk not found")
	ErrChunkIDMismatch = errors.New("dn: chunk body hash does not match id")
	ErrBadChunkID      = errors.New("dn: chunk id must be 64 hex chars (sha256)")
)

// Server is the DN HTTP + storage layer.
type Server struct {
	id      string
	dataDir string
	started time.Time

	// metrics (exposed via /healthz as a courtesy)
	chunkCount atomic.Int64
	bytesTotal atomic.Int64
}

// NewServer constructs a Server and ensures the chunks directory exists.
func NewServer(id, dataDir string) (*Server, error) {
	chunks := filepath.Join(dataDir, "chunks")
	if err := os.MkdirAll(chunks, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", chunks, err)
	}
	s := &Server{id: id, dataDir: dataDir, started: time.Now().UTC()}
	s.rescan()
	return s, nil
}

// Routes returns the HTTP handler for this DN.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /chunk/{id}", s.handleGet)
	mux.HandleFunc("PUT /chunk/{id}", s.handlePut)
	mux.HandleFunc("DELETE /chunk/{id}", s.handleDelete)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	return logRequests(mux)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validChunkID(id) {
		http.Error(w, "bad chunk id", http.StatusBadRequest)
		return
	}
	f, err := os.Open(s.chunkPath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", st.Size()))
	_, _ = io.Copy(w, f)
}

func (s *Server) handlePut(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validChunkID(id) {
		http.Error(w, "bad chunk id", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// integrity: the id the client sent must match sha256(body)
	got := hashHex(body)
	if got != id {
		http.Error(w, ErrChunkIDMismatch.Error(), http.StatusBadRequest)
		return
	}

	path := s.chunkPath(id)
	if _, err := os.Stat(path); err == nil {
		// idempotent: if chunk already stored with same content, 200 OK.
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"dn":"%s","status":"already-stored","size":%d}`, s.id, len(body))
		return
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// atomic write via tempfile + rename
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.chunkCount.Add(1)
	s.bytesTotal.Add(int64(len(body)))
	w.WriteHeader(http.StatusCreated)
	_, _ = fmt.Fprintf(w, `{"dn":"%s","status":"stored","size":%d}`, s.id, len(body))
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validChunkID(id) {
		http.Error(w, "bad chunk id", http.StatusBadRequest)
		return
	}
	path := s.chunkPath(id)
	info, err := os.Stat(path)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err := os.Remove(path); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.chunkCount.Add(-1)
	s.bytesTotal.Add(-info.Size())
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"dn":          s.id,
		"status":      "ok",
		"uptime_sec":  time.Since(s.started).Seconds(),
		"chunk_count": s.chunkCount.Load(),
		"bytes_total": s.bytesTotal.Load(),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// chunkPath returns <dataDir>/chunks/<id[0:2]>/<id[2:]>.
func (s *Server) chunkPath(id string) string {
	return filepath.Join(s.dataDir, "chunks", id[:2], id[2:])
}

// rescan walks chunks/ and recomputes chunk_count / bytes_total on startup.
func (s *Server) rescan() {
	root := filepath.Join(s.dataDir, "chunks")
	var cnt, total int64
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		cnt++
		total += info.Size()
		return nil
	})
	s.chunkCount.Store(cnt)
	s.bytesTotal.Store(total)
}

func validChunkID(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case '0' <= c && c <= '9':
		case 'a' <= c && c <= 'f':
		default:
			return false
		}
	}
	return true
}

func hashHex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// logRequests is a tiny access-log middleware.
func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusRecorder{ResponseWriter: w, status: 200}
		h.ServeHTTP(rw, r)
		fmt.Printf("[dn] %s %s -> %d (%s)\n", r.Method, r.URL.Path, rw.status, time.Since(start))
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
