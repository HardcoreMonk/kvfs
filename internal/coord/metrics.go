// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Coord-side metrics wiring (ADR-062, P8-15). Lives in `internal/coord/`
// to keep the instrumentation hooks close to the handlers they decorate.
//
// 비전공자용 해설
// ──────────────
// `internal/edge/metrics.go` 가 edge 의 metric 채널이라면, 본 파일은 coord
// 의 동등한 surface. anti-entropy 의 audit / repair / unrecoverable / throttle
// 사건이 hot path 외라 atomic.Add 자체 비용은 무시 가능 — slog.Error 만으로
// 알림 가능하지만 (ADR-061), Prometheus rate / alert / dashboard 와 결합
// 하려면 counter 가 필요.
//
// metric 이름 규칙은 edge 와 동일 (`kvfs_<area>_<type>`):
//
//   - kvfs_anti_entropy_audits_total            — audit 호출 수
//   - kvfs_anti_entropy_repairs_total{reason}   — 성공한 repair 수 (missing|corrupt|ec)
//   - kvfs_anti_entropy_unrecoverable_total     — 모든 replica 잃은 chunk 발생 수
//   - kvfs_anti_entropy_throttled_total         — max_repairs budget 초과 outcome 수
//   - kvfs_anti_entropy_auto_repair_runs_total  — auto-repair ticker 실행 수 (성공/실패 무관)
//
// coord 는 Elector / Store gauge 도 보탬:
//   - kvfs_coord_election_state                 — 0=follower, 1=candidate, 2=leader
//   - kvfs_coord_objects                        — 메타 객체 수
//
// /metrics endpoint 는 cmd/kvfs-coord/main.go 의 `--metrics` 플래그로 활성.
// default on (운영자가 끄려면 `--metrics=false` 또는 `COORD_METRICS=0`).
package coord

import (
	"net/http"
	"time"

	"github.com/HardcoreMonk/kvfs/internal/election"
	"github.com/HardcoreMonk/kvfs/internal/metrics"
)

// metricsHandle bundles all kvfs-coord counters / gauges into one struct so
// Server can pass it around without scattering individual *Counter pointers.
// nil-safe access via the helper methods on Server below — handlers don't
// have to check whether SetupMetrics ran.
type metricsHandle struct {
	reg            *metrics.Registry
	audits         *metrics.Counter   // anti-entropy audits
	repairs        *metrics.Counter   // labels: reason (missing|corrupt|ec)
	unrecoverable  *metrics.Counter   // chunks with no surviving replica
	throttled      *metrics.Counter   // outcomes capped by max_repairs
	autoRepairRuns *metrics.Counter   // scheduled auto-repair invocations
	skipped        *metrics.Counter   // ADR-063: labels: mode (skip|ec-deferred)
	auditDuration  *metrics.Histogram // ADR-063: runAntiEntropy wallclock
	repairDuration *metrics.Histogram // ADR-063: runAntiEntropyRepair wallclock
}

// histBucketsSeconds: low-latency ladder from 1ms to 30s. Captures the
// realistic spread for kvfs anti-entropy ops — small audits run in tens
// of ms, large repairs (corrupt + EC + many DNs) push to seconds. 30s
// is the saturation marker; anything beyond is +Inf.
var histBucketsSeconds = []float64{
	0.001, 0.002, 0.005, 0.010, 0.025, 0.050, 0.100, 0.250,
	0.500, 1.0, 2.5, 5.0, 10.0, 30.0,
}

// SetupMetrics builds the registry + counter / gauge definitions and stores
// them on the Server. Idempotent (replaces any existing registry).
func (s *Server) SetupMetrics() {
	reg := metrics.NewRegistry()
	h := &metricsHandle{
		reg:            reg,
		audits:         reg.Counter("kvfs_anti_entropy_audits_total", "Anti-entropy audits initiated"),
		repairs:        reg.Counter("kvfs_anti_entropy_repairs_total", "Successful anti-entropy repairs", "reason"),
		unrecoverable:  reg.Counter("kvfs_anti_entropy_unrecoverable_total", "Distinct chunks with no surviving replica (deduped, ADR-063)"),
		throttled:      reg.Counter("kvfs_anti_entropy_throttled_total", "Repair outcomes capped by max_repairs budget"),
		autoRepairRuns: reg.Counter("kvfs_anti_entropy_auto_repair_runs_total", "Auto-repair ticker invocations (leader-only)"),
		skipped:        reg.Counter("kvfs_anti_entropy_skipped_total", "Anti-entropy outcomes skipped without repair attempt", "mode"),
		auditDuration:  reg.Histogram("kvfs_anti_entropy_audit_duration_seconds", "Anti-entropy audit wallclock", histBucketsSeconds),
		repairDuration: reg.Histogram("kvfs_anti_entropy_repair_duration_seconds", "Anti-entropy repair wallclock", histBucketsSeconds),
	}

	if s.Elector != nil {
		reg.Gauge("kvfs_coord_election_state", "0=follower, 1=candidate, 2=leader", func() int64 {
			switch s.Elector.State() {
			case election.Follower:
				return 0
			case election.Candidate:
				return 1
			case election.Leader:
				return 2
			}
			return -1
		})
	}
	if s.Store != nil {
		reg.Gauge("kvfs_coord_objects", "Objects in coord's metadata store", func() int64 {
			st, err := s.Store.Stats()
			if err != nil {
				return -1
			}
			return int64(st.Objects)
		})
	}

	s.metrics = h
}

// handleMetrics serves the Prometheus text-exposition format. 200 OK with
// an empty body when SetupMetrics hasn't been called — mirrors edge's
// permissive shape so a probe doesn't fail-closed.
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	if s.metrics == nil {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_ = s.metrics.reg.Render(w)
}

// recordAudit / recordRepair / recordUnrecoverable / recordThrottled are
// nil-safe entry points for the anti-entropy code. Production code calls
// these directly; tests can assert via the counters' .Value().
func (s *Server) recordAudit() {
	if s.metrics != nil {
		s.metrics.audits.Inc()
	}
}

func (s *Server) recordRepair(reason string) {
	if s.metrics != nil {
		s.metrics.repairs.WithLabels(reason).Add(1)
	}
}

func (s *Server) recordUnrecoverable() {
	if s.metrics != nil {
		s.metrics.unrecoverable.Inc()
	}
}

func (s *Server) recordThrottled() {
	if s.metrics != nil {
		s.metrics.throttled.Inc()
	}
}

func (s *Server) recordAutoRepairRun() {
	if s.metrics != nil {
		s.metrics.autoRepairRuns.Inc()
	}
}

// recordSkipped (ADR-063): mode="skip" (DN unreachable) or "ec-deferred"
// (EC chunk without ec=1 flag). Counts the outcome the operator chose to
// defer — distinct from no-source / throttled which the system decided.
func (s *Server) recordSkipped(mode string) {
	if s.metrics != nil {
		s.metrics.skipped.WithLabels(mode).Add(1)
	}
}

// recordAuditDuration / recordRepairDuration (ADR-063): wallclock for the
// two hot paths. Bucket ladder is fixed (histBucketsSeconds); operators
// query rate(_bucket{le="..."}) for percentiles via histogram_quantile().
func (s *Server) recordAuditDuration(d time.Duration) {
	if s.metrics != nil {
		s.metrics.auditDuration.Observe(d.Seconds())
	}
}

func (s *Server) recordRepairDuration(d time.Duration) {
	if s.metrics != nil {
		s.metrics.repairDuration.Observe(d.Seconds())
	}
}
