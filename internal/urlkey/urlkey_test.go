// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package urlkey

import (
	"net/url"
	"strconv"
	"testing"
	"time"
)

func TestSignAndVerify_RoundTrip(t *testing.T) {
	s, err := NewSigner([]byte("test-secret-32-bytes-xxxxxxxxxxxxxx"))
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(1 * time.Hour).Unix()
	sig := s.Sign("PUT", "/v1/o/bucket/key", exp)

	q := url.Values{}
	q.Set("sig", sig)
	q.Set("exp", fmtInt(exp))
	if err := s.Verify("PUT", "/v1/o/bucket/key", q, time.Now()); err != nil {
		t.Fatalf("Verify round-trip failed: %v", err)
	}
}

func TestVerify_TamperDetection(t *testing.T) {
	s, _ := NewSigner([]byte("secret"))
	url := s.BuildURL("GET", "/v1/o/a/b", 1*time.Hour)

	q := parseQuery(t, url)
	// Deterministic tamper: flip first hex char to its XOR 0xf pair.
	// Guarantees a different character regardless of original value.
	sig := q.Get("sig")
	flipped := make([]byte, len(sig))
	copy(flipped, sig)
	// map '0'..'9','a'..'f' -> different hex char
	m := map[byte]byte{
		'0': 'f', '1': 'e', '2': 'd', '3': 'c',
		'4': 'b', '5': 'a', '6': '9', '7': '8',
		'8': '7', '9': '6', 'a': '5', 'b': '4',
		'c': '3', 'd': '2', 'e': '1', 'f': '0',
	}
	flipped[0] = m[flipped[0]]
	q.Set("sig", string(flipped))
	if err := s.Verify("GET", "/v1/o/a/b", q, time.Now()); err == nil {
		t.Fatal("tampered sig should fail verification")
	}
}

func TestVerify_Expired(t *testing.T) {
	s, _ := NewSigner([]byte("secret"))
	expired := time.Now().Add(-1 * time.Hour).Unix()
	sig := s.Sign("GET", "/p", expired)
	q := url.Values{}
	q.Set("sig", sig)
	q.Set("exp", fmtInt(expired))
	if err := s.Verify("GET", "/p", q, time.Now()); err != ErrExpired {
		t.Fatalf("expected ErrExpired, got %v", err)
	}
}

func TestVerify_MethodMismatch(t *testing.T) {
	s, _ := NewSigner([]byte("secret"))
	u := s.BuildURL("PUT", "/p", 1*time.Hour)
	q := parseQuery(t, u)
	// Verify with GET instead of PUT
	if err := s.Verify("GET", "/p", q, time.Now()); err != ErrBadSig {
		t.Fatalf("method mismatch should yield ErrBadSig, got %v", err)
	}
}

func TestNewSigner_EmptySecret(t *testing.T) {
	if _, err := NewSigner(nil); err != ErrEmptySecret {
		t.Fatalf("expected ErrEmptySecret, got %v", err)
	}
}

// helpers
func fmtInt(n int64) string { return strconv.FormatInt(n, 10) }

var _ = time.Now // keep import if not yet used in helpers

func parseQuery(t *testing.T, rawurl string) url.Values {
	t.Helper()
	// strip leading path before `?`
	for i := 0; i < len(rawurl); i++ {
		if rawurl[i] == '?' {
			q, err := url.ParseQuery(rawurl[i+1:])
			if err != nil {
				t.Fatal(err)
			}
			return q
		}
	}
	t.Fatal("no query string in built URL")
	return nil
}
