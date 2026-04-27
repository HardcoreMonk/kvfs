// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

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
//
// 비전공자용 해설
// ──────────────
// DN(DataNode) = "디스크 한 대" 라고 보면 된다. edge 가 정한 chunk_id 로
// 파일을 받아 그대로 디스크에 떨어뜨리는 단순 KV 서버:
//
//   PUT /chunk/<id>  body       → 디스크에 저장 (동일 id 재요청 시 idempotent)
//   GET /chunk/<id>             → 바이트 그대로 반환
//   DELETE /chunk/<id>          → 파일 삭제
//   GET  /chunks                → 보유 청크 ID 목록 (GC 가 사용, ADR-012)
//   GET  /healthz               → 부팅 시각 + 청크 수 + 총 바이트
//
// id 가 sha256(body) 이라는 단 한 가지 invariant 만 검증한다 (handlePut
// 안에서 hashHex 비교). 그 외 인증·복제·메타데이터는 edge 책임 — DN 은 stateless
// 에 가깝다 (자체 카운터만 atomic.Int64 로 보유).
//
// 디렉토리 분할 (`<id[0:2]>/<id[2:]>`) 은 파일 시스템 한 디렉토리당 inode 한도를
// 피하려는 흔한 해시 분할. id 가 hex 이라 첫 두 글자 = 256 가지 디렉토리.
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
	"sort"
	"sync"
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

	// Anti-entropy support (ADR-054). Default state = disabled (zero
	// value). Operator opts in via StartScrubber().
	scrubMu sync.Mutex
	scrub   scrubState
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
	mux.HandleFunc("GET /chunks", s.handleListChunks)
	// ADR-054 (S7 Ep.4) anti-entropy:
	mux.HandleFunc("GET /chunks/merkle", s.handleMerkle)
	mux.HandleFunc("GET /chunks/merkle/bucket", s.handleMerkleBucket)
	mux.HandleFunc("GET /chunks/scrub-status", s.handleScrubStatus)
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
	// ADR-056: ?force=1 bypasses the idempotent-existence skip. Used by
	// anti-entropy corrupt-repair where the file IS on disk but the bytes
	// are wrong (sha mismatch caught by scrubber). Body has already been
	// validated against id above so we can safely overwrite.
	force := r.URL.Query().Get("force") == "1"
	if !force {
		if _, err := os.Stat(path); err == nil {
			// idempotent: if chunk already stored with same content, 200 OK.
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"dn":"%s","status":"already-stored","size":%d}`, s.id, len(body))
			return
		}
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

// ChunkInfo describes a single on-disk chunk for /chunks listings.
//
// MTime 은 Unix seconds. GC 의 min-age 안전망이 사용 (ADR-012).
type ChunkInfo struct {
	ID    string `json:"id"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"`
}

// handleListChunks returns all chunk IDs on disk, sorted by ID.
//
// 사용처: GC (ADR-012) 가 메타와 비교해 surplus 청크 식별.
// MVP: 페이지네이션 없음. 청크 100만 개 이상이면 페이징 필요.
func (s *Server) handleListChunks(w http.ResponseWriter, r *http.Request) {
	root := filepath.Join(s.dataDir, "chunks")
	var infos []ChunkInfo
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		// 파일 경로에서 chunk_id 복원: <root>/<id[0:2]>/<id[2:]>
		dir := filepath.Base(filepath.Dir(p))
		base := filepath.Base(p)
		if len(dir) != 2 || len(base) != 62 {
			return nil // 형식 안 맞으면 무시 (.tmp 등)
		}
		id := dir + base
		if !validChunkID(id) {
			return nil
		}
		infos = append(infos, ChunkInfo{
			ID:    id,
			Size:  info.Size(),
			MTime: info.ModTime().Unix(),
		})
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].ID < infos[j].ID })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"dn":     s.id,
		"count":  len(infos),
		"chunks": infos,
	})
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
