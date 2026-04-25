// Package chunker splits objects into fixed-size content-addressable chunks
// and joins chunks back into objects.
//
// ──────────────────────────────────────────────────────────────────
// 비전공자용 해설
// ──────────────────────────────────────────────────────────────────
//
// 풀어야 할 문제 (ADR-011)
// ────────────────────────
// 1 GiB 객체를 통째로 다루면:
//   - edge RAM 폭발 (전체 body 로드 + sha256)
//   - placement 가 1 chunk 만 결정 → R 개 DN 에 트래픽 집중
//   - 99% 일치 + 1% 다른 두 객체 → sha256 완전히 다름 → dedup 0
//   - DN 1개 다운 시 1 GiB 통째로 재복사
//
// 답: 객체를 고정 크기 N 바이트 단위로 자른다. 마지막 청크는 잔여 크기.
//
//   body 크기 250 KiB, chunk_size 64 KiB
//     → chunks: [64KiB, 64KiB, 64KiB, 58KiB]
//     → chunk_id 4개: 각각 sha256(chunk_data)
//
// 각 chunk_id 가 placement.Pick 에 독립적으로 들어가 → R 개 DN 으로 fanout.
// 같은 객체의 청크들이 서로 다른 DN 집합에 흩뿌려질 가능성 높음.
//
// 알고리즘 단순함
// ──────────────
// 고정 크기. content-defined chunking (CDC) 같은 rolling hash 없음.
// 트레이드오프 인정: 정렬된 데이터 (예: tar 의 일부 변경) 의 dedup 효율은
// 약하지만 코드는 ~30 LOC 면 끝. CDC 는 ADR-018 후보.
package chunker

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// MinChunkSize is the smallest allowed chunk size in bytes.
// Below this the metadata overhead becomes silly (1 GiB / 1 KiB = 1M chunks).
const MinChunkSize = 1

// DefaultChunkSize is the default if EDGE_CHUNK_SIZE env var is unset.
// 4 MiB matches S3 multipart minimum and IPFS's common default.
const DefaultChunkSize = 4 * 1024 * 1024

// Chunk is a single piece of an object after splitting.
type Chunk struct {
	Data []byte // chunk bytes
	ID   string // hex(sha256(Data))
	Size int64  // == len(Data); convenience for callers
}

// Split divides body into Chunks of at most chunkSize bytes each.
// The final chunk may be smaller. Empty body returns nil.
//
// chunkSize must be >= MinChunkSize (panics if 0 or negative).
//
// Each Chunk's ID is hex(sha256(Data)) — content-addressable, identical bytes
// always yield identical chunk_id (dedup precondition, ADR-005).
func Split(body []byte, chunkSize int) []Chunk {
	if chunkSize < MinChunkSize {
		panic(fmt.Sprintf("chunker.Split: chunkSize %d < MinChunkSize %d", chunkSize, MinChunkSize))
	}
	if len(body) == 0 {
		return nil
	}
	n := (len(body) + chunkSize - 1) / chunkSize
	out := make([]Chunk, 0, n)
	for off := 0; off < len(body); off += chunkSize {
		end := off + chunkSize
		if end > len(body) {
			end = len(body)
		}
		// Slice does NOT copy — chunk views the body. Caller (edge handler)
		// owns body's lifetime; chunks are consumed before body is freed.
		data := body[off:end]
		sum := sha256.Sum256(data)
		out = append(out, Chunk{
			Data: data,
			ID:   hex.EncodeToString(sum[:]),
			Size: int64(end - off),
		})
	}
	return out
}

// JoinSpec is a single chunk reference for Join. Keep this minimal — the
// caller (edge handler) typically derives this from store.ChunkRef.
type JoinSpec struct {
	ChunkID string
	Size    int64
}

// Join concatenates chunks in the given order, fetching bytes via the supplied
// fetcher. Each chunk's bytes are SHA-256 verified against its declared
// ChunkID; any mismatch returns an error and partial body is discarded.
//
// totalSize is checked: sum of fetched chunk sizes must equal totalSize.
//
// fetcher signature mirrors coordinator.ReadChunk's typical shape.
func Join(specs []JoinSpec, totalSize int64, fetch func(chunkID string) ([]byte, error)) ([]byte, error) {
	out := make([]byte, 0, totalSize)
	for i, s := range specs {
		data, err := fetch(s.ChunkID)
		if err != nil {
			return nil, fmt.Errorf("chunk %d (%s): %w", i, s.ChunkID, err)
		}
		// content integrity check
		sum := sha256.Sum256(data)
		got := hex.EncodeToString(sum[:])
		if got != s.ChunkID {
			return nil, fmt.Errorf("chunk %d integrity: want %s, got %s", i, s.ChunkID, got)
		}
		if int64(len(data)) != s.Size {
			return nil, fmt.Errorf("chunk %d size: want %d, got %d", i, s.Size, len(data))
		}
		out = append(out, data...)
	}
	if int64(len(out)) != totalSize {
		return nil, fmt.Errorf("joined size %d != declared %d", len(out), totalSize)
	}
	return out, nil
}

// ErrEmpty is returned when callers need to distinguish empty body from error.
var ErrEmpty = errors.New("chunker: empty body")
