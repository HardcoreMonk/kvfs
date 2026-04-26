// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Package heartbeat tracks per-DN liveness via periodic in-edge probes
// (ADR-030). Pull-based: edge fans out GET /healthz to all runtime DNs each
// tick, updates last_seen + consecutive_failures.
//
// 비전공자용 해설
// ──────────────
// ADR-027 이 동적 DN registry 를 줬지만 운영자 admin endpoint 를 통해서만
// add/remove 가능하다. dn3 가 kernel panic 으로 그냥 죽으면 ListRuntimeDNs() 는
// 여전히 dn3 를 반환 — coordinator 가 dn3 에 PUT 시도 → 실패 → quorum 깨짐.
//
// 해결: edge 가 주기적으로 모든 runtime DN 에 /healthz 를 친다.
//   - 성공 → last_seen 갱신, consecutive_failures=0
//   - 실패 → consecutive_failures++. 임계 도달 시 healthy=false
//
// 본 패키지는 monitoring 만 — auto-eviction 은 별도 (ADR-030 §결과 참조).
// Snapshot() 으로 운영자가 GET /v1/admin/heartbeat 로 본다.
package heartbeat

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Probe abstracts the per-DN health check so the monitor is testable without
// real HTTP. Implementations return latency + error (nil = healthy).
type Probe interface {
	Probe(ctx context.Context, addr string) (latency time.Duration, err error)
}

// Status is the per-DN liveness snapshot.
type Status struct {
	Addr        string    `json:"addr"`
	Healthy     bool      `json:"healthy"`
	LastSeen    time.Time `json:"last_seen,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	LatencyMS   int64     `json:"latency_ms"`
	ConsecFails int       `json:"consec_fails"`
	TotalProbes uint64    `json:"total_probes"`
}

// Monitor tracks per-DN heartbeat status. Goroutine-safe.
type Monitor struct {
	mu            sync.RWMutex
	statuses      map[string]*Status
	probe         Probe
	failThreshold int // consec_fails ≥ threshold ⇒ Healthy=false
	probeTimeout  time.Duration
}

// New returns a Monitor. failThreshold ≤ 0 falls back to 3 (3 consecutive
// failures before marking unhealthy).
func New(probe Probe, failThreshold int, probeTimeout time.Duration) *Monitor {
	if failThreshold <= 0 {
		failThreshold = 3
	}
	if probeTimeout <= 0 {
		probeTimeout = 2 * time.Second
	}
	return &Monitor{
		statuses:      make(map[string]*Status),
		probe:         probe,
		failThreshold: failThreshold,
		probeTimeout:  probeTimeout,
	}
}

// Tick fans out probes to all addrs in parallel and updates statuses.
// Synchronous — caller (ticker goroutine) drives cadence.
//
// addrs MUST be the complete current registry (e.g. coord.DNs()). Statuses
// for addrs not in the slice are evicted, so passing a filtered subset will
// silently lose ConsecFails / TotalProbes history for the omitted DNs.
func (m *Monitor) Tick(ctx context.Context, addrs []string) {
	if len(addrs) == 0 {
		return
	}
	live := make(map[string]struct{}, len(addrs))
	for _, a := range addrs {
		live[a] = struct{}{}
	}
	// Drop statuses for addrs no longer in registry (shrinks map cleanly).
	m.mu.Lock()
	for addr := range m.statuses {
		if _, ok := live[addr]; !ok {
			delete(m.statuses, addr)
		}
	}
	m.mu.Unlock()

	var wg sync.WaitGroup
	for _, addr := range addrs {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, m.probeTimeout)
			defer cancel()
			lat, err := m.probe.Probe(pctx, addr)
			m.update(addr, lat, err)
		}(addr)
	}
	wg.Wait()
}

func (m *Monitor) update(addr string, lat time.Duration, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.statuses[addr]
	if !ok {
		st = &Status{Addr: addr, Healthy: true}
		m.statuses[addr] = st
	}
	st.TotalProbes++
	st.LatencyMS = lat.Milliseconds()
	if err == nil {
		st.LastSeen = time.Now().UTC()
		st.ConsecFails = 0
		st.LastError = ""
		st.Healthy = true
		return
	}
	st.ConsecFails++
	st.LastError = err.Error()
	if st.ConsecFails >= m.failThreshold {
		st.Healthy = false
	}
}

// Snapshot returns a sorted-by-addr copy of all current statuses.
func (m *Monitor) Snapshot() []Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Status, 0, len(m.statuses))
	for _, st := range m.statuses {
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr < out[j].Addr })
	return out
}

// HealthyAddrs returns the subset of probed DNs currently healthy. Useful for
// downstream filtering (e.g., placement could prefer only healthy nodes).
func (m *Monitor) HealthyAddrs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.statuses))
	for addr, st := range m.statuses {
		if st.Healthy {
			out = append(out, addr)
		}
	}
	sort.Strings(out)
	return out
}
