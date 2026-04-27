// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Bit-rot scrubber (ADR-054, S7 Ep.4):
//
// A periodic background pass that re-reads every chunk on disk, recomputes
// sha256, and compares it to the chunk_id (= file name). Any mismatch
// proves the on-disk byte sequence drifted from what was written —
// classic silent disk corruption. The corrupt set is exposed via
// GET /chunks/scrub-status so anti-entropy on coord can quarantine the
// chunk and trigger repair.
//
// Pacing: scan one chunk per ScrubInterval to keep IO bounded. A 10⁵-
// chunk DN at 10 ms/chunk finishes in ~17 minutes — fast enough that
// a corruption window is short, slow enough not to thrash the disk.
package dn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// ScrubStatus is the JSON body of GET /chunks/scrub-status. Mostly
// observability — coord uses it to know "is the scrubber alive?" and
// "how many chunks are known-corrupt?". The corrupt list is bounded
// by the on-disk count so it can never grow unbounded.
type ScrubStatus struct {
	DN              string    `json:"dn"`
	Running         bool      `json:"running"`
	IntervalSeconds float64   `json:"interval_seconds"`
	LastScanStarted time.Time `json:"last_scan_started"`
	LastScanEnd     time.Time `json:"last_scan_end,omitempty"`
	ChunksScrubbed  int64     `json:"chunks_scrubbed_total"`
	CorruptCount    int       `json:"corrupt_count"`
	Corrupt         []string  `json:"corrupt"` // sorted, possibly truncated for big sets
}

// scrubState lives on Server (Server.scrubMu guards mutation). The
// embedded fields are copied during status snapshots — that's fine,
// they're scalars + a map header that's protected by the same mutex.
type scrubState struct {
	enabled         bool
	interval        time.Duration
	lastScanStarted time.Time
	lastScanEnd     time.Time
	chunksScrubbed  int64
	corrupt         map[string]struct{}
}

// StartScrubber begins the background bit-rot detection loop. Idempotent
// per-Server — a second call is a no-op (ScrubInterval cannot be changed
// once started). interval ≤ 0 disables the scrubber entirely (default
// off — operators opt in via DN_SCRUB_INTERVAL or programmatic call).
func (s *Server) StartScrubber(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	s.scrubMu.Lock()
	if s.scrub.enabled {
		s.scrubMu.Unlock()
		return
	}
	s.scrub = scrubState{
		enabled:  true,
		interval: interval,
		corrupt:  make(map[string]struct{}),
	}
	s.scrubMu.Unlock()

	go s.scrubLoop(ctx)
}

// scrubLoop walks the on-disk chunks at one-per-tick pace. A full scan
// completes when filepath.Walk visits every file. Between scans there
// is no sleep — the rate-limit is per-chunk, so scrubber load is
// proportional to chunk count regardless of disk fullness.
func (s *Server) scrubLoop(ctx context.Context) {
	for {
		s.scrubMu.Lock()
		s.scrub.lastScanStarted = time.Now().UTC()
		interval := s.scrub.interval
		s.scrubMu.Unlock()

		root := filepath.Join(s.dataDir, "chunks")
		_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil || info == nil || info.IsDir() {
				return nil
			}
			dir := filepath.Base(filepath.Dir(p))
			base := filepath.Base(p)
			if len(dir) != 2 || len(base) != 62 {
				return nil
			}
			id := dir + base
			if !validChunkID(id) {
				return nil
			}
			s.scrubOne(p, id)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(interval):
			}
			return nil
		})

		s.scrubMu.Lock()
		s.scrub.lastScanEnd = time.Now().UTC()
		s.scrubMu.Unlock()

		// Pause briefly between full scans (5× the per-chunk interval,
		// capped at 1 minute). Scans don't need to chase each other —
		// once is enough until the next interval fires.
		breath := interval * 5
		if breath > time.Minute {
			breath = time.Minute
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(breath):
		}
	}
}

// scrubOne reads one chunk file and verifies sha256(body) == id.
// On mismatch (or read error) the id joins the corrupt set; subsequent
// reads from outside the scrubber go through the existing handleGet
// path which is unchanged — the corrupt set is informational, surfaced
// to coord via /chunks/scrub-status. We deliberately do NOT delete the
// bad file: anti-entropy on coord may want to compare against another
// replica before deciding repair vs accept-as-truth.
func (s *Server) scrubOne(path, id string) {
	data, err := os.ReadFile(path)
	if err != nil {
		s.markCorrupt(id) // unreadable = also bad
		return
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != id {
		s.markCorrupt(id)
		return
	}
	// Healthy read — clear from corrupt set in case a previous scan
	// flagged it (e.g. transient read error already remediated).
	s.scrubMu.Lock()
	delete(s.scrub.corrupt, id)
	s.scrub.chunksScrubbed++
	s.scrubMu.Unlock()
}

func (s *Server) markCorrupt(id string) {
	s.scrubMu.Lock()
	if s.scrub.corrupt == nil {
		s.scrub.corrupt = make(map[string]struct{})
	}
	s.scrub.corrupt[id] = struct{}{}
	s.scrub.chunksScrubbed++
	s.scrubMu.Unlock()
}

// MarkCorruptForTest exposes corruption injection so demos / tests can
// simulate bit-rot without actually flipping bytes on disk. Production
// has no use for this entry point.
func (s *Server) MarkCorruptForTest(id string) { s.markCorrupt(id) }

// handleScrubStatus serves /chunks/scrub-status. Always 200 — the
// scrubber being disabled is a valid state communicated by Running=false.
func (s *Server) handleScrubStatus(w http.ResponseWriter, _ *http.Request) {
	s.scrubMu.Lock()
	st := s.scrub
	corrupt := make([]string, 0, len(st.corrupt))
	for id := range st.corrupt {
		corrupt = append(corrupt, id)
	}
	s.scrubMu.Unlock()
	sort.Strings(corrupt)

	resp := ScrubStatus{
		DN:              s.id,
		Running:         st.enabled,
		IntervalSeconds: st.interval.Seconds(),
		LastScanStarted: st.lastScanStarted,
		LastScanEnd:     st.lastScanEnd,
		ChunksScrubbed:  st.chunksScrubbed,
		CorruptCount:    len(corrupt),
		Corrupt:         corrupt,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
