// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package edge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestParseQuorumHeader (ADR-053): valid range, bad input, missing.
func TestParseQuorumHeader(t *testing.T) {
	cases := []struct {
		name    string
		header  string
		max     int
		want    int
		wantErr bool
	}{
		{"empty → 0", "", 3, 0, false},
		{"valid 1", "1", 3, 1, false},
		{"valid 3", "3", 3, 3, false},
		{"too low", "0", 3, 0, true},
		{"too high", "5", 3, 0, true},
		{"non-numeric", "abc", 3, 0, true},
		{"max=0 disables upper bound", "100", 0, 100, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			if tc.header != "" {
				req.Header.Set("X-KVFS-W", tc.header)
			}
			got, err := parseQuorumHeader(req, "X-KVFS-W", tc.max)
			if (err != nil) != tc.wantErr {
				t.Errorf("err=%v want err=%v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// TestReadChunkAgreement_AllAgree: all r replicas return the same body
// (which they must — content-addressable). Returns body, no error.
func TestReadChunkAgreement_AllAgree(t *testing.T) {
	id, body := mkChunk("agreement-payload")
	dns := []*fakeDN{
		{chunks: map[string][]byte{id: body}},
		{chunks: map[string][]byte{id: body}},
		{chunks: map[string][]byte{id: body}},
	}
	addrs := startFakeDNs(t, dns)
	srv := newTestServer(t, addrs)

	got, err := srv.readChunkAgreement(context.Background(), id, addrs, 3)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("body mismatch")
	}
}

// TestReadChunkAgreement_OneFails: r=3 requested, one DN dead → 502.
// Caller (handleGet) translates this to BadGateway. Demonstrates that
// X-KVFS-R: 3 turns transient unavailability into a hard failure —
// trade-off for stronger consistency.
func TestReadChunkAgreement_OneFails(t *testing.T) {
	id, body := mkChunk("agreement-payload")
	dns := []*fakeDN{
		{chunks: map[string][]byte{id: body}},
		{chunks: map[string][]byte{id: body}, fail: true},
		{chunks: map[string][]byte{id: body}},
	}
	addrs := startFakeDNs(t, dns)
	srv := newTestServer(t, addrs)

	_, err := srv.readChunkAgreement(context.Background(), id, addrs, 3)
	if err == nil {
		t.Fatalf("expected error when one of r=3 replicas fails")
	}
	if !strings.Contains(err.Error(), "quorum") {
		t.Errorf("err should mention quorum: %v", err)
	}
}

// TestReadChunkAgreement_DisagreementDetected: simulate corruption by
// making one DN return wrong body. Since the chunk_id IS the sha256,
// the agreement check catches the bad body. The DN that returned wrong
// data fails verification, so we get a "did not return verified body"
// error rather than a "disagreement" error. Both are correct responses
// to corruption; the test asserts that 200 with bad body is *not* what
// we get.
func TestReadChunkAgreement_DisagreementDetected(t *testing.T) {
	id, body := mkChunk("real-payload")
	corrupt := []byte("not-the-right-bytes")
	dns := []*fakeDN{
		{chunks: map[string][]byte{id: body}},
		{chunks: map[string][]byte{id: corrupt}}, // wrong body, sha mismatch
		{chunks: map[string][]byte{id: body}},
	}
	addrs := startFakeDNs(t, dns)
	srv := newTestServer(t, addrs)

	got, err := srv.readChunkAgreement(context.Background(), id, addrs, 3)
	if err == nil {
		t.Errorf("expected corruption to surface as error; got body=%q", got)
	}
}

// (mkChunk + fakeDN + startFakeDNs + newTestServer live in
// parallel_fetch_test.go — same package, shared.)
var (
	_ = sha256.Sum256
	_ = hex.EncodeToString
	_ = fmt.Sprintf
)
