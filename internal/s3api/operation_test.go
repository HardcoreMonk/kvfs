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
