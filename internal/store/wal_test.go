// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package store

import (
	"bytes"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestWALAppendAndSince(t *testing.T) {
	dir := t.TempDir()
	w, err := OpenWAL(filepath.Join(dir, "wal.log"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer w.Close()

	for i := 1; i <= 5; i++ {
		seq, _, err := w.Append("put_object", map[string]any{"i": i})
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if seq != int64(i) {
			t.Errorf("seq=%d want %d", seq, i)
		}
	}

	// Read all.
	all, err := w.Since(0)
	if err != nil {
		t.Fatalf("since 0: %v", err)
	}
	if len(all) != 5 {
		t.Errorf("Since(0)=%d want 5", len(all))
	}

	// Read partial.
	tail, err := w.Since(3)
	if err != nil {
		t.Fatalf("since 3: %v", err)
	}
	if len(tail) != 2 {
		t.Errorf("Since(3)=%d want 2", len(tail))
	}
	if tail[0].Seq != 4 {
		t.Errorf("Since(3) first seq=%d want 4", tail[0].Seq)
	}
}

func TestWALRecoverLastSeq(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal.log")

	w, _ := OpenWAL(path)
	for i := 1; i <= 3; i++ {
		_, _, _ = w.Append("delete_object", map[string]any{"i": i})
	}
	_ = w.Close()

	w2, err := OpenWAL(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer w2.Close()
	if w2.LastSeq() != 3 {
		t.Errorf("after reopen LastSeq=%d want 3", w2.LastSeq())
	}
	// Continuing to append must produce seq 4.
	seq, _, _ := w2.Append("put_object", "hi")
	if seq != 4 {
		t.Errorf("next append seq=%d want 4", seq)
	}
}

func TestWALTruncate(t *testing.T) {
	dir := t.TempDir()
	w, _ := OpenWAL(filepath.Join(dir, "wal.log"))
	defer w.Close()

	for i := 0; i < 5; i++ {
		_, _, _ = w.Append("put_object", i)
	}
	prev, err := w.Truncate()
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if prev != 5 {
		t.Errorf("truncate returned prev=%d want 5", prev)
	}
	if w.LastSeq() != 0 {
		t.Errorf("after truncate LastSeq=%d want 0", w.LastSeq())
	}
	// New appends start at 1.
	seq, _, _ := w.Append("delete_object", nil)
	if seq != 1 {
		t.Errorf("post-truncate append seq=%d want 1", seq)
	}
}

func TestWALWriteSinceTo(t *testing.T) {
	dir := t.TempDir()
	w, _ := OpenWAL(filepath.Join(dir, "wal.log"))
	defer w.Close()
	for i := 1; i <= 4; i++ {
		_, _, _ = w.Append("put_object", i)
	}

	var buf bytes.Buffer
	n, err := w.WriteSinceTo(2, &buf)
	if err != nil {
		t.Fatalf("WriteSinceTo: %v", err)
	}
	if n != 2 {
		t.Errorf("count=%d want 2", n)
	}
	// Quick sanity: the buffer should contain seq 3 and 4.
	body := buf.String()
	if !contains(body, `"seq":3`) || !contains(body, `"seq":4`) {
		t.Errorf("WriteSinceTo body missing expected seqs: %q", body)
	}
}

func TestStoreEmitsWALOnPutAndDelete(t *testing.T) {
	dir := t.TempDir()
	wal, _ := OpenWAL(filepath.Join(dir, "wal.log"))
	defer wal.Close()
	st, err := Open(filepath.Join(dir, "edge.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	st.SetWAL(wal)

	if err := st.PutObject(&ObjectMeta{
		Bucket: "b", Key: "k",
		Chunks: []ChunkRef{{ChunkID: "c1", Replicas: []string{":1"}}},
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := st.DeleteObject("b", "k"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := st.AddRuntimeDN(":8001"); err != nil {
		t.Fatalf("add dn: %v", err)
	}
	if err := st.RemoveRuntimeDN(":8001"); err != nil {
		t.Fatalf("remove dn: %v", err)
	}

	entries, _ := wal.Since(0)
	if len(entries) != 4 {
		t.Fatalf("entries=%d want 4 (put/delete/add/remove)", len(entries))
	}
	wantOps := []string{"put_object", "delete_object", "add_runtime_dn", "remove_runtime_dn"}
	for i, e := range entries {
		if e.Op != wantOps[i] {
			t.Errorf("entry %d op=%q want %q", i, e.Op, wantOps[i])
		}
	}
}

func TestApplyEntryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	src, _ := Open(filepath.Join(dir, "src.db"))
	defer src.Close()
	wal, _ := OpenWAL(filepath.Join(dir, "wal.log"))
	defer wal.Close()
	src.SetWAL(wal)

	// Mutate src store.
	_ = src.PutObject(&ObjectMeta{
		Bucket: "b", Key: "k1",
		Chunks: []ChunkRef{{ChunkID: "c1", Replicas: []string{":1"}}},
	})
	_ = src.AddRuntimeDN(":8001")
	_ = src.AddRuntimeDN(":8002")
	_ = src.PutObject(&ObjectMeta{
		Bucket: "b", Key: "k2",
		Chunks: []ChunkRef{{ChunkID: "c2", Replicas: []string{":2"}}},
	})

	entries, _ := wal.Since(0)
	if len(entries) != 4 {
		t.Fatalf("entries=%d want 4", len(entries))
	}

	// Replay onto a fresh dst store.
	dst, _ := Open(filepath.Join(dir, "dst.db"))
	defer dst.Close()
	if err := dst.ApplyAll(entries); err != nil {
		t.Fatalf("apply all: %v", err)
	}

	// Verify dst has same objects + DNs.
	for _, k := range []string{"k1", "k2"} {
		got, err := dst.GetObject("b", k)
		if err != nil {
			t.Errorf("dst missing %s: %v", k, err)
		} else if got.Key != k {
			t.Errorf("dst key mismatch: %s", got.Key)
		}
	}
	dns, _ := dst.ListRuntimeDNs()
	if len(dns) != 2 {
		t.Errorf("dst dns=%d want 2", len(dns))
	}
}

// ADR-035: batched fsync — concurrent Appends should all return success
// with monotonic seqs, and Since(0) should see them all in order.
func TestWALBatchedConcurrentAppends(t *testing.T) {
	dir := t.TempDir()
	w, err := OpenWALWithBatch(filepath.Join(dir, "wal.log"), 5*time.Millisecond)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer w.Close()

	const N = 50
	var wg sync.WaitGroup
	seqs := make([]int64, N)
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			seq, _, err := w.Append("put_object", map[string]int{"i": i})
			seqs[i] = seq
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i := 0; i < N; i++ {
		if errs[i] != nil {
			t.Errorf("append %d: %v", i, errs[i])
		}
	}
	// All seqs must be unique and within [1, N].
	seen := map[int64]bool{}
	for _, s := range seqs {
		if s < 1 || s > N {
			t.Errorf("seq %d out of range [1,%d]", s, N)
		}
		if seen[s] {
			t.Errorf("duplicate seq %d", s)
		}
		seen[s] = true
	}

	entries, err := w.Since(0)
	if err != nil {
		t.Fatalf("since: %v", err)
	}
	if len(entries) != N {
		t.Errorf("Since(0)=%d want %d", len(entries), N)
	}
	for i, e := range entries {
		if int(e.Seq) != i+1 {
			t.Errorf("on-disk seq order: entry %d has seq %d, want %d", i, e.Seq, i+1)
		}
	}
}

// Truncate while a batched-mode Append is parked on cond MUST NOT deadlock.
// Pre-fix the waiter would re-park forever because durableSeq was reset to 0
// while its target seq stayed > 0. Post-fix Truncate bumps epoch and the
// waiter returns an error.
func TestWALBatchedTruncateUnblocksWaiter(t *testing.T) {
	dir := t.TempDir()
	// Long batch interval so the Append parks before the flusher fires.
	w, err := OpenWALWithBatch(filepath.Join(dir, "wal.log"), 500*time.Millisecond)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer w.Close()

	done := make(chan error, 1)
	go func() {
		_, _, err := w.Append("put_object", "x")
		done <- err
	}()
	// Give the Append goroutine time to write its line and park on cond.
	time.Sleep(50 * time.Millisecond)

	if _, err := w.Truncate(); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Errorf("Append should have errored after Truncate, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Append deadlocked after Truncate")
	}
}

// Closing a batched WAL with no in-flight appends should not hang or error.
func TestWALBatchedCleanClose(t *testing.T) {
	dir := t.TempDir()
	w, err := OpenWALWithBatch(filepath.Join(dir, "wal.log"), 10*time.Millisecond)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, _, err := w.Append("put_object", "x"); err != nil {
		t.Fatalf("append: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- w.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Close hung")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && bytes_contains(s, substr)
}
func bytes_contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
