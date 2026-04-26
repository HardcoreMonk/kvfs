// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Edge → coord HTTP client (Season 5 Ep.2, ADR-015 follow-up).
//
// 누적 capabilities (이 파일이 다루는 것 전부):
//   Ep.2  CommitObject / LookupObject / DeleteObject / Healthz   — 메타 RPC
//   Ep.3  do() 의 leader-redirect (503 + X-COORD-LEADER, ADR-038)
//   Ep.6  PlaceN                                                  — placement RPC
//   Ep.7  ListURLKeys                                             — kid registry sync source
//   P6-10 cache (cacheTTL/cache + SetLookupCache)                 — opt-in lookup cache
//   P7-08 do() 의 CANDIDATE 503-empty backoff                     — election 중 transparent retry
//
// 비전공자용 해설
// ──────────────
// EDGE_COORD_URL 이 설정되면 edge 가 자기 bbolt 대신 coord HTTP 에 모든
// 메타 / placement 작업을 위임. 결과 (coord-proxy mode):
//
//   - coord.bbolt 가 진실의 단일 출처
//   - edge.bbolt 는 미사용 (Season 7 미정의 정리 후보)
//   - read-after-write: 같은 client 가 commit 직후 lookup 하면 coord 의
//     bbolt 트랜잭션 직렬성 덕에 새 값 보장
//   - failover: coord 다중화 (ADR-038/039) 가 leader 변경 처리
//
// HTTP 클라이언트 정책:
//   - per-call timeout 10s (Timeout field). 5xx 시 instant fail (caller 가 retry 결정)
//   - 503 + X-COORD-LEADER 헤더: transparent 1-hop redirect (baseURL 갱신)
//   - 503 + 빈 헤더 (CANDIDATE 상태): 200ms backoff + retry (election convergence)
//   - 캐시 (P6-10, opt-in): SetLookupCache(ttl) 으로 활성. CommitObject/
//     DeleteObject 가 same-client 의 entry invalidate. 다른 edge 의
//     mutation 은 TTL 만료까지 stale (짧은 TTL 권장)
package edge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/HardcoreMonk/kvfs/internal/coord"
	"github.com/HardcoreMonk/kvfs/internal/store"
)

// CoordClient is a thin HTTP client for the kvfs-coord RPC surface defined
// in internal/coord. Only the methods the edge actually needs in Ep.2 are
// implemented — Place will join when placement also moves to coord (Ep.3+).
//
// HA awareness (Ep.3, ADR-038): when a write hits a follower coord, coord
// answers 503 with header `X-COORD-LEADER: <url>`. CoordClient follows the
// hint exactly once per call — caller sees a single transparent retry.
// MaxLeaderRedirects bounds that loop so a misconfigured cluster can't
// trap us in a cycle. Default 1 (one redirect = one retry max).
//
// baseURL is mutated on each leader-hint follow (so subsequent calls go
// straight to the new leader). Concurrent edge handlers share one
// CoordClient, so the field is unexported and gated by mu — exposing it
// would let one goroutine read it mid-update by another.
type CoordClient struct {
	HTTP               *http.Client  // caller-provided; nil → defaultHTTPClient
	Timeout            time.Duration // per-call deadline (default 10s)
	MaxLeaderRedirects int           // 0 → uses default 1

	mu      sync.RWMutex
	baseURL string

	// P6-10: opt-in per-(bucket,key) read-through cache for LookupObject.
	// Disabled when cacheTTL == 0. CommitObject + DeleteObject invalidate
	// by key on the SAME client; mutations from other edges wait out the
	// TTL (so default short — 1-5s).
	cacheTTL time.Duration
	cache    map[string]cachedMeta
}

// cachedMeta is one entry of CoordClient's optional read-through cache.
type cachedMeta struct {
	meta   *store.ObjectMeta
	expiry time.Time
}

// BaseURL returns the current target URL (the one that subsequent calls
// will hit until a leader-redirect updates it). Read-only diagnostic.
func (c *CoordClient) BaseURL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.baseURL
}

// NewCoordClient builds a client with a sensible default HTTP client.
func NewCoordClient(baseURL string) *CoordClient {
	return &CoordClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: 30 * time.Second},
		Timeout: 10 * time.Second,
	}
}

// SetLookupCache enables/disables the per-(bucket,key) read-through cache
// (P6-10). ttl > 0 enables; 0 disables and frees the map. Recommended:
// 1-5s. Multi-edge mutations from other clients won't invalidate this
// edge's cache, so don't set TTL beyond your tolerance for stale reads.
func (c *CoordClient) SetLookupCache(ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cacheTTL = ttl
	if ttl > 0 {
		c.cache = make(map[string]cachedMeta)
	} else {
		c.cache = nil
	}
}

func (c *CoordClient) cacheKey(bucket, key string) string {
	return bucket + "\x00" + key
}

func (c *CoordClient) cacheGet(bucket, key string) (*store.ObjectMeta, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.cache == nil {
		return nil, false
	}
	if e, ok := c.cache[c.cacheKey(bucket, key)]; ok && time.Now().Before(e.expiry) {
		return e.meta, true
	}
	return nil, false
}

func (c *CoordClient) cachePut(bucket, key string, meta *store.ObjectMeta) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache == nil {
		return
	}
	c.cache[c.cacheKey(bucket, key)] = cachedMeta{meta: meta, expiry: time.Now().Add(c.cacheTTL)}
}

func (c *CoordClient) cacheInvalidate(bucket, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache == nil {
		return
	}
	delete(c.cache, c.cacheKey(bucket, key))
}

// call is the shared marshal-do-decode loop for every coord RPC. req is
// optional (nil for GET / no-body POST). respOut is optional (nil = caller
// only cares about success/failure status, not body). notFoundOK lets
// the caller treat a 404 as a typed sentinel (ErrNotFound) instead of an
// arbitrary "non-2xx" string error — used by lookup/delete which need to
// distinguish "key absent" from "coord broken".
//
// Centralizing here means the leader-redirect, status-check, and
// readErrBody bookkeeping live in one place; per-RPC methods become 3-line
// wrappers that just describe their endpoint contract.
func (c *CoordClient) call(ctx context.Context, method, path, label string, req, respOut any, notFoundOK bool) error {
	var body []byte
	if req != nil {
		b, err := json.Marshal(req)
		if err != nil {
			return fmt.Errorf("coord-client: marshal %s: %w", label, err)
		}
		body = b
	}
	resp, err := c.do(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if notFoundOK && resp.StatusCode == http.StatusNotFound {
		return store.ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("coord-client: %s status %d: %s", label, resp.StatusCode, readErrBody(resp))
	}
	if respOut == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(respOut); err != nil {
		return fmt.Errorf("coord-client: decode %s: %w", label, err)
	}
	return nil
}

// PlaceN asks coord to pick n DNs for the given key (chunk_id or stripe_id).
// Coord runs the same HRW algorithm as edge would but against its own DN
// list — the authoritative one. ADR-041 (Season 5 Ep.6) makes this the
// single point of placement decision so divergence between edge instances
// and topology drift can't produce different placements for the same key.
func (c *CoordClient) PlaceN(ctx context.Context, key string, n int) ([]string, error) {
	var pr coord.PlaceResponse
	if err := c.call(ctx, "POST", "/v1/coord/place", "place",
		coord.PlaceRequest{Key: key, N: n}, &pr, false); err != nil {
		return nil, err
	}
	return pr.Addrs, nil
}

// CommitObject ships an ObjectMeta to coord for persistence. coord owns the
// bbolt write and assigns the version. Returns error on quorum/network/coord
// failure — caller (handlePut) should surface to client as 5xx.
//
// Invalidates the local lookup cache for (bucket, key) on success so a
// subsequent GET on the same edge sees the new version.
func (c *CoordClient) CommitObject(ctx context.Context, meta *store.ObjectMeta) error {
	if err := c.call(ctx, "POST", "/v1/coord/commit", "commit",
		coord.CommitRequest{Meta: meta}, nil, false); err != nil {
		return err
	}
	c.cacheInvalidate(meta.Bucket, meta.Key)
	return nil
}

// LookupObject fetches an object's metadata from coord. Returns
// store.ErrNotFound when coord answers 404, so callers can use errors.Is
// to distinguish missing keys from network errors.
//
// Read-through cache (P6-10, opt-in via SetLookupCache): hit short-
// circuits the RPC; miss falls through and caches the result.
//
// **Caller contract**: the returned *ObjectMeta is shared with the cache
// — do NOT mutate it. Read-only usage (chunk replicas iteration, size
// inspection) is fine. If you need to modify, deep-copy first. All
// current edge callers are read-only.
func (c *CoordClient) LookupObject(ctx context.Context, bucket, key string) (*store.ObjectMeta, error) {
	if cached, ok := c.cacheGet(bucket, key); ok {
		return cached, nil
	}
	path := fmt.Sprintf("/v1/coord/lookup?bucket=%s&key=%s", urlEsc(bucket), urlEsc(key))
	var meta store.ObjectMeta
	if err := c.call(ctx, "GET", path, "lookup", nil, &meta, true); err != nil {
		return nil, err
	}
	c.cachePut(bucket, key, &meta)
	return &meta, nil
}

// DeleteObject removes the meta record on coord. Same ErrNotFound contract.
// Invalidates the local lookup cache.
func (c *CoordClient) DeleteObject(ctx context.Context, bucket, key string) error {
	if err := c.call(ctx, "POST", "/v1/coord/delete", "delete",
		coord.DeleteRequest{Bucket: bucket, Key: key}, nil, true); err != nil {
		return err
	}
	c.cacheInvalidate(bucket, key)
	return nil
}

// Healthz returns nil when coord answers /healthz with 200. Used at edge
// startup so a misconfigured EDGE_COORD_URL surfaces as a fatal boot error
// rather than as 503 on every PUT later.
func (c *CoordClient) Healthz(ctx context.Context) error {
	return c.call(ctx, "GET", "/v1/coord/healthz", "healthz", nil, nil, false)
}

// ListURLKeys fetches the kid registry from coord (Season 6 Ep.7,
// ADR-049). Used by the edge poll loop to keep its in-memory
// urlkey.Signer in sync with mutations applied via cli rotate --coord.
func (c *CoordClient) ListURLKeys(ctx context.Context) ([]store.URLKeyEntry, error) {
	var keys []store.URLKeyEntry
	if err := c.call(ctx, "GET", "/v1/coord/admin/urlkey", "list-urlkey",
		nil, &keys, false); err != nil {
		return nil, err
	}
	return keys, nil
}

func (c *CoordClient) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	maxRedirects := c.MaxLeaderRedirects
	if maxRedirects <= 0 {
		maxRedirects = 1
	}
	c.mu.RLock()
	target := c.baseURL
	c.mu.RUnlock()
	for attempt := 0; attempt <= maxRedirects; attempt++ {
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, target+path, rdr)
		if err != nil {
			return nil, fmt.Errorf("coord-client: build req: %w", err)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, fmt.Errorf("coord-client: %s %s: %w", method, path, err)
		}
		// HA leader-redirect protocol (ADR-038): 503 + X-COORD-LEADER means
		// "I'm not the leader; here's who is." Update baseURL so subsequent
		// calls go straight to the new leader (cheap fast-path) and retry.
		//
		// P7-08: 503 with NO header means election in progress (CANDIDATE
		// state, no leader yet). Bounded sleep + retry once so cli /
		// edge handlers don't error out during the brief window. The
		// outer loop's maxRedirects bound caps total retries; this just
		// inserts a backoff before the next attempt.
		if resp.StatusCode == http.StatusServiceUnavailable {
			leader := resp.Header.Get(coord.HeaderCoordLeader)
			_ = resp.Body.Close()
			if leader != "" && leader != target {
				c.mu.Lock()
				c.baseURL = leader
				c.mu.Unlock()
				target = leader
				continue
			}
			if leader == "" && attempt < maxRedirects {
				// CANDIDATE state — typical election ~1-3s, sleep 200ms then retry.
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(200 * time.Millisecond):
				}
				continue
			}
		}
		return resp, nil
	}
	return nil, fmt.Errorf("coord-client: leader redirect loop exceeded (%d hops)", maxRedirects)
}

// readErrBody reads (small) error body from coord without leaking the
// reader. Bounded read to keep noisy 500s from filling logs.
func readErrBody(resp *http.Response) string {
	const max = 1024
	b, _ := io.ReadAll(io.LimitReader(resp.Body, max))
	return strings.TrimSpace(string(b))
}

// urlEsc encodes bucket/key for use as a query-string value. Standard
// QueryEscape — bucket and key are user-controlled and may contain `+`,
// `%`, `#`, multibyte UTF-8 etc. that the prior 3-char allowlist
// silently mangled.
func urlEsc(s string) string { return url.QueryEscape(s) }

// Sentinel for callers that want to detect "edge has no coord configured".
var ErrNoCoordClient = errors.New("edge: EDGE_COORD_URL not set")
