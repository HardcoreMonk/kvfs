// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Streaming chunker (ADR-017): produces one content-addressable chunk at a
// time from an io.Reader so the edge handler never needs to buffer the entire
// object. Memory footprint is bounded by chunkSize regardless of body size.
//
// 비전공자용 해설
// ──────────────
// 기존 Split([]byte, n) 은 입력이 이미 메모리에 다 있다고 가정. 1 GB 객체 PUT
// 이 들어오면 1 GB 가 edge RAM 에 머문다. ADR-017 streaming 의 목표는 이를
// "한 번에 ChunkSize 만큼만 들고 있다가 DN 으로 보내고 버린다" 로 바꾸는 것.
//
// Reader 사용 패턴:
//
//	r := chunker.NewReader(req.Body, s.ChunkSize)
//	for {
//	    p, err := r.Next()
//	    if errors.Is(err, io.EOF) { break }
//	    if err != nil { return err }
//	    coord.WriteChunk(ctx, p.ID, p.Data)
//	}
//
// 마지막 chunk 가 ChunkSize 보다 작아도 OK (io.ErrUnexpectedEOF 를 정상
// 종료로 처리). 0-byte 입력은 첫 호출이 곧바로 io.EOF.
package chunker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sync"
)

// scratchPool recycles per-Reader scratch buffers across PUTs so back-to-back
// uploads don't churn the allocator. Buffers are bucketed by size at the call
// site (small files vs default-size vs CDC-MaxSize), and entries are returned
// only when callers invoke Close — Next() must keep its buffer alive between
// calls.
//
// 비전공자용 해설
// ──────────────
// 매 PUT 마다 NewReader → make([]byte, chunkSize) → 한 번 쓰고 GC 대상.
// 4 MiB 짜리 슬라이스가 초당 수백 개 만들어지면 GC 압력 + RSS 흔들림.
// sync.Pool 은 "재사용 가능한 슬랩" 을 keep — Get 해서 쓰고 Put 으로 반납,
// GC 가 한가할 때만 비운다. 핵심: cap(buf) 가 같아야 의미 있는 재사용이 됨.
var scratchPool sync.Pool

// getScratch retrieves a []byte with cap >= n from the pool, or allocates a
// fresh one. Length is set to n.
func getScratch(n int) []byte {
	if v := scratchPool.Get(); v != nil {
		b := v.([]byte)
		if cap(b) >= n {
			return b[:n]
		}
		// Pool returned a too-small buf (different reader sized it); drop it.
	}
	return make([]byte, n)
}

// putScratch returns the buffer to the pool. Caller must not retain b.
func putScratch(b []byte) {
	if b == nil {
		return
	}
	scratchPool.Put(b[:0]) //nolint:staticcheck // pool of slices is fine; we restore len at Get.
}

// StreamPiece is a single chunk pulled from a streaming source.
//
// Data is owned by the caller after Next returns — readers internally
// allocate fresh storage per piece so the caller can safely retain the
// slice while the next Next() runs concurrently.
type StreamPiece struct {
	ID   string // hex(sha256(Data))
	Data []byte
	Size int64
}

// newStreamPiece is the shared constructor used by both fixed (Reader) and
// content-defined (CDCReader) chunkers. Callers pass an owned []byte (data
// is NOT defensively copied here — readers do that before calling).
func newStreamPiece(data []byte) *StreamPiece {
	sum := sha256.Sum256(data)
	return &StreamPiece{
		ID:   hex.EncodeToString(sum[:]),
		Data: data,
		Size: int64(len(data)),
	}
}

// Reader produces fixed-size content-addressable chunks from an io.Reader.
type Reader struct {
	src       io.Reader
	chunkSize int
	buf       []byte
	done      bool
}

// NewReader returns a streaming chunker. chunkSize ≤ 0 falls back to
// DefaultChunkSize. The internal scratch buffer is chunkSize bytes (drawn
// from a sync.Pool when possible); pieces returned by Next() are
// independently allocated copies. Call Close() when done to return the
// scratch buffer to the pool.
func NewReader(src io.Reader, chunkSize int) *Reader {
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}
	return &Reader{
		src:       src,
		chunkSize: chunkSize,
		buf:       getScratch(chunkSize),
	}
}

// Close returns the scratch buffer to the pool. Safe to call multiple times.
// Subsequent Next() calls return io.EOF.
func (r *Reader) Close() {
	if r.buf != nil {
		putScratch(r.buf)
		r.buf = nil
	}
	r.done = true
}

// Next returns the next chunk or io.EOF when the source is exhausted.
// Final chunks shorter than chunkSize are returned; callers do NOT need to
// special-case the tail.
func (r *Reader) Next() (*StreamPiece, error) {
	if r.done {
		return nil, io.EOF
	}
	n, err := io.ReadFull(r.src, r.buf)
	switch err {
	case nil:
		// Full chunk read; more data may follow.
	case io.EOF:
		// Stream ended exactly on a chunk boundary; we read 0 bytes this round.
		r.done = true
		return nil, io.EOF
	case io.ErrUnexpectedEOF:
		// Final short chunk: n bytes valid, no more after this.
		r.done = true
	default:
		return nil, fmt.Errorf("chunker: read: %w", err)
	}
	if n == 0 {
		return nil, io.EOF
	}
	// Defensive copy: caller may keep the slice past the next Next() call.
	data := make([]byte, n)
	copy(data, r.buf[:n])
	return newStreamPiece(data), nil
}
