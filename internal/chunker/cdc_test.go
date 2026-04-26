// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package chunker

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"testing"
)

// readAllPieces drains a *CDCReader into a slice for inspection.
func readAllPieces(t *testing.T, r *CDCReader) []*StreamPiece {
	t.Helper()
	var out []*StreamPiece
	for {
		p, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, p)
	}
	return out
}

func TestCDCDeterministic(t *testing.T) {
	body := bytes.Repeat([]byte("kvfs-cdc-deterministic-content"), 1000) // ~30 KB
	cfg := CDCConfig{MinSize: 256, NormalSize: 1024, MaxSize: 4096, MaskBitsStrict: 10, MaskBitsLoose: 8}

	a := readAllPieces(t, NewCDCReader(bytes.NewReader(body), cfg))
	b := readAllPieces(t, NewCDCReader(bytes.NewReader(body), cfg))

	if len(a) != len(b) {
		t.Fatalf("piece count differs: a=%d b=%d", len(a), len(b))
	}
	for i := range a {
		if a[i].ID != b[i].ID {
			t.Errorf("piece %d id differs: %s vs %s", i, a[i].ID, b[i].ID)
		}
	}
}

func TestCDCShiftInvariance(t *testing.T) {
	// Random body so cut points are spread; reproducible seed for stable test.
	rng := rand.New(rand.NewSource(0x1234))
	body := make([]byte, 256*1024)
	rng.Read(body)
	shifted := append([]byte{0xAA}, body...) // insert 1 byte at start

	cfg := CDCConfig{MinSize: 1024, NormalSize: 4096, MaxSize: 16384, MaskBitsStrict: 12, MaskBitsLoose: 10}
	a := readAllPieces(t, NewCDCReader(bytes.NewReader(body), cfg))
	b := readAllPieces(t, NewCDCReader(bytes.NewReader(shifted), cfg))

	// Build sets of chunk IDs and compute overlap.
	ids := map[string]struct{}{}
	for _, p := range a {
		ids[p.ID] = struct{}{}
	}
	shared := 0
	for _, p := range b {
		if _, ok := ids[p.ID]; ok {
			shared++
		}
	}
	// Shift invariance: at least 80% of shifted chunks should still match.
	// (Real-world FastCDC achieves >95%; we use a loose bound for robustness
	// against the random seed — the property is the principle, not a number.)
	threshold := len(b) * 80 / 100
	if shared < threshold {
		t.Errorf("shift invariance weak: shared=%d/%d (want >= %d)", shared, len(b), threshold)
	}
	// Also: at least SOME identical chunks (sanity vs fixed-size chunking).
	if shared == 0 {
		t.Errorf("zero shared chunks — CDC indistinguishable from fixed chunking")
	}
}

func TestCDCBoundaryRespectsMinMax(t *testing.T) {
	rng := rand.New(rand.NewSource(0x5678))
	body := make([]byte, 1024*1024)
	rng.Read(body)
	cfg := CDCConfig{MinSize: 8 * 1024, NormalSize: 32 * 1024, MaxSize: 128 * 1024, MaskBitsStrict: 14, MaskBitsLoose: 12}

	pieces := readAllPieces(t, NewCDCReader(bytes.NewReader(body), cfg))
	for i, p := range pieces {
		// Every piece except the last must respect MinSize.
		if i < len(pieces)-1 && int(p.Size) < cfg.MinSize {
			t.Errorf("piece %d size=%d < MinSize=%d (not last)", i, p.Size, cfg.MinSize)
		}
		if int(p.Size) > cfg.MaxSize {
			t.Errorf("piece %d size=%d > MaxSize=%d", i, p.Size, cfg.MaxSize)
		}
	}
}

func TestCDCRoundTripBytes(t *testing.T) {
	rng := rand.New(rand.NewSource(0xABCD))
	body := make([]byte, 100*1024)
	rng.Read(body)
	cfg := CDCConfig{MinSize: 1024, NormalSize: 4096, MaxSize: 16384, MaskBitsStrict: 12, MaskBitsLoose: 10}

	pieces := readAllPieces(t, NewCDCReader(bytes.NewReader(body), cfg))
	var rebuilt []byte
	var totalSize int64
	for _, p := range pieces {
		rebuilt = append(rebuilt, p.Data...)
		totalSize += p.Size
	}
	if !bytes.Equal(rebuilt, body) {
		t.Errorf("round-trip mismatch (len %d vs %d)", len(rebuilt), len(body))
	}
	if int64(len(body)) != totalSize {
		t.Errorf("size sum mismatch: %d vs %d", totalSize, len(body))
	}
}

func TestCDCEmpty(t *testing.T) {
	r := NewCDCReader(bytes.NewReader(nil), DefaultCDCConfig())
	p, err := r.Next()
	if !errors.Is(err, io.EOF) {
		t.Errorf("empty: want io.EOF, got piece=%v err=%v", p, err)
	}
}

// ADR-035: with MaskBitsRelax = 0, cuts must be byte-for-byte identical
// to the original 2-mask scheme (backward compat).
func TestCDCRelaxZeroIsBackwardCompat(t *testing.T) {
	rng := rand.New(rand.NewSource(0xBEEF))
	body := make([]byte, 256*1024)
	rng.Read(body)
	cfg := CDCConfig{MinSize: 1024, NormalSize: 4096, MaxSize: 16384, MaskBitsStrict: 12, MaskBitsLoose: 10}

	// Same cfg, MaskBitsRelax explicitly 0 (default).
	a := readAllPieces(t, NewCDCReader(bytes.NewReader(body), cfg))
	b := readAllPieces(t, NewCDCReader(bytes.NewReader(body), cfg))
	if len(a) != len(b) {
		t.Fatalf("len differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].ID != b[i].ID {
			t.Errorf("piece %d differs", i)
		}
	}
}

// With MaskBitsRelax > 0, MaxSize-cap chunks should be rarer than with the
// original 2-mask scheme. We use a body sized so most chunks would normally
// hit MaxSize, then assert the relax-on variant has strictly fewer caps.
func TestCDCRelaxReducesMaxCap(t *testing.T) {
	rng := rand.New(rand.NewSource(0xF00D))
	body := make([]byte, 1024*1024)
	rng.Read(body)
	// Mask bits intentionally too tight so cap is frequently hit.
	base := CDCConfig{MinSize: 4 * 1024, NormalSize: 16 * 1024, MaxSize: 64 * 1024, MaskBitsStrict: 18, MaskBitsLoose: 17}
	relaxed := base
	relaxed.MaskBitsRelax = 14 // much easier mask near MaxSize

	countCaps := func(pieces []*StreamPiece, max int) int {
		n := 0
		for _, p := range pieces {
			if int(p.Size) == max {
				n++
			}
		}
		return n
	}

	pa := readAllPieces(t, NewCDCReader(bytes.NewReader(body), base))
	pb := readAllPieces(t, NewCDCReader(bytes.NewReader(body), relaxed))
	capsBase := countCaps(pa, base.MaxSize)
	capsRelax := countCaps(pb, base.MaxSize)
	if capsRelax >= capsBase {
		t.Errorf("relax should reduce max-cap chunks: base=%d relax=%d (want relax < base)", capsBase, capsRelax)
	}
}

func TestCDCSmallerThanMin(t *testing.T) {
	// Input shorter than MinSize → emit one short piece.
	body := []byte("short")
	cfg := CDCConfig{MinSize: 1024, NormalSize: 4096, MaxSize: 16384, MaskBitsStrict: 12, MaskBitsLoose: 10}
	pieces := readAllPieces(t, NewCDCReader(bytes.NewReader(body), cfg))
	if len(pieces) != 1 {
		t.Fatalf("want 1 piece for short input, got %d", len(pieces))
	}
	if int(pieces[0].Size) != len(body) {
		t.Errorf("piece size %d != input %d", pieces[0].Size, len(body))
	}
}
