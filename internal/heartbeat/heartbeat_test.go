// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package heartbeat

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeProbe struct {
	mu      sync.Mutex
	results map[string]error // addr → err per probe (consumed once each)
	queues  map[string][]error
	delays  map[string]time.Duration
}

func newFakeProbe() *fakeProbe {
	return &fakeProbe{
		results: map[string]error{},
		queues:  map[string][]error{},
		delays:  map[string]time.Duration{},
	}
}

func (p *fakeProbe) setStatic(addr string, err error)         { p.results[addr] = err }
func (p *fakeProbe) enqueue(addr string, errs ...error)       { p.queues[addr] = append(p.queues[addr], errs...) }
func (p *fakeProbe) setDelay(addr string, d time.Duration)    { p.delays[addr] = d }

func (p *fakeProbe) Probe(ctx context.Context, addr string) (time.Duration, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d := p.delays[addr]; d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return d, ctx.Err()
		}
	}
	if q, ok := p.queues[addr]; ok && len(q) > 0 {
		err := q[0]
		p.queues[addr] = q[1:]
		return time.Millisecond, err
	}
	return time.Millisecond, p.results[addr]
}

func TestTickAllHealthy(t *testing.T) {
	probe := newFakeProbe()
	probe.setStatic("dn1:8080", nil)
	probe.setStatic("dn2:8080", nil)
	probe.setStatic("dn3:8080", nil)
	m := New(probe, 3, time.Second)

	m.Tick(context.Background(), []string{"dn1:8080", "dn2:8080", "dn3:8080"})

	snap := m.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("want 3 statuses, got %d", len(snap))
	}
	for _, s := range snap {
		if !s.Healthy {
			t.Errorf("%s: want healthy", s.Addr)
		}
		if s.LastSeen.IsZero() {
			t.Errorf("%s: LastSeen empty", s.Addr)
		}
		if s.TotalProbes != 1 {
			t.Errorf("%s: TotalProbes=%d want 1", s.Addr, s.TotalProbes)
		}
	}
}

func TestUnhealthyAfterThreshold(t *testing.T) {
	probe := newFakeProbe()
	probe.setStatic("dn1:8080", errors.New("connection refused"))
	m := New(probe, 3, time.Second)

	for i := 1; i <= 4; i++ {
		m.Tick(context.Background(), []string{"dn1:8080"})
		snap := m.Snapshot()
		if len(snap) != 1 {
			t.Fatalf("iter %d: want 1 status, got %d", i, len(snap))
		}
		s := snap[0]
		if s.ConsecFails != i {
			t.Errorf("iter %d: ConsecFails=%d want %d", i, s.ConsecFails, i)
		}
		wantHealthy := i < 3
		if s.Healthy != wantHealthy {
			t.Errorf("iter %d: Healthy=%v want %v (consec=%d, threshold=3)", i, s.Healthy, wantHealthy, i)
		}
	}
}

func TestRecovery(t *testing.T) {
	probe := newFakeProbe()
	probe.enqueue("dn1:8080", errors.New("e1"), errors.New("e2"), errors.New("e3"), nil, nil)
	m := New(probe, 3, time.Second)

	for i := 1; i <= 5; i++ {
		m.Tick(context.Background(), []string{"dn1:8080"})
	}
	snap := m.Snapshot()[0]
	if !snap.Healthy {
		t.Errorf("after recovery: Healthy=false, want true")
	}
	if snap.ConsecFails != 0 {
		t.Errorf("after recovery: ConsecFails=%d want 0", snap.ConsecFails)
	}
	if snap.LastError != "" {
		t.Errorf("after recovery: LastError=%q want empty", snap.LastError)
	}
}

func TestHealthyAddrs(t *testing.T) {
	probe := newFakeProbe()
	probe.setStatic("good", nil)
	probe.setStatic("bad", errors.New("dead"))
	m := New(probe, 1, time.Second) // threshold=1 so 1 fail = unhealthy

	m.Tick(context.Background(), []string{"good", "bad"})

	healthy := m.HealthyAddrs()
	if len(healthy) != 1 || healthy[0] != "good" {
		t.Errorf("HealthyAddrs=%v want [good]", healthy)
	}
}

func TestRegistryShrink(t *testing.T) {
	probe := newFakeProbe()
	probe.setStatic("a", nil)
	probe.setStatic("b", nil)
	m := New(probe, 3, time.Second)

	m.Tick(context.Background(), []string{"a", "b"})
	if len(m.Snapshot()) != 2 {
		t.Fatalf("want 2 statuses after first tick")
	}
	// Drop 'b' from registry; next tick should remove it.
	m.Tick(context.Background(), []string{"a"})
	snap := m.Snapshot()
	if len(snap) != 1 || snap[0].Addr != "a" {
		t.Errorf("after shrink: want [a], got %+v", snap)
	}
}

func TestTickEmptyAddrs(t *testing.T) {
	m := New(newFakeProbe(), 3, time.Second)
	m.Tick(context.Background(), nil)
	if len(m.Snapshot()) != 0 {
		t.Errorf("nil addrs: want 0 statuses")
	}
}
