// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package chunker

import (
	"fmt"
	"testing"
)

// BenchmarkSplit measures chunking + per-chunk sha256 throughput at varying
// body sizes against a fixed 4 MiB chunk size (default).
func BenchmarkSplit(b *testing.B) {
	const chunkSize = 4 * 1024 * 1024
	cases := []struct {
		name string
		size int
	}{
		{"1MiB", 1 << 20},
		{"4MiB", 4 << 20},
		{"16MiB", 16 << 20},
		{"64MiB", 64 << 20},
	}
	for _, tc := range cases {
		b.Run(fmt.Sprintf("body=%s_chunk=4MiB", tc.name), func(b *testing.B) {
			body := make([]byte, tc.size)
			// fill with a non-trivial pattern so sha256 doesn't optimize zeros
			for i := range body {
				body[i] = byte(i)
			}
			b.ReportAllocs()
			b.SetBytes(int64(tc.size))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = Split(body, chunkSize)
			}
		})
	}
}
