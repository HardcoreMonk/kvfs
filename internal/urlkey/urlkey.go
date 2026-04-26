// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Package urlkey implements HMAC-SHA256 presigned URL signing and verification.
//
// Design:
//   - Signature input:  "<METHOD>:<PATH>:<EXP_UNIX>"
//   - Signature output: hex(HMAC-SHA256(secret, input))
//   - URL form:         <path>?sig=<hex>&exp=<unix-seconds>
//
// Why HMAC-SHA256 over Base62+APR1-MD5:
//   - Same security guarantee, simpler primitives
//   - Standard library only (crypto/hmac + crypto/sha256)
//   - No LuaJIT int64 FFI bug class
package urlkey

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Errors returned by Verify and key-management methods.
var (
	ErrMissingSig    = errors.New("urlkey: missing sig query parameter")
	ErrMissingExp    = errors.New("urlkey: missing exp query parameter")
	ErrBadExp        = errors.New("urlkey: exp is not a valid unix timestamp")
	ErrExpired       = errors.New("urlkey: signature expired")
	ErrBadSig        = errors.New("urlkey: signature does not match")
	ErrEmptySecret   = errors.New("urlkey: secret must not be empty")
	ErrUnknownKid    = errors.New("urlkey: unknown kid")
	ErrDuplicateKid  = errors.New("urlkey: kid already exists")
	ErrPrimaryRemove = errors.New("urlkey: cannot remove primary kid")
	ErrLastKidRemove = errors.New("urlkey: cannot remove the only kid")
)

// DefaultKid is used when the legacy single-secret constructor is invoked
// or when a URL omits the ?kid= parameter (backward-compat path).
const DefaultKid = "v1"

// Signer signs and verifies URL signatures using one or more HMAC secrets
// indexed by kid (key id). New URLs are always signed with the primary kid.
// Verify accepts any registered kid; URL-omitted kid falls back to primary.
type Signer struct {
	mu      sync.RWMutex
	keys    map[string][]byte
	primary string
}

// NewSigner constructs a single-key Signer using DefaultKid as primary.
// Backward-compatible with the original ADR-007 single-secret API.
// In production, use at least 32 random bytes.
func NewSigner(secret []byte) (*Signer, error) {
	if len(secret) == 0 {
		return nil, ErrEmptySecret
	}
	s := &Signer{keys: map[string][]byte{}}
	if err := s.Add(DefaultKid, secret); err != nil {
		return nil, err
	}
	s.primary = DefaultKid
	return s, nil
}

// NewMultiSigner constructs a multi-key Signer.
// keys must contain at least one entry; primary must be one of them.
func NewMultiSigner(keys map[string][]byte, primary string) (*Signer, error) {
	if len(keys) == 0 {
		return nil, ErrEmptySecret
	}
	if _, ok := keys[primary]; !ok {
		return nil, ErrUnknownKid
	}
	s := &Signer{keys: make(map[string][]byte, len(keys))}
	for kid, secret := range keys {
		if len(secret) == 0 {
			return nil, ErrEmptySecret
		}
		buf := make([]byte, len(secret))
		copy(buf, secret)
		s.keys[kid] = buf
	}
	s.primary = primary
	return s, nil
}

// Primary returns the kid currently used for new Sign / BuildURL calls.
func (s *Signer) Primary() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.primary
}

// Kids returns sorted list of registered kids.
func (s *Signer) Kids() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.keys))
	for kid := range s.keys {
		out = append(out, kid)
	}
	sort.Strings(out)
	return out
}

// Add registers a new (kid, secret) pair. Returns ErrDuplicateKid if kid
// already exists. Does NOT change primary.
func (s *Signer) Add(kid string, secret []byte) error {
	if kid == "" {
		return errors.New("urlkey: empty kid")
	}
	if len(secret) == 0 {
		return ErrEmptySecret
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.keys[kid]; exists {
		return ErrDuplicateKid
	}
	buf := make([]byte, len(secret))
	copy(buf, secret)
	s.keys[kid] = buf
	return nil
}

// SetPrimary marks kid as the new primary for subsequent Sign / BuildURL.
// Returns ErrUnknownKid if kid was not previously Add'd.
func (s *Signer) SetPrimary(kid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.keys[kid]; !ok {
		return ErrUnknownKid
	}
	s.primary = kid
	return nil
}

// Remove deletes a kid. Refuses to remove the primary or the last kid.
func (s *Signer) Remove(kid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.keys[kid]; !ok {
		return ErrUnknownKid
	}
	if kid == s.primary {
		return ErrPrimaryRemove
	}
	if len(s.keys) == 1 {
		return ErrLastKidRemove
	}
	delete(s.keys, kid)
	return nil
}

// Sign produces a hex-encoded HMAC-SHA256 signature using the primary kid.
// Returned kid is the one used (callers who need to embed it explicitly may).
// Most callers use BuildURL which embeds kid into the query.
func (s *Signer) Sign(method, path string, expUnix int64) (sig, kid string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	secret := s.keys[s.primary]
	mac := hmac.New(sha256.New, secret)
	fmt.Fprintf(mac, "%s:%s:%d", method, path, expUnix)
	return hex.EncodeToString(mac.Sum(nil)), s.primary
}

// BuildURL is a convenience: returns `<path>?kid=<primary>&sig=<sig>&exp=<exp>`.
func (s *Signer) BuildURL(method, path string, ttl time.Duration) string {
	exp := time.Now().Add(ttl).Unix()
	sig, kid := s.Sign(method, path, exp)
	return fmt.Sprintf("%s?kid=%s&sig=%s&exp=%d", path, kid, sig, exp)
}

// Verify checks that the request signature in `q` matches (method, path, exp).
//
// kid resolution:
//   - If `?kid=<id>` is present: verify with that specific secret only.
//     Unknown kid → ErrUnknownKid (was retired or never existed).
//   - If kid is absent (legacy URL signed before ADR-028, or sign_url helper
//     that doesn't include kid): try EVERY registered kid in turn. A match
//     any one of them means the URL was signed at a time when that kid was
//     primary. Once a kid is Remove'd, URLs that depended on it are
//     genuinely rejected — that's the rotation guarantee.
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

	s.mu.RLock()
	defer s.mu.RUnlock()

	if kid := q.Get("kid"); kid != "" {
		secret, ok := s.keys[kid]
		if !ok {
			return ErrUnknownKid
		}
		return verifyOne(secret, method, path, expUnix, sig)
	}
	// No kid: try all registered kids.
	for _, secret := range s.keys {
		if err := verifyOne(secret, method, path, expUnix, sig); err == nil {
			return nil
		}
	}
	return ErrBadSig
}

func verifyOne(secret []byte, method, path string, expUnix int64, sig string) error {
	mac := hmac.New(sha256.New, secret)
	fmt.Fprintf(mac, "%s:%s:%d", method, path, expUnix)
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return ErrBadSig
	}
	return nil
}
