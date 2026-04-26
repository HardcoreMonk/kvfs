// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Multi-edge HA — read-replica follower (ADR-022).
//
// 비전공자용 해설
// ──────────────
// kvfs-edge 는 single-writer bbolt 를 쓴다. 두 edge 가 같은 bbolt 를 동시에 열
// 수 없다. multi-edge HA 의 가장 단순한 형태:
//
//   primary  ── /v1/o/...PUT/GET/DELETE 모두 처리, 자기 bbolt 갱신
//      │
//      ▼  ADR-014 snapshot 을 흘려보냄
//   follower ── PUT/DELETE 거부 (X-KVFS-Primary 헤더로 redirect),
//               GET 만 자기 (stale) bbolt 로 처리.
//               주기적으로 primary snapshot 을 pull → store.Reload(path)
//               으로 hot-swap.
//
// 핵심 trade-off:
//   - read 부하 분산 가능 (follower N 대로)
//   - read 결과는 follower pull interval 만큼 stale (예: 30s 까지)
//   - failover 는 명시적 운영자 절차 (env 바꿔 follower → primary 로 재시작).
//     자동 election 은 별도 ADR (Raft / etcd 통합) 필요.
package edge

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Role identifies edge mode (ADR-022).
type Role string

const (
	RolePrimary  Role = "primary"
	RoleFollower Role = "follower"
)

// FollowerConfig is the follower-only sync settings. Set on Server when
// Role == RoleFollower; ignored otherwise.
type FollowerConfig struct {
	PrimaryURL   string        // e.g. "http://primary:8000"
	DataDir      string        // dir holding edge.db (snapshot is renamed in)
	PullInterval time.Duration // default 30s
	HTTPClient   *http.Client  // optional; default 60s timeout
}

// roleSnapshot summarizes the current role for /v1/admin/role.
type roleSnapshot struct {
	Role         Role      `json:"role"`
	PrimaryURL   string    `json:"primary_url,omitempty"`
	PullInterval string    `json:"pull_interval,omitempty"`
	LastSync     time.Time `json:"last_sync,omitempty"`
	LastSize     int64     `json:"last_size_bytes,omitempty"`
	LastErr      string    `json:"last_err,omitempty"`
	TotalSyncs   uint64    `json:"total_syncs"`
	TotalErrors  uint64    `json:"total_errors"`
}

// followerState is owned by Server when in follower role.
type followerState struct {
	cfg FollowerConfig

	mu        sync.RWMutex
	lastSync  time.Time
	lastSize  int64
	lastErr   string
	totalRuns uint64
	totalErrs uint64

	// syncSeq is the per-sync counter used to pick a unique tmp filename.
	// Same path can't be reopened by bbolt while a prior reload still owns
	// it (file lock); rotating the name avoids the conflict. Old files are
	// unlinked after reload — Linux keeps the inode alive for any open fd.
	syncSeq uint64

	// activePath is the most recently Reload-ed snapshot file, retained so
	// the next sync can unlink it after the new file is loaded.
	activePath string
}

func (s *followerState) recordOK(n int64) {
	s.mu.Lock()
	s.lastSync = time.Now().UTC()
	s.lastSize = n
	s.lastErr = ""
	s.totalRuns++
	s.mu.Unlock()
}

func (s *followerState) recordErr(err error) {
	s.mu.Lock()
	s.lastErr = err.Error()
	s.totalErrs++
	s.mu.Unlock()
}

// runFollowerSync is the periodic snapshot-pull loop. Stops on ctx.Done.
func (s *Server) runFollowerSync(ctx context.Context) {
	st := s.followerSt
	if st == nil {
		return
	}
	cfg := st.cfg
	interval := cfg.PullInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	// Run an initial sync immediately so a fresh follower has data ASAP.
	if err := s.followerSyncOnce(ctx, client, cfg); err != nil {
		s.logger().Warn("follower initial sync failed", slog.String("err", err.Error()))
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.followerSyncOnce(ctx, client, cfg); err != nil {
				s.logger().Warn("follower sync failed", slog.String("err", err.Error()))
			}
		}
	}
}

func (s *Server) followerSyncOnce(ctx context.Context, client *http.Client, cfg FollowerConfig) error {
	url := strings.TrimRight(cfg.PrimaryURL, "/") + "/v1/admin/meta/snapshot"
	pctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, url, nil)
	if err != nil {
		s.followerSt.recordErr(err)
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		s.followerSt.recordErr(err)
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
		s.followerSt.recordErr(err)
		return err
	}
	s.followerSt.mu.Lock()
	s.followerSt.syncSeq++
	seq := s.followerSt.syncSeq
	prevActive := s.followerSt.activePath
	s.followerSt.mu.Unlock()

	tmp := filepath.Join(cfg.DataDir, fmt.Sprintf("edge.db.sync.%d", seq))
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		s.followerSt.recordErr(err)
		return err
	}
	n, copyErr := io.Copy(f, resp.Body)
	if cerr := f.Close(); copyErr == nil {
		copyErr = cerr
	}
	if copyErr != nil {
		_ = os.Remove(tmp)
		s.followerSt.recordErr(copyErr)
		return copyErr
	}
	// Hot-swap: open the synced file as the new bbolt, atomic.Pointer-swap
	// inside the store, and async-close the old one. After Reload succeeds,
	// the previous sync file (if any) can be unlinked — bbolt has either
	// already released it via async-close, or its open fd keeps the inode
	// alive on Linux until close.
	if err := s.Store.Reload(tmp); err != nil {
		_ = os.Remove(tmp)
		s.followerSt.recordErr(err)
		return err
	}
	s.followerSt.mu.Lock()
	s.followerSt.activePath = tmp
	s.followerSt.mu.Unlock()
	if prevActive != "" {
		_ = os.Remove(prevActive)
	}
	s.followerSt.recordOK(n)
	return nil
}

// roleStatus returns the current role + (follower) sync stats.
func (s *Server) roleStatus() roleSnapshot {
	if s.Role == RoleFollower && s.followerSt != nil {
		s.followerSt.mu.RLock()
		defer s.followerSt.mu.RUnlock()
		return roleSnapshot{
			Role:         RoleFollower,
			PrimaryURL:   s.followerSt.cfg.PrimaryURL,
			PullInterval: s.followerSt.cfg.PullInterval.String(),
			LastSync:     s.followerSt.lastSync,
			LastSize:     s.followerSt.lastSize,
			LastErr:      s.followerSt.lastErr,
			TotalSyncs:   s.followerSt.totalRuns,
			TotalErrors:  s.followerSt.totalErrs,
		}
	}
	return roleSnapshot{Role: RolePrimary}
}

// rejectIfFollowerWrite is the middleware-style guard mounted on PUT/DELETE
// data-path handlers. Returns true if it wrote a 503 response.
func (s *Server) rejectIfFollowerWrite(w http.ResponseWriter, r *http.Request) bool {
	if s.Role != RoleFollower {
		return false
	}
	switch r.Method {
	case http.MethodPut, http.MethodPost, http.MethodDelete, http.MethodPatch:
		// Allow admin PUT/POST/DELETE (URL key rotation, dns admin etc.) only
		// against the primary; data-path writes route through this guard.
		if strings.HasPrefix(r.URL.Path, "/v1/o/") {
			if s.followerSt != nil && s.followerSt.cfg.PrimaryURL != "" {
				w.Header().Set("X-KVFS-Primary", s.followerSt.cfg.PrimaryURL)
			}
			writeError(w, http.StatusServiceUnavailable, "edge is in follower role; route writes to primary (see X-KVFS-Primary header)")
			return true
		}
	}
	return false
}
