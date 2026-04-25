// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package reedsolomon

import (
	"fmt"
	"testing"
)

// makeBenchData returns K data shards of shardSize bytes each.
func makeBenchData(k, shardSize int) [][]byte {
	out := make([][]byte, k)
	for i := 0; i < k; i++ {
		s := make([]byte, shardSize)
		for j := range s {
			s[j] = byte((i*131 + j*17 + 1) % 256)
		}
		out[i] = s
	}
	return out
}

// BenchmarkEncode measures GF(2^8) parity calculation throughput.
// Reports bytes/op = K * shardSize (the data volume processed per call).
func BenchmarkEncode(b *testing.B) {
	cases := []struct {
		name string
		k, m int
		size int
	}{
		{"4+2_4KiB", 4, 2, 4 * 1024},
		{"4+2_64KiB", 4, 2, 64 * 1024},
		{"6+3_64KiB", 6, 3, 64 * 1024},
		{"10+4_64KiB", 10, 4, 64 * 1024},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			enc, err := NewEncoder(tc.k, tc.m)
			if err != nil {
				b.Fatalf("NewEncoder: %v", err)
			}
			data := makeBenchData(tc.k, tc.size)
			b.ReportAllocs()
			b.SetBytes(int64(tc.k * tc.size))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := enc.Encode(data); err != nil {
					b.Fatalf("Encode: %v", err)
				}
			}
		})
	}
}

// BenchmarkReconstruct measures Gauss-Jordan invert + per-byte rebuild cost
// when M shards (the maximum tolerated) are missing.
func BenchmarkReconstruct(b *testing.B) {
	cases := []struct {
		name string
		k, m int
		size int
	}{
		{"4+2_4KiB", 4, 2, 4 * 1024},
		{"4+2_64KiB", 4, 2, 64 * 1024},
		{"6+3_64KiB", 6, 3, 64 * 1024},
		{"10+4_64KiB", 10, 4, 64 * 1024},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			enc, _ := NewEncoder(tc.k, tc.m)
			data := makeBenchData(tc.k, tc.size)
			parity, _ := enc.Encode(data)

			// Pre-build the "lost first M data shards" template; clone per-iter
			// so Reconstruct sees nil at positions 0..M-1.
			template := make([][]byte, tc.k+tc.m)
			for i := 0; i < tc.k; i++ {
				template[i] = data[i]
			}
			for i := 0; i < tc.m; i++ {
				template[tc.k+i] = parity[i]
			}
			b.ReportAllocs()
			b.SetBytes(int64(tc.k * tc.size))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				shards := make([][]byte, tc.k+tc.m)
				copy(shards, template)
				for j := 0; j < tc.m; j++ {
					shards[j] = nil
				}
				if err := enc.Reconstruct(shards); err != nil {
					b.Fatalf("Reconstruct: %v", err)
				}
			}
		})
	}
}

// BenchmarkGFMul measures the inner GF(2^8) multiplication primitive.
// Hot path of Encode/Reconstruct.
func BenchmarkGFMul(b *testing.B) {
	a := byte(0xab)
	c := byte(0xcd)
	var sink byte
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sink = gfMul(a, c)
	}
	_ = fmt.Sprint(sink)
}
