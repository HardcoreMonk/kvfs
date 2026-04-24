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
