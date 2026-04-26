// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Package tlsutil holds shared TLS helpers used by both kvfs-edge and kvfs-dn.
//
// Currently: PEM CA pool loader. Both binaries need it for ADR-029 — edge for
// trusting DN server certs (RootCAs) and DN for verifying edge client certs
// (ClientCAs). The body is identical; only the *tls.Config field differs at
// the call site.
//
// 비전공자용 해설
// ──────────────
// CA pool = "이 인증서들이 서명한 cert 는 신뢰한다" 라는 화이트리스트. PEM 파일
// 한 개에 여러 CA 가 들어있을 수 있어서 AppendCertsFromPEM 으로 한꺼번에 로드.
//
// kvfs 는 자체 demo CA 한 개로 edge·dn·client cert 모두 발급
// (`scripts/gen-tls-certs.sh`). production 에선 진짜 CA (Let's Encrypt /
// 사내 PKI) 발급 cert 를 사용.
package tlsutil

import (
	"crypto/x509"
	"fmt"
	"os"
)

// LoadCertPool reads a PEM-encoded certificate file and returns a x509.CertPool
// containing every certificate in it. Returns an error if the file is unreadable
// or contains no PEM blocks.
func LoadCertPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read CA %s: %w", path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("CA %s: no PEM blocks found", path)
	}
	return pool, nil
}
