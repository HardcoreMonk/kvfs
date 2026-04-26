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
//
// Two write modes:
//   - Inline (default): every Append flushes + fsyncs before returning.
//     Lowest possible RPO; throughput limited by ~1 fsync per write.
//   - Batched (ADR-035): set batchInterval > 0. Append writes bytes under
//     the same mu (preserving on-disk order), then waits on a cond for a
//     background flusher to issue a single fsync covering many writes.
//     Throughput scales with concurrency at the cost of up to batchInterval
//     additional latency per write.
//
// 비전공자용 해설 (group commit)
// ─────────────────────────────
// 매 PUT 마다 fsync 하면 NVMe 라도 ~10 µs ~ 수 ms (디바이스에 따라). PUT 이
// 100 개 동시 들어오면 100 번 fsync — 직렬화. group commit 은 "5 ms 동안 들어온
// PUT 을 모아 한 번만 fsync" — write throughput ↑, 개별 latency 는 batchInterval
// 만큼 살짝 ↑. PostgreSQL · MySQL · etcd 등 모든 DB 가 사용하는 표준 기법.
type WAL struct {
	path string

	mu  sync.Mutex // serializes Append; protects f
	f   *os.File
	w   *bufio.Writer
	seq atomic.Int64 // monotonic; last written entry's seq

	// Group-commit state (active only when batchInterval > 0).
	batchInterval time.Duration
	cond          *sync.Cond    // tied to mu; broadcast when durableSeq advances or closed
	durableSeq    int64         // covered by mu; highest seq known fsynced
	closed        bool          // covered by mu
	closeCh       chan struct{} // closed by Close to signal flusher exit
	flusherDone   chan struct{} // flusher closes when it returns
	// epoch bumps on Truncate so cond-waiters from a prior generation can
	// detect that their seq is no longer reachable and bail instead of
	// parking forever.
	epoch uint64

	// Observability (ADR-036). Both are 0 when batchInterval == 0 or no
	// pending entries — gauges interpret 0 as "nothing to report".
	lastBatchSize    atomic.Int64 // entries flushed in the most recent fsync cycle
	oldestUnsyncedNs atomic.Int64 // unix-ns of the oldest unsynced entry, 0 = none
}

// OpenWAL opens (or creates) the WAL at path with inline fsync (one fsync
// per Append). Recovers the highest seq from the existing file so subsequent
// Appends continue monotonically.
func OpenWAL(path string) (*WAL, error) {
	return OpenWALWithBatch(path, 0)
}

// OpenWALWithBatch opens (or creates) the WAL with optional group-commit
// batching. batchInterval == 0 → inline fsync (same as OpenWAL). >0 spawns
// a background flusher that fsyncs every batchInterval ms; Append waits for
// the next fsync covering its seq before returning.
//
// Recommended values: 1ms~10ms. Below 1ms the timer overhead eats the
// savings; above 10ms the per-write latency penalty is noticeable.
func OpenWALWithBatch(path string, batchInterval time.Duration) (*WAL, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("wal open %s: %w", path, err)
	}
	w := &WAL{path: path, f: f, w: bufio.NewWriter(f), batchInterval: batchInterval}
	w.cond = sync.NewCond(&w.mu)
	last, err := w.recoverLastSeq()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	w.seq.Store(last)
	w.durableSeq = last
	if batchInterval > 0 {
		w.closeCh = make(chan struct{})
		w.flusherDone = make(chan struct{})
		go w.flusher()
	}
	return w, nil
}

// flusher is the background group-commit goroutine. Wakes every
// batchInterval, flushes any buffered bytes + fsyncs once, then advances
// durableSeq and broadcasts to waiters. Runs only when batchInterval > 0.
func (w *WAL) flusher() {
	defer close(w.flusherDone)
	t := time.NewTicker(w.batchInterval)
	defer t.Stop()
	for {
		select {
		case <-w.closeCh:
			return
		case <-t.C:
			w.flushOnce()
		}
	}
}

// flushOnce performs one flush+fsync cycle if there's pending data.
//
// Critical-section discipline:
//   - Hold mu only long enough to push bufio into the kernel and snapshot
//     `pending`. The slow part — f.Sync() — runs OUTSIDE mu so the next
//     batch of Append calls can fill bufio in parallel with the disk fsync.
//   - Re-acquire mu briefly to publish durableSeq + Broadcast to waiters.
//
// Safe because the underlying *os.File is concurrent-safe for write+sync,
// and bufio.Writer is no longer touched during the fsync window.
func (w *WAL) flushOnce() {
	w.mu.Lock()
	pending := w.seq.Load()
	if pending <= w.durableSeq || w.closed {
		w.mu.Unlock()
		return
	}
	epoch := w.epoch
	if err := w.w.Flush(); err != nil {
		w.mu.Unlock()
		return // leave durableSeq unchanged; next tick retries
	}
	f := w.f
	w.mu.Unlock()

	if err := f.Sync(); err != nil {
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	// Drop this fsync's result if Truncate rotated the file under us — the
	// pending seq we observed no longer means anything.
	if w.epoch != epoch || w.closed {
		return
	}
	if pending > w.durableSeq {
		w.lastBatchSize.Store(pending - w.durableSeq)
		w.durableSeq = pending
		w.oldestUnsyncedNs.Store(0)
		w.cond.Broadcast()
	}
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

// LastBatchSize returns the entry count of the most recent fsync cycle in
// group-commit mode (0 if inline mode or no flush has happened yet). Used
// by ADR-036 metric kvfs_wal_batch_size.
func (w *WAL) LastBatchSize() int64 {
	if w == nil {
		return 0
	}
	return w.lastBatchSize.Load()
}

// OldestUnsyncedAge returns how long the oldest unsynced entry has been
// waiting (0 if nothing pending or inline mode). Used by ADR-036 metric
// kvfs_wal_durable_lag_seconds.
func (w *WAL) OldestUnsyncedAge() time.Duration {
	if w == nil {
		return 0
	}
	ns := w.oldestUnsyncedNs.Load()
	if ns == 0 {
		return 0
	}
	return time.Duration(time.Now().UnixNano() - ns)
}

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
	if w.batchInterval == 0 {
		if err := w.w.Flush(); err != nil {
			return 0, nil, fmt.Errorf("wal flush: %w", err)
		}
		if err := w.f.Sync(); err != nil {
			return 0, nil, fmt.Errorf("wal fsync: %w", err)
		}
		return seq, line, nil
	}
	// Mark this entry as the oldest unsynced one if no batch is in flight.
	// The flusher resets oldestUnsyncedNs to 0 on each successful fsync.
	w.oldestUnsyncedNs.CompareAndSwap(0, time.Now().UnixNano())
	// Group-commit: park until flusher fsyncs past our seq, OR Truncate
	// bumps the epoch out from under us, OR Close fires. cond is bound to
	// mu so Wait atomically releases + reacquires.
	myEpoch := w.epoch
	for w.durableSeq < seq && !w.closed && w.epoch == myEpoch {
		w.cond.Wait()
	}
	if w.epoch != myEpoch {
		return 0, nil, fmt.Errorf("wal: truncated while waiting for fsync (seq=%d)", seq)
	}
	if w.closed && w.durableSeq < seq {
		return 0, nil, fmt.Errorf("wal: closed before fsync (seq=%d, durable=%d)", seq, w.durableSeq)
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
	w.durableSeq = 0
	w.epoch++
	w.lastBatchSize.Store(0)
	w.oldestUnsyncedNs.Store(0)
	if w.cond != nil {
		w.cond.Broadcast()
	}
	return prev, nil
}

// Close flushes pending writes, signals the flusher (if any) to stop, and
// closes the file handle. Safe to call multiple times.
//
// Group-commit mode: a final inline flush+fsync runs so any Append goroutines
// still waiting on cond can return success rather than seeing the "closed
// before fsync" error.
func (w *WAL) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	// Final flush+fsync covers any in-flight Appends.
	if w.w != nil {
		_ = w.w.Flush()
	}
	if w.f != nil {
		_ = w.f.Sync()
	}
	if pending := w.seq.Load(); pending > w.durableSeq {
		w.durableSeq = pending
	}
	if w.cond != nil {
		w.cond.Broadcast()
	}
	closeCh := w.closeCh
	flusherDone := w.flusherDone
	w.mu.Unlock()

	// Wait for the flusher to exit (it may be parked on its ticker).
	if closeCh != nil {
		close(closeCh)
		<-flusherDone
	}

	w.mu.Lock()
	defer w.mu.Unlock()
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
