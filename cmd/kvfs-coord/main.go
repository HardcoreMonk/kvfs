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
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/HardcoreMonk/kvfs/internal/cliutil"
	"github.com/HardcoreMonk/kvfs/internal/coord"
	"github.com/HardcoreMonk/kvfs/internal/coordinator"
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
		// WAL replication (Ep.4, ADR-039): leader pushes each WAL entry to
		// peers; followers ApplyEntry to keep bbolt in sync. Empty = no WAL
		// (Ep.3 behavior — followers' bbolt stays empty after election).
		flagWALPath = flag.String("wal-path", envOr("COORD_WAL_PATH", ""), "WAL file (ADR-019). Required for coord-to-coord sync (ADR-039)")
		// Transactional commit (Ep.5, ADR-040): replicate-then-commit, no
		// phantom writes on leader-loss. Requires both -peers and -wal-path.
		flagTxnRaft = flag.Bool("transactional-raft", envOr("COORD_TRANSACTIONAL_RAFT", "") == "1", "ADR-040: replicate-then-commit semantics. Requires COORD_PEERS + COORD_WAL_PATH.")
		// DN I/O (Ep.2, ADR-044): coord embeds a coordinator.Coordinator
		// instance to do chunk Read/Put against DNs directly. Required for
		// rebalance/gc/repair APPLY paths to run on coord.
		flagDNIO = flag.Bool("dn-io", envOr("COORD_DN_IO", "") == "1", "ADR-044: enable coord-side DN I/O so apply paths (rebalance/gc/repair) run on coord")
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

	// Optional WAL — required for ADR-039 coord-to-coord sync. Without it,
	// HA mode (COORD_PEERS) still elects but follower coords' bbolt stays
	// empty (the Ep.3 known gap).
	if *flagWALPath != "" {
		wal, werr := store.OpenWAL(*flagWALPath)
		if werr != nil {
			fatal("WAL open: " + werr.Error())
		}
		st.SetWAL(wal)
		defer wal.Close()
		log.Info("kvfs-coord WAL enabled", "path", *flagWALPath, "last_seq", wal.LastSeq())
	}

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

		// AppendEntryFn (ADR-039): a follower coord receives a pushed WAL
		// entry from the current leader → decode → apply locally. This is
		// what keeps follower bbolts in sync with the leader's, closing
		// the Ep.3 gap. NOOP when no WAL is configured (sync requires WAL).
		var appendFn election.AppendEntryFunc
		if st.WAL() != nil {
			appendFn = func(entryBody []byte) error {
				var entry store.WALEntry
				if err := json.Unmarshal(entryBody, &entry); err != nil {
					return fmt.Errorf("decode wal entry: %w", err)
				}
				return st.ApplyEntry(entry)
			}
		}

		elector = election.New(election.Config{
			SelfID:        *flagSelfURL,
			Peers:         peers,
			Log:           log,
			AppendEntryFn: appendFn,
		})

		// WAL hook on the leader side (ADR-039): every successful local
		// WAL.Append calls this with the raw JSON line. We push it to peers
		// in parallel and wait for quorum. If we're not the leader we
		// silently skip — followers shouldn't push anything.
		if st.WAL() != nil {
			st.SetWALHook(func(line []byte) error {
				if !elector.IsLeader() {
					return nil
				}
				ctxRep, cancelRep := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancelRep()
				return elector.ReplicateEntry(ctxRep, line)
			})
		}

		log.Info("kvfs-coord HA mode (Ep.3+Ep.4)",
			"self", *flagSelfURL, "peers", len(peers),
			"wal_replication", st.WAL() != nil)
	}

	if *flagTxnRaft {
		switch {
		case elector == nil:
			fatal("COORD_TRANSACTIONAL_RAFT=1 requires COORD_PEERS")
		case st.WAL() == nil:
			fatal("COORD_TRANSACTIONAL_RAFT=1 requires COORD_WAL_PATH")
		}
		log.Info("kvfs-coord transactional commit (Ep.5, ADR-040)",
			"semantics", "replicate-then-commit; quorum failure → 503 + no local commit")
	}

	var dnCoord *coordinator.Coordinator
	if *flagDNIO {
		dnCoord, err = coordinator.New(coordinator.Config{Nodes: nodes})
		if err != nil {
			fatal("coordinator.New: " + err.Error())
		}
		log.Info("kvfs-coord DN I/O enabled (Ep.2, ADR-044)",
			"note", "rebalance/gc/repair apply paths runnable on coord")
	}

	srv := &coord.Server{
		Store:               st,
		Placer:              placer,
		Log:                 log,
		Elector:             elector,
		TransactionalCommit: *flagTxnRaft,
		Coord:               dnCoord,
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

// Local thin shims so existing call sites stay tight. Helpers themselves
// live in internal/cliutil for cross-binary reuse (P6-09).
//
// Note: the extracted EnvOr uses os.LookupEnv (treats empty strings as
// "set"), which is slightly different from the prior local envOr that
// used os.Getenv (treats empty as unset). All COORD_* defaults are
// safe under the lookup behavior — operator setting an empty env was
// a misconfig either way. Coord-specific Fatal prefix preserved here.
func envOr(k, def string) string  { return cliutil.EnvOr(k, def) }
func splitCSV(s string) []string  { return cliutil.SplitCSV(s) }
func fatal(msg string)            { cliutil.Fatal("kvfs-coord: " + msg) }
