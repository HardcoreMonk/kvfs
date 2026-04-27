# Episode 33 — coord 의 transactional commit

> **Series**: kvfs — distributed object storage from scratch
> **Season**: 5 (coord 분리) · **Episode**: 5
> **연결**: [ADR-040](../docs/adr/ADR-040-coord-transactional-commit.md) · [demo-he](../scripts/demo-he.sh)

---

## 마지막 갭

[Ep.32](32-coord-wal-repl.md) 끝에서 인정한 것:

```
t=0   coord1 leader, PUT 도착
t=1   coord1: bbolt commit + WAL append (this is the racy moment)
t=2   coord1 leadership lost
t=3   walHook fires: IsLeader()? false → push 안 함
t=4   client: 5xx
t=5   coord1.bbolt 에 entry 있음, peers 없음 → divergence
```

rare race. 그러나 production 에선 rare ≠ never. 분산 시스템 정합성을 위해서는
이 window 도 닫아야 한다.

해결: **commit 의 순서를 뒤집는다**. 그것이 ADR-040.

## ADR-034 의 port

[Ep.21 transactional Raft](21-transactional-raft.md) 가 multi-edge 환경에서
이미 한 일:

```
1. MarshalPutObjectEntry(meta)   ← 메모리에서 entry 만들기, bbolt 안 건드림
2. Elector.ReplicateEntry        ← peers 에 push, quorum ack 대기
3. quorum OK → PutObjectAfterReplicate(meta)   ← 비로소 bbolt commit
4. quorum 못 받음 → 503, leader bbolt 무변화
```

같은 패턴을 coord 에 그대로 가져온다. 새 알고리즘 없음. **위치 이동만**.

## 흐름 비교

### ADR-039 (best-effort, Ep.32):

```
PUT → coord (leader)
      bbolt.PutObject(meta) ← commit FIRST
      WAL.Append → walHook → ReplicateEntry (best-effort)
      return 200/503

failure mode: leadership lost between commit and walHook → entry stranded
```

### ADR-040 (transactional, this ep):

```
PUT → coord (leader)
      MarshalPutObjectEntry(meta) → entry bytes (in memory only)
      Elector.ReplicateEntry(entry) → quorum ack
        if no quorum: return error (bbolt UNTOUCHED)
      bbolt.PutObjectAfterReplicate(meta) ← commit AFTER
      return 200

failure mode: NONE for phantom write — bbolt only touched after quorum confirmed
```

핵심 한 줄: **replicate before commit**. 이 순서 뒤집기가 phantom write window
를 0 으로 만든다.

## 코드 ~30 LOC

`internal/coord/coord.go`:

```go
func (s *Server) commit(ctx context.Context, meta *store.ObjectMeta) error {
    // Prerequisites guard — silent fallback 보다 명시적 fatal at boot
    if !s.TransactionalCommit || s.Elector == nil || s.Store.WAL() == nil {
        return s.Store.PutObject(meta)  // legacy ADR-039 path
    }
    body, err := s.Store.MarshalPutObjectEntry(meta)
    if err != nil {
        return fmt.Errorf("marshal entry: %w", err)
    }
    repCtx, cancel := context.WithTimeout(ctx, transactionalCommitTimeout)
    defer cancel()
    if err := s.Elector.ReplicateEntry(repCtx, body); err != nil {
        return fmt.Errorf("transactional replicate: %w", err)
    }
    return s.Store.PutObjectAfterReplicate(meta)
}
```

`PutObjectAfterReplicate` 가 `appendWALControlled(op, args, fireHook=false)` —
peers 가 이미 entry 받았으니 hook 다시 firing 하지 않게.

## prerequisite mismatch — fatal

`COORD_TRANSACTIONAL_RAFT=1` 켰는데 `COORD_PEERS` 또는 `COORD_WAL_PATH` 미설정?
→ daemon startup fatal. silent fallback 안 함.

```go
if *flagTxnRaft {
    switch {
    case elector == nil:
        fatal("COORD_TRANSACTIONAL_RAFT=1 requires COORD_PEERS")
    case st.WAL() == nil:
        fatal("COORD_TRANSACTIONAL_RAFT=1 requires COORD_WAL_PATH")
    }
    log.Info("kvfs-coord transactional commit (Ep.5, ADR-040)",
        "semantics", "replicate-then-commit; quorum failure → 503 + no local commit")
}
```

config 함정 회피. 운영자가 "transactional 켰다고 생각했는데 알고 보니
best-effort 였다" 사고 방지.

## ה he 데모

```
$ ./scripts/demo-he.sh
==> 3-coord cluster + WAL repl + COORD_TRANSACTIONAL_RAFT=1
    leader = coord1:9000

==> PUT /v1/o/he/v1 (quorum healthy)
    {"ok":true} ✓

==> kill 2 of 3 coords (only coord3 alive)
    sleep past leader lease...

==> PUT /v1/o/he/v2 during 2/3 outage
    HTTP/1.1 503 Service Unavailable
    {"error":"transactional replicate: only 1/3 acks (need 2)"}

==> verify survivor coord3.bbolt unchanged
    object count baseline: 1
    object count during quorum loss: 1   ← unchanged ✓

==> restart coords + wait
    new leader elected
    PUT /v1/o/he/v3 → 200

==> verify v2 was NEVER committed (no phantom)
    GET /v1/o/he/v2 → 404 ✓

=== ה PASS: transactional commit prevents phantom write ===
```

evidence: quorum 못 받은 PUT 은 leader 의 bbolt 도 안 건드림. client 가 503
받은 데이터는 진짜로 존재하지 않음.

이게 [Ep.28](28-strict-vs-txn.md) 에서 그렸던 그림 — strict 모드의 503 받은
client 가 GET 하면 200, transactional 의 503 받은 client 가 GET 하면 404.
**같은 503, 다른 의미**.

## CP 의 비용

이 정합성의 가격:

- **Latency** — 매 PUT 이 quorum RTT 만큼 ↑. 로컬 LAN ~1ms, cross-AZ ~5ms.
  hot path 의 정직한 비용. Hot 데이터 워크로드는 이 latency 가 SLO 깰 수 있음.
- **Availability** — minority partition 의 leader 는 write 거부. 분산 시스템의
  CAP — kvfs 는 transactional 모드에서 CP. best-effort 는 AP. 운영자가 선택.

## chaos 검증 — P8-01 의 첫 finding

[chaos-coord-quorum-loss.sh](../scripts/chaos-coord-quorum-loss.sh) 가 이 ADR
의 invariant 를 자동 검증:

- Phase A: quorum healthy 시 PUT 5/5 OK
- Phase C: 2/3 coord 죽인 후 PUT 5 → **5/5 expected-fail (503)**
- Phase D: survivor bbolt count 변화 0 (drift 0)
- Phase F: post-restore phantom commit 0 (절대 retrievable 하지 않음)

이 chaos test 가 P8-01 첫 시도 때 false-failure 를 잡았다 — stale docker image
가 P7-08 (CoordClient retry) 미반영이라 잘못된 path 로 동작. 이미지 rebuild
후 PASS. 즉 architectural invariant 자체는 holds — chaos suite 가 deployment
hygiene bug 를 catch 한 것.

## "왜 PutObject 만"

ADR-040 의 -항목: `DeleteObject + AddRuntimeDN + RemoveRuntimeDN 미적용`. 이는
ADR-034 의 결정 그대로 — phantom write 가 phantom delete 보다 사용자에게 더
보임. 운영 결정.

후속 ADR 에서 확장 가능. 지금은 PutObject 만으로 production storage 의 가장
중요한 invariant 충족.

## 다음

Ep.34 — placement 결정도 coord 에. ADR-041. 메타에 이어 placement 까지 single
source. demo-vav (ו). edge 의 마지막 distributed-decision 책임이 사라진다.
