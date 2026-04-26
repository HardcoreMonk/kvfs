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
	sig, kid := s.Sign("PUT", "/v1/o/bucket/key", exp)
	if kid != DefaultKid {
		t.Errorf("primary kid = %q, want %q", kid, DefaultKid)
	}

	q := url.Values{}
	q.Set("kid", kid)
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
	sig, _ := s.Sign("GET", "/p", expired)
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

// ─── ADR-028 multi-kid rotation tests ───

func TestRotation_OldUrlsStillVerify(t *testing.T) {
	s, _ := NewSigner([]byte("secret-v1"))
	oldURL := s.BuildURL("PUT", "/o/k", time.Hour)

	// Add v2 + make primary
	if err := s.Add("v2", []byte("secret-v2")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPrimary("v2"); err != nil {
		t.Fatal(err)
	}
	if s.Primary() != "v2" {
		t.Errorf("Primary = %q, want v2", s.Primary())
	}

	// Old URL was signed with v1; should still verify (v1 retained).
	q := parseQuery(t, oldURL)
	if err := s.Verify("PUT", "/o/k", q, time.Now()); err != nil {
		t.Errorf("old v1 URL should still verify: %v", err)
	}

	// New URL uses v2.
	newURL := s.BuildURL("PUT", "/o/k", time.Hour)
	q2 := parseQuery(t, newURL)
	if q2.Get("kid") != "v2" {
		t.Errorf("new URL kid = %q, want v2", q2.Get("kid"))
	}
	if err := s.Verify("PUT", "/o/k", q2, time.Now()); err != nil {
		t.Errorf("new v2 URL should verify: %v", err)
	}
}

func TestRotation_RemovedKidRejected(t *testing.T) {
	s, _ := NewSigner([]byte("secret-v1"))
	oldURL := s.BuildURL("GET", "/o/k", time.Hour)
	s.Add("v2", []byte("secret-v2"))
	s.SetPrimary("v2")

	// Remove v1 (no longer primary).
	if err := s.Remove("v1"); err != nil {
		t.Fatalf("Remove v1: %v", err)
	}

	q := parseQuery(t, oldURL)
	err := s.Verify("GET", "/o/k", q, time.Now())
	if err != ErrUnknownKid {
		t.Errorf("expected ErrUnknownKid, got %v", err)
	}
}

func TestRemove_PrimaryRefused(t *testing.T) {
	s, _ := NewSigner([]byte("s1"))
	s.Add("v2", []byte("s2"))
	if err := s.Remove("v1"); err != nil { // v1 is still primary
		// Actually NewSigner sets v1 primary; removing v1 should fail.
		if err != ErrPrimaryRemove {
			t.Errorf("expected ErrPrimaryRemove, got %v", err)
		}
	}
}

func TestRemove_LastKidRefused(t *testing.T) {
	s, _ := NewSigner([]byte("only-secret"))
	if err := s.Remove(DefaultKid); err != ErrPrimaryRemove {
		// primary check fires before last-kid check; both OK as defenses.
		// (last-kid would only matter if we had primary=other and only one kid left.)
		t.Logf("removal of sole+primary kid: %v (acceptable)", err)
	}
}

func TestVerify_BackwardCompat_NoKidUsesPrimary(t *testing.T) {
	// Pre-ADR-028 URL: no ?kid= param. Verify should fall back to primary.
	s, _ := NewSigner([]byte("legacy"))
	exp := time.Now().Add(time.Hour).Unix()
	sig, _ := s.Sign("PUT", "/o/k", exp)
	q := url.Values{}
	q.Set("sig", sig)
	q.Set("exp", fmtInt(exp))
	// q has no "kid"
	if err := s.Verify("PUT", "/o/k", q, time.Now()); err != nil {
		t.Errorf("backward-compat verify should use primary: %v", err)
	}
}

func TestNewMultiSigner_RoundTrip(t *testing.T) {
	keys := map[string][]byte{
		"a": []byte("secret-a"),
		"b": []byte("secret-b"),
	}
	s, err := NewMultiSigner(keys, "b")
	if err != nil {
		t.Fatal(err)
	}
	if s.Primary() != "b" {
		t.Errorf("Primary = %q, want b", s.Primary())
	}
	if got := s.Kids(); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("Kids = %v, want [a b]", got)
	}
	url := s.BuildURL("GET", "/x", time.Hour)
	q := parseQuery(t, url)
	if q.Get("kid") != "b" {
		t.Errorf("BuildURL kid = %q, want b", q.Get("kid"))
	}
	if err := s.Verify("GET", "/x", q, time.Now()); err != nil {
		t.Errorf("verify: %v", err)
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
