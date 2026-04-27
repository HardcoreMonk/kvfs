# ADR-053 — Tunable consistency (per-request W/R quorum)

상태: Accepted (2026-04-27)
시즌: Season 7 Ep.3 — frame 2 (textbook primitives)

## 배경

Replication mode 의 quorum 은 boot 시점에 고정:
- N (replicationFactor) = 3
- W (write quorum) = ⌊N/2⌋+1 = 2
- R (read quorum) = 1

Dynamo 의 textbook 모델은 **per-request 조정**:
- W + R > N → strong consistency (마지막 write 가 보임)
- W = 1, R = 1 → maximum availability, eventually consistent
- W = N, R = 1 → strong write, fast read
- W = 1, R = N → fast write, strong read

지금까지 kvfs 는 "W=2, R=1" 한 점 — operator 가 trade-off 위치를 못 고름.
이번 ep 가 이 한계 메움: **per-request HTTP 헤더로 quorum 선택**.

## 결정

새 헤더:

- `X-KVFS-W: <int>` on PUT — write quorum (1 ≤ W ≤ N)
- `X-KVFS-R: <int>` on GET — read quorum (1 ≤ R ≤ object's min replicas)

미설정 시 default 보존 (W=⌊N/2⌋+1, R=1) → **back-compat 100%**.

### Validation

```
W < 1                          → 400 Bad Request
W > replicationFactor          → 400 (would always fail anyway)
R < 1                          → 400
R > min(chunks[*].replicas)    → 400 (more than chunks have)
non-numeric                    → 400
empty / missing                → 0 (= use default)
```

400 으로 빨리 거부 — 운영자 실수 즉시 시그널.

### Write path

`Coordinator.WriteChunkToAddrsW(ctx, chunkID, data, targets, w)`:
- w == 0 → 기존 default (`c.quorumWrite`)
- w > 0 → 모든 target 에 fan-out PUT 후 ack 가 w 이상이면 성공
- 알고리즘 변경 0 — 임계값만 caller-controlled

### Read path

R == 1 → 기존 path (`Coord.ReadChunk` 가 replicas 순차 시도, 첫 성공 win).
R > 1 → 새 helper `readChunkAgreement`:

1. 첫 R replicas 에 동시 GET
2. 각 replica 의 sha256 검증 (chunk_id 가 곧 sha256 — content-addressable)
3. 모든 R 이 동일 body 일치 시 200 + body
4. 어느 하나라도 fail / sha mismatch / replicas disagreement → 502

Latency: R-th 가장 느린 replica 가 결정. R > 1 = consistency over latency
trade-off, 명시적 선택.

### Default 값 + 헤더 양립

```
헤더 없음:    W=2, R=1 (기존)
X-KVFS-W:1   W=1, R=default
X-KVFS-W:3   W=3, R=default
X-KVFS-R:3   W=default, R=3
둘 다 지정:    각 자기 default 대신 명시값
```

### Header → metric

`kvfs_tunable_quorum_total{op="read"|"write", value="N"}` — operator 가
"누가 어떤 quorum 으로 부르는지" 시각화 가능.

### EC mode

본 ADR 은 **replication mode 만**. EC 의 quorum 의미는 다름 (K shards 는
data 정합성의 hard requirement, M parity 는 redundancy). EC 의 tunable
ack-quorum 은 별도 ADR — 미래 후보.

EC 요청에 X-KVFS-W/R 헤더 와도 거부하지 않음 (graceful 무시), 다만
header parsing 단계는 replication 분기에만 — EC 분기는 영향 0.

## 부수 fix — handleGet 의 phantom Content-Length

ADR-053 구현 중 발견. handleGet 가 chunk fetch 전에 `Content-Length: meta.
Size` 를 헤더에 설정 → 첫 chunk 실패 시 writeError 가 502 + 짧은 JSON 응답
하지만 Content-Length 는 이미 meta.Size 로 헤더 버퍼에 들어감 → 클라이언트
가 "transfer closed with N bytes remaining" 보고. content-length 거짓말.

수정: 첫 chunk 성공 후 Content-Length 설정. 502 응답에는 별도 CL 안 넣음
(writeError 내부에서 짧은 JSON CL 자동).

이 fix 는 ADR-053 의 chaos test 에서 발견 — `set -e` 쉘 스크립트가 curl 의
"transfer closed" exit non-zero 에 정지. 운영 시 monitoring tool / 클라이언트
SDK 가 같은 함정에 빠질 수 있음. 작지만 의미 있는 정확성 개선.

## 비채택 대안

| 대안 | 기각 사유 |
|---|---|
| Cluster-wide W/R env (`EDGE_QUORUM_WRITE_OVERRIDE`) | 운영자가 보내는 client 마다 다른 SLO. per-request 가 정상 |
| Body 기반 (JSON request body 의 quorum 필드) | RESTful PUT 의 body 는 데이터. 메타 conf 를 헤더에 두는 게 표준 |
| W>0 + R 무시 (write-only 조정) | 양방향 trade-off 가 textbook. R 도 조정 가능해야 의미 |
| W+R>N invariant 자동 강제 (헤더로 W, R 보내면 합 ≥ N+1 만 허용) | 운영자에게 weak consistency 옵션 박탈. eventually consistent 도 valid 선택 (cache, log) |
| EC tunable W 본 ADR 에 포함 | scope 폭증. EC 의 ack-quorum 은 다른 trade-off (M parity 의 의미) — 별도 ADR 가치 |

## 결과

+ **per-request consistency**: 같은 cluster 위에서 hot data (W=3, R=1) 와
  cache data (W=1, R=1) 를 client 가 헤더로 선택.
+ **integrity probe**: R > 1 의 sha256 agreement 가 silent corruption 자동
  발견 (chunk_id 가 sha256 인 ADR-005 덕분에 free).
+ **observability**: `kvfs_tunable_quorum_total` counter 가 operator 의
  실제 사용 패턴 노출.
+ **back-compat**: 헤더 미설정 시 동작 0 변경. 기존 client 영향 없음.
+ **honest fix**: phantom Content-Length 도 같이 정정 — chaos test 의
  부수 finding 을 production-ready 수준으로 마무리.
- **EC 미적용**: EC GET/PUT 은 헤더 무시. 운영자가 EC 객체에 X-KVFS-W
  보내도 default 동작. 미래 ADR.
- **R-th-slowest tail**: R > 1 의 GET 은 R 개 replica 모두 응답할 때까지
  대기. R = N (= 3) 면 가장 느린 replica 가 결정. R = 1 의 fastest-wins
  대비 latency ↑ — 운영자의 명시적 선택.
- **N (replicationFactor) 변경 시 재검증**: 운영자가 R 을 늘리면 (ADR-053
  은 그 가능성 미지원이긴 하지만) 기존 객체의 chunks 는 옛 N 의 replica
  list 를 가짐. X-KVFS-R 가 새 N 까지 valid 한지 객체별 확인 필요 —
  `maxReplicas` helper 가 이 chunk 단위 확인.
- **No write-side fanout limit**: WriteChunkToAddrsW 가 모든 target 에
  fan-out PUT 시도. W < N 이면 잉여 PUT 까지 동시에. DN load 영향 ≤ pre-S7
  default (이미 모든 R 에 fan-out 했음).

## 호환성

- 헤더 미설정 시 0 변경.
- `Coordinator.WriteChunkToAddrs` API 보존; 새 `WriteChunkToAddrsW` 가
  옵셔널 W 파라미터. 기존 caller 자동으로 default path.
- handleGet 의 Content-Length timing 변경 — 정상 GET response shape 동일,
  502 의 헤더만 정정 (외부 가시 변화 거의 없음, monitoring 만 더 깨끗).

## 검증

`internal/edge/tunable_quorum_test.go` 신규:
- `TestParseQuorumHeader` (subtests × 7) — empty, valid, too low, too
  high, non-numeric, max=0 disable.
- `TestReadChunkAgreement_AllAgree` — 3 fake DN 일치 → 성공.
- `TestReadChunkAgreement_OneFails` — 3 중 1 fail → quorum 미달 error.
- `TestReadChunkAgreement_DisagreementDetected` — 1 corrupt body →
  sha mismatch 잡힘.

`scripts/demo-pe.sh` 9 stages live:
1. default 동작 (W=2 R=1) → 201/200
2. invalid X-KVFS-W=99 → 400
3. W=3 with 3 alive → 201
4. W=3 with 1 dead → 502 (quorum 불가)
5. W=1 with 1 dead → 201 (살아남음)
6. R=3 with 3 alive → 200 (agreement)
7. R=3 with 1 dead → 502
8. R=1 with 1 dead → 200 (back-compat 동작)
9. metric counter `kvfs_tunable_quorum_total` 시각화 — labels 별 분리

전체 test suite **170 PASS** (tunable_quorum +3 + ParseQuorumHeader subtests).

## 후속

- **EC tunable ack-quorum**: K+M shards 의 ack 임계값 per-request. ADR
  미작성 — measurement 후 결정.
- **W+R>N 강제 옵션**: 운영자가 cluster-wide invariant 를 켜면 weak 조합
  거부. 구체화는 후속.
- **Hedged read**: R = 1 의 fast path 에 timeout 후 second replica 동시
  발사. tail latency 더 단축. 별도 ADR.
