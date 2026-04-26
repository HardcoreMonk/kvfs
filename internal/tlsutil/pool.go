// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Package tlsutil holds shared TLS helpers used by both kvfs-edge and kvfs-dn.
//
// Currently: PEM CA pool loader. Both binaries need it for ADR-029 — edge for
// trusting DN server certs (RootCAs) and DN for verifying edge client certs
// (ClientCAs). The body is identical; only the *tls.Config field differs at
// the call site.
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
