# Episode 21 — Transactional Raft: commit-before-quorum

> **Series**: kvfs — distributed object storage from scratch
> **Season**: 4 (performance/efficiency) · **Episode**: 8 (post-Ep.20 strict)
> **ADR**: [034](../docs/adr/ADR-034-transactional-raft.md) · **Demo**: env `EDGE_TRANSACTIONAL_RAFT=1`

---

## Ep.20 가 아직 잃을 수 있는 것

[Ep.20 sync replication](20-sync-replication.md) 의 흐름:

```
1. client → leader: PUT
2. leader: bbolt commit + WAL.Append (이미 local 디스크)
3. hook: peer push (parallel)
4. quorum ack 받으면 → leader 가 client 에게 200
5. quorum 못 받으면 → strict 모드면 503, best-effort 면 200 (drift 가능)
```

**문제 시점**: 2 직후, 3 가 끝나기 전에 leader 가 죽으면? local 에는 entry 가 있는데 peer 들엔 없음. 새 leader 는 그 entry 를 모르고, 그 entry 가 쓴 객체를 client 가 GET 하면 404.

이게 "write loss window" — leader 가 commit 하고 push 사이의 ms 단위 구간.

## 순서를 뒤집는다

진정한 Raft 의 답: **replicate first, commit second**.

```
1. client → leader: PUT
2. leader: WAL entry 를 메모리에 만들기 (아직 디스크 안 씀)
3. leader → peers: parallel push (이 entry 받아서 디스크에 적용해 둬)
4. quorum ack 받으면 → leader 가 자기 bbolt 에 commit
5. ack 못 받으면 → 503, leader 도 commit 안 함
```

이제 client 가 200 을 받았다는 것은:
- leader 디스크에 있고
- ≥ R/2+1 peer 디스크에 있음.

leader 가 그 직후 죽어도 새 leader (peer 중 하나) 는 그 entry 를 가지고 있다. write loss = **0**.

## 구현 핵심: 두 단계 commit

`internal/store/store.go`:

```go
// MarshalPutObjectEntry: entry JSON 만 만들기 (디스크/메모리 영향 0).
func (m *MetaStore) MarshalPutObjectEntry(obj *ObjectMeta) ([]byte, error) {
    rawArgs, _ := json.Marshal(obj)
    entry := WALEntry{
        Op: string(OpPutObject),
        Args: rawArgs,
        Timestamp: time.Now().UTC(),
        // Seq 는 0 — followers 는 op+args 만 본다, leader local seq 는 commit 시 부여.
    }
    return json.Marshal(entry)
}

// PutObjectAfterReplicate: WAL hook 을 suppress 하고 bbolt commit.
func (m *MetaStore) PutObjectAfterReplicate(obj *ObjectMeta) error {
    return m.putObjectInternalFull(obj, true /*writeWAL*/, false /*fireHook*/)
}
```

edge handler:

```go
func (s *Server) commitPutMeta(ctx context.Context, meta *store.ObjectMeta) error {
    if s.StrictReplication && s.Elector != nil && s.Elector.IsLeader() {
        body, err := s.Store.MarshalPutObjectEntry(meta)
        if err != nil { return err }
        if err := s.Elector.ReplicateEntry(ctx, body); err != nil {
            return fmt.Errorf("strict replicate: %w", err)  // → 503
        }
        return s.Store.PutObjectAfterReplicate(meta)
    }
    return s.Store.PutObject(meta)  // 기존 path
}
```

`fireHook=false` 가 핵심: peer push 가 이미 되었으므로 local Append 의 hook 은 다시 push 하지 않는다. 안 그러면 같은 entry 가 두 번 push 됨.

## ApplyEntry 의 멱등성

peer 가 같은 entry 를 두 번 받아도 (network retry 등) 안전해야 함. `ApplyEntry` 가 PutObject 를 부르므로 — bbolt 는 같은 key 에 같은 value 를 쓰는 것에 무관심. 같은 obj.Bucket+obj.Key+obj.Version 이면 효과 동일. 멱등.

## 가용성 비용

```
peers: edge1 (leader), edge2, edge3
edge3 가 partition. edge1 ↔ edge2 만 통신 가능.
quorum = ⌊3/2⌋+1 = 2. edge1 + edge2 = 2 → quorum OK. PUT 진행.

만약 edge2 도 partition 되면: edge1 만 살아있음. quorum = 2, ack = 1 → 503.
```

→ minority partition 에 들어간 leader 는 **write 거부**. CP 시스템 (Consistency over Availability).

best-effort sync (Ep.20) 와 비교: best-effort 는 같은 상황에서 leader 가 200 응답 후 자기 bbolt 에만 commit. partition 회복 시 conflict resolution 필요 (지금은 없음 — last-write-wins by Version).

## 실측

```
$ EDGE_TRANSACTIONAL_RAFT=1 EDGE_PEERS=... ./scripts/up-3edge.sh
$ for i in $(seq 1 100); do
    curl -X PUT --data "v$i" "http://leader:8000/v1/o/b/k$i?sig=...&exp=..."
  done
$ sleep 0.5

$ # leader kill
$ docker kill edge1
$ sleep 5  # election timeout

$ # 새 leader 에서 모든 100 entries 살아있는지 확인
$ for i in $(seq 1 100); do
    curl -s "http://newleader:8000/v1/o/b/k$i?sig=...&exp=..." > /dev/null \
      || echo "MISSING k$i"
  done | wc -l
0   # ← 0 missing. 모든 entry 가 quorum 에 도달했었음.
```

비교: best-effort 모드로 같은 시나리오 → 마지막 ~3 entries 가 missing 가능성 (write loss window).

## 비용

- **Latency**: PUT 이 quorum ack 까지 1 RTT 추가 — 즉 `sequential bbolt commit + 1 RTT` → `1 RTT + bbolt commit`. quorum 후 commit 이라 RTT 가 critical path. local commit 은 fast SSD 면 ~50 µs, network RTT 는 ~ms. ~10× latency 증가.
- **Throughput**: replicate 가 parallel 이라 cluster 가 4-edge → 5-edge 가도 throughput 큰 변화 없음. quorum 사이즈만 조금 늘어남.
- **운영 복잡도**: peer health 가 서비스 가용성을 직접 결정. 한 peer down 은 OK, 둘 down 은 write 정지.

## 언제 켜나

| 시나리오 | 권장 |
|---|---|
| 캐시·로그·metric — 잃어도 OK | best-effort (default) |
| 트래픽 적당, RPO 분 단위 OK | snapshot pull (Ep.13) |
| 트래픽 많고 RPO 초 단위 필요 | sync push (Ep.20, default 모드) |
| **금융/결제/감사 — RPO 0 필수** | **transactional Raft (이 ep)** |

## 한계

- **PutObject 만** — DeleteObject, AddRuntimeDN 은 여전히 best-effort. 우선 PutObject 만 적용한 이유: 데이터 손실 = 사라진 객체 가 가장 사용자에게 보이는 형태. delete 는 idempotent 한 측면이 있음.
- **bbolt single-writer** 는 그대로. coord daemon 분리 ([ADR-015 Proposed](../docs/adr/ADR-015-coordinator-daemon-split.md)) 시 quorum scope 도 함께 재설계 필요.

## 다음

[Ep.22](22-micro-opts.md): 큰 architectural 결정이 끝났으니 이제 **micro-optimization 묶음** — group commit, 3-region CDC, sync.Pool. 작지만 throughput 곡선의 한 칸 한 칸을 끌어올린다.
