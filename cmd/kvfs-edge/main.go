// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Command kvfs-edge is the gateway + inline coordinator.
//
// Config (env vars, with flag fallback):
//
//	EDGE_ADDR            string   ":8000"                — HTTP bind address
//	EDGE_DNS             string   required (comma-sep)   — DN addresses, e.g. "dn1:8080,dn2:8080,dn3:8080"
//	EDGE_DATA_DIR        string   "./edge-data"           — where to put bbolt file
//	EDGE_URLKEY_SECRET   string   required               — HMAC-SHA256 secret (>= 16 bytes recommended)
//	EDGE_QUORUM_WRITE    int      auto (ceil(N/2+1))     — min replica acks for success
//	EDGE_CHUNK_SIZE      int      4194304 (4 MiB)        — bytes per chunk (ADR-011)
//	EDGE_AUTO            bool     0                      — opt-in auto rebalance + GC (ADR-013)
//	EDGE_AUTO_REBALANCE_INTERVAL  duration  5m           — auto rebalance ticker interval
//	EDGE_AUTO_GC_INTERVAL         duration  15m          — auto GC ticker interval
//	EDGE_AUTO_GC_MIN_AGE          duration  60s          — min chunk age for auto-GC
//	EDGE_AUTO_CONCURRENCY         int       4            — parallel ops per cycle
//	EDGE_SKIP_AUTH       bool     false                  — DEMO ONLY: disable UrlKey verification
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/HardcoreMonk/kvfs/internal/chunker"
	"github.com/HardcoreMonk/kvfs/internal/coordinator"
	"github.com/HardcoreMonk/kvfs/internal/edge"
	"github.com/HardcoreMonk/kvfs/internal/store"
	"github.com/HardcoreMonk/kvfs/internal/urlkey"
)

func main() {
	var (
		flagAddr    = flag.String("addr", envOr("EDGE_ADDR", ":8000"), "HTTP bind address")
		flagDNs     = flag.String("dns", envOr("EDGE_DNS", ""), "comma-separated DN addresses")
		flagDataDir = flag.String("data-dir", envOr("EDGE_DATA_DIR", "./edge-data"), "dir for bbolt file")
		flagSecret  = flag.String("secret", envOr("EDGE_URLKEY_SECRET", ""), "HMAC-SHA256 secret")
		flagQuorum  = flag.Int("quorum", atoiOr(envOr("EDGE_QUORUM_WRITE", "0"), 0), "write quorum; 0 = auto")
		flagChunk   = flag.Int("chunk-size", atoiOr(envOr("EDGE_CHUNK_SIZE", "0"), 0), "bytes per chunk (ADR-011); 0 = default 4 MiB")
		flagAuto    = flag.Bool("auto", envOr("EDGE_AUTO", "") == "1", "enable auto rebalance + GC loops (ADR-013)")
		flagAutoRb  = flag.String("auto-rebalance-interval", envOr("EDGE_AUTO_REBALANCE_INTERVAL", "5m"), "auto rebalance ticker interval")
		flagAutoGC  = flag.String("auto-gc-interval", envOr("EDGE_AUTO_GC_INTERVAL", "15m"), "auto GC ticker interval")
		flagAutoMin = flag.String("auto-gc-min-age", envOr("EDGE_AUTO_GC_MIN_AGE", "60s"), "min chunk age for auto GC")
		flagAutoCnc = flag.Int("auto-concurrency", atoiOr(envOr("EDGE_AUTO_CONCURRENCY", "4"), 4), "parallel ops per auto cycle")
		flagSkip    = flag.Bool("skip-auth", envOr("EDGE_SKIP_AUTH", "") == "1", "DEMO ONLY: skip UrlKey verify")
	)
	flag.Parse()

	if *flagDNs == "" {
		fatal("EDGE_DNS / -dns required")
	}
	if !*flagSkip && *flagSecret == "" {
		fatal("EDGE_URLKEY_SECRET / -secret required (or set EDGE_SKIP_AUTH=1 for demo)")
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	if err := os.MkdirAll(*flagDataDir, 0o755); err != nil {
		fatal(err.Error())
	}
	metaPath := filepath.Join(*flagDataDir, "edge.db")

	ms, err := store.Open(metaPath)
	if err != nil {
		fatal(err.Error())
	}
	defer ms.Close()

	envDNs := splitTrim(*flagDNs)

	// ADR-027 dynamic DN registry: dns_runtime bucket overrides EDGE_DNS env
	// when populated. EDGE_DNS_RESET=1 forces re-seed from env (recovery).
	dns := envDNs
	runtimeDNs, lerr := ms.ListRuntimeDNs()
	if lerr != nil {
		fatal("read dns_runtime: " + lerr.Error())
	}
	if envOr("EDGE_DNS_RESET", "") == "1" || len(runtimeDNs) == 0 {
		if len(envDNs) == 0 {
			fatal("dns_runtime bucket empty AND EDGE_DNS unset")
		}
		if err := ms.SeedRuntimeDNs(envDNs); err != nil {
			fatal("seed dns_runtime: " + err.Error())
		}
		log.Info("seeded dns_runtime from EDGE_DNS env", "addrs", envDNs)
	} else {
		dns = runtimeDNs
		log.Info("using dns_runtime from bbolt (EDGE_DNS env ignored; set EDGE_DNS_RESET=1 to override)",
			"addrs", dns)
	}

	// ReplicationFactor = 3 by default for 3-way replication MVP.
	coord, err := coordinator.NewWithAddrs(dns, 3, *flagQuorum, 10*time.Second)
	if err != nil {
		fatal(err.Error())
	}

	var signer *urlkey.Signer
	if !*flagSkip {
		signer, err = urlkey.NewSigner([]byte(*flagSecret))
		if err != nil {
			fatal(err.Error())
		}
	}

	chunkSize := *flagChunk
	if chunkSize <= 0 {
		chunkSize = chunker.DefaultChunkSize
	}

	autoCfg := edge.AutoConfig{Enabled: *flagAuto, Concurrency: *flagAutoCnc}
	parseAutoDur := func(name, raw string, dst *time.Duration) {
		d, err := time.ParseDuration(raw)
		if err != nil {
			if *flagAuto {
				fatal("invalid " + name + ": " + err.Error())
			}
			return
		}
		*dst = d
	}
	parseAutoDur("EDGE_AUTO_REBALANCE_INTERVAL", *flagAutoRb, &autoCfg.RebalanceInterval)
	parseAutoDur("EDGE_AUTO_GC_INTERVAL", *flagAutoGC, &autoCfg.GCInterval)
	parseAutoDur("EDGE_AUTO_GC_MIN_AGE", *flagAutoMin, &autoCfg.GCMinAge)

	srv := &edge.Server{
		Store:     ms,
		Coord:     coord,
		Signer:    signer,
		Log:       log,
		ChunkSize: chunkSize,
		AutoCfg:   autoCfg,
		SkipAuth:  *flagSkip,
	}

	log.Info("kvfs-edge starting",
		"addr", *flagAddr,
		"dns", dns,
		"quorum_write", coord.QuorumWrite(),
		"chunk_size", chunkSize,
		"auto", autoCfg.Enabled,
		"meta_path", metaPath,
		"skip_auth", *flagSkip,
	)
	if *flagSkip {
		log.Warn("UrlKey verification is DISABLED. Do not use in production.")
	}

	httpSrv := &http.Server{
		Addr:              *flagAddr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Start auto-trigger loops (no-op if AutoCfg.Enabled is false). They
	// exit when ctx is cancelled by signal.
	srv.StartAuto(ctx)

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			log.Error("listen error", "err", err)
		}
	}

	shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shCtx)
	log.Info("kvfs-edge stopped")
}

func envOr(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

func splitTrim(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "kvfs-edge: "+msg)
	os.Exit(2)
}
