# ADR-039 — Coord-to-coord WAL replication (ADR-038 follow-up, gap closer)

상태: Accepted (2026-04-27)
시즌: Season 5 Ep.4 (ADR-038 의 명시적 갭 메움)

## 배경

[ADR-038](ADR-038-coord-ha-via-raft.md) 가 coord 끼리 election 까지만 했다. follower coord 는 mutating RPC 거부 (503 + `X-COORD-LEADER`) 로 leader 일관성 보장 — 그러나 **follower 의 bbolt 는 비어있음**. leader 가 죽고 follower 가 새 leader 가 되면 그 동안의 commit 데이터를 모름. demo-gimel 의 GET 404 가 이 갭을 노출.

ADR-031 follow-up (Season 4 Ep.8) 에서 이미 multi-edge 환경의 leader→follower WAL push 메커니즘을 만들었다 (`Elector.AppendEntryFn` + `Elector.ReplicateEntry` + `Elector.HandleAppendWAL` + `MetaStore.SetWALHook`). coord 에 그대로 가져오면 이 ADR 의 일.

## 결정

`COORD_WAL_PATH` env 가 설정되면 coord daemon 이:

1. `store.OpenWAL(path)` 로 WAL 열고 `MetaStore.SetWAL(wal)` 등록 — leader 가 commit 할 때마다 WAL 에 한 줄씩 append.
2. `Elector.Config.AppendEntryFn = func(body) { entry := decode(body); return ms.ApplyEntry(entry) }` — follower 가 push 받으면 자기 bbolt 에 적용.
3. `MetaStore.SetWALHook(func(line) { if leader { Elector.ReplicateEntry(ctx, line) } else { return nil } })` — leader 가 매 Append 후 quorum 에 push (best-effort: hook 의 error 가 client 503 으로 propagate 됨, ADR-033 패턴 그대로).
4. `coord.Server.Routes()` 가 `POST /v1/election/append-wal` 마운트 — follower 가 push 받는 endpoint.

`COORD_PEERS` 만 설정하고 `COORD_WAL_PATH` 미설정 시 → Ep.3 동작 (election 만, 데이터 sync 0).

## 동작 흐름

```
Client → edge → CoordClient.CommitObject → leader coord
  leader:
    PutObject(meta) → bbolt commit
    appendWAL(...) → wal.Append → fsync(local)
                  → walHook(line) → ReplicateEntry(ctx, line)
                                      ↓ parallel POST to peers
                       follower1: HandleAppendWAL → ApplyEntry(entry) → bbolt commit + wal.Append (no hook re-fire)
                       follower2: HandleAppendWAL → ApplyEntry(entry) → bbolt commit + wal.Append
                                      ↓ ack
                                    quorum check
                                    return nil/err
                  ← walHook returns
    PutObject returns
  ← 200/503 to client
```

핵심 detail:
- **follower 의 ApplyEntry → MetaStore.PutObject** 가 다시 walHook 을 fire 하지 않게 해야 함. 이미 ADR-031 follow-up 에서 `appendWALControlled(op, args, fireHook=false)` 패턴 마련됨. ApplyEntry 가 이 path 사용 → 무한 루프 없음.
- **best-effort vs strict**: 현재 hook 가 error 를 propagate 하지만 strict-by-default 가 좋은지 best-effort 가 좋은지는 운영자 결정. Ep.4 는 best-effort 로 시작 (hook error → client 5xx). 향후 `COORD_STRICT_REPL` env 로 ADR-033/034 패턴 미러링 가능.

## ADR-031 follow-up 과의 차이

기존 (multi-edge):
- 각 edge 가 자기 bbolt + WAL.
- leader edge 가 push, follower edge 가 ApplyEntry.
- single-edge 모드 (default) 는 election 자체가 없음.

이 ADR (multi-coord):
- 각 coord 가 자기 bbolt + WAL.
- leader coord 가 push, follower coord 가 ApplyEntry.
- 메커니즘 100% 동일 — 위치만 edge → coord.

따라서 이 ADR 은 **새 메커니즘 도입이 아니라 location 이동**. 코드 ~50 LOC, ADR 짧음.

## 결과

+ **ADR-038 의 갭 완전 해소**: leader 죽어도 새 leader (= 직전 follower) 가 모든 committed entry 를 가지고 있음. write loss = 0 (best-effort 의 push 직전 leader 죽음 window 제외).
+ **Read availability 확장**: follower coord 도 동일 데이터 → lookup RPC 를 leader 대신 follower 가 답해도 됨. 다만 현재 구현은 coord.Server.handleLookup 에 leader 체크 안 걸어둠 (이미 follower OK) — Ep.3 부터 그렇게 되어 있음. 이 ADR 이후 follower lookup 이 진정 의미 있음.
+ **메커니즘 reuse 의 가치 입증**: ADR-031 + ADR-031 follow-up 의 코드가 별 수정 없이 새 위치에서 작동 — 좋은 abstraction 의 증거.
- **opt-in**: WAL 미설정 시 sync 안 함 (Ep.3 동작). 운영자가 명시적으로 켜야.
- **strict 모드 미구현**: ADR-033 informational strict + ADR-034 transactional Raft 의 coord 버전이 후속 과제 (별도 ADR 또는 본 ADR 의 follow-up).
- **Leader-loss-mid-write divergence (알려진 갭, P6-07)**: 현재 walHook 은 `wal.Append` 직후 = 즉 **bbolt commit 이 끝난 후** 호출. PutObject 호출 시점에 leader 였던 coord 가 commit 도중 leadership 잃으면, hook 안의 `IsLeader()` 체크가 false 라 push 안 됨. 결과: 옛 leader 의 bbolt + WAL 에는 entry 가 있지만 peers 에 도달 못 함 → 새 leader 가 모름 → 일종의 phantom write. ADR-034 의 transactional path (`MarshalPutObjectEntry → ReplicateEntry → PutObjectAfterReplicate`) 를 coord 에 port 하면 해결. P6-07 로 follow-up 예정 (best-effort 모드의 잘 알려진 한계 — quorum 받기 전엔 commit 안 함 패턴).

## 호환성

- `COORD_WAL_PATH` 미설정 시 동작 0 변경 (Ep.3 그대로).
- `coord.Server` API 변경 0 — daemon main 의 wiring 만 추가.
- `internal/election` 변경 0 — 기존 surface 그대로 사용.

## demo

`scripts/demo-dalet.sh` (히브리 ד) — demo-gimel 과 동일 구조 + WAL 활성. 차이점: leader kill 후 새 leader 의 GET 이 **200** 으로 응답 (gimel 은 404 였음). 갭이 사라진 것이 시각적.
