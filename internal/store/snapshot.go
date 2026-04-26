// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Snapshot/Restore (ADR-014): hot snapshot of the metadata bbolt file via
// Tx.WriteTo. Restore is offline (operator stops edge, swaps the file, restarts).
//
// 비전공자용 해설
// ──────────────
// 메타 손상 = 클러스터 인지 끊김. chunk 데이터는 살아있어도 어떤 객체가 어떤
// chunk 묶음인지 알 길이 없다. 정기 snapshot 으로 안전망:
//
//   1. Snapshot — bbolt 의 read transaction 안에서 tx.WriteTo(w). atomic
//      consistent point-in-time copy. edge 동작 중에도 가능 (writers 잠깐 대기).
//   2. Stream — HTTP GET 으로 snapshot 바이트 그대로 흘려보냄.
//   3. Restore — edge 정지 → snapshot 파일 → 데이터 디렉토리 교체 → 재시작.
//      MVP 는 in-process restore 안 함 (bbolt file lock conflict 위험).
package store

import (
	"fmt"
	"io"

	"go.etcd.io/bbolt"
)

// Snapshot writes a consistent point-in-time copy of the metadata DB to w.
// Returns bytes written. Safe while writers are active — bbolt read tx coexists
// with one writer.
func (m *MetaStore) Snapshot(w io.Writer) (int64, error) {
	var n int64
	err := m.db.Load().View(func(tx *bbolt.Tx) error {
		var werr error
		n, werr = tx.WriteTo(w)
		return werr
	})
	if err != nil {
		return n, fmt.Errorf("snapshot: %w", err)
	}
	return n, nil
}

// MetaStats summarizes the current store contents (read-only).
type MetaStats struct {
	Objects     int   `json:"objects"`
	ECObjects   int   `json:"ec_objects"`
	ChunkCount  int   `json:"chunk_count"`
	StripeCount int   `json:"stripe_count"`
	ShardCount  int   `json:"shard_count"`
	DNs         int   `json:"dns"`
	RuntimeDNs  int   `json:"runtime_dns"`
	URLKeys     int   `json:"url_keys"`
	BBoltBytes  int64 `json:"bbolt_size_bytes"`
}

// Stats returns aggregate counts for /v1/admin/meta/info and CLI display.
func (m *MetaStore) Stats() (MetaStats, error) {
	var s MetaStats
	objs, err := m.ListObjects()
	if err != nil {
		return s, fmt.Errorf("stats: list objects: %w", err)
	}
	s.Objects = len(objs)
	for _, o := range objs {
		if o.IsEC() {
			s.ECObjects++
			for _, st := range o.Stripes {
				s.StripeCount++
				s.ShardCount += len(st.Shards)
			}
		} else {
			s.ChunkCount += len(o.Chunks)
		}
	}
	if dns, err := m.ListDNs(); err == nil {
		s.DNs = len(dns)
	}
	if rdns, err := m.ListRuntimeDNs(); err == nil {
		s.RuntimeDNs = len(rdns)
	}
	if keys, err := m.ListURLKeys(); err == nil {
		s.URLKeys = len(keys)
	}
	// bbolt logical size = pages × pageSize via a read tx (cheap).
	_ = m.db.Load().View(func(tx *bbolt.Tx) error {
		s.BBoltBytes = tx.Size()
		return nil
	})
	return s, nil
}
