# S3 Compatibility Foundation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the P9-02 S3 protocol foundation: SigV4 verification, S3 XML errors, operation classification, edge route wiring, and an `aws`/`mc` smoke-suite skeleton without implementing bucket/object semantics yet.

**Architecture:** `internal/s3api` owns protocol concerns: SigV4 canonicalization, XML response shape, and S3 route classification. `internal/edge` adds a thin S3 front door that authenticates and classifies S3 requests, then returns S3-shaped `NotImplemented` until P9-03 maps those operations to store/coord behavior. Native `/v1/*`, `/healthz`, and `/metrics` routes remain unchanged and more specific than the S3 catch-all route patterns.

**Tech Stack:** Go 1.26 standard library (`crypto/hmac`, `crypto/sha256`, `encoding/xml`, `net/http`), existing edge `http.ServeMux`, shell smoke script, existing docs drift checks.

---

## Scope Check

This plan implements **P9-02 S3 compatibility foundation only**.

In scope:

- `internal/s3api` package.
- Header-based SigV4 verification for ordinary AWS SDK/CLI requests.
- S3 XML error writer.
- S3 route classifier for the ADR-064 operation list.
- Edge route wiring that returns S3-shaped `NotImplemented` for classified
  operations after successful SigV4 verification.
- `EDGE_S3_ACCESS_KEY`, `EDGE_S3_SECRET_KEY`, and `EDGE_S3_REGION` wiring.
- Smoke-suite skeleton for `aws` and `mc`.
- Documentation updates for the new foundation and env vars.

Out of scope:

- Successful `CreateBucket`, `ListBuckets`, `DeleteBucket`.
- Successful `PutObject`, `GetObject`, `HeadObject`, `DeleteObject`,
  `ListObjectsV2`.
- Multipart state or part storage.
- Production profile enforcement.
- Admin-plane auth.

Those are P9-03, P9-04, and P9-05.

## File Structure

- Create `internal/s3api/errors.go`: S3 error model and XML writer.
- Create `internal/s3api/errors_test.go`: XML error shape tests.
- Create `internal/s3api/operation.go`: S3 operation enum and request classifier.
- Create `internal/s3api/operation_test.go`: path/query/method classification tests.
- Create `internal/s3api/sigv4.go`: SigV4 header parser, canonical request builder, and verifier.
- Create `internal/s3api/sigv4_test.go`: canonicalization and signature tests.
- Modify `internal/edge/edge.go`: add S3 credential fields, S3 route patterns, and foundation handler.
- Create `internal/edge/s3_test.go`: verify S3 foundation responses and native route precedence.
- Modify `cmd/kvfs-edge/main.go`: wire `EDGE_S3_ACCESS_KEY`, `EDGE_S3_SECRET_KEY`, `EDGE_S3_REGION`.
- Create `scripts/smoke-s3-compat.sh`: foundation-mode AWS CLI and MinIO client smoke skeleton.
- Modify `README.md`: add S3 foundation status and env vars.
- Modify `docs/GUIDE.md`: add S3 foundation walkthrough note and env vars.
- Modify `docs/guide.html`: mirror `docs/GUIDE.md`.
- Modify `docs/ARCHITECTURE.md`: add `internal/s3api` package boundary.
- Modify `docs/FOLLOWUP.md`: mark P9-02 complete after implementation.
- Create `docs/operations/2026-05-16-s3-compatibility-foundation-handoff.md`: release-to-operate handoff for this foundation slice.

## Inputs

- Approved production MVP design:
  `docs/superpowers/specs/2026-05-16-production-mvp-replacement-design.md`
- P9-01 accepted ADR:
  `docs/adr/ADR-064-production-mvp-profile.md`
- P9-02 grill-me record:
  `docs/superpowers/grill-me/2026-05-16-s3-compatibility-foundation.md`

### Task 1: S3 XML Error Foundation

**Files:**
- Create: `internal/s3api/errors.go`
- Create: `internal/s3api/errors_test.go`

- [ ] **Step 1: Write failing XML error tests**

Create `internal/s3api/errors_test.go`:

```go
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
```

- [ ] **Step 2: Run the test and verify it fails**

Run:

```bash
go test ./internal/s3api
```

Expected: FAIL because `internal/s3api` does not exist.

- [ ] **Step 3: Implement S3 error XML writer**

Create `internal/s3api/errors.go`:

```go
// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Package s3api owns the S3-compatible protocol surface for kvfs-edge.
// It intentionally contains protocol translation only: SigV4, XML shapes,
// route classification, and S3 error codes. Storage, placement, and repair
// remain in edge, coord, store, and DN packages.
package s3api

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

const (
	CodeAccessDenied                 = "AccessDenied"
	CodeAuthorizationHeaderMalformed = "AuthorizationHeaderMalformed"
	CodeInvalidArgument              = "InvalidArgument"
	CodeInvalidRequest               = "InvalidRequest"
	CodeMissingSecurityHeader        = "MissingSecurityHeader"
	CodeNoSuchBucket                 = "NoSuchBucket"
	CodeNoSuchKey                    = "NoSuchKey"
	CodeNotImplemented               = "NotImplemented"
	CodeSignatureDoesNotMatch        = "SignatureDoesNotMatch"
)

var requestSeq uint64

type Error struct {
	Status   int
	Code     string
	Message  string
	Resource string
}

func NewError(status int, code, message, resource string) *Error {
	if status == 0 {
		status = http.StatusInternalServerError
	}
	return &Error{Status: status, Code: code, Message: message, Resource: resource}
}

func (e *Error) Error() string {
	return fmt.Sprintf("s3api: %s: %s", e.Code, e.Message)
}

type ErrorResponse struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource,omitempty"`
	RequestID string   `xml:"RequestId"`
}

func WriteError(w http.ResponseWriter, r *http.Request, s3err *Error) {
	if s3err == nil {
		s3err = NewError(http.StatusInternalServerError, CodeInvalidRequest, "unknown S3 error", "")
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("X-Amz-Request-Id", nextRequestID())
	w.WriteHeader(s3err.Status)
	if r != nil && r.Method == http.MethodHead {
		return
	}
	resp := ErrorResponse{
		Code:      s3err.Code,
		Message:   s3err.Message,
		Resource:  s3err.Resource,
		RequestID: w.Header().Get("X-Amz-Request-Id"),
	}
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(resp)
}

func nextRequestID() string {
	n := atomic.AddUint64(&requestSeq, 1)
	return strconv.FormatInt(time.Now().UTC().UnixNano(), 36) + "-" + strconv.FormatUint(n, 36)
}
```

- [ ] **Step 4: Run the tests and verify they pass**

Run:

```bash
go test ./internal/s3api
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/s3api/errors.go internal/s3api/errors_test.go
git commit -m "feat: add s3 xml error foundation"
```

### Task 2: S3 Operation Classifier

**Files:**
- Create: `internal/s3api/operation.go`
- Create: `internal/s3api/operation_test.go`

- [ ] **Step 1: Write failing classifier tests**

Create `internal/s3api/operation_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package s3api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClassifyOperation(t *testing.T) {
	tests := []struct {
		name   string
		method string
		target string
		op     Operation
		bucket string
		key    string
	}{
		{"list buckets", http.MethodGet, "/", OperationListBuckets, "", ""},
		{"create bucket", http.MethodPut, "/photos", OperationCreateBucket, "photos", ""},
		{"delete bucket", http.MethodDelete, "/photos", OperationDeleteBucket, "photos", ""},
		{"list objects v2", http.MethodGet, "/photos?list-type=2&prefix=raw/", OperationListObjectsV2, "photos", ""},
		{"put object", http.MethodPut, "/photos/raw/a.jpg", OperationPutObject, "photos", "raw/a.jpg"},
		{"get object", http.MethodGet, "/photos/raw/a.jpg", OperationGetObject, "photos", "raw/a.jpg"},
		{"head object", http.MethodHead, "/photos/raw/a.jpg", OperationHeadObject, "photos", "raw/a.jpg"},
		{"delete object", http.MethodDelete, "/photos/raw/a.jpg", OperationDeleteObject, "photos", "raw/a.jpg"},
		{"create multipart upload", http.MethodPost, "/photos/raw/a.jpg?uploads", OperationCreateMultipartUpload, "photos", "raw/a.jpg"},
		{"upload part", http.MethodPut, "/photos/raw/a.jpg?partNumber=1&uploadId=u1", OperationUploadPart, "photos", "raw/a.jpg"},
		{"list parts", http.MethodGet, "/photos/raw/a.jpg?uploadId=u1", OperationListParts, "photos", "raw/a.jpg"},
		{"complete multipart", http.MethodPost, "/photos/raw/a.jpg?uploadId=u1", OperationCompleteMultipartUpload, "photos", "raw/a.jpg"},
		{"abort multipart", http.MethodDelete, "/photos/raw/a.jpg?uploadId=u1", OperationAbortMultipartUpload, "photos", "raw/a.jpg"},
		{"unsupported list v1", http.MethodGet, "/photos", OperationUnsupported, "photos", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.target, nil)
			got := Classify(req)
			if got.Operation != tt.op {
				t.Fatalf("Operation = %s, want %s", got.Operation, tt.op)
			}
			if got.Bucket != tt.bucket || got.Key != tt.key {
				t.Fatalf("Bucket/Key = %q/%q, want %q/%q", got.Bucket, got.Key, tt.bucket, tt.key)
			}
		})
	}
}

func TestOperationString(t *testing.T) {
	if OperationPutObject.String() != "PutObject" {
		t.Fatalf("OperationPutObject.String() = %q", OperationPutObject.String())
	}
}
```

- [ ] **Step 2: Run the test and verify it fails**

Run:

```bash
go test ./internal/s3api
```

Expected: FAIL because `Operation`, `Classify`, and related symbols are not defined.

- [ ] **Step 3: Implement the classifier**

Create `internal/s3api/operation.go`:

```go
// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package s3api

import (
	"net/http"
	"net/url"
	"strings"
)

type Operation string

const (
	OperationUnsupported           Operation = "Unsupported"
	OperationListBuckets           Operation = "ListBuckets"
	OperationCreateBucket          Operation = "CreateBucket"
	OperationDeleteBucket          Operation = "DeleteBucket"
	OperationPutObject             Operation = "PutObject"
	OperationGetObject             Operation = "GetObject"
	OperationHeadObject            Operation = "HeadObject"
	OperationDeleteObject          Operation = "DeleteObject"
	OperationListObjectsV2         Operation = "ListObjectsV2"
	OperationCreateMultipartUpload Operation = "CreateMultipartUpload"
	OperationUploadPart            Operation = "UploadPart"
	OperationListParts             Operation = "ListParts"
	OperationCompleteMultipartUpload Operation = "CompleteMultipartUpload"
	OperationAbortMultipartUpload  Operation = "AbortMultipartUpload"
)

func (op Operation) String() string { return string(op) }

type RequestInfo struct {
	Operation Operation
	Bucket    string
	Key       string
	Resource  string
}

func Classify(r *http.Request) RequestInfo {
	bucket, key := splitS3Path(r.URL.Path)
	info := RequestInfo{
		Operation: OperationUnsupported,
		Bucket:    bucket,
		Key:       key,
		Resource:  r.URL.EscapedPath(),
	}
	if info.Resource == "" {
		info.Resource = "/"
	}
	q := r.URL.Query()

	if bucket == "" && key == "" && r.Method == http.MethodGet {
		info.Operation = OperationListBuckets
		return info
	}
	if bucket == "" {
		return info
	}
	if key == "" {
		switch r.Method {
		case http.MethodPut:
			info.Operation = OperationCreateBucket
		case http.MethodDelete:
			info.Operation = OperationDeleteBucket
		case http.MethodGet:
			if q.Get("list-type") == "2" {
				info.Operation = OperationListObjectsV2
			}
		}
		return info
	}

	switch r.Method {
	case http.MethodPut:
		if q.Get("uploadId") != "" && q.Get("partNumber") != "" {
			info.Operation = OperationUploadPart
		} else {
			info.Operation = OperationPutObject
		}
	case http.MethodGet:
		if q.Get("uploadId") != "" {
			info.Operation = OperationListParts
		} else {
			info.Operation = OperationGetObject
		}
	case http.MethodHead:
		info.Operation = OperationHeadObject
	case http.MethodDelete:
		if q.Get("uploadId") != "" {
			info.Operation = OperationAbortMultipartUpload
		} else {
			info.Operation = OperationDeleteObject
		}
	case http.MethodPost:
		if _, ok := q["uploads"]; ok {
			info.Operation = OperationCreateMultipartUpload
		} else if q.Get("uploadId") != "" {
			info.Operation = OperationCompleteMultipartUpload
		}
	}
	return info
}

func splitS3Path(rawPath string) (bucket, key string) {
	trimmed := strings.TrimPrefix(rawPath, "/")
	trimmed = strings.TrimSuffix(trimmed, "/")
	if trimmed == "" {
		return "", ""
	}
	head, tail, hasTail := strings.Cut(trimmed, "/")
	bucket, _ = url.PathUnescape(head)
	if !hasTail {
		return bucket, ""
	}
	key, _ = url.PathUnescape(tail)
	return bucket, key
}
```

- [ ] **Step 4: Run tests and format**

Run:

```bash
gofmt -w internal/s3api/operation.go internal/s3api/operation_test.go
go test ./internal/s3api
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/s3api/operation.go internal/s3api/operation_test.go
git commit -m "feat: classify s3 operations"
```

### Task 3: SigV4 Header Verification

**Files:**
- Create: `internal/s3api/sigv4.go`
- Create: `internal/s3api/sigv4_test.go`

- [ ] **Step 1: Write failing SigV4 tests**

Create `internal/s3api/sigv4_test.go`:

```go
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
	req := httptest.NewRequest(http.MethodGet, "/bucket/a b?z=last&a=first&z=again", nil)
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
```

- [ ] **Step 2: Run the test and verify it fails**

Run:

```bash
go test ./internal/s3api
```

Expected: FAIL because SigV4 symbols are not defined.

- [ ] **Step 3: Implement SigV4 verifier**

Create `internal/s3api/sigv4.go`:

```go
// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package s3api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	algorithmAWS4HMACSHA256 = "AWS4-HMAC-SHA256"
	emptyPayloadSHA256      = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	maxClockSkew            = 15 * time.Minute
)

type Credentials struct {
	AccessKey string
	SecretKey string
}

type CredentialProvider interface {
	LookupAccessKey(ctx context.Context, accessKey string) (Credentials, bool)
}

type StaticCredentials map[string]string

func (s StaticCredentials) LookupAccessKey(_ context.Context, accessKey string) (Credentials, bool) {
	secret, ok := s[accessKey]
	if !ok {
		return Credentials{}, false
	}
	return Credentials{AccessKey: accessKey, SecretKey: secret}, true
}

type AuthResult struct {
	AccessKey     string
	Region        string
	Service       string
	SignedHeaders []string
	RequestTime   time.Time
}

func VerifyRequest(r *http.Request, provider CredentialProvider, now time.Time) (*AuthResult, error) {
	if provider == nil {
		return nil, NewError(http.StatusForbidden, CodeAccessDenied, "S3 credentials are not configured", r.URL.Path)
	}
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return nil, NewError(http.StatusForbidden, CodeMissingSecurityHeader, "missing Authorization header", r.URL.Path)
	}
	parsed, err := parseAuthorization(auth)
	if err != nil {
		return nil, err
	}
	if parsed.algorithm != algorithmAWS4HMACSHA256 {
		return nil, NewError(http.StatusBadRequest, CodeAuthorizationHeaderMalformed, "unsupported authorization algorithm", r.URL.Path)
	}
	if len(parsed.scope) != 5 || parsed.scope[4] != "aws4_request" {
		return nil, NewError(http.StatusBadRequest, CodeAuthorizationHeaderMalformed, "credential scope must be date/region/service/aws4_request", r.URL.Path)
	}
	creds, ok := provider.LookupAccessKey(r.Context(), parsed.accessKey)
	if !ok {
		return nil, NewError(http.StatusForbidden, CodeAccessDenied, "unknown access key", r.URL.Path)
	}
	amzDate := r.Header.Get("X-Amz-Date")
	if amzDate == "" {
		return nil, NewError(http.StatusBadRequest, CodeMissingSecurityHeader, "missing X-Amz-Date header", r.URL.Path)
	}
	reqTime, err := time.Parse("20060102T150405Z", amzDate)
	if err != nil {
		return nil, NewError(http.StatusBadRequest, CodeAuthorizationHeaderMalformed, "invalid X-Amz-Date header", r.URL.Path)
	}
	if reqTime.Before(now.Add(-maxClockSkew)) || reqTime.After(now.Add(maxClockSkew)) {
		return nil, NewError(http.StatusForbidden, CodeAccessDenied, "request time is outside the allowed clock skew", r.URL.Path)
	}
	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	if payloadHash == "" {
		return nil, NewError(http.StatusBadRequest, CodeMissingSecurityHeader, "missing X-Amz-Content-Sha256 header", r.URL.Path)
	}
	creq, err := canonicalRequest(r, parsed.signedHeaders, payloadHash)
	if err != nil {
		return nil, err
	}
	scope := strings.Join(parsed.scope, "/")
	sts := stringToSign(amzDate, scope, creq)
	key := signingKey(creds.SecretKey, parsed.scope[0], parsed.scope[1], parsed.scope[2])
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(sts))
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(parsed.signature)) {
		return nil, NewError(http.StatusForbidden, CodeSignatureDoesNotMatch, "signature does not match", r.URL.Path)
	}
	return &AuthResult{
		AccessKey:     parsed.accessKey,
		Region:        parsed.scope[1],
		Service:       parsed.scope[2],
		SignedHeaders: parsed.signedHeaders,
		RequestTime:   reqTime,
	}, nil
}

func verifyOnly(r *http.Request, provider CredentialProvider, now time.Time) error {
	_, err := VerifyRequest(r, provider, now)
	return err
}

type parsedAuthorization struct {
	algorithm     string
	accessKey     string
	scope         []string
	signedHeaders []string
	signature     string
}

func parseAuthorization(auth string) (parsedAuthorization, error) {
	algo, rest, ok := strings.Cut(strings.TrimSpace(auth), " ")
	if !ok {
		return parsedAuthorization{}, NewError(http.StatusBadRequest, CodeAuthorizationHeaderMalformed, "invalid Authorization header", "")
	}
	fields := map[string]string{}
	for _, part := range strings.Split(rest, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return parsedAuthorization{}, NewError(http.StatusBadRequest, CodeAuthorizationHeaderMalformed, "invalid Authorization parameter", "")
		}
		fields[k] = v
	}
	credential := fields["Credential"]
	signedHeaders := fields["SignedHeaders"]
	signature := fields["Signature"]
	if credential == "" || signedHeaders == "" || signature == "" {
		return parsedAuthorization{}, NewError(http.StatusBadRequest, CodeAuthorizationHeaderMalformed, "Authorization header requires Credential, SignedHeaders, and Signature", "")
	}
	accessKey, scopeRaw, ok := strings.Cut(credential, "/")
	if !ok || accessKey == "" {
		return parsedAuthorization{}, NewError(http.StatusBadRequest, CodeAuthorizationHeaderMalformed, "invalid Credential parameter", "")
	}
	scope := strings.Split(scopeRaw, "/")
	headers := strings.Split(strings.ToLower(signedHeaders), ";")
	sort.Strings(headers)
	return parsedAuthorization{algorithm: algo, accessKey: accessKey, scope: scope, signedHeaders: headers, signature: signature}, nil
}

func canonicalRequest(r *http.Request, signedHeaders []string, payloadHash string) (string, error) {
	headers, signed, err := canonicalHeaders(r, signedHeaders)
	if err != nil {
		return "", err
	}
	return strings.Join([]string{
		r.Method,
		canonicalURI(r.URL.EscapedPath()),
		canonicalQuery(r.URL.Query()),
		headers,
		signed,
		payloadHash,
	}, "\n"), nil
}

func canonicalURI(escapedPath string) string {
	if escapedPath == "" {
		return "/"
	}
	decoded, err := url.PathUnescape(escapedPath)
	if err != nil {
		return escapedPath
	}
	parts := strings.Split(decoded, "/")
	for i := range parts {
		parts[i] = sigv4Escape(parts[i])
	}
	return strings.Join(parts, "/")
}

func canonicalQuery(q url.Values) string {
	type pair struct{ key, value string }
	var pairs []pair
	for key, values := range q {
		for _, value := range values {
			pairs = append(pairs, pair{sigv4Escape(key), sigv4Escape(value)})
		}
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

func canonicalHeaders(r *http.Request, signedHeaders []string) (string, string, error) {
	if len(signedHeaders) == 0 {
		return "", "", NewError(http.StatusBadRequest, CodeAuthorizationHeaderMalformed, "SignedHeaders must not be empty", r.URL.Path)
	}
	headers := make([]string, len(signedHeaders))
	copy(headers, signedHeaders)
	sort.Strings(headers)
	lines := make([]string, 0, len(headers)+1)
	for _, name := range headers {
		value := ""
		if name == "host" {
			value = r.Host
		} else {
			value = r.Header.Get(name)
		}
		if value == "" {
			return "", "", NewError(http.StatusBadRequest, CodeMissingSecurityHeader, "missing signed header "+name, r.URL.Path)
		}
		lines = append(lines, name+":"+strings.Join(strings.Fields(value), " "))
	}
	return strings.Join(lines, "\n") + "\n", strings.Join(headers, ";"), nil
}

func sigv4Escape(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

func stringToSign(amzDate, scope, canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return strings.Join([]string{
		algorithmAWS4HMACSHA256,
		amzDate,
		scope,
		hex.EncodeToString(sum[:]),
	}, "\n")
}

func signingKey(secret, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), date)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	return hmacSHA256(kService, "aws4_request")
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(data))
	return mac.Sum(nil)
}
```

- [ ] **Step 4: Run tests and format**

Run:

```bash
gofmt -w internal/s3api/sigv4.go internal/s3api/sigv4_test.go
go test ./internal/s3api
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/s3api/sigv4.go internal/s3api/sigv4_test.go
git commit -m "feat: verify s3 sigv4 headers"
```

### Task 4: Edge S3 Foundation Route Wiring

**Files:**
- Modify: `internal/edge/edge.go`
- Create: `internal/edge/s3_test.go`

- [ ] **Step 1: Write failing edge S3 tests**

Create `internal/edge/s3_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package edge

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/HardcoreMonk/kvfs/internal/s3api"
)

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
	amzDate := "20260516T010203Z"
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonical := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		"list-type=2",
		"host:" + req.Host,
		"x-amz-content-sha256:" + req.Header.Get("X-Amz-Content-Sha256"),
		"x-amz-date:" + amzDate,
		"",
		signedHeaders,
		req.Header.Get("X-Amz-Content-Sha256"),
	}, "\n")
	if req.URL.RawQuery == "" {
		canonical = strings.Replace(canonical, "list-type=2\n", "\n", 1)
	}
	scope := "20260516/" + region + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(canonical))
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	key := s3TestSigningKey(secret, "20260516", region, "s3")
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(stringToSign))
	sig := hex.EncodeToString(mac.Sum(nil))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKey+"/"+scope+", SignedHeaders="+signedHeaders+", Signature="+sig)
}

func s3TestSigningKey(secret, date, region, service string) []byte {
	kDate := s3TestHMAC([]byte("AWS4"+secret), date)
	kRegion := s3TestHMAC(kDate, region)
	kService := s3TestHMAC(kRegion, service)
	return s3TestHMAC(kService, "aws4_request")
}

func s3TestHMAC(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(data))
	return mac.Sum(nil)
}
```

- [ ] **Step 2: Run the edge tests and verify they fail**

Run:

```bash
go test ./internal/edge -run 'TestS3Foundation'
```

Expected: FAIL because `Server.S3Credentials` and `Server.S3Region` are not defined.

- [ ] **Step 3: Modify edge imports**

In `internal/edge/edge.go`, add the S3 package import:

```go
	"github.com/HardcoreMonk/kvfs/internal/s3api"
```

- [ ] **Step 4: Add S3 fields to `Server`**

In `internal/edge/edge.go`, inside `type Server struct`, after `Signer *urlkey.Signer`, add:

```go
	// S3Credentials enables the P9 S3-compatible front door. nil means the
	// S3 route surface exists but fails closed with S3-shaped AccessDenied.
	S3Credentials s3api.CredentialProvider

	// S3Region is the SigV4 region advertised/accepted for the first MVP.
	// Empty falls back to us-east-1.
	S3Region string
```

- [ ] **Step 5: Add the S3 foundation handler**

In `internal/edge/edge.go`, before `Routes()`, add:

```go
func (s *Server) s3Region() string {
	if s.S3Region == "" {
		return "us-east-1"
	}
	return s.S3Region
}

func (s *Server) handleS3Foundation(w http.ResponseWriter, r *http.Request) {
	info := s3api.Classify(r)
	if info.Operation == s3api.OperationUnsupported {
		s3api.WriteError(w, r, s3api.NewError(
			http.StatusNotImplemented,
			s3api.CodeNotImplemented,
			"unsupported S3 operation",
			info.Resource,
		))
		return
	}
	auth, err := s3api.VerifyRequest(r, s.S3Credentials, time.Now())
	if err != nil {
		if s3err, ok := err.(*s3api.Error); ok {
			s3api.WriteError(w, r, s3err)
			return
		}
		s3api.WriteError(w, r, s3api.NewError(http.StatusForbidden, s3api.CodeAccessDenied, err.Error(), info.Resource))
		return
	}
	if auth.Service != "s3" || auth.Region != s.s3Region() {
		s3api.WriteError(w, r, s3api.NewError(
			http.StatusBadRequest,
			s3api.CodeAuthorizationHeaderMalformed,
			"credential scope must use region "+s.s3Region()+" and service s3",
			info.Resource,
		))
		return
	}
	s3api.WriteError(w, r, s3api.NewError(
		http.StatusNotImplemented,
		s3api.CodeNotImplemented,
		info.Operation.String()+" is not implemented in P9-02",
		info.Resource,
	))
}
```

- [ ] **Step 6: Register S3 route patterns**

In `internal/edge/edge.go`, modify `Routes()` so the S3 routes are registered
after existing native/admin/health routes:

```go
	mux.HandleFunc("GET /healthz", s.handleHealth)

	mux.HandleFunc("GET /", s.handleS3Foundation)
	mux.HandleFunc("PUT /{bucket}", s.handleS3Foundation)
	mux.HandleFunc("DELETE /{bucket}", s.handleS3Foundation)
	mux.HandleFunc("GET /{bucket}", s.handleS3Foundation)
	mux.HandleFunc("PUT /{bucket}/{key...}", s.handleS3Foundation)
	mux.HandleFunc("GET /{bucket}/{key...}", s.handleS3Foundation)
	mux.HandleFunc("HEAD /{bucket}/{key...}", s.handleS3Foundation)
	mux.HandleFunc("DELETE /{bucket}/{key...}", s.handleS3Foundation)
	mux.HandleFunc("POST /{bucket}/{key...}", s.handleS3Foundation)
	return logRequests(mux)
```

Keep the existing native route registrations above this block unchanged.

- [ ] **Step 7: Run tests and fix the test signer if canonical query differs**

Run:

```bash
gofmt -w internal/edge/edge.go internal/edge/s3_test.go
go test ./internal/edge -run 'TestS3Foundation'
```

Expected: PASS. If the test signer fails only because canonical query ordering
differs, change the test helper canonical query string to match the canonical
query rules in `internal/s3api/sigv4.go` and rerun.

- [ ] **Step 8: Run the focused package tests**

Run:

```bash
go test ./internal/s3api ./internal/edge
```

Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/edge/edge.go internal/edge/s3_test.go
git commit -m "feat: wire s3 foundation routes"
```

### Task 5: Edge S3 Credential Env Wiring

**Files:**
- Modify: `cmd/kvfs-edge/main.go`

- [ ] **Step 1: Add the failing expectation manually**

Run:

```bash
go test ./cmd/kvfs-edge
```

Expected before changes: PASS or `? github.com/HardcoreMonk/kvfs/cmd/kvfs-edge [no test files]`.
This command establishes the package baseline before wiring.

- [ ] **Step 2: Modify imports**

In `cmd/kvfs-edge/main.go`, add:

```go
	"github.com/HardcoreMonk/kvfs/internal/s3api"
```

- [ ] **Step 3: Add flags**

In the existing `var (` flag block in `cmd/kvfs-edge/main.go`, after
`flagSecret`, add:

```go
		flagS3AccessKey = flag.String("s3-access-key", envOr("EDGE_S3_ACCESS_KEY", ""), "S3 SigV4 access key for the P9 S3 front door")
		flagS3SecretKey = flag.String("s3-secret-key", envOr("EDGE_S3_SECRET_KEY", ""), "S3 SigV4 secret key for the P9 S3 front door")
		flagS3Region    = flag.String("s3-region", envOr("EDGE_S3_REGION", "us-east-1"), "S3 SigV4 region for the P9 S3 front door")
```

- [ ] **Step 4: Build the credential provider**

After the UrlKey signer setup block and before chunker configuration, add:

```go
	var s3Creds s3api.CredentialProvider
	if (*flagS3AccessKey == "") != (*flagS3SecretKey == "") {
		fatal("EDGE_S3_ACCESS_KEY and EDGE_S3_SECRET_KEY must be set together")
	}
	if *flagS3AccessKey != "" {
		s3Creds = s3api.StaticCredentials{*flagS3AccessKey: *flagS3SecretKey}
		log.Info("s3 front door foundation enabled", "region", *flagS3Region)
	}
```

- [ ] **Step 5: Pass the provider into `edge.Server`**

In the `srv := &edge.Server{...}` literal, add:

```go
		S3Credentials:        s3Creds,
		S3Region:             *flagS3Region,
```

- [ ] **Step 6: Build and test**

Run:

```bash
gofmt -w cmd/kvfs-edge/main.go
go test ./cmd/kvfs-edge ./internal/s3api ./internal/edge
go build ./cmd/kvfs-edge
```

Expected: all PASS/build succeeds.

- [ ] **Step 7: Commit**

```bash
git add cmd/kvfs-edge/main.go
git commit -m "feat: wire edge s3 credentials"
```

### Task 6: S3 Compatibility Smoke Skeleton

**Files:**
- Create: `scripts/smoke-s3-compat.sh`

- [ ] **Step 1: Create the smoke script**

Create `scripts/smoke-s3-compat.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail

endpoint="${KVFS_S3_ENDPOINT:-http://127.0.0.1:8000}"
region="${KVFS_S3_REGION:-us-east-1}"
bucket="${KVFS_S3_BUCKET:-kvfs-smoke}"
expect_foundation="${KVFS_S3_EXPECT_FOUNDATION:-1}"

if [[ -z "${AWS_ACCESS_KEY_ID:-}" || -z "${AWS_SECRET_ACCESS_KEY:-}" ]]; then
  echo "SKIP: set AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY for SigV4 smoke" >&2
  exit 0
fi

if ! command -v aws >/dev/null 2>&1; then
  echo "SKIP: aws CLI not installed" >&2
  exit 0
fi

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

echo "[aws] list-buckets against ${endpoint}"
set +e
aws --endpoint-url "$endpoint" --region "$region" s3api list-buckets >"$tmpdir/aws.out" 2>"$tmpdir/aws.err"
aws_status=$?
set -e

if [[ "$expect_foundation" == "1" ]]; then
  if [[ "$aws_status" -eq 0 ]]; then
    echo "FAIL: foundation mode expected aws list-buckets to fail with NotImplemented" >&2
    exit 1
  fi
  if ! grep -qi "NotImplemented" "$tmpdir/aws.err"; then
    echo "FAIL: expected aws stderr to mention NotImplemented" >&2
    cat "$tmpdir/aws.err" >&2
    exit 1
  fi
  echo "PASS: aws CLI reached S3 foundation and received NotImplemented"
else
  if [[ "$aws_status" -ne 0 ]]; then
    cat "$tmpdir/aws.err" >&2
    exit "$aws_status"
  fi
  echo "PASS: aws list-buckets succeeded"
fi

if ! command -v mc >/dev/null 2>&1; then
  echo "SKIP: mc not installed" >&2
  exit 0
fi

echo "[mc] alias + ls against ${endpoint}"
mc alias set kvfs-smoke "$endpoint" "$AWS_ACCESS_KEY_ID" "$AWS_SECRET_ACCESS_KEY" >/dev/null
set +e
mc ls kvfs-smoke/"$bucket" >"$tmpdir/mc.out" 2>"$tmpdir/mc.err"
mc_status=$?
set -e

if [[ "$expect_foundation" == "1" ]]; then
  if [[ "$mc_status" -eq 0 ]]; then
    echo "FAIL: foundation mode expected mc ls to fail before P9-03" >&2
    exit 1
  fi
  echo "PASS: mc can target the endpoint; successful bucket behavior starts in P9-03"
else
  if [[ "$mc_status" -ne 0 ]]; then
    cat "$tmpdir/mc.err" >&2
    exit "$mc_status"
  fi
  echo "PASS: mc ls succeeded"
fi
```

- [ ] **Step 2: Make it executable and run syntax checks**

Run:

```bash
chmod +x scripts/smoke-s3-compat.sh
bash -n scripts/smoke-s3-compat.sh
./scripts/smoke-s3-compat.sh
```

Expected: `bash -n` passes. The script exits 0 with `SKIP` if credentials or
tools are not present.

- [ ] **Step 3: Commit**

```bash
git add scripts/smoke-s3-compat.sh
git commit -m "test: add s3 compatibility smoke skeleton"
```

### Task 7: Documentation and Follow-Up Update

**Files:**
- Modify: `README.md`
- Modify: `docs/GUIDE.md`
- Modify: `docs/guide.html`
- Modify: `docs/ARCHITECTURE.md`
- Modify: `docs/FOLLOWUP.md`
- Create: `docs/operations/2026-05-16-s3-compatibility-foundation-handoff.md`

- [ ] **Step 1: Update README env table**

In `README.md`, add these rows near the edge auth/env rows:

```markdown
| `EDGE_S3_ACCESS_KEY` / `EDGE_S3_SECRET_KEY` | (off) | — | P9-02 S3 SigV4 foundation credentials. Set together to enable authenticated S3 front-door probes. |
| `EDGE_S3_REGION` | `us-east-1` | — | P9-02 S3 SigV4 credential-scope region. |
```

- [ ] **Step 2: Update README P9 section**

In `README.md` P9 section, add:

```markdown
P9-02 adds the S3 protocol foundation only: SigV4 header verification,
S3-shaped XML errors, operation classification, route mapping, and the
`scripts/smoke-s3-compat.sh` skeleton. Bucket/object success paths start in
P9-03, so P9-02 may intentionally return S3 `NotImplemented` for recognized
operations after authentication succeeds.
```

- [ ] **Step 3: Update GUIDE env table and narrative**

In `docs/GUIDE.md`, add the same env rows:

```markdown
| `EDGE_S3_ACCESS_KEY` / `EDGE_S3_SECRET_KEY` | (off) | P9-02 S3 SigV4 foundation credentials. Set together. |
| `EDGE_S3_REGION` | `us-east-1` | P9-02 S3 SigV4 credential-scope region. |
```

Also add this paragraph near the production MVP or API walkthrough section:

```markdown
P9-02 S3 foundation은 S3 protocol boundary 를 먼저 세운다. `internal/s3api`
가 SigV4, XML error, route classification 을 담당하고 edge 는 인증된 S3
요청에 S3-shaped `NotImplemented` 를 반환한다. 실제 bucket/object 성공
workflow 는 P9-03 의 범위다.
```

- [ ] **Step 4: Mirror GUIDE changes into HTML**

In `docs/guide.html`, add equivalent rows and paragraph:

```html
<tr><td><code>EDGE_S3_ACCESS_KEY</code> / <code>EDGE_S3_SECRET_KEY</code></td><td>(off)</td><td>P9-02 S3 SigV4 foundation credentials. Set together.</td></tr>
<tr><td><code>EDGE_S3_REGION</code></td><td><code>us-east-1</code></td><td>P9-02 S3 SigV4 credential-scope region.</td></tr>
```

```html
<p>P9-02 S3 foundation은 S3 protocol boundary 를 먼저 세운다.
<code>internal/s3api</code> 가 SigV4, XML error, route classification 을 담당하고 edge 는 인증된 S3 요청에 S3-shaped <code>NotImplemented</code> 를 반환한다. 실제 bucket/object 성공 workflow 는 P9-03 의 범위다.</p>
```

- [ ] **Step 5: Update architecture package list**

In `docs/ARCHITECTURE.md`, add `internal/s3api` to the package structure block:

```markdown
internal/s3api/               P9 S3 protocol boundary — SigV4, XML errors,
                               route classification, compatibility helpers
```

- [ ] **Step 6: Update follow-up status**

In `docs/FOLLOWUP.md`, replace the P9-02 bullet list with:

```markdown
### ~~[P9-02] S3 compatibility foundation~~

- **DONE 2026-05-16**: `internal/s3api` added for SigV4 header verification,
  S3 XML errors, and operation classification. edge wires S3 route patterns and
  returns authenticated S3-shaped `NotImplemented` until P9-03 maps operations
  to bucket/object behavior. `scripts/smoke-s3-compat.sh` added as the
  foundation-mode `aws`/`mc` smoke skeleton.
```

- [ ] **Step 7: Add operation handoff**

Create `docs/operations/2026-05-16-s3-compatibility-foundation-handoff.md`:

```markdown
# S3 Compatibility Foundation Handoff

## Status

P9-02 complete on 2026-05-16.

## What Changed

- Added `internal/s3api` for S3 protocol concerns:
  - SigV4 header verification
  - canonical request generation
  - S3 XML errors
  - S3 operation classification
- Added edge S3 route patterns.
- Added `EDGE_S3_ACCESS_KEY`, `EDGE_S3_SECRET_KEY`, and `EDGE_S3_REGION`.
- Added `scripts/smoke-s3-compat.sh`.

## Current Runtime Behavior

Authenticated recognized S3 operations return S3-shaped `NotImplemented`.
This is intentional for P9-02. First successful bucket/object workflows belong
to P9-03.

## Verification

- `go test ./internal/s3api ./internal/edge ./cmd/kvfs-edge`
- `go test ./...`
- `go vet ./...`
- `bash -n scripts/smoke-s3-compat.sh`
- `./scripts/smoke-s3-compat.sh`
- `./scripts/check-doc-drift.sh`
- `git diff --check`

## Next Slice

P9-03 Bucket + Object API:

- CreateBucket
- ListBuckets
- DeleteBucket
- PutObject
- GetObject
- HeadObject
- DeleteObject
- ListObjectsV2
```

- [ ] **Step 8: Run documentation checks**

Run:

```bash
./scripts/check-doc-drift.sh
git diff --check
rg -n '<AGENTS.md stale-marker regex>' -g '*.md' -g '*.html' -g '!AGENTS.md'
```

Expected: doc drift and diff check pass. The `rg` stale-marker scan should
return no matches and exit 1.

- [ ] **Step 9: Commit**

```bash
git add README.md docs/GUIDE.md docs/guide.html docs/ARCHITECTURE.md docs/FOLLOWUP.md docs/operations/2026-05-16-s3-compatibility-foundation-handoff.md
git commit -m "docs: document s3 compatibility foundation"
```

### Task 8: Final Verification

**Files:**
- No new files unless checks reveal required fixes.

- [ ] **Step 1: Run focused tests**

Run:

```bash
go test ./internal/s3api ./internal/edge ./cmd/kvfs-edge
```

Expected: PASS.

- [ ] **Step 2: Run full Go tests**

Run:

```bash
go test ./...
```

Expected: PASS.

- [ ] **Step 3: Run vet**

Run:

```bash
go vet ./...
```

Expected: PASS.

- [ ] **Step 4: Run smoke skeleton syntax and skip-safe execution**

Run:

```bash
bash -n scripts/smoke-s3-compat.sh
./scripts/smoke-s3-compat.sh
```

Expected: syntax PASS. Runtime exits 0 with either `SKIP` when tools or
credentials are absent, or foundation-mode PASS when an edge endpoint is
available.

- [ ] **Step 5: Run docs and worktree checks**

Run:

```bash
./scripts/check-doc-drift.sh
git diff --check
rg -n '<AGENTS.md stale-marker regex>' -g '*.md' -g '*.html' -g '!AGENTS.md'
git status --short --branch
```

Expected:

- `./scripts/check-doc-drift.sh`: `OK — no drift detected.`
- `git diff --check`: no output.
- stale-marker `rg`: no matches, exit 1.
- `git status --short --branch`: clean except branch ahead count.

- [ ] **Step 6: Commit verification-only fixes if any were needed**

If verification required code or docs fixes, commit those fixes:

```bash
git add <changed-files>
git commit -m "fix: stabilize s3 foundation verification"
```

If no fixes were needed, do not create an empty commit.

## Self-Review

### Spec Coverage

- `internal/s3api`: Task 1, Task 2, Task 3.
- SigV4 canonical request verification: Task 3.
- S3 XML response and error shape: Task 1.
- Route mapping: Task 2 and Task 4.
- `aws s3` / `mc` smoke suite skeleton: Task 6.
- Documentation updates for new env vars: Task 7.
- P9-03 boundary preserved: Scope Check, Task 4 response behavior, Task 7 docs.

### Placeholder Scan

This plan intentionally avoids red-flag planning language. All code-bearing steps
include concrete file paths, commands, and expected outcomes.

### Type Consistency

- `s3api.StaticCredentials` is defined in Task 3 and used in Task 4 and Task 5.
- `s3api.CredentialProvider` is defined in Task 3 and used by `edge.Server`.
- `s3api.WriteError(w, r, err)` is defined in Task 1 and used in Task 4.
- `s3api.Operation` and `RequestInfo` are defined in Task 2 and used in Task 4.
