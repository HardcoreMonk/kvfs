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
// For MVP: 1 object = 1 chunk, replicated across Replicas.
type ObjectMeta struct {
	Bucket      string    `json:"bucket"`
	Key         string    `json:"key"`
	Size        int64     `json:"size"`
	ChunkID     string    `json:"chunk_id"`             // hex(sha256(body))
	Replicas    []string  `json:"replicas"`             // DN addresses that acked the write
	Version     int64     `json:"version"`              // monotonic per-key version
	CreatedAt   time.Time `json:"created_at"`
	ContentType string    `json:"content_type,omitempty"`
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

// GetObject fetches an object's metadata.
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
	return &out, nil
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
func (m *MetaStore) ListObjects() ([]*ObjectMeta, error) {
	var out []*ObjectMeta
	err := m.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketObjects).ForEach(func(_, v []byte) error {
			var o ObjectMeta
			if err := json.Unmarshal(v, &o); err != nil {
				return err
			}
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
