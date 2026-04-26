# ADR-033 — Strict replication mode (informational, not transactional)

## 상태
Accepted · 2026-04-26

## 맥락

Season 4 Ep.8 (sync Raft-style WAL push) 이 follower 가 leader-pushed entry 를
받으면 즉시 적용하는 path 를 추가했지만, 동작은 best-effort:

- Leader가 bbolt commit + WAL append 후 push 시작
- Push 가 quorum-ack 못 받아도 client 에는 200 OK
- Follower lag 는 metrics 로만 노출

이 "best-effort"는 운영자가 "내 write 는 정말 quorum 에 닿았나?" 를 확신할 수
없게 만든다. 진정한 Raft 는 commit 전에 quorum-ack 를 보장한다.

## 결정 — informational strict mode

EDGE_STRICT_REPL=1 환경 변수로 활성화. 동작:

1. WALHook 시그니처가 `func([]byte) error` (이전: `func([]byte)`)
2. Strict 모드: hook 이 ReplicateEntry 를 동기적으로 호출, quorum-ack 못 받으면
   error 반환
3. mutation 메서드 (PutObject etc) 가 그 error 를 caller 로 propagate
4. handlePut 등이 error 보면 503 → client 가 retry 가능

**중요한 한계**: bbolt commit 은 이미 일어났음. error 반환은 "quorum 안 닿았다"
의 informational 신호일 뿐 transactional rollback 아님.

따라서 strict 모드는:
- **alerting upgrade** — quorum 실패가 client-visible (503) → 운영자가 즉시 인지
- **NOT data-loss prevention** — leader 의 bbolt 에는 이미 적용됨
- 후속 snapshot pull 또는 WAL pull 이 follower 를 catch up

## 진정한 transactional Raft (별도 ADR 후보)

진짜 strict 를 하려면:
1. WAL.Append 가 fsync 안 된 "prepared" 상태로 entry 저장
2. ReplicateEntry 가 quorum 받으면 fsync + bbolt commit
3. Quorum 실패 → entry 삭제 (rollback) + bbolt 미적용

이는 store schema (bbolt + WAL) 의 commit 순서를 모두 뒤집는 큰 작업. 본
ADR 의 범위 외.

## 결과

**긍정**:
- Strict 모드 활성화 시 follower lag 가 client-visible — 운영자가 즉시 alert
- 시스템 invariant: "leader 가 200 응답한 write 는 quorum-ack 됐다" (informational)
- async 모드 (default) 는 변동 0 — 기존 운영 환경 영향 없음

**부정**:
- bbolt 가 이미 commit 됐으므로 진짜 atomicity 는 아님 — drift 가능 (leader 적용 + peers 안 받음)
- snapshot pull 이 follower heal 하지만, 그 사이의 read-from-follower 는 stale
- Real Raft 는 quorum 기반 commit + apply order 가 필요 — 본 모드는 그 단계 직전

**트레이드오프**:
- async (default): 낮은 latency, 약한 보장
- strict: 높은 latency (quorum RTT), 강한 alerting (transactional 아님)

## 관련

- `internal/store/wal.go` — WALHook 시그니처 변경 (`error` 반환)
- `internal/store/store.go` — 4 mutation 메서드의 appendWAL 결과 propagate
- `cmd/kvfs-edge/main.go` — `EDGE_STRICT_REPL=1` 분기, hook 동기/비동기
- 후속 ADR-034: transactional Raft (commit 후 push → push 후 commit)
- 관련 ADR: ADR-019 (WAL), ADR-022 (multi-edge HA), ADR-031 (election + sync push)
