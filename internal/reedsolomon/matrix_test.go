// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package reedsolomon

import "testing"

func TestMatrix_Identity(t *testing.T) {
	I := identity(4)
	for r := 0; r < 4; r++ {
		for c := 0; c < 4; c++ {
			want := byte(0)
			if r == c {
				want = 1
			}
			if I.get(r, c) != want {
				t.Errorf("I[%d][%d]=%d, want %d", r, c, I.get(r, c), want)
			}
		}
	}
}

func TestMatrix_MultiplyByIdentity(t *testing.T) {
	I := identity(3)
	M := newMatrix(3, 3)
	for r := 0; r < 3; r++ {
		for c := 0; c < 3; c++ {
			M.set(r, c, byte(r*3+c+1))
		}
	}
	out := M.multiply(I)
	for i := 0; i < 9; i++ {
		if out.data[i] != M.data[i] {
			t.Fatalf("M*I[%d]=%d, want %d", i, out.data[i], M.data[i])
		}
	}
}

func TestMatrix_InvertRoundTrip(t *testing.T) {
	// Use a known invertible matrix: [[1,2],[3,4]]
	M := newMatrix(2, 2)
	M.set(0, 0, 1)
	M.set(0, 1, 2)
	M.set(1, 0, 3)
	M.set(1, 1, 4)

	inv, err := M.invert()
	if err != nil {
		t.Fatalf("invert: %v", err)
	}
	prod := M.multiply(inv)
	I := identity(2)
	for i := 0; i < 4; i++ {
		if prod.data[i] != I.data[i] {
			t.Errorf("M * M^-1 != I at index %d: got %d want %d", i, prod.data[i], I.data[i])
		}
	}
}

func TestMatrix_InvertSingularReturnsError(t *testing.T) {
	// Zero matrix is singular.
	M := newMatrix(3, 3)
	if _, err := M.invert(); err == nil {
		t.Errorf("expected error for singular matrix")
	}
}

func TestMatrix_InvertNonSquarePanicsOrErrors(t *testing.T) {
	M := newMatrix(2, 3)
	if _, err := M.invert(); err == nil {
		t.Errorf("expected error for non-square")
	}
}

func TestMatrix_SystematicEncoding_TopIsIdentity(t *testing.T) {
	for _, p := range [][2]int{{4, 2}, {6, 3}, {3, 1}, {10, 4}} {
		k, m := p[0], p[1]
		E, err := makeSystematicEncoding(k, m)
		if err != nil {
			t.Fatalf("makeSystematicEncoding(%d,%d): %v", k, m, err)
		}
		if E.rows != k+m || E.cols != k {
			t.Errorf("(%d,%d): shape = %dx%d, want %dx%d", k, m, E.rows, E.cols, k+m, k)
		}
		// top K rows must be identity
		for r := 0; r < k; r++ {
			for c := 0; c < k; c++ {
				want := byte(0)
				if r == c {
					want = 1
				}
				if E.get(r, c) != want {
					t.Errorf("(%d,%d) E[%d][%d]=%d, want %d (top should be I)", k, m, r, c, E.get(r, c), want)
				}
			}
		}
	}
}

func TestMatrix_SystematicEncoding_AnyKRowsInvertible(t *testing.T) {
	// For (K=4, M=2) the encoding matrix is 6x4. Any 4 of 6 rows must be
	// linearly independent (invertible 4x4 sub-matrix). This is the MDS
	// (maximum distance separable) property.
	k, m := 4, 2
	E, err := makeSystematicEncoding(k, m)
	if err != nil {
		t.Fatalf("makeSystematicEncoding: %v", err)
	}
	// Try every subset of size k from k+m rows.
	rows := []int{0, 1, 2, 3, 4, 5}
	subsets := combinations(rows, k)
	for _, subset := range subsets {
		sub := E.subMatrix(subset)
		if _, err := sub.invert(); err != nil {
			t.Errorf("rows %v not invertible: %v", subset, err)
		}
	}
}

// combinations returns all k-subsets of the input set. Iterative.
func combinations(set []int, k int) [][]int {
	var out [][]int
	n := len(set)
	if k > n {
		return out
	}
	idx := make([]int, k)
	for i := range idx {
		idx[i] = i
	}
	for {
		picked := make([]int, k)
		for i, j := range idx {
			picked[i] = set[j]
		}
		out = append(out, picked)
		// advance
		i := k - 1
		for ; i >= 0; i-- {
			if idx[i] < n-k+i {
				break
			}
		}
		if i < 0 {
			return out
		}
		idx[i]++
		for j := i + 1; j < k; j++ {
			idx[j] = idx[j-1] + 1
		}
	}
}

func TestMatrix_SwapRows(t *testing.T) {
	M := newMatrix(3, 2)
	M.set(0, 0, 1)
	M.set(0, 1, 2)
	M.set(1, 0, 3)
	M.set(1, 1, 4)
	swapRows(M, 0, 1)
	if M.get(0, 0) != 3 || M.get(0, 1) != 4 || M.get(1, 0) != 1 || M.get(1, 1) != 2 {
		t.Errorf("swap failed: %v", M.data)
	}
}
