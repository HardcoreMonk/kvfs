// Command kvfs-dn is a data node: HTTP endpoint for chunk storage on local disk.
//
// Config (env vars, with flag fallback):
//
//	DN_ID       string   required  — DN identifier, e.g. "dn1"
//	DN_ADDR     string   ":8080"   — HTTP bind address
//	DN_DATA_DIR string   required  — chunks directory root
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/HardcoreMonk/kvfs/internal/dn"
)

func main() {
	var (
		flagID      = flag.String("id", envOr("DN_ID", ""), "DN identifier (e.g. dn1)")
		flagAddr    = flag.String("addr", envOr("DN_ADDR", ":8080"), "HTTP bind address")
		flagDataDir = flag.String("data-dir", envOr("DN_DATA_DIR", ""), "chunks directory root")
	)
	flag.Parse()

	if *flagID == "" {
		fatal("DN_ID / -id required")
	}
	if *flagDataDir == "" {
		fatal("DN_DATA_DIR / -data-dir required")
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	log.Info("kvfs-dn starting", "id", *flagID, "addr", *flagAddr, "data_dir", *flagDataDir)

	srv, err := dn.NewServer(*flagID, *flagDataDir)
	if err != nil {
		fatal(err.Error())
	}
	httpSrv := &http.Server{
		Addr:              *flagAddr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// graceful shutdown
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
	log.Info("kvfs-dn stopped")
}

func envOr(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "kvfs-dn: "+msg)
	os.Exit(2)
}
