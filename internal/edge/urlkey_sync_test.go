// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package edge

import (
	"encoding/hex"
	"sort"
	"testing"

	"github.com/HardcoreMonk/kvfs/internal/store"
	"github.com/HardcoreMonk/kvfs/internal/urlkey"
)

// SyncURLKeys must add new kids, swap primary, and remove disappeared
// kids — all in a single sweep, idempotent on repeat calls. Guards the
// ADR-049 propagation contract.
func TestSyncURLKeys_AddSwapPrimaryRemove(t *testing.T) {
	// Boot signer: only "v1".
	v1 := mustHex("aaaa")
	signer, err := urlkey.NewSigner(v1)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	if signer.Primary() != "v1" {
		t.Fatalf("primary = %q, want v1", signer.Primary())
	}

	// Coord's view: v1 demoted, v2 added as primary.
	coord := []store.URLKeyEntry{
		{Kid: "v1", SecretHex: "aaaa", IsPrimary: false},
		{Kid: "v2", SecretHex: "bbbb", IsPrimary: true},
	}

	var failures []error
	SyncURLKeys(signer, coord, func(e error) { failures = append(failures, e) })
	if len(failures) > 0 {
		t.Errorf("unexpected sync failures: %v", failures)
	}

	if got := sortedKids(signer); !equalSlices(got, []string{"v1", "v2"}) {
		t.Errorf("post-sync kids = %v, want [v1 v2]", got)
	}
	if signer.Primary() != "v2" {
		t.Errorf("primary = %q, want v2", signer.Primary())
	}

	// Now coord removes v1 (operator: cli urlkey remove --kid v1).
	coord = []store.URLKeyEntry{
		{Kid: "v2", SecretHex: "bbbb", IsPrimary: true},
	}
	SyncURLKeys(signer, coord, func(e error) { failures = append(failures, e) })
	if got := sortedKids(signer); !equalSlices(got, []string{"v2"}) {
		t.Errorf("after remove, kids = %v, want [v2]", got)
	}

	// Idempotent: same coord state → no changes, no errors.
	failures = nil
	SyncURLKeys(signer, coord, func(e error) { failures = append(failures, e) })
	if len(failures) > 0 {
		t.Errorf("idempotent sync produced failures: %v", failures)
	}
}

// Sync must not remove the last kid even if coord lists none — Signer's
// Remove rejects that case, and SyncURLKeys must swallow that specific
// error rather than spamming the failure callback.
func TestSyncURLKeys_PreservesLastKid(t *testing.T) {
	v1 := mustHex("cccc")
	signer, _ := urlkey.NewSigner(v1)

	var failures []error
	SyncURLKeys(signer, nil, func(e error) { failures = append(failures, e) })

	if len(signer.Kids()) != 1 {
		t.Errorf("after empty-coord sync, kids = %v, want [v1] preserved", signer.Kids())
	}
	if len(failures) > 0 {
		t.Errorf("preserve-last should not emit failures, got %v", failures)
	}
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func sortedKids(s *urlkey.Signer) []string {
	out := append([]string{}, s.Kids()...)
	sort.Strings(out)
	return out
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
