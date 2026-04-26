// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package urlkey

import (
	"net/url"
	"testing"
	"time"
)

func benchSigner(b *testing.B) *Signer {
	b.Helper()
	s, err := NewSigner([]byte("benchmark-secret-must-be-long-enough"))
	if err != nil {
		b.Fatalf("NewSigner: %v", err)
	}
	return s
}

func BenchmarkSign(b *testing.B) {
	s := benchSigner(b)
	exp := time.Now().Add(time.Hour).Unix()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.Sign("GET", "/v1/o/bucket/some/key/path.bin", exp)
	}
}

func BenchmarkBuildURL(b *testing.B) {
	s := benchSigner(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.BuildURL("PUT", "/v1/o/bucket/some/key/path.bin", time.Hour)
	}
}

func BenchmarkVerify(b *testing.B) {
	s := benchSigner(b)
	rel := s.BuildURL("GET", "/v1/o/bucket/some/key/path.bin", time.Hour)
	// rel is "<path>?sig=...&exp=..."; split into path + query for Verify().
	u, err := url.Parse(rel)
	if err != nil {
		b.Fatalf("url.Parse: %v", err)
	}
	now := time.Now()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.Verify("GET", u.Path, u.Query(), now); err != nil {
			b.Fatalf("Verify: %v", err)
		}
	}
}
