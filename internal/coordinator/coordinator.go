// Package coordinator implements chunk placement, fanout writes, and quorum reads.
//
// This is the "inline library" in kvfs-edge. Carved as a dedicated package so
// it can be split into a separate daemon in Season 2 without touching handler code.
//
// Core contract:
//   - WriteChunk picks ReplicationFactor nodes via placement.Placer (Rendezvous
//     Hashing) and fans out PUT /chunk/<id> in parallel. Returns success when
//     at least QuorumWrite acks arrive.
//   - ReadChunk tries the stored replica list (from object metadata) in order,
//     returning the first success.
//
// Season 2 ADR-009 변경점:
//   - 이전: 모든 DN에 쓰기 (len(dns) replicas)
//   - 지금: chunkID → placement.Placer.Pick(ReplicationFactor) 로 N개 선택
//   - 효과: DN 추가·제거 시 전체 흔들림 없이 약 R/N 만 이동
package coordinator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/HardcoreMonk/kvfs/internal/placement"
)

// Coordinator routes chunks to DNs and drives fanout/quorum.
type Coordinator struct {
	placer            *placement.Placer
	replicationFactor int // number of replicas per chunk (R)
	quorumWrite       int // min acks for success (e.g. 2 of 3)
	client            *http.Client
}

// Config bundles Coordinator construction parameters.
type Config struct {
	// Nodes is the set of DataNode targets. Each Node has ID (stable) and Addr (network).
	// 최소 1개. 과거 버전의 DNs([]string) 과 하위호환성을 위해 Addr만 있는 경우
	// NewWithAddrs() 도우미 참고.
	Nodes []placement.Node

	// ReplicationFactor: 청크당 복제 수. 기본은 min(3, len(Nodes)).
	// 3-way replication MVP 유지. Season 2+ 에서 EC(8+3) 등으로 대체 가능.
	ReplicationFactor int

	// QuorumWrite: 몇 개 ack 시 성공 처리할지. 기본 = ReplicationFactor/2 + 1.
	// 예: ReplicationFactor=3 → QuorumWrite=2. 1개 DN 죽어도 쓰기 성공.
	QuorumWrite int

	// Timeout: per-request HTTP deadline.
	Timeout time.Duration
}

// New builds a Coordinator. Defaults:
//   - ReplicationFactor = min(3, len(Nodes))
//   - QuorumWrite = ReplicationFactor/2 + 1
//   - Timeout = 10s
func New(cfg Config) (*Coordinator, error) {
	if len(cfg.Nodes) == 0 {
		return nil, errors.New("coordinator: at least one node required")
	}
	r := cfg.ReplicationFactor
	if r <= 0 {
		r = min(3, len(cfg.Nodes))
	}
	if r > len(cfg.Nodes) {
		return nil, fmt.Errorf("coordinator: ReplicationFactor(%d) > node count(%d)", r, len(cfg.Nodes))
	}
	q := cfg.QuorumWrite
	if q <= 0 {
		q = r/2 + 1
	}
	if q > r {
		return nil, fmt.Errorf("coordinator: QuorumWrite(%d) > ReplicationFactor(%d)", q, r)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Coordinator{
		placer:            placement.New(cfg.Nodes),
		replicationFactor: r,
		quorumWrite:       q,
		client:            &http.Client{Timeout: timeout},
	}, nil
}

// NewWithAddrs is a convenience for legacy callers who only have addresses.
// Each address becomes both ID and Addr of a Node.
//
// 기존 main.go 에서 `EDGE_DNS=dn1:8080,dn2:8080,...` 환경변수로 들어오는
// 주소 문자열 목록을 그대로 받기 위한 도우미. 운영상 ID는 주소와 동일해도
// placement의 결정적 특성에는 영향 없음 (같은 ID는 같은 score → 같은 선택).
func NewWithAddrs(addrs []string, replicationFactor, quorumWrite int, timeout time.Duration) (*Coordinator, error) {
	nodes := make([]placement.Node, len(addrs))
	for i, a := range addrs {
		nodes[i] = placement.Node{ID: a, Addr: a}
	}
	return New(Config{
		Nodes:             nodes,
		ReplicationFactor: replicationFactor,
		QuorumWrite:       quorumWrite,
		Timeout:           timeout,
	})
}

// Nodes returns the configured DN set (read-only copy).
func (c *Coordinator) Nodes() []placement.Node { return c.placer.Nodes() }

// DNs returns the configured DN addresses (read-only view).
// 하위호환: 기존 코드에서 addr만 필요하면 이 함수 사용.
func (c *Coordinator) DNs() []string {
	ns := c.placer.Nodes()
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = n.Addr
	}
	return out
}

// ReplicationFactor returns R (replicas per chunk).
func (c *Coordinator) ReplicationFactor() int { return c.replicationFactor }

// QuorumWrite returns the write quorum (min acks for success).
func (c *Coordinator) QuorumWrite() int { return c.quorumWrite }

// PlaceChunk returns the addresses this Coordinator would pick for chunkID.
// 외부에서 "이 청크는 어디 갈까?" 를 미리 계산하고 싶을 때 사용.
// (예: admin 도구, 디버깅, placement-sim)
func (c *Coordinator) PlaceChunk(chunkID string) []string {
	picked := c.placer.Pick(chunkID, c.replicationFactor)
	out := make([]string, len(picked))
	for i, n := range picked {
		out[i] = n.Addr
	}
	return out
}

// WriteChunk picks ReplicationFactor nodes via placement, fans out PUT in parallel,
// and returns the addresses that acked successfully. Error if fewer than
// QuorumWrite acks arrive.
//
// 순서:
//   1. placer.Pick(chunkID, R) → 대상 노드 R개
//   2. goroutine R개로 PUT 병렬 실행
//   3. 성공 ack 개수 집계
//   4. QuorumWrite 이상이면 성공, 아니면 에러
func (c *Coordinator) WriteChunk(ctx context.Context, chunkID string, data []byte) ([]string, error) {
	targets := c.placer.Pick(chunkID, c.replicationFactor)
	if len(targets) == 0 {
		return nil, errors.New("coordinator: no target nodes available")
	}

	type result struct {
		addr string
		err  error
	}
	results := make(chan result, len(targets))
	var wg sync.WaitGroup
	for _, node := range targets {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			err := c.putChunk(ctx, addr, chunkID, data)
			results <- result{addr: addr, err: err}
		}(node.Addr)
	}
	go func() { wg.Wait(); close(results) }()

	var ok []string
	var errs []error
	for r := range results {
		if r.err == nil {
			ok = append(ok, r.addr)
		} else {
			errs = append(errs, fmt.Errorf("%s: %w", r.addr, r.err))
		}
	}
	if len(ok) < c.quorumWrite {
		return ok, fmt.Errorf("quorum not reached: %d/%d success; errors: %v",
			len(ok), c.quorumWrite, errs)
	}
	return ok, nil
}

// ReadChunk tries candidates in order, returning the first successful body.
//
// 주의: candidates 는 object metadata의 Replicas 배열 — 쓰기 당시 실제 ack 한
// DN 주소. 현재 placement 결과와 다를 수 있음 (DN 추가·제거 후). 그래도
// 여전히 유효한 복제본이므로 그대로 쓴다. 재배치는 별도 rebalance 작업의
// 책임 (Season 2+ ADR-010 예상).
func (c *Coordinator) ReadChunk(ctx context.Context, chunkID string, candidates []string) ([]byte, string, error) {
	var lastErr error
	for _, addr := range candidates {
		body, err := c.getChunk(ctx, addr, chunkID)
		if err == nil {
			return body, addr, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no candidates")
	}
	return nil, "", fmt.Errorf("all replicas failed: %w", lastErr)
}

// DeleteChunk fires a DELETE to each replica (best-effort).
func (c *Coordinator) DeleteChunk(ctx context.Context, chunkID string, replicas []string) error {
	var errs []error
	for _, addr := range replicas {
		if err := c.deleteChunk(ctx, addr, chunkID); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", addr, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("partial delete failure: %v", errs)
	}
	return nil
}

// ---- low-level HTTP helpers ----

func (c *Coordinator) putChunk(ctx context.Context, addr, chunkID string, data []byte) error {
	url := fmt.Sprintf("http://%s/chunk/%s", addr, chunkID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("PUT returned %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

func (c *Coordinator) getChunk(ctx context.Context, addr, chunkID string) ([]byte, error) {
	url := fmt.Sprintf("http://%s/chunk/%s", addr, chunkID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET returned %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func (c *Coordinator) deleteChunk(ctx context.Context, addr, chunkID string) error {
	url := fmt.Sprintf("http://%s/chunk/%s", addr, chunkID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("DELETE returned %d", resp.StatusCode)
	}
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
