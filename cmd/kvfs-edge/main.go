// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Command kvfs-edge is the gateway + inline coordinator.
//
// Config (env vars, with flag fallback) — full table in ../../README.md
// "환경 변수" 섹션. The most-used keys:
//
//	EDGE_ADDR / EDGE_DNS / EDGE_DATA_DIR / EDGE_URLKEY_SECRET   (required basics)
//	EDGE_QUORUM_WRITE / EDGE_CHUNK_SIZE                         (write tunables)
//	EDGE_AUTO + EDGE_AUTO_*                                     (ADR-013)
//	EDGE_HEARTBEAT_*                                            (ADR-030)
//	EDGE_SNAPSHOT_*                                             (ADR-016)
//	EDGE_ROLE / EDGE_PRIMARY_URL / EDGE_FOLLOWER_PULL_INTERVAL  (ADR-022)
//	EDGE_TLS_* / EDGE_DN_TLS_*                                  (ADR-029)
//	EDGE_SKIP_AUTH                                              (DEMO only)
//
// 비전공자용 해설
// ──────────────
// main 의 일은 wiring 에 가깝다:
//
//  1. env / flag 파싱
//  2. bbolt (메타) open
//  3. dns_runtime bucket → coordinator (ADR-027 동적 registry)
//  4. urlkey_secrets bucket → multi-key Signer (ADR-028)
//  5. (선택) TLS / mTLS config
//  6. edge.Server 조립 + Heartbeat / SnapshotScheduler / FollowerConfig 주입
//  7. http.Server 띄우고 SIGTERM 대기
//  8. graceful shutdown (autoLoop ctx 취소 → bbolt close)
//
// 모든 backend 로직은 internal/edge·internal/coordinator·internal/store 등에
// 위임. 이 파일은 env → struct 매핑 + 라이프사이클 관리만.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
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

	"crypto/tls"

	"github.com/HardcoreMonk/kvfs/internal/chunker"
	"github.com/HardcoreMonk/kvfs/internal/coordinator"
	"github.com/HardcoreMonk/kvfs/internal/edge"
	"github.com/HardcoreMonk/kvfs/internal/election"
	"github.com/HardcoreMonk/kvfs/internal/heartbeat"
	"github.com/HardcoreMonk/kvfs/internal/placement"
	"github.com/HardcoreMonk/kvfs/internal/store"
	"github.com/HardcoreMonk/kvfs/internal/tlsutil"
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
		flagHB      = flag.String("heartbeat-interval", envOr("EDGE_HEARTBEAT_INTERVAL", "10s"), "DN heartbeat probe interval (ADR-030); 0s disables")
		flagHBFail  = flag.Int("heartbeat-fail-threshold", atoiOr(envOr("EDGE_HEARTBEAT_FAIL_THRESHOLD", "3"), 3), "consecutive probe failures before DN marked unhealthy")
		flagSnapDir = flag.String("snapshot-dir", envOr("EDGE_SNAPSHOT_DIR", ""), "auto-snapshot output directory (ADR-016); empty disables")
		flagSnapInt = flag.String("snapshot-interval", envOr("EDGE_SNAPSHOT_INTERVAL", "1h"), "auto-snapshot ticker interval")
		flagSnapKp  = flag.Int("snapshot-keep", atoiOr(envOr("EDGE_SNAPSHOT_KEEP", "7"), 7), "how many recent snapshots to retain")
		flagChunkMd  = flag.String("chunk-mode", envOr("EDGE_CHUNK_MODE", "fixed"), "PUT chunker mode (ADR-018): fixed | cdc")
		flagPeers    = flag.String("peers", envOr("EDGE_PEERS", ""), "ADR-031 election: comma-sep peer URLs (incl. self), e.g. 'http://edge1:8000,http://edge2:8000'; empty disables election")
		flagSelfURL  = flag.String("self-url", envOr("EDGE_SELF_URL", ""), "ADR-031 election: this edge's own peer URL; required when -peers set")
		flagElectHB  = flag.String("election-heartbeat-interval", envOr("EDGE_ELECTION_HB_INTERVAL", "500ms"), "leader heartbeat cadence (election mode)")
		flagElectMin = flag.String("election-timeout-min", envOr("EDGE_ELECTION_TIMEOUT_MIN", "1500ms"), "follower → candidate timeout (min)")
		flagElectMax = flag.String("election-timeout-max", envOr("EDGE_ELECTION_TIMEOUT_MAX", "3000ms"), "follower → candidate timeout (max)")
		flagWALPath  = flag.String("wal-path", envOr("EDGE_WAL_PATH", ""), "ADR-019 WAL file path (empty disables WAL)")
		flagMetrics  = flag.Bool("metrics", envOr("EDGE_METRICS", "1") == "1", "expose /metrics Prometheus endpoint (default on)")
		flagRole    = flag.String("role", envOr("EDGE_ROLE", "primary"), "edge role (ADR-022): primary | follower")
		flagPrim    = flag.String("primary-url", envOr("EDGE_PRIMARY_URL", ""), "follower-only: primary edge base URL (e.g. http://primary:8000)")
		flagPullInt = flag.String("follower-pull-interval", envOr("EDGE_FOLLOWER_PULL_INTERVAL", "30s"), "follower-only: snapshot pull interval")
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

	if *flagWALPath != "" {
		wal, werr := store.OpenWAL(*flagWALPath)
		if werr != nil {
			fatal("WAL open: " + werr.Error())
		}
		ms.SetWAL(wal)
		defer wal.Close()
		log.Info("WAL enabled", "path", *flagWALPath, "last_seq", wal.LastSeq())
	}

	// ADR-031 follow-up — synchronous Raft-style replication: when election
	// is enabled AND WAL is enabled, the leader pushes each new WAL entry
	// to peers and waits for quorum ack. The hook fires AFTER local commit,
	// so failure to reach quorum is logged but not propagated to the client
	// (best-effort replication; followers can still catch up via WAL pull).
	// The actual hook is wired below after elector is constructed.

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

	// ADR-029: optional TLS for edge → DN HTTPS. EDGE_DN_TLS_CA enables.
	dnTLSCfg, dnScheme, terr := buildDNTLSConfig(log)
	if terr != nil {
		fatal("DN TLS: " + terr.Error())
	}

	// ReplicationFactor = 3 by default for 3-way replication MVP.
	nodes := make([]placement.Node, len(dns))
	for i, a := range dns {
		nodes[i] = placement.Node{ID: a, Addr: a}
	}
	coord, err := coordinator.New(coordinator.Config{
		Nodes:             nodes,
		ReplicationFactor: 3,
		QuorumWrite:       *flagQuorum,
		Timeout:           10 * time.Second,
		TLSConfig:         dnTLSCfg,
		DNScheme:          dnScheme,
	})
	if err != nil {
		fatal(err.Error())
	}

	// ADR-028 multi-key signer: load all kids from urlkey_secrets bucket;
	// seed kid="v1" from EDGE_URLKEY_SECRET env if bucket empty.
	var signer *urlkey.Signer
	if !*flagSkip {
		entries, lerr := ms.ListURLKeys()
		if lerr != nil {
			fatal("read urlkey_secrets: " + lerr.Error())
		}
		if len(entries) == 0 {
			if err := ms.PutURLKey(urlkey.DefaultKid, hex.EncodeToString([]byte(*flagSecret)), true); err != nil {
				fatal("seed urlkey: " + err.Error())
			}
			entries, _ = ms.ListURLKeys()
			log.Info("seeded urlkey_secrets with EDGE_URLKEY_SECRET", "kid", urlkey.DefaultKid)
		}
		keys := make(map[string][]byte, len(entries))
		var primary string
		for _, e := range entries {
			b, derr := hex.DecodeString(e.SecretHex)
			if derr != nil {
				fatal("decode urlkey kid=" + e.Kid + ": " + derr.Error())
			}
			keys[e.Kid] = b
			if e.IsPrimary {
				primary = e.Kid
			}
		}
		if primary == "" {
			primary = entries[0].Kid // fallback
		}
		if override := envOr("EDGE_URLKEY_PRIMARY_KID", ""); override != "" {
			primary = override
		}
		signer, err = urlkey.NewMultiSigner(keys, primary)
		if err != nil {
			fatal("build signer: " + err.Error())
		}
		log.Info("urlkey signer ready", "kids", len(entries), "primary", primary)
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

	hbInterval, perr := time.ParseDuration(*flagHB)
	if perr != nil {
		fatal("invalid EDGE_HEARTBEAT_INTERVAL: " + perr.Error())
	}
	var hbMon *heartbeat.Monitor
	if hbInterval > 0 {
		hbMon = heartbeat.New(httpHealthProbe(dnScheme, dnTLSCfg), *flagHBFail, 2*time.Second)
	}

	var snapSched *store.SnapshotScheduler
	if *flagSnapDir != "" {
		snapInt, perr := time.ParseDuration(*flagSnapInt)
		if perr != nil {
			fatal("invalid EDGE_SNAPSHOT_INTERVAL: " + perr.Error())
		}
		snapSched = store.NewSnapshotScheduler(ms, *flagSnapDir, snapInt, *flagSnapKp)
	}

	cdcEnabled := false
	switch *flagChunkMd {
	case "fixed", "":
	case "cdc":
		cdcEnabled = true
	default:
		fatal("EDGE_CHUNK_MODE must be 'fixed' or 'cdc' (got " + *flagChunkMd + ")")
	}

	// ADR-031 election (opt-in via EDGE_PEERS).
	var elector *election.Elector
	if *flagPeers != "" {
		if *flagSelfURL == "" {
			fatal("EDGE_PEERS set but EDGE_SELF_URL missing — election needs to know self's URL")
		}
		peerURLs := splitTrim(*flagPeers)
		peers := make([]election.Peer, 0, len(peerURLs))
		for _, u := range peerURLs {
			peers = append(peers, election.Peer{ID: u, URL: u})
		}
		hbInt, perr := time.ParseDuration(*flagElectHB)
		if perr != nil {
			fatal("invalid EDGE_ELECTION_HB_INTERVAL: " + perr.Error())
		}
		etMin, perr := time.ParseDuration(*flagElectMin)
		if perr != nil {
			fatal("invalid EDGE_ELECTION_TIMEOUT_MIN: " + perr.Error())
		}
		etMax, perr := time.ParseDuration(*flagElectMax)
		if perr != nil {
			fatal("invalid EDGE_ELECTION_TIMEOUT_MAX: " + perr.Error())
		}
		elector = election.New(election.Config{
			SelfID:             *flagSelfURL,
			Peers:              peers,
			HeartbeatInterval:  hbInt,
			ElectionTimeoutMin: etMin,
			ElectionTimeoutMax: etMax,
			Log:                log,
			// Followers apply pushed WAL entries directly into local store
			// (ADR-031 follow-up — synchronous replication path).
			AppendEntryFn: func(body []byte) error {
				var e store.WALEntry
				if err := json.Unmarshal(body, &e); err != nil {
					return err
				}
				return ms.ApplyEntry(e)
			},
		})
		// Leader-side: every local WAL.Append fires this hook → push to
		// peers (best-effort, quorum-ack within Config.ReplicateTimeout).
		ms.SetWALHook(func(entryJSON []byte) {
			if !elector.IsLeader() {
				return // followers don't replicate
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := elector.ReplicateEntry(ctx, entryJSON); err != nil {
				log.Warn("replication failed", "err", err.Error())
			}
		})
		log.Info("election enabled", "self", *flagSelfURL, "peers", len(peers),
			"hb", hbInt, "timeout_min", etMin, "timeout_max", etMax)
	}

	srv := &edge.Server{
		Store:             ms,
		Coord:             coord,
		Signer:            signer,
		Log:               log,
		ChunkSize:         chunkSize,
		AutoCfg:           autoCfg,
		SkipAuth:          *flagSkip,
		Heartbeat:         hbMon,
		SnapshotScheduler: snapSched,
		CDCEnabled:        cdcEnabled,
		Elector:           elector,
	}

	if elector != nil {
		// Election mode: every edge runs the snapshot-pull loop. The sync
		// function self-skips when this edge is currently leader.
		pullDur, perr := time.ParseDuration(*flagPullInt)
		if perr != nil {
			fatal("invalid EDGE_FOLLOWER_PULL_INTERVAL: " + perr.Error())
		}
		srv.SetElectionFollowerSync(*flagDataDir, pullDur)
	} else {
		// Manual mode (ADR-022): operator picks primary/follower via env.
		switch edge.Role(*flagRole) {
		case edge.RolePrimary:
			// default; nothing extra
		case edge.RoleFollower:
			if *flagPrim == "" {
				fatal("EDGE_PRIMARY_URL required when EDGE_ROLE=follower")
			}
			pullDur, perr := time.ParseDuration(*flagPullInt)
			if perr != nil {
				fatal("invalid EDGE_FOLLOWER_PULL_INTERVAL: " + perr.Error())
			}
			srv.SetFollowerConfig(edge.FollowerConfig{
				PrimaryURL:   *flagPrim,
				DataDir:      *flagDataDir,
				PullInterval: pullDur,
			})
			log.Info("kvfs-edge in follower role", "primary", *flagPrim, "pull_interval", pullDur)
		default:
			fatal("EDGE_ROLE must be 'primary' or 'follower' (got " + *flagRole + ")")
		}
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

	// Wire Prometheus-style /metrics endpoint (default on).
	if *flagMetrics {
		srv.SetupMetrics()
	}

	// Start auto-trigger loops (no-op if AutoCfg.Enabled is false). They
	// exit when ctx is cancelled by signal.
	srv.StartAuto(ctx)

	// Start DN heartbeat (ADR-030). No-op if Heartbeat is nil.
	srv.StartHeartbeat(ctx, hbInterval)

	// Start auto-snapshot scheduler (ADR-016). No-op if SnapshotScheduler is nil.
	srv.StartSnapshotScheduler(ctx)

	// Start follower snapshot pull (ADR-022 manual / ADR-031 election).
	// No-op if neither mode configured a followerSt.
	srv.StartFollowerSync(ctx)

	// Start election state machine (ADR-031). No-op if Elector is nil.
	srv.StartElector(ctx)

	errCh := make(chan error, 1)
	tlsCert := envOr("EDGE_TLS_CERT", "")
	tlsKey := envOr("EDGE_TLS_KEY", "")
	if tlsCert != "" && tlsKey != "" {
		log.Info("kvfs-edge HTTPS enabled (ADR-029)", "cert", tlsCert)
		go func() { errCh <- httpSrv.ListenAndServeTLS(tlsCert, tlsKey) }()
	} else {
		go func() { errCh <- httpSrv.ListenAndServe() }()
	}

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

// buildDNTLSConfig assembles edge → DN TLS config from EDGE_DN_TLS_* env
// (ADR-029). Returns (nil, "http", nil) when TLS is not opted in.
//
// CA env enables HTTPS to DNs. Optional client cert env adds mTLS.
// CLIENT_CERT and CLIENT_KEY must be set together — XOR is rejected to avoid
// silent downgrades.
func buildDNTLSConfig(log *slog.Logger) (*tls.Config, string, error) {
	caPath := envOr("EDGE_DN_TLS_CA", "")
	if caPath == "" {
		return nil, "http", nil
	}
	pool, err := tlsutil.LoadCertPool(caPath)
	if err != nil {
		return nil, "", err
	}
	cfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}

	clientCert := envOr("EDGE_DN_TLS_CLIENT_CERT", "")
	clientKey := envOr("EDGE_DN_TLS_CLIENT_KEY", "")
	if (clientCert == "") != (clientKey == "") {
		return nil, "", fmt.Errorf("EDGE_DN_TLS_CLIENT_CERT and EDGE_DN_TLS_CLIENT_KEY must be set together (or neither)")
	}
	if clientCert != "" {
		cert, err := tls.LoadX509KeyPair(clientCert, clientKey)
		if err != nil {
			return nil, "", fmt.Errorf("load client cert: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
		log.Info("edge → DN mTLS enabled", "client_cert", clientCert)
	} else {
		log.Info("edge → DN HTTPS enabled (no mTLS client cert)", "ca", caPath)
	}
	return cfg, "https", nil
}

// httpHealthProbe returns a heartbeat.Probe that GETs <scheme>://<addr>/healthz.
// scheme/tlsCfg come from buildDNTLSConfig (ADR-029) so probes use the same
// transport as data-path coordinator calls.
func httpHealthProbe(scheme string, tlsCfg *tls.Config) heartbeat.Probe {
	transport := &http.Transport{TLSClientConfig: tlsCfg}
	client := &http.Client{Transport: transport}
	return probeFn(func(ctx context.Context, addr string) (time.Duration, error) {
		start := time.Now()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, scheme+"://"+addr+"/healthz", nil)
		if err != nil {
			return 0, err
		}
		resp, err := client.Do(req)
		lat := time.Since(start)
		if err != nil {
			return lat, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return lat, fmt.Errorf("healthz: HTTP %d", resp.StatusCode)
		}
		return lat, nil
	})
}

type probeFn func(ctx context.Context, addr string) (time.Duration, error)

func (f probeFn) Probe(ctx context.Context, addr string) (time.Duration, error) {
	return f(ctx, addr)
}
