# ADR-040 — Coord transactional commit (replicate-then-commit, ADR-034 port)

상태: Accepted (2026-04-27)
시즌: Season 5 Ep.5 · ADR-039 의 best-effort 한계 보완

## 배경

[ADR-039](ADR-039-coord-wal-replication.md) 가 coord-to-coord WAL replication 를 만들었지만 **best-effort**: leader 가 bbolt commit 후 walHook 안에서 peers 에 push. 그 사이에 leadership 잃으면:

```
leader (term=N): handleCommit → bbolt commit OK → walHook fires → IsLeader()? false (lost mid-write)
                 → push skipped → entry on local bbolt only → phantom write
new leader (term=N+1): doesn't know about that entry → starts from older state → divergence
```

/simplify (2026-04-27) 가 이 갭을 잡아 P6-07 로 등록 → 본 ADR 이 fix.

ADR-034 가 동일한 패턴을 edge 측에 이미 적용 (transactional Raft for PutObject). coord 에 그대로 port 하면 됨.

## 결정

`coord.Server.TransactionalCommit bool` field. true 일 때 `commit()` 헬퍼가 다음 흐름:

```
1. MarshalPutObjectEntry(meta) → entry 미리 직렬화 (bbolt 안 건드림)
2. ReplicateEntry(ctx, body) → peers 에 parallel push, quorum ack 대기
3. quorum 받으면 → PutObjectAfterReplicate(meta) → bbolt commit + WAL.Append (hook suppressed; peers 가 이미 받음)
4. quorum 못 받으면 → return error → handleCommit 가 503 응답, bbolt 무변화
```

**Prerequisite**:
- `Elector` 활성 (HA mode)
- WAL 활성 (`COORD_WAL_PATH`)

이 둘 중 하나라도 nil 이면 `commit()` 가 legacy `Store.PutObject` 로 fallback. main daemon 이 startup 시 mismatch (TXN_RAFT=1 + Elector/WAL 누락) 면 fatal — config-trap 회피.

`COORD_TRANSACTIONAL_RAFT=1` env opt-in (default off = ADR-039 behavior 유지).

## ADR-034 와의 관계

| 측면 | ADR-034 (edge) | ADR-040 (coord) |
|---|---|---|
| 적용 위치 | edge.commitPutMeta | coord.commit |
| Trigger | `EDGE_TRANSACTIONAL_RAFT=1` + Elector + Leader | `COORD_TRANSACTIONAL_RAFT=1` + Elector + WAL |
| 메커니즘 | 같음 (MarshalPutObjectEntry → ReplicateEntry → PutObjectAfterReplicate) | 같음 |
| 한계 | edge → coord proxy 모드 (Ep.2+) 에서는 우회됨 — coord 가 commit 함 | 본 ADR 이 그 우회를 메움 |

ADR-034 는 coord 분리 전 시대의 transactional Raft. coord 가 분리된 후엔 이 ADR (040) 가 진짜 commit-before-quorum 의 위치.

## 비채택 대안

| 대안 | 기각 사유 |
|---|---|
| walHook 안에서 IsLeader 체크를 lock 으로 감싸 atomic 하게 | bbolt commit 자체는 lock 밖에서 일어남 → 여전히 race. lock 안에 bbolt 밀어넣으면 throughput 폭락 |
| commit-then-push 후 push-fail 시 bbolt rollback | bbolt 의 PutObject 는 single tx commit — 이미 fsync 된 후 되돌릴 수 없음. transaction-then-commit 가 유일한 답 |
| best-effort 그대로 두고 phantom write 를 monitoring 으로 catch | 데이터 정합성 문제를 운영자 책임으로 미루는 것 — kvfs 의 "이해 가능한 reference" 정신과 맞지 않음 |

## 결과

+ **Phantom write 완전 제거**: leader-loss-mid-write 시 client 가 503 받음, leader bbolt 무변화. 새 leader 가 보지 못한 entry 가 옛 leader 에 남는 상황 자체가 발생 안 함.
+ **ADR-034 의 패턴 reuse**: `MarshalPutObjectEntry` / `PutObjectAfterReplicate` / `Elector.ReplicateEntry` 가 이미 존재 — 본 ADR 의 코드 추가는 ~30 LOC.
+ **Opt-in**: 기존 best-effort 사용자 영향 0. operator 가 명시적으로 켜야.
- **Latency**: 모든 commit 이 quorum RTT 만큼 ↑ (~1 ms 로컬 LAN, ~5 ms cross-AZ). hot path 의 정직한 비용.
- **가용성**: minority partition 의 leader 는 write 거부. CP 시스템 (consistency over availability). best-effort 모드는 AP — 운영자 선택 필요.
- **DeleteObject + AddRuntimeDN + RemoveRuntimeDN 은 미적용**: PutObject 만 transactional. 이는 ADR-034 의 동일한 결정 — phantom delete 보다 phantom write 가 사용자에게 더 보임. 후속 ADR 에서 확장 가능.

## 호환성

- `COORD_TRANSACTIONAL_RAFT` 미설정 시 동작 0 변경.
- `coord.Server.TransactionalCommit` field 추가 — false 가 zero value, 기존 caller 영향 0.
- Prerequisites mismatch (TRANSACTIONAL_RAFT=1 + Elector/WAL 누락) → daemon startup fatal. silent fallback 보다 명확한 에러.

## 검증

`internal/coord/coord_test.go`:
- `TestTransactionalCommit_QuorumFailureLeavesBboltUntouched` — 3-peer cluster 에서 self 만 reachable → quorum 불가능 → commit() 가 error 반환 + bbolt 무변화 + WAL.LastSeq 무변화 확인.
- `TestTransactionalCommit_FallsBackWhenPrerequisitesMissing` — TRANSACTIONAL_RAFT=true 인데 Elector + WAL 둘 다 nil → legacy path 로 정상 commit (config-trap guard).

`scripts/demo-he.sh` (히브리 ה) — live 시연: 3-coord cluster + COORD_TRANSACTIONAL_RAFT=1. 2개 coord 죽이면 quorum 깨짐 → PUT 503. 살리면 정상 commit. 갭 메움 시각화.
