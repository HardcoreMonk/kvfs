// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package edge

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/HardcoreMonk/kvfs/internal/s3api"
)

const s3TestEmptyPayloadSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func TestS3Foundation_AuthenticatedOperationReturnsXMLNotImplemented(t *testing.T) {
	srv := &Server{
		S3Credentials: s3api.StaticCredentials{"AKIA_TEST": "test-secret"},
		S3Region:      "us-east-1",
	}
	req := httptest.NewRequest(http.MethodGet, "/photos?list-type=2", nil)
	req.Host = "kvfs.local"
	signS3TestRequest(t, req, "AKIA_TEST", "test-secret", "us-east-1")

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusNotImplemented, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/xml") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "<Code>NotImplemented</Code>") {
		t.Fatalf("body should contain S3 NotImplemented XML: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ListObjectsV2") {
		t.Fatalf("body should name classified operation: %s", rec.Body.String())
	}
}

func TestS3Foundation_MissingAuthReturnsS3XML(t *testing.T) {
	srv := &Server{S3Credentials: s3api.StaticCredentials{"AKIA_TEST": "test-secret"}}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if !strings.Contains(rec.Body.String(), "<Code>MissingSecurityHeader</Code>") {
		t.Fatalf("body should contain S3 MissingSecurityHeader XML: %s", rec.Body.String())
	}
}

func TestS3Foundation_NativeRoutesKeepPrecedence(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", rec.Code)
	}
}

func signS3TestRequest(t *testing.T, req *http.Request, accessKey, secret, region string) {
	t.Helper()
	amzDate := time.Now().UTC().Format("20060102T150405Z")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", s3TestEmptyPayloadSHA256)

	signedHeaders := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	scopeDate := amzDate[:len("20060102")]
	scope := scopeDate + "/" + region + "/s3/aws4_request"
	creq, err := canonicalS3TestRequest(req, signedHeaders, s3TestEmptyPayloadSHA256)
	if err != nil {
		t.Fatal(err)
	}
	sts := s3TestStringToSign(amzDate, scope, creq)
	key := s3TestSigningKey(secret, scopeDate, region, "s3")
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(sts))
	sig := hex.EncodeToString(mac.Sum(nil))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKey+"/"+scope+", SignedHeaders="+strings.Join(signedHeaders, ";")+", Signature="+sig)
}

func canonicalS3TestRequest(r *http.Request, signedHeaders []string, payloadHash string) (string, error) {
	headers, signed, err := canonicalS3TestHeaders(r, signedHeaders)
	if err != nil {
		return "", err
	}
	return strings.Join([]string{
		r.Method,
		canonicalS3TestURI(r.URL.EscapedPath()),
		canonicalS3TestQuery(r.URL.RawQuery),
		headers,
		signed,
		payloadHash,
	}, "\n"), nil
}

func canonicalS3TestURI(escapedPath string) string {
	if escapedPath == "" {
		return "/"
	}
	decoded, err := url.PathUnescape(escapedPath)
	if err != nil {
		return escapedPath
	}
	parts := strings.Split(decoded, "/")
	for i := range parts {
		parts[i] = sigv4TestEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

func canonicalS3TestQuery(rawQuery string) string {
	type pair struct{ key, value string }
	var pairs []pair
	if rawQuery == "" {
		return ""
	}
	for _, part := range strings.Split(rawQuery, "&") {
		if part == "" {
			continue
		}
		key, value, _ := strings.Cut(part, "=")
		pairs = append(pairs, pair{sigv4TestEscape(s3TestPathUnescape(key)), sigv4TestEscape(s3TestPathUnescape(value))})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].key == pairs[j].key {
			return pairs[i].value < pairs[j].value
		}
		return pairs[i].key < pairs[j].key
	})
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, p.key+"="+p.value)
	}
	return strings.Join(out, "&")
}

func canonicalS3TestHeaders(r *http.Request, signedHeaders []string) (string, string, error) {
	headers := make([]string, len(signedHeaders))
	copy(headers, signedHeaders)
	sort.Strings(headers)
	lines := make([]string, 0, len(headers))
	for _, name := range headers {
		value := ""
		if name == "host" {
			value = r.Host
		} else {
			value = canonicalS3TestHeaderValue(r.Header.Values(name))
		}
		lines = append(lines, name+":"+strings.Join(strings.Fields(value), " "))
	}
	return strings.Join(lines, "\n") + "\n", strings.Join(headers, ";"), nil
}

func canonicalS3TestHeaderValue(values []string) string {
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		normalized = append(normalized, strings.Join(strings.Fields(value), " "))
	}
	return strings.Join(normalized, ",")
}

func s3TestStringToSign(amzDate, scope, canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(sum[:]),
	}, "\n")
}

func s3TestSigningKey(secret, date, region, service string) []byte {
	kDate := s3TestHMACSHA256([]byte("AWS4"+secret), date)
	kRegion := s3TestHMACSHA256(kDate, region)
	kService := s3TestHMACSHA256(kRegion, service)
	return s3TestHMACSHA256(kService, "aws4_request")
}

func s3TestHMACSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(data))
	return mac.Sum(nil)
}

func s3TestPathUnescape(s string) string {
	decoded, err := url.PathUnescape(s)
	if err != nil {
		return s
	}
	return decoded
}

func sigv4TestEscape(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}
