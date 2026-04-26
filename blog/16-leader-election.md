# Episode 16 — Auto leader election: 운영자 깨어나기 전에 cluster 가 알아서

> **Series**: kvfs — distributed object storage from scratch
> **Season**: 4 (performance/efficiency) · **Episode**: 3
> **ADR**: [031](../docs/adr/ADR-031-auto-leader-election.md) · **Demo**: `./scripts/demo-upsilon.sh`

---

## ADR-022 가 남긴 죄

Multi-edge HA (ADR-022) 가 read-replica 모델을 줬다:
- primary 는 write/read
- follower 는 read 만 + 주기적 snapshot pull
- failover = **운영자 수동 절차** (env 갱신 + restart)

마지막 줄이 발목. primary 가 새벽 3시에 죽으면 누군가 깨어나서 follower 를
승격해야 한다. 그동안 write 처리 불가.

ADR-031 의 목표: 그 결정을 **자동화**.

## 풀 Raft 가 아닌, election 만

진짜 Raft 는 두 가지:
1. **Leader election** — 누가 leader 인가
2. **Log replication** — leader 의 write 를 follower 로 복제

본 ep 는 **(1) 만**. (2) 는 ADR-022 의 snapshot-pull 로 이미 처리. 정확한
strong consistency 는 trade-off — leader change 시 마지막 snapshot 이후
write 는 잃을 수 있음 (RPO = snapshot interval).

이 trade-off 가 OK 인 이유: kvfs 는 교육 reference. snapshot interval (default
1h, 데모는 2s) 만큼의 윈도우는 운영자가 통제 가능.

## 3-state machine

```
   Follower ──(election timeout)──▶ Candidate ──(quorum vote)──▶ Leader
      ▲                                │                            │
      │                                │ (lost)                     │
      └─────(higher term seen)─────────┴────────────────────────────┘
```

- **Follower**: leader heartbeat 받으면 election timer reset. 타임아웃 (랜덤
  1.5-3s) 초과 시 → candidate
- **Candidate**: term++, 자기에게 vote, peer 에게 vote 요청. quorum (N/2+1)
  받으면 leader, 못 받으면 follower 복귀
- **Leader**: 주기적 heartbeat (default 500ms) → 모든 peer

핵심 invariant 두 개로 split-brain 방지:
1. **Term 단조 증가** — 같은 term 의 leader 는 한 명. higher term 보면 step down.
2. **한 term 한 vote** — 한 candidate 만 vote 받음 (quorum 한 명만 가능)

## 코드 — 핵심 200 LOC

```go
// internal/election/election.go
type Elector struct {
    cfg        Config
    
    mu          sync.RWMutex
    state       State       // Follower / Candidate / Leader
    currentTerm uint64
    votedFor    string
    leader      string
    lastHB      time.Time
}

func (e *Elector) Run(ctx context.Context) {
    for {
        switch e.State() {
        case Follower:  e.runFollower(ctx)
        case Candidate: e.runCandidate(ctx)
        case Leader:    e.runLeader(ctx)
        }
    }
}
```

3 state 가 각자 자기 루프 — 주식 거래소 같은 turn 진행.

### Follower 루프 — heartbeat 기다리기

```go
func (e *Elector) runFollower(ctx) {
    timeout := e.randomElectionTimeout()  // jittered 1.5-3s
    timer := time.NewTimer(timeout)
    for {
        select {
        case <-ctx.Done():
            return
        case <-timer.C:
            e.becomeCandidate()
            return
        case <-time.After(50ms):
            // periodic check: did HB reset us?
            if time.Since(e.lastHB) < timeout {
                timer.Reset(timeout - time.Since(e.lastHB))
            }
        }
    }
}
```

Jittered timeout 이 핵심 — 두 follower 가 동시에 timer 만료되어 동시 candidate
되는 것 (split vote) 방지. 한 명이 먼저 만료되면 그 쪽이 leader 될 가능성 ↑.

### Candidate 루프 — vote 모으기

```go
func (e *Elector) runCandidate(ctx) {
    e.currentTerm++
    e.votedFor = e.cfg.SelfID
    term := e.currentTerm
    
    votes := 1  // 자기 vote
    var wg sync.WaitGroup
    for _, peer := range e.cfg.Peers {
        wg.Add(1)
        go func(p Peer) {
            defer wg.Done()
            if granted, peerTerm, _ := e.requestVote(ctx, p, term); {
                if peerTerm > term {
                    e.stepDown(peerTerm)  // 누군가 higher term — give up
                    return
                }
                if granted { atomic_inc(votes) }
            }
        }(peer)
    }
    wg.Wait()
    
    quorum := len(e.cfg.Peers)/2 + 1
    if votes >= quorum {
        e.becomeLeader(term)
    } else {
        e.becomeFollower()
    }
}
```

`atomic_inc` 는 mutex로 protect. parallel vote 요청.

### Leader 루프 — heartbeat 송신

```go
func (e *Elector) runLeader(ctx) {
    t := time.NewTicker(e.cfg.HeartbeatInterval)
    e.broadcastHeartbeat(ctx)  // 즉시 1회
    for {
        select {
        case <-ctx.Done(): return
        case <-t.C:
        }
        if e.State() != Leader { return }
        e.broadcastHeartbeat(ctx)
    }
}
```

Heartbeat 은 단순 POST `/v1/election/heartbeat?term=X&leader=Y`. follower 가
받으면 lastHB 갱신 + leader pointer 업데이트.

### Vote 핸들러 — Raft rules 그대로

```go
func (e *Elector) HandleVote(w, r) {
    term := parseTerm(...)
    candidate := r.URL.Query().Get("candidate")
    
    if term < e.currentTerm {
        write({granted: false, term: e.currentTerm})  // stale
        return
    }
    if term > e.currentTerm {
        e.currentTerm = term; e.votedFor = ""; e.state = Follower
    }
    if e.votedFor == "" || e.votedFor == candidate {
        e.votedFor = candidate
        write({granted: true, term: e.currentTerm})
        return
    }
    write({granted: false, term: e.currentTerm})
}
```

3 가지 룰만 있으면 election 끝:
1. stale candidate 거절
2. higher term 보면 step down
3. 한 term 에 한 candidate 만 vote (votedFor 검사)

## Edge integration — 동적 role

기존 `Server.Role` 은 정적 (env 로 박힘). 이제 elector 가 있으면:

```go
func (s *Server) effectiveRole() Role {
    if s.Elector != nil {
        if s.Elector.IsLeader() { return RolePrimary }
        return RoleFollower
    }
    return s.Role  // ADR-022 manual mode 호환
}
```

`rejectIfFollowerWrite`, `roleStatus`, `followerSyncOnce` 모두 위 함수 통해
동적 결정. 따라서 leader 가 바뀌면:
- 옛 leader → write 거부 (X-KVFS-Primary 새 leader 가리킴)
- 새 leader → write 처리
- follower sync 의 다음 tick → 새 leader 의 snapshot pull

## 환경 변수 — opt-in, 기존 호환

```bash
EDGE_PEERS=http://edge1:8000,http://edge2:8000,http://edge3:8000
EDGE_SELF_URL=http://edge1:8000
EDGE_ELECTION_HB_INTERVAL=500ms          # default
EDGE_ELECTION_TIMEOUT_MIN=1500ms         # default
EDGE_ELECTION_TIMEOUT_MAX=3000ms         # default
```

`EDGE_PEERS` 비어있으면 election 비활성 → ADR-022 manual mode 그대로.

## 라이브 데모 (`./scripts/demo-upsilon.sh`)

3 edge + 3 DN, election interval 짧게 (200ms HB / 600-1200ms timeout):

```
--- step 1: which edge is leader? ---
  edge1 (port 8000): leader term= 1
  edge2 (port 8001): follower term= 1
  edge3 (port 8002): follower term= 1
  → leader: edge1

--- step 2: PUT obj-1 via leader (edge1) ---
  PUT obj-1 OK

--- step 3: PUT to non-leader → expect 503 + X-KVFS-Primary header ---
  edge2 response:
    HTTP/1.1 503 Service Unavailable
    X-Kvfs-Primary: http://edge1:8000

--- step 4: kill leader (edge1) ---
  edge1 killed; waiting for failover...

--- step 5: new leader after failover ---
  edge2 (port 8001): follower term= 2
  edge3 (port 8002): leader term= 2          ← edge3 가 election 이김!
  → new leader: edge3

--- step 6: PUT obj-2 via new leader (edge3) ---
  PUT obj-2 OK

✅ υ demo PASS — auto-failover within ~4s, writes resumed via new leader
```

흐름:
- t=0: edge1 leader (term 1)
- t=2: PUT obj-1 OK
- t=3: edge1 kill
- t≈3.6 (heartbeat 끊긴 후 600-1200ms): edge2/3 의 timer 만료
- t≈3.7: 가장 빠른 candidate (edge3) 가 term 2 로 vote 요청
- t≈3.8: edge2 가 vote 줌, edge3 quorum 2/3 → leader
- t≈4.0: edge3 heartbeat broadcast → edge2 가 새 leader 인지
- t=4: PUT obj-2 OK

총 RTO ~1초 (HB interval 짧게 잡았기 때문).

## Trade-off — 솔직한 한계

**얻는 것**:
- ✅ Auto failover — 운영자 개입 0
- ✅ Split-brain 방지 — Raft term + voting invariant
- ✅ 외부 의존 0 (etcd/Consul 등 추가 daemon 불필요)
- ✅ ADR-022 호환 — `EDGE_PEERS` 안 쓰면 정확히 기존 동작

**잃는 것**:
- ❌ Write loss window — leader change 시 last snapshot 이후 write 잃음
  (snapshot interval 만큼이 RPO 상한)
- ❌ Peer set 정적 — 운영 중 peer add/remove 안 됨 (재시작 필요)
- ❌ N=2 cluster 부적합 — quorum=2 = 모두 살아야 election 가능
- ❌ Network partition 시 minority 쪽은 leader 못 만듦 (의도, 안전장치)

**RTO 튜닝**:
- HB 짧게 / timeout 짧게 → failover 빠름, jitter 에 민감 (가짜 election 가능)
- HB 길게 / timeout 길게 → 안정적, RTO 증가
- Default 500ms / 1.5-3s = LAN 환경. WAN 은 늘려야

## 비범위 — 진짜 Raft 가 아닌 것

- **Log replication** — write 를 follower 로 즉시 복제하는 진짜 strong
  consistency. 본 ep 는 election 만; sync 는 ADR-022 snapshot-pull 그대로
- **Dynamic peer reconfiguration** — Raft 의 joint consensus. 운영 복잡
  → 별도
- **Linearizable reads** — follower read 가 항상 최신 보장. 현재는 stale read
  허용

## 코드·테스트

- `internal/election/election.go` — Elector + state machine + RPCs (~440 LOC)
- `internal/election/election_test.go` — 6 tests (vote stale / vote 1-per-term
  / higher-term step-down / heartbeat reset / stale HB rejected / live 3-edge cluster)
- `internal/edge/{edge,replica}.go` — Server.Elector 필드, effectiveRole/PrimaryURL,
  dynamic follower sync, route mounts
- `cmd/kvfs-edge/main.go` — env wiring (EDGE_PEERS / SELF_URL / ELECTION_*)
- `scripts/demo-upsilon.sh` — 3-edge auto-failover live

총 변경: ~700 LOC.

## 다음 ep ([P4-04](../docs/FOLLOWUP.md))

- **ADR-019 — WAL / incremental backup** (분 단위 RPO, ADR-016/022/031 시너지)
- **EC streaming** (ADR-017 follow-up)
- **EC + CDC 조합** (ADR-018 follow-up)
- **진짜 Raft log replication** (ADR-031 후속)

---

*kvfs 는 교육적 레퍼런스. 본 ep 의 election 은 Raft 의 일부분 — 진짜 strong
consistency 는 log replication 까지 필요.*
