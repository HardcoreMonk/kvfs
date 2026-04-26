// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Package metrics is a tiny Prometheus-compatible metrics registry. No
// external dependency — emits the text-exposition format directly so the
// project rule "stdlib + bbolt only" is preserved.
//
// 비전공자용 해설
// ──────────────
// Prometheus client_golang 라이브러리는 강력하지만 무거움 (수십 의존성).
// kvfs 는 외부 의존 최소 원칙이라 stdlib 만으로 동등한 surface 를 작성:
//
//   - Counter — 단조 증가하는 수 (요청 수, 에러 수)
//   - Gauge   — 임의 값 (현재 객체 수, 마지막 WAL seq)
//   - Histogram (단순화) — 버킷에 카운트 + sum/count
//
// 모두 atomic.Int64 / atomic.Uint64 기반 lock-free. 모든 metric 은 fixed
// label set (kvfs 내부 사용은 label cardinality 가 작다 — DN addr, EC k+m
// 정도). high-cardinality label 은 의도적으로 미지원.
//
// 출력은 Prometheus text-exposition 0.0.4 포맷:
//
//   # HELP kvfs_put_total ...
//   # TYPE kvfs_put_total counter
//   kvfs_put_total{mode="replication"} 42
//   kvfs_put_total{mode="ec"} 7
//
// 사용:
//
//   reg := metrics.NewRegistry()
//   putCounter := reg.Counter("kvfs_put_total", "Total PUT requests", "mode")
//   putCounter.WithLabels("replication").Inc()
//   ...
//   reg.WriteTo(w)  // GET /metrics handler
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Registry holds all named metrics. Goroutine-safe.
type Registry struct {
	mu      sync.RWMutex
	metrics []metric
}

func NewRegistry() *Registry { return &Registry{} }

// metric is the common interface every metric type implements for export.
type metric interface {
	Name() string
	Help() string
	TypeName() string
	Render(w io.Writer) error
}

// register adds m to the registry. Duplicate names are silently overwritten
// (last-writer-wins) to avoid registration ordering pain.
func (r *Registry) register(m metric) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, existing := range r.metrics {
		if existing.Name() == m.Name() {
			r.metrics[i] = m
			return
		}
	}
	r.metrics = append(r.metrics, m)
}

// WriteTo emits all registered metrics in text-exposition format.
func (r *Registry) Render(w io.Writer) error {
	r.mu.RLock()
	snap := make([]metric, len(r.metrics))
	copy(snap, r.metrics)
	r.mu.RUnlock()
	sort.Slice(snap, func(i, j int) bool { return snap[i].Name() < snap[j].Name() })
	for _, m := range snap {
		if _, err := fmt.Fprintf(w, "# HELP %s %s\n", m.Name(), m.Help()); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "# TYPE %s %s\n", m.Name(), m.TypeName()); err != nil {
			return err
		}
		if err := m.Render(w); err != nil {
			return err
		}
	}
	return nil
}

// ─── Counter ────────────────────────────────────────────────────────────

// Counter is a monotonically-increasing 64-bit value with optional labels.
// Labels are fixed at registration; per-label-combination counters live in
// an internal map keyed by the label tuple.
type Counter struct {
	name, help string
	labelKeys  []string
	mu         sync.RWMutex
	values     map[string]*atomic.Uint64
}

// Counter registers a new counter. labelKeys may be empty for a no-label counter.
func (r *Registry) Counter(name, help string, labelKeys ...string) *Counter {
	c := &Counter{
		name:      name,
		help:      help,
		labelKeys: labelKeys,
		values:    map[string]*atomic.Uint64{},
	}
	r.register(c)
	return c
}

func (c *Counter) Name() string     { return c.name }
func (c *Counter) Help() string     { return c.help }
func (c *Counter) TypeName() string { return "counter" }

// WithLabels returns the per-label-tuple slot. Number of values must match
// labelKeys len at registration; mismatch panics (programmer error).
func (c *Counter) WithLabels(labelValues ...string) *atomic.Uint64 {
	if len(labelValues) != len(c.labelKeys) {
		panic(fmt.Sprintf("metrics: counter %s expects %d labels, got %d",
			c.name, len(c.labelKeys), len(labelValues)))
	}
	key := strings.Join(labelValues, "\x00")
	c.mu.RLock()
	if v, ok := c.values[key]; ok {
		c.mu.RUnlock()
		return v
	}
	c.mu.RUnlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.values[key]; ok {
		return v
	}
	v := &atomic.Uint64{}
	c.values[key] = v
	return v
}

// Inc is a convenience for the no-label case (or first label slot).
func (c *Counter) Inc() { c.WithLabels(c.labelKeys...).Add(1) }

// Add increments the no-label counter by n.
func (c *Counter) Add(n uint64) { c.WithLabels(c.labelKeys...).Add(n) }

func (c *Counter) Render(w io.Writer) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.labelKeys) == 0 {
		v := c.values[""]
		val := uint64(0)
		if v != nil {
			val = v.Load()
		}
		_, err := fmt.Fprintf(w, "%s %d\n", c.name, val)
		return err
	}
	keys := make([]string, 0, len(c.values))
	for k := range c.values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		labelVals := strings.Split(k, "\x00")
		var pairs []string
		for i, lk := range c.labelKeys {
			pairs = append(pairs, fmt.Sprintf(`%s=%q`, lk, labelVals[i]))
		}
		if _, err := fmt.Fprintf(w, "%s{%s} %d\n", c.name, strings.Join(pairs, ","), c.values[k].Load()); err != nil {
			return err
		}
	}
	return nil
}

// ─── Gauge ──────────────────────────────────────────────────────────────

// Gauge is a settable int64 value (can go up or down) backed by a callback.
// We use a callback model so the gauge always reflects fresh state without
// requiring instrumented code to call Set on every change.
type Gauge struct {
	name, help string
	fn         func() int64
}

// Gauge registers a new callback-driven gauge. The callback is invoked on
// every /metrics scrape — keep it cheap.
func (r *Registry) Gauge(name, help string, fn func() int64) *Gauge {
	g := &Gauge{name: name, help: help, fn: fn}
	r.register(g)
	return g
}

func (g *Gauge) Name() string     { return g.name }
func (g *Gauge) Help() string     { return g.help }
func (g *Gauge) TypeName() string { return "gauge" }

func (g *Gauge) Render(w io.Writer) error {
	_, err := fmt.Fprintf(w, "%s %d\n", g.name, g.fn())
	return err
}
