// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Package gc deletes surplus chunks: on-disk chunks no object's metadata claims.
//
// ──────────────────────────────────────────────────────────────────
// 비전공자용 해설
// ──────────────────────────────────────────────────────────────────
//
// 풀어야 할 문제 (ADR-012 의 동기)
// ────────────────────────────────
// ADR-010 (Rebalance worker) 의 안전 규칙 "copy-then-update, never delete"
// 덕분에 마이그레이션 도중 데이터 손실 위험은 0 이지만, **잉여 청크가 누적**
// 된다. dn2 가 들고 있는 청크인데 메타는 [dn1, dn3, dn4] 만 가리키는 상태.
// 누적되면 디스크가 새고, dedup 의도와도 어긋난다.
//
// GC 의 책임은 단 하나:
//
//   "메타가 단 하나도 가리키지 않는 디스크 청크" 를 안전하게 삭제
//
// 두 단계 안전망
// ──────────────
// 1. **claimed-set** — 메타 스냅샷에서 (chunk_id, addr) 페어 집합 구성.
//    이 집합에 든 청크는 절대 삭제 금지. dedup 친화적: 같은 chunk_id 가
//    다른 객체의 replica 로도 등장하면 모두 보호됨
// 2. **min-age** — 청크 mtime 기준 N 초 이내면 보호. race window 차단:
//    GC 가 list 를 찍은 직후 PUT 이 들어와 메타가 갱신될 수도 있다.
//    그 PUT 의 chunk_id 가 우연히 GC 대상이라면? min-age 가 그 PUT 을 보호
//
// 두 안전망은 AND 결합. 한쪽이라도 보호하면 삭제 안 함.
//
// rebalance 와의 관계
// ──────────────────
// rebalance 는 메타 → 디스크 방향으로 동기화 (없는 곳에 복사).
// gc 는 디스크 → 메타 방향으로 동기화 (메타가 모르는 거 삭제).
// 둘이 직교하므로 동시 실행도 안전.
//
// 인터페이스 분리
// ──────────────
// rebalance 와 같은 패턴: Coordinator·ObjectStore 를 인터페이스로 받아
// fake 주입 테스트 가능. 같은 가독성·테스트성 트레이드오프.
package gc

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/HardcoreMonk/kvfs/internal/coordinator"
	"github.com/HardcoreMonk/kvfs/internal/store"
)

// DefaultMinAge is the default mtime grace window. 60 seconds is conservative
// for a demo: covers normal PUT → meta-update propagation, and gives the
// operator time to abort a misfire.
const DefaultMinAge = 60 * time.Second

// Coordinator is the subset used by GC.
type Coordinator interface {
	// DNs returns the addresses currently configured for the cluster.
	DNs() []string
	// ListChunks enumerates on-disk chunks at addr.
	ListChunks(ctx context.Context, addr string) ([]coordinator.ChunkInfo, error)
	// DeleteChunkFrom removes a single chunk from a single DN. Idempotent.
	DeleteChunkFrom(ctx context.Context, addr, chunkID string) error
}

// ObjectStore is the subset used by GC.
type ObjectStore interface {
	ListObjects() ([]*store.ObjectMeta, error)
}

// Sweep is one chunk-on-DN slated for deletion.
type Sweep struct {
	Addr     string `json:"addr"`
	ChunkID  string `json:"chunk_id"`
	Size     int64  `json:"size"`
	AgeSec   int64  `json:"age_seconds"`
}

// Plan summarizes everything GC would do.
type Plan struct {
	Scanned     int     `json:"scanned"`        // 모든 DN 의 디스크 청크 합계
	ClaimedKeys int     `json:"claimed_keys"`   // 메타가 보호 중인 (chunk_id, addr) 페어 수
	Sweeps      []Sweep `json:"sweeps"`         // 삭제 대상
	GeneratedAt string  `json:"generated_at,omitempty"`
}

// RunStats reports outcome of executing a Plan.
type RunStats struct {
	Deleted     int      `json:"deleted"`
	Failed      int      `json:"failed"`
	BytesFreed  int64    `json:"bytes_freed"`
	Errors      []string `json:"errors,omitempty"`
}

// ComputePlan walks all objects + all DNs, returning sweeps that pass both
// safety nets (claimed-set AND min-age).
//
// minAge <= 0 falls back to DefaultMinAge.
func ComputePlan(ctx context.Context, coord Coordinator, st ObjectStore, minAge time.Duration) (Plan, error) {
	if minAge <= 0 {
		minAge = DefaultMinAge
	}

	objs, err := st.ListObjects()
	if err != nil {
		return Plan{}, fmt.Errorf("gc: list objects: %w", err)
	}
	claimed := buildClaimedSet(objs)

	plan := Plan{ClaimedKeys: countPairs(claimed)}
	cutoff := time.Now().Add(-minAge).Unix()

	for _, addr := range coord.DNs() {
		chunks, err := coord.ListChunks(ctx, addr)
		if err != nil {
			// Treat per-DN list failure as soft: include addr in errors and continue.
			// (We don't add to plan.Sweeps for this DN — under-scan is safer than over-delete.)
			return Plan{}, fmt.Errorf("gc: list chunks @ %s: %w", addr, err)
		}
		plan.Scanned += len(chunks)
		for _, c := range chunks {
			if claimed[c.ID][addr] {
				continue // protected by meta
			}
			if c.MTime > cutoff {
				continue // too fresh — race protection
			}
			plan.Sweeps = append(plan.Sweeps, Sweep{
				Addr:    addr,
				ChunkID: c.ID,
				Size:    c.Size,
				AgeSec:  time.Now().Unix() - c.MTime,
			})
		}
	}
	// Stable order helps human review of the plan output.
	sort.Slice(plan.Sweeps, func(i, j int) bool {
		if plan.Sweeps[i].Addr != plan.Sweeps[j].Addr {
			return plan.Sweeps[i].Addr < plan.Sweeps[j].Addr
		}
		return plan.Sweeps[i].ChunkID < plan.Sweeps[j].ChunkID
	})
	return plan, nil
}

// Run deletes the chunks listed in plan.Sweeps with the given concurrency.
// concurrency <= 0 falls back to 1.
//
// Per-sweep failure: counted in stats.Failed, captured in stats.Errors.
// Sucess: counted in stats.Deleted with bytes summed in BytesFreed.
func Run(ctx context.Context, coord Coordinator, plan Plan, concurrency int) RunStats {
	if concurrency <= 0 {
		concurrency = 1
	}
	stats := RunStats{}
	if len(plan.Sweeps) == 0 {
		return stats
	}

	type result struct {
		ok    bool
		bytes int64
		err   string
	}
	resultCh := make(chan result, len(plan.Sweeps))

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, s := range plan.Sweeps {
		wg.Add(1)
		s := s
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			err := coord.DeleteChunkFrom(ctx, s.Addr, s.ChunkID)
			if err != nil {
				resultCh <- result{
					err: fmt.Sprintf("%s/%s: %v", s.Addr, idPrefix(s.ChunkID), err),
				}
				return
			}
			resultCh <- result{ok: true, bytes: s.Size}
		}()
	}
	go func() { wg.Wait(); close(resultCh) }()

	for r := range resultCh {
		if r.ok {
			stats.Deleted++
			stats.BytesFreed += r.bytes
		} else {
			stats.Failed++
			if r.err != "" {
				stats.Errors = append(stats.Errors, r.err)
			}
		}
	}
	return stats
}

// ─── helpers ───

// buildClaimedSet returns map[chunkID]map[addr]bool of every (chunk, addr)
// pair that any object's metadata claims.
//
// Iterates both replication-mode chunks and EC-mode stripe shards. Same
// chunk_id may appear across multiple objects (dedup); their replica addrs
// union into one set.
func buildClaimedSet(objs []*store.ObjectMeta) map[string]map[string]bool {
	out := make(map[string]map[string]bool)
	add := func(c store.ChunkRef) {
		set, ok := out[c.ChunkID]
		if !ok {
			set = make(map[string]bool, len(c.Replicas))
			out[c.ChunkID] = set
		}
		for _, addr := range c.Replicas {
			set[addr] = true
		}
	}
	for _, o := range objs {
		for _, c := range o.Chunks {
			add(c)
		}
		for _, st := range o.Stripes {
			for _, sh := range st.Shards {
				add(sh)
			}
		}
	}
	return out
}

func countPairs(m map[string]map[string]bool) int {
	n := 0
	for _, s := range m {
		n += len(s)
	}
	return n
}

// idPrefix safely truncates a chunk_id for log messages.
// Real chunk_ids are 64 hex chars, but tests may use short fakes.
func idPrefix(id string) string {
	const max = 16
	if len(id) <= max {
		return id
	}
	return id[:max]
}
