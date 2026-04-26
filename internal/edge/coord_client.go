// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Edge → coord HTTP client (Season 5 Ep.2, ADR-015 follow-up).
//
// Wires the edge into kvfs-coord's RPC surface so metadata reads and writes
// proxy through coord instead of hitting edge's local bbolt. Created when
// EDGE_COORD_URL is set; nil otherwise (preserves Season 1~4 inline mode).
//
// 비전공자용 해설
// ──────────────
// Ep.1 (demo-aleph) 에서 coord 를 standalone 으로 띄웠지만 edge 는 자기 bbolt
// 를 그대로 썼다. 이번 ep 에서 edge 가 EDGE_COORD_URL 을 보면 모든 메타 작업
// (commit / delete / lookup) 을 coord 에 위임한다. 결과:
//
//   - coord.bbolt 가 진실의 단일 출처
//   - edge.bbolt 는 EDGE_COORD_URL 설정 시 unused (혹은 미생성)
//   - placement 결정은 edge 에 잠시 머물러 있음 (edge 가 자기 DN list 알기에).
//     Ep.3 또는 Ep.4 에서 coord 가 placement 도 가져갈 예정.
//
// 동기화·일관성:
//   - 모든 commit 이 coord 의 single bbolt writer 통과 → 멀티-edge 라도
//     메타 일관성 자연 유지 (ADR-022 의 snapshot pull 우회 불필요)
//   - read-after-write: edge 가 자기 commit RPC 직후 lookup RPC → 같은
//     coord 가 응답. coord 의 bbolt 트랜잭션이 직렬이므로 보장됨
//   - failover: coord 다중화는 Ep.3 (P6-03) 의 책임
package edge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/HardcoreMonk/kvfs/internal/coord"
	"github.com/HardcoreMonk/kvfs/internal/store"
)

// CoordClient is a thin HTTP client for the kvfs-coord RPC surface defined
// in internal/coord. Only the methods the edge actually needs in Ep.2 are
// implemented — Place will join when placement also moves to coord (Ep.3+).
type CoordClient struct {
	BaseURL string        // e.g. "http://coord:9000"
	HTTP    *http.Client  // caller-provided; nil → defaultHTTPClient
	Timeout time.Duration // per-call deadline (default 10s)
}

// NewCoordClient builds a client with a sensible default HTTP client.
func NewCoordClient(baseURL string) *CoordClient {
	return &CoordClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: 30 * time.Second},
		Timeout: 10 * time.Second,
	}
}

// CommitObject ships an ObjectMeta to coord for persistence. coord owns the
// bbolt write and assigns the version. Returns error on quorum/network/coord
// failure — caller (handlePut) should surface to client as 5xx.
func (c *CoordClient) CommitObject(ctx context.Context, meta *store.ObjectMeta) error {
	body, err := json.Marshal(coord.CommitRequest{Meta: meta})
	if err != nil {
		return fmt.Errorf("coord-client: marshal commit: %w", err)
	}
	resp, err := c.do(ctx, "POST", "/v1/coord/commit", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("coord-client: commit status %d: %s", resp.StatusCode, readErrBody(resp))
	}
	return nil
}

// LookupObject fetches an object's metadata from coord. Returns
// store.ErrNotFound when coord answers 404, so callers can use errors.Is
// to distinguish missing keys from network errors.
func (c *CoordClient) LookupObject(ctx context.Context, bucket, key string) (*store.ObjectMeta, error) {
	path := fmt.Sprintf("/v1/coord/lookup?bucket=%s&key=%s", urlEsc(bucket), urlEsc(key))
	resp, err := c.do(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, store.ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("coord-client: lookup status %d: %s", resp.StatusCode, readErrBody(resp))
	}
	var meta store.ObjectMeta
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return nil, fmt.Errorf("coord-client: decode lookup: %w", err)
	}
	return &meta, nil
}

// DeleteObject removes the meta record on coord. Same ErrNotFound contract.
func (c *CoordClient) DeleteObject(ctx context.Context, bucket, key string) error {
	body, _ := json.Marshal(coord.DeleteRequest{Bucket: bucket, Key: key})
	resp, err := c.do(ctx, "POST", "/v1/coord/delete", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return store.ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("coord-client: delete status %d: %s", resp.StatusCode, readErrBody(resp))
	}
	return nil
}

// Healthz returns nil when coord answers /healthz with 200. Used at edge
// startup so a misconfigured EDGE_COORD_URL surfaces as a fatal boot error
// rather than as 503 on every PUT later.
func (c *CoordClient) Healthz(ctx context.Context) error {
	resp, err := c.do(ctx, "GET", "/v1/coord/healthz", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("coord-client: healthz status %d", resp.StatusCode)
	}
	return nil
}

func (c *CoordClient) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
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
	return resp, nil
}

// readErrBody reads (small) error body from coord without leaking the
// reader. Bounded read to keep noisy 500s from filling logs.
func readErrBody(resp *http.Response) string {
	const max = 1024
	b, _ := io.ReadAll(io.LimitReader(resp.Body, max))
	return strings.TrimSpace(string(b))
}

// urlEsc keeps the bucket/key path-safe for the lookup query string. We
// only worry about /, ?, & — full URL escaping isn't worth importing
// net/url for one site.
var urlReplacer = strings.NewReplacer("&", "%26", "?", "%3F", " ", "%20")

func urlEsc(s string) string { return urlReplacer.Replace(s) }

// Sentinel for callers that want to detect "edge has no coord configured".
var ErrNoCoordClient = errors.New("edge: EDGE_COORD_URL not set")
