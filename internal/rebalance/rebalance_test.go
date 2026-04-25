// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package rebalance

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/HardcoreMonk/kvfs/internal/store"
)

// ─── fakes ───

type fakeCoord struct {
	// placement map: chunkID -> desired addresses
	placeMap map[string][]string
	// chunk bodies stored per (addr, chunkID)
	mu    sync.Mutex
	disks map[string]map[string][]byte // addr -> chunkID -> data
	// readErr forces ReadChunk to fail for a given chunkID
	readErr map[string]error
	// putErr forces PutChunkTo to fail for (addr, chunkID)
	putErr map[string]error
}

func newFakeCoord() *fakeCoord {
	return &fakeCoord{
		placeMap: map[string][]string{},
		disks:    map[string]map[string][]byte{},
		readErr:  map[string]error{},
		putErr:   map[string]error{},
	}
}

func (f *fakeCoord) seedDisk(addr, chunkID string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.disks[addr]; !ok {
		f.disks[addr] = map[string][]byte{}
	}
	f.disks[addr][chunkID] = data
}

func (f *fakeCoord) PlaceChunk(chunkID string) []string {
	out := append([]string(nil), f.placeMap[chunkID]...)
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
	return nil, "", errors.New("not found on any candidate")
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

type fakeStore struct {
	mu      sync.Mutex
	objects []*store.ObjectMeta
}

func (f *fakeStore) ListObjects() ([]*store.ObjectMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*store.ObjectMeta, len(f.objects))
	for i, o := range f.objects {
		copyObj := *o
		copyObj.Chunks = make([]store.ChunkRef, len(o.Chunks))
		for ci, c := range o.Chunks {
			copyObj.Chunks[ci] = store.ChunkRef{
				ChunkID:  c.ChunkID,
				Size:     c.Size,
				Replicas: append([]string(nil), c.Replicas...),
			}
		}
		out[i] = &copyObj
	}
	return out, nil
}

func (f *fakeStore) UpdateChunkReplicas(bucket, key string, chunkIndex int, replicas []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, o := range f.objects {
		if o.Bucket == bucket && o.Key == key {
			if chunkIndex < 0 || chunkIndex >= len(o.Chunks) {
				return errors.New("chunk index out of range")
			}
			o.Chunks[chunkIndex].Replicas = append([]string(nil), replicas...)
			return nil
		}
	}
	return store.ErrNotFound
}

// makeObj builds a single-chunk ObjectMeta for tests.
func makeObj(bucket, key, chunkID string, replicas []string, size int64) *store.ObjectMeta {
	return &store.ObjectMeta{
		Bucket: bucket, Key: key, Size: size,
		Chunks: []store.ChunkRef{
			{ChunkID: chunkID, Size: size, Replicas: replicas},
		},
	}
}

// ─── tests ───

func TestComputePlan_AllOK(t *testing.T) {
	coord := newFakeCoord()
	coord.placeMap["c1"] = []string{"dn1", "dn2", "dn3"}
	st := &fakeStore{
		objects: []*store.ObjectMeta{
			makeObj("b", "k", "c1", []string{"dn1", "dn2", "dn3"}, 10),
		},
	}
	plan, err := ComputePlan(coord, st)
	if err != nil {
		t.Fatalf("ComputePlan error: %v", err)
	}
	if plan.Scanned != 1 {
		t.Errorf("Scanned = %d, want 1", plan.Scanned)
	}
	if len(plan.Migrations) != 0 {
		t.Errorf("Migrations = %d, want 0 (all in place)", len(plan.Migrations))
	}
}

func TestComputePlan_OneMisplaced(t *testing.T) {
	coord := newFakeCoord()
	coord.placeMap["c1"] = []string{"dn4", "dn1", "dn3"} // dn4 desired but missing
	st := &fakeStore{
		objects: []*store.ObjectMeta{
			makeObj("b", "k", "c1", []string{"dn1", "dn2", "dn3"}, 100),
		},
	}
	plan, err := ComputePlan(coord, st)
	if err != nil {
		t.Fatalf("ComputePlan error: %v", err)
	}
	if len(plan.Migrations) != 1 {
		t.Fatalf("Migrations = %d, want 1", len(plan.Migrations))
	}
	m := plan.Migrations[0]
	if !reflect.DeepEqual(m.Missing, []string{"dn4"}) {
		t.Errorf("Missing = %v, want [dn4]", m.Missing)
	}
	if !reflect.DeepEqual(m.Surplus, []string{"dn2"}) {
		t.Errorf("Surplus = %v, want [dn2]", m.Surplus)
	}
	if !sort.StringsAreSorted(m.Actual) || !sort.StringsAreSorted(m.Desired) {
		t.Errorf("Actual/Desired must be sorted")
	}
}

func TestRun_HappyPath_CopiesAndUpdatesMeta(t *testing.T) {
	coord := newFakeCoord()
	coord.placeMap["c1"] = []string{"dn4", "dn1", "dn3"}
	body := []byte("hello-rebalance")
	for _, src := range []string{"dn1", "dn2", "dn3"} {
		coord.seedDisk(src, "c1", body)
	}
	st := &fakeStore{
		objects: []*store.ObjectMeta{
			makeObj("b", "k", "c1", []string{"dn1", "dn2", "dn3"}, int64(len(body))),
		},
	}
	plan, _ := ComputePlan(coord, st)
	stats := Run(context.Background(), coord, st, plan, 1)
	if stats.Migrated != 1 {
		t.Errorf("Migrated = %d, want 1", stats.Migrated)
	}
	if stats.Failed != 0 {
		t.Errorf("Failed = %d, want 0", stats.Failed)
	}
	if stats.BytesCopied != int64(len(body)) {
		t.Errorf("BytesCopied = %d, want %d", stats.BytesCopied, len(body))
	}
	// dn4 should now hold the chunk
	if _, ok := coord.disks["dn4"]["c1"]; !ok {
		t.Errorf("dn4 should hold c1 after rebalance")
	}
	// On full success, meta replicas should equal Desired (surplus dn2 dropped from meta).
	// dn2 still has the chunk on disk — that's the surplus GC will later clean.
	got := st.objects[0].Chunks[0].Replicas
	want := []string{"dn1", "dn3", "dn4"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("replicas after rebalance = %v, want %v (Desired set)", got, want)
	}
	// dn2 still holds the chunk on (fake) disk — kept for GC, never deleted by rebalance
	if _, ok := coord.disks["dn2"]["c1"]; !ok {
		t.Errorf("dn2 disk should still hold c1 (rebalance never deletes)")
	}
}

func TestRun_PartialCopyFailure_KeepsOldRetriesNext(t *testing.T) {
	coord := newFakeCoord()
	coord.placeMap["c1"] = []string{"dn4", "dn5", "dn1"} // missing dn4 + dn5
	body := []byte("ABC")
	coord.seedDisk("dn1", "c1", body)
	coord.seedDisk("dn2", "c1", body)
	coord.seedDisk("dn3", "c1", body)
	// dn5 PUT will fail
	coord.putErr["dn5:c1"] = errors.New("simulated dn5 down")

	st := &fakeStore{
		objects: []*store.ObjectMeta{
			makeObj("b", "k", "c1", []string{"dn1", "dn2", "dn3"}, int64(len(body))),
		},
	}
	plan, _ := ComputePlan(coord, st)
	stats := Run(context.Background(), coord, st, plan, 2)
	if stats.Failed != 1 {
		t.Errorf("Failed = %d, want 1 (dn5 PUT failed)", stats.Failed)
	}
	if stats.Migrated != 0 {
		t.Errorf("Migrated = %d, want 0 (had partial)", stats.Migrated)
	}
	// dn4 should still be in meta (partial success)
	got := st.objects[0].Chunks[0].Replicas
	wantContains := "dn4"
	found := false
	for _, r := range got {
		if r == wantContains {
			found = true
		}
	}
	if !found {
		t.Errorf("replicas should include dn4 after partial success: got %v", got)
	}
	// dn5 must NOT be in meta (PUT failed)
	for _, r := range got {
		if r == "dn5" {
			t.Errorf("replicas should NOT include dn5 after PUT failure: %v", got)
		}
	}
}

func TestRun_SourceUnavailable_FailsCleanly(t *testing.T) {
	coord := newFakeCoord()
	coord.placeMap["c1"] = []string{"dn4"}
	coord.readErr["c1"] = errors.New("all sources dead")
	st := &fakeStore{
		objects: []*store.ObjectMeta{
			makeObj("b", "k", "c1", []string{"dn1"}, 5),
		},
	}
	plan, _ := ComputePlan(coord, st)
	stats := Run(context.Background(), coord, st, plan, 1)
	if stats.Failed != 1 {
		t.Errorf("Failed = %d, want 1", stats.Failed)
	}
	if stats.BytesCopied != 0 {
		t.Errorf("BytesCopied = %d, want 0", stats.BytesCopied)
	}
}

func TestRun_Idempotent_SecondRunZeroMigrations(t *testing.T) {
	coord := newFakeCoord()
	coord.placeMap["c1"] = []string{"dn4", "dn1", "dn3"}
	body := []byte("x")
	for _, src := range []string{"dn1", "dn2", "dn3"} {
		coord.seedDisk(src, "c1", body)
	}
	st := &fakeStore{
		objects: []*store.ObjectMeta{
			makeObj("b", "k", "c1", []string{"dn1", "dn2", "dn3"}, 1),
		},
	}
	plan1, _ := ComputePlan(coord, st)
	Run(context.Background(), coord, st, plan1, 1)

	plan2, _ := ComputePlan(coord, st)
	if len(plan2.Migrations) != 0 {
		t.Errorf("second plan should be empty, got %d migrations", len(plan2.Migrations))
	}
}

func TestRun_EmptyPlan_NoOp(t *testing.T) {
	coord := newFakeCoord()
	st := &fakeStore{}
	stats := Run(context.Background(), coord, st, Plan{}, 4)
	if stats.Migrated != 0 || stats.Failed != 0 || stats.BytesCopied != 0 {
		t.Errorf("empty plan should produce zero stats: %+v", stats)
	}
}

func TestRun_Concurrency_ParallelBatch(t *testing.T) {
	coord := newFakeCoord()
	body := []byte("payload")
	const N = 20
	st := &fakeStore{}
	for i := 0; i < N; i++ {
		cid := chunkName(i)
		coord.placeMap[cid] = []string{"dn4", "dn1", "dn2"}
		coord.seedDisk("dn1", cid, body)
		coord.seedDisk("dn3", cid, body)
		st.objects = append(st.objects,
			makeObj("b", chunkName(i), cid, []string{"dn1", "dn3"}, int64(len(body))))
	}
	plan, _ := ComputePlan(coord, st)
	stats := Run(context.Background(), coord, st, plan, 8)
	if stats.Migrated != N {
		t.Errorf("Migrated = %d, want %d", stats.Migrated, N)
	}
	// every object should now have dn4
	for _, o := range st.objects {
		found := false
		for _, r := range o.Chunks[0].Replicas {
			if r == "dn4" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s missing dn4 after rebalance", o.Key)
		}
	}
}

func chunkName(i int) string {
	return string(rune('a'+i%26)) + string(rune('0'+i/26))
}
