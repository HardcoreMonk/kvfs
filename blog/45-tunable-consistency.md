# Episode 45 — Dynamo W+R>N: client 가 일관성 위치를 고른다

> **Series**: kvfs — distributed object storage from scratch
> **Season**: 7 (textbook primitives) · **Episode**: 3
> **연결**: [ADR-053](../docs/adr/ADR-053-tunable-consistency.md) · [demo-pe](../scripts/demo-pe.sh)

---

## 한 점에서 한 줄로

S1~S6 까지 kvfs 의 quorum 은 boot 시 한 번 박힌 한 점:

```
N (replicationFactor) = 3
W (write quorum)      = 2     ← ⌊N/2⌋+1
R (read quorum)       = 1
```

cluster 의 모든 client 가 이 한 점을 따름. 빠른 응답이 필요한 cache 도, 결제
로그도 같은 latency / consistency.

textbook (Dynamo, 2007) 은 **per-request 조정** 을 가르친다:

```
W + R > N → strong consistency (마지막 write 가 다음 read 에 보임)
W + R ≤ N → eventually consistent (write replicas 와 read replicas 가 안 만남)
```

같은 cluster 위에서 client 별로 trade-off 를 고른다.

## 헤더 두 개

```
PUT /v1/o/bucket/key
X-KVFS-W: 3            ← 모든 3 replicas ack 받아야 성공 (강한 쓰기)

GET /v1/o/bucket/key
X-KVFS-R: 3            ← 3 replicas 가 sha256 일치해야 성공 (강한 읽기)
```

미설정 시 default — 헤더 0 일 때는 기존 동작 그대로.

## Validation

```
W < 1                          → 400
W > replicationFactor          → 400 (어차피 못 채움)
R > min(chunks[*].replicas)    → 400 (chunks 가 가진 것보다 많이 요구)
non-numeric                     → 400
empty                          → use default (back-compat)
```

400 빨리 거부 — 운영자 실수가 시스템 안 흔들고 즉시 시그널.

## Write path: WriteChunkToAddrsW

```go
// Coordinator.WriteChunkToAddrsW(ctx, chunkID, data, targets, w)
// w == 0 → default quorum
// w > 0  → 모든 target 에 fan-out, ack ≥ w 면 성공
```

알고리즘 변경 0. fan-out + ack 카운팅은 그대로. 임계값만 caller-controlled.

```go
if len(ok) < w {
    return ok, fmt.Errorf("quorum not reached: %d/%d", len(ok), w)
}
```

## Read path: readChunkAgreement

R > 1 일 때 새 helper:

```go
func (s *Server) readChunkAgreement(ctx, chunkID, replicas, r) ([]byte, error) {
    // 1. 첫 r replicas 동시 GET
    // 2. 각 replica 의 body 의 sha256 == chunk_id 검증 (free integrity probe!)
    // 3. r 개 모두 동일 body 면 200 + body
    // 4. 어느 하나라도 fail / mismatch → 502
}
```

**Free integrity probe**: chunk_id 가 sha256 인 [Ep.1 의 content-addressable](
01-hello-kvfs.md) 결정 덕분에, R > 1 의 agreement 는 sha 일치를 강제 — silent
corruption 발견. ADR-005 의 sha256 결정이 이렇게 ADR-053 까지 살아 있다.

## פ pe 데모

```
$ ./scripts/demo-pe.sh

==> stage 1: default (W=2, R=1) all 3 DN alive
    PUT (no header) → 201
    GET (no header) → 200

==> stage 2: invalid X-KVFS-W=99 (R=3 cluster)
    PUT W=99 → 400

==> stage 3: strong write W=3 with all alive
    PUT W=3 → 201

==> stage 4: kill dn3, retry W=3
    PUT W=3 (1 DN dead) → 502 (quorum unreachable)

==> stage 5: same outage, weak write W=1
    PUT W=1 (1 DN dead) → 201 (saves writes when DN flaky)

==> stage 6: bring dn3 back, R=3 all-agree
    GET R=3 (all alive) → 200

==> stage 7: kill dn3, R=3
    GET R=3 (1 DN dead) → 502

==> stage 8: R=1 still works
    GET R=1 (1 DN dead) → 200

==> stage 9: metrics
    kvfs_tunable_quorum_total{op="read",value="3"} 2
    kvfs_tunable_quorum_total{op="write",value="1"} 1
    kvfs_tunable_quorum_total{op="write",value="3"} 2
```

9 stages, all green. metric 이 누가 어떤 quorum 으로 부르는지 시각화.

## 4 가지 운영 모드

| 시나리오 | W | R | W+R | 의미 |
|---|---|---|---|---|
| Cache, log | 1 | 1 | 2 (≤N) | 빠름, eventually consistent |
| 결제, 감사 | 3 | 1 | 4 (>N) | 강한 쓰기, 빠른 읽기 |
| Critical read | 1 | 3 | 4 (>N) | 빠른 쓰기, 강한 읽기 |
| Strict | 3 | 3 | 6 (>N) | 둘 다 강함, 느림 |
| 기본 | 2 | 1 | 3 (=N) | balanced (kvfs default) |

같은 cluster, 같은 코드, 다른 헤더로 client 마다 자기 trade-off.

## 부수 fix — phantom Content-Length

ADR-053 의 demo 만들면서 발견. handleGet 가 chunk fetch **전에**
`Content-Length: meta.Size` 헤더 설정 → 첫 chunk 실패 시 writeError 가 502 +
짧은 JSON 응답. 그러나 Content-Length 이미 헤더 버퍼에 들어가 있어 클라이언트
가 "transfer closed with N bytes remaining" 경고.

원래 동작: 502 status 는 정확하지만 body 가 promised CL 보다 짧음. curl 같은
strict client 가 exit non-zero. 쉘 스크립트의 `set -e` 가 이걸로 실패.

수정: 첫 chunk 성공 후에 Content-Length 설정. 502 의 헤더는 깨끗.

이게 chaos test 의 가치. **demo 가 안 굴러가는 것이 architectural 정확성의
신호** — 작은 잘못이 더 큰 시스템에서 어떻게 보일지 미리 보여준다.

## EC 는 다음 ep 의 일

ADR-053 의 - 항목: **EC 모드는 헤더 무시**. EC 의 quorum 의미는 다름:
- K shards = data 정합성 hard requirement (any K → reconstruct)
- M parity = redundancy

EC 의 tunable ack-quorum 은 trade-off 가 다른 차원 — 별도 ADR 후보.

## frame 2 진척

| primitive | 상태 |
|---|---|
| Failure domain hierarchy (Ep.43) | ✓ |
| Degraded read (Ep.44) | ✓ |
| **Tunable consistency** (이 글) | ✓ |
| Anti-entropy / Merkle tree | 다음 (Ep.46) |

S7 4 ep 중 3 마감. 마지막 ep 가 frame 2 100% 완성.

## 다음

Ep.46 — anti-entropy / Merkle tree. DN 끼리 주기적으로 hash tree 비교, silent
corruption 자동 발견. textbook 의 "Cassandra read repair" 같은 패턴. ADR-054.
demo-tsadi (צ).

S7 마지막 ep — kvfs 가 Frame 2 완성하는 순간.
