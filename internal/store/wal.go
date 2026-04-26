// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Write-ahead log of metadata mutations (ADR-019).
//
// Append-only JSON-lines file. Each mutation that hits bbolt also gets one
// WAL entry written + fsynced *after* the bbolt commit succeeds. The WAL
// gives followers (ADR-022/031) a way to ship deltas instead of full
// snapshots, dropping RPO from "snapshot interval" to "WAL flush latency".
//
// 비전공자용 해설
// ──────────────
// ADR-014 snapshot 은 메타 전체를 한 번에 복사. 1h 마다 한 번이면 마지막 snapshot
// 이후의 write 는 leader 죽을 때 잃는다 (RPO ≤ 1h).
//
// WAL 은 매 mutation 을 별도 파일에 append. 따라서:
//   - leader 가 PUT obj-X 처리 → bbolt commit → WAL.Append("put_object", obj-X)
//   - follower 가 GET /v1/admin/wal?since=last_seq → 그 동안 변경분 byte stream
//   - follower 가 ApplyEntry(entry) 로 자기 bbolt 에 동일 변경 적용
//
// 결과: snapshot interval 이 길어도 WAL pull 만 자주 (예: 1s) 돌리면 follower
// lag 가 ~ WAL pull interval 에 가까움. RPO 분 → 초.
//
// MVP 가정:
//   - WAL 은 optional — 안 켜도 기존 동작 0 변경
//   - WAL 은 단순 JSON-lines append-only (binary format 아님)
//   - rotation 은 snapshot 시 truncate (snapshot 이 모든 상태 포함)
//   - 본 ADR 은 WAL endpoint + apply API 까지. follower 자동 pull integration 은 후속
package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// WALOp is the typed name of a mutation kind. Defined as a string for
// JSON-friendly wire format; constants below are the only legal values
// (ApplyEntry's switch is the authoritative producer/consumer pair).
type WALOp string

const (
	OpPutObject      WALOp = "put_object"
	OpDeleteObject   WALOp = "delete_object"
	OpAddRuntimeDN   WALOp = "add_runtime_dn"
	OpRemoveRuntimeDN WALOp = "remove_runtime_dn"
)

// WALEntry is one record in the WAL.
type WALEntry struct {
	Seq       int64           `json:"seq"`
	Timestamp time.Time       `json:"ts"`
	Op        string          `json:"op"`
	Args      json.RawMessage `json:"args"`
}

// WAL is an append-only mutation log.
type WAL struct {
	path string

	mu  sync.Mutex // serializes Append; protects f
	f   *os.File
	w   *bufio.Writer
	seq atomic.Int64 // monotonic; last written entry's seq
}

// OpenWAL opens (or creates) the WAL at path. Recovers the highest seq from
// the existing file so subsequent Appends continue monotonically.
func OpenWAL(path string) (*WAL, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("wal open %s: %w", path, err)
	}
	w := &WAL{path: path, f: f, w: bufio.NewWriter(f)}
	last, err := w.recoverLastSeq()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	w.seq.Store(last)
	return w, nil
}

// recoverLastSeq scans the existing file to find the highest seq.
// Called once on Open; never on the hot path.
func (w *WAL) recoverLastSeq() (int64, error) {
	if _, err := w.f.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	defer w.f.Seek(0, io.SeekEnd) //nolint:errcheck
	sc := bufio.NewScanner(w.f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var last int64
	for sc.Scan() {
		var e WALEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			// Corrupt tail: stop here. Subsequent appends will continue from last.
			break
		}
		if e.Seq > last {
			last = e.Seq
		}
	}
	return last, nil
}

// LastSeq returns the seq of the most recently appended entry (0 = empty WAL).
func (w *WAL) LastSeq() int64 { return w.seq.Load() }

// Append writes one entry, fsyncs, and returns the assigned seq plus the
// raw JSON line bytes (without the trailing newline). The bytes are returned
// so callers can reuse them for replication hooks without re-marshalling.
// Args is marshalled to JSON internally.
func (w *WAL) Append(op string, args any) (int64, []byte, error) {
	if w == nil {
		return 0, nil, nil
	}
	rawArgs, err := json.Marshal(args)
	if err != nil {
		return 0, nil, fmt.Errorf("wal marshal args: %w", err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	seq := w.seq.Add(1)
	entry := WALEntry{Seq: seq, Timestamp: time.Now().UTC(), Op: op, Args: rawArgs}
	line, err := json.Marshal(entry)
	if err != nil {
		return 0, nil, fmt.Errorf("wal marshal entry: %w", err)
	}
	if _, err := w.w.Write(line); err != nil {
		return 0, nil, fmt.Errorf("wal write: %w", err)
	}
	if err := w.w.WriteByte('\n'); err != nil {
		return 0, nil, fmt.Errorf("wal write nl: %w", err)
	}
	if err := w.w.Flush(); err != nil {
		return 0, nil, fmt.Errorf("wal flush: %w", err)
	}
	if err := w.f.Sync(); err != nil {
		return 0, nil, fmt.Errorf("wal fsync: %w", err)
	}
	return seq, line, nil
}

// Since returns all entries with seq > sinceSeq, in order.
// Streams via the file handle — caller may iterate large WALs without
// loading the whole thing.
func (w *WAL) Since(sinceSeq int64) ([]WALEntry, error) {
	if w == nil {
		return nil, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.w.Flush(); err != nil {
		return nil, err
	}
	f, err := os.Open(w.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []WALEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var e WALEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			break
		}
		if e.Seq > sinceSeq {
			out = append(out, e)
		}
	}
	return out, nil
}

// WriteSinceTo streams entries with seq > sinceSeq directly to w (used by
// the HTTP handler so we don't materialize a large slice).
func (w *WAL) WriteSinceTo(sinceSeq int64, dst io.Writer) (int, error) {
	if w == nil {
		return 0, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.w.Flush(); err != nil {
		return 0, err
	}
	f, err := os.Open(w.path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	count := 0
	for sc.Scan() {
		// Cheap seq peek without full unmarshal.
		var hdr struct {
			Seq int64 `json:"seq"`
		}
		if err := json.Unmarshal(sc.Bytes(), &hdr); err != nil {
			break
		}
		if hdr.Seq <= sinceSeq {
			continue
		}
		if _, err := dst.Write(sc.Bytes()); err != nil {
			return count, err
		}
		if _, err := dst.Write([]byte{'\n'}); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// Truncate replaces the WAL with an empty file and resets seq to 0.
// Called by snapshot routines: a fresh snapshot includes all current state,
// so older WAL entries become redundant.
//
// Caller MUST ensure no concurrent Append is in-flight (typically the same
// goroutine that drives snapshot). Returns the seq that was discarded so
// snapshot manifests can record "snapshot includes WAL up to seq N".
func (w *WAL) Truncate() (int64, error) {
	if w == nil {
		return 0, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	prev := w.seq.Load()
	if err := w.w.Flush(); err != nil {
		return prev, err
	}
	if err := w.f.Close(); err != nil {
		return prev, err
	}
	if err := os.Truncate(w.path, 0); err != nil {
		return prev, err
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return prev, err
	}
	w.f = f
	w.w = bufio.NewWriter(f)
	w.seq.Store(0)
	return prev, nil
}

// Close flushes pending writes and closes the file handle.
func (w *WAL) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.w != nil {
		_ = w.w.Flush()
	}
	if w.f != nil {
		return w.f.Close()
	}
	return nil
}

// ApplyEntry replays one WAL entry against the store. Used by followers to
// catch up between snapshot pulls. The entry's WAL hook is suppressed — we
// don't want followers to re-emit entries they're consuming.
//
// Unknown ops return an error so a future schema bump (new mutation type)
// is loud rather than silent.
func (m *MetaStore) ApplyEntry(e WALEntry) error {
	switch WALOp(e.Op) {
	case OpPutObject:
		var obj ObjectMeta
		if err := json.Unmarshal(e.Args, &obj); err != nil {
			return fmt.Errorf("apply put_object decode: %w", err)
		}
		return m.putObjectInternal(&obj, false)
	case OpDeleteObject:
		var a struct{ Bucket, Key string }
		if err := json.Unmarshal(e.Args, &a); err != nil {
			return fmt.Errorf("apply delete_object decode: %w", err)
		}
		return m.deleteObjectInternal(a.Bucket, a.Key, false)
	case OpAddRuntimeDN:
		var addr string
		if err := json.Unmarshal(e.Args, &addr); err != nil {
			return fmt.Errorf("apply add_runtime_dn decode: %w", err)
		}
		return m.addRuntimeDNInternal(addr, false)
	case OpRemoveRuntimeDN:
		var addr string
		if err := json.Unmarshal(e.Args, &addr); err != nil {
			return fmt.Errorf("apply remove_runtime_dn decode: %w", err)
		}
		return m.removeRuntimeDNInternal(addr, false)
	default:
		return fmt.Errorf("apply: unknown op %q", e.Op)
	}
}

// ApplyAll replays a batch of entries in order. Stops at the first error.
func (m *MetaStore) ApplyAll(entries []WALEntry) error {
	for i, e := range entries {
		if err := m.ApplyEntry(e); err != nil {
			return fmt.Errorf("apply entry %d (seq %d, op %s): %w", i, e.Seq, e.Op, err)
		}
	}
	return nil
}

// SetWAL attaches a WAL to the store. Pass nil to disable.
// All future PutObject / DeleteObject / Add+RemoveRuntimeDN calls will append
// an entry after the bbolt commit.
func (m *MetaStore) SetWAL(w *WAL) { m.wal = w }

// WAL returns the attached WAL (or nil).
func (m *MetaStore) WAL() *WAL { return m.wal }

// WALHook is invoked synchronously after each successful local WAL.Append.
// Used by ADR-031 follow-up (synchronous Raft-style replication) to push
// the just-appended entry to peers and wait for quorum ack.
//
// The hook receives the raw JSON entry body (already serialized for the
// WAL file) so it can be reused as the replication payload without
// re-marshal.
//
// Hook return semantics:
//   - nil (default async hook implementation) — best-effort, mutation
//     proceeds even if peers fall behind; operator monitors via metrics.
//   - error (strict-replication mode, EDGE_STRICT_REPL=1) — surfaced as
//     the mutation method's return; edge handlers respond 503. Note: the
//     bbolt commit has already happened, so this is "informational strict"
//     not transactional rollback. Followers heal via snapshot+WAL on next
//     pull. See ADR-033.
type WALHook func(entryJSON []byte) error

// SetWALHook registers a callback fired after each WAL.Append. Pass nil to
// disable. Called synchronously from PutObject / DeleteObject etc.
func (m *MetaStore) SetWALHook(h WALHook) { m.walHook = h }

// appendWAL is the unified writeWAL+hook helper used by every mutation
// method. Reuses the JSON line returned by WAL.Append so the hook payload
// is byte-identical to the on-disk entry (same seq, same timestamp).
// No-op when WAL is disabled.
//
// Returns the hook's error (if any) so strict-replication mode can surface
// quorum failure to the mutation method's caller. Async hooks should
// return nil unconditionally.
func (m *MetaStore) appendWAL(op WALOp, args any) error {
	return m.appendWALControlled(op, args, true)
}

// appendWALControlled is the same as appendWAL but lets the caller suppress
// the hook (used by ADR-034 transactional Raft commit path: peer push
// already happened pre-commit, so we don't want to push again).
func (m *MetaStore) appendWALControlled(op WALOp, args any, fireHook bool) error {
	if m.wal == nil {
		return nil
	}
	_, line, err := m.wal.Append(string(op), args)
	if err != nil || line == nil {
		return err
	}
	if fireHook && m.walHook != nil {
		return m.walHook(line)
	}
	return nil
}

// MarshalPutObjectEntry returns the WAL entry body that PutObject WOULD
// emit, without persisting anything. Used by ADR-034 transactional Raft:
// leader pre-marshals the entry, replicates to peers, waits for quorum,
// then calls PutObjectAfterReplicate to commit locally.
//
// Seq is left 0 in this preview — followers don't need leader's seq for
// ApplyEntry (they apply by op+args). Local seq is assigned at real
// PutObjectAfterReplicate time.
func (m *MetaStore) MarshalPutObjectEntry(obj *ObjectMeta) ([]byte, error) {
	rawArgs, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	entry := WALEntry{
		Op:        string(OpPutObject),
		Args:      rawArgs,
		Timestamp: time.Now().UTC(),
	}
	return json.Marshal(entry)
}

// Errors returned by WAL operations.
var (
	ErrWALClosed = errors.New("wal: closed")
)
