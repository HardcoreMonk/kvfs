// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package reedsolomon

import (
	"errors"
	"fmt"
)

// matrix is a row-major 2D byte matrix over GF(2^8).
//
// rows × cols of bytes. data[r*cols + c] = M[r][c].
//
// 모든 산술은 gf* 함수 사용 (덧셈 = XOR, 곱셈 = log/exp 테이블).
type matrix struct {
	rows, cols int
	data       []byte
}

func newMatrix(rows, cols int) *matrix {
	return &matrix{rows: rows, cols: cols, data: make([]byte, rows*cols)}
}

func (m *matrix) get(r, c int) byte    { return m.data[r*m.cols+c] }
func (m *matrix) set(r, c int, v byte) { m.data[r*m.cols+c] = v }

// row returns a copy of row r.
func (m *matrix) row(r int) []byte {
	out := make([]byte, m.cols)
	copy(out, m.data[r*m.cols:(r+1)*m.cols])
	return out
}

// identity returns an n×n identity matrix.
func identity(n int) *matrix {
	m := newMatrix(n, n)
	for i := 0; i < n; i++ {
		m.set(i, i, 1)
	}
	return m
}

// vandermonde returns an rows×cols Vandermonde matrix:
//
//	V[i][j] = i ^ j   (in GF(2^8))
//
// rows >= cols required for the typical use (encoding matrix).
//
// Note: a "raw" Vandermonde over GF(2^8) is NOT guaranteed to have all sub-matrices
// invertible for all rows/cols. We address this by row-reducing into a systematic
// form (top cols rows = identity), which guarantees the bottom (rows-cols) rows
// (parity rows) yield invertible sub-matrices when picked alongside any subset of
// the identity rows.
func vandermonde(rows, cols int) *matrix {
	v := newMatrix(rows, cols)
	for r := 0; r < rows; r++ {
		for c := 0; c < cols; c++ {
			v.set(r, c, gfPow(byte(r), c))
		}
	}
	return v
}

// multiply returns m × other.
// m is (a × b), other is (b × c), result is (a × c).
func (m *matrix) multiply(other *matrix) *matrix {
	if m.cols != other.rows {
		panic(fmt.Sprintf("matrix dim mismatch: %d×%d * %d×%d", m.rows, m.cols, other.rows, other.cols))
	}
	out := newMatrix(m.rows, other.cols)
	for r := 0; r < out.rows; r++ {
		for c := 0; c < out.cols; c++ {
			var sum byte
			for k := 0; k < m.cols; k++ {
				sum = gfAdd(sum, gfMul(m.get(r, k), other.get(k, c)))
			}
			out.set(r, c, sum)
		}
	}
	return out
}

// invert returns the inverse of a square matrix via Gauss-Jordan elimination
// over GF(2^8).
//
// Panics if not square. Returns error if singular (no inverse exists).
//
// Algorithm:
//  1. Augment with identity: [M | I] (n × 2n)
//  2. For each column c in [0, n):
//     - Find pivot row p with non-zero in column c. If none, singular.
//     - Swap row p with row c if needed.
//     - Scale row c so M[c][c] = 1.
//     - Eliminate column c in all other rows by subtracting a multiple of row c.
//  3. Right half of augmented matrix is now the inverse.
func (m *matrix) invert() (*matrix, error) {
	if m.rows != m.cols {
		return nil, fmt.Errorf("matrix.invert: not square (%dx%d)", m.rows, m.cols)
	}
	n := m.rows
	// Build augmented [M | I]
	aug := newMatrix(n, 2*n)
	for r := 0; r < n; r++ {
		for c := 0; c < n; c++ {
			aug.set(r, c, m.get(r, c))
		}
		aug.set(r, n+r, 1)
	}

	// Gauss-Jordan
	for c := 0; c < n; c++ {
		// Find pivot
		if aug.get(c, c) == 0 {
			pivot := -1
			for r := c + 1; r < n; r++ {
				if aug.get(r, c) != 0 {
					pivot = r
					break
				}
			}
			if pivot < 0 {
				return nil, errSingular
			}
			swapRows(aug, c, pivot)
		}
		// Scale row c so diagonal = 1
		inv := gfInv(aug.get(c, c))
		for cc := 0; cc < 2*n; cc++ {
			aug.set(c, cc, gfMul(aug.get(c, cc), inv))
		}
		// Eliminate column c in all other rows
		for r := 0; r < n; r++ {
			if r == c {
				continue
			}
			factor := aug.get(r, c)
			if factor == 0 {
				continue
			}
			for cc := 0; cc < 2*n; cc++ {
				aug.set(r, cc, gfAdd(aug.get(r, cc), gfMul(factor, aug.get(c, cc))))
			}
		}
	}

	// Extract right half
	out := newMatrix(n, n)
	for r := 0; r < n; r++ {
		for c := 0; c < n; c++ {
			out.set(r, c, aug.get(r, n+c))
		}
	}
	return out, nil
}

func swapRows(m *matrix, a, b int) {
	if a == b {
		return
	}
	rowA := m.data[a*m.cols : (a+1)*m.cols]
	rowB := m.data[b*m.cols : (b+1)*m.cols]
	for i := range rowA {
		rowA[i], rowB[i] = rowB[i], rowA[i]
	}
}

// subMatrix returns the matrix consisting of the listed row indices.
// Used by RS Reconstruct: pick K surviving rows from the encoding matrix.
func (m *matrix) subMatrix(rowIdx []int) *matrix {
	out := newMatrix(len(rowIdx), m.cols)
	for i, r := range rowIdx {
		copy(out.data[i*out.cols:(i+1)*out.cols], m.data[r*m.cols:(r+1)*m.cols])
	}
	return out
}

// makeSystematicEncoding builds a (K+M) × K encoding matrix where:
//   - top K rows form the identity (so first K output shards == K input data shards)
//   - bottom M rows are a parity matrix derived from a Vandermonde, processed so
//     that any K rows (any mix of identity and parity) yield an invertible K×K
//     submatrix.
//
// Approach: build raw Vandermonde V of shape (K+M)×K. Top K rows V[0..K) form a
// K×K matrix (call it T). Compute T^-1. Multiply the entire V on the right by
// T^-1: V * T^-1. This makes the top K rows the identity (since T * T^-1 = I)
// while preserving the row-rank properties. The result is the systematic
// encoding matrix.
func makeSystematicEncoding(k, m int) (*matrix, error) {
	v := vandermonde(k+m, k)
	// Top K block T
	t := newMatrix(k, k)
	for r := 0; r < k; r++ {
		for c := 0; c < k; c++ {
			t.set(r, c, v.get(r, c))
		}
	}
	tInv, err := t.invert()
	if err != nil {
		return nil, fmt.Errorf("makeSystematicEncoding: invert top: %w", err)
	}
	return v.multiply(tInv), nil
}

var errSingular = errors.New("matrix is singular (no inverse over GF(2^8))")
