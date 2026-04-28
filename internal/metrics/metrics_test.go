// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package metrics

import (
	"bytes"
	"strings"
	"testing"
)

func TestCounterNoLabels(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("kvfs_test_total", "Test counter")
	c.Inc()
	c.Inc()
	c.Add(3)

	var buf bytes.Buffer
	_ = r.Render(&buf)
	out := buf.String()
	if !strings.Contains(out, "# TYPE kvfs_test_total counter") {
		t.Errorf("missing TYPE line: %q", out)
	}
	if !strings.Contains(out, "kvfs_test_total 5\n") {
		t.Errorf("expected count 5: %q", out)
	}
}

func TestCounterWithLabels(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("kvfs_put_total", "PUT requests", "mode")
	c.WithLabels("replication").Add(10)
	c.WithLabels("ec").Add(3)
	c.WithLabels("replication").Add(2)

	var buf bytes.Buffer
	_ = r.Render(&buf)
	out := buf.String()
	if !strings.Contains(out, `kvfs_put_total{mode="replication"} 12`) {
		t.Errorf("replication count wrong: %q", out)
	}
	if !strings.Contains(out, `kvfs_put_total{mode="ec"} 3`) {
		t.Errorf("ec count wrong: %q", out)
	}
}

func TestGauge(t *testing.T) {
	r := NewRegistry()
	val := int64(42)
	r.Gauge("kvfs_objects", "Object count", func() int64 { return val })

	var buf bytes.Buffer
	_ = r.Render(&buf)
	out := buf.String()
	if !strings.Contains(out, "# TYPE kvfs_objects gauge") {
		t.Errorf("missing TYPE: %q", out)
	}
	if !strings.Contains(out, "kvfs_objects 42\n") {
		t.Errorf("expected 42: %q", out)
	}

	val = 99
	buf.Reset()
	_ = r.Render(&buf)
	if !strings.Contains(buf.String(), "kvfs_objects 99\n") {
		t.Errorf("gauge didn't reflect updated value: %q", buf.String())
	}
}

func TestRegistryDuplicateOverwrites(t *testing.T) {
	r := NewRegistry()
	c1 := r.Counter("kvfs_x", "first")
	c1.Inc()
	c2 := r.Counter("kvfs_x", "second") // same name; should replace c1
	c2.Inc()

	var buf bytes.Buffer
	_ = r.Render(&buf)
	out := buf.String()
	if !strings.Contains(out, "kvfs_x 1\n") {
		t.Errorf("post-overwrite count wrong: %q", out)
	}
	if !strings.Contains(out, "second") {
		t.Errorf("help text not updated: %q", out)
	}
}

func TestCounterLabelMismatchPanics(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("kvfs_y", "test", "a", "b")
	defer func() {
		if recover() == nil {
			t.Errorf("expected panic on label arity mismatch")
		}
	}()
	c.WithLabels("only-one").Add(1)
}

// ADR-063 (P8-16): Histogram counts observations into cumulative buckets.
// Verifies bucket boundaries, +Inf, sum/count, and the Prometheus text
// shape downstream tooling depends on.
func TestHistogram_CumulativeBucketsSumCount(t *testing.T) {
	r := NewRegistry()
	h := r.Histogram("kvfs_test_duration_seconds", "Test histogram", []float64{0.005, 0.05, 0.5})

	// 4 observations: 1ms, 30ms, 200ms, 2s.
	h.Observe(0.001) // ≤ all 3 buckets
	h.Observe(0.030) // ≤ 0.05, ≤ 0.5
	h.Observe(0.200) // ≤ 0.5 only
	h.Observe(2.0)   // > all → only +Inf

	var buf bytes.Buffer
	_ = r.Render(&buf)
	out := buf.String()

	wants := []string{
		`kvfs_test_duration_seconds_bucket{le="0.005"} 1`,
		`kvfs_test_duration_seconds_bucket{le="0.05"} 2`,
		`kvfs_test_duration_seconds_bucket{le="0.5"} 3`,
		`kvfs_test_duration_seconds_bucket{le="+Inf"} 4`,
		`kvfs_test_duration_seconds_count 4`,
		`# TYPE kvfs_test_duration_seconds histogram`,
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
	// Sum should be 0.001 + 0.030 + 0.200 + 2.0 = 2.231 (within float tolerance).
	if !strings.Contains(out, "kvfs_test_duration_seconds_sum 2.231") {
		t.Errorf("expected sum 2.231 in:\n%s", out)
	}
}

func TestHistogram_NegativeClampsToZero(t *testing.T) {
	r := NewRegistry()
	h := r.Histogram("kvfs_test_clamp_seconds", "Negative clamp", []float64{0.001})
	h.Observe(-1.0) // should land in 0.001 bucket as 0s
	var buf bytes.Buffer
	_ = r.Render(&buf)
	out := buf.String()
	if !strings.Contains(out, `kvfs_test_clamp_seconds_bucket{le="0.001"} 1`) {
		t.Errorf("negative observation not clamped: %s", out)
	}
}

func TestSortedOutput(t *testing.T) {
	r := NewRegistry()
	r.Counter("kvfs_b_total", "b")
	r.Counter("kvfs_a_total", "a")
	r.Counter("kvfs_c_total", "c")

	var buf bytes.Buffer
	_ = r.Render(&buf)
	idxA := strings.Index(buf.String(), "kvfs_a_total")
	idxB := strings.Index(buf.String(), "kvfs_b_total")
	idxC := strings.Index(buf.String(), "kvfs_c_total")
	if !(idxA < idxB && idxB < idxC) {
		t.Errorf("metrics not sorted: a@%d b@%d c@%d", idxA, idxB, idxC)
	}
}
