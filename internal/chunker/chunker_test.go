package chunker

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
)

func TestSplit_EmptyReturnsNil(t *testing.T) {
	out := Split(nil, 64)
	if out != nil {
		t.Errorf("Split(nil) = %v, want nil", out)
	}
}

func TestSplit_ExactlyOneChunk(t *testing.T) {
	body := []byte("hello, kvfs")
	out := Split(body, 1024)
	if len(out) != 1 {
		t.Fatalf("len(out) = %d, want 1", len(out))
	}
	if string(out[0].Data) != string(body) {
		t.Errorf("chunk data = %q, want %q", out[0].Data, body)
	}
	if out[0].Size != int64(len(body)) {
		t.Errorf("Size = %d, want %d", out[0].Size, len(body))
	}
	expSum := sha256.Sum256(body)
	if out[0].ID != hex.EncodeToString(expSum[:]) {
		t.Errorf("ID mismatch")
	}
}

func TestSplit_MultipleEvenChunks(t *testing.T) {
	body := make([]byte, 200)
	for i := range body {
		body[i] = byte(i % 256)
	}
	out := Split(body, 50)
	if len(out) != 4 {
		t.Fatalf("len(out) = %d, want 4 (200 / 50)", len(out))
	}
	for i, c := range out {
		if c.Size != 50 {
			t.Errorf("chunk %d size = %d, want 50", i, c.Size)
		}
	}
}

func TestSplit_MultipleWithRemainder(t *testing.T) {
	body := make([]byte, 250)
	out := Split(body, 100)
	if len(out) != 3 {
		t.Fatalf("len(out) = %d, want 3", len(out))
	}
	if out[0].Size != 100 || out[1].Size != 100 || out[2].Size != 50 {
		t.Errorf("sizes = [%d %d %d], want [100 100 50]",
			out[0].Size, out[1].Size, out[2].Size)
	}
}

func TestSplit_DedupSameContentSameID(t *testing.T) {
	body := []byte("repeat-me-please")
	a := Split(body, 1024)
	b := Split(body, 1024)
	if a[0].ID != b[0].ID {
		t.Errorf("identical body produced different IDs")
	}
}

func TestSplit_PanicsOnZeroChunkSize(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on chunkSize=0")
		}
	}()
	Split([]byte("x"), 0)
}

func TestJoin_HappyPathReassembles(t *testing.T) {
	body := []byte("the quick brown fox jumps over the lazy dog")
	chunks := Split(body, 10)
	specs := make([]JoinSpec, len(chunks))
	store := map[string][]byte{}
	for i, c := range chunks {
		specs[i] = JoinSpec{ChunkID: c.ID, Size: c.Size}
		store[c.ID] = c.Data
	}
	got, err := Join(specs, int64(len(body)), func(id string) ([]byte, error) {
		return store[id], nil
	})
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("rejoined = %q, want %q", got, body)
	}
}

func TestJoin_WrongHashRejected(t *testing.T) {
	specs := []JoinSpec{{ChunkID: "deadbeef", Size: 5}}
	_, err := Join(specs, 5, func(id string) ([]byte, error) {
		return []byte("hello"), nil // sha256 != "deadbeef"
	})
	if err == nil {
		t.Errorf("expected integrity error, got nil")
	}
}

func TestJoin_WrongSizeRejected(t *testing.T) {
	body := []byte("abc")
	sum := sha256.Sum256(body)
	id := hex.EncodeToString(sum[:])
	specs := []JoinSpec{{ChunkID: id, Size: 99}}
	_, err := Join(specs, 99, func(_ string) ([]byte, error) {
		return body, nil
	})
	if err == nil {
		t.Errorf("expected size mismatch error, got nil")
	}
}

func TestJoin_FetchErrorPropagates(t *testing.T) {
	specs := []JoinSpec{{ChunkID: "x", Size: 1}}
	wantErr := errors.New("DN dead")
	_, err := Join(specs, 1, func(_ string) ([]byte, error) { return nil, wantErr })
	if err == nil || !contains(err.Error(), "DN dead") {
		t.Errorf("expected fetch error to propagate, got %v", err)
	}
}

func TestJoin_TotalSizeMismatchRejected(t *testing.T) {
	body := []byte("abc")
	sum := sha256.Sum256(body)
	id := hex.EncodeToString(sum[:])
	specs := []JoinSpec{{ChunkID: id, Size: 3}}
	_, err := Join(specs, 99, func(_ string) ([]byte, error) { return body, nil })
	if err == nil {
		t.Errorf("expected total size error, got nil")
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestSplit_HugeChunkSizeOK(t *testing.T) {
	body := []byte("small")
	out := Split(body, 1024*1024)
	if len(out) != 1 {
		t.Errorf("expected 1 chunk for body smaller than chunk size, got %d", len(out))
	}
}

// Sanity: split + join round-trip on random-ish data
func TestSplitJoin_RoundTrip(t *testing.T) {
	body := make([]byte, 1024)
	for i := range body {
		body[i] = byte((i*31 + 7) % 256)
	}
	chunks := Split(body, 100)
	store := map[string][]byte{}
	specs := make([]JoinSpec, len(chunks))
	for i, c := range chunks {
		store[c.ID] = c.Data
		specs[i] = JoinSpec{ChunkID: c.ID, Size: c.Size}
	}
	got, err := Join(specs, int64(len(body)), func(id string) ([]byte, error) {
		d, ok := store[id]
		if !ok {
			return nil, fmt.Errorf("missing %s", id)
		}
		return d, nil
	})
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if len(got) != len(body) {
		t.Errorf("size mismatch: %d vs %d", len(got), len(body))
	}
	for i := range body {
		if got[i] != body[i] {
			t.Fatalf("byte %d mismatch", i)
		}
	}
}
