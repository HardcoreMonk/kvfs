// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// kvfs-coord — the coordinator daemon (ADR-015, Season 5 Ep.1).
//
// Owns:
//   - HRW placement (the runtime DN list — single source of truth)
//   - bbolt metadata (objects, dns_runtime, urlkey_secrets buckets)
//   - WAL + snapshot (subset of S3/S4 capabilities; full carry-over is later eps)
//
// HTTP RPC (versioned at /v1/coord/*):
//   POST  /v1/coord/place   — placement decision
//   POST  /v1/coord/commit  — write ObjectMeta
//   GET   /v1/coord/lookup  — read ObjectMeta
//   POST  /v1/coord/delete  — remove ObjectMeta
//   GET   /v1/coord/healthz
//
// Boot env (subset; full list will grow with later eps):
//   COORD_ADDR       — HTTP bind (default :9000)
//   COORD_DATA_DIR   — bbolt + (later) WAL/snapshot dir (default ./coord-data)
//   COORD_DNS        — comma-separated DN addresses for HRW (required)
//
// Edge wires to coord via EDGE_COORD_URL=http://coord:9000.
// When EDGE_COORD_URL is unset, edge stays in inline mode (Season 1~4
// behavior preserved — backward compat is the Ep.1 contract).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/HardcoreMonk/kvfs/internal/coord"
	"github.com/HardcoreMonk/kvfs/internal/election"
	"github.com/HardcoreMonk/kvfs/internal/placement"
	"github.com/HardcoreMonk/kvfs/internal/store"
)

func main() {
	var (
		flagAddr    = flag.String("addr", envOr("COORD_ADDR", ":9000"), "HTTP bind address")
		flagDataDir = flag.String("data-dir", envOr("COORD_DATA_DIR", "./coord-data"), "dir for bbolt file")
		flagDNs     = flag.String("dns", envOr("COORD_DNS", ""), "comma-separated DN addresses (required)")
		// HA mode (Ep.3, ADR-038): set both to enable Raft-style leader election.
		flagPeers   = flag.String("peers", envOr("COORD_PEERS", ""), "comma-separated peer URLs incl. self, e.g. http://coord1:9000,http://coord2:9000,http://coord3:9000 (empty = single-coord mode)")
		flagSelfURL = flag.String("self-url", envOr("COORD_SELF_URL", ""), "this coord's own URL (must match an entry in -peers when -peers set)")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	if *flagDNs == "" {
		fatal("COORD_DNS / -dns required (comma-separated DN addresses)")
	}

	if err := os.MkdirAll(*flagDataDir, 0o755); err != nil {
		fatal(err.Error())
	}
	metaPath := filepath.Join(*flagDataDir, "coord.db")

	st, err := store.Open(metaPath)
	if err != nil {
		fatal(err.Error())
	}
	defer st.Close()

	dnAddrs := splitCSV(*flagDNs)
	nodes := make([]placement.Node, 0, len(dnAddrs))
	for _, addr := range dnAddrs {
		nodes = append(nodes, placement.Node{ID: addr, Addr: addr})
	}
	placer := placement.New(nodes)

	var elector *election.Elector
	if *flagPeers != "" {
		if *flagSelfURL == "" {
			fatal("COORD_SELF_URL required when COORD_PEERS is set")
		}
		peerURLs := splitCSV(*flagPeers)
		peers := make([]election.Peer, 0, len(peerURLs))
		for _, u := range peerURLs {
			peers = append(peers, election.Peer{ID: u, URL: u})
		}
		elector = election.New(election.Config{
			SelfID: *flagSelfURL,
			Peers:  peers,
			Log:    log,
		})
		log.Info("kvfs-coord HA mode (Ep.3)", "self", *flagSelfURL, "peers", len(peers))
	}

	srv := &coord.Server{
		Store:   st,
		Placer:  placer,
		Log:     log,
		Elector: elector,
	}

	httpSrv := &http.Server{
		Addr:              *flagAddr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if elector != nil {
		go elector.Run(ctx)
	}

	go func() {
		log.Info("kvfs-coord listening", "addr", *flagAddr, "dns", len(dnAddrs), "data", metaPath)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("listen", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	log.Info("kvfs-coord shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "kvfs-coord:", msg)
	os.Exit(1)
}
