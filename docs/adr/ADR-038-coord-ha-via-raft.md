# ADR-038 — Coord HA via Raft (election + leader-redirect, ADR-031 reuse)

상태: Accepted (2026-04-27)
시즌: Season 5 Ep.3 (ADR-015 follow-up)

## 배경

ADR-015 (S5 Ep.1) 가 coord 를 별도 daemon 으로 분리. ADR-002 의 단일 edge SPOF 는 깨졌지만 새로 만든 coord 가 그 자리에 들어왔다 — single coord = 새로운 SPOF.

해결 방향은 명확: ADR-031 의 election 을 그대로 coord 에 가져오면 된다. coord 끼리 peer 들어 leader election. write 는 leader 만 받음. follower 가 받으면 거부 + leader 위치 알려줌.

## 결정

1. **`internal/election` 재사용**. 새 state machine 없음. coord 가 자기 peers 로 `election.New()` 호출, `Run(ctx)` 시작. RPC 경로 `/v1/election/{vote,heartbeat}` 가 coord 의 ServeMux 에 마운트 (edge 와 다른 포트라 conflict 없음).

2. **새 redirect 헤더 `X-COORD-LEADER`**. follower coord 가 mutating RPC (commit/delete) 받으면 503 + 이 헤더 = leader URL. ADR-022 의 `X-KVFS-Primary` 와 동등 패턴.

3. **edge.CoordClient 가 transparent retry**. 503 + X-COORD-LEADER 보면 BaseURL 갱신 후 한 번 재시도. `MaxLeaderRedirects=1` (default). caller 는 단일 호출로 보임.

4. **읽기 (lookup) + healthz + place 는 follower 도 응답**. write 만 leader-only.

5. **opt-in**. `COORD_PEERS` 미설정 시 single-coord 모드 (Ep.1/Ep.2 동작 그대로).

## 명시적 비포함 (= P6-04)

> **이 ADR 은 election + redirect 만**. coord 간 메타 sync (WAL replication) 는 P6-04 / 후속 ADR 의 책임.

따라서 Ep.3 만으로는:
- leader coord 가 commit 받음 → bbolt 에 씀
- leader 죽음 → 다른 coord 가 새 leader 됨
- 새 leader 의 bbolt 는 **비어있음** → 그 동안 쓴 데이터 안 보임

데모 (`demo-gimel.sh`) 가 election 동작을 보여주지만 데이터 손실 갭도 명시. P6-04 가 WAL push (ADR-031 follow-up Ep.8 의 그 hook) 를 coord 간에 적용해서 갭 메움.

이 분할은 의도적: election 메커니즘 자체의 정확성 ↔ 데이터 sync 정확성을 따로 검증하기 위함.

## 비채택 대안

| 대안 | 기각 사유 |
|---|---|
| follower 가 write 를 leader 로 internally proxy | 추가 RTT (follower → leader → DN). 클라이언트가 직접 leader 로 가는 게 빠르고 명확 |
| edge.CoordClient 가 모든 coord URL 알기 + healthz polling | edge 가 coord topology 알아야 → coupling. redirect 헤더가 더 단순 |
| Raft log replication 까지 한 ADR 에 묶기 | scope 폭증. election 자체의 검증 가능한 단계 잃음 |

## 결과

+ coord 자체의 SPOF 해결 — single coord 모드와 HA 모드 둘 다 운영 가능
+ election 의 correctness 가 P4-09 wave (ADR-031, demo-upsilon) 로 이미 검증됨 — Ep.3 는 mounting + redirect 패턴 추가만
+ edge 측 변경 = 한 함수 (CoordClient.do) 안의 redirect loop. caller 영향 0
- **데이터 sync 갭** — P6-04 까지는 leader 교체 시 새 leader 가 옛 데이터 모름 (의도된 stage)
- 운영자가 COORD_PEERS / COORD_SELF_URL 둘 다 정확히 설정해야 함 (mismatch 시 fatal)
- 503 redirect loop 한 번 = 추가 RTT 1회 (write latency 약간 ↑)

## 호환성

- `COORD_PEERS` 미설정 시 동작 0 변경 (Ep.1/Ep.2 그대로)
- `coord.Server.Elector` field 추가 — nil 이면 single-coord 모드
- `X-COORD-LEADER` 는 새 헤더, 기존 클라이언트가 무시해도 단지 503 으로 보임 (forward compat)

## 후속

- **ADR-039 (P6-04)**: coord 간 WAL replication. ADR-031 의 `AppendEntryFn` + `ReplicateEntry` 가 coord 의 `MetaStore.ApplyEntry` 에 연결. quorum-acked write 만 leader bbolt commit.
- 그 후: edge 의 placement 코드 완전 제거 (P6-05).
