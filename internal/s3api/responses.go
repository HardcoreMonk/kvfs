// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package s3api

import (
	"encoding/xml"
	"net/http"
	"time"
)

const xmlNS = "http://s3.amazonaws.com/doc/2006-03-01/"

// Bucket is one entry in ListBuckets.
type Bucket struct {
	Name         string    `xml:"Name"`
	CreationDate time.Time `xml:"CreationDate"`
}

// ListAllMyBucketsResult is the S3 ListBuckets response shape.
type ListAllMyBucketsResult struct {
	XMLName xml.Name `xml:"ListAllMyBucketsResult"`
	XMLNS   string   `xml:"xmlns,attr,omitempty"`
	Buckets []Bucket `xml:"Buckets>Bucket"`
}

// ObjectContent is one Contents entry in ListObjectsV2.
type ObjectContent struct {
	Key          string    `xml:"Key"`
	LastModified time.Time `xml:"LastModified"`
	ETag         string    `xml:"ETag,omitempty"`
	Size         int64     `xml:"Size"`
	StorageClass string    `xml:"StorageClass,omitempty"`
}

// CommonPrefix is one CommonPrefixes entry for delimiter listings.
type CommonPrefix struct {
	Prefix string `xml:"Prefix"`
}

// ListBucketResult is the S3 ListObjectsV2 response shape.
type ListBucketResult struct {
	XMLName               xml.Name        `xml:"ListBucketResult"`
	XMLNS                 string          `xml:"xmlns,attr,omitempty"`
	Name                  string          `xml:"Name"`
	Prefix                string          `xml:"Prefix"`
	KeyCount              int             `xml:"KeyCount"`
	MaxKeys               int             `xml:"MaxKeys"`
	IsTruncated           bool            `xml:"IsTruncated"`
	ContinuationToken     string          `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string          `xml:"NextContinuationToken,omitempty"`
	StartAfter            string          `xml:"StartAfter,omitempty"`
	Contents              []ObjectContent `xml:"Contents"`
	CommonPrefixes        []CommonPrefix  `xml:"CommonPrefixes"`
}

// WriteXML writes an S3 XML success response. HEAD responses intentionally
// carry headers only.
func WriteXML(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("X-Amz-Request-Id", nextRequestID())
	w.WriteHeader(status)
	if r != nil && r.Method == http.MethodHead {
		return
	}
	if body == nil {
		return
	}
	body = addNamespace(body)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(body)
}

func addNamespace(body any) any {
	switch v := body.(type) {
	case *ListAllMyBucketsResult:
		if v.XMLNS == "" {
			v.XMLNS = xmlNS
		}
		return v
	case ListAllMyBucketsResult:
		if v.XMLNS == "" {
			v.XMLNS = xmlNS
		}
		return v
	case *ListBucketResult:
		if v.XMLNS == "" {
			v.XMLNS = xmlNS
		}
		return v
	case ListBucketResult:
		if v.XMLNS == "" {
			v.XMLNS = xmlNS
		}
		return v
	default:
		return body
	}
}
