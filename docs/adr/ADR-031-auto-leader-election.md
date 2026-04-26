# ADR-031 — Auto leader election (Raft-style, multi-edge HA)

## 상태
Accepted · 2026-04-26

## 맥락

ADR-022 가 read-replica multi-edge HA 의 MVP 를 줬다. 한계:

- failover 가 **운영자 수동** — primary 죽으면 follower 의 env 갱신 + restart
- 운영자가 깨어나기 전에는 write 처리 불가
- "primary URL" 이 모든 follower 에 정적으로 박힘 — 새 primary 등극 시 모두 갱신 필요

본 ADR 의 목표는 그 결정 단계를 자동화 — Raft 의 **leader election 만** 이식
(log replication 은 ADR-022 의 snapshot-pull 그대로 활용).

## 결정

### Raft-style 3-state machine

```
   Follower ──(election timeout)──▶ Candidate ──(quorum vote)──▶ Leader
      ▲                                │                            │
      │                                │ (lost)                     │
      └─────(higher term seen)─────────┴────────────────────────────┘
```

- **Follower**: leader 의 heartbeat 받기. 타임아웃 (랜덤 1.5-3s) 초과 → candidate
- **Candidate**: term++, 자기에게 vote, peer 들에게 vote 요청. quorum (N/2+1) 받으면 → leader
- **Leader**: 주기적 heartbeat (default 500ms) → 모든 peer

Term 은 단조 증가. 한 term 에 한 번만 vote 가능 (votedFor). 더 높은 term 발견 시
즉시 step down. 이 invariants 가 split-brain 방지.

### 모듈 구조

`internal/election/` 새 패키지 (~440 LOC):
- `Elector` (state machine + RWMutex 상태)
- HTTP 핸들러: `HandleVote`, `HandleHeartbeat`, `HandleState`
- `Run(ctx)` blocking 루프 — switch state then dispatch

Edge integration:
- `Server.Elector *election.Elector` 추가
- `effectiveRole()` 가 elector 있으면 `Elector.IsLeader() ? Primary : Follower`,
  없으면 `Server.Role` (ADR-022 manual mode 호환)
- `effectivePrimaryURL()` 가 elector 있으면 `Elector.LeaderURL()`,
  없으면 `followerSt.cfg.PrimaryURL`
- `rejectIfFollowerWrite` / `runFollowerSync` 모두 위 두 함수 통해 동적 결정
- 라우트: `POST /v1/election/{vote,heartbeat}`, `GET /v1/election/state`

### 환경 변수

```
EDGE_PEERS=http://edge1:8000,http://edge2:8000,http://edge3:8000
EDGE_SELF_URL=http://edge1:8000
EDGE_ELECTION_HB_INTERVAL=500ms          (기본)
EDGE_ELECTION_TIMEOUT_MIN=1500ms         (기본)
EDGE_ELECTION_TIMEOUT_MAX=3000ms         (기본)
```

`EDGE_PEERS` 비어있으면 election 비활성 — ADR-022 manual mode 폴백.

### Failover 동작

1. Leader (edge1) 죽음 → heartbeat 끊김
2. 약 1.5-3s 후 follower 들의 election timer 만료 → candidate 전환
3. 가장 빨리 timer 만료된 candidate 가 term++, vote 요청
4. quorum 받으면 leader → 새 heartbeat 시작
5. 모든 follower 가 새 leader URL 알게 됨 (heartbeat payload 의 leader 필드)
6. follower sync loop 의 다음 tick → snapshot pull URL 자동 갱신

총 RTO: **약 2-5초** (election timeout + 1 vote round-trip + 1 heartbeat).

### Log replication 비범위

본 ADR 은 **leadership 결정만**. 메타 sync 는 ADR-022 의 snapshot-pull 그대로
유지. 즉:
- 새 leader 가 자기 bbolt 로 write 시작
- 옛 leader 가 못 본 in-flight write 가 있을 수 있음 (snapshot 사이 gap)
- Strong consistency 는 본 ADR 범위 외 — 진짜 Raft log replication 은 후속

운영 가정: leader change 가 일어나기 전 마지막 snapshot pull 시점 이후 write 는
"잃을 수 있음". 운영자가 RPO 설정 (snapshot interval) 으로 통제.

## 결과

**긍정**:
- Failover 자동화 — 운영자 개입 0
- Split-brain 방지 — Raft 의 term + voting invariant
- 외부 의존 0 (etcd/Consul 등 추가 daemon 불필요)
- ADR-022 호환 — `EDGE_PEERS` 안 쓰면 정확히 기존 동작
- 6 unit tests PASS (vote rules / higher-term step-down / heartbeat reset / live cluster 3-edge election)

**부정**:
- write loss window — leader change 시 마지막 snapshot 이후 write 잃음
  (snapshot interval 만큼이 RPO 상한)
- peer set 정적 — 운영 중 peer add/remove 안 됨 (재시작 필요)
- 작은 cluster (N=2) 에서는 quorum=2 = 모두 살아야 election 가능 (2-edge 비추)
- network partition 시 minority 쪽은 leader 못 만듦 (의도, 안전장치)

**트레이드오프**:
- HB 짧게 / timeout 짧게 → failover 빠름, 네트워크 jitter 에 민감 (가짜 election)
- HB 길게 / timeout 길게 → 안정적이나 RTO 증가
- Default (500ms / 1.5-3s) 은 LAN 환경 가정. WAN 은 늘려야

## 보안 주의

- Election RPC (vote/heartbeat) 는 인증 없음 — admin 망 격리 가정
- 악성 peer 가 자기를 leader 로 우길 수 있음 (peer set 변조 시) — TLS + mTLS 권장
  (ADR-029)
- term 변조로 cluster freeze 시키는 공격 가능 — 위 mTLS 로 차단

## 데모 시나리오 (`scripts/demo-upsilon.sh`)

3 edge 클러스터 + 동일 DN cluster. 첫 leader 등극 후 PUT 1, leader 죽이고
새 leader 자동 등극 확인 후 PUT 2 (옛 follower 가 받음).

## 관련

- `internal/election/election.go` — Elector + state machine + RPCs (~440 LOC)
- `internal/election/election_test.go` — 6 tests (vote rules, term, heartbeat, live cluster)
- `internal/edge/edge.go` — `Server.Elector`, route mounts, `StartElector`
- `internal/edge/replica.go` — `effectiveRole`, `effectivePrimaryURL`, dynamic
  follower sync, election-mode `SetElectionFollowerSync`
- `cmd/kvfs-edge/main.go` — env wiring (`EDGE_PEERS/SELF_URL/ELECTION_*`)
- `scripts/demo-upsilon.sh` — 3-edge auto-failover live
- 후속 ADR 후보: 진짜 Raft log replication (write loss window 제거),
  dynamic peer add/remove, learner replicas
