// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestBucketRegistry_CreateGetListDelete(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	created, err := st.CreateBucket("photos")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if created.Name != "photos" || created.CreatedAt.IsZero() {
		t.Fatalf("created bucket = %+v", created)
	}

	got, err := st.GetBucket("photos")
	if err != nil {
		t.Fatalf("get bucket: %v", err)
	}
	if got.Name != "photos" || !got.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("got bucket = %+v, want %+v", got, created)
	}

	if _, err := st.CreateBucket("logs"); err != nil {
		t.Fatalf("create logs: %v", err)
	}
	buckets, err := st.ListBuckets()
	if err != nil {
		t.Fatalf("list buckets: %v", err)
	}
	if len(buckets) != 2 || buckets[0].Name != "logs" || buckets[1].Name != "photos" {
		t.Fatalf("buckets = %+v, want logs/photos sorted", buckets)
	}

	if err := st.DeleteBucket("logs"); err != nil {
		t.Fatalf("delete logs: %v", err)
	}
	if _, err := st.GetBucket("logs"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get deleted bucket err = %v, want ErrNotFound", err)
	}
}

func TestBucketRegistry_DuplicateInvalidAndNonEmpty(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	if _, err := st.CreateBucket("photos"); err != nil {
		t.Fatalf("create photos: %v", err)
	}
	if _, err := st.CreateBucket("photos"); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate create err = %v, want ErrAlreadyExists", err)
	}
	for _, name := range []string{"ab", "-bad", "bad-", "Bad", "bad..dots", "192.168.1.1"} {
		if _, err := st.CreateBucket(name); err == nil {
			t.Fatalf("CreateBucket(%q) succeeded, want validation error", name)
		}
	}

	if err := st.PutObject(&ObjectMeta{
		Bucket: "photos", Key: "a.jpg", Size: 1,
		Chunks: []ChunkRef{{ChunkID: "c1", Size: 1, Replicas: []string{"dn1"}}},
	}); err != nil {
		t.Fatalf("put object: %v", err)
	}
	has, err := st.BucketHasObjects("photos")
	if err != nil {
		t.Fatalf("BucketHasObjects: %v", err)
	}
	if !has {
		t.Fatal("BucketHasObjects = false, want true")
	}
	if err := st.DeleteBucket("photos"); !errors.Is(err, ErrBucketNotEmpty) {
		t.Fatalf("delete non-empty err = %v, want ErrBucketNotEmpty", err)
	}
}

func TestBucketRegistry_WALReplay(t *testing.T) {
	dir := t.TempDir()
	src, err := Open(filepath.Join(dir, "src.db"))
	if err != nil {
		t.Fatalf("open src: %v", err)
	}
	defer src.Close()
	wal, err := OpenWAL(filepath.Join(dir, "wal.log"))
	if err != nil {
		t.Fatalf("open wal: %v", err)
	}
	defer wal.Close()
	src.SetWAL(wal)

	if _, err := src.CreateBucket("photos"); err != nil {
		t.Fatalf("create photos: %v", err)
	}
	if _, err := src.CreateBucket("logs"); err != nil {
		t.Fatalf("create logs: %v", err)
	}
	if err := src.DeleteBucket("logs"); err != nil {
		t.Fatalf("delete logs: %v", err)
	}

	entries, err := wal.Since(0)
	if err != nil {
		t.Fatalf("wal since: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries=%d want 3", len(entries))
	}

	dst, err := Open(filepath.Join(dir, "dst.db"))
	if err != nil {
		t.Fatalf("open dst: %v", err)
	}
	defer dst.Close()
	if err := dst.ApplyAll(entries); err != nil {
		t.Fatalf("apply all: %v", err)
	}
	if _, err := dst.GetBucket("photos"); err != nil {
		t.Fatalf("dst missing photos: %v", err)
	}
	if _, err := dst.GetBucket("logs"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("dst logs err = %v, want ErrNotFound", err)
	}
}
