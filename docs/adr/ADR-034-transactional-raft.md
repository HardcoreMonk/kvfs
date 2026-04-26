# ADR-034 — Transactional Raft (replicate-then-commit for PutObject)

## 상태
Accepted · 2026-04-26

## 맥락

ADR-033 의 informational strict mode 는 quorum 실패를 client 에 503 으로 알려주지만
**bbolt 는 이미 commit 됨** — 진짜 transactional 보장 X. 운영자 alerting 만 강화.

진정한 Raft semantics:
1. Leader 가 entry 를 log 에 append (durable)
2. 모든 peer 에 replicate
3. **Quorum-ack 받은 후에야** state machine 적용 + client 응답
4. Quorum 실패 시 entry 삭제 + client error

본 ADR 은 (3)-(4) 를 PutObject 에 한해서 구현 (가장 빈번한 mutation).

## 결정

### 새 store API: pre-flight + commit-after-replicate

`internal/store/wal.go`:
```go
// Pre-flight: build the entry that PutObject WOULD emit, without persisting.
func (m *MetaStore) MarshalPutObjectEntry(obj *ObjectMeta) ([]byte, error)
```

`internal/store/store.go`:
```go
// Commit path called by edge AFTER quorum-ack from peers. Writes bbolt +
// WAL but suppresses the WAL hook (peers already have it; double-push 방지).
func (m *MetaStore) PutObjectAfterReplicate(obj *ObjectMeta) error
```

내부적으로 `putObjectInternalFull(obj, writeWAL=true, fireHook=false)` 를 호출.

### 새 edge wrapper: commitPutMeta

```go
func (s *Server) commitPutMeta(ctx, meta) error {
    if s.StrictReplication && s.Elector != nil && s.Elector.IsLeader() {
        body, _ := s.Store.MarshalPutObjectEntry(meta)
        if err := s.Elector.ReplicateEntry(ctx, body); err != nil {
            return fmt.Errorf("strict replicate: %w", err)  // ← 여기서 멈춤
        }
        return s.Store.PutObjectAfterReplicate(meta)
    }
    return s.Store.PutObject(meta)
}
```

handlePut 와 handlePutECStream 둘 다 `commitPutMeta` 사용 → replication path
일관.

### 환경 변수 분리

- `EDGE_STRICT_REPL=1` (ADR-033) — informational: bbolt 이미 commit, 503 은
  alert 신호. WAL hook 동기화.
- `EDGE_TRANSACTIONAL_RAFT=1` (ADR-034) — true strict: replicate 먼저, quorum
  후에 bbolt commit. PutObject 한정.

둘 다 동시 활성 가능 — 다른 mutation (DeleteObject 등) 은 informational, PutObject
는 transactional.

### 한계 (의도적)

- **PutObject 만** transactional. DeleteObject / Add+RemoveRuntimeDN 은 ADR-033
  informational mode 그대로. 추가 op 는 같은 패턴으로 확장 가능.
- **chunks 는 이미 DN 에 쓰여있음** — meta commit 만 transactional. quorum 실패 후
  client retry 시 chunk content-addressable id 가 같으면 dedup, 다르면 새 chunk.
  GC 가 결국 정리.
- **단일 결정** — log replication 이 entry 단위로 quorum-acked. ordering /
  prevLogIndex 검증 없음 — true Raft 의 log-matching invariant 는 미보장.

## 결과

**긍정**:
- Quorum 실패 시 client 가 503 받고 leader bbolt 도 깨끗 (나중에 stale read 없음)
- "내 PutObject 200 = 적어도 N/2+1 peer 가 본 상태" invariant 성립
- 기존 코드 영향 0 — opt-in (`EDGE_TRANSACTIONAL_RAFT=1`)

**부정**:
- 추가 RTT (push 후 commit) — write latency = bbolt + push round-trip
- PutObject 만 — 다른 mutation 은 mixed semantics (운영자 인지 필요)
- chunks 는 이미 DN 에 쓰여있어 cleanup 책임이 GC 로 (ADR-012)

**트레이드오프**:
- True log-matching Raft 는 prevLogIndex/Term 매칭 + 충돌 정리 + commit index
  추적 등 본격 구현 필요 — 본 ADR 의 단일-결정 모델은 pragmatic stopping point
- ApplyEntry on followers: leader 가 보낸 entry 의 seq 는 0 (placeholder).
  Followers 가 자기 seq 부여 — leader/follower seq drift 의도적 (각자 local
  audit log)

## 데모 스토리

3-edge cluster + EDGE_TRANSACTIONAL_RAFT=1:
1. Healthy: PUT → leader replicate → quorum (3/3) → commit → 200
2. kill 2 peers (quorum 무너짐)
3. PUT → leader replicate → 0/2 peers respond → quorum fail → 503
4. Restart peers, snapshot pull, rejoin
5. PUT → 200 (다시 healthy)

## 관련

- `internal/store/wal.go` — `MarshalPutObjectEntry`, `appendWALControlled`
- `internal/store/store.go` — `PutObjectAfterReplicate`, `putObjectInternalFull`
- `internal/edge/edge.go` — `Server.StrictReplication`, `commitPutMeta`
- `cmd/kvfs-edge/main.go` — `EDGE_TRANSACTIONAL_RAFT` env
- 후속 ADR 후보:
  - 모든 mutation 에 transactional 확장
  - True log-matching Raft (prevLogIndex / log conflict 처리)
  - Atomic chunk + meta commit (rollback chunk 도 시도)
