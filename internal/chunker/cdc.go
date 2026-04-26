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
//   - MaskBitsRelax   0 (disabled by default; set >0 to enable 3rd "very
//     loose" region near MaxSize for tighter upper-tail size variance)
//
// 비전공자용 해설 (MaskBitsRelax)
// ─────────────────────────────
// 표준 2-mask FastCDC 는 NormalSize 통과 후 maskL (예: 20-bit) 로 컷.
// MaxSize 까지 끝내 컷이 안 나오면 hard cap → 그 chunk 가 "MaxSize" 길이.
// 결과적으로 length 분포 상단이 MaxSize 에 살짝 누적된다.
//
// Relax mask 는 (NormalSize + MaxSize) / 2 지점부터 더 쉬운 마스크 (예: 18-bit
// = 4× 더 자주 hit) 로 전환해 MaxSize 도달 직전에 거의 확실히 컷이 나도록 유도.
// 평균 chunk 크기는 NormalSize 근처로 유지되면서 MaxSize 캡 빈도만 줄어든다.
// dedup 효율은 약간 좋아지고 (size variance ↓), 동일 콘텐츠는 여전히 동일하게
// 컷되므로 shift-invariance 도 보존됨.
type CDCConfig struct {
	MinSize        int // 절대 그보다 작게 자르지 않음
	NormalSize     int // target average; switches mask hardness
	MaxSize        int // 절대 그보다 크게 자르지 않음
	MaskBitsStrict int // bits in maskS (strict; pre-NormalSize)
	MaskBitsLoose  int // bits in maskL (loose; post-NormalSize)
	MaskBitsRelax  int // bits in maskR (relax; post-(Normal+Max)/2). 0 = disabled.
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
	maskR uint64 // 0 if MaskBitsRelax disabled

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
	if cfg.MaskBitsRelax < 0 {
		cfg.MaskBitsRelax = 0
	}
	r := &CDCReader{
		src:   src,
		cfg:   cfg,
		maskS: (uint64(1) << cfg.MaskBitsStrict) - 1,
		maskL: (uint64(1) << cfg.MaskBitsLoose) - 1,
	}
	if cfg.MaskBitsRelax > 0 {
		r.maskR = (uint64(1) << cfg.MaskBitsRelax) - 1
	}
	return r
}

// Close returns the internal scratch buffer to the pool. Safe to call
// multiple times. Subsequent Next() calls return io.EOF.
func (c *CDCReader) Close() {
	if c.buf != nil {
		putScratch(c.buf)
		c.buf = nil
	}
	c.done = true
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
	return newStreamPiece(data), nil
}

// fillBuf reads from src until buf has at least MaxSize bytes or src is
// exhausted. Reads directly into c.buf's tail (no per-Read allocation).
// Returns io.EOF when src is fully drained.
func (c *CDCReader) fillBuf() error {
	if cap(c.buf) < c.cfg.MaxSize {
		nb := getScratch(c.cfg.MaxSize)
		copy(nb, c.buf)
		nb = nb[:len(c.buf)]
		if c.buf != nil {
			putScratch(c.buf)
		}
		c.buf = nb
	}
	for !c.done && len(c.buf) < c.cfg.MaxSize {
		end := len(c.buf)
		n, err := c.src.Read(c.buf[end:c.cfg.MaxSize])
		if n > 0 {
			c.buf = c.buf[:end+n]
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
// Implements FastCDC's region-scan with an optional 4th "relax" region:
//
//	Phase 1: 0..MinSize          → no test (warm fp)
//	Phase 2: MinSize..NormalSize → strict mask (maskS)
//	Phase 3: NormalSize..relax   → loose mask  (maskL)
//	Phase 4: relax..limit        → very-loose mask (maskR), only if enabled
//	hard cap at limit.
//
// "relax" boundary = (NormalSize + MaxSize) / 2 — past the average we want
// to bias more aggressively toward cutting before MaxSize cap. When
// MaskBitsRelax = 0 (default), Phases 3 and 4 collapse into one (= original
// behavior, byte-for-byte same cuts as before).
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
	// relax kicks in halfway between NormalSize and MaxSize. If MaskBitsRelax
	// is zero we set relax = limit so Phase 4 never executes.
	relax := limit
	if c.maskR != 0 {
		relax = (c.cfg.NormalSize + c.cfg.MaxSize) / 2
		if relax > limit {
			relax = limit
		}
		if relax < normal {
			relax = normal
		}
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
	// Phase 3: NormalSize..relax with loose mask.
	for i := normal; i < relax; i++ {
		fp = (fp << 1) + gearTable[data[i]]
		if fp&c.maskL == 0 {
			return i + 1
		}
	}
	// Phase 4: relax..limit with very-loose mask (skipped when maskR == 0,
	// since relax == limit in that case).
	for i := relax; i < limit; i++ {
		fp = (fp << 1) + gearTable[data[i]]
		if fp&c.maskR == 0 {
			return i + 1
		}
	}
	return limit
}
