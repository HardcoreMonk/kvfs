# Episode 48 — bit-rot 자동 회복 + dry-run preview

> **Series**: kvfs — distributed object storage from scratch
> **Wave**: P8-09 (ADR-055 follow-up) · **Episode**: 2
> **연결**: [ADR-056](../docs/adr/ADR-056-corrupt-repair-and-dry-run.md) · [demo-anti-entropy-repair-corrupt](../scripts/demo-anti-entropy-repair-corrupt.sh)

---

## 두 detection, 한 repair

[Ep.46 anti-entropy](46-anti-entropy-merkle.md) 가 두 detection 채널을 만들었다:

1. **Coord-side Merkle audit** — DN inventory 가 ObjectMeta 와 일치하나?
   (chunk 가 있어야 할 곳에 있는가)
2. **DN-side scrubber** — DN 에 있는 chunk 의 sha256 가 chunk_id 와 일치하나?
   (있는 chunk 가 진짜 그 chunk 인가)

[Ep.47 auto-repair](47-anti-entropy-auto-repair.md) 가 (1) 의 detection-action
loop 를 닫았다 — `inventory missing` 발견 시 자동 복구.

그러나 (2) 는 detection 만이고 repair 안 됨. scrubber 가 corrupt 보고해도
운영자가 수동 처리. 이 ep 가 두 번째 loop 닫음.

또한 Ep.47 의 repair 는 **dry-run 없음** — operator 가 큰 cluster 에서
"이거 진짜 실행해도 안전?" preview 못 함. 이 ep 가 그것도 메움.

## 작은 함정 — DN PUT 의 idempotent

scrubber 가 corrupt 발견 → repair 가 healthy replica 에서 read → target 에
PUT. 작동? 안 함.

문제: DN 의 `handlePut`:

```go
if _, err := os.Stat(path); err == nil {
    // idempotent: file exists → 200 OK + skip
    w.WriteHeader(http.StatusOK)
    return
}
```

corrupt 시나리오에서 file IS 디스크에. 다만 byte 가 잘못됨. 이 idempotent
skip 이 ADR-005 (chunk_id == sha256) invariant 가정 — file 이 path 에 있으면
content 가 그 chunk_id 의 sha256 인 것이 보장됐었다.

bit-rot 이 그 invariant 위반. PUT 이 skip 하면 corrupt 는 영원.

## Force overwrite

해결: DN PUT 에 `?force=1` query param. Coord 의 새 helper:

```go
func (c *Coordinator) PutChunkToForce(ctx, addr, chunkID, data) error {
    url := fmt.Sprintf("%s://%s/chunk/%s?force=1", ...)
    // ... overwrite 강제
}
```

DN handlePut 에 한 줄:

```go
force := r.URL.Query().Get("force") == "1"
if !force {
    if _, err := os.Stat(path); err == nil {
        // existing idempotent skip
        return
    }
}
// fall through to write
```

force=1 시 existing skip 우회. body 는 여전히 sha256(body) == chunk_id
강제 검증하므로 잘못된 body 는 통과 못 함. invariant 보존, 수정 가능.

## Anti-entropy 측 통합

`/anti-entropy/repair?corrupt=1` flag:

```go
type AntiEntropyRepairOpts struct {
    Corrupt bool  // ADR-056
    DryRun  bool  // ADR-056
}

// runAntiEntropyRepair:
if opts.Corrupt {
    corruptByDN = collectCorruptByDN(...)  // GET /chunks/scrub-status from each
}
// 각 DN 에 대해 missing 우선 처리, 그 다음 corrupt 처리
// corrupt repair 시 PutChunkToForce 사용
```

ChunkRepairOutcome 에 `Reason` 필드 추가 — `"missing"` 또는 `"corrupt"`.
operator 가 어느 detection channel 이 trigger 했는지 식별.

## Dry-run

같은 query param style:

```
POST /v1/coord/admin/anti-entropy/repair?dry_run=1
```

같은 pipeline (audit → source 선택) — 다만 ReadChunk + PutChunkTo 호출
**안 함**. outcome 에 `Planned: true` 표시. byte movement 0.

`?corrupt=1&dry_run=1` 조합으로 corrupt repair 도 preview 가능.

## 데모 — 7 stages

```
$ ./scripts/demo-anti-entropy-repair-corrupt.sh

==> stage 1: PUT 3 objects
==> stage 2: bit-rot on dn2
    overwrote b87b474f.. with garbage
    waiting 4s for scrubber...
    scrub-status: {"corrupt_count":1,"corrupt":["b87b474f..."]}
    ✓ scrubber flagged
==> stage 3: default repair (missing-only) — no work
    {"repaired_ok":0,"dry_run":false}
    ✓ default ignores corrupt
==> stage 4: dry-run + corrupt — preview
    {"dry_run":true,"planned":1,
     "example":{"target_dn":"dn2:8080","source_dn":"dn1:8080",
                "reason":"corrupt","planned":true}}
    ✓ dn2 disk still corrupt — dry-run didn't move bytes
==> stage 5: real repair + corrupt
    {"repaired_ok":1,"reasons":["corrupt"]}
    ✓ corrupt overwritten from healthy replica
==> stage 6: next scrub pass
    scrub-status: {"corrupt_count":0,"scrubbed":62}
    ✓ scrubber confirmed bytes healthy
==> stage 7: GET via edge → intact body

=== PASS ===
```

7 stages 모두 expected. detection (scrubber) → preview (dry-run) →
action (repair with force) → verify (next scrub) → user-visible.

## "왜 PUT default 를 force 로 바꾸지 않나"

대안: handlePut 가 file 존재 시 **항상** sha256 재검증. 일치면 idempotent
skip, 불일치면 overwrite. 운영자가 force flag 안 보내도 자동 self-heal.

장점: ADR-005 invariant 가 더 자동.
단점: **매 PUT 마다 기존 file 의 full read + sha256**. hot path 부담 ↑↑.

ADR-056 의 결정: cost 를 corrupt-repair 시점으로 localize. force flag 는
명시적 — caller (anti-entropy worker) 가 corrupt 임을 알 때만 활성. 일반
PUT 은 idempotent 빠른 path 그대로.

## "왜 dry-run 이 별도 endpoint 가 아닌 flag 인가"

대안: `POST /anti-entropy/plan` (preview) + `POST /anti-entropy/repair`
(execute).

장점: REST-y.
단점: logic 중복 (두 엔드포인트가 같은 audit + source 선택). flag 1개로
같은 pipeline 의 마지막 step 만 분기.

ADR-056 의 결정: flag. 코드 한 곳.

## ADR-005 의 자가 회복 미덕, 또

Force PUT 은 위험해 보이지만 **DN 이 여전히 sha256(body) == chunk_id 강제
검증**. 잘못된 body 는 force 든 아니든 통과 못 함. content-addressable
invariant 가 force flag 하에서도 보호.

ADR-005 (sha256 chunk_id) 가 또 한 번 dividend — repair worker 가 byte
movement 할 때 자동 integrity 검증.

## 정량

| | |
|---|---|
| 코드 | DN handlePut +5 LOC, `Coordinator.PutChunkToForce` ~25 LOC, anti-entropy refactor (Opts struct + Reason field + corrupt collector + force in copyChunk) ~80 LOC |
| 새 endpoints | 0 (기존 `/repair` 에 query param 추가) |
| 새 query params | `corrupt=1`, `dry_run=1`, DN PUT 의 `force=1` |
| cli flags | `--corrupt`, `--dry-run` |
| 데모 | 7 stages (corrupt repair + dry-run 둘 다) |
| 테스트 | 회귀 0 (174 PASS) |

## 다음 후보 (P8-09 punch)

ADR-056 의 -항목들 + ADR-055 후속 잔존:
- Concurrent repair (직렬 → parallel)
- Persistent scrubber state (DN 재시작 시 corrupt 보존)
- EC inline repair (ADR-046 worker 통합)
- `COORD_AUTO_REPAIR_INTERVAL` (operator-explicit ticker auto-repair)
- Repair throttling / rate-limit
- Notify on unrecoverable (모든 replica corrupt 시 hook)

각 별도 ADR. 본 ADR 의 단순함 보존.

## P8 wave 의 self-heal 풍경

P8-08 + P8-09 = anti-entropy 의 detection-action loop 두 갈래 모두 닫힘:

```
inventory missing (Merkle audit)        ← detection (ADR-054)
                ↓
        /anti-entropy/repair             ← action (ADR-055)
                ↓
        cluster heals

bit-rot (DN scrubber)                    ← detection (ADR-054)
                ↓
        /anti-entropy/repair?corrupt=1   ← action (ADR-056)
                ↓
        cluster heals
```

operator 명령어 한 번 = self-heal trigger. cluster 의 자기 보존이 코드로.

## 다음 글

P8-09 의 다음 polish ep, 또는 새로운 wave. 운영자 결정.
