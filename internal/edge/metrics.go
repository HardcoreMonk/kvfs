// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Edge-side metrics wiring. Lives in `internal/edge/` to keep the
// instrumentation hooks close to the handlers they decorate.
//
// 비전공자용 해설
// ──────────────
// `internal/metrics` 가 일반 registry 라면, 본 파일은 그 위에 kvfs-edge 의 의미
// 있는 metric 들을 등록하고, handlePut/handleGet 등 hot path 에서 호출되는
// helper (recordPut / recordGet 등) 를 제공.
//
// metric 이름 규칙: `kvfs_<area>_<type>` (Prometheus 관행).
//   - kvfs_put_total{mode}     — PUT 호출 수 (replication / ec)
//   - kvfs_get_total{mode}     — GET 호출 수
//   - kvfs_delete_total        — DELETE 호출 수
//   - kvfs_objects             — 메타에 등록된 객체 수 (gauge, callback)
//   - kvfs_dns_runtime         — runtime DN 등록 수 (gauge)
//   - kvfs_wal_last_seq        — WAL 마지막 seq (gauge)
//   - kvfs_election_state      — 0=follower, 1=candidate, 2=leader (gauge)
//   - kvfs_election_term       — 현재 term (gauge)
//
// 모든 수치는 `/metrics` endpoint 로 노출 — Prometheus scrape interval
// 마다 한번씩 callback 실행. instrumentation 자체는 atomic.Add (lock-free).
package edge

import (
	"net/http"

	"github.com/HardcoreMonk/kvfs/internal/chunker"
	"github.com/HardcoreMonk/kvfs/internal/metrics"
)

// metricsHandle bundles all kvfs-edge counters/gauges into one struct so
// Server can pass it around without scattering individual *Counter pointers.
type metricsHandle struct {
	reg            *metrics.Registry
	puts           *metrics.Counter // labels: mode
	gets           *metrics.Counter // labels: mode
	deletes        *metrics.Counter
	walAppended    *metrics.Counter // labels: op (put_object|delete_object|...)
	failover       *metrics.Counter // election term changes (informational)
	ecDegradedRead *metrics.Counter // ADR-052: EC GET stripes that needed RS Reconstruct
}

// SetupMetrics builds the registry + all metric definitions and attaches
// callback gauges to the live Server state. Idempotent (replaces existing).
func (s *Server) SetupMetrics() {
	reg := metrics.NewRegistry()
	h := &metricsHandle{
		reg:            reg,
		puts:           reg.Counter("kvfs_put_total", "Total PUT requests", "mode"),
		gets:           reg.Counter("kvfs_get_total", "Total GET requests", "mode"),
		deletes:        reg.Counter("kvfs_delete_total", "Total DELETE requests"),
		walAppended:    reg.Counter("kvfs_wal_appended_total", "WAL entries appended", "op"),
		failover:       reg.Counter("kvfs_election_term_changes_total", "Election term increments observed"),
		ecDegradedRead: reg.Counter("kvfs_ec_degraded_read_total", "EC GET stripes served with reconstruct (one or more shards missing)"),
	}

	// Object count gauge: cheap if Stats() is fast (it's not — full scan;
	// a registered counter would be better but requires invasive changes).
	reg.Gauge("kvfs_objects", "Total objects in the metadata store", func() int64 {
		st, err := s.Store.Stats()
		if err != nil {
			return -1
		}
		return int64(st.Objects)
	})
	reg.Gauge("kvfs_dns_runtime", "Active DataNodes in the runtime registry", func() int64 {
		dns, err := s.Store.ListRuntimeDNs()
		if err != nil {
			return -1
		}
		return int64(len(dns))
	})
	if s.Store.WAL() != nil {
		reg.Gauge("kvfs_wal_last_seq", "Most recent WAL sequence number", func() int64 {
			return s.Store.WAL().LastSeq()
		})
		// ADR-036: group-commit observability. Both 0 in inline mode.
		reg.Gauge("kvfs_wal_batch_size", "Entries in the most recent fsync batch (group-commit mode)", func() int64 {
			return s.Store.WAL().LastBatchSize()
		})
		reg.Gauge("kvfs_wal_durable_lag_seconds", "Age of the oldest unsynced WAL entry in seconds (0 = nothing pending)", func() int64 {
			return int64(s.Store.WAL().OldestUnsyncedAge().Seconds())
		})
	}
	if s.Elector != nil {
		reg.Gauge("kvfs_election_state", "0=follower 1=candidate 2=leader", func() int64 {
			return int64(s.Elector.State())
		})
		reg.Gauge("kvfs_election_term", "Current election term", func() int64 {
			return int64(s.Elector.CurrentTerm())
		})
	}
	if s.Heartbeat != nil {
		reg.Gauge("kvfs_dns_healthy", "DataNodes currently passing heartbeat", func() int64 {
			return int64(len(s.Heartbeat.HealthyAddrs()))
		})
	}
	// ADR-037: chunker scratch-pool memory usage (running cap-bytes total).
	reg.Gauge("kvfs_chunker_pool_bytes", "Cumulative cap of slabs held in chunker scratch pool", func() int64 {
		bytes, _ := chunker.PoolStats()
		return bytes
	})

	s.Metrics = h
}

// handleMetrics serves Prometheus text-exposition. No content negotiation;
// returns 503 if metrics weren't set up.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if s.Metrics == nil {
		http.Error(w, "metrics not enabled", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_ = s.Metrics.reg.Render(w)
}

// recordPut bumps PUT counters. mode is "replication" or "ec". Safe when
// Metrics is nil (no-op).
func (s *Server) recordPut(mode string) {
	if s.Metrics != nil {
		s.Metrics.puts.WithLabels(mode).Add(1)
	}
}

// recordGet bumps GET counters. mode is "replication" or "ec".
func (s *Server) recordGet(mode string) {
	if s.Metrics != nil {
		s.Metrics.gets.WithLabels(mode).Add(1)
	}
}

func (s *Server) recordDelete() {
	if s.Metrics != nil {
		s.Metrics.deletes.WithLabels().Add(1)
	}
}

// recordEcDegradedRead bumps the counter when an EC GET stripe needed
// Reed-Solomon Reconstruct (one or more shards unavailable). ADR-052.
func (s *Server) recordEcDegradedRead() {
	if s.Metrics != nil {
		s.Metrics.ecDegradedRead.WithLabels().Add(1)
	}
}

