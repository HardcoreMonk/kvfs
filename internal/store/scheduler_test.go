// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newSeededStore(t *testing.T) (*MetaStore, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "edge.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, key := range []string{"a", "b", "c"} {
		if err := st.PutObject(&ObjectMeta{
			Bucket: "bk", Key: key,
			Chunks: []ChunkRef{{ChunkID: "c-" + key, Replicas: []string{":1"}}},
		}); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	return st, dir
}

func TestSchedulerOneRunCreatesSnapshot(t *testing.T) {
	st, _ := newSeededStore(t)
	defer st.Close()
	dir := t.TempDir()

	sch := NewSnapshotScheduler(st, dir, 50*time.Millisecond, 5)
	sch.runOnce()

	stats := sch.Stats()
	if stats.TotalRuns != 1 {
		t.Errorf("TotalRuns=%d want 1", stats.TotalRuns)
	}
	if stats.LastErr != "" {
		t.Errorf("LastErr=%q want empty", stats.LastErr)
	}
	if stats.LastSize <= 0 {
		t.Errorf("LastSize=%d want > 0", stats.LastSize)
	}
	if len(stats.History) != 1 {
		t.Errorf("History=%d want 1", len(stats.History))
	}
	if !strings.HasPrefix(stats.History[0].Name, snapshotPrefix) {
		t.Errorf("snapshot name=%q wrong prefix", stats.History[0].Name)
	}
}

func TestSchedulerPruneRetainsKeep(t *testing.T) {
	st, _ := newSeededStore(t)
	defer st.Close()
	dir := t.TempDir()

	sch := NewSnapshotScheduler(st, dir, time.Hour, 3) // interval irrelevant; we drive runOnce
	for i := 0; i < 5; i++ {
		sch.runOnce()
		// snapshot filenames are second-resolution; sleep enough to differ.
		time.Sleep(1100 * time.Millisecond)
	}

	stats := sch.Stats()
	if len(stats.History) != 3 {
		t.Errorf("History=%d want 3 (keep=3)", len(stats.History))
	}
	// Newest-first ordering.
	for i := 1; i < len(stats.History); i++ {
		if stats.History[i-1].ModTime.Before(stats.History[i].ModTime) {
			t.Errorf("history not newest-first at idx %d", i)
		}
	}
}

func TestSchedulerStatsWithoutRuns(t *testing.T) {
	st, _ := newSeededStore(t)
	defer st.Close()
	dir := t.TempDir()
	sch := NewSnapshotScheduler(st, dir, time.Second, 5)
	stats := sch.Stats()
	if stats.TotalRuns != 0 || stats.LastSize != 0 {
		t.Errorf("fresh scheduler stats=%+v want zeros", stats)
	}
	if !stats.Enabled {
		t.Errorf("Enabled=false want true (interval > 0)")
	}
}

func TestSchedulerSkipsNonSnapshotFiles(t *testing.T) {
	st, _ := newSeededStore(t)
	defer st.Close()
	dir := t.TempDir()
	// Drop a stray file; prune must ignore it.
	stray := filepath.Join(dir, "README.txt")
	if err := os.WriteFile(stray, []byte("ignore me"), 0o644); err != nil {
		t.Fatalf("write stray: %v", err)
	}
	sch := NewSnapshotScheduler(st, dir, time.Second, 1)
	sch.runOnce()
	time.Sleep(1100 * time.Millisecond)
	sch.runOnce()

	if _, err := os.Stat(stray); err != nil {
		t.Errorf("stray file deleted by prune: %v", err)
	}
	stats := sch.Stats()
	if len(stats.History) != 1 {
		t.Errorf("History=%d want 1", len(stats.History))
	}
}
