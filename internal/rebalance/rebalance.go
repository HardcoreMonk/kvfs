// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Package rebalance migrates existing chunks to their currently-desired DNs.
//
// ──────────────────────────────────────────────────────────────────
// 비전공자용 해설
// ──────────────────────────────────────────────────────────────────
//
// 풀어야 할 문제 (ADR-010 의 동기)
// ────────────────────────────────
// ADR-009 (Rendezvous Hashing) 으로 **새 쓰기** 는 항상 현재 토폴로지에 맞춰
// 상위 R 개 DN 에 간다. 그러나 **DN 추가 직후의 기존 청크** 는 쓰기 시점에
// 결정된 DN 에 그대로 누워 있다. 새 DN(예: dn4) 디스크에는 그 청크가 0개.
//
//   meta.Replicas      = ["dn1:8080", "dn2:8080", "dn3:8080"]   ← 쓰기 시점 사실
//   placer.Pick(...)   = ["dn1:8080", "dn4:8080", "dn3:8080"]   ← 지금이라면
//                                       ↑ dn4 는 청크 없음
//
// 이 패키지는 두 집합을 비교해 **누락된 DN(missing)** 으로 청크를 복사하고
// 메타의 Replicas 배열을 갱신한다.
//
// 안전 규칙: copy-then-update, never delete
// ─────────────────────────────────────────
// "새 자리에 복사 → 메타 갱신 → 이전 자리에서 삭제" 의 마지막 단계를 **하지 않는다**.
// 즉 over-replicate 를 허용한다. 이유:
//   - 마이그레이션 도중 서버가 죽어도 청크가 살아있음
//   - 멱등성 — 같은 plan 을 두 번 돌려도 안전 (같은 chunk_id 면 PUT 덮어쓰기)
//   - 디스크 사용량 일시 증가는 받아들이는 트레이드오프
//
// 잉여 청크(surplus) 정리는 **별도 GC 패스** (ADR-012 예상) 의 책임이다.
//
// 동시성·실패 처리
// ───────────────
//   - Run 은 concurrency goroutine 으로 청크 단위 병렬 처리
//   - 한 청크의 부분 실패: 성공한 dst 만 메타에 추가 (다음 run 이 나머지 재시도)
//   - 소스 GET 자체 실패 (모든 actual DN 죽음): 그 청크는 손상 위험. 별도 alert
//     필요 — 현 ADR 범위 밖. 통계의 Failed 카운터에만 기록
//
// 인터페이스 분리
// ──────────────
// Coordinator·ObjectStore 를 인터페이스로 받는다. 이유:
//   - 테스트에서 fake 주입 가능 (네트워크·디스크 없이 알고리즘 검증)
//   - 미래에 별도 daemon 으로 분리해도 같은 패키지 재사용
package rebalance

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/HardcoreMonk/kvfs/internal/store"
)

// Coordinator is the subset of *coordinator.Coordinator used by rebalance.
// Defined as an interface for testability.
type Coordinator interface {
	// PlaceChunk returns the R desired addresses for a replication chunk.
	PlaceChunk(chunkID string) []string

	// PlaceN returns the top-n addresses for an arbitrary key. Used for
	// stripe-level placement (ADR-024 EC stripe rebalance).
	PlaceN(key string, n int) []string

	// PlaceNFromAddrs runs HRW against an arbitrary subset of DN addresses.
	// Used by class-aware rebalance (P5-04) when the object's Class label
	// restricts placement to a subset of the runtime DN list.
	PlaceNFromAddrs(key string, n int, addrs []string) []string

	// ReadChunk fetches the chunk body from the first responsive candidate.
	// Returns body, address-served-from, error.
	ReadChunk(ctx context.Context, chunkID string, candidates []string) ([]byte, string, error)

	// PutChunkTo writes a chunk to a single DN address (no fanout, no quorum).
	PutChunkTo(ctx context.Context, addr, chunkID string, data []byte) error
}

// ClassResolver maps a class label ("hot", "cold") to the list of DN
// addresses currently registered with that label. Empty class returns the
// full DN set (no filtering). P5-04: lets rebalance compute desired
// placement for class-tagged objects without coupling to the runtime
// MetaStore directly.
type ClassResolver interface {
	ListRuntimeDNsByClass(class string) ([]string, error)
}

// ObjectStore is the subset of *store.MetaStore used by rebalance.
type ObjectStore interface {
	ListObjects() ([]*store.ObjectMeta, error)

	// UpdateChunkReplicas updates one replication chunk's Replicas (no Version bump).
	UpdateChunkReplicas(bucket, key string, chunkIndex int, replicas []string) error

	// UpdateShardReplicas updates one EC shard's Replicas inside a stripe
	// (ADR-024). Single-addr per shard.
	UpdateShardReplicas(bucket, key string, stripeIndex, shardIndex int, replicas []string) error
}

// MigrationKind tags Migration as either a replication-chunk move (ADR-010)
// or an EC-shard move (ADR-024).
type MigrationKind string

const (
	KindChunk MigrationKind = "chunk" // replication mode (obj.Chunks)
	KindShard MigrationKind = "shard" // EC mode (obj.Stripes[].Shards)
)

// Migration is one planned move. Field set varies by Kind:
//
//	KindChunk: ChunkIndex + Actual/Desired/Missing/Surplus (R-list semantics)
//	KindShard: StripeIndex + ShardIndex + OldAddr + NewAddr (single-addr)
//
// Both share Bucket, Key, ChunkID, Size.
type Migration struct {
	Bucket  string        `json:"bucket"`
	Key     string        `json:"key"`
	Kind    MigrationKind `json:"kind"`
	ChunkID string        `json:"chunk_id"`
	Size    int64         `json:"size"`

	// KindChunk fields
	ChunkIndex int      `json:"chunk_index,omitempty"`
	Actual     []string `json:"actual,omitempty"`
	Desired    []string `json:"desired,omitempty"`
	Missing    []string `json:"missing,omitempty"`
	Surplus    []string `json:"surplus,omitempty"`

	// KindShard fields
	StripeIndex int    `json:"stripe_index,omitempty"`
	ShardIndex  int    `json:"shard_index,omitempty"`
	OldAddr     string `json:"old_addr,omitempty"`
	NewAddr     string `json:"new_addr,omitempty"`
}

// Plan summarizes what would change. Pure read — no side effect.
type Plan struct {
	Scanned    int         `json:"scanned"`             // 메타에서 본 객체 수
	Migrations []Migration `json:"migrations"`          // Missing 이 빈 것은 제외
	GeneratedAt string     `json:"generated_at,omitempty"`
}

// RunStats reports the outcome of executing a Plan.
type RunStats struct {
	Migrated    int      `json:"migrated"`     // 메타까지 갱신 완료된 청크 수
	Failed      int      `json:"failed"`       // 부분 실패 또는 전체 실패
	BytesCopied int64    `json:"bytes_copied"` // 누적 복사 바이트 (size × dst 수)
	Errors      []string `json:"errors,omitempty"`
}

// ComputePlan walks every chunk (replication) and every shard (EC) of every
// object, comparing desired vs actual placement. Read-only.
//
// Scanned counts objects. Migrations are per-chunk (replication) or per-shard
// (EC) — a 100-chunk object or a 50-stripe EC object can contribute many
// entries.
//
// Backward-compatible: pass `class` as nil to disable class-aware filtering
// (legacy behavior). When non-nil, objects with `Meta.Class != ""` get their
// desired placement computed from the class-filtered DN subset (P5-04 hot/
// cold rebalance integration).
func ComputePlan(coord Coordinator, st ObjectStore, class ClassResolver) (Plan, error) {
	objs, err := st.ListObjects()
	if err != nil {
		return Plan{}, fmt.Errorf("rebalance: list objects: %w", err)
	}
	plan := Plan{Scanned: len(objs)}
	for _, obj := range objs {
		if obj.IsEC() {
			plan.Migrations = append(plan.Migrations, planEC(coord, obj, class)...)
			continue
		}
		plan.Migrations = append(plan.Migrations, planChunks(coord, obj, class)...)
	}
	return plan, nil
}

// classedDNs returns the class-filtered DN list for obj, or nil if class
// filtering doesn't apply (no resolver, empty class label, or fewer DNs in
// that class than R). Caller falls back to the full DN set when nil.
func classedDNs(class ClassResolver, label string, want int) []string {
	if class == nil || label == "" {
		return nil
	}
	dns, err := class.ListRuntimeDNsByClass(label)
	if err != nil || len(dns) < want {
		return nil
	}
	return dns
}

func planChunks(coord Coordinator, obj *store.ObjectMeta, class ClassResolver) []Migration {
	var out []Migration
	// Replication factor inferred from any chunk's existing replica count;
	// if the object has no chunks yet, fall back to the class subset's full
	// length later. For a class-tagged object, compute desired by picking
	// from the class subset so off-class DNs never appear in Desired.
	r := 0
	for _, c := range obj.Chunks {
		if len(c.Replicas) > r {
			r = len(c.Replicas)
		}
	}
	subset := classedDNs(class, obj.Class, r)
	for ci, chunk := range obj.Chunks {
		var desired []string
		if subset != nil {
			desired = sortedCopy(coord.PlaceNFromAddrs(chunk.ChunkID, len(chunk.Replicas), subset))
		} else {
			desired = sortedCopy(coord.PlaceChunk(chunk.ChunkID))
		}
		actual := sortedCopy(chunk.Replicas)
		missing := setDiff(desired, actual)
		surplus := setDiff(actual, desired)
		if len(missing) == 0 {
			continue
		}
		out = append(out, Migration{
			Bucket: obj.Bucket, Key: obj.Key, Kind: KindChunk,
			ChunkID: chunk.ChunkID, Size: chunk.Size,
			ChunkIndex: ci,
			Actual:     actual, Desired: desired,
			Missing: missing, Surplus: surplus,
		})
	}
	return out
}

// planEC implements ADR-024 set-based stripe rebalance:
// for each stripe, find shards whose addr is no longer in the desired set and
// schedule them onto sorted unused desired-set slots (deterministic).
//
// P5-04: when obj.Class is set and the resolver returns ≥ K+M DNs of that
// class, desired placement is computed against the class subset only.
func planEC(coord Coordinator, obj *store.ObjectMeta, class ClassResolver) []Migration {
	var out []Migration
	want := obj.EC.K + obj.EC.M
	subset := classedDNs(class, obj.Class, want)
	for si, stripe := range obj.Stripes {
		var desired []string
		if subset != nil {
			desired = coord.PlaceNFromAddrs(stripe.StripeID, want, subset)
		} else {
			desired = coord.PlaceN(stripe.StripeID, want)
		}
		desiredSet := make(map[string]struct{}, len(desired))
		for _, a := range desired {
			desiredSet[a] = struct{}{}
		}
		actualSet := make(map[string]struct{}, len(stripe.Shards))
		for _, sh := range stripe.Shards {
			if len(sh.Replicas) > 0 {
				actualSet[sh.Replicas[0]] = struct{}{}
			}
		}
		// unused = desired - actual, sorted for deterministic assignment.
		var unused []string
		for _, a := range desired {
			if _, in := actualSet[a]; !in {
				unused = append(unused, a)
			}
		}
		sort.Strings(unused)

		for shi, sh := range stripe.Shards {
			if len(sh.Replicas) == 0 {
				continue
			}
			addr := sh.Replicas[0]
			if _, ok := desiredSet[addr]; ok {
				continue
			}
			if len(unused) == 0 {
				continue
			}
			newAddr := unused[0]
			unused = unused[1:]
			out = append(out, Migration{
				Bucket: obj.Bucket, Key: obj.Key, Kind: KindShard,
				ChunkID: sh.ChunkID, Size: sh.Size,
				StripeIndex: si, ShardIndex: shi,
				OldAddr:     addr, NewAddr: newAddr,
			})
		}
	}
	return out
}

// Run executes the plan with the given concurrency.
// concurrency <= 0 falls back to 1.
//
// 한 청크 처리 순서:
//  1. Actual DN 중 한 곳에서 청크 GET (Coordinator.ReadChunk 가 첫 성공 반환)
//  2. Missing 의 각 DN 으로 PutChunkTo
//  3. 성공한 dst 만 unique(actual ∪ successful) 로 메타 갱신
//
// 모든 Missing 이 성공해야 Migrated++, 그렇지 않으면 Failed++.
// 부분 성공도 메타는 갱신함 — 다음 run 이 나머지 시도 가능.
func Run(ctx context.Context, coord Coordinator, st ObjectStore, plan Plan, concurrency int) RunStats {
	if concurrency <= 0 {
		concurrency = 1
	}
	stats := RunStats{}
	if len(plan.Migrations) == 0 {
		return stats
	}

	resultCh := make(chan migrateResult, len(plan.Migrations))

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, mg := range plan.Migrations {
		wg.Add(1)
		mg := mg // capture
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			resultCh <- migrateOne(ctx, coord, st, mg)
		}()
	}
	go func() { wg.Wait(); close(resultCh) }()

	for r := range resultCh {
		if r.ok {
			stats.Migrated++
		} else {
			stats.Failed++
		}
		stats.BytesCopied += r.bytesCopied
		if r.err != "" {
			stats.Errors = append(stats.Errors, r.err)
		}
	}
	return stats
}

type migrateResult struct {
	ok          bool
	bytesCopied int64
	err         string
}

func migrateOne(ctx context.Context, coord Coordinator, st ObjectStore, mg Migration) migrateResult {
	switch mg.Kind {
	case KindShard:
		return migrateShard(ctx, coord, st, mg)
	default:
		// KindChunk (or empty for legacy callers)
		return migrateChunk(ctx, coord, st, mg)
	}
}

func migrateChunk(ctx context.Context, coord Coordinator, st ObjectStore, mg Migration) (out migrateResult) {
	if len(mg.Actual) == 0 {
		out.err = fmt.Sprintf("%s/%s: no actual replicas to read from", mg.Bucket, mg.Key)
		return
	}
	data, _, err := coord.ReadChunk(ctx, mg.ChunkID, mg.Actual)
	if err != nil {
		out.err = fmt.Sprintf("%s/%s: read source: %v", mg.Bucket, mg.Key, err)
		return
	}

	successfulMissing := []string{}
	var copyErrs []string
	for _, dst := range mg.Missing {
		if err := coord.PutChunkTo(ctx, dst, mg.ChunkID, data); err != nil {
			copyErrs = append(copyErrs, fmt.Sprintf("%s: %v", dst, err))
			continue
		}
		successfulMissing = append(successfulMissing, dst)
		out.bytesCopied += int64(len(data))
	}

	// Final replica list — two regimes:
	//   - All Missing copied successfully → trim meta to Desired
	//     (drop Surplus DNs from meta; GC will clean their disk later).
	//     Result: meta == Desired exactly.
	//   - Any Missing copy failed → keep Actual ∪ successful (safety).
	//     We must not drop Surplus from meta because we can't guarantee
	//     R replicas without the surplus DN as a fallback. Next rebalance
	//     run will retry the failed copies.
	var finalReplicas []string
	if len(copyErrs) == 0 {
		finalReplicas = uniqueSorted(append(intersect(mg.Actual, mg.Desired), successfulMissing...))
	} else {
		finalReplicas = uniqueSorted(append(append([]string(nil), mg.Actual...), successfulMissing...))
	}

	if updateErr := st.UpdateChunkReplicas(mg.Bucket, mg.Key, mg.ChunkIndex, finalReplicas); updateErr != nil {
		out.err = fmt.Sprintf("%s/%s: update meta: %v", mg.Bucket, mg.Key, updateErr)
		return
	}

	if len(copyErrs) == 0 {
		out.ok = true
		return
	}
	out.err = fmt.Sprintf("%s/%s: partial copy: %s", mg.Bucket, mg.Key, joinErrs(copyErrs))
	return
}

// migrateShard moves one EC shard from OldAddr to NewAddr (ADR-024).
// Single-source single-destination — simpler than chunk migration.
func migrateShard(ctx context.Context, coord Coordinator, st ObjectStore, mg Migration) (out migrateResult) {
	if mg.OldAddr == "" || mg.NewAddr == "" {
		out.err = fmt.Sprintf("%s/%s shard %d/%d: missing OldAddr or NewAddr",
			mg.Bucket, mg.Key, mg.StripeIndex, mg.ShardIndex)
		return
	}
	data, _, err := coord.ReadChunk(ctx, mg.ChunkID, []string{mg.OldAddr})
	if err != nil {
		out.err = fmt.Sprintf("%s/%s shard %d/%d: read %s: %v",
			mg.Bucket, mg.Key, mg.StripeIndex, mg.ShardIndex, mg.OldAddr, err)
		return
	}
	if err := coord.PutChunkTo(ctx, mg.NewAddr, mg.ChunkID, data); err != nil {
		// Meta untouched; next cycle retries with idempotent PUT.
		out.err = fmt.Sprintf("%s/%s shard %d/%d: PUT %s: %v",
			mg.Bucket, mg.Key, mg.StripeIndex, mg.ShardIndex, mg.NewAddr, err)
		return
	}
	out.bytesCopied = int64(len(data))
	if err := st.UpdateShardReplicas(mg.Bucket, mg.Key, mg.StripeIndex, mg.ShardIndex, []string{mg.NewAddr}); err != nil {
		// Data is now over-replicated (old + new); next cycle re-detects and
		// retries the meta update (PUT is idempotent on the new DN).
		out.err = fmt.Sprintf("%s/%s shard %d/%d: update meta: %v",
			mg.Bucket, mg.Key, mg.StripeIndex, mg.ShardIndex, err)
		return
	}
	out.ok = true
	return
}

// ─── helpers ───

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// setDiff returns elements in a but not in b. Both must be sorted (or sorted before pass).
func setDiff(a, b []string) []string {
	bset := make(map[string]struct{}, len(b))
	for _, x := range b {
		bset[x] = struct{}{}
	}
	var out []string
	for _, x := range a {
		if _, ok := bset[x]; !ok {
			out = append(out, x)
		}
	}
	return out
}

// intersect returns elements present in both a and b.
func intersect(a, b []string) []string {
	bset := make(map[string]struct{}, len(b))
	for _, x := range b {
		bset[x] = struct{}{}
	}
	var out []string
	for _, x := range a {
		if _, ok := bset[x]; ok {
			out = append(out, x)
		}
	}
	return out
}

func uniqueSorted(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, x := range in {
		if _, ok := seen[x]; ok {
			continue
		}
		seen[x] = struct{}{}
		out = append(out, x)
	}
	sort.Strings(out)
	return out
}

func joinErrs(errs []string) string {
	out := ""
	for i, e := range errs {
		if i > 0 {
			out += "; "
		}
		out += e
	}
	return out
}

// ErrEmptyPlan signals Run was called with no migrations. Reported for diagnostics
// — callers may treat as success.
var ErrEmptyPlan = errors.New("rebalance: plan has no migrations")
