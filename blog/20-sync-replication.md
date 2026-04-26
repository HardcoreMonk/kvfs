# Episode 20 — Sync Raft-style WAL replication

> **Series**: kvfs — distributed object storage from scratch
> **Season**: 4 (performance/efficiency) · **Episode**: 7 (Ep.3 election + Ep.4 WAL follow-up)
> **ADR**: [031](../docs/adr/ADR-031-auto-leader-election.md) follow-up · **Demo**: `./scripts/demo-omega.sh`

---

## 지금까지의 RPO 단계

| 메커니즘 | RPO 상한 | 출처 |
|---|---|---|
| 단일 edge + manual snapshot | 운영자 호출 간격 | [Ep.10](10-meta-backup.md) |
| auto snapshot (1h) | 1h | [Ep.12](12-auto-snapshot.md) |
| follower snapshot pull (30s) | 30s | [Ep.13](13-multi-edge-ha.md) |
| follower WAL pull (5s) | 5s | [Ep.17 후속](17-wal.md) |
| sync push (이 ep) | **~ms** (network RTT) | 이 ep |

목표 RPO = network RTT. leader 가 매 WAL Append 마다 followers 에게 즉시 push.

## 새 RPC: append-wal

`POST /v1/election/append-wal?term=<X>` body = 단일 WALEntry (raw JSON). follower 가:

1. term 검증 (stale leader 거부)
2. `MetaStore.ApplyEntry(entry)` — 자기 bbolt 에 mutation 적용
3. 200 OK

leader 측 hook:

```go
ms.SetWALHook(func(entryJSON []byte) error {
    // leader 만 push (follower 는 hook 비활성)
    if elector.State() != election.StateLeader { return nil }
    return elector.ReplicateEntry(ctx, entryJSON)
})
```

`ReplicateEntry` 는 모든 peer 에 parallel POST. quorum (R/2+1) ack 받으면 nil, 못 받으면 error. 여기서는 best-effort: 실패해도 leader 는 이미 local commit. follower 가 lagging 됐을 뿐.

## 왜 hook 가 적당한가

WAL.Append 가 `(seq, line, error)` 를 돌려준 이래 ([Ep.17](17-wal.md)), hook 가 line 을 byte-identical 로 받을 수 있다. 즉 hook 는 "방금 디스크에 쓴 그 bytes 를 그대로" peer 에 던진다. 재-marshal 0, timestamp drift 0.

```go
func (w *WAL) Append(op string, args any) (int64, []byte, error) {
    ...
    line, _ := json.Marshal(entry)
    w.w.Write(line); w.w.WriteByte('\n')
    if w.batchInterval == 0 {
        w.w.Flush(); w.f.Sync()  // inline durable
        return seq, line, nil
    }
    // batched: park on cond until durable
    ...
    return seq, line, nil
}

func (m *MetaStore) appendWAL(op WALOp, args any) error {
    _, line, err := m.wal.Append(string(op), args)
    if err != nil || line == nil { return err }
    if m.walHook != nil { return m.walHook(line) }
    return nil
}
```

hook 는 Append 가 끝난 직후 (즉 leader 의 디스크에 들어간 직후) 동기적으로 실행. peer 가 응답하면 PutObject 가 반환. 응답 못 오면 client 가 어떻게 보느냐는 옵션 — 아래 "엄격 모드" 참조.

## Failover 의 효과

leader L 이 push 한 entry 가 followers F1, F2 에 적용된 직후 L 이 죽었다고 하자.
- ADR-031 election: F1 또는 F2 가 새 leader 가 됨 (term + 1).
- 새 leader 의 bbolt 는 직전 push 까지 모두 반영됨 — **client 가 보낸 마지막 PUT 까지 보존**.
- write loss window = leader 가 push 한 직후 ~ peer 가 ApplyEntry 직전 사이의 (network + bbolt write) 시간.

snapshot interval (1h) 와 비교하면 6 자리 수 줄어듦.

## 엄격 모드 (informational)

`EDGE_STRICT_REPL=1` 켜면 hook 의 error 가 client 까지 올라감 (503). 그러나 **bbolt commit 은 이미 했으므로** 진정한 commit-before-quorum 은 아님. 단순히 운영자에게 "이 PUT 이 quorum 까지 못 갔다" 는 시그널. 진짜 commit-before-quorum 은 [Ep.21 transactional Raft](21-transactional-raft.md) 가 다룬다.

## 데모

3-edge cluster + 4 DN. `EDGE_PEERS=edge1,edge2,edge3` 로 election + sync push 활성.

```
$ ./scripts/up-3edge.sh && sleep 5
$ EDGE=$(./scripts/find-leader.sh)  # /v1/admin/role 로 leader 발견
$ FOLLOWER=$(./scripts/find-follower.sh)

$ curl -X PUT -H "Content-Type: text/plain" \
    --data-binary "hello sync" \
    "http://$EDGE:8000/v1/o/b/sync-test?sig=...&exp=..."

$ sleep 0.1   # network RTT 정도만 기다리고

$ curl "http://$FOLLOWER:8000/v1/o/b/sync-test?sig=...&exp=..."
hello sync   # ← snapshot pull 안 했는데도 follower 가 가지고 있음
```

이전엔 `sleep 30` (snapshot pull interval) 또는 `sleep 5` (WAL pull interval) 가 필요했음. 이제는 RTT 만.

## 트레이드오프

- **Latency**: PUT 이 hook 의 quorum ack 까지 기다림. client-perceived latency 가 RTT × 1 만큼 증가. 5-edge cluster 기준 ~1 ms (loopback) ~ 5 ms (cross-host).
- **Failure mode**: peer 가 모두 죽으면 hook fail → strict 모드면 503, best-effort 모드면 무시. peer 가 partition 되면 leader 가 그 사실을 모를 수 있음 (그래서 election 의 heartbeat 가 별도로 detect).
- **Throughput**: parallel push 라 N peer 가 늘어도 latency 는 거의 일정 (slowest peer 가 bottleneck).

## 한계

- "best-effort" 라는 표현이 이미 답한다: leader local commit 은 무조건 일어난다. peer 가 실패해도 client 는 데이터 잃지 않음 — 그러나 leader 가 직후 죽으면 그 entry 는 잃을 수 있음 (write loss window).
- 그 window 자체를 0 으로 만들려면 commit 순서를 뒤집어야 함 → [Ep.21](21-transactional-raft.md).

## 다음

[Ep.21](21-transactional-raft.md): "replicate-then-commit" — 진짜 transactional Raft. quorum ack 받기 전엔 local commit 자체를 안 함. 가용성 ↓, 일관성 ↑.
