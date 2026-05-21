// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package edge

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	coorddaemon "github.com/HardcoreMonk/kvfs/internal/coord"
	"github.com/HardcoreMonk/kvfs/internal/coordinator"
	"github.com/HardcoreMonk/kvfs/internal/dn"
	"github.com/HardcoreMonk/kvfs/internal/placement"
	"github.com/HardcoreMonk/kvfs/internal/s3api"
	"github.com/HardcoreMonk/kvfs/internal/store"
)

const s3TestEmptyPayloadSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func TestS3Foundation_AuthenticatedOperationReturnsXMLNotImplemented(t *testing.T) {
	srv := &Server{
		S3Credentials: s3api.StaticCredentials{"AKIA_TEST": "test-secret"},
		S3Region:      "us-east-1",
	}
	req := httptest.NewRequest(http.MethodPost, "/photos/raw/a.jpg?uploads", nil)
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
	if !strings.Contains(rec.Body.String(), "CreateMultipartUpload") {
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

func TestS3Foundation_UnauthenticatedUnsupportedOperationFailsClosed(t *testing.T) {
	srv := &Server{S3Credentials: s3api.StaticCredentials{"AKIA_TEST": "test-secret"}}
	req := httptest.NewRequest(http.MethodGet, "/photos", nil)
	rec := httptest.NewRecorder()

	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "<Code>MissingSecurityHeader</Code>") {
		t.Fatalf("body should contain S3 MissingSecurityHeader XML: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "<Code>NotImplemented</Code>") {
		t.Fatalf("body should not disclose NotImplemented before auth: %s", rec.Body.String())
	}
}

func TestS3Foundation_AuthenticatedUnsupportedOperationReturnsXMLNotImplemented(t *testing.T) {
	srv := &Server{S3Credentials: s3api.StaticCredentials{"AKIA_TEST": "test-secret"}}
	req := httptest.NewRequest(http.MethodGet, "/photos", nil)
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
}

func TestS3Foundation_BucketPostRoutesToFoundation(t *testing.T) {
	srv := &Server{S3Credentials: s3api.StaticCredentials{"AKIA_TEST": "test-secret"}}
	req := httptest.NewRequest(http.MethodPost, "/photos?delete", nil)
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
}

func TestS3Foundation_HEADObjectRoutesToFoundationWithoutBody(t *testing.T) {
	srv := newS3OnlyTestServer(t)
	req := signedS3Request(t, http.MethodHead, "/photos/raw/a.jpg", nil)
	rec := httptest.NewRecorder()

	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/xml") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD response body length = %d, want 0; body=%s", rec.Body.Len(), rec.Body.String())
	}
}

func TestS3Foundation_WrongRegionOrServiceRejected(t *testing.T) {
	tests := []struct {
		name    string
		region  string
		service string
	}{
		{name: "wrong region", region: "us-west-2", service: "s3"},
		{name: "wrong service", region: "us-east-1", service: "ec2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := &Server{
				S3Credentials: s3api.StaticCredentials{"AKIA_TEST": "test-secret"},
				S3Region:      "us-east-1",
			}
			req := httptest.NewRequest(http.MethodGet, "/photos?list-type=2", nil)
			req.Host = "kvfs.local"
			signS3TestRequestWithService(t, req, "AKIA_TEST", "test-secret", tt.region, tt.service)
			rec := httptest.NewRecorder()

			srv.Routes().ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "<Code>AuthorizationHeaderMalformed</Code>") {
				t.Fatalf("body should contain S3 AuthorizationHeaderMalformed XML: %s", rec.Body.String())
			}
		})
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

func TestS3Foundation_NativeV1UnknownPathDoesNotRouteToS3(t *testing.T) {
	srv := &Server{S3Credentials: s3api.StaticCredentials{"AKIA_TEST": "test-secret"}}
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/nope", nil)
	rec := httptest.NewRecorder()

	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "<Code>MissingSecurityHeader</Code>") {
		t.Fatalf("body should not contain S3 MissingSecurityHeader XML: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "<Code>NotImplemented</Code>") {
		t.Fatalf("body should not contain S3 NotImplemented XML: %s", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "application/xml") {
		t.Fatalf("Content-Type = %q, should not be S3 XML", ct)
	}
}

func TestS3Foundation_UnsupportedObjectMethodMissingAuthReturnsS3XML(t *testing.T) {
	srv := &Server{S3Credentials: s3api.StaticCredentials{"AKIA_TEST": "test-secret"}}
	req := httptest.NewRequest(http.MethodPatch, "/photos/raw/a.jpg", nil)
	rec := httptest.NewRecorder()

	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "<Code>MissingSecurityHeader</Code>") {
		t.Fatalf("body should contain S3 MissingSecurityHeader XML: %s", rec.Body.String())
	}
}

func TestS3Foundation_UnsupportedObjectMethodWithAuthReturnsXMLNotImplemented(t *testing.T) {
	srv := &Server{S3Credentials: s3api.StaticCredentials{"AKIA_TEST": "test-secret"}}
	req := httptest.NewRequest(http.MethodPatch, "/photos/raw/a.jpg", nil)
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
}

func TestS3Foundation_UnsupportedBucketMethodMissingAuthReturnsS3XML(t *testing.T) {
	srv := &Server{S3Credentials: s3api.StaticCredentials{"AKIA_TEST": "test-secret"}}
	req := httptest.NewRequest(http.MethodOptions, "/photos", nil)
	rec := httptest.NewRecorder()

	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "<Code>MissingSecurityHeader</Code>") {
		t.Fatalf("body should contain S3 MissingSecurityHeader XML: %s", rec.Body.String())
	}
}

func TestS3BucketAPI_CreateListDelete(t *testing.T) {
	srv := newS3OnlyTestServer(t)

	createReq := signedS3Request(t, http.MethodPut, "/photos", nil)
	createRec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create bucket status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	listReq := signedS3Request(t, http.MethodGet, "/", nil)
	listRec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list buckets status=%d body=%s", listRec.Code, listRec.Body.String())
	}
	if !strings.Contains(listRec.Body.String(), "<Name>photos</Name>") {
		t.Fatalf("list buckets body missing photos: %s", listRec.Body.String())
	}

	deleteReq := signedS3Request(t, http.MethodDelete, "/photos", nil)
	deleteRec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(deleteRec, deleteReq)
	if deleteRec.Code != http.StatusOK {
		t.Fatalf("delete bucket status=%d body=%s", deleteRec.Code, deleteRec.Body.String())
	}
}

func TestS3BucketAPI_DeleteNonEmptyReturnsBucketNotEmpty(t *testing.T) {
	srv := newS3ObjectTestServer(t)
	mustS3(t, srv, http.MethodPut, "/photos", nil, http.StatusOK)
	mustS3(t, srv, http.MethodPut, "/photos/a.txt", strings.NewReader("hello"), http.StatusOK)

	req := signedS3Request(t, http.MethodDelete, "/photos", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("delete non-empty status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "<Code>BucketNotEmpty</Code>") {
		t.Fatalf("body should contain BucketNotEmpty XML: %s", rec.Body.String())
	}
}

func TestS3ObjectAPI_PutGetHeadDelete(t *testing.T) {
	srv := newS3ObjectTestServer(t)
	mustS3(t, srv, http.MethodPut, "/photos", nil, http.StatusOK)

	putReq := signedS3Request(t, http.MethodPut, "/photos/a.txt", strings.NewReader("hello"))
	putRec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("put object status=%d body=%s", putRec.Code, putRec.Body.String())
	}
	etag := putRec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("PUT ETag missing")
	}

	getReq := signedS3Request(t, http.MethodGet, "/photos/a.txt", nil)
	getRec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get object status=%d body=%s", getRec.Code, getRec.Body.String())
	}
	if getRec.Body.String() != "hello" {
		t.Fatalf("get body=%q want hello", getRec.Body.String())
	}
	if got := getRec.Header().Get("ETag"); got != etag {
		t.Fatalf("GET ETag=%q want %q", got, etag)
	}

	headReq := signedS3Request(t, http.MethodHead, "/photos/a.txt", nil)
	headRec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(headRec, headReq)
	if headRec.Code != http.StatusOK {
		t.Fatalf("head object status=%d body=%s", headRec.Code, headRec.Body.String())
	}
	if headRec.Body.Len() != 0 {
		t.Fatalf("HEAD body len=%d want 0", headRec.Body.Len())
	}

	deleteReq := signedS3Request(t, http.MethodDelete, "/photos/a.txt", nil)
	deleteRec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(deleteRec, deleteReq)
	if deleteRec.Code != http.StatusNoContent {
		t.Fatalf("delete object status=%d body=%s", deleteRec.Code, deleteRec.Body.String())
	}

	getMissingReq := signedS3Request(t, http.MethodGet, "/photos/a.txt", nil)
	getMissingRec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(getMissingRec, getMissingReq)
	if getMissingRec.Code != http.StatusNotFound {
		t.Fatalf("missing get status=%d body=%s", getMissingRec.Code, getMissingRec.Body.String())
	}
	if !strings.Contains(getMissingRec.Body.String(), "<Code>NoSuchKey</Code>") {
		t.Fatalf("missing get should be NoSuchKey: %s", getMissingRec.Body.String())
	}
}

func TestS3ObjectAPI_ListObjectsV2PrefixAndDelimiter(t *testing.T) {
	srv := newS3ObjectTestServer(t)
	mustS3(t, srv, http.MethodPut, "/photos", nil, http.StatusOK)
	mustS3(t, srv, http.MethodPut, "/photos/raw/a.txt", strings.NewReader("a"), http.StatusOK)
	mustS3(t, srv, http.MethodPut, "/photos/raw/b.txt", strings.NewReader("b"), http.StatusOK)
	mustS3(t, srv, http.MethodPut, "/photos/thumbs/a.txt", strings.NewReader("thumb"), http.StatusOK)

	prefixReq := signedS3Request(t, http.MethodGet, "/photos?list-type=2&prefix=raw/", nil)
	prefixRec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(prefixRec, prefixReq)
	if prefixRec.Code != http.StatusOK {
		t.Fatalf("prefix list status=%d body=%s", prefixRec.Code, prefixRec.Body.String())
	}
	if !strings.Contains(prefixRec.Body.String(), "<Key>raw/a.txt</Key>") ||
		!strings.Contains(prefixRec.Body.String(), "<Key>raw/b.txt</Key>") {
		t.Fatalf("prefix list missing raw keys: %s", prefixRec.Body.String())
	}
	if strings.Contains(prefixRec.Body.String(), "<Key>thumbs/a.txt</Key>") {
		t.Fatalf("prefix list should not include thumbs key: %s", prefixRec.Body.String())
	}

	delimiterReq := signedS3Request(t, http.MethodGet, "/photos?list-type=2&delimiter=/", nil)
	delimiterRec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(delimiterRec, delimiterReq)
	if delimiterRec.Code != http.StatusOK {
		t.Fatalf("delimiter list status=%d body=%s", delimiterRec.Code, delimiterRec.Body.String())
	}
	if !strings.Contains(delimiterRec.Body.String(), "<Prefix>raw/</Prefix>") ||
		!strings.Contains(delimiterRec.Body.String(), "<Prefix>thumbs/</Prefix>") {
		t.Fatalf("delimiter list missing common prefixes: %s", delimiterRec.Body.String())
	}
	if strings.Contains(delimiterRec.Body.String(), "<Key>raw/a.txt</Key>") {
		t.Fatalf("delimiter list should collapse raw keys: %s", delimiterRec.Body.String())
	}
}

func TestS3ObjectAPI_CoordProxyMetadata(t *testing.T) {
	srv := newS3CoordProxyTestServer(t)
	mustS3(t, srv, http.MethodPut, "/photos", nil, http.StatusOK)
	mustS3(t, srv, http.MethodPut, "/photos/a.txt", strings.NewReader("hello"), http.StatusOK)

	if _, err := srv.Store.GetBucket("photos"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("edge local bucket err=%v, want ErrNotFound in coord-proxy mode", err)
	}
	if _, err := srv.Store.GetObject("photos", "a.txt"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("edge local object err=%v, want ErrNotFound in coord-proxy mode", err)
	}

	listReq := signedS3Request(t, http.MethodGet, "/photos?list-type=2", nil)
	listRec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("coord-proxy list status=%d body=%s", listRec.Code, listRec.Body.String())
	}
	if !strings.Contains(listRec.Body.String(), "<Key>a.txt</Key>") {
		t.Fatalf("coord-proxy list missing key: %s", listRec.Body.String())
	}

	getReq := signedS3Request(t, http.MethodGet, "/photos/a.txt", nil)
	getRec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK || getRec.Body.String() != "hello" {
		t.Fatalf("coord-proxy get status=%d body=%q", getRec.Code, getRec.Body.String())
	}
}

func signedS3Request(t *testing.T, method, target string, body io.Reader) *http.Request {
	t.Helper()
	if body == nil {
		body = http.NoBody
	}
	req := httptest.NewRequest(method, target, body)
	req.Host = "kvfs.local"
	signS3TestRequest(t, req, "AKIA_TEST", "test-secret", "us-east-1")
	return req
}

func mustS3(t *testing.T, srv *Server, method, target string, body io.Reader, want int) {
	t.Helper()
	req := signedS3Request(t, method, target, body)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != want {
		t.Fatalf("%s %s status=%d want %d body=%s", method, target, rec.Code, want, rec.Body.String())
	}
}

func newS3OnlyTestServer(t *testing.T) *Server {
	t.Helper()
	ms, err := store.Open(filepath.Join(t.TempDir(), "edge.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = ms.Close() })
	return &Server{
		Store:         ms,
		S3Credentials: s3api.StaticCredentials{"AKIA_TEST": "test-secret"},
		S3Region:      "us-east-1",
		Log:           slog.New(slog.NewTextHandler(testLogWriter{t}, &slog.HandlerOptions{Level: slog.LevelError})),
	}
}

func newS3ObjectTestServer(t *testing.T) *Server {
	t.Helper()
	dnSrv, err := dn.NewServer("dn1", t.TempDir())
	if err != nil {
		t.Fatalf("new dn: %v", err)
	}
	ts := httptest.NewServer(dnSrv.Routes())
	t.Cleanup(ts.Close)
	addr := strings.TrimPrefix(ts.URL, "http://")
	ms, err := store.Open(filepath.Join(t.TempDir(), "edge.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = ms.Close() })
	coord, err := coordinator.New(coordinator.Config{
		Nodes:             []placement.Node{{ID: addr, Addr: addr}},
		ReplicationFactor: 1,
		QuorumWrite:       1,
	})
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	return &Server{
		Store:         ms,
		Coord:         coord,
		S3Credentials: s3api.StaticCredentials{"AKIA_TEST": "test-secret"},
		S3Region:      "us-east-1",
		Log:           slog.New(slog.NewTextHandler(testLogWriter{t}, &slog.HandlerOptions{Level: slog.LevelError})),
	}
}

func newS3CoordProxyTestServer(t *testing.T) *Server {
	t.Helper()
	dnSrv, err := dn.NewServer("dn1", t.TempDir())
	if err != nil {
		t.Fatalf("new dn: %v", err)
	}
	dnTS := httptest.NewServer(dnSrv.Routes())
	t.Cleanup(dnTS.Close)
	addr := strings.TrimPrefix(dnTS.URL, "http://")

	coordStore, err := store.Open(filepath.Join(t.TempDir(), "coord.db"))
	if err != nil {
		t.Fatalf("open coord store: %v", err)
	}
	t.Cleanup(func() { _ = coordStore.Close() })
	coordSrv := &coorddaemon.Server{
		Store:  coordStore,
		Placer: placement.New([]placement.Node{{ID: addr, Addr: addr}}),
	}
	coordTS := httptest.NewServer(coordSrv.Routes())
	t.Cleanup(coordTS.Close)

	edgeStore, err := store.Open(filepath.Join(t.TempDir(), "edge.db"))
	if err != nil {
		t.Fatalf("open edge store: %v", err)
	}
	t.Cleanup(func() { _ = edgeStore.Close() })
	edgeCoord, err := coordinator.New(coordinator.Config{
		Nodes:             []placement.Node{{ID: addr, Addr: addr}},
		ReplicationFactor: 1,
		QuorumWrite:       1,
	})
	if err != nil {
		t.Fatalf("edge coordinator: %v", err)
	}
	return &Server{
		Store:         edgeStore,
		Coord:         edgeCoord,
		CoordClient:   NewCoordClient(coordTS.URL),
		S3Credentials: s3api.StaticCredentials{"AKIA_TEST": "test-secret"},
		S3Region:      "us-east-1",
		Log:           slog.New(slog.NewTextHandler(testLogWriter{t}, &slog.HandlerOptions{Level: slog.LevelError})),
	}
}

func signS3TestRequest(t *testing.T, req *http.Request, accessKey, secret, region string) {
	t.Helper()
	signS3TestRequestWithService(t, req, accessKey, secret, region, "s3")
}

func signS3TestRequestWithService(t *testing.T, req *http.Request, accessKey, secret, region, service string) {
	t.Helper()
	amzDate := time.Now().UTC().Format("20060102T150405Z")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", s3TestEmptyPayloadSHA256)

	signedHeaders := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	scopeDate := amzDate[:len("20060102")]
	scope := scopeDate + "/" + region + "/" + service + "/aws4_request"
	creq, err := canonicalS3TestRequest(req, signedHeaders, s3TestEmptyPayloadSHA256)
	if err != nil {
		t.Fatal(err)
	}
	sts := s3TestStringToSign(amzDate, scope, creq)
	key := s3TestSigningKey(secret, scopeDate, region, service)
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
