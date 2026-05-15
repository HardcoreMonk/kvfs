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
	OperationUnsupported             Operation = "Unsupported"
	OperationListBuckets             Operation = "ListBuckets"
	OperationCreateBucket            Operation = "CreateBucket"
	OperationDeleteBucket            Operation = "DeleteBucket"
	OperationPutObject               Operation = "PutObject"
	OperationGetObject               Operation = "GetObject"
	OperationHeadObject              Operation = "HeadObject"
	OperationDeleteObject            Operation = "DeleteObject"
	OperationListObjectsV2           Operation = "ListObjectsV2"
	OperationCreateMultipartUpload   Operation = "CreateMultipartUpload"
	OperationUploadPart              Operation = "UploadPart"
	OperationListParts               Operation = "ListParts"
	OperationCompleteMultipartUpload Operation = "CompleteMultipartUpload"
	OperationAbortMultipartUpload    Operation = "AbortMultipartUpload"
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
