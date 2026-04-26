// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Command kvfs-dn is a data node: HTTP endpoint for chunk storage on local disk.
//
// Config (env vars, with flag fallback):
//
//	DN_ID              string   required  — DN identifier, e.g. "dn1"
//	DN_ADDR            string   ":8080"   — HTTP bind address
//	DN_DATA_DIR        string   required  — chunks directory root
//	DN_TLS_CERT/KEY    string   optional  — TLS server (ADR-029)
//	DN_TLS_CLIENT_CA   string   optional  — mTLS: verify edge client cert
//
// 비전공자용 해설
// ──────────────
// kvfs-dn = 디스크 한 대 = HTTP KV 서버. main 함수의 일은 단순:
//
//  1. env 읽고 (필수: DN_ID, DN_DATA_DIR)
//  2. internal/dn.NewServer 로 storage 핸들러 생성
//  3. (선택) TLS / mTLS cert 로드
//  4. http.Server 띄우고 SIGTERM 까지 ListenAndServe
//
// edge 와 달리 외부 의존 0 (다른 DN 도, 메타 DB 도 모름). 멈춰도 chunk 파일만
// 살아있으면 다른 replica 에서 GET 으로 복원 가능.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/HardcoreMonk/kvfs/internal/dn"
	"github.com/HardcoreMonk/kvfs/internal/tlsutil"
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
	tlsCfg, terr := buildServerTLS(log)
	if terr != nil {
		fatal("TLS: " + terr.Error())
	}
	httpSrv := &http.Server{
		Addr:              *flagAddr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         tlsCfg,
	}

	// graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tlsCert := envOr("DN_TLS_CERT", "")
	tlsKey := envOr("DN_TLS_KEY", "")
	errCh := make(chan error, 1)
	if tlsCert != "" && tlsKey != "" {
		log.Info("kvfs-dn HTTPS enabled (ADR-029)", "cert", tlsCert)
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

// buildServerTLS returns a TLS config when DN_TLS_CLIENT_CA is set (mTLS).
// Pure server TLS (no mTLS) is configured via httpSrv.ListenAndServeTLS path,
// not here — this only adds client-cert verification.
func buildServerTLS(log *slog.Logger) (*tls.Config, error) {
	clientCAPath := envOr("DN_TLS_CLIENT_CA", "")
	if clientCAPath == "" {
		return nil, nil
	}
	pool, err := tlsutil.LoadCertPool(clientCAPath)
	if err != nil {
		return nil, err
	}
	log.Info("kvfs-dn mTLS enabled (verifying edge client cert)", "client_ca", clientCAPath)
	return &tls.Config{
		ClientCAs:  pool,
		ClientAuth: tls.RequireAndVerifyClientCert,
		MinVersion: tls.VersionTLS12,
	}, nil
}
