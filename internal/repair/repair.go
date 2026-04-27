// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Package repair implements EC stripe repair (ADR-025): rebuild shards whose
// holding DN is no longer in the cluster, using K surviving shards in the
// same stripe via Reed-Solomon Reconstruct.
//
// 비전공자용 해설
// ──────────────
// ADR-024 (EC stripe rebalance) 는 shard 의 OldAddr DN 이 살아있을 때만
// 동작한다. dn4 가 영구 사망 + 운영자가 dns_runtime 에서 제거 → ADR-024
// 의 ReadChunk(dn4) 가 모두 fail. 데이터는 같은 stripe 의 다른 shards 에
// 살아있는데 활용 못 함.
//
// 이 패키지가 그 갭을 메운다:
//   1. dead shard 식별 — Replicas[0] 가 현 cluster DNs 셋에 없는 shard
//   2. 같은 stripe 의 K 개 surviving shard 로 reedsolomon.Reconstruct
//   3. 재구성된 데이터를 새 DN (PlaceN 의 unused) 으로 PUT
//   4. 메타 갱신 (Replicas = [NewAddr])
//
// chunk_id 는 content-addressable 이라 재구성 결과 sha256 = 원본 sha256.
// 메타에 기록된 ChunkID 그대로 유효.
package repair

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/HardcoreMonk/kvfs/internal/reedsolomon"
	"github.com/HardcoreMonk/kvfs/internal/store"
)

// Coordinator 는 repair 가 사용하는 Coordinator 의 부분 인터페이스.
type Coordinator interface {
	DNs() []string
	PlaceN(key string, n int) []string
	ReadChunk(ctx context.Context, chunkID string, candidates []string) ([]byte, string, error)
	PutChunkTo(ctx context.Context, addr, chunkID string, data []byte) error
	// PutChunkToForce (ADR-058): overwrite path for corrupt-shard repair.
	// Vanilla PutChunkTo skips when the chunk file exists (idempotent, ADR-005
	// invariant). For corrupt shards the file IS on disk but the bytes are
	// wrong; force bypasses the existence-skip. Body still validated against
	// chunk_id by the DN, so invariant holds.
	PutChunkToForce(ctx context.Context, addr, chunkID string, data []byte) error
}

// ObjectStore 는 repair 가 사용하는 store 의 부분 인터페이스.
type ObjectStore interface {
	ListObjects() ([]*store.ObjectMeta, error)
	UpdateShardReplicas(bucket, key string, stripeIndex, shardIndex int, replicas []string) error
}

// SurvivorRef는 stripe 안의 살아있는 shard 한 개에 대한 reference.
type SurvivorRef struct {
	ShardIndex int    `json:"shard_index"`
	ChunkID    string `json:"chunk_id"`
	Addr       string `json:"addr"`
}

// DeadShard는 옛 위치(OldAddr)가 cluster 에서 사라진 shard.
// NewAddr 가 destination — sorted unused desired DNs 에서 결정적으로 할당.
//
// Force (ADR-058) 시 PutChunkToForce 사용 — corrupt 재작성 시나리오에서
// 기존 file 존재 시의 idempotent skip 우회. ADR-046 / ADR-057 (missing) 은
// 기본 false 그대로.
type DeadShard struct {
	ShardIndex int    `json:"shard_index"`
	ChunkID    string `json:"chunk_id"`
	OldAddr    string `json:"old_addr"`
	NewAddr    string `json:"new_addr"`
	Size       int64  `json:"size"`
	Force      bool   `json:"force,omitempty"`
}

// StripeRepair는 한 stripe 의 repair 작업 정의.
// Survivors 가 K 개 미만이면 Unrepairable.
type StripeRepair struct {
	Bucket      string        `json:"bucket"`
	Key         string        `json:"key"`
	StripeIndex int           `json:"stripe_index"`
	K           int           `json:"k"`
	M           int           `json:"m"`
	Survivors   []SurvivorRef `json:"survivors"`
	DeadShards  []DeadShard   `json:"dead_shards"`
}

// Plan은 ComputePlan 의 결과. Repairs 는 K survivors 보장; Unrepairable 은
// 운영자 alert 용 (수학적 복구 불가).
type Plan struct {
	Scanned      int            `json:"scanned"`
	Repairs      []StripeRepair `json:"repairs"`
	Unrepairable []StripeRepair `json:"unrepairable,omitempty"`
}

// RunStats는 Run 의 결과 통계.
type RunStats struct {
	Stripes      int      `json:"stripes"`       // 시도한 stripe 수
	Repaired     int      `json:"repaired"`      // 모든 dead shard 복구 + 메타 갱신
	Failed       int      `json:"failed"`        // 부분/전체 실패
	BytesWritten int64    `json:"bytes_written"` // 누적 PUT 바이트
	Errors       []string `json:"errors,omitempty"`
}

// ComputePlan은 모든 EC object 를 walk 하면서 dead shard 가 있는 stripe 를
// 찾아 repair plan 으로 분류한다. Read-only.
//
// dead 판정: shard.Replicas[0] not in coord.DNs() (ADR-027 dynamic registry).
func ComputePlan(coord Coordinator, st ObjectStore) (Plan, error) {
	objs, err := st.ListObjects()
	if err != nil {
		return Plan{}, fmt.Errorf("repair: list objects: %w", err)
	}
	dns := coord.DNs()
	liveDNs := make(map[string]struct{}, len(dns))
	for _, a := range dns {
		liveDNs[a] = struct{}{}
	}

	plan := Plan{}
	for _, obj := range objs {
		if !obj.IsEC() {
			continue
		}
		k, m := obj.EC.K, obj.EC.M
		want := k + m
		for si, stripe := range obj.Stripes {
			plan.Scanned++
			var survivors []SurvivorRef
			var deadIdx []int
			actualSet := map[string]struct{}{}
			for shi, sh := range stripe.Shards {
				if len(sh.Replicas) == 0 {
					deadIdx = append(deadIdx, shi)
					continue
				}
				addr := sh.Replicas[0]
				if _, ok := liveDNs[addr]; ok {
					survivors = append(survivors, SurvivorRef{
						ShardIndex: shi, ChunkID: sh.ChunkID, Addr: addr,
					})
					actualSet[addr] = struct{}{}
				} else {
					deadIdx = append(deadIdx, shi)
				}
			}
			if len(deadIdx) == 0 {
				continue // healthy
			}
			rep := StripeRepair{
				Bucket: obj.Bucket, Key: obj.Key, StripeIndex: si,
				K: k, M: m, Survivors: survivors,
			}
			if len(survivors) < k {
				// Forensics: list which shards we lost (NewAddr empty — unallocatable).
				for _, shi := range deadIdx {
					oldAddr := ""
					if shi < len(stripe.Shards) && len(stripe.Shards[shi].Replicas) > 0 {
						oldAddr = stripe.Shards[shi].Replicas[0]
					}
					rep.DeadShards = append(rep.DeadShards, DeadShard{
						ShardIndex: shi,
						ChunkID:    stripe.Shards[shi].ChunkID,
						OldAddr:    oldAddr,
						Size:       stripe.Shards[shi].Size,
					})
				}
				plan.Unrepairable = append(plan.Unrepairable, rep)
				continue
			}
			// Pick K+M desired DNs; remove any already-occupied by survivors.
			desired := coord.PlaceN(stripe.StripeID, want)
			var unused []string
			for _, a := range desired {
				if _, used := actualSet[a]; !used {
					unused = append(unused, a)
				}
			}
			sort.Strings(unused)
			for _, shi := range deadIdx {
				if len(unused) == 0 {
					break // can't allocate; rare (cluster smaller than K+M)
				}
				newAddr := unused[0]
				unused = unused[1:]
				oldAddr := ""
				if shi < len(stripe.Shards) && len(stripe.Shards[shi].Replicas) > 0 {
					oldAddr = stripe.Shards[shi].Replicas[0]
				}
				rep.DeadShards = append(rep.DeadShards, DeadShard{
					ShardIndex: shi,
					ChunkID:    stripe.Shards[shi].ChunkID,
					OldAddr:    oldAddr,
					NewAddr:    newAddr,
					Size:       stripe.Shards[shi].Size,
				})
			}
			if len(rep.DeadShards) > 0 {
				plan.Repairs = append(plan.Repairs, rep)
			}
		}
	}
	return plan, nil
}

// Run은 plan.Repairs 를 concurrency goroutine 으로 실행. Unrepairable 은
// 무시 (caller 가 별도 alert).
//
// concurrency <= 0 falls back to 1.
func Run(ctx context.Context, coord Coordinator, st ObjectStore, plan Plan, concurrency int) RunStats {
	if concurrency <= 0 {
		concurrency = 1
	}
	stats := RunStats{}
	if len(plan.Repairs) == 0 {
		return stats
	}

	type result struct {
		ok    bool
		bytes int64
		err   string
	}
	resultCh := make(chan result, len(plan.Repairs))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, rep := range plan.Repairs {
		wg.Add(1)
		rep := rep
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ok, bytes, err := repairStripe(ctx, coord, st, rep)
			if err != nil {
				resultCh <- result{ok: false, bytes: bytes, err: err.Error()}
				return
			}
			resultCh <- result{ok: ok, bytes: bytes}
		}()
	}
	go func() { wg.Wait(); close(resultCh) }()

	for r := range resultCh {
		stats.Stripes++
		stats.BytesWritten += r.bytes
		if r.ok {
			stats.Repaired++
		} else {
			stats.Failed++
			if r.err != "" {
				stats.Errors = append(stats.Errors, r.err)
			}
		}
	}
	return stats
}

// repairStripe handles one stripe end-to-end: fetch K survivors → Reconstruct
// → Encode (parity if needed) → PUT to NewAddr → UpdateShardReplicas.
func repairStripe(ctx context.Context, coord Coordinator, st ObjectStore, rep StripeRepair) (bool, int64, error) {
	enc, err := reedsolomon.NewEncoder(rep.K, rep.M)
	if err != nil {
		return false, 0, fmt.Errorf("%s/%s stripe %d: encoder: %w",
			rep.Bucket, rep.Key, rep.StripeIndex, err)
	}
	n := rep.K + rep.M

	// 1. Fetch K survivors. Use first K (any K is fine for Reconstruct).
	if len(rep.Survivors) < rep.K {
		return false, 0, fmt.Errorf("%s/%s stripe %d: only %d survivors < K=%d",
			rep.Bucket, rep.Key, rep.StripeIndex, len(rep.Survivors), rep.K)
	}
	shards := make([][]byte, n)
	for i := 0; i < rep.K; i++ {
		s := rep.Survivors[i]
		data, _, err := coord.ReadChunk(ctx, s.ChunkID, []string{s.Addr})
		if err != nil {
			return false, 0, fmt.Errorf("%s/%s stripe %d: read survivor shard %d @ %s: %w",
				rep.Bucket, rep.Key, rep.StripeIndex, s.ShardIndex, s.Addr, err)
		}
		shards[s.ShardIndex] = data
	}

	// 2. Reconstruct → fills nil data positions (0..K-1).
	if err := enc.Reconstruct(shards); err != nil {
		return false, 0, fmt.Errorf("%s/%s stripe %d: reconstruct: %w",
			rep.Bucket, rep.Key, rep.StripeIndex, err)
	}

	// 3. If any dead is parity (>= K), recompute all M parity from data.
	needParity := false
	for _, d := range rep.DeadShards {
		if d.ShardIndex >= rep.K {
			needParity = true
			break
		}
	}
	if needParity {
		dataShards := shards[:rep.K]
		parity, err := enc.Encode(dataShards)
		if err != nil {
			return false, 0, fmt.Errorf("%s/%s stripe %d: encode parity: %w",
				rep.Bucket, rep.Key, rep.StripeIndex, err)
		}
		for i := 0; i < rep.M; i++ {
			shards[rep.K+i] = parity[i]
		}
	}

	// 4. PUT each rebuilt shard to NewAddr + update meta.
	var bytesWritten int64
	for _, d := range rep.DeadShards {
		data := shards[d.ShardIndex]
		if data == nil {
			return false, bytesWritten, fmt.Errorf("%s/%s stripe %d shard %d: rebuilt data nil",
				rep.Bucket, rep.Key, rep.StripeIndex, d.ShardIndex)
		}
		put := coord.PutChunkTo
		if d.Force {
			put = coord.PutChunkToForce
		}
		if err := put(ctx, d.NewAddr, d.ChunkID, data); err != nil {
			return false, bytesWritten, fmt.Errorf("%s/%s stripe %d shard %d: PUT %s: %w",
				rep.Bucket, rep.Key, rep.StripeIndex, d.ShardIndex, d.NewAddr, err)
		}
		bytesWritten += int64(len(data))
		if err := st.UpdateShardReplicas(rep.Bucket, rep.Key, rep.StripeIndex, d.ShardIndex, []string{d.NewAddr}); err != nil {
			return false, bytesWritten, fmt.Errorf("%s/%s stripe %d shard %d: meta update: %w",
				rep.Bucket, rep.Key, rep.StripeIndex, d.ShardIndex, err)
		}
	}
	return true, bytesWritten, nil
}

// ErrNotEC is returned when ComputePlan is somehow handed a non-EC object;
// currently filtered upstream so this is defensive.
var ErrNotEC = errors.New("repair: object is not EC-mode")
