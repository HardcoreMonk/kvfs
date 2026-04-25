// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Package urlkey implements HMAC-SHA256 presigned URL signing and verification.
//
// Design:
//   - Signature input:  "<METHOD>:<PATH>:<EXP_UNIX>"
//   - Signature output: hex(HMAC-SHA256(secret, input))
//   - URL form:         <path>?sig=<hex>&exp=<unix-seconds>
//
// Why HMAC-SHA256 over Base62+APR1-MD5 (original system's legacy URL signature):
//   - Same security guarantee, simpler primitives
//   - Standard library only (crypto/hmac + crypto/sha256)
//   - No LuaJIT int64 FFI bug class (original system had this)
package urlkey

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// Errors returned by Verify.
var (
	ErrMissingSig  = errors.New("urlkey: missing sig query parameter")
	ErrMissingExp  = errors.New("urlkey: missing exp query parameter")
	ErrBadExp      = errors.New("urlkey: exp is not a valid unix timestamp")
	ErrExpired     = errors.New("urlkey: signature expired")
	ErrBadSig      = errors.New("urlkey: signature does not match")
	ErrEmptySecret = errors.New("urlkey: secret must not be empty")
)

// Signer signs and verifies URL signatures using a shared HMAC secret.
type Signer struct {
	secret []byte
}

// NewSigner constructs a Signer. The secret must be non-empty.
// In production, use at least 32 random bytes.
func NewSigner(secret []byte) (*Signer, error) {
	if len(secret) == 0 {
		return nil, ErrEmptySecret
	}
	s := make([]byte, len(secret))
	copy(s, secret)
	return &Signer{secret: s}, nil
}

// Sign produces a hex-encoded HMAC-SHA256 signature over (method, path, exp).
// The caller is responsible for appending `?sig=<Sign(...)>&exp=<expUnix>` to the URL.
func (s *Signer) Sign(method, path string, expUnix int64) string {
	mac := hmac.New(sha256.New, s.secret)
	fmt.Fprintf(mac, "%s:%s:%d", method, path, expUnix)
	return hex.EncodeToString(mac.Sum(nil))
}

// BuildURL is a convenience: returns `<path>?sig=<sig>&exp=<exp>`.
func (s *Signer) BuildURL(method, path string, ttl time.Duration) string {
	exp := time.Now().Add(ttl).Unix()
	sig := s.Sign(method, path, exp)
	return fmt.Sprintf("%s?sig=%s&exp=%d", path, sig, exp)
}

// Verify checks that the request signature in `q` matches (method, path, exp)
// and that exp is in the future. now should normally be time.Now().
func (s *Signer) Verify(method, path string, q url.Values, now time.Time) error {
	sig := q.Get("sig")
	if sig == "" {
		return ErrMissingSig
	}
	expStr := q.Get("exp")
	if expStr == "" {
		return ErrMissingExp
	}
	expUnix, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return ErrBadExp
	}
	if now.Unix() > expUnix {
		return ErrExpired
	}
	expected := s.Sign(method, path, expUnix)
	// constant-time comparison via hmac.Equal
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return ErrBadSig
	}
	return nil
}
