// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package s3api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCanonicalRequest_SortsQueryAndHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/bucket/a%20b?z=last&a=first&z=again", nil)
	req.Host = "example.com"
	req.Header.Set("X-Amz-Date", "20260516T010203Z")
	req.Header.Set("X-Amz-Content-Sha256", emptyPayloadSHA256)

	got, err := canonicalRequest(req, []string{"host", "x-amz-content-sha256", "x-amz-date"}, emptyPayloadSHA256)
	if err != nil {
		t.Fatalf("canonicalRequest: %v", err)
	}
	want := strings.Join([]string{
		"GET",
		"/bucket/a%20b",
		"a=first&z=again&z=last",
		"host:example.com",
		"x-amz-content-sha256:" + emptyPayloadSHA256,
		"x-amz-date:20260516T010203Z",
		"",
		"host;x-amz-content-sha256;x-amz-date",
		emptyPayloadSHA256,
	}, "\n")
	if got != want {
		t.Fatalf("canonical request mismatch\nwant:\n%s\n\ngot:\n%s", want, got)
	}
}

func TestVerifyRequest_RoundTrip(t *testing.T) {
	now := time.Date(2026, 5, 16, 1, 2, 3, 0, time.UTC)
	req := httptest.NewRequest(http.MethodGet, "/bucket/key?list-type=2", nil)
	req.Host = "kvfs.local"
	req.Header.Set("X-Amz-Date", "20260516T010203Z")
	req.Header.Set("X-Amz-Content-Sha256", emptyPayloadSHA256)
	signTestRequest(t, req, "AKIA_TEST", "test-secret", "us-east-1", "s3")

	res, err := VerifyRequest(req, StaticCredentials{"AKIA_TEST": "test-secret"}, now)
	if err != nil {
		t.Fatalf("VerifyRequest: %v", err)
	}
	if res.AccessKey != "AKIA_TEST" || res.Region != "us-east-1" || res.Service != "s3" {
		t.Fatalf("AuthResult = %+v", res)
	}
}

func TestVerifyRequest_TamperFails(t *testing.T) {
	now := time.Date(2026, 5, 16, 1, 2, 3, 0, time.UTC)
	req := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
	req.Host = "kvfs.local"
	req.Header.Set("X-Amz-Date", "20260516T010203Z")
	req.Header.Set("X-Amz-Content-Sha256", emptyPayloadSHA256)
	signTestRequest(t, req, "AKIA_TEST", "test-secret", "us-east-1", "s3")
	req.URL.RawQuery = "tampered=1"

	err := verifyOnly(req, StaticCredentials{"AKIA_TEST": "test-secret"}, now)
	if err == nil {
		t.Fatal("tampered query should fail")
	}
	if s3err, ok := err.(*Error); !ok || s3err.Code != CodeSignatureDoesNotMatch {
		t.Fatalf("err = %T %[1]v, want SignatureDoesNotMatch", err)
	}
}

func TestVerifyRequest_RejectsStaleDate(t *testing.T) {
	now := time.Date(2026, 5, 16, 2, 0, 0, 0, time.UTC)
	req := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
	req.Host = "kvfs.local"
	req.Header.Set("X-Amz-Date", "20260516T010203Z")
	req.Header.Set("X-Amz-Content-Sha256", emptyPayloadSHA256)
	signTestRequest(t, req, "AKIA_TEST", "test-secret", "us-east-1", "s3")

	err := verifyOnly(req, StaticCredentials{"AKIA_TEST": "test-secret"}, now)
	if err == nil {
		t.Fatal("stale request should fail")
	}
	if s3err, ok := err.(*Error); !ok || s3err.Code != CodeAccessDenied {
		t.Fatalf("err = %T %[1]v, want AccessDenied", err)
	}
}

func signTestRequest(t *testing.T, req *http.Request, accessKey, secret, region, service string) {
	t.Helper()
	signedHeaders := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	creq, err := canonicalRequest(req, signedHeaders, req.Header.Get("X-Amz-Content-Sha256"))
	if err != nil {
		t.Fatal(err)
	}
	scope := "20260516/" + region + "/" + service + "/aws4_request"
	sts := stringToSign("20260516T010203Z", scope, creq)
	key := signingKey(secret, "20260516", region, service)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(sts))
	sig := hex.EncodeToString(mac.Sum(nil))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKey+"/"+scope+", SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature="+sig)
}
