// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package s3api

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWriteXML_ListBucketsShape(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	created := time.Date(2026, 5, 22, 1, 2, 3, 0, time.UTC)

	WriteXML(rec, req, http.StatusOK, ListAllMyBucketsResult{
		Buckets: []Bucket{{Name: "photos", CreationDate: created}},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/xml") {
		t.Fatalf("Content-Type=%q", ct)
	}
	var out ListAllMyBucketsResult
	if err := xml.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, rec.Body.String())
	}
	if len(out.Buckets) != 1 || out.Buckets[0].Name != "photos" {
		t.Fatalf("buckets=%+v", out.Buckets)
	}
}

func TestWriteXML_ListObjectsV2Shape(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/photos?list-type=2", nil)

	WriteXML(rec, req, http.StatusOK, ListBucketResult{
		Name:        "photos",
		Prefix:      "raw/",
		KeyCount:    1,
		MaxKeys:     1000,
		IsTruncated: false,
		Contents: []ObjectContent{{
			Key:          "raw/a.jpg",
			LastModified: time.Date(2026, 5, 22, 1, 2, 3, 0, time.UTC),
			ETag:         `"abc123"`,
			Size:         7,
			StorageClass: "STANDARD",
		}},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var out ListBucketResult
	if err := xml.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, rec.Body.String())
	}
	if out.Name != "photos" || out.KeyCount != 1 || len(out.Contents) != 1 {
		t.Fatalf("out=%+v", out)
	}
	if out.Contents[0].Key != "raw/a.jpg" || out.Contents[0].ETag != `"abc123"` {
		t.Fatalf("contents=%+v", out.Contents[0])
	}
}

func TestWriteXML_HEADHasNoBody(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodHead, "/photos/raw/a.jpg", nil)

	WriteXML(rec, req, http.StatusOK, ListBucketResult{Name: "photos"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD body len=%d want 0", rec.Body.Len())
	}
}
