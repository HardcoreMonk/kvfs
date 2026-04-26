# Episode 28 — Strict vs Transactional: 같은 모양의 두 다른 보장

> **Series**: kvfs — distributed object storage from scratch
> **Season**: P4-09 wave · **Episode**: 15
> **연결**: [ADR-033](../docs/adr/ADR-033-strict-replication.md) · [ADR-034](../docs/adr/ADR-034-transactional-raft.md)

---

## 같은 503, 다른 의미

```
EDGE_STRICT_REPL=1            → quorum 실패 시 503
EDGE_TRANSACTIONAL_RAFT=1     → quorum 실패 시 503
```

503 응답은 같다. 그러나 **그 직전 leader 의 디스크 상태** 가 정반대.

| 모드 | quorum 실패 시 leader bbolt | client 가 다음 GET 하면 |
|---|---|---|
| best-effort (default) | committed | 200 (그 데이터 보임) |
| **strict** (ADR-033) | committed | 200 (그 데이터 보임) |
| **transactional** (ADR-034) | NOT committed | 404 |

strict 와 best-effort 는 **client 응답 만 다름**. transactional 은 진짜로 다른 의미체계.

이 글이 그 차이를 풀어쓰고, 언제 어느 걸 쓸지 가이드한다.

## Best-effort 의 흐름

[Ep.20 sync replication](20-sync-replication.md):

```
client → leader: PUT
  leader bbolt commit
  WAL.Append              ← hook fires here
    hook: peer push parallel
      ack received from peer1, peer2 → quorum OK → hook returns nil
      ack failure → hook returns error (BUT WE IGNORE IT in best-effort)
  PutObject() returns nil
client ← 200
```

quorum 못 받아도 200. 운영자는 모름. follower 가 다음 snapshot pull 또는 WAL pull 때 catch up — eventual consistency.

## Strict 의 흐름 (ADR-033)

```
client → leader: PUT
  leader bbolt commit
  WAL.Append
    hook: peer push parallel
      quorum failure → hook returns error
  PutObject() returns the hook's error  ← 차이점
client ← 503
```

**그러나 leader 의 bbolt 에는 이미 commit 됐다**. 503 받은 client 가 GET 하면 그 데이터 보임.

이게 "informational strict" 라 불리는 이유. 503 은 client 에게 "조심해, 이 PUT 이 quorum 까지 못 갔어" 라는 시그널이지 "데이터 없다" 가 아님. ADR-033 본문이 명시.

쓰임: client 가 503 받으면 retry. retry 가 quorum 으로 가면 그제서야 진짜 200. 일종의 **back-pressure**.

## Transactional 의 흐름 (ADR-034)

```
client → leader: PUT
  leader: MarshalPutObjectEntry(meta)  ← 메모리에서만 entry 만들기
  Elector.ReplicateEntry(entry_body)
    parallel push to peers
    wait for quorum ack
    quorum OK → returns nil
    quorum failure → returns error
  if err: return 503 to client; LEADER bbolt NOT touched
  if OK: PutObjectAfterReplicate(meta)  ← 비로소 leader bbolt commit
client ← 200 (or 503)
```

**replicate first, commit second**. 503 받은 client 가 GET → 404. 데이터가 진짜로 없음.

leader 가 그 직후 죽어도 quorum-acked entries 는 followers 에 살아있음. 새 leader 가 이어받음. write loss = 0.

## 주요 차이 요약

|  | best-effort | strict (ADR-033) | transactional (ADR-034) |
|---|---|---|---|
| quorum 실패 시 leader bbolt | committed | committed | not committed |
| client 응답 (실패 시) | 200 | 503 | 503 |
| 503 client 가 GET 시 | 200 | 200 | 404 |
| Write loss window | 있음 (push 직후 leader 죽으면) | 있음 | **없음** |
| Latency overhead | 0 (hook async 가능) | hook RTT 만 | hook RTT — full Raft round trip |
| Implementation 어려움 | 낮음 | 낮음 (best-effort 에 503 만 추가) | **중상** (commit 순서 뒤집기, hook suppression) |

## 코드 차이 핵심

`internal/edge/edge.go`:

```go
func (s *Server) commitPutMeta(ctx, meta) error {
    // Transactional path
    if s.StrictReplication && s.Elector != nil && s.Elector.IsLeader() {
        body, _ := s.Store.MarshalPutObjectEntry(meta)
        if err := s.Elector.ReplicateEntry(ctx, body); err != nil {
            return fmt.Errorf("strict replicate: %w", err)
        }
        return s.Store.PutObjectAfterReplicate(meta)
    }
    // Best-effort + (informational strict if EDGE_STRICT_REPL): 그냥 PutObject.
    // Strict 모드는 hook 의 error 가 PutObject 의 return 으로 propagate 됨.
    return s.Store.PutObject(meta)
}
```

`Store.PutObjectAfterReplicate` 는 `appendWALControlled(op, args, false)` 호출 — hook **suppressed** 이라 peer 에 다시 push 안 함 (이미 했으니까). 이 detail 이 ADR-034 의 핵심.

## 어느 걸 쓰나

```
이 데이터가 사라지면 어떻게 되나?
├ "괜찮음, 다시 만들 수 있음" (캐시, 로그, metric)
│  → best-effort (default)
├ "곤란하지만 client 가 retry 하면 됨" (일반 운영 트래픽)
│  → strict (informational, simpler ops)
├ "절대 사라지면 안 됨" (금융, 결제, 감사 로그)
│  → transactional Raft
└ "RPO 분 단위 OK" (백업)
   → snapshot only (Ep.10), strict 도 transactional 도 불필요
```

Strict 는 "transactional 에 도달하기 전 단계" 같은 느낌이지만 사실은 다른 용도. **strict 는 client 의 retry 정책 trigger 용** (back-pressure), **transactional 은 진정한 commit guarantee**.

## "왜 두 개를 다 만들었나"

처음엔 strict 만 있었다 (ADR-033). 그 다음 strict 의 한계 — "503 받았는데 데이터는 commit 됐다" 는 의미 분기 — 가 명확해져서 transactional (ADR-034) 추가. ADR-033 은 polite한 안내, ADR-034 은 진짜 보장.

둘이 공존하는 이유: 운영자가 명시적으로 선택. 둘 다 false (default) → best-effort. strict only (`EDGE_STRICT_REPL=1`) → 503 시 retry 권장. transactional (`EDGE_TRANSACTIONAL_RAFT=1`, strict 자동 함의) → commit-before-quorum.

## P4-09 wave 의 끝

P4-09 (Season 4 close 후 wave) 가 다룬 6 candidate:
- ✅ Prometheus metrics ([Ep.23](23-metrics.md))
- ✅ SIMD-style RS ([Ep.24](24-simd-rs.md))
- ✅ Hot/Cold tier ([Ep.25](25-hot-cold-tier.md)) — P5-04 rebalance 통합 포함
- ✅ NFS deferred ([Ep.26](26-nfs-deferred.md)) — 안 한 결정도 결정
- ✅ WAL log compaction ([Ep.27](27-log-compaction.md))
- ✅ Strict vs transactional (이 글)

S5 진입 (`kvfs-coord` daemon, [Ep.29](29-coord-skeleton.md) 가 시작) 직전. 본 wave 가 운영성·일관성 의 미세 튜닝을 마무리.

## 다음

Season 5 시작. ADR-015 Accept. coord daemon 분리 — kvfs 의 두 번째 architectural pivot (첫 번째 = ADR-009 HRW + ADR-008 EC 의 distributed primitives).
