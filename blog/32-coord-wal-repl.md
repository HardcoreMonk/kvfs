# Episode 32 — coord 간 WAL replication

> **Series**: kvfs — distributed object storage from scratch
> **Season**: 5 (coord 분리) · **Episode**: 4
> **연결**: [ADR-039](../docs/adr/ADR-039-coord-wal-replication.md) · [demo-dalet](../scripts/demo-dalet.sh)

---

## 갭 메움

[Ep.31](31-coord-ha.md) 의 마지막 화면은 의도된 실패였다:

```
==> PUT /v1/o/gimel/v1 (coord1 leader)
==> kill coord1 → coord2 new leader
==> GET /v1/o/gimel/v1 → 404
```

leader 죽으면 새 leader 가 옛 데이터 모름. ADR-038 명시적으로 "이 ADR 은
election 만" 이라 기록. 이 ep 가 그 갭의 closer.

## 100% reuse

S4 Ep.8 (ADR-031 follow-up) 에서 이미 만든 것:

```go
type Elector struct {
    AppendEntryFn AppendEntryFunc   // follower 가 받은 entry 처리
    ReplicateEntry(ctx, body) error // leader 가 quorum push
    HandleAppendWAL(...)            // /v1/election/append-wal endpoint
}
```

multi-edge HA 에서 leader→follower WAL push 하던 그 메커니즘. coord 에 그대로
가져온다. **새 메커니즘 0줄**.

```go
// cmd/kvfs-coord/main.go
if st.WAL() != nil {
    appendFn = func(entryBody []byte) error {
        var entry store.WALEntry
        json.Unmarshal(entryBody, &entry)
        return st.ApplyEntry(entry)
    }
}
elector = election.New(election.Config{ ..., AppendEntryFn: appendFn })

// leader 가 commit 할 때마다 hook 으로 push
st.SetWALHook(func(line []byte) error {
    if !elector.IsLeader() { return nil }
    return elector.ReplicateEntry(ctx, line)
})
```

50 LOC 정도. ADR-039 본문이 짧은 이유.

## 동작 흐름

```
client → edge → coord (leader)
  PutObject(meta) → bbolt commit
  appendWAL(line) → fsync + walHook
                  └→ ReplicateEntry parallel push
                         ↓                ↓
                     follower1.        follower2.
                     ApplyEntry(entry) ApplyEntry(entry)
                     bbolt commit      bbolt commit
                  ← quorum ack
client ← 200
```

leader 가 WAL append 하는 모든 entry 가 즉시 followers 에 push. quorum acked
면 client 에 200, 못 받으면 best-effort 로 5xx.

## 무한 루프 방지

follower 가 push 받아 ApplyEntry → MetaStore.PutObject → walHook 다시 fire ?

이 무한 루프는 [Ep.20 sync replication](20-sync-replication.md) 에서 이미
해결됨: `appendWALControlled(op, args, fireHook=false)`. follower path 는 hook
suppression 모드로 PutObject. 메커니즘이 그대로 coord 로 이전.

## ד dalet 데모

```
$ ./scripts/demo-dalet.sh
==> 3-coord cluster + WAL replication
    leader = coord1:9000

==> PUT /v1/o/dalet/v1 (pre-failover)
{"bucket":"dalet","key":"v1","size":15}

==> verify all 3 coord bbolts have it
    coord1.db: ✓
    coord2.db: ✓
    coord3.db: ✓

==> kill leader coord1 → election
    new leader = coord2:9001

==> GET /v1/o/dalet/v1 (after failover)
    "pre-failover write"        ← demo-gimel 은 404 였음
    GET → 200 ✓

=== ד PASS: pre-failover writes survive across leader change ===
```

evidence: 모든 3 coord 의 bbolt 가 같은 데이터. leader 죽어도 새 leader 가
이미 가지고 있음. demo-gimel 의 404 가 사라짐.

## 알려진 갭 — leader-loss-mid-write

ADR-039 본문에 명시된 -항목:

> 현재 walHook 은 wal.Append 직후 = 즉 bbolt commit 이 끝난 후 호출. PutObject
> 시점에 leader 였던 coord 가 commit 도중 leadership 잃으면, hook 안의
> IsLeader() 체크가 false 라 push 안 됨. 결과: 옛 leader 의 bbolt + WAL 에는
> entry 가 있지만 peers 에 도달 못 함 → phantom write.

```
t=0   coord1 leader. client PUT 도착.
t=1   coord1: PutObject(meta) → bbolt commit + WAL append
t=2   coord1 의 leadership lost (term ↑, election fired)
t=3   walHook fire: IsLeader() → false → 그냥 return (push 안 함)
t=4   client: 5xx
t=5   새 leader (coord2): bbolt 에 t=1 entry 없음
       옛 leader (coord1): bbolt 에 t=1 entry 있음
```

rare race window. 하지만 가능. 이게 best-effort 의 본질적 한계 — quorum 도달
**전에** local commit 함.

해결: ADR-040 의 transactional path. **commit-before-quorum** 을 **replicate-
before-commit** 으로 뒤집기. Ep.33 에서 다룬다.

## 메커니즘 reuse 의 가치 입증

ADR-039 본문이 자랑한다:

> ADR-031 + ADR-031 follow-up 의 코드가 별 수정 없이 새 위치에서 작동 — 좋은
> abstraction 의 증거.

이게 production 시스템 개발의 미덕. **한 번 잘 만든 메커니즘이 새 도메인에서
재사용**. multi-edge HA 와 multi-coord HA 가 같은 코드를 공유.

이런 reuse 가 가능한 이유: ADR-031 의 Elector 가 자기 도메인 (election +
log replication) 에 충실, application 도메인 (어느 daemon 인지) 에 비독립. 좋은
abstraction.

## opt-in 두 가지

```
COORD_PEERS=...                              # election 만 (Ep.3 동작)
COORD_PEERS=... COORD_WAL_PATH=/path/wal     # election + sync (이 ep)
```

WAL 경로 설정으로 두 모드 분리. 점진 도입 — 운영자가 원할 때 sync 켤 수 있게.

## 다음

Ep.33 — ADR-040 transactional commit. ADR-039 의 phantom-write window 메움.
ADR-034 (multi-edge transactional Raft) 를 coord 로 port. `replicate first,
commit second`. demo-he (ה).

이게 S5 의 일관성 모델 정점 — write loss 0 보장.
