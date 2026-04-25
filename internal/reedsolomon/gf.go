// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Package reedsolomon implements Reed-Solomon erasure coding over GF(2^8).
//
// ──────────────────────────────────────────────────────────────────
// 비전공자용 해설 — GF(2^8) 이라는 "유한체"
// ──────────────────────────────────────────────────────────────────
//
// 왜 byte 산술 만으로는 부족한가
// ─────────────────────────────
// RS encoding 은 행렬 곱셈 (Vandermonde × data_shards = parity_shards) 이고
// decoding 은 행렬 역산이다. 일반 정수 산술 (uint8 mod 256) 로는:
//   - 곱셈 결과가 256 을 넘으면 잘려 정보 손실
//   - 나눗셈이 정확히 정의되지 않음 (예: 6 ÷ 4 = ?)
//   - 행렬 역산이 보장 안 됨
//
// 답: 256 개 원소 (0~255) 위에 **새로운 + - × ÷** 를 정의해서 모든 0 아닌 원소가
// 곱셈에 대한 역원을 갖도록 한다. 이런 구조를 **유한체 GF(2^8)** 이라 부른다.
//
// 핵심 트릭: bit-wise XOR 가 +, 그리고 곱셈은 다항식으로 정의
// ──────────────────────────────────────────────────────────────
//   각 byte 를 GF(2)[x] 의 다항식 (계수 0 또는 1) 로 본다.
//     예: 0b1010_1100 → x^7 + x^5 + x^3 + x^2
//
//   덧셈 = 계수별 mod-2 덧셈 = bit XOR
//     0b1010 + 0b0110 = 0b1100 (xor)
//
//   곱셈 = 다항식 곱셈 후 **primitive polynomial 로 mod**
//     primitive poly = x^8 + x^4 + x^3 + x^2 + 1 = 0b1_0001_1101 = 0x11d
//     (이 다항식은 GF(2)[x] 위에서 irreducible — AES 등에서 표준 사용)
//
// 빠른 곱셈 = log/exp 테이블
// ─────────────────────────
// 매번 다항식 곱셈 + mod 를 하면 느리다. 표준 트릭:
//   α = 2 (primitive element)
//   exp[i] = α^i for i in 0..254  (255 개 nonzero 원소를 모두 거침)
//   log[α^i] = i
//
//   mul(a, b) = exp[(log[a] + log[b]) mod 255]   if a, b ≠ 0
//   div(a, b) = exp[(log[a] - log[b] + 255) mod 255]  if a, b ≠ 0
//
// 0 곱셈은 0. 0 으로 나눗셈은 정의 안 됨 (panic).
//
// 이 패키지는 위 테이블을 init() 에서 256 entry × 2 = 512 byte 만 만든다.
// 그 위에 mul/div/add 함수가 ns 단위로 실행.
//
// 표준
// ────
// AES, Reed-Solomon, BCH, CD-ROM ECC 등 광범위 사용. 1968년 Reed-Solomon
// 원논문은 이 수학 위에 구현. RAID-6 의 Q parity 도 같은 GF(2^8).
package reedsolomon

// primitivePoly defines GF(2^8) used here: x^8 + x^4 + x^3 + x^2 + 1.
// Same polynomial used by AES, Reed-Solomon literature, and most software.
const primitivePoly = 0x11d

var (
	gfExp [512]byte // gfExp[i] = α^i (extended for wrap-around without mod)
	gfLog [256]byte // gfLog[α^i] = i; gfLog[0] is unused (0 has no log)
)

func init() {
	// Generate exp/log tables.
	x := byte(1)
	for i := 0; i < 255; i++ {
		gfExp[i] = x
		gfLog[x] = byte(i)
		// multiply x by α (= 2), with reduction by primitivePoly when overflow.
		hi := x & 0x80
		x <<= 1
		if hi != 0 {
			x ^= byte(primitivePoly & 0xff)
		}
	}
	// Duplicate the table so (log[a]+log[b]) up to 508 maps directly without mod.
	for i := 255; i < 512; i++ {
		gfExp[i] = gfExp[i-255]
	}
}

// gfAdd is GF(2^8) addition — bit-wise XOR. Same as subtraction.
func gfAdd(a, b byte) byte { return a ^ b }

// gfMul is GF(2^8) multiplication via the log/exp tables.
// Constant-time-ish: branch only on zero.
func gfMul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return gfExp[int(gfLog[a])+int(gfLog[b])]
}

// gfDiv is GF(2^8) division. Panics if b == 0 (no division by zero in any field).
func gfDiv(a, b byte) byte {
	if b == 0 {
		panic("reedsolomon: division by zero in GF(2^8)")
	}
	if a == 0 {
		return 0
	}
	// log[a] - log[b] in Z/255. Add 255 first to avoid negative.
	d := int(gfLog[a]) - int(gfLog[b]) + 255
	return gfExp[d]
}

// gfPow is GF(2^8) exponentiation: a ^ n.
// Used to build Vandermonde matrix rows.
func gfPow(a byte, n int) byte {
	if a == 0 {
		if n == 0 {
			return 1 // 0^0 conventionally 1
		}
		return 0
	}
	// (α^k)^n = α^(k*n)  in cyclic group of order 255
	k := int(gfLog[a])
	return gfExp[(k*n)%255]
}

// gfInv is the multiplicative inverse: 1 / a.
func gfInv(a byte) byte { return gfDiv(1, a) }
