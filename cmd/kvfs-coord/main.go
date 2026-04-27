// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// kvfs-coord — the coordinator daemon (ADR-015, Season 5 Ep.1).
//
// 비전공자용 해설
// ──────────────
// kvfs-edge 가 Season 1~4 동안 게이트웨이 + coordinator + meta-store 의
// 3 역을 했다. 단일 edge 의 bbolt single-writer 가 메타 throughput 의
// 천장이었고 멀티-edge HA 도 snapshot-pull / Raft 같은 우회로 풀었다.
// ADR-015 가 그 결정을 뒤집어 coord 를 별도 daemon 으로 분리.
//
// 책임 분담 (Season 5 close 시점):
//   kvfs-edge   — HTTP 종단, UrlKey 검증, chunker/EC encoder, DN I/O
//   kvfs-coord  — placement (HRW) + 메타 (bbolt) + 일관성 (Raft + WAL)
//   kvfs-dn     — chunk byte 저장 (변동 없음)
//   kvfs-cli    — 운영자 도구 (이젠 coord 에 직접 admin 가능)
//
// Season 6 가 추가로 worker (rebalance / GC / repair) 와 mutating admin
// (DN 추가/제거/class, URLKey rotation) 도 coord 로 옮겼다. 결과: edge 는
// 진정한 thin gateway, 모든 운영 결정은 coord 에서.
//
// HTTP RPC 표면 (현재):
//   /v1/coord/{place,commit,lookup,delete,healthz}        — Season 5 Ep.1
//   /v1/election/{vote,heartbeat,append-wal}              — Season 5 Ep.3/Ep.4
//   /v1/coord/admin/{objects,dns,urlkey}                  — Season 5 Ep.7 + Season 6 Ep.5/6
//   /v1/coord/admin/dns{,/class}                          — Season 6 Ep.5
//   /v1/coord/admin/{rebalance,gc,repair}/{plan,apply}    — Season 6 Ep.1~4
//
// Boot env (전체):
//   COORD_ADDR                   — HTTP bind (default :9000)
//   COORD_DATA_DIR               — bbolt + WAL/snapshot dir (default ./coord-data)
//   COORD_DNS                    — comma-separated DN addresses for HRW (required)
//   COORD_PEERS / COORD_SELF_URL — HA mode (ADR-038): leader election + redirect
//   COORD_WAL_PATH               — WAL replication (ADR-039): peers stay in sync
//   COORD_TRANSACTIONAL_RAFT     — replicate-then-commit (ADR-040, 진짜 strict)
//   COORD_DN_IO                  — coord-side DN HTTP client (ADR-044): apply paths
//
// Edge 사이드 (참고): EDGE_COORD_URL=http://coord:9000 으로 proxy 모드. 미설정 시
// edge 는 인라인 모드 유지 (Season 1~4 backward compat).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
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

	envDNs := splitCSV(*flagDNs)

	// Seed dns_runtime bucket from COORD_DNS on first boot so admin
	// endpoints (list/class/domain) don't 404 every DN until the operator
	// explicitly `dns add`s each one. Mirror of edge's pattern (cmd/
	// kvfs-edge/main.go ADR-027 block). dns_runtime entries persist —
	// once seeded, future boots skip and use whatever is in bbolt.
	runtimeDNs, lerr := st.ListRuntimeDNs()
	if lerr != nil {
		fatal("read dns_runtime: " + lerr.Error())
	}
	dnAddrs := envDNs
	if len(runtimeDNs) == 0 {
		if len(envDNs) == 0 {
			fatal("dns_runtime bucket empty AND COORD_DNS unset")
		}
		if err := st.SeedRuntimeDNs(envDNs); err != nil {
			fatal("seed dns_runtime: " + err.Error())
		}
		log.Info("seeded dns_runtime from COORD_DNS env", "addrs", envDNs)
	} else {
		dnAddrs = runtimeDNs
	}

	// Now load failure-domain labels (ADR-051) so the boot-time Placer
	// already knows them. Subsequent admin mutations rebuild via
	// refreshDNTopology.
	withDomain, _ := st.ListRuntimeDNsWithDomain()
	nodes := make([]placement.Node, 0, len(dnAddrs))
	for _, addr := range dnAddrs {
		nodes = append(nodes, placement.Node{ID: addr, Addr: addr, Domain: withDomain[addr]})
	}
	placer := placement.New(nodes)

	var elector *election.Elector
	var peerURLs []string
	if *flagPeers != "" {
		if *flagSelfURL == "" {
			fatal("COORD_SELF_URL required when COORD_PEERS is set")
		}
		peerURLs = splitCSV(*flagPeers)
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

		// LastLogSeqFn (P8-06): expose WAL.LastSeq for Raft §5.4.1
		// log-up-to-date vote check. Returns 0 if WAL not configured —
		// election degrades to term-only (Ep.3 behavior) which is fine for
		// non-WAL clusters since they have no committed log to protect.
		// WAL.LastSeq is int64 (signed monotonic counter); election uses
		// uint64 — cast in the closure since the value is always ≥0.
		var lastLogSeqFn func() uint64
		if w := st.WAL(); w != nil {
			lastLogSeqFn = func() uint64 { return uint64(w.LastSeq()) }
		}

		elector = election.New(election.Config{
			SelfID:        *flagSelfURL,
			Peers:         peers,
			Log:           log,
			AppendEntryFn: appendFn,
			LastLogSeqFn:  lastLogSeqFn,
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

	// P8-06: bootstrap snapshot pull. If we are starting fresh OR with a
	// stale bbolt (e.g. crashed and restarting after peers committed new
	// entries), grab the current snapshot from a peer before joining the
	// election. Without this, ADR-039's push-only WAL replication can't
	// retroactively deliver missed entries — the chaos-mixed gap.
	//
	// Best-effort: if no peer is reachable (cluster bootstrap, full outage),
	// proceed without a snapshot. The election runs anyway and we'll catch
	// up via ongoing walHook pushes once peers come back.
	if elector != nil && st.WAL() != nil {
		if err := bootstrapFromPeer(*flagSelfURL, peerURLs, *flagDataDir, st, log); err != nil {
			log.Warn("bootstrap snapshot pull failed (proceeding with local state)", "err", err)
		}
	}

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

// bootstrapFromPeer attempts to fetch the current bbolt snapshot from any
// reachable peer, write it next to our data dir, and Reload the local
// MetaStore. Best-effort — returns the last error encountered if no peer
// served us, which the caller logs and proceeds anyway. Skips self.
//
// Strategy: probe peers in order, prefer one whose healthz says
// role=leader (most likely to have the freshest log). Fall back to any
// peer that responds — followers also serve the snapshot endpoint, and
// any quorum-acked entry must be on at least one of them.
//
// Closes the chaos-mixed gap (P8-06): a coord that died during a commit
// window now starts with the cluster's current state instead of stale
// (or empty) bbolt, so the §5.4.1 log-up-to-date vote check can do its
// job and a stale-log election win becomes impossible.
func bootstrapFromPeer(selfURL string, peerURLs []string, dataDir string, st *store.MetaStore, log *slog.Logger) error {
	candidates := make([]string, 0, len(peerURLs))
	for _, u := range peerURLs {
		if u != selfURL {
			candidates = append(candidates, u)
		}
	}
	if len(candidates) == 0 {
		return nil // single-coord cluster; nothing to pull
	}
	// Prefer leader; healthz is cheap.
	httpC := &http.Client{Timeout: 2 * time.Second}
	leaderFirst := make([]string, 0, len(candidates))
	rest := make([]string, 0, len(candidates))
	for _, u := range candidates {
		role := probeRole(httpC, u)
		if role == "leader" {
			leaderFirst = append(leaderFirst, u)
		} else {
			rest = append(rest, u)
		}
	}
	ordered := append(leaderFirst, rest...)

	tmpPath := filepath.Join(dataDir, "bootstrap.snapshot.tmp")
	finalPath := filepath.Join(dataDir, "bootstrap.snapshot.db")

	var lastErr error
	for _, peer := range ordered {
		log.Info("bootstrap: trying peer", "url", peer)
		if err := fetchSnapshot(peer, tmpPath); err != nil {
			lastErr = err
			log.Warn("bootstrap: peer fetch failed", "url", peer, "err", err)
			continue
		}
		// Atomic rename — Reload requires a stable file path.
		if err := os.Rename(tmpPath, finalPath); err != nil {
			lastErr = fmt.Errorf("rename: %w", err)
			continue
		}
		if err := st.Reload(finalPath); err != nil {
			lastErr = fmt.Errorf("reload: %w", err)
			continue
		}
		log.Info("bootstrap: snapshot loaded", "url", peer, "path", finalPath)
		return nil
	}
	return lastErr
}

func probeRole(httpC *http.Client, peerURL string) string {
	resp, err := httpC.Get(peerURL + "/v1/coord/healthz")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var body struct{ Role string `json:"role"` }
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ""
	}
	return body.Role
}

func fetchSnapshot(peerURL, dst string) error {
	httpC := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpC.Get(peerURL + "/v1/coord/admin/meta/snapshot")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return err
	}
	return f.Sync()
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
