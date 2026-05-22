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

func TestWriteXML_InitiateMultipartUploadShape(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/photos/a.txt?uploads", nil)

	WriteXML(rec, req, http.StatusOK, InitiateMultipartUploadResult{
		Bucket:   "photos",
		Key:      "a.txt",
		UploadID: "upload-1",
	})

	var out InitiateMultipartUploadResult
	if err := xml.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, rec.Body.String())
	}
	if out.Bucket != "photos" || out.Key != "a.txt" || out.UploadID != "upload-1" {
		t.Fatalf("out=%+v", out)
	}
	if !strings.Contains(rec.Body.String(), `xmlns="http://s3.amazonaws.com/doc/2006-03-01/"`) {
		t.Fatalf("missing S3 namespace: %s", rec.Body.String())
	}
}

func TestWriteXML_ListPartsShape(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/photos/a.txt?uploadId=upload-1", nil)

	WriteXML(rec, req, http.StatusOK, ListPartsResult{
		Bucket:      "photos",
		Key:         "a.txt",
		UploadID:    "upload-1",
		MaxParts:    1000,
		IsTruncated: false,
		Parts: []Part{{
			PartNumber:   1,
			LastModified: time.Date(2026, 5, 22, 1, 2, 3, 0, time.UTC),
			ETag:         `"p1"`,
			Size:         5,
		}},
	})

	var out ListPartsResult
	if err := xml.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, rec.Body.String())
	}
	if out.Bucket != "photos" || out.Key != "a.txt" || out.UploadID != "upload-1" {
		t.Fatalf("out=%+v", out)
	}
	if len(out.Parts) != 1 || out.Parts[0].PartNumber != 1 || out.Parts[0].ETag != `"p1"` {
		t.Fatalf("parts=%+v", out.Parts)
	}
}

func TestParseCompleteMultipartUpload(t *testing.T) {
	body := strings.NewReader(`<CompleteMultipartUpload>
  <Part><PartNumber>1</PartNumber><ETag>"p1"</ETag></Part>
  <Part><PartNumber>2</PartNumber><ETag>"p2"</ETag></Part>
</CompleteMultipartUpload>`)

	parts, err := ParseCompleteMultipartUpload(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parts) != 2 || parts[0].PartNumber != 1 || parts[0].ETag != `"p1"` || parts[1].PartNumber != 2 {
		t.Fatalf("parts=%+v", parts)
	}
}

func TestWriteXML_CompleteMultipartUploadShape(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/photos/a.txt?uploadId=upload-1", nil)

	WriteXML(rec, req, http.StatusOK, CompleteMultipartUploadResult{
		Location: "http://kvfs.local/photos/a.txt",
		Bucket:   "photos",
		Key:      "a.txt",
		ETag:     `"final-2"`,
	})

	var out CompleteMultipartUploadResult
	if err := xml.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, rec.Body.String())
	}
	if out.Bucket != "photos" || out.Key != "a.txt" || out.ETag != `"final-2"` {
		t.Fatalf("out=%+v", out)
	}
}
