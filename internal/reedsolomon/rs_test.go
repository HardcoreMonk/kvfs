package reedsolomon

import (
	"bytes"
	"errors"
	"testing"
)

// helper: build K data shards of length n each; deterministic content
func makeData(k, n int) [][]byte {
	out := make([][]byte, k)
	for i := 0; i < k; i++ {
		s := make([]byte, n)
		for j := range s {
			s[j] = byte((i*131 + j*17 + 1) % 256)
		}
		out[i] = s
	}
	return out
}

func TestEncoder_New_BadParams(t *testing.T) {
	if _, err := NewEncoder(0, 2); err == nil {
		t.Error("expected error for K=0")
	}
	if _, err := NewEncoder(4, 0); err == nil {
		t.Error("expected error for M=0")
	}
	if _, err := NewEncoder(200, 200); err == nil {
		t.Error("expected error for K+M > 256")
	}
}

func TestEncoder_EncodeShape(t *testing.T) {
	e, err := NewEncoder(4, 2)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	data := makeData(4, 16)
	parity, err := e.Encode(data)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(parity) != 2 {
		t.Errorf("len(parity) = %d, want 2", len(parity))
	}
	for i, p := range parity {
		if len(p) != 16 {
			t.Errorf("parity[%d] size = %d, want 16", i, len(p))
		}
	}
}

func TestEncoder_EncodeRejectsWrongDataCount(t *testing.T) {
	e, _ := NewEncoder(4, 2)
	data := makeData(3, 16)
	if _, err := e.Encode(data); err == nil {
		t.Errorf("expected error for K=4 but 3 shards passed")
	}
}

func TestEncoder_EncodeRejectsMismatchedShardSizes(t *testing.T) {
	e, _ := NewEncoder(2, 1)
	data := [][]byte{make([]byte, 8), make([]byte, 16)}
	if _, err := e.Encode(data); err == nil {
		t.Errorf("expected error for mismatched shard sizes")
	}
}

func TestRoundTrip_NoFailures_Identity(t *testing.T) {
	e, _ := NewEncoder(4, 2)
	data := makeData(4, 32)
	parity, _ := e.Encode(data)
	shards := make([][]byte, 6)
	for i := 0; i < 4; i++ {
		shards[i] = data[i]
	}
	for i := 0; i < 2; i++ {
		shards[4+i] = parity[i]
	}
	if err := e.Reconstruct(shards); err != nil {
		t.Fatalf("Reconstruct (no failures): %v", err)
	}
	for i, d := range data {
		if !bytes.Equal(shards[i], d) {
			t.Fatalf("shard[%d] mismatch after no-failure reconstruct", i)
		}
	}
}

func TestRoundTrip_OneDataShardLost(t *testing.T) {
	e, _ := NewEncoder(4, 2)
	data := makeData(4, 32)
	parity, _ := e.Encode(data)
	shards := [][]byte{
		data[0], data[1], nil /* lost */, data[3],
		parity[0], parity[1],
	}
	expected := append([][]byte(nil), data[2])
	if err := e.Reconstruct(shards); err != nil {
		t.Fatalf("Reconstruct: %v", err)
	}
	if !bytes.Equal(shards[2], expected[0]) {
		t.Errorf("rebuilt data[2] does not match original")
	}
}

func TestRoundTrip_TwoShardsLost_AllCombinations(t *testing.T) {
	e, _ := NewEncoder(4, 2)
	data := makeData(4, 24)
	parity, _ := e.Encode(data)
	original := append(append([][]byte(nil), data...), parity...)
	indices := []int{0, 1, 2, 3, 4, 5}
	subs := combinations(indices, 2)
	for _, lost := range subs {
		shards := make([][]byte, 6)
		for i, s := range original {
			shards[i] = append([]byte(nil), s...)
		}
		for _, idx := range lost {
			shards[idx] = nil
		}
		if err := e.Reconstruct(shards); err != nil {
			t.Fatalf("Reconstruct lost=%v: %v", lost, err)
		}
		// data shards (positions 0..3) must match original data
		for i := 0; i < 4; i++ {
			if !bytes.Equal(shards[i], data[i]) {
				t.Fatalf("lost=%v: data[%d] not recovered", lost, i)
			}
		}
	}
}

func TestRoundTrip_ThreeShardsLost_FailsCleanly(t *testing.T) {
	e, _ := NewEncoder(4, 2)
	data := makeData(4, 24)
	parity, _ := e.Encode(data)
	shards := [][]byte{
		data[0], nil, nil, nil, // 3 data lost
		parity[0], parity[1],
	}
	err := e.Reconstruct(shards)
	if !errors.Is(err, ErrTooFewShards) {
		t.Errorf("expected ErrTooFewShards, got %v", err)
	}
}

func TestRoundTrip_AllParityLost(t *testing.T) {
	// All data survives → no work needed; Reconstruct should succeed without
	// touching data.
	e, _ := NewEncoder(4, 2)
	data := makeData(4, 16)
	shards := [][]byte{data[0], data[1], data[2], data[3], nil, nil}
	if err := e.Reconstruct(shards); err != nil {
		t.Fatalf("Reconstruct: %v", err)
	}
	for i := 0; i < 4; i++ {
		if !bytes.Equal(shards[i], data[i]) {
			t.Errorf("data[%d] disturbed", i)
		}
	}
}

func TestRoundTrip_AllDataLost_RebuildFromParityPlusOneData(t *testing.T) {
	// Lose 2 data shards (max we can with M=2).
	e, _ := NewEncoder(3, 2)
	data := makeData(3, 16)
	parity, _ := e.Encode(data)
	shards := [][]byte{nil, nil, data[2], parity[0], parity[1]}
	if err := e.Reconstruct(shards); err != nil {
		t.Fatalf("Reconstruct: %v", err)
	}
	for i := 0; i < 3; i++ {
		if !bytes.Equal(shards[i], data[i]) {
			t.Errorf("data[%d] not recovered", i)
		}
	}
}

func TestEncoder_Various_K_M(t *testing.T) {
	cases := []struct{ k, m int }{
		{2, 1}, {3, 2}, {4, 2}, {6, 3}, {10, 4}, {16, 4},
	}
	for _, tc := range cases {
		e, err := NewEncoder(tc.k, tc.m)
		if err != nil {
			t.Errorf("(%d,%d): NewEncoder: %v", tc.k, tc.m, err)
			continue
		}
		data := makeData(tc.k, 64)
		parity, err := e.Encode(data)
		if err != nil {
			t.Errorf("(%d,%d): Encode: %v", tc.k, tc.m, err)
			continue
		}
		// Lose the last m shards (could be data, could be parity).
		shards := make([][]byte, tc.k+tc.m)
		for i := 0; i < tc.k; i++ {
			shards[i] = data[i]
		}
		for i := 0; i < tc.m; i++ {
			shards[tc.k+i] = parity[i]
		}
		// Lose the first M shards (data shards).
		for i := 0; i < tc.m; i++ {
			shards[i] = nil
		}
		if err := e.Reconstruct(shards); err != nil {
			t.Fatalf("(%d,%d): Reconstruct: %v", tc.k, tc.m, err)
		}
		for i := 0; i < tc.k; i++ {
			if !bytes.Equal(shards[i], data[i]) {
				t.Fatalf("(%d,%d): data[%d] not recovered", tc.k, tc.m, i)
			}
		}
	}
}

func TestEncode_DeterministicOutput(t *testing.T) {
	// Same data → same parity (encoding is a pure function).
	e, _ := NewEncoder(4, 2)
	data := makeData(4, 32)
	a, _ := e.Encode(data)
	b, _ := e.Encode(data)
	for i := 0; i < 2; i++ {
		if !bytes.Equal(a[i], b[i]) {
			t.Errorf("parity[%d] not deterministic", i)
		}
	}
}
