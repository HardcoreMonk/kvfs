// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package repair

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/HardcoreMonk/kvfs/internal/reedsolomon"
	"github.com/HardcoreMonk/kvfs/internal/store"
)

// ─── fakes ───

type fakeCoord struct {
	dns       []string
	placeNMap map[string][]string
	mu        sync.Mutex
	disks     map[string]map[string][]byte // addr → chunkID → data
	readErr   map[string]error             // chunkID → forced err
	putErr    map[string]error             // "addr:chunkID" → forced err
}

func newFakeCoord(dns ...string) *fakeCoord {
	return &fakeCoord{
		dns:       dns,
		placeNMap: map[string][]string{},
		disks:     map[string]map[string][]byte{},
		readErr:   map[string]error{},
		putErr:    map[string]error{},
	}
}

func (f *fakeCoord) DNs() []string                    { return append([]string(nil), f.dns...) }
func (f *fakeCoord) PlaceN(key string, n int) []string {
	out := append([]string(nil), f.placeNMap[key]...)
	if n < len(out) {
		out = out[:n]
	}
	return out
}

func (f *fakeCoord) ReadChunk(_ context.Context, chunkID string, candidates []string) ([]byte, string, error) {
	if err, ok := f.readErr[chunkID]; ok {
		return nil, "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, addr := range candidates {
		if d, ok := f.disks[addr]; ok {
			if body, ok := d[chunkID]; ok {
				return body, addr, nil
			}
		}
	}
	return nil, "", errors.New("not found on candidates")
}

func (f *fakeCoord) PutChunkTo(_ context.Context, addr, chunkID string, data []byte) error {
	if err, ok := f.putErr[addr+":"+chunkID]; ok {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.disks[addr]; !ok {
		f.disks[addr] = map[string][]byte{}
	}
	f.disks[addr][chunkID] = append([]byte(nil), data...)
	return nil
}

func (f *fakeCoord) seed(addr, chunkID string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.disks[addr]; !ok {
		f.disks[addr] = map[string][]byte{}
	}
	f.disks[addr][chunkID] = data
}

type fakeStore struct{ objs []*store.ObjectMeta }

func (f *fakeStore) ListObjects() ([]*store.ObjectMeta, error) { return f.objs, nil }

func (f *fakeStore) UpdateShardReplicas(bucket, key string, stripeIdx, shardIdx int, replicas []string) error {
	for _, o := range f.objs {
		if o.Bucket == bucket && o.Key == key {
			if stripeIdx < 0 || stripeIdx >= len(o.Stripes) {
				return errors.New("stripe out of range")
			}
			s := &o.Stripes[stripeIdx]
			if shardIdx < 0 || shardIdx >= len(s.Shards) {
				return errors.New("shard out of range")
			}
			s.Shards[shardIdx].Replicas = append([]string(nil), replicas...)
			return nil
		}
	}
	return store.ErrNotFound
}

// makeECObj builds a single-stripe EC object with given (K, M).
// Each shard's chunk_id is the actual sha256 of its bytes (so ReadChunk
// returns content matching what Reconstruct expects).
func makeECObj(t *testing.T, bucket, key, stripeID string, k, m int, shardSize int, addrs []string) (*store.ObjectMeta, [][]byte) {
	t.Helper()
	if len(addrs) != k+m {
		t.Fatalf("addrs len = %d, want K+M=%d", len(addrs), k+m)
	}
	dataShards := make([][]byte, k)
	for i := 0; i < k; i++ {
		s := make([]byte, shardSize)
		for j := range s {
			s[j] = byte((i*131 + j*17 + 1) % 256)
		}
		dataShards[i] = s
	}
	enc, err := reedsolomon.NewEncoder(k, m)
	if err != nil {
		t.Fatal(err)
	}
	parityShards, err := enc.Encode(dataShards)
	if err != nil {
		t.Fatal(err)
	}
	all := append(append([][]byte(nil), dataShards...), parityShards...)

	// repairStripe doesn't verify ChunkID against bytes; only PUT does (in
	// real DN). Tests use synthetic stripe-shard-i IDs.
	shards := make([]store.ChunkRef, k+m)
	for i := range all {
		shards[i] = store.ChunkRef{
			ChunkID:  fmt.Sprintf("%s-shard-%d", stripeID, i),
			Size:     int64(shardSize),
			Replicas: []string{addrs[i]},
		}
	}
	obj := &store.ObjectMeta{
		Bucket: bucket, Key: key, Size: int64(k * shardSize),
		EC: &store.ECParams{K: k, M: m, ShardSize: shardSize, DataSize: int64(k * shardSize)},
		Stripes: []store.Stripe{
			{StripeID: stripeID, Shards: shards},
		},
	}
	return obj, all
}

// ─── tests ───

func TestComputePlan_HealthyStripe_NoRepair(t *testing.T) {
	coord := newFakeCoord("dn1", "dn2", "dn3", "dn4", "dn5", "dn6")
	coord.placeNMap["s1"] = []string{"dn1", "dn2", "dn3", "dn4", "dn5", "dn6"}
	obj, _ := makeECObj(t, "b", "k", "s1", 4, 2, 16, []string{"dn1", "dn2", "dn3", "dn4", "dn5", "dn6"})
	st := &fakeStore{objs: []*store.ObjectMeta{obj}}
	plan, err := ComputePlan(coord, st)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Scanned != 1 {
		t.Errorf("Scanned = %d, want 1", plan.Scanned)
	}
	if len(plan.Repairs) != 0 || len(plan.Unrepairable) != 0 {
		t.Errorf("expected no repairs, got %d/%d", len(plan.Repairs), len(plan.Unrepairable))
	}
}

func TestComputePlan_OneDeadShard_Repairable(t *testing.T) {
	// dn5 removed from cluster — shard 4 of stripe is dead.
	coord := newFakeCoord("dn1", "dn2", "dn3", "dn4", "dn6", "dn7")
	coord.placeNMap["s1"] = []string{"dn1", "dn2", "dn3", "dn4", "dn6", "dn7"} // dn5 not in placement
	obj, _ := makeECObj(t, "b", "k", "s1", 4, 2, 16, []string{"dn1", "dn2", "dn3", "dn4", "dn5", "dn6"})
	st := &fakeStore{objs: []*store.ObjectMeta{obj}}
	plan, _ := ComputePlan(coord, st)
	if len(plan.Repairs) != 1 {
		t.Fatalf("Repairs = %d, want 1", len(plan.Repairs))
	}
	r := plan.Repairs[0]
	if len(r.DeadShards) != 1 {
		t.Errorf("DeadShards = %d, want 1", len(r.DeadShards))
	}
	if r.DeadShards[0].ShardIndex != 4 || r.DeadShards[0].OldAddr != "dn5" {
		t.Errorf("dead shard = %+v, want shard 4 / dn5", r.DeadShards[0])
	}
	if r.DeadShards[0].NewAddr != "dn7" {
		t.Errorf("NewAddr = %s, want dn7 (only unused desired)", r.DeadShards[0].NewAddr)
	}
	if len(r.Survivors) != 5 {
		t.Errorf("Survivors = %d, want 5", len(r.Survivors))
	}
}

func TestComputePlan_TwoDeadShards_BothMustRepair(t *testing.T) {
	// dn5 + dn6 removed; M=2 so still K=4 survivors.
	coord := newFakeCoord("dn1", "dn2", "dn3", "dn4", "dn7", "dn8")
	coord.placeNMap["s1"] = []string{"dn1", "dn2", "dn3", "dn4", "dn7", "dn8"}
	obj, _ := makeECObj(t, "b", "k", "s1", 4, 2, 16, []string{"dn1", "dn2", "dn3", "dn4", "dn5", "dn6"})
	st := &fakeStore{objs: []*store.ObjectMeta{obj}}
	plan, _ := ComputePlan(coord, st)
	if len(plan.Repairs) != 1 {
		t.Fatalf("Repairs = %d, want 1", len(plan.Repairs))
	}
	r := plan.Repairs[0]
	if len(r.DeadShards) != 2 {
		t.Errorf("DeadShards = %d, want 2", len(r.DeadShards))
	}
	// Sorted unused = [dn7, dn8] → first dead (idx 4) → dn7, second (idx 5) → dn8
	if r.DeadShards[0].NewAddr != "dn7" || r.DeadShards[1].NewAddr != "dn8" {
		t.Errorf("NewAddrs = %s/%s, want dn7/dn8",
			r.DeadShards[0].NewAddr, r.DeadShards[1].NewAddr)
	}
}

func TestComputePlan_TooManyDead_Unrepairable(t *testing.T) {
	// Only 3 alive (< K=4).
	coord := newFakeCoord("dn1", "dn2", "dn3")
	coord.placeNMap["s1"] = []string{"dn1", "dn2", "dn3"}
	obj, _ := makeECObj(t, "b", "k", "s1", 4, 2, 16, []string{"dn1", "dn2", "dn3", "dn4", "dn5", "dn6"})
	st := &fakeStore{objs: []*store.ObjectMeta{obj}}
	plan, _ := ComputePlan(coord, st)
	if len(plan.Repairs) != 0 {
		t.Errorf("Repairs = %d, want 0", len(plan.Repairs))
	}
	if len(plan.Unrepairable) != 1 {
		t.Errorf("Unrepairable = %d, want 1 (data loss alert)", len(plan.Unrepairable))
	}
}

func TestRun_HappyPath_ReconstructsAndUpdates(t *testing.T) {
	coord := newFakeCoord("dn1", "dn2", "dn3", "dn4", "dn7", "dn8")
	coord.placeNMap["s1"] = []string{"dn1", "dn2", "dn3", "dn4", "dn7", "dn8"}
	obj, allShards := makeECObj(t, "b", "k", "s1", 4, 2, 16, []string{"dn1", "dn2", "dn3", "dn4", "dn5", "dn6"})

	// Seed all 4 surviving shards on their addrs.
	for i := 0; i < 4; i++ {
		coord.seed(obj.Stripes[0].Shards[i].Replicas[0], obj.Stripes[0].Shards[i].ChunkID, allShards[i])
	}
	// Note: dn5/dn6 NOT in coord.dns; their shards are "lost" (data never seeded on dn7/dn8).

	st := &fakeStore{objs: []*store.ObjectMeta{obj}}
	plan, _ := ComputePlan(coord, st)
	if len(plan.Repairs) != 1 {
		t.Fatalf("Repairs = %d", len(plan.Repairs))
	}

	stats := Run(context.Background(), coord, st, plan, 1)
	if stats.Repaired != 1 || stats.Failed != 0 {
		t.Errorf("Repaired=%d Failed=%d, want 1 0; errors=%v", stats.Repaired, stats.Failed, stats.Errors)
	}
	// dn7 + dn8 should now hold the rebuilt shards (idx 4, 5).
	if _, ok := coord.disks["dn7"][obj.Stripes[0].Shards[4].ChunkID]; !ok {
		t.Errorf("dn7 should hold rebuilt shard 4")
	}
	if _, ok := coord.disks["dn8"][obj.Stripes[0].Shards[5].ChunkID]; !ok {
		t.Errorf("dn8 should hold rebuilt shard 5")
	}
	// Meta updated.
	if obj.Stripes[0].Shards[4].Replicas[0] != "dn7" {
		t.Errorf("shard 4 Replicas = %v, want [dn7]", obj.Stripes[0].Shards[4].Replicas)
	}
	if obj.Stripes[0].Shards[5].Replicas[0] != "dn8" {
		t.Errorf("shard 5 Replicas = %v, want [dn8]", obj.Stripes[0].Shards[5].Replicas)
	}
}

func TestRun_SurvivorReadFailure(t *testing.T) {
	coord := newFakeCoord("dn1", "dn2", "dn3", "dn4", "dn7")
	coord.placeNMap["s1"] = []string{"dn1", "dn2", "dn3", "dn4", "dn7", "dn8"}
	obj, allShards := makeECObj(t, "b", "k", "s1", 4, 2, 16, []string{"dn1", "dn2", "dn3", "dn4", "dn5", "dn6"})
	for i := 0; i < 4; i++ {
		coord.seed(obj.Stripes[0].Shards[i].Replicas[0], obj.Stripes[0].Shards[i].ChunkID, allShards[i])
	}
	// Force read on dn1 to fail (= dn1 just went down mid-repair).
	coord.readErr[obj.Stripes[0].Shards[0].ChunkID] = errors.New("dn1 timeout")

	st := &fakeStore{objs: []*store.ObjectMeta{obj}}
	plan, _ := ComputePlan(coord, st)
	stats := Run(context.Background(), coord, st, plan, 1)
	if stats.Failed != 1 {
		t.Errorf("Failed = %d, want 1 (survivor read failed)", stats.Failed)
	}
	// Meta untouched.
	if obj.Stripes[0].Shards[4].Replicas[0] != "dn5" {
		t.Errorf("meta should be unchanged after failure: shard 4 = %v", obj.Stripes[0].Shards[4].Replicas)
	}
}

func TestRun_Idempotent_SecondRunIsNoop(t *testing.T) {
	coord := newFakeCoord("dn1", "dn2", "dn3", "dn4", "dn7", "dn8")
	coord.placeNMap["s1"] = []string{"dn1", "dn2", "dn3", "dn4", "dn7", "dn8"}
	obj, allShards := makeECObj(t, "b", "k", "s1", 4, 2, 16, []string{"dn1", "dn2", "dn3", "dn4", "dn5", "dn6"})
	for i := 0; i < 4; i++ {
		coord.seed(obj.Stripes[0].Shards[i].Replicas[0], obj.Stripes[0].Shards[i].ChunkID, allShards[i])
	}
	st := &fakeStore{objs: []*store.ObjectMeta{obj}}
	plan1, _ := ComputePlan(coord, st)
	Run(context.Background(), coord, st, plan1, 1)

	plan2, _ := ComputePlan(coord, st)
	if len(plan2.Repairs) != 0 {
		t.Errorf("second plan should be empty, got %d repairs", len(plan2.Repairs))
	}
}

func TestComputePlan_NoECObjects_NoRepairs(t *testing.T) {
	coord := newFakeCoord("dn1", "dn2", "dn3")
	st := &fakeStore{objs: []*store.ObjectMeta{
		{Bucket: "b", Key: "k", Chunks: []store.ChunkRef{
			{ChunkID: "c1", Size: 10, Replicas: []string{"dn1", "dn2", "dn3"}},
		}},
	}}
	plan, _ := ComputePlan(coord, st)
	if len(plan.Repairs) != 0 || plan.Scanned != 0 {
		t.Errorf("non-EC objects should not be scanned: scanned=%d repairs=%d",
			plan.Scanned, len(plan.Repairs))
	}
}
