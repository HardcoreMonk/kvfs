// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestMultipartUploadLifecycle(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if _, err := st.CreateBucket("photos"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	up, err := st.CreateMultipartUpload("photos", "raw/a.txt", "text/plain")
	if err != nil {
		t.Fatalf("initiate: %v", err)
	}
	if up.UploadID == "" || up.Bucket != "photos" || up.Key != "raw/a.txt" || up.ContentType != "text/plain" {
		t.Fatalf("upload meta = %+v", up)
	}

	part2 := MultipartPartMeta{PartNumber: 2, ETag: `"p2"`, Size: 2, Chunks: []ChunkRef{{ChunkID: "c2", Size: 2, Replicas: []string{"dn1"}}}}
	part1 := MultipartPartMeta{PartNumber: 1, ETag: `"p1"`, Size: 1, Chunks: []ChunkRef{{ChunkID: "c1", Size: 1, Replicas: []string{"dn1"}}}}
	if err := st.PutMultipartPart("photos", "raw/a.txt", up.UploadID, part2); err != nil {
		t.Fatalf("put part2: %v", err)
	}
	if err := st.PutMultipartPart("photos", "raw/a.txt", up.UploadID, part1); err != nil {
		t.Fatalf("put part1: %v", err)
	}

	parts, err := st.ListMultipartParts("photos", "raw/a.txt", up.UploadID)
	if err != nil {
		t.Fatalf("list parts: %v", err)
	}
	if len(parts) != 2 || parts[0].PartNumber != 1 || parts[1].PartNumber != 2 {
		t.Fatalf("parts = %+v, want sorted 1,2", parts)
	}

	obj, completed, err := st.CompleteMultipartUpload("photos", "raw/a.txt", up.UploadID, []CompletePart{
		{PartNumber: 1, ETag: `"p1"`},
		{PartNumber: 2, ETag: `"p2"`},
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if obj.Bucket != "photos" || obj.Key != "raw/a.txt" || obj.Size != 3 || obj.ContentType != "text/plain" || len(obj.Chunks) != 2 {
		t.Fatalf("object = %+v", obj)
	}
	if len(completed) != 2 || completed[0].PartNumber != 1 || completed[1].PartNumber != 2 {
		t.Fatalf("completed = %+v", completed)
	}
	if _, err := st.GetMultipartUpload("photos", "raw/a.txt", up.UploadID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get completed upload err = %v, want ErrNotFound", err)
	}
	got, err := st.GetObject("photos", "raw/a.txt")
	if err != nil {
		t.Fatalf("get completed object: %v", err)
	}
	if got.ETag == "" {
		t.Fatalf("completed object ETag missing: %+v", got)
	}
}

func TestMultipartCompleteValidatesOrderAndETag(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if _, err := st.CreateBucket("photos"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	up, err := st.CreateMultipartUpload("photos", "a.txt", "")
	if err != nil {
		t.Fatalf("initiate: %v", err)
	}
	for _, p := range []MultipartPartMeta{
		{PartNumber: 1, ETag: `"p1"`, Size: 1, Chunks: []ChunkRef{{ChunkID: "c1", Size: 1, Replicas: []string{"dn1"}}}},
		{PartNumber: 2, ETag: `"p2"`, Size: 1, Chunks: []ChunkRef{{ChunkID: "c2", Size: 1, Replicas: []string{"dn1"}}}},
	} {
		if err := st.PutMultipartPart("photos", "a.txt", up.UploadID, p); err != nil {
			t.Fatalf("put part %d: %v", p.PartNumber, err)
		}
	}
	if _, _, err := st.CompleteMultipartUpload("photos", "a.txt", up.UploadID, []CompletePart{{PartNumber: 2}, {PartNumber: 1}}); !errors.Is(err, ErrInvalidPartOrder) {
		t.Fatalf("out-of-order complete err = %v, want ErrInvalidPartOrder", err)
	}
	if _, _, err := st.CompleteMultipartUpload("photos", "a.txt", up.UploadID, []CompletePart{{PartNumber: 1, ETag: `"bad"`}}); !errors.Is(err, ErrInvalidPart) {
		t.Fatalf("etag mismatch err = %v, want ErrInvalidPart", err)
	}
	if _, _, err := st.CompleteMultipartUpload("photos", "a.txt", up.UploadID, []CompletePart{{PartNumber: 3}}); !errors.Is(err, ErrInvalidPart) {
		t.Fatalf("missing part err = %v, want ErrInvalidPart", err)
	}
}

func TestMultipartAbortExpiredUploads(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if _, err := st.CreateBucket("photos"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	old, err := st.CreateMultipartUpload("photos", "old.txt", "")
	if err != nil {
		t.Fatalf("init old: %v", err)
	}
	fresh, err := st.CreateMultipartUpload("photos", "fresh.txt", "")
	if err != nil {
		t.Fatalf("init fresh: %v", err)
	}
	old.CreatedAt = time.Now().Add(-48 * time.Hour)
	if err := st.putMultipartUploadReplay(old); err != nil {
		t.Fatalf("age old: %v", err)
	}

	removed, err := st.AbortExpiredMultipartUploads(time.Now().Add(-24*time.Hour), 10)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if len(removed) != 1 || removed[0].UploadID != old.UploadID {
		t.Fatalf("removed = %+v, want old", removed)
	}
	if _, err := st.GetMultipartUpload("photos", "old.txt", old.UploadID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old err = %v, want ErrNotFound", err)
	}
	if _, err := st.GetMultipartUpload("photos", "fresh.txt", fresh.UploadID); err != nil {
		t.Fatalf("fresh missing: %v", err)
	}
	aborted, err := st.AbortMultipartUpload("photos", "fresh.txt", fresh.UploadID)
	if err != nil {
		t.Fatalf("abort fresh: %v", err)
	}
	if aborted.UploadID != fresh.UploadID {
		t.Fatalf("aborted = %+v, want fresh", aborted)
	}
}

func TestMultipartWALReplay(t *testing.T) {
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
		t.Fatalf("create bucket: %v", err)
	}
	up, err := src.CreateMultipartUpload("photos", "a.txt", "text/plain")
	if err != nil {
		t.Fatalf("initiate: %v", err)
	}
	if err := src.PutMultipartPart("photos", "a.txt", up.UploadID, MultipartPartMeta{
		PartNumber: 1, ETag: `"p1"`, Size: 1,
		Chunks: []ChunkRef{{ChunkID: "c1", Size: 1, Replicas: []string{"dn1"}}},
	}); err != nil {
		t.Fatalf("put part: %v", err)
	}
	if _, _, err := src.CompleteMultipartUpload("photos", "a.txt", up.UploadID, []CompletePart{{PartNumber: 1, ETag: `"p1"`}}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	entries, err := wal.Since(0)
	if err != nil {
		t.Fatalf("wal since: %v", err)
	}
	dst, err := Open(filepath.Join(dir, "dst.db"))
	if err != nil {
		t.Fatalf("open dst: %v", err)
	}
	defer dst.Close()
	if err := dst.ApplyAll(entries); err != nil {
		t.Fatalf("apply all: %v", err)
	}
	got, err := dst.GetObject("photos", "a.txt")
	if err != nil {
		t.Fatalf("dst object: %v", err)
	}
	if got.Size != 1 || got.ContentType != "text/plain" || len(got.Chunks) != 1 {
		t.Fatalf("dst object = %+v", got)
	}
	if _, err := dst.GetMultipartUpload("photos", "a.txt", up.UploadID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("dst upload err = %v, want ErrNotFound", err)
	}
}
