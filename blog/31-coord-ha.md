# Episode 31 — coord 끼리 Raft

> **Series**: kvfs — distributed object storage from scratch
> **Season**: 5 (coord 분리) · **Episode**: 3
> **연결**: [ADR-038](../docs/adr/ADR-038-coord-ha-via-raft.md) · [demo-gimel](../scripts/demo-gimel.sh)

---

## 새로 생긴 SPOF

Ep.29 (ADR-015) 이 edge 의 SPOF 를 깼다 — 그러나 그 자리에 coord 가 들어왔다.
single coord = 새로운 SPOF.

```
S1~S4: edge SPOF
S5 Ep.1~2: coord SPOF (메타 owner, 한 대)
S5 Ep.3: coord 도 다중화. ADR-038
```

해결 방향은 너무 명확. ADR-031 (Ep.16) 의 election 을 그대로 가져오면 된다.

## 메커니즘 reuse 의 가치

`internal/election` 은 이미 production-grade. 3-state machine, term-based
voting, randomized election timeout — 같은 패턴을 또 짜는 건 낭비.

```go
// cmd/kvfs-coord/main.go
elector = election.New(election.Config{
    SelfID:        *flagSelfURL,
    Peers:         peers,
    AppendEntryFn: nil,  // Ep.3 만에는 없음, Ep.4 가 채움
})
go elector.Run(ctx)
```

새 state machine 0줄. coord.Server 가 elector 를 들고만 있을 뿐.

## leader-redirect 헤더

새로운 wire-level 약속: `X-COORD-LEADER`. follower coord 가 mutating RPC 받으면
503 + 이 헤더 = leader 의 URL.

```go
func (s *Server) requireLeader(w http.ResponseWriter) bool {
    if s.Elector == nil { return false }       // single-coord 모드
    if s.Elector.IsLeader() { return false }   // 내가 leader, OK
    if leader := s.Elector.LeaderURL(); leader != "" {
        w.Header().Set(HeaderCoordLeader, leader)
    }
    writeErr(w, http.StatusServiceUnavailable, errors.New("not the coord leader"))
    return true
}
```

ADR-022 의 `X-KVFS-Primary` 와 동등 패턴. 이 패턴이 kvfs 의 HA 표준 — 같은
헤더가 multi-edge HA 에서도, multi-coord HA 에서도 작동.

## CoordClient 의 transparent retry

edge 측은 이 헤더 한 개만 추가 처리하면 됨:

```go
// internal/edge/coord_client.go (간략화)
for attempt := 0; attempt <= maxRedirects; attempt++ {
    resp := http.Do(target + path)
    if resp.StatusCode == 503 {
        if leader := resp.Header.Get("X-COORD-LEADER"); leader != "" {
            target = leader   // 갱신, 다음 iteration 에서 새 target
            continue
        }
    }
    return resp
}
```

caller (commitPutMeta 등) 는 단일 호출로 보임. baseURL 도 갱신되어 다음 PUT
부터 곧장 leader 로.

`MaxLeaderRedirects=1` (default). 한 hop 만 — leader 가 또 follower 라면 이미
무언가 잘못됨.

## ג gimel 데모

```
$ ./scripts/demo-gimel.sh
==> waiting for election (~4s)
    leader = coord1:9000

==> PUT /v1/o/gimel/v1 — edge follows X-COORD-LEADER if needed
{"bucket":"gimel","key":"v1","size":11}
    GET round-trip ✓

==> killing leader coord1 → expect re-election
    new leader = coord2:9001

==> PUT after failover
{"bucket":"gimel","key":"v2","size":14}

==> known limitation (ADR-038 → ADR-039 fixes): pre-failover writes
    GET gimel/v1 → 404 expected for now
```

세 가지 evidence:
1. **Election 동작** — coord1 죽고 ~5초 안에 coord2 가 leader 등극.
2. **CoordClient transparent retry** — edge 가 EDGE_COORD_URL=coord1 이지만
   leader 가 coord2 임을 자동 학습.
3. **데이터 sync 갭** — PUT v1 은 coord1 의 bbolt 에만. coord2 가 새 leader
   가 됐지만 그 데이터 모름. **404**.

## 갭은 의도

ADR-038 본문이 명시: **이 ADR 은 election + redirect 만**. 메타 sync 는 다음
ADR (ADR-039 / Ep.32) 가 채운다.

왜 분리? **검증 가능한 단계**. election 메커니즘 자체의 정확성을 데이터 sync
정확성과 따로 확인하고 싶다. 한 ADR 에 둘 다 묶으면 어느 것이 깨졌는지
isolation 어려움.

이게 kvfs 의 ADR 운영 철학 — 큰 변경을 작은 검증 가능 단위로 쪼개기.

## opt-in

```
COORD_PEERS=                                        # single-coord (Ep.1/2)
COORD_PEERS=http://coord1:9000,http://coord2:9000,...    # HA 모드
COORD_SELF_URL=http://coordN:9000                   # 이 coord 가 누구인지
```

미설정 시 Ep.1/Ep.2 동작 그대로. coord.Server.Elector 가 nil → requireLeader
가 항상 false (= 통과). single-coord 운영자에게 보이지 않음.

## 비채택 대안

| 대안 | 기각 사유 |
|---|---|
| follower 가 internally proxy 하기 | RTT 1회 추가. 클라이언트가 직접 leader 로 가는 게 더 빠르고 명확 |
| edge 가 모든 coord URL 알기 + healthz polling | edge 가 coord topology 알아야 → coupling. redirect 헤더가 더 단순 |
| Raft log replication 까지 한 ADR 묶기 | scope 폭증. 검증 단위 잃음 |

## "왜 일부러 다 안 만들었나"

이 글이 자꾸 강조하는 "갭은 의도" — 의문 가질 수 있다. **production 시스템의
개발은 한번에 다 만들지 않는다.** 대신 작은 단위 + 명시적 한계 + 다음 PR 의
약속.

ADR-038 의 -항목에 "데이터 sync 갭 — P6-04 까지는 leader 교체 시 새 leader
가 옛 데이터 모름 (의도된 stage)" 이 쓰여있다. 이 한 줄이 **production
disciplinary** — 갭을 숨기지 않고 ADR 본문에 박는다.

## 다음

Ep.32 — coord 간 WAL replication. ADR-039. 갭 메움. Ep.31 의 demo-gimel 의
404 가 demo-dalet 에서는 200 으로 복원. 같은 메커니즘 (ADR-031 follow-up 의
WAL push), 다른 위치.
