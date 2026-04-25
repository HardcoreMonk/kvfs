package gc

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/HardcoreMonk/kvfs/internal/coordinator"
	"github.com/HardcoreMonk/kvfs/internal/store"
)

// ─── fakes ───

type fakeCoord struct {
	mu       sync.Mutex
	dns      []string
	disks    map[string][]coordinator.ChunkInfo // addr -> chunks
	deleted  map[string]bool                    // "addr|id" -> true
	listErr  map[string]error
	delErr   map[string]error
}

func newFakeCoord(dns ...string) *fakeCoord {
	return &fakeCoord{
		dns:     dns,
		disks:   map[string][]coordinator.ChunkInfo{},
		deleted: map[string]bool{},
		listErr: map[string]error{},
		delErr:  map[string]error{},
	}
}

func (f *fakeCoord) DNs() []string { return append([]string(nil), f.dns...) }

func (f *fakeCoord) ListChunks(_ context.Context, addr string) ([]coordinator.ChunkInfo, error) {
	if err, ok := f.listErr[addr]; ok {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]coordinator.ChunkInfo(nil), f.disks[addr]...)
	return out, nil
}

func (f *fakeCoord) DeleteChunkFrom(_ context.Context, addr, chunkID string) error {
	if err, ok := f.delErr[addr+"|"+chunkID]; ok {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted[addr+"|"+chunkID] = true
	return nil
}

func (f *fakeCoord) seed(addr string, ci coordinator.ChunkInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disks[addr] = append(f.disks[addr], ci)
}

type fakeStore struct{ objs []*store.ObjectMeta }

func (f *fakeStore) ListObjects() ([]*store.ObjectMeta, error) { return f.objs, nil }

// makeObj builds a single-chunk ObjectMeta for tests.
func makeObj(bucket, key, chunkID string, replicas []string, size int64) *store.ObjectMeta {
	return &store.ObjectMeta{
		Bucket: bucket, Key: key, Size: size,
		Chunks: []store.ChunkRef{
			{ChunkID: chunkID, Size: size, Replicas: replicas},
		},
	}
}

// ─── helpers ───

func ago(d time.Duration) int64 { return time.Now().Add(-d).Unix() }

// ─── tests ───

func TestComputePlan_AllClaimed_NoSweeps(t *testing.T) {
	coord := newFakeCoord("dn1", "dn2")
	coord.seed("dn1", coordinator.ChunkInfo{ID: "c1", Size: 10, MTime: ago(2 * time.Hour)})
	coord.seed("dn2", coordinator.ChunkInfo{ID: "c1", Size: 10, MTime: ago(2 * time.Hour)})
	st := &fakeStore{objs: []*store.ObjectMeta{
		makeObj("b", "k", "c1", []string{"dn1", "dn2"}, 10),
	}}
	plan, err := ComputePlan(context.Background(), coord, st, time.Minute)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if len(plan.Sweeps) != 0 {
		t.Errorf("Sweeps = %d, want 0 (all claimed)", len(plan.Sweeps))
	}
	if plan.Scanned != 2 {
		t.Errorf("Scanned = %d, want 2", plan.Scanned)
	}
}

func TestComputePlan_OneSurplus(t *testing.T) {
	coord := newFakeCoord("dn1", "dn2", "dn3")
	for _, d := range []string{"dn1", "dn2", "dn3"} {
		coord.seed(d, coordinator.ChunkInfo{ID: "c1", Size: 100, MTime: ago(2 * time.Hour)})
	}
	// meta only claims dn1+dn3; dn2 holds a surplus copy
	st := &fakeStore{objs: []*store.ObjectMeta{
		makeObj("b", "k", "c1", []string{"dn1", "dn3"}, 100),
	}}
	plan, _ := ComputePlan(context.Background(), coord, st, time.Minute)
	if len(plan.Sweeps) != 1 {
		t.Fatalf("Sweeps = %d, want 1", len(plan.Sweeps))
	}
	if plan.Sweeps[0].Addr != "dn2" || plan.Sweeps[0].ChunkID != "c1" {
		t.Errorf("Sweep = %+v, want dn2/c1", plan.Sweeps[0])
	}
}

func TestComputePlan_MinAgeProtectsFreshChunks(t *testing.T) {
	coord := newFakeCoord("dn1")
	// chunk on disk but not claimed; only 5 seconds old
	coord.seed("dn1", coordinator.ChunkInfo{ID: "c1", Size: 1, MTime: ago(5 * time.Second)})
	st := &fakeStore{}
	plan, _ := ComputePlan(context.Background(), coord, st, time.Minute) // min-age = 60s
	if len(plan.Sweeps) != 0 {
		t.Errorf("Sweeps = %d, want 0 (chunk too fresh)", len(plan.Sweeps))
	}
	// with min-age = 0 the same chunk becomes a sweep
	plan2, _ := ComputePlan(context.Background(), coord, st, 1*time.Nanosecond)
	if len(plan2.Sweeps) != 1 {
		t.Errorf("Sweeps with min-age=0 = %d, want 1", len(plan2.Sweeps))
	}
}

func TestComputePlan_DedupSharedChunkProtectsAllReplicas(t *testing.T) {
	// Two objects share the same chunk_id (dedup). Their replica sets union.
	coord := newFakeCoord("dn1", "dn2", "dn3")
	for _, d := range []string{"dn1", "dn2", "dn3"} {
		coord.seed(d, coordinator.ChunkInfo{ID: "shared", Size: 50, MTime: ago(time.Hour)})
	}
	st := &fakeStore{objs: []*store.ObjectMeta{
		makeObj("b", "k1", "shared", []string{"dn1", "dn2"}, 50),
		makeObj("b", "k2", "shared", []string{"dn3"}, 50),
	}}
	plan, _ := ComputePlan(context.Background(), coord, st, time.Minute)
	if len(plan.Sweeps) != 0 {
		t.Errorf("Sweeps = %d, want 0 (union of replicas covers all DNs)", len(plan.Sweeps))
	}
}

func TestRun_DeletesAndCountsBytes(t *testing.T) {
	coord := newFakeCoord("dn1", "dn2")
	coord.seed("dn2", coordinator.ChunkInfo{ID: "c1", Size: 200, MTime: ago(time.Hour)})
	coord.seed("dn2", coordinator.ChunkInfo{ID: "c2", Size: 300, MTime: ago(time.Hour)})
	st := &fakeStore{}
	plan, _ := ComputePlan(context.Background(), coord, st, time.Minute)
	stats := Run(context.Background(), coord, plan, 2)
	if stats.Deleted != 2 || stats.Failed != 0 {
		t.Errorf("Deleted=%d Failed=%d, want 2 0", stats.Deleted, stats.Failed)
	}
	if stats.BytesFreed != 500 {
		t.Errorf("BytesFreed=%d, want 500", stats.BytesFreed)
	}
	if !coord.deleted["dn2|c1"] || !coord.deleted["dn2|c2"] {
		t.Errorf("expected both chunks deleted on dn2")
	}
}

func TestRun_PerDeleteFailureCountedNotFatal(t *testing.T) {
	coord := newFakeCoord("dn1")
	coord.seed("dn1", coordinator.ChunkInfo{ID: "good", Size: 10, MTime: ago(time.Hour)})
	coord.seed("dn1", coordinator.ChunkInfo{ID: "bad", Size: 20, MTime: ago(time.Hour)})
	coord.delErr["dn1|bad"] = errors.New("simulated")
	st := &fakeStore{}
	plan, _ := ComputePlan(context.Background(), coord, st, time.Minute)
	stats := Run(context.Background(), coord, plan, 2)
	if stats.Deleted != 1 || stats.Failed != 1 {
		t.Errorf("Deleted=%d Failed=%d, want 1 1", stats.Deleted, stats.Failed)
	}
	if stats.BytesFreed != 10 {
		t.Errorf("BytesFreed=%d, want 10 (only good)", stats.BytesFreed)
	}
}

func TestRun_Idempotent_SecondRunNoSweeps(t *testing.T) {
	coord := newFakeCoord("dn1")
	coord.seed("dn1", coordinator.ChunkInfo{ID: "c1", Size: 10, MTime: ago(time.Hour)})
	st := &fakeStore{}
	plan, _ := ComputePlan(context.Background(), coord, st, time.Minute)
	Run(context.Background(), coord, plan, 1)
	// emulate disk no longer having the deleted chunk
	coord.disks["dn1"] = nil
	plan2, _ := ComputePlan(context.Background(), coord, st, time.Minute)
	if len(plan2.Sweeps) != 0 {
		t.Errorf("second plan = %d sweeps, want 0", len(plan2.Sweeps))
	}
}

func TestComputePlan_ListErrorIsFatal(t *testing.T) {
	coord := newFakeCoord("dn1")
	coord.listErr["dn1"] = errors.New("boom")
	st := &fakeStore{}
	_, err := ComputePlan(context.Background(), coord, st, time.Minute)
	if err == nil {
		t.Fatalf("expected error from list failure, got nil")
	}
}

func TestRun_EmptyPlanNoOp(t *testing.T) {
	coord := newFakeCoord("dn1")
	stats := Run(context.Background(), coord, Plan{}, 4)
	if stats.Deleted != 0 || stats.Failed != 0 || stats.BytesFreed != 0 {
		t.Errorf("empty plan should be zero stats: %+v", stats)
	}
}
