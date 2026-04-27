# ADR-050 — Raft §5.4.1 log-up-to-date vote check + coord bootstrap snapshot pull

상태: Accepted (2026-04-27)
시즌: P8-06 (Frame-1+2 100% wave 의 simplified Raft 강화)

## 배경

[chaos-mixed.sh](../../scripts/chaos-mixed.sh) 가 실 architectural gap 을
확률적으로 노출했다 — 5회 중 1회 정도 빈도로 200-PUT 이 post-recovery 시
GET 404. 분석 결과 두 결합된 단순화의 결과:

1. **Vote rule 의 단순화**. ADR-031 의 `HandleVote` 가 term-only 검사:
   incoming term ≥ currentTerm 이면 vote 부여. Raft §5.4.1 의 "candidate
   's log must be at least as up-to-date as voter's" invariant **부재**.
2. **WAL replication 의 push-only 설계**. ADR-039 의 `walHook` 은 leader
   가 commit 할 때 peers 에 push. 그러나 **dead 였던 follower 는 missed
   entry 를 catch-up 하는 메커니즘이 0**. ADR-022 의 multi-edge HA 가
   가졌던 snapshot-pull 의 coord 측 미구현.

두 단순화 결합 시 시나리오:

```
t=0  coord A leader, coord B follower, coord C ALIVE.
t=1  PUT 도착 → A가 ReplicateEntry: A self ack + C ack = quorum 2/3.
     B 는 dead 라 push 못 받음. A, C 의 bbolt 에만 entry.
t=2  A dead.
t=3  B 살아남. B 는 자기 stale bbolt 그대로. C 는 entry 보유.
t=4  B 가 election timer fire → term ↑. ask vote.
     C 는 term-only check 통과 → vote 부여. B 가 leader 됨.
t=5  client GET → edge → B → 404 (B 는 entry 모름).
```

C 가 가지고 있는 데이터를 B 가 모르고 leader 가 됨. quorum 으로 commit 한
entry 가 user 시점에서 사라짐. ADR-040 의 "phantom write 0" 약속이 깨지는
순간.

## 결정

두 갈래 동시 적용 — 각각 하나만 단독 적용 시 다른 한 갈래가 갭으로 남음.

### Part 1: Raft §5.4.1 log-up-to-date vote check

`election.Config.LastLogSeqFn` 신규. caller 가 local lastSeq 반환 콜백 등록
(coord daemon 은 `WAL.LastSeq` 으로 wire). vote request URL 에
`last_log_seq` 파라미터 추가.

`HandleVote` 가 voter 의 lastLogSeq > candidate 의 lastLogSeq 이면 vote
거부. 거부 시 logger 에 양측 seq 노출 (디버깅).

```
URL: /v1/election/vote?term=X&candidate=Y&last_log_seq=Z

if voterLogSeq > candidateLogSeq:
    deny vote (Raft §5.4.1)
```

`LastLogSeqFn` 미설정 시 check skip → 기존 ADR-031 동작 (single-coord 또는
non-WAL cluster 에 backward compat).

**왜 seq 만 비교하나, term 안 비교?** kvfs WAL 은 monotonic seq 만 기록 —
entry 별 term 미보존. Raft 정통은 (lastLogTerm, lastLogIndex) 두 쌍 비교지만
kvfs 는 transactional Raft (ADR-040) 가 leader-only commit 강제 → 같은 seq
의 entry 가 두 coord 에서 다른 내용일 수 없음 (impossible by construction).
seq 비교만으로 충분.

### Part 2: Coord bootstrap snapshot pull

새 endpoint:

```
GET /v1/coord/admin/meta/snapshot   → bbolt stream (coord)
```

ADR-014 의 edge-side `meta/snapshot` 의 coord 측 미러. 인증 같음 (cluster-
internal trust model). leader 만 줄 필요 없음 — follower 도 자기 로컬 bbolt
stream 가능 (어차피 quorum-acked entry 는 어느 살아있는 peer 에든 있다).

coord daemon 의 boot 시퀀스에 `bootstrapFromPeer` 추가:

1. `peerURLs` (self 제외) 의 healthz 에서 role=leader 인 peer 우선
2. 그 peer 에서 snapshot 받아 `<dataDir>/bootstrap.snapshot.db` 로 저장
3. `MetaStore.Reload(path)` — atomic swap (ADR-022 의 hot-swap 메커니즘 그대로)
4. 모든 peer 가 unreachable → log warning, 진행 (best-effort)

이 두 갈래가 결합돼 시나리오 t=4 가 다음으로 변함:

```
t=3  B 살아남. ★boot 시 snapshot pull from C. B 도 이제 entry 보유.
t=4  B 가 candidate. C 의 voter check: voter.seq == candidate.seq → vote OK.
     B 가 leader 되어도 entry 보유.
or
t=3  B 살아남. snapshot pull 실패 (C 도 dead 인 partition).
t=4  B 가 candidate. lastLogSeq 0. C 가 ack 못 받음 (혹은 alive 면 거부 by §5.4.1).
     B 가 leader 못 됨. 안전.
```

## 비채택 대안

| 대안 | 기각 사유 |
|---|---|
| Term per WAL entry (Raft 정통) | schema migration. transactional commit 가 invariant 강제하므로 미필요 |
| Heartbeat 에 prevLogIndex 동봉 + AppendEntries replay | 복잡 (prevLogIndex per-peer state). snapshot pull 이 단순하고 충분 |
| Lazy catch-up: dead-then-alive coord 가 자기 stale bbolt 로 가다가 incoming heartbeat 의 term 보고 step down | step down 만으로 entry 안 옴. snapshot pull 또는 log replay 가 필수 |
| chaos-mixed 발견 무시 (educational reference, production 아님) | invariant 깨짐은 reference 에서도 부정확. 학습성 해침 — "이 코드가 ADR-040 phantom-write 0 약속한다" 가 거짓이 됨 |

## 결과

+ **chaos-mixed 의 hard invariant (FINAL_FAIL == 0) 이 일관되게 holds**:
  fix 전 5회 중 1회 violation → fix 후 5회 연속 0 violation.
+ Coord daemon 의 분산 storage primitive 가 textbook Raft 의 핵심 invariant
  를 (단순화된 형태로) 충족 — frame 2 의 진척.
+ ADR-022 의 snapshot-pull 패턴이 두 도메인 (multi-edge follower + coord
  follower) 에서 재사용 — 좋은 abstraction 의 또 다른 증거.
- **Boot latency**: snapshot 받는 동안 coord 시작 지연. 작은 cluster 에선
  ~100ms 단위, 큰 클러스터 (수백만 entry) 에선 수 초. opt-in 화 후보지만
  default 로 fail-safe.
- **Snapshot consistency window**: pull 도중 leader 가 새 entry commit 하면
  snapshot 에 안 들어감. coord boot 후 walHook push 로 받음 → eventually
  consistent. 새 ep 동안의 인접 entry 손실은 없음 (ADR-040 transactional
  보장 그대로).
- **lastLogSeq 의 weak ordering**: 두 candidates 가 같은 seq 면 둘 다 vote
  받을 수 있음. 한 term 에 한 vote rule 이 split 방지 — 일반 Raft 와 동등.
- **opt-in 부분 호환**: `LastLogSeqFn` 미설정 시 vote check 비활성. WAL 미사용
  cluster (single-coord 모드) 가 영향 받지 않음.

## 호환성

- `election.Config.LastLogSeqFn` 신규 필드 — 기본값 nil → 기존 동작.
- 새 query 파라미터 `last_log_seq` — 미포함 시 0 으로 파싱, voter 가 자기
  seq 비교 → 자기 seq 가 0 이면 통과. 즉 양쪽 다 nil 인 cluster (= 전체
  미적용) 는 기존 동작 유지.
- `coord.Server.Routes` 에 새 endpoint `meta/snapshot` 추가. 기존 endpoint
  영향 0.
- coord daemon 의 boot path 에 snapshot pull 추가 — peer 미설정 (single-
  coord) 또는 WAL 미설정 시 skip.

## 검증

- `internal/election/election_test.go::TestVoteRejectStaleLog` —
  LastLogSeqFn=100 voter 가 last_log_seq=50 candidate 거부, last_log_seq=
  100 candidate 허용. nil-LastLogSeqFn fallback 도 검증.
- `scripts/chaos-mixed.sh` 5회 연속 실행: 5회 모두 FINAL_FAIL=0. 이전에는
  같은 5회 실행 중 평균 1회 violation (확률적).
- 전체 test suite 161 PASS (1 신규).

## 후속

- **lastLogTerm 비교**: WAL entry 에 term 추가 후 (lastTerm, lastSeq) 페어
  비교로 격상. transactional commit 강제 하에서는 needed 하지 않으나, 미래의
  multi-leader / lossy partition 시나리오에 대비.
- **AppendEntries-style replay**: snapshot pull 보다 가볍게 따라잡는 path.
  큰 cluster 의 coord restart 에서 latency 단축.
- **Snapshot streaming compression**: 큰 bbolt 에서 boot latency 완화.
