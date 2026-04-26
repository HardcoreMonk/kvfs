// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Command kvfs-cli is the developer CLI:
//
//	kvfs-cli sign --method PUT --path /v1/o/bucket/key --ttl 1h
//	kvfs-cli inspect [--db ./edge-data/edge.db] [--object bucket/key]
//
// The secret for sign/verify is read from EDGE_URLKEY_SECRET (shared with kvfs-edge).
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/HardcoreMonk/kvfs/internal/placement"
	"github.com/HardcoreMonk/kvfs/internal/store"
	"github.com/HardcoreMonk/kvfs/internal/urlkey"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "sign":
		cmdSign(os.Args[2:])
	case "inspect":
		cmdInspect(os.Args[2:])
	case "placement-sim":
		cmdPlacementSim(os.Args[2:])
	case "rebalance":
		cmdRebalance(os.Args[2:])
	case "gc":
		cmdGC(os.Args[2:])
	case "auto":
		cmdAuto(os.Args[2:])
	case "dns":
		cmdDNs(os.Args[2:])
	case "urlkey":
		cmdURLKey(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `kvfs-cli — kvfs developer tool

Subcommands:
  sign            Generate an HMAC-signed presigned URL
  inspect         Dump bbolt metadata store contents
  placement-sim   Simulate Rendezvous Hashing: chunk distribution + rebalance
  rebalance       Migrate misplaced chunks to their HRW-desired DNs (ADR-010)
  gc              Delete surplus chunks no object's metadata claims (ADR-012)
  auto            Show auto-trigger config + recent run history (ADR-013)
  dns             list / add / remove DNs in the live registry (ADR-027)
  urlkey          list / rotate / remove signing kids (ADR-028)

Run 'kvfs-cli <subcommand> -h' for subcommand help.`)
}

// ---- sign ----

func cmdSign(args []string) {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	method := fs.String("method", "GET", "HTTP method")
	path := fs.String("path", "", "request path (e.g. /v1/o/bucket/key)")
	ttl := fs.Duration("ttl", 1*time.Hour, "signature TTL")
	base := fs.String("base", "", "optional base URL prepended to the result (e.g. http://localhost:8000)")
	fs.Parse(args)

	if *path == "" {
		fmt.Fprintln(os.Stderr, "sign: -path required")
		os.Exit(2)
	}
	secret := os.Getenv("EDGE_URLKEY_SECRET")
	if secret == "" {
		fmt.Fprintln(os.Stderr, "sign: EDGE_URLKEY_SECRET env required")
		os.Exit(2)
	}

	signer, err := urlkey.NewSigner([]byte(secret))
	if err != nil {
		fail(err)
	}
	rel := signer.BuildURL(strings.ToUpper(*method), *path, *ttl)
	if *base != "" {
		fmt.Println(strings.TrimRight(*base, "/") + rel)
	} else {
		fmt.Println(rel)
	}
}

// ---- inspect ----

func cmdInspect(args []string) {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	dbPath := fs.String("db", "./edge-data/edge.db", "bbolt file path")
	oneObj := fs.String("object", "", "limit to single object key (format: bucket/key)")
	fs.Parse(args)

	ms, err := store.Open(*dbPath)
	if err != nil {
		fail(err)
	}
	defer ms.Close()

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")

	if *oneObj != "" {
		parts := strings.SplitN(*oneObj, "/", 2)
		if len(parts) != 2 {
			fail(fmt.Errorf("-object must be bucket/key"))
		}
		obj, err := ms.GetObject(parts[0], parts[1])
		if err != nil {
			fail(err)
		}
		_ = enc.Encode(obj)
		return
	}

	objs, err := ms.ListObjects()
	if err != nil {
		fail(err)
	}
	dns, _ := ms.ListDNs()
	_ = enc.Encode(map[string]any{
		"objects": objs,
		"dns":     dns,
	})
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "kvfs-cli:", err)
	os.Exit(1)
}

// ---- placement-sim ----
//
// DN 개수를 바꿔가며 청크 분산·재배치 비율을 실측하는 교육용 도구.
//
// 예:
//   kvfs-cli placement-sim --nodes 10 --chunks 10000 --replicas 3 --add 1
//     → 10 DN 에 10000 chunks 배치, DN 1개 추가 시 이동률 출력
//   kvfs-cli placement-sim --nodes 10 --chunks 10000 --replicas 3 --remove 1
//     → 10 DN 에서 DN 1개 제거 시 이동률 출력
//
// Bar chart 해석 주의:
//   bar 의 % 값 = "그 DN 이 포함된 청크 비율". 한 청크가 R 개 DN 에 배치되므로
//   모든 DN 의 % 합 = R × 100% (R=3 이면 평균 300%).
//
//   특수 케이스 R=N (예: 3 DN + R=3): 모든 청크가 모든 DN 에 배치되므로
//   각 DN bar = **항상 100%**. 이는 분산 알고리즘이 무의미한 (선택지 1) 환경
//   에서 정상 동작 — 시각적으로 "DN 들이 다 꽉 찼다" 는 의미 아님. 효과는
//   N > R (예: 5 DN + R=3) 부터 관찰 가능하며, 평균은 R/N = 60% 근처로 수렴.
func cmdPlacementSim(args []string) {
	fs := flag.NewFlagSet("placement-sim", flag.ExitOnError)
	nodeCount := fs.Int("nodes", 5, "initial DN count")
	chunkCount := fs.Int("chunks", 1000, "number of simulated chunks")
	replicas := fs.Int("replicas", 3, "replication factor per chunk")
	add := fs.Int("add", 0, "how many DNs to add after initial placement")
	remove := fs.Int("remove", 0, "how many DNs to remove after initial placement")
	fs.Parse(args)

	if *nodeCount < 1 {
		fail(fmt.Errorf("--nodes must be >= 1"))
	}
	if *replicas < 1 || *replicas > *nodeCount {
		fail(fmt.Errorf("--replicas must be in [1, nodes]"))
	}
	if *add > 0 && *remove > 0 {
		fail(fmt.Errorf("specify either --add or --remove, not both"))
	}
	if *replicas == *nodeCount {
		fmt.Println("ℹ️  R = N: every chunk lands on every DN. Bars below show 100% per DN — that is")
		fmt.Println("    expected (no placement choice). Re-run with N > R to see the actual effect.")
		fmt.Println()
	}

	// 초기 노드 집합
	initialNodes := makeSimNodes(*nodeCount, 0)
	placerBefore := placement.New(initialNodes)

	fmt.Printf("Initial: %d DNs, placing %d chunks with replicas=%d\n",
		*nodeCount, *chunkCount, *replicas)
	distributionBefore := simulate(placerBefore, *chunkCount, *replicas)
	printDistribution(distributionBefore, *chunkCount)

	if *add == 0 && *remove == 0 {
		return
	}

	// 변경 후 노드 집합
	var afterNodes []placement.Node
	var action string
	switch {
	case *add > 0:
		afterNodes = append([]placement.Node{}, initialNodes...)
		afterNodes = append(afterNodes, makeSimNodes(*add, *nodeCount)...)
		action = fmt.Sprintf("After adding %d DN(s) → %d total", *add, len(afterNodes))
	case *remove > 0:
		if *remove >= *nodeCount {
			fail(fmt.Errorf("cannot remove >= %d DNs (would leave 0)", *nodeCount))
		}
		// 마지막 N개 제거
		afterNodes = append([]placement.Node{}, initialNodes[:*nodeCount-*remove]...)
		action = fmt.Sprintf("After removing %d DN(s) → %d total", *remove, len(afterNodes))
	}

	placerAfter := placement.New(afterNodes)

	// 이동률 계산
	moved := 0
	for i := 0; i < *chunkCount; i++ {
		chunkID := fmt.Sprintf("chunk-%d", i)
		before := idSetFromNodes(placerBefore.Pick(chunkID, *replicas))
		after := idSetFromNodes(placerAfter.Pick(chunkID, *replicas))
		if !sameSetKeys(before, after) {
			moved++
		}
	}

	fmt.Println()
	fmt.Println(action)
	distributionAfter := simulate(placerAfter, *chunkCount, *replicas)
	printDistribution(distributionAfter, *chunkCount)

	fmt.Println()
	ratio := float64(moved) / float64(*chunkCount)
	// 이론값
	var theoretical float64
	if *add > 0 {
		theoretical = float64(*replicas) / float64(*nodeCount+*add)
	} else {
		theoretical = float64(*replicas) / float64(*nodeCount)
	}
	fmt.Printf("📊 Chunks moved: %d / %d = %.2f%%\n", moved, *chunkCount, ratio*100)
	fmt.Printf("📐 Theoretical lower bound (R/N): %.2f%%\n", theoretical*100)
	fmt.Printf("💡 즉, 전체 흔들림 없이 필요한 최소한만 이동 (consistent hashing의 핵심)\n")
}

func makeSimNodes(count, startIdx int) []placement.Node {
	out := make([]placement.Node, count)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("dn%d", startIdx+i+1)
		out[i] = placement.Node{ID: id, Addr: id + ":8080"}
	}
	return out
}

// simulate counts how many times each DN is picked as a replica.
// distribution[ID] = 해당 DN이 포함된 청크 수.
func simulate(p *placement.Placer, chunks, replicas int) map[string]int {
	d := make(map[string]int)
	for _, n := range p.Nodes() {
		d[n.ID] = 0
	}
	for i := 0; i < chunks; i++ {
		picked := p.Pick(fmt.Sprintf("chunk-%d", i), replicas)
		for _, n := range picked {
			d[n.ID]++
		}
	}
	return d
}

func printDistribution(d map[string]int, chunks int) {
	ids := make([]string, 0, len(d))
	for id := range d {
		ids = append(ids, id)
	}
	// 숫자 접미사 기준 정렬 (dn1, dn2, dn3, ..., dn10, dn11, ...)
	sortByNumericSuffix(ids)

	for _, id := range ids {
		count := d[id]
		pct := float64(count) / float64(chunks) * 100
		bar := barChart(pct, 30)
		fmt.Printf("  %-6s %5d chunks (%5.1f%%) %s\n", id, count, pct, bar)
	}
}

// barChart renders pct (0~100) as a fixed-width bar.
//
// 100% (R=N 케이스 등) 는 width 가 꽉 찬 정상 결과 — clamp 는 방어 코드일 뿐
// 알고리즘 버그 indicator 가 아님. 자세한 해석은 cmdPlacementSim 의 패키지
// 주석 참조.
func barChart(pct float64, width int) string {
	filled := int(pct / 100 * float64(width))
	if filled > width {
		filled = width
	}
	if filled < 0 {
		filled = 0
	}
	empty := width - filled
	return "│" + repeat("█", filled) + repeat("·", empty) + "│"
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

func sortByNumericSuffix(ids []string) {
	// 간단한 bubble sort — 비전공자도 읽을 수 있게
	for i := 0; i < len(ids); i++ {
		for j := i + 1; j < len(ids); j++ {
			if numSuffix(ids[i]) > numSuffix(ids[j]) {
				ids[i], ids[j] = ids[j], ids[i]
			}
		}
	}
}

func numSuffix(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n
}

func idSetFromNodes(nodes []placement.Node) map[string]bool {
	out := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		out[n.ID] = true
	}
	return out
}

func sameSetKeys(a, b map[string]bool) bool {
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

// ---- rebalance ----
//
// Talks to a running edge over HTTP:
//   POST /v1/admin/rebalance/plan        — read-only, returns Plan JSON
//   POST /v1/admin/rebalance/apply?...   — runs migration, returns RunStats JSON
//
// Use --plan first to preview. --apply executes (idempotent: safe to re-run).
func cmdRebalance(args []string) {
	fs := flag.NewFlagSet("rebalance", flag.ExitOnError)
	edge := fs.String("edge", "http://localhost:8000", "edge base URL")
	doPlan := fs.Bool("plan", false, "show migration plan only (no changes)")
	doApply := fs.Bool("apply", false, "execute the plan")
	concurrency := fs.Int("concurrency", 4, "parallel chunk copies during apply")
	verbose := fs.Bool("v", false, "print every migration entry (otherwise just summary)")
	fs.Parse(args)

	if !*doPlan && !*doApply {
		fmt.Fprintln(os.Stderr, "rebalance: specify --plan or --apply")
		os.Exit(2)
	}
	if *doPlan && *doApply {
		fmt.Fprintln(os.Stderr, "rebalance: --plan and --apply are mutually exclusive")
		os.Exit(2)
	}

	if *doPlan {
		runRebalancePlan(*edge, *verbose)
	} else {
		runRebalanceApply(*edge, *concurrency, *verbose)
	}
}

type planMigration struct {
	Bucket      string   `json:"bucket"`
	Key         string   `json:"key"`
	Kind        string   `json:"kind"`
	ChunkID     string   `json:"chunk_id"`
	Size        int64    `json:"size"`
	ChunkIndex  int      `json:"chunk_index"`
	Actual      []string `json:"actual"`
	Desired     []string `json:"desired"`
	Missing     []string `json:"missing"`
	Surplus     []string `json:"surplus"`
	StripeIndex int      `json:"stripe_index"`
	ShardIndex  int      `json:"shard_index"`
	OldAddr     string   `json:"old_addr"`
	NewAddr     string   `json:"new_addr"`
}

func runRebalancePlan(edge string, verbose bool) {
	url := strings.TrimRight(edge, "/") + "/v1/admin/rebalance/plan"
	body := mustPost(url)
	var plan struct {
		Scanned    int              `json:"scanned"`
		Migrations []planMigration  `json:"migrations"`
	}
	if err := json.Unmarshal(body, &plan); err != nil {
		fail(fmt.Errorf("decode plan: %w", err))
	}

	chunks, shards := 0, 0
	for _, m := range plan.Migrations {
		if m.Kind == "shard" {
			shards++
		} else {
			chunks++
		}
	}

	fmt.Printf("📋 Rebalance plan — scanned %d objects, %d total migrations (%d chunks, %d EC shards)\n",
		plan.Scanned, len(plan.Migrations), chunks, shards)
	if len(plan.Migrations) == 0 {
		fmt.Println("   ✅ Cluster is in HRW-desired state. No work to do.")
		return
	}

	limit := len(plan.Migrations)
	if !verbose && limit > 5 {
		limit = 5
	}
	fmt.Println()
	if !verbose {
		fmt.Printf("First %d migrations:\n", limit)
	}
	for i := 0; i < limit; i++ {
		printMigration(plan.Migrations[i], verbose)
	}
	if !verbose && len(plan.Migrations) > limit {
		fmt.Printf("  ... and %d more (use -v to list all)\n", len(plan.Migrations)-limit)
	}

	fmt.Println()
	fmt.Println("Next:  kvfs-cli rebalance --apply")
}

func printMigration(m planMigration, verbose bool) {
	if m.Kind == "shard" {
		fmt.Printf("  [shard]  %s/%s  stripe=%d shard=%d  size=%d  %s → %s\n",
			m.Bucket, m.Key, m.StripeIndex, m.ShardIndex, m.Size, m.OldAddr, m.NewAddr)
		if verbose {
			fmt.Printf("           chunk=%s..\n", idShort(m.ChunkID))
		}
		return
	}
	if !verbose {
		fmt.Printf("  [chunk]  %s/%s  missing=%v  (currently on %v)\n",
			m.Bucket, m.Key, m.Missing, m.Actual)
		return
	}
	fmt.Printf("  [chunk]  %s/%s  size=%d  chunk=%s..\n", m.Bucket, m.Key, m.Size, idShort(m.ChunkID))
	fmt.Printf("    actual:  %s\n", strings.Join(m.Actual, ", "))
	fmt.Printf("    desired: %s\n", strings.Join(m.Desired, ", "))
	fmt.Printf("    missing: %s\n", strings.Join(m.Missing, ", "))
	if len(m.Surplus) > 0 {
		fmt.Printf("    surplus: %s  (kept — never delete; GC cleans)\n", strings.Join(m.Surplus, ", "))
	}
}

func runRebalanceApply(edge string, concurrency int, verbose bool) {
	url := fmt.Sprintf("%s/v1/admin/rebalance/apply?concurrency=%d",
		strings.TrimRight(edge, "/"), concurrency)
	body := mustPost(url)
	var stats struct {
		Scanned     int      `json:"scanned"`
		PlanSize    int      `json:"plan_size"`
		Concurrency int      `json:"concurrency"`
		Migrated    int      `json:"migrated"`
		Failed      int      `json:"failed"`
		BytesCopied int64    `json:"bytes_copied"`
		Errors      []string `json:"errors"`
	}
	if err := json.Unmarshal(body, &stats); err != nil {
		fail(fmt.Errorf("decode apply result: %w", err))
	}

	fmt.Printf("⚙️  Rebalance applied (concurrency=%d)\n", stats.Concurrency)
	fmt.Printf("   scanned:      %d\n", stats.Scanned)
	fmt.Printf("   planned:      %d\n", stats.PlanSize)
	fmt.Printf("   migrated:     %d\n", stats.Migrated)
	fmt.Printf("   failed:       %d\n", stats.Failed)
	fmt.Printf("   bytes_copied: %d\n", stats.BytesCopied)
	if verbose && len(stats.Errors) > 0 {
		fmt.Println("   errors:")
		for _, e := range stats.Errors {
			fmt.Printf("     - %s\n", e)
		}
	}
	if stats.Failed > 0 {
		os.Exit(1)
	}
}

func mustPost(url string) []byte { return mustHTTP(http.MethodPost, url) }
func mustGet(url string) []byte  { return mustHTTP(http.MethodGet, url) }

func mustHTTP(method, url string) []byte {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		fail(err)
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		fail(fmt.Errorf("%s %s: %w", method, url, err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fail(err)
	}
	if resp.StatusCode != http.StatusOK {
		fail(fmt.Errorf("%s %s: HTTP %d: %s", method, url, resp.StatusCode, string(body)))
	}
	return body
}

// ---- gc ----
//
// Talks to a running edge:
//   POST /v1/admin/gc/plan?min_age_seconds=N
//   POST /v1/admin/gc/apply?min_age_seconds=N&concurrency=N
//
// Two safety nets (ADR-012):
//   - claimed-set: meta-claimed (chunk_id, addr) pairs are never deleted
//   - min-age:    fresh chunks (mtime within N seconds) are never deleted
func cmdGC(args []string) {
	fs := flag.NewFlagSet("gc", flag.ExitOnError)
	edge := fs.String("edge", "http://localhost:8000", "edge base URL")
	doPlan := fs.Bool("plan", false, "show GC plan only (no changes)")
	doApply := fs.Bool("apply", false, "execute the plan")
	concurrency := fs.Int("concurrency", 4, "parallel deletes during apply")
	minAge := fs.Int("min-age", 60, "min chunk age in seconds (race protection)")
	verbose := fs.Bool("v", false, "print every sweep entry (otherwise just summary)")
	fs.Parse(args)

	if !*doPlan && !*doApply {
		fmt.Fprintln(os.Stderr, "gc: specify --plan or --apply")
		os.Exit(2)
	}
	if *doPlan && *doApply {
		fmt.Fprintln(os.Stderr, "gc: --plan and --apply are mutually exclusive")
		os.Exit(2)
	}

	if *doPlan {
		runGCPlan(*edge, *minAge, *verbose)
	} else {
		runGCApply(*edge, *minAge, *concurrency, *verbose)
	}
}

func runGCPlan(edge string, minAge int, verbose bool) {
	url := fmt.Sprintf("%s/v1/admin/gc/plan?min_age_seconds=%d",
		strings.TrimRight(edge, "/"), minAge)
	body := mustPost(url)
	var plan struct {
		Scanned     int `json:"scanned"`
		ClaimedKeys int `json:"claimed_keys"`
		Sweeps      []struct {
			Addr    string `json:"addr"`
			ChunkID string `json:"chunk_id"`
			Size    int64  `json:"size"`
			AgeSec  int64  `json:"age_seconds"`
		} `json:"sweeps"`
	}
	if err := json.Unmarshal(body, &plan); err != nil {
		fail(fmt.Errorf("decode plan: %w", err))
	}

	fmt.Printf("🧹 GC plan — scanned %d on-disk chunks across all DNs\n", plan.Scanned)
	fmt.Printf("   meta-claimed pairs (protected): %d\n", plan.ClaimedKeys)
	fmt.Printf("   min-age threshold (protected if newer): %ds\n", minAge)
	fmt.Printf("   sweeps proposed: %d\n", len(plan.Sweeps))
	if len(plan.Sweeps) == 0 {
		fmt.Println("   ✅ No surplus to clean. Cluster disk = meta intent.")
		return
	}

	if verbose {
		fmt.Println()
		for _, s := range plan.Sweeps {
			fmt.Printf("  %s  chunk=%s..  size=%d  age=%ds\n",
				s.Addr, idShort(s.ChunkID), s.Size, s.AgeSec)
		}
	} else {
		shown := 5
		if shown > len(plan.Sweeps) {
			shown = len(plan.Sweeps)
		}
		fmt.Println()
		fmt.Printf("First %d sweeps:\n", shown)
		for i := 0; i < shown; i++ {
			s := plan.Sweeps[i]
			fmt.Printf("  %s  chunk=%s..  size=%d  age=%ds\n",
				s.Addr, idShort(s.ChunkID), s.Size, s.AgeSec)
		}
		if len(plan.Sweeps) > shown {
			fmt.Printf("  ... and %d more (use -v to list all)\n", len(plan.Sweeps)-shown)
		}
	}

	fmt.Println()
	fmt.Println("Next:  kvfs-cli gc --apply")
}

func runGCApply(edge string, minAge, concurrency int, verbose bool) {
	url := fmt.Sprintf("%s/v1/admin/gc/apply?min_age_seconds=%d&concurrency=%d",
		strings.TrimRight(edge, "/"), minAge, concurrency)
	body := mustPost(url)
	var stats struct {
		Scanned     int      `json:"scanned"`
		ClaimedKeys int      `json:"claimed_keys"`
		PlanSize    int      `json:"plan_size"`
		Concurrency int      `json:"concurrency"`
		MinAgeSec   int      `json:"min_age_sec"`
		Deleted     int      `json:"deleted"`
		Failed      int      `json:"failed"`
		BytesFreed  int64    `json:"bytes_freed"`
		Errors      []string `json:"errors"`
	}
	if err := json.Unmarshal(body, &stats); err != nil {
		fail(fmt.Errorf("decode apply result: %w", err))
	}

	fmt.Printf("🧹 GC applied (concurrency=%d, min-age=%ds)\n", stats.Concurrency, stats.MinAgeSec)
	fmt.Printf("   scanned:      %d\n", stats.Scanned)
	fmt.Printf("   claimed:      %d\n", stats.ClaimedKeys)
	fmt.Printf("   planned:      %d\n", stats.PlanSize)
	fmt.Printf("   deleted:      %d\n", stats.Deleted)
	fmt.Printf("   failed:       %d\n", stats.Failed)
	fmt.Printf("   bytes_freed:  %d\n", stats.BytesFreed)
	if verbose && len(stats.Errors) > 0 {
		fmt.Println("   errors:")
		for _, e := range stats.Errors {
			fmt.Printf("     - %s\n", e)
		}
	}
	if stats.Failed > 0 {
		os.Exit(1)
	}
}

func idShort(id string) string {
	if len(id) > 16 {
		return id[:16]
	}
	return id
}

func cmdAuto(args []string) {
	fs := flag.NewFlagSet("auto", flag.ExitOnError)
	edge := fs.String("edge", "http://localhost:8000", "edge base URL")
	doStatus := fs.Bool("status", false, "show auto-trigger status")
	verbose := fs.Bool("v", false, "print all history entries (otherwise last 5)")
	fs.Parse(args)

	if !*doStatus {
		fmt.Fprintln(os.Stderr, "auto: specify --status")
		os.Exit(2)
	}

	url := strings.TrimRight(*edge, "/") + "/v1/admin/auto/status"
	body := mustGet(url)

	var status struct {
		Config struct {
			Enabled           bool   `json:"enabled"`
			RebalanceInterval string `json:"rebalance_interval"`
			GCInterval        string `json:"gc_interval"`
			GCMinAge          string `json:"gc_min_age"`
			Concurrency       int    `json:"concurrency"`
		} `json:"config"`
		LastCheckRebalance time.Time `json:"last_check_rebalance"`
		LastCheckGC        time.Time `json:"last_check_gc"`
		NextRebalance      time.Time `json:"next_rebalance"`
		NextGC             time.Time `json:"next_gc"`
		HistorySize        int       `json:"history_size"`
		Runs               []struct {
			Job        string                 `json:"job"`
			StartedAt  time.Time              `json:"started_at"`
			DurationMS int64                  `json:"duration_ms"`
			PlanSize   int                    `json:"plan_size"`
			Stats      map[string]interface{} `json:"stats"`
			Error      string                 `json:"error"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(body, &status); err != nil {
		fail(fmt.Errorf("decode status: %w", err))
	}

	state := "DISABLED (opt-in via EDGE_AUTO=1)"
	if status.Config.Enabled {
		state = "ENABLED"
	}
	fmt.Printf("⏱  Auto-trigger: %s\n", state)
	if status.Config.Enabled {
		fmt.Printf("   rebalance_interval:    %s\n", status.Config.RebalanceInterval)
		fmt.Printf("   gc_interval:           %s\n", status.Config.GCInterval)
		fmt.Printf("   gc_min_age:            %s\n", status.Config.GCMinAge)
		fmt.Printf("   concurrency:           %d\n", status.Config.Concurrency)
		printTime("last_check_rebalance", status.LastCheckRebalance)
		printTime("last_check_gc       ", status.LastCheckGC)
		printTime("next_rebalance      ", status.NextRebalance)
		printTime("next_gc             ", status.NextGC)
	}

	if status.HistorySize == 0 {
		fmt.Println()
		fmt.Println("No work or errors recorded yet (empty cycles are not stored — see last_check_* above).")
		return
	}

	show := len(status.Runs)
	if !*verbose && show > 5 {
		show = 5
	}
	fmt.Println()
	fmt.Printf("Recent %d runs (most recent last):\n", show)
	for _, r := range status.Runs[len(status.Runs)-show:] {
		ts := r.StartedAt.Format("15:04:05")
		if r.Error != "" {
			fmt.Printf("  %s  %-9s  ERROR: %s\n", ts, r.Job, r.Error)
			continue
		}
		fmt.Printf("  %s  %-9s  plan=%d  stats=%v  (%dms)\n",
			ts, r.Job, r.PlanSize, r.Stats, r.DurationMS)
	}
}

func printTime(label string, t time.Time) {
	if t.IsZero() {
		return
	}
	fmt.Printf("   %s:    %s\n", label, t.Format(time.RFC3339))
}

// ---- dns ----
//
// Live DN registry (ADR-027). Operator-friendly wrapper for:
//   GET    /v1/admin/dns
//   POST   /v1/admin/dns      body {"addr":"dn7:8080"}
//   DELETE /v1/admin/dns?addr=dn7:8080
//
// Usage:
//   kvfs-cli dns list
//   kvfs-cli dns add dn7:8080
//   kvfs-cli dns remove dn7:8080
func cmdDNs(args []string) {
	fs := flag.NewFlagSet("dns", flag.ExitOnError)
	edge := fs.String("edge", "http://localhost:8000", "edge base URL")
	fs.Parse(args)

	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "dns: missing subcommand (list | add <addr> | remove <addr>)")
		os.Exit(2)
	}
	base := strings.TrimRight(*edge, "/")
	switch rest[0] {
	case "list":
		body := mustGet(base + "/v1/admin/dns")
		var resp struct {
			DNs         []string `json:"dns"`
			QuorumWrite int      `json:"quorum_write"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			fail(err)
		}
		fmt.Printf("%d DN(s)  quorum_write=%d\n", len(resp.DNs), resp.QuorumWrite)
		for _, a := range resp.DNs {
			fmt.Printf("  %s\n", a)
		}
	case "add":
		if len(rest) != 2 {
			fmt.Fprintln(os.Stderr, "dns add: usage: kvfs-cli dns add <addr>")
			os.Exit(2)
		}
		body := mustHTTPJSON(http.MethodPost, base+"/v1/admin/dns",
			fmt.Sprintf(`{"addr":%q}`, rest[1]))
		fmt.Printf("✅ added %s\n", rest[1])
		fmt.Println(string(body))
	case "remove":
		if len(rest) != 2 {
			fmt.Fprintln(os.Stderr, "dns remove: usage: kvfs-cli dns remove <addr>")
			os.Exit(2)
		}
		fmt.Println("⚠️  remove does NOT migrate data off the DN. Run rebalance --apply first if it holds chunks.")
		body := mustHTTPJSON(http.MethodDelete, base+"/v1/admin/dns?addr="+rest[1], "")
		fmt.Printf("✅ removed %s\n", rest[1])
		fmt.Println(string(body))
	default:
		fmt.Fprintf(os.Stderr, "dns: unknown subcommand %q (want list | add | remove)\n", rest[0])
		os.Exit(2)
	}
}

// ---- urlkey ----
//
// Live UrlKey kid registry (ADR-028). Wraps:
//   GET    /v1/admin/urlkey
//   POST   /v1/admin/urlkey/rotate body {"kid":..,"secret_hex":..}
//   DELETE /v1/admin/urlkey?kid=v1
//
// Usage:
//   kvfs-cli urlkey list
//   kvfs-cli urlkey rotate                    # auto-generate kid + secret
//   kvfs-cli urlkey rotate --kid v3 --secret-hex 0123abcd...
//   kvfs-cli urlkey remove --kid v1           # 옛 kid (verify 무효화)
func cmdURLKey(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "urlkey: missing subcommand (list | rotate | remove)")
		os.Exit(2)
	}
	sub := args[0]
	fs := flag.NewFlagSet("urlkey "+sub, flag.ExitOnError)
	edge := fs.String("edge", "http://localhost:8000", "edge base URL")
	flagKid := fs.String("kid", "", "kid for rotate/remove")
	flagSecret := fs.String("secret-hex", "", "hex-encoded secret for rotate (auto-gen if empty)")
	fs.Parse(args[1:])

	base := strings.TrimRight(*edge, "/")
	switch sub {
	case "list":
		body := mustGet(base + "/v1/admin/urlkey")
		var resp struct {
			Kids []struct {
				Kid       string    `json:"kid"`
				IsPrimary bool      `json:"is_primary"`
				CreatedAt time.Time `json:"created_at"`
			} `json:"kids"`
			Primary string `json:"primary"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			fail(err)
		}
		fmt.Printf("%d kid(s)  primary=%s\n", len(resp.Kids), resp.Primary)
		for _, k := range resp.Kids {
			marker := " "
			if k.IsPrimary {
				marker = "*"
			}
			fmt.Printf("  %s %-12s  created=%s\n", marker, k.Kid, k.CreatedAt.Format(time.RFC3339))
		}
	case "rotate":
		kid := *flagKid
		if kid == "" {
			kid = fmt.Sprintf("v%d", time.Now().Unix())
		}
		secret := *flagSecret
		if secret == "" {
			buf := make([]byte, 32)
			if _, err := rand.Read(buf); err != nil {
				fail(err)
			}
			secret = hex.EncodeToString(buf)
			fmt.Printf("ℹ️  generated random 32-byte secret\n")
		}
		payload := fmt.Sprintf(`{"kid":%q,"secret_hex":%q}`, kid, secret)
		body := mustHTTPJSON(http.MethodPost, base+"/v1/admin/urlkey/rotate", payload)
		fmt.Printf("✅ rotated; primary=%s\n", kid)
		fmt.Println(string(body))
	case "remove":
		if *flagKid == "" {
			fmt.Fprintln(os.Stderr, "urlkey remove: --kid required")
			os.Exit(2)
		}
		body := mustHTTPJSON(http.MethodDelete, base+"/v1/admin/urlkey?kid="+*flagKid, "")
		fmt.Printf("✅ removed kid=%s\n", *flagKid)
		fmt.Println(string(body))
	default:
		fmt.Fprintf(os.Stderr, "urlkey: unknown subcommand %q\n", sub)
		os.Exit(2)
	}
}

// mustHTTPJSON variant that POSTs/DELETEs with optional JSON body.
func mustHTTPJSON(method, url, body string) []byte {
	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		fail(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		fail(fmt.Errorf("%s %s: %w", method, url, err))
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fail(fmt.Errorf("%s %s: HTTP %d: %s", method, url, resp.StatusCode, string(out)))
	}
	return out
}
