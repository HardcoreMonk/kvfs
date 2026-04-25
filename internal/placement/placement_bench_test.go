// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package placement

import (
	"fmt"
	"testing"
)

// BenchmarkPick measures Pick latency at varying cluster sizes (N).
// Run: go test -bench=. -benchmem ./internal/placement
func BenchmarkPick(b *testing.B) {
	for _, n := range []int{3, 10, 50, 100, 500, 1000} {
		nodes := make([]Node, n)
		for i := 0; i < n; i++ {
			id := fmt.Sprintf("dn%d", i+1)
			nodes[i] = Node{ID: id, Addr: id + ":8080"}
		}
		p := New(nodes)
		b.Run(fmt.Sprintf("N=%d_R=3", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = p.Pick("chunk-fixed-id-for-bench", 3)
			}
		})
	}
}
