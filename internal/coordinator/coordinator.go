// Package coordinator implements chunk placement, fanout writes, and quorum reads.
//
// This is the "inline library" in kvfs-edge. Carved as a dedicated package so
// it can be split into a separate daemon in Season 2 without touching handler code.
//
// Core contract:
//   - WriteChunk fans out PUT /chunk/<id> to all configured DNs in parallel,
//     returning success when at least QuorumWrite successes have come back.
//   - ReadChunk tries replicas in order, returning the first success.
package coordinator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// Coordinator routes chunks to DNs and drives fanout/quorum.
type Coordinator struct {
	dns         []string // DN addresses, e.g. ["dn1:8080", "dn2:8080", "dn3:8080"]
	quorumWrite int      // min successful replicas for a successful write (e.g. 2 of 3)
	client      *http.Client
}

// Config bundles Coordinator construction parameters.
type Config struct {
	DNs         []string      // required, >=1
	QuorumWrite int           // default: ceil(len(DNs)/2+1); MVP we use 2 for 3 DNs
	Timeout     time.Duration // per-request
}

// New builds a Coordinator. Defaults: quorum = 2 if DNs=3, else ceil((N/2)+1); timeout 10s.
func New(cfg Config) (*Coordinator, error) {
	if len(cfg.DNs) == 0 {
		return nil, errors.New("coordinator: at least one DN required")
	}
	q := cfg.QuorumWrite
	if q <= 0 {
		q = len(cfg.DNs)/2 + 1
	}
	if q > len(cfg.DNs) {
		return nil, fmt.Errorf("coordinator: quorum(%d) > DN count(%d)", q, len(cfg.DNs))
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Coordinator{
		dns:         append([]string(nil), cfg.DNs...),
		quorumWrite: q,
		client:      &http.Client{Timeout: timeout},
	}, nil
}

// DNs returns the configured DN addresses (read-only view).
func (c *Coordinator) DNs() []string { return append([]string(nil), c.dns...) }

// QuorumWrite returns the write quorum (min acks for success).
func (c *Coordinator) QuorumWrite() int { return c.quorumWrite }

// WriteChunk fans out a PUT to all DNs in parallel. It returns the list of DN
// addresses that acked successfully. Returns error if fewer than QuorumWrite acked.
func (c *Coordinator) WriteChunk(ctx context.Context, chunkID string, data []byte) ([]string, error) {
	type result struct {
		dn  string
		err error
	}
	results := make(chan result, len(c.dns))
	var wg sync.WaitGroup
	for _, dn := range c.dns {
		wg.Add(1)
		go func(dn string) {
			defer wg.Done()
			err := c.putChunk(ctx, dn, chunkID, data)
			results <- result{dn: dn, err: err}
		}(dn)
	}
	go func() { wg.Wait(); close(results) }()

	var ok []string
	var errs []error
	for r := range results {
		if r.err == nil {
			ok = append(ok, r.dn)
		} else {
			errs = append(errs, fmt.Errorf("%s: %w", r.dn, r.err))
		}
	}
	if len(ok) < c.quorumWrite {
		return ok, fmt.Errorf("quorum not reached: %d/%d success; errors: %v",
			len(ok), c.quorumWrite, errs)
	}
	return ok, nil
}

// ReadChunk tries replicas in order, returning the first successful body.
func (c *Coordinator) ReadChunk(ctx context.Context, chunkID string, candidates []string) ([]byte, string, error) {
	var lastErr error
	for _, dn := range candidates {
		body, err := c.getChunk(ctx, dn, chunkID)
		if err == nil {
			return body, dn, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no candidates")
	}
	return nil, "", fmt.Errorf("all replicas failed: %w", lastErr)
}

// DeleteChunk fires a DELETE to each replica; errors are returned jointly but
// best-effort (does not require quorum).
func (c *Coordinator) DeleteChunk(ctx context.Context, chunkID string, replicas []string) error {
	var errs []error
	for _, dn := range replicas {
		if err := c.deleteChunk(ctx, dn, chunkID); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", dn, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("partial delete failure: %v", errs)
	}
	return nil
}

// ---- low-level HTTP helpers ----

func (c *Coordinator) putChunk(ctx context.Context, dn, chunkID string, data []byte) error {
	url := fmt.Sprintf("http://%s/chunk/%s", dn, chunkID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("PUT returned %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

func (c *Coordinator) getChunk(ctx context.Context, dn, chunkID string) ([]byte, error) {
	url := fmt.Sprintf("http://%s/chunk/%s", dn, chunkID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET returned %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func (c *Coordinator) deleteChunk(ctx context.Context, dn, chunkID string) error {
	url := fmt.Sprintf("http://%s/chunk/%s", dn, chunkID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("DELETE returned %d", resp.StatusCode)
	}
	return nil
}
