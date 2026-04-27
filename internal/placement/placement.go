// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Package placement picks N storage nodes for a given chunk using
// Rendezvous Hashing (a.k.a. HRW — Highest Random Weight).
//
// ──────────────────────────────────────────────────────────────────
// 비전공자용 해설
// ──────────────────────────────────────────────────────────────────
//
// 풀어야 할 문제
// ─────────────
// "청크 하나를 어느 DN 3개에 복제할까?"
//
// 제약:
//   1. 같은 청크는 **항상 같은 DN 3개** 에 배치되어야 함
//      (저장할 때 dn1·dn2·dn3 에 저장했으면, 읽을 때도 dn1·dn2·dn3 에서 찾음)
//   2. DN 을 **추가/제거** 해도 기존 청크의 대부분은 그 자리에 있어야 함
//      (아니면 DN 하나 추가할 때마다 전체 청크가 이사 → 운영 불가)
//
// 가장 단순한 답 (안 됨)
// ─────────────────────
// "무작위 3개 고르기"
//   → 같은 청크 다시 찾을 때 어디 넣었는지 기억 필요. 메타DB 부담.
//
// Rendezvous Hashing 의 답
// ──────────────────────────
// **각 DN에 대해 청크와의 '점수' 를 계산하고, 점수 높은 N개 선택.**
//
//   점수 = hash(chunk_id + dn_id)     // 같은 입력 → 항상 같은 점수
//
// 모든 DN이 같은 규칙을 따르므로, 어떤 DN 3개가 선택될지
// chunk_id · DN 목록만 있으면 **누구나 같은 결정** 을 내린다.
// 메타DB에 replica 목록 저장 없이도 재현 가능.
//
// 구체 예시 (DN 5개, 청크 "abc", 복제 3개)
// ─────────────────────────────────────────
//
//   hash("abc" + "dn1") = 0x1234
//   hash("abc" + "dn2") = 0xabcd   ← 2위
//   hash("abc" + "dn3") = 0x5678
//   hash("abc" + "dn4") = 0xfedc   ← 1위  🥇
//   hash("abc" + "dn5") = 0x9876   ← 3위
//                         ────────
//   상위 3개 선택        →  [dn4, dn2, dn5]
//
// 이 결정은 chunk_id="abc" 가 바뀌지 않는 한 **영원히 동일**.
//
// 왜 "consistent" 인가
// ────────────────────
// DN 추가 시 (dn5→dn6 추가, 6개):
//   - 각 청크에 대해 새 DN(dn6)과 점수 재계산
//   - 새 DN의 점수가 기존 상위 3에 안 들어가면 → 기존 3개 그대로, 이동 없음
//   - 들어가면 → 가장 낮았던 기존 DN 하나 밀려남 → **1개 청크 이동**
//
// 통계적으로 평균 **3/6 = 50% 의 청크가 영향**, 그러나 실제 이동은
// **1/6 ≈ 16.7%**. 공식: N→N+1 시 **R/(N+1)** (R=replica 수)
//
// 기존 DN 제거 시도 동일 논리. 이것이 "consistent" 의 의미:
// **전체가 흔들리지 않는다**.
//
// Rendezvous vs 고전 "링 기반" 일관 해싱
// ─────────────────────────────────────
// 고전 링 (Dynamo·Cassandra):
//   - DN을 가상 노드로 링에 흩뿌림
//   - chunk hash → 시계방향 가장 가까운 DN
//   - 균등 분포 위해 DN당 virtual node 100~1000개 필요
//
// Rendezvous (이 패키지):
//   - 각 조합마다 점수 계산 (O(N) per lookup)
//   - virtual node 불필요
//   - 코드 20줄, 균등 분포 자동
//   - N이 작을 때 (수백 이하) 성능 충분
//
// kvfs의 규모(수십 DN 이하)에선 Rendezvous가 **압도적으로 단순**하고
// **디버깅이 쉽다**. 블로그 독자 이해에도 최적.
//
// 알고리즘 자체는 1998년 논문 "A Name-Based Mapping Scheme for Rendezvous".
// 20+년 검증된 표준. Ceph CRUSH, Cassandra 같은 시스템도
// Rendezvous 변종을 내부에서 사용.
package placement

import (
	"crypto/sha256"
	"encoding/binary"
	"sort"
)

// Node represents a storage target (DN).
// ID is a stable human-readable identifier (e.g. "dn1").
// Addr is the network endpoint for actual I/O (e.g. "dn1:8080").
// Domain is an optional failure-domain label (e.g. "rack1", "us-west-2a")
// — when set, [PickFromNodesByDomain] spreads replicas across distinct
// values to avoid storing all R replicas in the same rack/AZ. Empty
// Domain is treated as "default" — back-compat with pre-S7 nodes that
// don't carry a domain tag.
//
// ID가 점수 계산의 키이므로 **재시작 후에도 변하지 않아야** 한다.
// Addr은 네트워크 경로만 바꾸면 되므로 변경 가능.
// Domain 은 운영자 지정 — 같은 rack/AZ 의 DN 들이 동일 값을 가지면
// HRW 가 자동으로 다른 domain 을 우선 골라 R replica 가 한 도메인에
// 다 가지 않도록 한다 (CRUSH 의 rack-aware 의 단순 등가물).
type Node struct {
	ID     string
	Addr   string
	Domain string
}

// Placer picks N nodes for a given chunk.
//
// Placer는 노드 목록을 읽기 전용으로 보관하고, 호출될 때마다 상위 N개를
// 골라 돌려준다. 상태 없음 — 동일 입력에 동일 출력.
type Placer struct {
	nodes []Node
}

// New creates a Placer over the given node set.
// 노드 목록은 Placer가 내부 복사본으로 보관한다. 외부에서 slice를
// 수정해도 Placer의 결정이 바뀌지 않도록.
func New(nodes []Node) *Placer {
	copied := make([]Node, len(nodes))
	copy(copied, nodes)
	return &Placer{nodes: copied}
}

// Nodes returns the configured node set (read-only copy).
func (p *Placer) Nodes() []Node {
	out := make([]Node, len(p.nodes))
	copy(out, p.nodes)
	return out
}

// Pick selects n nodes for the given chunkID, chosen by Rendezvous Hashing.
//
// Returns nodes ordered by descending score (highest first).
// If n > len(nodes), returns all nodes.
// If n == 0 or no nodes, returns an empty slice.
//
// ───────────────────────────────────────────────────────
// 핵심 단계 (주석으로 풀어 쓴 알고리즘)
// ───────────────────────────────────────────────────────
//
//  1. 각 노드에 대해 'score = hash(chunkID + nodeID)' 계산
//  2. score 내림차순 정렬
//  3. 상위 n개 반환
//
// 보는 사람에 따라 "겨우 이게 분산 스토리지 라우팅?" 싶을 만큼 단순하다.
// 실제 프로덕션(Ceph CRUSH 등)도 이 핵심을 공유한다 — 단지 추가로
// rack-awareness, failure domain 등 조건을 얹는 것뿐.
func (p *Placer) Pick(chunkID string, n int) []Node {
	return PickFromNodes(chunkID, n, p.nodes)
}

// PickFromNodes runs the same HRW algorithm against an arbitrary node
// subset (e.g., hot-tier filtered list). Used by tiered-placement code in
// edge that wants to bias new writes toward a class without rebuilding a
// whole Placer. Stateless / pure function.
func PickFromNodes(chunkID string, n int, nodes []Node) []Node {
	if n <= 0 || len(nodes) == 0 {
		return nil
	}
	if n > len(nodes) {
		n = len(nodes)
	}

	// 1. 각 노드의 점수 계산
	type scored struct {
		node  Node
		score uint64
	}
	scoreds := make([]scored, len(nodes))
	for i, node := range nodes {
		scoreds[i] = scored{node: node, score: hrwScore(chunkID, node.ID)}
	}

	// 2. 점수 내림차순 정렬
	//    동점일 때는 ID를 tiebreaker로 써서 결정적으로 만듦.
	//    (동점 확률은 2^64 분의 1 이라 사실상 안 일어나지만 안전장치)
	sort.Slice(scoreds, func(i, j int) bool {
		if scoreds[i].score != scoreds[j].score {
			return scoreds[i].score > scoreds[j].score
		}
		return scoreds[i].node.ID < scoreds[j].node.ID
	})

	// 3. 상위 n개 반환
	out := make([]Node, n)
	for i := 0; i < n; i++ {
		out[i] = scoreds[i].node
	}
	return out
}

// PickFromNodesByDomain runs HRW with a failure-domain diversity
// constraint (ADR-051, S7 Ep.1). When any node has Domain != "", the
// pick walks scored-descending nodes and prefers ones whose Domain has
// not been used yet — only after every distinct domain is represented
// does it allow a second pick from the same domain.
//
// Behavior:
//   - All nodes have empty Domain → identical to PickFromNodes (back-compat).
//   - n ≤ distinct domain count → every replica in a different domain.
//   - n > distinct domain count → the first D picks span all domains,
//     remaining n-D picks fill in by raw HRW score (still deterministic).
//
// Determinism: same (chunkID, node-set) → same output. The greedy domain
// walk is fully ordered by score, then by ID as the tiebreaker — no
// randomness, no map iteration order in the result.
func PickFromNodesByDomain(chunkID string, n int, nodes []Node) []Node {
	if n <= 0 || len(nodes) == 0 {
		return nil
	}
	if n > len(nodes) {
		n = len(nodes)
	}

	// Fast path: no domain info anywhere → score-only (identical result
	// to PickFromNodes; saves the diversity walk).
	hasDomain := false
	for _, nd := range nodes {
		if nd.Domain != "" {
			hasDomain = true
			break
		}
	}
	if !hasDomain {
		return PickFromNodes(chunkID, n, nodes)
	}

	type scored struct {
		node  Node
		score uint64
	}
	scoreds := make([]scored, len(nodes))
	for i, nd := range nodes {
		scoreds[i] = scored{node: nd, score: hrwScore(chunkID, nd.ID)}
	}
	sort.Slice(scoreds, func(i, j int) bool {
		if scoreds[i].score != scoreds[j].score {
			return scoreds[i].score > scoreds[j].score
		}
		return scoreds[i].node.ID < scoreds[j].node.ID
	})

	out := make([]Node, 0, n)
	usedDomain := make(map[string]bool, n)
	usedID := make(map[string]bool, n)

	// First pass: pick the top-scored node per distinct domain.
	for _, s := range scoreds {
		if len(out) >= n {
			break
		}
		if usedDomain[s.node.Domain] {
			continue
		}
		out = append(out, s.node)
		usedDomain[s.node.Domain] = true
		usedID[s.node.ID] = true
	}

	// Second pass (only if n > distinct-domain count): fill remaining
	// slots by raw score, allowing same-domain picks but not the same
	// node twice.
	if len(out) < n {
		for _, s := range scoreds {
			if len(out) >= n {
				break
			}
			if usedID[s.node.ID] {
				continue
			}
			out = append(out, s.node)
			usedID[s.node.ID] = true
		}
	}
	return out
}

// PickByDomain is the Placer's domain-aware variant of Pick. Uses the
// same node set the Placer was constructed with.
func (p *Placer) PickByDomain(chunkID string, n int) []Node {
	return PickFromNodesByDomain(chunkID, n, p.nodes)
}

// hrwScore computes the HRW score for a (chunkID, nodeID) pair.
//
// 공식:
//   score = uint64(sha256(chunkID + "|" + nodeID)[0:8])
//
// SHA-256의 첫 8바이트만 사용해 uint64로 해석한다.
// 전체 해시를 쓸 필요 없음 — 비교에는 64비트 충분.
//
// 구분자("|")는 chunkID와 nodeID의 경계를 명확히 해 collision 방지:
//   예: chunkID="ab", nodeID="c"  vs  chunkID="a", nodeID="bc"
//   구분자 없이 이어붙이면 둘 다 "abc" 로 동일. 버그 발생.
func hrwScore(chunkID, nodeID string) uint64 {
	h := sha256.New()
	h.Write([]byte(chunkID))
	h.Write([]byte("|"))
	h.Write([]byte(nodeID))
	sum := h.Sum(nil)
	return binary.BigEndian.Uint64(sum[:8])
}
