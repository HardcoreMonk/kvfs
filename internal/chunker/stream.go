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
	"sync/atomic"
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
//
// 명시적 cap (ADR-037)
// ────────────────────
// sync.Pool 자체는 GC 라운드마다 비워지지만 라운드 사이에는 무한 누적 가능.
// 동시 PUT spike 후 모든 reader 가 16 MiB 슬랩을 반납하면 메모리 압박.
// `poolCapBytes` 는 Pool 누적 cap 의 soft cap — putScratch 가 이미 누적이
// 한도를 넘었으면 슬랩을 drop (GC 회수에 맡김). atomic counter 로 lock-free.
var (
	scratchPool       sync.Pool
	scratchPoolBytes  atomic.Int64 // 풀에 들어있는 슬랩들의 cap 합계
	poolCapBytes      atomic.Int64 // 0 = unlimited (default for backward compat)
)

// SetPoolCap sets the upper bound (in bytes) on cumulative slab capacity
// kept in the chunker pool. 0 disables the cap (default). Caller would
// typically wire this from EDGE_CHUNKER_POOL_CAP_BYTES env var. Safe to
// call anytime; takes effect on subsequent putScratch calls.
func SetPoolCap(bytes int64) {
	if bytes < 0 {
		bytes = 0
	}
	poolCapBytes.Store(bytes)
}

// PoolStats returns (current cumulative slab bytes in pool, cap bytes).
// Used by the kvfs_chunker_pool_bytes metric. cap=0 means unlimited.
func PoolStats() (int64, int64) {
	return scratchPoolBytes.Load(), poolCapBytes.Load()
}

// getScratch retrieves a []byte with cap in [n, 2n] from the pool, or
// allocates a fresh one. Length is set to n.
//
// The 2× upper bound prevents a large slab (e.g. 16 MiB CDC MaxSize) from
// being handed to a small request (e.g. 4 MiB fixed chunk) and pinning
// extra memory for the request's lifetime. Slabs outside the band are
// dropped on the floor for GC.
func getScratch(n int) []byte {
	for i := 0; i < 2; i++ { // try up to twice; if the first slab is wrong-sized, give the pool one more chance.
		v := scratchPool.Get()
		if v == nil {
			break
		}
		b := v.([]byte)
		// Slab leaving the pool — debit the running total before checking fit
		// (matches the credit on Put). If the slab doesn't fit, we drop it
		// without re-Put, so the debit is the right book-keeping either way.
		scratchPoolBytes.Add(-int64(cap(b)))
		if cap(b) >= n && cap(b) <= 2*n {
			return b[:n]
		}
		// Wrong size band — let it fall to GC (already debited).
	}
	return make([]byte, n)
}

// putScratch returns the buffer to the pool, subject to the cumulative cap
// (ADR-037). When the pool is over its cap, the slab is dropped on the floor
// for GC instead of returned. cap=0 means unlimited.
func putScratch(b []byte) {
	if b == nil {
		return
	}
	c := int64(cap(b))
	limit := poolCapBytes.Load()
	if limit > 0 && scratchPoolBytes.Load()+c > limit {
		return // over cap; let GC reclaim.
	}
	scratchPoolBytes.Add(c)
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
