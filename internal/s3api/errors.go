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
	CodeBucketAlreadyOwnedByYou      = "BucketAlreadyOwnedByYou"
	CodeBucketNotEmpty               = "BucketNotEmpty"
	CodeInternalError                = "InternalError"
	CodeInvalidArgument              = "InvalidArgument"
	CodeInvalidBucketName            = "InvalidBucketName"
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
