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
	"time"

	"go.etcd.io/bbolt"
)

var (
	bucketObjects = []byte("objects")
	bucketDNs     = []byte("dns")

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
	Chunks      []ChunkRef `json:"chunks,omitempty"`
	Version     int64      `json:"version"`
	CreatedAt   time.Time  `json:"created_at"`
	ContentType string     `json:"content_type,omitempty"`

	// Legacy single-chunk fields. Read-only; never written by current code.
	// Present so that old bbolt records still decode without error.
	LegacyChunkID  string   `json:"chunk_id,omitempty"`
	LegacyReplicas []string `json:"replicas,omitempty"`
}

// ChunkRef points to one chunk that, together with its peers in
// ObjectMeta.Chunks, reconstructs an object.
type ChunkRef struct {
	ChunkID  string   `json:"chunk_id"` // hex(sha256(chunk_data))
	Size     int64    `json:"size"`
	Replicas []string `json:"replicas"` // DN addresses that acked
}

// DNInfo records a known DataNode (registry, heartbeats).
// Populated lazily when edge first talks to a DN.
type DNInfo struct {
	ID     string    `json:"id"`
	Addr   string    `json:"addr"`
	LastHB time.Time `json:"last_hb"`
	Status string    `json:"status"` // "healthy" | "unhealthy"
}

// MetaStore is a bbolt-backed metadata store.
type MetaStore struct {
	db *bbolt.DB
}

// Open opens or creates a bbolt database file at path.
func Open(path string) (*MetaStore, error) {
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open bbolt: %w", err)
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		for _, b := range [][]byte{bucketObjects, bucketDNs} {
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
	return &MetaStore{db: db}, nil
}

// Close flushes and closes the bbolt file.
func (m *MetaStore) Close() error { return m.db.Close() }

// PutObject stores or overwrites an object's metadata.
// Version is auto-incremented if an existing record is found.
func (m *MetaStore) PutObject(obj *ObjectMeta) error {
	k := objKey(obj.Bucket, obj.Key)
	return m.db.Update(func(tx *bbolt.Tx) error {
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
	err := m.db.View(func(tx *bbolt.Tx) error {
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
	return m.db.Update(func(tx *bbolt.Tx) error {
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
	if len(o.Chunks) == 0 && o.LegacyChunkID != "" {
		o.Chunks = []ChunkRef{{
			ChunkID:  o.LegacyChunkID,
			Size:     o.Size,
			Replicas: o.LegacyReplicas,
		}}
	}
	// Always clear legacy fields after normalization so callers see only Chunks.
	o.LegacyChunkID = ""
	o.LegacyReplicas = nil
}

// DeleteObject removes an object's metadata. Returns ErrNotFound if absent.
func (m *MetaStore) DeleteObject(bucket, key string) error {
	return m.db.Update(func(tx *bbolt.Tx) error {
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
	err := m.db.View(func(tx *bbolt.Tx) error {
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
	return m.db.Update(func(tx *bbolt.Tx) error {
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
	err := m.db.View(func(tx *bbolt.Tx) error {
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
