// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package chunker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReaderExactBoundary(t *testing.T) {
	body := bytes.Repeat([]byte("ABCD"), 8) // 32 bytes
	r := NewReader(bytes.NewReader(body), 16)
	var pieces []*StreamPiece
	for {
		p, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		pieces = append(pieces, p)
	}
	if len(pieces) != 2 {
		t.Fatalf("want 2 pieces (32/16), got %d", len(pieces))
	}
	for i, p := range pieces {
		if int(p.Size) != 16 {
			t.Errorf("piece %d size=%d want 16", i, p.Size)
		}
		exp := sha256.Sum256(body[i*16 : (i+1)*16])
		if p.ID != hex.EncodeToString(exp[:]) {
			t.Errorf("piece %d id mismatch", i)
		}
	}
}

func TestReaderShortTail(t *testing.T) {
	body := []byte("hello, kvfs streaming world!") // 28 bytes
	r := NewReader(bytes.NewReader(body), 10)
	sizes := []int64{}
	for {
		p, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, p.Size)
	}
	want := []int64{10, 10, 8}
	if len(sizes) != len(want) {
		t.Fatalf("piece count: got %v want %v", sizes, want)
	}
	for i, s := range sizes {
		if s != want[i] {
			t.Errorf("piece %d size %d want %d", i, s, want[i])
		}
	}
}

func TestReaderEmpty(t *testing.T) {
	r := NewReader(bytes.NewReader(nil), 16)
	p, err := r.Next()
	if !errors.Is(err, io.EOF) {
		t.Errorf("empty reader: want io.EOF, got piece=%v err=%v", p, err)
	}
}

func TestReaderRoundTripWithSplit(t *testing.T) {
	// Equivalence: streaming output must match Split() output for any input.
	body := []byte(strings.Repeat("kvfs-streaming-equivalence-check ", 200))
	chunkSize := 64

	splitPieces := Split(body, chunkSize)
	var streamPieces []*StreamPiece
	r := NewReader(bytes.NewReader(body), chunkSize)
	for {
		p, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		streamPieces = append(streamPieces, p)
	}
	if len(splitPieces) != len(streamPieces) {
		t.Fatalf("piece count mismatch: split=%d stream=%d", len(splitPieces), len(streamPieces))
	}
	for i := range splitPieces {
		if splitPieces[i].ID != streamPieces[i].ID {
			t.Errorf("piece %d id: split=%s stream=%s", i, splitPieces[i].ID, streamPieces[i].ID)
		}
		if !bytes.Equal(splitPieces[i].Data, streamPieces[i].Data) {
			t.Errorf("piece %d data mismatch", i)
		}
	}
}

func TestReaderDefaultChunkSize(t *testing.T) {
	r := NewReader(bytes.NewReader([]byte("x")), 0)
	if r.chunkSize != DefaultChunkSize {
		t.Errorf("chunkSize=%d want DefaultChunkSize=%d", r.chunkSize, DefaultChunkSize)
	}
}

// flakeyReader fails on the Nth read.
type flakeyReader struct {
	src       io.Reader
	failAfter int
	calls     int
}

func (f *flakeyReader) Read(p []byte) (int, error) {
	f.calls++
	if f.calls > f.failAfter {
		return 0, errors.New("simulated I/O error")
	}
	return f.src.Read(p)
}

func TestReaderPropagatesError(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 200)
	src := &flakeyReader{src: bytes.NewReader(body), failAfter: 1}
	r := NewReader(src, 16)
	for i := 0; i < 50; i++ {
		_, err := r.Next()
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			t.Errorf("got EOF before simulated error reached")
		}
		// Got an error wrapped — that's the expected exit path.
		if !strings.Contains(err.Error(), "simulated I/O error") {
			t.Errorf("err=%v want wrapped simulated I/O error", err)
		}
		return
	}
	t.Errorf("never got an error from flakey reader")
}
