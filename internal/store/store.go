// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Package store wraps bbolt for kvfs-edge metadata.
//
// Buckets:
//   - "objects"   : "<bucket>/<key>" -> JSON(ObjectMeta)
//   - "dns"       : "<dn_id>"        -> JSON(DNInfo)       // registry (future)
//
// Design notes:
//   - bbolt is pure-Go, single-writer, serializable transactions.
//   - Single bbolt file per edge instance (single-writer constraint).
//   - For HA/sharded edges: Season 2 topic (Raft-replicated state).
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"go.etcd.io/bbolt"
)

var (
	bucketObjects       = []byte("objects")
	bucketDNs           = []byte("dns")
	bucketDNsRuntime    = []byte("dns_runtime")    // ADR-027
	bucketURLKeySecrets = []byte("urlkey_secrets") // ADR-028: kid -> JSON {secret_hex, created_at, is_primary}

	ErrNotFound = errors.New("store: not found")
)

// ObjectMeta is the persisted record for a stored object.
//
// Schema (ADR-011, supersedes ADR-006):
//   - Chunks: ordered list of chunk references. Joining the chunk bytes in
//     order reconstructs the object.
//   - Size: total object size = sum(Chunks[*].Size). Stored explicitly for
//     fast metadata browsing.
//
// Backward compatibility (legacy ADR-006 reads):
//   - Old records had top-level ChunkID + Replicas (single-chunk schema).
//     LegacyChunkID / LegacyReplicas exist only to deserialize those.
//     Adapter in GetObject / ListObjects normalizes legacy → Chunks shape
//     so callers always see the new schema.
type ObjectMeta struct {
	Bucket      string     `json:"bucket"`
	Key         string     `json:"key"`
	Size        int64      `json:"size"`
	ContentType string     `json:"content_type,omitempty"`

	// Replication mode (mutually exclusive with EC).
	// Set when the object was stored as N replicated chunks.
	Chunks []ChunkRef `json:"chunks,omitempty"`

	// Erasure-coded mode (ADR-008, mutually exclusive with Chunks).
	// EC nil → replication mode.
	EC      *ECParams `json:"ec,omitempty"`
	Stripes []Stripe  `json:"stripes,omitempty"`

	Version   int64     `json:"version"`
	CreatedAt time.Time `json:"created_at"`

	// Legacy single-chunk fields. Read-only; never written by current code.
	// Present so that old bbolt records still decode without error.
	LegacyChunkID  string   `json:"chunk_id,omitempty"`
	LegacyReplicas []string `json:"replicas,omitempty"`
}

// IsEC reports whether this object is stored in erasure-coded mode.
func (o *ObjectMeta) IsEC() bool { return o.EC != nil && len(o.Stripes) > 0 }

// ChunkRef points to one chunk that, together with its peers (in
// ObjectMeta.Chunks for replication mode, or as a Shard inside a Stripe for
// EC mode), reconstructs an object.
//
// In replication mode: Replicas is the list of DN addresses holding copies.
// In EC mode: Replicas has length 1 (a single addr per shard).
type ChunkRef struct {
	ChunkID  string   `json:"chunk_id"` // hex(sha256(chunk_data or shard_data))
	Size     int64    `json:"size"`
	Replicas []string `json:"replicas"`
}

// ECParams describes the erasure-coding configuration for a stored object.
type ECParams struct {
	K         int   `json:"k"`          // data shards per stripe
	M         int   `json:"m"`          // parity shards per stripe
	ShardSize int   `json:"shard_size"` // bytes per shard (uniform within an object)
	DataSize  int64 `json:"data_size"`  // total bytes of meaningful data (last stripe padded)
}

// Stripe is one (K data + M parity) group inside an EC-stored object.
//
// Shards length = K + M. Shards[0..K) are data, Shards[K..K+M) are parity.
// StripeID is sha256 of the K data shards concatenated; used as the placement
// key (Pick(StripeID, K+M) → K+M distinct DNs).
type Stripe struct {
	StripeID string     `json:"stripe_id"`
	Shards   []ChunkRef `json:"shards"`
}

// DNInfo records a known DataNode (registry, heartbeats).
// Populated lazily when edge first talks to a DN.
type DNInfo struct {
	ID     string    `json:"id"`
	Addr   string    `json:"addr"`
	LastHB time.Time `json:"last_hb"`
	Status string    `json:"status"` // "healthy" | "unhealthy"
}

// MetaStore is a bbolt-backed metadata store. The underlying *bbolt.DB is held
// in an atomic.Pointer so it can be hot-swapped by Reload (ADR-022 follower
// sync) without invalidating concurrent readers — readers load the current
// pointer per call, and the swapped-out DB is closed only after pending tx
// finish (bbolt.Close blocks; we run it in a goroutine).
//
// reloadMu serializes Reload so back-to-back follower syncs cannot race on
// the open + swap + async-close sequence.
type MetaStore struct {
	db       atomic.Pointer[bbolt.DB]
	reloadMu sync.Mutex
}

// Open opens or creates a bbolt database file at path.
func Open(path string) (*MetaStore, error) {
	db, err := openInitialized(path)
	if err != nil {
		return nil, err
	}
	m := &MetaStore{}
	m.db.Store(db)
	return m, nil
}

// openInitialized opens a bbolt file and ensures the standard buckets exist.
func openInitialized(path string) (*bbolt.DB, error) {
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open bbolt: %w", err)
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		for _, b := range [][]byte{bucketObjects, bucketDNs, bucketDNsRuntime, bucketURLKeySecrets} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// Reload swaps the internal *bbolt.DB to the file at path (ADR-022). Used by
// follower edges to consume a freshly-pulled snapshot from primary. The old
// DB is closed asynchronously so in-flight read txns are not aborted.
//
// path must point to a valid bbolt file — typically a snapshot dropped into
// the data dir via atomic temp+rename. If Reload fails, the previous DB
// remains active.
func (m *MetaStore) Reload(path string) error {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	newDB, err := openInitialized(path)
	if err != nil {
		return fmt.Errorf("reload: %w", err)
	}
	old := m.db.Swap(newDB)
	if old != nil {
		go func() { _ = old.Close() }()
	}
	return nil
}

// Close flushes and closes the bbolt file.
func (m *MetaStore) Close() error { return m.db.Load().Close() }

// PutObject stores or overwrites an object's metadata.
// Version is auto-incremented if an existing record is found.
func (m *MetaStore) PutObject(obj *ObjectMeta) error {
	k := objKey(obj.Bucket, obj.Key)
	return m.db.Load().Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketObjects)
		if prev := b.Get(k); prev != nil {
			var prevObj ObjectMeta
			if err := json.Unmarshal(prev, &prevObj); err == nil {
				obj.Version = prevObj.Version + 1
			}
		}
		if obj.Version == 0 {
			obj.Version = 1
		}
		if obj.CreatedAt.IsZero() {
			obj.CreatedAt = time.Now().UTC()
		}
		buf, err := json.Marshal(obj)
		if err != nil {
			return err
		}
		return b.Put(k, buf)
	})
}

// GetObject fetches an object's metadata. Legacy single-chunk records
// are normalized to the new Chunks-shape transparently.
func (m *MetaStore) GetObject(bucket, key string) (*ObjectMeta, error) {
	var out ObjectMeta
	err := m.db.Load().View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketObjects).Get(objKey(bucket, key))
		if raw == nil {
			return ErrNotFound
		}
		return json.Unmarshal(raw, &out)
	})
	if err != nil {
		return nil, err
	}
	normalizeLegacy(&out)
	return &out, nil
}

// UpdateChunkReplicas updates Replicas of a single chunk inside an object,
// without bumping Version (data unchanged — only placement metadata moved).
//
// chunkIndex is the 0-based position in obj.Chunks. Used by the rebalance worker
// after copying a chunk to additional DNs.
//
// Returns ErrNotFound if the object doesn't exist;
// Returns an error if chunkIndex is out of range.
func (m *MetaStore) UpdateChunkReplicas(bucket, key string, chunkIndex int, replicas []string) error {
	k := objKey(bucket, key)
	return m.db.Load().Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketObjects)
		raw := b.Get(k)
		if raw == nil {
			return ErrNotFound
		}
		var obj ObjectMeta
		if err := json.Unmarshal(raw, &obj); err != nil {
			return err
		}
		normalizeLegacy(&obj)
		if chunkIndex < 0 || chunkIndex >= len(obj.Chunks) {
			return fmt.Errorf("store: chunk index %d out of range [0,%d)", chunkIndex, len(obj.Chunks))
		}
		obj.Chunks[chunkIndex].Replicas = replicas
		// Drop legacy fields on write so file stays in new schema.
		obj.LegacyChunkID = ""
		obj.LegacyReplicas = nil
		buf, err := json.Marshal(&obj)
		if err != nil {
			return err
		}
		return b.Put(k, buf)
	})
}

// normalizeLegacy converts an old single-chunk record (LegacyChunkID set,
// Chunks empty) into the new Chunks-shape in-memory. Idempotent.
func normalizeLegacy(o *ObjectMeta) {
	if len(o.Chunks) == 0 && len(o.Stripes) == 0 && o.LegacyChunkID != "" {
		o.Chunks = []ChunkRef{{
			ChunkID:  o.LegacyChunkID,
			Size:     o.Size,
			Replicas: o.LegacyReplicas,
		}}
	}
	// Always clear legacy fields after normalization so callers see only Chunks/Stripes.
	o.LegacyChunkID = ""
	o.LegacyReplicas = nil
}

// UpdateShardReplicas updates the Replicas of a single shard inside an
// EC-stored object's stripe. Used by rebalance for EC objects.
//
// Returns ErrNotFound if the object doesn't exist; an error if the indices
// are out of range or the object isn't EC-mode.
func (m *MetaStore) UpdateShardReplicas(bucket, key string, stripeIndex, shardIndex int, replicas []string) error {
	k := objKey(bucket, key)
	return m.db.Load().Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketObjects)
		raw := b.Get(k)
		if raw == nil {
			return ErrNotFound
		}
		var obj ObjectMeta
		if err := json.Unmarshal(raw, &obj); err != nil {
			return err
		}
		normalizeLegacy(&obj)
		if !obj.IsEC() {
			return fmt.Errorf("store: %s/%s is not EC-mode", bucket, key)
		}
		if stripeIndex < 0 || stripeIndex >= len(obj.Stripes) {
			return fmt.Errorf("store: stripe index %d out of range [0,%d)", stripeIndex, len(obj.Stripes))
		}
		stripe := &obj.Stripes[stripeIndex]
		if shardIndex < 0 || shardIndex >= len(stripe.Shards) {
			return fmt.Errorf("store: shard index %d out of range [0,%d)", shardIndex, len(stripe.Shards))
		}
		stripe.Shards[shardIndex].Replicas = replicas
		obj.LegacyChunkID = ""
		obj.LegacyReplicas = nil
		buf, err := json.Marshal(&obj)
		if err != nil {
			return err
		}
		return b.Put(k, buf)
	})
}

// DeleteObject removes an object's metadata. Returns ErrNotFound if absent.
func (m *MetaStore) DeleteObject(bucket, key string) error {
	return m.db.Load().Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketObjects)
		k := objKey(bucket, key)
		if b.Get(k) == nil {
			return ErrNotFound
		}
		return b.Delete(k)
	})
}

// ListObjects returns all objects (MVP: no pagination).
// Legacy single-chunk records are normalized to the new Chunks-shape.
func (m *MetaStore) ListObjects() ([]*ObjectMeta, error) {
	var out []*ObjectMeta
	err := m.db.Load().View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketObjects).ForEach(func(_, v []byte) error {
			var o ObjectMeta
			if err := json.Unmarshal(v, &o); err != nil {
				return err
			}
			normalizeLegacy(&o)
			out = append(out, &o)
			return nil
		})
	})
	return out, err
}

// PutDN upserts a DN registry entry.
func (m *MetaStore) PutDN(dn *DNInfo) error {
	return m.db.Load().Update(func(tx *bbolt.Tx) error {
		buf, err := json.Marshal(dn)
		if err != nil {
			return err
		}
		return tx.Bucket(bucketDNs).Put([]byte(dn.ID), buf)
	})
}

// ListDNs lists all known DNs.
func (m *MetaStore) ListDNs() ([]*DNInfo, error) {
	var out []*DNInfo
	err := m.db.Load().View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketDNs).ForEach(func(_, v []byte) error {
			var d DNInfo
			if err := json.Unmarshal(v, &d); err != nil {
				return err
			}
			out = append(out, &d)
			return nil
		})
	})
	return out, err
}

func objKey(bucket, key string) []byte {
	return fmt.Appendf(nil, "%s/%s", bucket, key)
}

// ─── Runtime DN registry (ADR-027) ───
//
// dns_runtime bucket: addr → registered_at JSON. Distinct from DNInfo (above)
// which is a future heartbeat registry. dns_runtime is the live placement set.

// AddRuntimeDN registers an addr as part of the live DN set. Idempotent
// (re-add updates registered_at).
func (m *MetaStore) AddRuntimeDN(addr string) error {
	if addr == "" {
		return errors.New("store: empty addr")
	}
	return m.db.Load().Update(func(tx *bbolt.Tx) error {
		buf, err := json.Marshal(map[string]any{
			"addr":          addr,
			"registered_at": time.Now().UTC(),
		})
		if err != nil {
			return err
		}
		return tx.Bucket(bucketDNsRuntime).Put([]byte(addr), buf)
	})
}

// RemoveRuntimeDN deletes addr from the live DN set. Returns nil if absent.
func (m *MetaStore) RemoveRuntimeDN(addr string) error {
	if addr == "" {
		return errors.New("store: empty addr")
	}
	return m.db.Load().Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketDNsRuntime).Delete([]byte(addr))
	})
}

// ListRuntimeDNs returns sorted addrs from the live DN set.
func (m *MetaStore) ListRuntimeDNs() ([]string, error) {
	var out []string
	err := m.db.Load().View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketDNsRuntime).ForEach(func(k, _ []byte) error {
			out = append(out, string(k))
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// SeedRuntimeDNs populates the bucket from a list (for first boot from
// EDGE_DNS env, or for EDGE_DNS_RESET=1 disaster-recovery).
func (m *MetaStore) SeedRuntimeDNs(addrs []string) error {
	return m.db.Load().Update(func(tx *bbolt.Tx) error {
		// Wipe + reseed.
		if err := tx.DeleteBucket(bucketDNsRuntime); err != nil && !errors.Is(err, bbolt.ErrBucketNotFound) {
			return err
		}
		b, err := tx.CreateBucket(bucketDNsRuntime)
		if err != nil {
			return err
		}
		for _, addr := range addrs {
			buf, err := json.Marshal(map[string]any{
				"addr":          addr,
				"registered_at": time.Now().UTC(),
			})
			if err != nil {
				return err
			}
			if err := b.Put([]byte(addr), buf); err != nil {
				return err
			}
		}
		return nil
	})
}

// ─── UrlKey secret store (ADR-028) ───

// URLKeyEntry is the persisted record per kid.
type URLKeyEntry struct {
	Kid       string    `json:"kid"`
	SecretHex string    `json:"secret_hex"` // hex-encoded; binary safe in JSON
	IsPrimary bool      `json:"is_primary"`
	CreatedAt time.Time `json:"created_at"`
}

// PutURLKey upserts a (kid, secret) pair. If isPrimary, also clears
// is_primary on all other kids in one transaction.
func (m *MetaStore) PutURLKey(kid string, secretHex string, isPrimary bool) error {
	if kid == "" || secretHex == "" {
		return errors.New("store: kid and secret_hex required")
	}
	return m.db.Load().Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketURLKeySecrets)
		if isPrimary {
			// Clear primary flag on existing entries.
			if err := b.ForEach(func(k, v []byte) error {
				var e URLKeyEntry
				if err := json.Unmarshal(v, &e); err != nil {
					return err
				}
				if !e.IsPrimary {
					return nil
				}
				e.IsPrimary = false
				buf, err := json.Marshal(&e)
				if err != nil {
					return err
				}
				return b.Put(k, buf)
			}); err != nil {
				return err
			}
		}
		// Preserve created_at if exists; otherwise set now.
		created := time.Now().UTC()
		if prev := b.Get([]byte(kid)); prev != nil {
			var pe URLKeyEntry
			if err := json.Unmarshal(prev, &pe); err == nil && !pe.CreatedAt.IsZero() {
				created = pe.CreatedAt
			}
		}
		entry := URLKeyEntry{Kid: kid, SecretHex: secretHex, IsPrimary: isPrimary, CreatedAt: created}
		buf, err := json.Marshal(&entry)
		if err != nil {
			return err
		}
		return b.Put([]byte(kid), buf)
	})
}

// DeleteURLKey removes a kid. Refuses primary kid (caller must rotate first).
func (m *MetaStore) DeleteURLKey(kid string) error {
	return m.db.Load().Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketURLKeySecrets)
		raw := b.Get([]byte(kid))
		if raw == nil {
			return ErrNotFound
		}
		var e URLKeyEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			return err
		}
		if e.IsPrimary {
			return errors.New("store: cannot delete primary kid")
		}
		return b.Delete([]byte(kid))
	})
}

// ListURLKeys returns all entries sorted by kid.
func (m *MetaStore) ListURLKeys() ([]*URLKeyEntry, error) {
	var out []*URLKeyEntry
	err := m.db.Load().View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketURLKeySecrets).ForEach(func(_, v []byte) error {
			var e URLKeyEntry
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			out = append(out, &e)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kid < out[j].Kid })
	return out, nil
}
