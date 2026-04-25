// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package reedsolomon

import "testing"

func TestGF_AddIsXOR(t *testing.T) {
	if gfAdd(0xab, 0xcd) != 0xab^0xcd {
		t.Errorf("add != xor")
	}
	if gfAdd(0, 0x42) != 0x42 {
		t.Errorf("0 + a != a")
	}
	if gfAdd(0xff, 0xff) != 0 {
		t.Errorf("a + a != 0")
	}
}

func TestGF_MulIdentities(t *testing.T) {
	for a := 0; a < 256; a++ {
		if gfMul(byte(a), 0) != 0 {
			t.Errorf("%d * 0 != 0", a)
		}
		if gfMul(0, byte(a)) != 0 {
			t.Errorf("0 * %d != 0", a)
		}
		if gfMul(byte(a), 1) != byte(a) {
			t.Errorf("%d * 1 != %d (got %d)", a, a, gfMul(byte(a), 1))
		}
	}
}

func TestGF_MulCommutative(t *testing.T) {
	for a := 1; a < 256; a++ {
		for b := 1; b < 256; b++ {
			if gfMul(byte(a), byte(b)) != gfMul(byte(b), byte(a)) {
				t.Fatalf("%d * %d != %d * %d", a, b, b, a)
			}
		}
	}
}

func TestGF_DivIsInverseOfMul(t *testing.T) {
	for a := 0; a < 256; a++ {
		for b := 1; b < 256; b++ {
			ab := gfMul(byte(a), byte(b))
			if got := gfDiv(ab, byte(b)); got != byte(a) {
				t.Fatalf("(%d*%d)/%d = %d, want %d", a, b, b, got, a)
			}
		}
	}
}

func TestGF_DivByZeroPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on div by zero")
		}
	}()
	gfDiv(0x42, 0)
}

func TestGF_InverseRoundtrip(t *testing.T) {
	for a := 1; a < 256; a++ {
		inv := gfInv(byte(a))
		if gfMul(byte(a), inv) != 1 {
			t.Fatalf("%d * inv(%d) = %d, want 1", a, a, gfMul(byte(a), inv))
		}
	}
}

func TestGF_PowKnownValues(t *testing.T) {
	// α = 2 by construction
	if gfPow(2, 0) != 1 {
		t.Errorf("2^0 != 1")
	}
	if gfPow(2, 1) != 2 {
		t.Errorf("2^1 != 2")
	}
	// 2^7 in GF(2^8) = 128
	if gfPow(2, 7) != 128 {
		t.Errorf("2^7 = %d, want 128", gfPow(2, 7))
	}
	// 2^8 should reduce by primitivePoly: x^8 = x^4 + x^3 + x^2 + 1 = 0x1d
	if gfPow(2, 8) != 0x1d {
		t.Errorf("2^8 = %d, want 0x1d (29)", gfPow(2, 8))
	}
}

func TestGF_MultiplicativeOrderIs255(t *testing.T) {
	// α^255 must equal 1 (cyclic group of order 255).
	if gfPow(2, 255) != 1 {
		t.Errorf("α^255 = %d, want 1", gfPow(2, 255))
	}
	// And α^k != 1 for 0 < k < 255 (primitive element).
	for k := 1; k < 255; k++ {
		if gfPow(2, k) == 1 {
			t.Fatalf("α^%d == 1 (period less than 255 — α is not primitive)", k)
		}
	}
}

func TestGF_AllNonzeroAreReachable(t *testing.T) {
	// exp[0..254] should hit all 255 nonzero elements once.
	seen := make(map[byte]bool, 255)
	for i := 0; i < 255; i++ {
		v := gfExp[i]
		if v == 0 {
			t.Fatalf("exp[%d] = 0 (impossible)", i)
		}
		if seen[v] {
			t.Fatalf("exp[%d] = %d already seen — not a bijection", i, v)
		}
		seen[v] = true
	}
	if len(seen) != 255 {
		t.Errorf("seen %d distinct, want 255", len(seen))
	}
}

func TestGF_LogIsInverseOfExp(t *testing.T) {
	for i := 0; i < 255; i++ {
		v := gfExp[i]
		if int(gfLog[v]) != i {
			t.Fatalf("log[exp[%d]=%d] = %d, want %d", i, v, gfLog[v], i)
		}
	}
}
