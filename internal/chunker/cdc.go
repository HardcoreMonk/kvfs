// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Content-defined chunking (FastCDC, ADR-018).
//
// Implements a streaming chunker that picks chunk boundaries based on content
// rather than fixed offsets. Same underlying byte sequence — even shifted by
// 1 byte at the start — produces (mostly) identical chunk IDs, which combined
// with the content-addressable storage (ADR-005) gives us shift-invariant
// deduplication.
//
// 비전공자용 해설
// ──────────────
// 고정 크기 chunking (ADR-011) 의 한계:
//
//   파일 F1 = AAAAAAAA...  (16 MiB 의 무작위 바이트)
//   파일 F2 = X + AAAAAA...  (1 byte 끼워 넣은 같은 파일)
//
// 4 MiB 고정으로 자르면 F1 과 F2 의 모든 chunk 가 서로 다른 sha256 → dedup 0%.
// chunk 경계가 파일 시작에서 4·8·12 MiB offset 에 박혀있기 때문.
//
// CDC 는 경계를 **content 가 결정**:
//
//   매 byte 에 대해 fp = (fp << 1) + GearTable[byte]
//   fp 가 마스크 조건을 만족하면 경계
//
// fp 는 전 byte 의 효과가 빠져나가는 rolling hash 이므로 같은 32-byte window
// 가 들어오면 같은 fp 가 나온다. 즉 F1 의 경계와 F2 의 경계가 1-byte 이후
// 부터 자연스럽게 정렬된다.
//
// 결과: F1 과 F2 의 첫 chunk 1-2 개만 다르고 나머지 ~99% 는 동일 chunk_id →
// CA 저장으로 dedup. backup 시스템 (Borg, Restic, rsync) 의 핵심 기법.
//
// FastCDC (Xia et al. 2016) 가 원조 Rabin 보다 ~3× 빠르고 cut 분포도 좋아서
// 산업 표준 (Restic, Borg, rdedup 모두 채택).
package chunker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	mathrand "math/rand"
)

// CDCConfig bundles FastCDC tuning parameters.
//
// Defaults are reasonable for object storage with ~4 MiB target chunks:
//   - MinSize 1 MiB · NormalSize 4 MiB · MaxSize 16 MiB
//   - MaskBitsStrict 22 (≈ 1/4M chance per byte before NormalSize)
//   - MaskBitsLoose  20 (4× easier hit after NormalSize)
type CDCConfig struct {
	MinSize        int    // 절대 그보다 작게 자르지 않음
	NormalSize     int    // target average; switches mask hardness
	MaxSize        int    // 절대 그보다 크게 자르지 않음
	MaskBitsStrict int    // bits in maskS (strict; pre-NormalSize)
	MaskBitsLoose  int    // bits in maskL (loose; post-NormalSize)
}

// DefaultCDCConfig returns parameters tuned for ~4 MiB average chunks
// (matches the default fixed chunkSize so apples-to-apples comparison).
func DefaultCDCConfig() CDCConfig {
	return CDCConfig{
		MinSize:        1 * 1024 * 1024,
		NormalSize:     4 * 1024 * 1024,
		MaxSize:        16 * 1024 * 1024,
		MaskBitsStrict: 22,
		MaskBitsLoose:  20,
	}
}

// gearTable is FastCDC's per-byte hash contribution. Deterministic seed so
// every kvfs instance picks identical cut points for identical content (this
// is the dedup contract — different instances must converge on the same chunk
// boundaries).
var gearTable = func() [256]uint64 {
	var t [256]uint64
	rng := mathrand.New(mathrand.NewSource(0xCAFEBABE))
	for i := 0; i < 256; i++ {
		t[i] = rng.Uint64()
	}
	return t
}()

// CDCReader is a streaming content-defined chunker. Same Next() shape as
// Reader (fixed-size) so handlePut can swap implementations transparently.
type CDCReader struct {
	src   io.Reader
	cfg   CDCConfig
	maskS uint64
	maskL uint64

	buf  []byte
	done bool
}

// NewCDCReader returns a streaming FastCDC chunker. cfg.MinSize ≤ 0 falls
// back to DefaultCDCConfig.
func NewCDCReader(src io.Reader, cfg CDCConfig) *CDCReader {
	if cfg.MinSize <= 0 || cfg.NormalSize <= 0 || cfg.MaxSize <= 0 {
		cfg = DefaultCDCConfig()
	}
	if cfg.MinSize > cfg.NormalSize || cfg.NormalSize > cfg.MaxSize {
		cfg = DefaultCDCConfig()
	}
	if cfg.MaskBitsStrict <= 0 {
		cfg.MaskBitsStrict = 22
	}
	if cfg.MaskBitsLoose <= 0 {
		cfg.MaskBitsLoose = 20
	}
	return &CDCReader{
		src:   src,
		cfg:   cfg,
		maskS: (uint64(1) << cfg.MaskBitsStrict) - 1,
		maskL: (uint64(1) << cfg.MaskBitsLoose) - 1,
	}
}

// Next returns the next CDC-cut chunk or io.EOF when the source is exhausted.
// Final chunk may be shorter than MinSize if the source ends early.
func (c *CDCReader) Next() (*StreamPiece, error) {
	if err := c.fillBuf(); err != nil {
		// fillBuf returns io.EOF only after marking done + emptying upstream.
		if len(c.buf) == 0 {
			return nil, err
		}
		// Otherwise we still have buffered bytes to emit as the final chunk.
	}
	if len(c.buf) == 0 {
		return nil, io.EOF
	}
	cut := c.cutpoint(c.buf)
	data := make([]byte, cut)
	copy(data, c.buf[:cut])
	c.buf = c.buf[cut:]
	sum := sha256.Sum256(data)
	return &StreamPiece{
		ID:   hex.EncodeToString(sum[:]),
		Data: data,
		Size: int64(cut),
	}, nil
}

// fillBuf reads from src until buf has at least MaxSize bytes or src is
// exhausted. Returns io.EOF when src is fully drained AND we've decided not
// to read more (so caller can switch to flush mode).
func (c *CDCReader) fillBuf() error {
	for !c.done && len(c.buf) < c.cfg.MaxSize {
		need := c.cfg.MaxSize - len(c.buf)
		tmp := make([]byte, need)
		n, err := c.src.Read(tmp)
		if n > 0 {
			c.buf = append(c.buf, tmp[:n]...)
		}
		if err == io.EOF {
			c.done = true
			return io.EOF
		}
		if err != nil {
			return fmt.Errorf("chunker(CDC): read: %w", err)
		}
		if n == 0 {
			// Pathological reader; avoid spin.
			break
		}
	}
	return nil
}

// cutpoint returns the offset (1-indexed length) of the next chunk in data.
// Implements FastCDC's three-region scan: skip up to MinSize, strict mask
// up to NormalSize, loose mask up to MaxSize, hard cap at MaxSize.
func (c *CDCReader) cutpoint(data []byte) int {
	n := len(data)
	if n <= c.cfg.MinSize {
		return n
	}
	limit := n
	if limit > c.cfg.MaxSize {
		limit = c.cfg.MaxSize
	}
	normal := c.cfg.NormalSize
	if normal > limit {
		normal = limit
	}

	var fp uint64
	// Phase 1: warm fp without testing (skip up to MinSize).
	for i := 0; i < c.cfg.MinSize; i++ {
		fp = (fp << 1) + gearTable[data[i]]
	}
	// Phase 2: MinSize..NormalSize with strict mask.
	for i := c.cfg.MinSize; i < normal; i++ {
		fp = (fp << 1) + gearTable[data[i]]
		if fp&c.maskS == 0 {
			return i + 1
		}
	}
	// Phase 3: NormalSize..limit with loose mask.
	for i := normal; i < limit; i++ {
		fp = (fp << 1) + gearTable[data[i]]
		if fp&c.maskL == 0 {
			return i + 1
		}
	}
	return limit
}
