// Reed-Solomon encoding/decoding API.
//
// ──────────────────────────────────────────────────────────────────
// 비전공자용 해설 — 행렬 곱 한 줄로 복원하는 마법
// ──────────────────────────────────────────────────────────────────
//
// Encoding (K data → K+M shards)
// ──────────────────────────────
// E = (K+M) × K systematic encoding matrix (top K rows = identity).
// data = K × shardSize 행렬 (각 행이 한 shard).
//
//   shards = E × data   (over GF(2^8), byte-by-byte)
//
// 결과 shards 의 위 K 행 = data 그대로 (identity 부분), 아래 M 행 = parity.
// shardSize 가 1024 byte 면 행렬 곱이 "1024 개 독립 GF 곱" 으로 분해됨.
//
// Decoding (any K of K+M shards → original K data)
// ───────────────────────────────────────────────
// 살아있는 K 개 행 인덱스 idx[0..K) 가 있다고 가정.
// E_sub = E.subMatrix(idx)  →  K × K 행렬.
// MDS 성질에 의해 E_sub 가 invertible (matrix_test 가 검증).
//
//   data = E_sub^-1 × surviving_shards   (over GF(2^8))
//
// 즉 K 개 살아있는 shard 만으로 원본 K data shard 복원.
// 이게 "M failure tolerance" 의 정체.
//
// Reconstruct 의 입력: shards 배열 (길이 K+M, 각 entry 는 nil 또는 shardSize byte).
// nil 인 위치가 잃어버린 shard. 살아있는 게 K 개 미만이면 복원 불가 (ErrTooFewShards).
//
// from-scratch 의 한계
// ──────────────────────
// 이 구현은 byte-by-byte GF 곱. SIMD 안 씀. 경험적으로 ~50 MB/s 수준.
// 프로덕션은 klauspost/reedsolomon 같은 SIMD 라이브러리가 GB/s+. ADR-008 참조.
package reedsolomon

import (
	"errors"
	"fmt"
)

// Encoder is a stateless RS encoder/decoder for fixed (K, M).
//
// Build once via NewEncoder(K, M); reuse for any number of stripes of the
// same shape.
type Encoder struct {
	k, m   int
	encMat *matrix // (K+M) × K systematic encoding matrix
}

// NewEncoder builds an encoder for K data shards + M parity shards per stripe.
// Returns an error if the parameters are out of range or if the encoding
// matrix construction fails.
func NewEncoder(k, m int) (*Encoder, error) {
	if k <= 0 || m <= 0 {
		return nil, fmt.Errorf("reedsolomon: K (%d) and M (%d) must be positive", k, m)
	}
	if k+m > 256 {
		return nil, fmt.Errorf("reedsolomon: K+M (%d) exceeds GF(2^8) capacity (256)", k+m)
	}
	enc, err := makeSystematicEncoding(k, m)
	if err != nil {
		return nil, fmt.Errorf("reedsolomon: build encoding matrix: %w", err)
	}
	return &Encoder{k: k, m: m, encMat: enc}, nil
}

// K returns the number of data shards per stripe.
func (e *Encoder) K() int { return e.k }

// M returns the number of parity shards per stripe.
func (e *Encoder) M() int { return e.m }

// Encode produces M parity shards from K data shards.
//
// dataShards must have length K and each shard must be the same length.
// Returns M parity shards of the same length.
//
// The original data shards are unchanged (top K rows of the encoding matrix
// are identity, so output[0..K) == dataShards). For storage efficiency this
// implementation returns ONLY the M parity shards; the caller already has
// the data shards.
func (e *Encoder) Encode(dataShards [][]byte) ([][]byte, error) {
	if len(dataShards) != e.k {
		return nil, fmt.Errorf("reedsolomon: Encode wants %d data shards, got %d", e.k, len(dataShards))
	}
	shardSize := -1
	for i, s := range dataShards {
		if shardSize < 0 {
			shardSize = len(s)
		} else if len(s) != shardSize {
			return nil, fmt.Errorf("reedsolomon: shard %d size %d differs from shard 0 size %d", i, len(s), shardSize)
		}
	}
	if shardSize == 0 {
		return nil, errors.New("reedsolomon: empty shards")
	}

	// parity[m_row][byte_col]
	parity := make([][]byte, e.m)
	for i := range parity {
		parity[i] = make([]byte, shardSize)
	}

	// For each parity row p (rows K..K+M of encMat) and each byte position c:
	//   parity[p][c] = sum over k of encMat[K+p][k] * dataShards[k][c]
	for p := 0; p < e.m; p++ {
		row := e.encMat.row(e.k + p) // length K
		out := parity[p]
		for kIdx := 0; kIdx < e.k; kIdx++ {
			coef := row[kIdx]
			if coef == 0 {
				continue
			}
			src := dataShards[kIdx]
			for c := 0; c < shardSize; c++ {
				out[c] = gfAdd(out[c], gfMul(coef, src[c]))
			}
		}
	}
	return parity, nil
}

// Reconstruct rebuilds missing data shards from any K surviving shards.
//
// shards must have length K+M. Each entry is the shard bytes, OR nil/empty
// for a missing shard. After Reconstruct returns nil, every entry in the
// FIRST K positions is filled (data shards). Parity shards (positions K..K+M)
// may remain nil — they're not rebuilt unless the caller wants them
// (RebuildParity for that, or just call Encode on the new data shards).
//
// Returns ErrTooFewShards if fewer than K shards survive.
func (e *Encoder) Reconstruct(shards [][]byte) error {
	n := e.k + e.m
	if len(shards) != n {
		return fmt.Errorf("reedsolomon: Reconstruct wants %d shards (slots), got %d", n, len(shards))
	}

	// Index of surviving shards.
	var survIdx []int
	shardSize := -1
	for i, s := range shards {
		if len(s) > 0 {
			survIdx = append(survIdx, i)
			if shardSize < 0 {
				shardSize = len(s)
			} else if len(s) != shardSize {
				return fmt.Errorf("reedsolomon: shard %d size %d differs from %d", i, len(s), shardSize)
			}
		}
	}
	if len(survIdx) < e.k {
		return ErrTooFewShards
	}

	// Fast path: if first K positions all survived, nothing to do.
	allDataPresent := true
	for i := 0; i < e.k; i++ {
		if len(shards[i]) == 0 {
			allDataPresent = false
			break
		}
	}
	if allDataPresent {
		return nil
	}

	// Take first K surviving rows as the basis.
	basis := survIdx[:e.k]
	subEnc := e.encMat.subMatrix(basis) // K × K
	inv, err := subEnc.invert()
	if err != nil {
		return fmt.Errorf("reedsolomon: invert sub-matrix (rows %v): %w", basis, err)
	}

	// Recompute each missing data shard d (d in 0..K) that is currently nil:
	//   shard[d] = sum over j of inv[d][j] * shards[basis[j]]   (byte-wise)
	// Note: this also OVERWRITES surviving data shards with the same value
	// (no-op since correct), so we just rebuild all K data positions.
	rebuilt := make([][]byte, e.k)
	for d := 0; d < e.k; d++ {
		// only rebuild missing ones; preserve existing for clarity
		if len(shards[d]) > 0 {
			continue
		}
		out := make([]byte, shardSize)
		for j := 0; j < e.k; j++ {
			coef := inv.get(d, j)
			if coef == 0 {
				continue
			}
			src := shards[basis[j]]
			for c := 0; c < shardSize; c++ {
				out[c] = gfAdd(out[c], gfMul(coef, src[c]))
			}
		}
		rebuilt[d] = out
	}
	for d := 0; d < e.k; d++ {
		if rebuilt[d] != nil {
			shards[d] = rebuilt[d]
		}
	}
	return nil
}

// ErrTooFewShards is returned by Reconstruct when fewer than K shards survive.
// Below this threshold the data is mathematically unrecoverable.
var ErrTooFewShards = errors.New("reedsolomon: fewer than K shards survive (data unrecoverable)")
