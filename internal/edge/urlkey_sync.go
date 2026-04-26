// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Edge-side URLKey signer propagation (Season 6 Ep.7, ADR-049).
//
// Closes the ADR-048 gap: coord owns the kid registry, but edge has its
// own in-memory urlkey.Signer. When operator rotates via `cli urlkey
// rotate --coord ...`, the new key lives in coord.bbolt — without this
// loop, edge keeps signing/verifying with its boot-time key set forever.
//
// 비전공자용 해설
// ──────────────
// ADR-048 가 추가한 coord 의 urlkey list/rotate/remove 는 "메타" 만 다룬다.
// 실제 verify 는 edge 가 자기 메모리의 Signer 로 함. 둘이 어긋나면 새 kid 로
// 서명한 URL 이 401 — 작동 안 함.
//
// 가장 단순한 동기화: 주기적으로 coord 에 list 요청 → 우리 Signer 와 diff →
// 추가/제거/primary 변경 적용. polling interval 만큼의 lag 가 trade-off.
// 더 빠른 propagation 이 필요하면 lazy 401-refresh (verify 실패 시 즉시 한
// 번 sync 후 retry) 를 후속 ep 에서 추가 가능.
package edge

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/HardcoreMonk/kvfs/internal/store"
	"github.com/HardcoreMonk/kvfs/internal/urlkey"
)

// SyncURLKeys reconciles signer's key set with coord's view of the kid
// registry. Add new kids, remove disappeared kids, swap primary if it
// changed. Errors on individual operations are logged via failFn (caller
// supplies — the Signer methods can fail e.g. on duplicate-kid races,
// which is benign during a steady-state poll).
//
// This function is the unit testable core; the running poll loop in
// startURLKeyPoll calls it on every tick.
func SyncURLKeys(signer *urlkey.Signer, coord []store.URLKeyEntry, failFn func(error)) {
	if signer == nil {
		failFn(errors.New("urlkey-sync: nil signer"))
		return
	}
	wantSet := make(map[string]struct{}, len(coord))
	var newPrimary string
	for _, e := range coord {
		wantSet[e.Kid] = struct{}{}
		if e.IsPrimary {
			newPrimary = e.Kid
		}
		// Add if new. Add() returns ErrDuplicateKid on existing — benign.
		secret, err := hex.DecodeString(e.SecretHex)
		if err != nil {
			failFn(fmt.Errorf("decode secret for kid %q: %w", e.Kid, err))
			continue
		}
		if err := signer.Add(e.Kid, secret); err != nil && err != urlkey.ErrDuplicateKid {
			failFn(fmt.Errorf("add kid %q: %w", e.Kid, err))
		}
	}

	// SetPrimary BEFORE Remove (the latter refuses primary).
	if newPrimary != "" && newPrimary != signer.Primary() {
		if err := signer.SetPrimary(newPrimary); err != nil {
			failFn(fmt.Errorf("set primary %q: %w", newPrimary, err))
		}
	}

	// Remove kids no longer in coord. Skip primary + last (Signer refuses).
	for _, kid := range signer.Kids() {
		if _, keep := wantSet[kid]; keep {
			continue
		}
		if err := signer.Remove(kid); err != nil &&
			err != urlkey.ErrPrimaryRemove && err != urlkey.ErrLastKidRemove {
			failFn(fmt.Errorf("remove kid %q: %w", kid, err))
		}
	}
}

// urlKeyPoller runs the SyncURLKeys loop until ctx is cancelled.
// CoordClient + Signer captured by closure. Errors are logged; one bad
// poll never kills the loop.
func (s *Server) urlKeyPoller(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			coord, err := s.CoordClient.ListURLKeys(pollCtx)
			cancel()
			if err != nil {
				s.logger().Warn("urlkey-sync: list from coord failed", "err", err)
				continue
			}
			SyncURLKeys(s.Signer, coord, func(syncErr error) {
				s.logger().Warn("urlkey-sync: apply", "err", syncErr)
			})
		}
	}
}

// StartURLKeyPolling launches the background poll loop. Caller controls
// lifetime via ctx (typically the daemon's shutdown ctx). No-op when
// CoordClient is nil (inline mode — bbolt is the source of truth) OR
// when interval <= 0 (operator explicitly disabled). Default tuning
// (~30s) lives at the env-parse layer in cmd/kvfs-edge/main.go so the
// polling decision is visible at the call site.
func (s *Server) StartURLKeyPolling(ctx context.Context, interval time.Duration) {
	if s.CoordClient == nil || interval <= 0 {
		return
	}
	go s.urlKeyPoller(ctx, interval)
}
