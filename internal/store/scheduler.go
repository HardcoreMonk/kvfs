// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Auto-snapshot scheduler (ADR-016): periodic in-edge ticker that drops
// timestamped snapshots into a directory and prunes oldest beyond `keep`.
//
// 비전공자용 해설
// ──────────────
// ADR-014 가 manual snapshot endpoint 를 줬다. 운영자가 cron + curl 로 주기적
// snapshot 자동화 가능하지만, 클러스터 외부 시스템이 필요하다 (cron daemon /
// systemd timer / Kubernetes CronJob). 작은 single-node 데모에서는 부담.
//
// 본 모듈은 edge 안에서 같은 일을 한다. ADR-013 auto-trigger 의 ticker 패턴 그대로:
//   - StartScheduler(ctx, dir, interval, keep) → goroutine 1개
//   - 매 tick: dir 에 timestamped snapshot 작성 (atomic temp+rename)
//   - 작성 후: dir 의 snap 파일들을 mtime 정렬, 오래된 것 (keep 초과) 삭제
//
// 외부 백업 (S3 sync 등) 은 여전히 운영자 몫 — 이 모듈은 단지 dir 에 always-fresh
// snapshot 을 보장.
package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// SnapshotInfo describes one file in the snapshot directory.
type SnapshotInfo struct {
	Path      string    `json:"path"`
	Name      string    `json:"name"`
	SizeBytes int64     `json:"size_bytes"`
	ModTime   time.Time `json:"mod_time"`
}

// SchedulerStats summarizes the scheduler's recent activity.
type SchedulerStats struct {
	Enabled       bool          `json:"enabled"`
	Dir           string        `json:"dir,omitempty"`
	Interval      string        `json:"interval,omitempty"`
	Keep          int           `json:"keep,omitempty"`
	LastRun       time.Time     `json:"last_run,omitempty"`
	LastSize      int64         `json:"last_size_bytes,omitempty"`
	LastErr       string        `json:"last_err,omitempty"`
	TotalRuns     uint64        `json:"total_runs"`
	TotalErrors   uint64        `json:"total_errors"`
	History       []SnapshotInfo `json:"history,omitempty"`
}

// SnapshotScheduler is a ticker-driven periodic snapshotter with rotation.
type SnapshotScheduler struct {
	store    *MetaStore
	dir      string
	interval time.Duration
	keep     int

	mu       sync.RWMutex
	lastRun  time.Time
	lastSize int64
	lastErr  string
	runs     uint64
	errs     uint64
}

const snapshotPrefix = "kvfs-meta-snap-"
const snapshotSuffix = ".bbolt"

// NewSnapshotScheduler returns a scheduler. keep ≤ 0 falls back to 7.
func NewSnapshotScheduler(store *MetaStore, dir string, interval time.Duration, keep int) *SnapshotScheduler {
	if keep <= 0 {
		keep = 7
	}
	return &SnapshotScheduler{store: store, dir: dir, interval: interval, keep: keep}
}

// Run blocks until ctx.Done. Performs one snapshot immediately, then on each
// tick. Errors are recorded in stats but do not stop the loop.
func (s *SnapshotScheduler) Run(stop <-chan struct{}) {
	if s.interval <= 0 {
		return
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		s.recordErr(fmt.Errorf("mkdir %s: %w", s.dir, err))
		return
	}
	s.runOnce()
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s.runOnce()
		}
	}
}

func (s *SnapshotScheduler) runOnce() {
	name := snapshotPrefix + time.Now().UTC().Format("20060102T150405Z") + snapshotSuffix
	full := filepath.Join(s.dir, name)
	tmp := full + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		s.recordErr(fmt.Errorf("open %s: %w", tmp, err))
		return
	}
	n, snapErr := s.store.Snapshot(f)
	if cerr := f.Close(); snapErr == nil {
		snapErr = cerr
	}
	if snapErr != nil {
		_ = os.Remove(tmp)
		s.recordErr(fmt.Errorf("snapshot: %w", snapErr))
		return
	}
	if err := os.Rename(tmp, full); err != nil {
		_ = os.Remove(tmp)
		s.recordErr(fmt.Errorf("rename %s: %w", full, err))
		return
	}
	s.recordOK(n)
	if err := s.prune(); err != nil {
		s.recordErr(fmt.Errorf("prune: %w", err))
	}
	// Log compaction (ADR-019 follow-up): the fresh snapshot embeds all
	// current state, so any pre-snapshot WAL entry is redundant. Truncate
	// the WAL to seq=0; followers pulling WAL since-N before this point
	// will fall back to a full snapshot pull (their existing path).
	if w := s.store.WAL(); w != nil {
		if _, err := w.Truncate(); err != nil {
			s.recordErr(fmt.Errorf("wal truncate: %w", err))
		}
	}
}

// prune deletes oldest snapshot files beyond `keep`. Newest are kept by mtime.
func (s *SnapshotScheduler) prune() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	type snap struct {
		path string
		mod  time.Time
	}
	var snaps []snap
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, snapshotPrefix) || !strings.HasSuffix(name, snapshotSuffix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		snaps = append(snaps, snap{filepath.Join(s.dir, name), info.ModTime()})
	}
	if len(snaps) <= s.keep {
		return nil
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].mod.After(snaps[j].mod) })
	for _, sn := range snaps[s.keep:] {
		if err := os.Remove(sn.path); err != nil {
			return err
		}
	}
	return nil
}

func (s *SnapshotScheduler) recordOK(size int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastRun = time.Now().UTC()
	s.lastSize = size
	s.lastErr = ""
	s.runs++
}

func (s *SnapshotScheduler) recordErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastErr = err.Error()
	s.errs++
}

// Stats returns the current scheduler status + a sorted-newest-first listing
// of the on-disk snapshots.
func (s *SnapshotScheduler) Stats() SchedulerStats {
	s.mu.RLock()
	st := SchedulerStats{
		Enabled:     s.interval > 0,
		Dir:         s.dir,
		Interval:    s.interval.String(),
		Keep:        s.keep,
		LastRun:     s.lastRun,
		LastSize:    s.lastSize,
		LastErr:     s.lastErr,
		TotalRuns:   s.runs,
		TotalErrors: s.errs,
	}
	s.mu.RUnlock()

	st.History = listSnapshots(s.dir)
	return st
}

func listSnapshots(dir string) []SnapshotInfo {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []SnapshotInfo
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, snapshotPrefix) || !strings.HasSuffix(name, snapshotSuffix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, SnapshotInfo{
			Path:      filepath.Join(dir, name),
			Name:      name,
			SizeBytes: info.Size(),
			ModTime:   info.ModTime().UTC(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	return out
}
