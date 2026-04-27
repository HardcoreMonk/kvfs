// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// placement_test는 3가지를 증명한다:
//
//   1. 결정적 — 같은 입력에 항상 같은 출력 (replication·read 일관성)
//   2. 균등 분포 — 청크가 노드에 고르게 퍼짐 (1개 노드에 몰리지 않음)
//   3. 낮은 변동 — 노드 추가/제거 시 전체의 1/N 근처만 이동
//
// 세 번째가 "consistent" 의 핵심. 숫자로 본다.
package placement

import (
	"fmt"
	"math"
	"testing"
)

// ───────────────────────────────────────────────────────
// 1. 결정적
// ───────────────────────────────────────────────────────

func TestPick_Deterministic(t *testing.T) {
	nodes := makeNodes(5)
	p := New(nodes)

	// 같은 chunkID로 두 번 호출 → 같은 결과
	a := p.Pick("my-chunk", 3)
	b := p.Pick("my-chunk", 3)
	if !sameNodes(a, b) {
		t.Fatalf("결정적이어야 함. got1=%v got2=%v", ids(a), ids(b))
	}
}

func TestPick_SizeConstraints(t *testing.T) {
	p := New(makeNodes(5))

	if got := p.Pick("x", 3); len(got) != 3 {
		t.Errorf("n=3 요청했는데 len=%d", len(got))
	}
	if got := p.Pick("x", 10); len(got) != 5 {
		t.Errorf("n=10 (> 노드 수) 요청했는데 len=%d, 전체=5여야 함", len(got))
	}
	if got := p.Pick("x", 0); len(got) != 0 {
		t.Errorf("n=0 이면 빈 slice, got len=%d", len(got))
	}
	if got := New(nil).Pick("x", 3); len(got) != 0 {
		t.Errorf("노드 없으면 빈 slice")
	}
}

// ───────────────────────────────────────────────────────
// 2. 균등 분포 — 많은 청크를 배치했을 때 어느 한 노드가 독식하지 않는다
// ───────────────────────────────────────────────────────

func TestPick_Uniform(t *testing.T) {
	const (
		nodeCount  = 10
		chunkCount = 10000
		replicas   = 3
	)
	p := New(makeNodes(nodeCount))

	// 각 노드가 "1순위" 로 몇 번 뽑혔는지 세기
	primary := make(map[string]int, nodeCount)
	for i := 0; i < chunkCount; i++ {
		picked := p.Pick(fmt.Sprintf("chunk-%d", i), replicas)
		primary[picked[0].ID]++
	}

	// 이상적 평균: chunkCount / nodeCount = 1000
	// 허용 오차: ±30% (통계적 변동 범위)
	expectedPerNode := chunkCount / nodeCount
	tolerance := int(float64(expectedPerNode) * 0.30)
	for id, count := range primary {
		if count < expectedPerNode-tolerance || count > expectedPerNode+tolerance {
			t.Errorf("%s가 1순위로 %d회 뽑힘. 기대 %d±%d 범위 벗어남",
				id, count, expectedPerNode, tolerance)
		}
	}
}

// ───────────────────────────────────────────────────────
// 3. "consistent" — 노드 추가 시 극히 일부만 이동
// ───────────────────────────────────────────────────────

// 핵심 이론:
//   N개 노드, replica R개 → N+1개로 추가 시 이동률 ≈ R/(N+1)
//   N=3, R=3 → 3/4 = 75%     (지금은 이 케이스가 모든 청크 영향)
//   N=10, R=3 → 3/11 = 27%
//   N=100, R=3 → 3/101 = 2.97%
//
// N이 클수록 안정적. 아래 테스트는 N=10 상태에서 이동률이
// 이론값 주변인지 확인.

func TestPick_LowDisruption_OnAdd(t *testing.T) {
	const (
		nodeCount  = 10
		chunkCount = 10000
		replicas   = 3
	)

	before := New(makeNodes(nodeCount))
	after := New(makeNodes(nodeCount + 1))

	moved := 0
	for i := 0; i < chunkCount; i++ {
		chunkID := fmt.Sprintf("chunk-%d", i)
		a := idSet(before.Pick(chunkID, replicas))
		b := idSet(after.Pick(chunkID, replicas))
		if !sameSet(a, b) {
			moved++
		}
	}

	ratio := float64(moved) / float64(chunkCount)
	theoretical := float64(replicas) / float64(nodeCount+1) // 3/11 ≈ 0.273
	// 실제 관찰값: 이론값이 "청크가 바뀐 비율" 의 lower bound.
	// 단 3개 중 1개만 바뀌어도 "이동" 으로 카운트하므로 실제는 약간 더 높게 나옴.
	// 허용 범위: 이론값 × 2.5 이내.
	if ratio > theoretical*2.5 {
		t.Errorf("노드 추가로 %.1f%% 이동. 이론 최대 ≈ %.1f%% × 2.5. 너무 많이 움직임",
			ratio*100, theoretical*100)
	}
	t.Logf("N=%d → %d 추가. 이동률 %.2f%% (이론 lower bound %.2f%%)",
		nodeCount, nodeCount+1, ratio*100, theoretical*100)
}

func TestPick_LowDisruption_OnRemove(t *testing.T) {
	const (
		nodeCount  = 10
		chunkCount = 10000
		replicas   = 3
	)

	before := New(makeNodes(nodeCount))

	// dn10 제거 (마지막)
	reduced := make([]Node, 0, nodeCount-1)
	for _, n := range before.Nodes() {
		if n.ID != "dn10" {
			reduced = append(reduced, n)
		}
	}
	after := New(reduced)

	moved := 0
	for i := 0; i < chunkCount; i++ {
		chunkID := fmt.Sprintf("chunk-%d", i)
		a := idSet(before.Pick(chunkID, replicas))
		b := idSet(after.Pick(chunkID, replicas))
		if !sameSet(a, b) {
			moved++
		}
	}

	// 직관: 제거된 노드를 복제 목록에 가지고 있던 청크만 재배치 필요.
	// 기댓값: R/N = 3/10 = 30% 의 청크가 dn10을 포함 → 이동.
	ratio := float64(moved) / float64(chunkCount)
	expected := float64(replicas) / float64(nodeCount) // 0.30
	// 허용 범위: 이론값 × 1.8
	if ratio > expected*1.8 {
		t.Errorf("노드 제거로 %.1f%% 이동. 이론 ≈ %.1f%% × 1.8. 너무 많이 움직임",
			ratio*100, expected*100)
	}
	t.Logf("N=%d → %d 제거. 이동률 %.2f%% (이론 %.2f%%)",
		nodeCount, nodeCount-1, ratio*100, expected*100)
}

// ───────────────────────────────────────────────────────
// 부가: 노드 집합이 같으면 순서 무관하게 같은 결과
// ───────────────────────────────────────────────────────

func TestPick_OrderIndependent(t *testing.T) {
	a := makeNodes(5)
	b := make([]Node, len(a))
	copy(b, a)
	// 역순으로
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}

	pa := New(a).Pick("x", 3)
	pb := New(b).Pick("x", 3)
	if !sameSet(idSet(pa), idSet(pb)) {
		t.Errorf("노드 입력 순서만 바꿨는데 결과 변함. a=%v b=%v", ids(pa), ids(pb))
	}
}

// ───────────────────────────────────────────────────────
// 부가: hrwScore 는 SHA-256 기반으로 충돌 확률 매우 낮음
// ───────────────────────────────────────────────────────

func TestHrwScore_Spread(t *testing.T) {
	const n = 1000
	seen := make(map[uint64]bool, n)
	for i := 0; i < n; i++ {
		s := hrwScore("chunk", fmt.Sprintf("node-%d", i))
		if seen[s] {
			t.Fatalf("score 충돌 발견 at i=%d. SHA-256이라면 사실상 불가능", i)
		}
		seen[s] = true
	}
}

// ───────────────────────────────────────────────────────
// 테스트 헬퍼
// ───────────────────────────────────────────────────────

func makeNodes(n int) []Node {
	out := make([]Node, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("dn%d", i+1)
		out[i] = Node{ID: id, Addr: id + ":8080"}
	}
	return out
}

// makeNodesInDomains: build N DN, distributing them round-robin across the
// given domains. e.g. makeNodesInDomains(6, "rack1", "rack2", "rack3")
// returns dn1@rack1, dn2@rack2, dn3@rack3, dn4@rack1, dn5@rack2, dn6@rack3.
func makeNodesInDomains(n int, domains ...string) []Node {
	out := make([]Node, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("dn%d", i+1)
		out[i] = Node{ID: id, Addr: id + ":8080", Domain: domains[i%len(domains)]}
	}
	return out
}

// TestPickByDomain_DistinctDomainsWhenPossible (ADR-051): when n ≤ distinct
// domain count, every replica must land in a different domain. Probes a
// representative key population to catch any chunk that lands all R in
// one rack.
func TestPickByDomain_DistinctDomainsWhenPossible(t *testing.T) {
	// 6 nodes in 3 domains: 2 per domain, R=3 → expect each replica in a
	// distinct domain for every chunk.
	nodes := makeNodesInDomains(6, "rack1", "rack2", "rack3")
	p := New(nodes)
	for i := 0; i < 100; i++ {
		picked := p.PickByDomain(fmt.Sprintf("chunk-%d", i), 3)
		if len(picked) != 3 {
			t.Fatalf("chunk-%d: want 3 picks, got %d", i, len(picked))
		}
		seen := make(map[string]bool, 3)
		for _, nd := range picked {
			if seen[nd.Domain] {
				t.Errorf("chunk-%d: domain %q used twice (picks=%v)", i, nd.Domain, ids(picked))
			}
			seen[nd.Domain] = true
		}
	}
}

// TestPickByDomain_FallsBackWhenDomainsScarce: if R > distinct domain
// count, the first D picks span all domains, the rest fill in by score
// (allowing same-domain duplicates but never the same node twice).
func TestPickByDomain_FallsBackWhenDomainsScarce(t *testing.T) {
	// 6 nodes in 2 domains (3 per domain). R=3, so domain 1 must take 2
	// replicas + domain 2 takes 1 (or vice versa).
	nodes := makeNodesInDomains(6, "rack1", "rack2")
	p := New(nodes)
	for i := 0; i < 100; i++ {
		picked := p.PickByDomain(fmt.Sprintf("chunk-%d", i), 3)
		if len(picked) != 3 {
			t.Fatalf("chunk-%d: want 3 picks, got %d", i, len(picked))
		}
		// Both domains must appear at least once.
		seenD := make(map[string]int, 2)
		for _, nd := range picked {
			seenD[nd.Domain]++
		}
		if len(seenD) != 2 {
			t.Errorf("chunk-%d: only %d domains spanned (want 2): %v", i, len(seenD), ids(picked))
		}
		// No node duplicated.
		seenID := make(map[string]bool, 3)
		for _, nd := range picked {
			if seenID[nd.ID] {
				t.Errorf("chunk-%d: node %q picked twice", i, nd.ID)
			}
			seenID[nd.ID] = true
		}
	}
}

// TestPickByDomain_BackCompat_NoDomainTags: when no node has Domain set,
// PickByDomain must return identical results to legacy Pick — guarantees
// that adding the API does not silently change behavior on existing
// clusters before they tag domains.
func TestPickByDomain_BackCompat_NoDomainTags(t *testing.T) {
	nodes := makeNodes(5) // all empty Domain
	p := New(nodes)
	for i := 0; i < 50; i++ {
		key := fmt.Sprintf("chunk-%d", i)
		legacy := p.Pick(key, 3)
		domain := p.PickByDomain(key, 3)
		if !sameNodes(legacy, domain) {
			t.Errorf("chunk-%s: PickByDomain diverges from Pick when no domains set\n  legacy=%v\n  domain=%v",
				key, ids(legacy), ids(domain))
		}
	}
}

func ids(nodes []Node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.ID
	}
	return out
}

func idSet(nodes []Node) map[string]bool {
	out := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		out[n.ID] = true
	}
	return out
}

func sameNodes(a, b []Node) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID {
			return false
		}
	}
	return true
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// 비통계 참조 (테스트에서 쓰지 않지만 교육 목적)
var _ = math.Sqrt // keep math import if we expand tests later
