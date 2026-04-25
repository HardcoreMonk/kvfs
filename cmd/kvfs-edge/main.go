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

	dns := splitTrim(*flagDNs)
	// ReplicationFactor = 3 by default for 3-way replication MVP.
	// Placement is now Rendezvous-hashed (see internal/placement). When
	// N > ReplicationFactor, each chunk is placed on the R highest-score nodes.
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

	srv := &edge.Server{
		Store:     ms,
		Coord:     coord,
		Signer:    signer,
		ChunkSize: chunkSize,
		SkipAuth:  *flagSkip,
	}

	log.Info("kvfs-edge starting",
		"addr", *flagAddr,
		"dns", dns,
		"quorum_write", coord.QuorumWrite(),
		"chunk_size", chunkSize,
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
