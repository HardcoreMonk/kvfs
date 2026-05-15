// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package s3api

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteError_XMLShape(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)

	WriteError(rec, req, NewError(http.StatusForbidden, CodeAccessDenied, "bad credentials", "/bucket/key"))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/xml") {
		t.Fatalf("Content-Type = %q, want application/xml", ct)
	}
	var body ErrorResponse
	if err := xml.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal XML: %v\n%s", err, rec.Body.String())
	}
	if body.Code != CodeAccessDenied {
		t.Fatalf("Code = %q, want %q", body.Code, CodeAccessDenied)
	}
	if body.Message != "bad credentials" {
		t.Fatalf("Message = %q", body.Message)
	}
	if body.Resource != "/bucket/key" {
		t.Fatalf("Resource = %q", body.Resource)
	}
	if body.RequestID == "" {
		t.Fatal("RequestID should be populated")
	}
}

func TestWriteError_HEADHasNoBody(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodHead, "/bucket/key", nil)

	WriteError(rec, req, NewError(http.StatusNotFound, CodeNoSuchKey, "object not found", "/bucket/key"))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD response body length = %d, want 0", rec.Body.Len())
	}
}
