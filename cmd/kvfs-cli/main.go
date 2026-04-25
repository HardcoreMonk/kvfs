// Command kvfs-cli is the developer CLI:
//
//	kvfs-cli sign --method PUT --path /v1/o/bucket/key --ttl 1h
//	kvfs-cli inspect [--db ./edge-data/edge.db] [--object bucket/key]
//
// The secret for sign/verify is read from EDGE_URLKEY_SECRET (shared with kvfs-edge).
package main

import (
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

func barChart(pct float64, width int) string {
	// pct는 0~100 범위. width 칸에 비례해 채운다.
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

func runRebalancePlan(edge string, verbose bool) {
	url := strings.TrimRight(edge, "/") + "/v1/admin/rebalance/plan"
	body := mustPost(url)
	var plan struct {
		Scanned    int `json:"scanned"`
		Migrations []struct {
			Bucket  string   `json:"bucket"`
			Key     string   `json:"key"`
			ChunkID string   `json:"chunk_id"`
			Size    int64    `json:"size"`
			Actual  []string `json:"actual"`
			Desired []string `json:"desired"`
			Missing []string `json:"missing"`
			Surplus []string `json:"surplus"`
		} `json:"migrations"`
	}
	if err := json.Unmarshal(body, &plan); err != nil {
		fail(fmt.Errorf("decode plan: %w", err))
	}

	fmt.Printf("📋 Rebalance plan — scanned %d objects, %d need migration\n",
		plan.Scanned, len(plan.Migrations))
	if len(plan.Migrations) == 0 {
		fmt.Println("   ✅ Cluster is in HRW-desired state. No work to do.")
		return
	}

	if verbose {
		fmt.Println()
		for _, m := range plan.Migrations {
			fmt.Printf("  %s/%s  size=%d  chunk=%s..\n", m.Bucket, m.Key, m.Size, m.ChunkID[:16])
			fmt.Printf("    actual:  %s\n", strings.Join(m.Actual, ", "))
			fmt.Printf("    desired: %s\n", strings.Join(m.Desired, ", "))
			fmt.Printf("    missing: %s\n", strings.Join(m.Missing, ", "))
			if len(m.Surplus) > 0 {
				fmt.Printf("    surplus: %s  (kept — never delete in MVP)\n", strings.Join(m.Surplus, ", "))
			}
		}
	} else {
		// brief: show first 5
		shown := 5
		if shown > len(plan.Migrations) {
			shown = len(plan.Migrations)
		}
		fmt.Println()
		fmt.Printf("First %d migrations:\n", shown)
		for i := 0; i < shown; i++ {
			m := plan.Migrations[i]
			fmt.Printf("  %s/%s  missing=%v  (currently on %v)\n",
				m.Bucket, m.Key, m.Missing, m.Actual)
		}
		if len(plan.Migrations) > shown {
			fmt.Printf("  ... and %d more (use -v to list all)\n", len(plan.Migrations)-shown)
		}
	}

	fmt.Println()
	fmt.Println("Next:  kvfs-cli rebalance --apply")
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

func mustPost(url string) []byte {
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		fail(err)
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		fail(fmt.Errorf("POST %s: %w", url, err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fail(err)
	}
	if resp.StatusCode != http.StatusOK {
		fail(fmt.Errorf("POST %s: HTTP %d: %s", url, resp.StatusCode, string(body)))
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

// ---- auto ----
//
// Queries GET /v1/admin/auto/status and pretty-prints config + history.
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
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		fail(err)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fail(fmt.Errorf("GET %s: %w", url, err))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fail(fmt.Errorf("GET %s: HTTP %d: %s", url, resp.StatusCode, string(body)))
	}

	var status struct {
		Config struct {
			Enabled           bool   `json:"enabled"`
			RebalanceInterval string `json:"rebalance_interval"`
			GCInterval        string `json:"gc_interval"`
			GCMinAge          string `json:"gc_min_age"`
			Concurrency       int    `json:"concurrency"`
		} `json:"config"`
		LastRebalance time.Time `json:"last_rebalance"`
		LastGC        time.Time `json:"last_gc"`
		NextRebalance time.Time `json:"next_rebalance"`
		NextGC        time.Time `json:"next_gc"`
		HistorySize   int       `json:"history_size"`
		Runs          []struct {
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
		fmt.Printf("   rebalance_interval: %s\n", status.Config.RebalanceInterval)
		fmt.Printf("   gc_interval:        %s\n", status.Config.GCInterval)
		fmt.Printf("   gc_min_age:         %s\n", status.Config.GCMinAge)
		fmt.Printf("   concurrency:        %d\n", status.Config.Concurrency)
		if !status.LastRebalance.IsZero() {
			fmt.Printf("   last_rebalance:     %s\n", status.LastRebalance.Format(time.RFC3339))
		}
		if !status.LastGC.IsZero() {
			fmt.Printf("   last_gc:            %s\n", status.LastGC.Format(time.RFC3339))
		}
		if !status.NextRebalance.IsZero() {
			fmt.Printf("   next_rebalance:     %s\n", status.NextRebalance.Format(time.RFC3339))
		}
		if !status.NextGC.IsZero() {
			fmt.Printf("   next_gc:            %s\n", status.NextGC.Format(time.RFC3339))
		}
	}

	if status.HistorySize == 0 {
		fmt.Println()
		fmt.Println("No runs yet.")
		return
	}

	show := len(status.Runs)
	if !*verbose && show > 5 {
		show = 5
	}
	fmt.Println()
	fmt.Printf("Recent %d runs (most recent last):\n", show)
	startIdx := len(status.Runs) - show
	for i := startIdx; i < len(status.Runs); i++ {
		r := status.Runs[i]
		ts := r.StartedAt.Format("15:04:05")
		if r.Error != "" {
			fmt.Printf("  %s  %-9s  ERROR: %s\n", ts, r.Job, r.Error)
			continue
		}
		if r.PlanSize == 0 {
			fmt.Printf("  %s  %-9s  no work  (%dms)\n", ts, r.Job, r.DurationMS)
			continue
		}
		fmt.Printf("  %s  %-9s  plan=%d  stats=%v  (%dms)\n",
			ts, r.Job, r.PlanSize, r.Stats, r.DurationMS)
	}
}
