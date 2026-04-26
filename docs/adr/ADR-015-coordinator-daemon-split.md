# ADR-015 — Coordinator daemon 분리 (Proposed, ADR-002 supersede 후보)

상태: **Accepted** (2026-04-26) — Season 5 진입 트리거 채택 완료. **ADR-002 supersede**.

> Proposed 단계에서 본 ADR 본문이 결정 근거를 기록. 사용자 채택 확정으로 Season 5 (분리 트랙) 시작. 첫 episode = `cmd/kvfs-coord/` skeleton.

## 배경

ADR-002 (2-daemon MVP) 은 의도적으로 단순함을 택했다: edge 가 게이트웨이 + 인라인 coordinator. 그러나 Season 4 종료 시점 의 한계가 분명해졌다:

1. **Single-writer 천장** — bbolt 는 한 프로세스 내에서 단일 writer. edge 1 대 = 메타 mutation 직렬. 인라인 fsync 로 ~480 ops/s, group commit (ADR-035) 으로 ~7400 ops/s. 그 위로 못 올라감.
2. **Edge HA 와 coordinator 책임의 충돌** — multi-edge HA (ADR-022) + auto leader election (ADR-031) + transactional Raft (ADR-034) 으로 메타 일관성은 다중화되지만, **placement 결정** 은 여전히 각 edge 가 독립 수행. 두 edge 가 같은 chunk_id 에 대해 **다른 시점의 DN topology** 를 보면 placement 결과 분기 가능 (이론상).
3. **확장성 단방향** — DN 은 horizontal scale (DN 10 → 100), 그러나 edge/coordinator 책임은 그렇지 못함. 더 많은 메타 throughput 이 필요하면 partition (다른 ADR) 또는 책임 분리 둘 중 하나가 필요.
4. **Operational 모호성** — "edge 는 게이트웨이인가 coordinator 인가" 가 코드 곳곳에서 흐려진다. `internal/edge/edge.go` 는 1700+ LOC, handler 와 placement 로직과 rebalance 트리거가 한 파일에. 책임 분리 시 코드도 자연 분리.

## 결정 (제안)

`kvfs-coord` daemon 신설. 책임:

- placement (HRW + class-aware subset)
- write quorum 결정 (R/2+1)
- rebalance / GC / repair worker
- DN heartbeat
- WAL 단일-writer (메타 진실의 원천)

`kvfs-edge` 는 axis-aligned 게이트웨이로 축소:

- HTTP/HTTPS 종단
- UrlKey 검증
- chunker / EC encoder (CPU 작업)
- coord 에 RPC: `Place(chunkID, R) → DN[]`, `Commit(meta) → ok`, `Lookup(b/k) → meta`

```
                  ┌──────────────┐
Client ─HTTP──▶  edge × N         ─RPC▶  coord (HA, Raft) ─▶  bbolt + WAL
                  │                       │
                  └──HTTP REST────────────┴──▶ kvfs-dn × M
```

3-daemon. coord 는 본인끼리 Raft (ADR-034 적용).

## 채택 트리거

- 단일 edge 에서 group commit + transactional Raft 켜고 운영 중인데 메타 mutation throughput 이 응용의 천장이 됐을 때.
- 또는 Season 5 의 명시적 진입 결정 (사용자 디렉티브).

## 비채택 대안

| 대안 | 기각 사유 |
|---|---|
| **bbolt → SQLite-WAL/Postgres** | "외부 의존 최소" 원칙 위반. bbolt 단일 파일의 backup/restore 단순함도 잃음 |
| **bbolt sharding by bucket** | 클라이언트가 routing 책임 가짐. 새 bucket 마다 shard 결정. 운영 복잡도 ↑↑ |
| **edge 들이 직접 quorum (etcd-style)** | kvfs 가 의존하는 etcd-class 의존성을 자체 구현하는 셈. ADR-031 의 Raft 부분만으로는 메타 트랜잭션 lock 부족 |
| **Status quo + 더 나은 batching** | 이미 ADR-035 (group commit) + ADR-034 (transactional Raft) 가 채워둠. 추가 limit 돌파에는 분리 필요 |

## 결과 (제안 채택 시)

+ Edge horizontal scale 가능 — chunker/EC CPU bound 작업이 분리됨.
+ Placement 일관성 강화 — coord 가 DN topology snapshot 의 단일 출처.
+ 코드 책임 명확화 — edge.go 1700 → ~700 LOC 추정.
+ Future: coord daemon 자체의 partition/shard 도 자연스럽게.
- **운영 복잡도 ↑↑** — daemon 종류 2 → 3. systemd unit, health, deploy 모두 ×3/2.
- **Latency overhead** — 매 PUT/GET 이 edge↔coord RPC 1회 추가 (loopback ~50µs, cross-host ~1ms).
- **첫 시즌 (S1) 의 단순함 신화 종료** — kvfs 의 매력 일부였던 "2 daemon" 슬로건 폐기.

## 호환성

- ADR-002 supersede. 모든 데이터 모델 (ObjectMeta · ChunkRef · Stripe · WAL) 그대로 — coord 가 owner 가 됨.
- 기존 `EDGE_*` 환경 변수 일부는 `COORD_*` 로 이전 (placement, rebalance, GC 관련). chunker/HTTP 는 edge 에 잔류.
- 첫 마이그레이션: 단일 coord = single-edge 와 동등. 점진적 다중화.

## 시작 episode (Season 5 Ep.1)

`cmd/kvfs-coord/` skeleton — minimal viable separation:
- coord daemon HTTP server (`:9000`)
- 첫 RPC 두 개: `POST /v1/coord/place` (chunkID, n → DN list), `POST /v1/coord/commit` (ObjectMeta → ok)
- edge 가 `EDGE_COORD_URL` 설정되면 RPC, 미설정이면 인라인 (backward compat)
- 데모 `scripts/demo-aleph.sh` (Hebrew letters for Season 5 — Greek alpha-omega 소진)

이후 episodes:
- Ep.2 — coord 자체의 Raft (peer set, leader election 재사용)
- Ep.3 — coord daemon 간 메타 sync (WAL replication 재사용)
- Ep.4 — edge 의 placement 코드 완전 제거, coord 만 의사결정
- Ep.5 — kvfs-cli 가 coord 직접 admin

---

연결: [P5-03](../FOLLOWUP.md) — Accept (2026-04-26). [P6-01](../FOLLOWUP.md) — Season 5 Ep.1 시작.
