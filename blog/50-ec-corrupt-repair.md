# Episode 50 — EC corrupt repair: self-heal coverage 마지막 빈 칸

> **Series**: kvfs — distributed object storage from scratch
> **Wave**: P8-11 (ADR-055 후속) · **Episode**: 4 (P8 wave self-heal milestone)
> **연결**: [ADR-058](../docs/adr/ADR-058-ec-corrupt-repair.md) · [demo-anti-entropy-repair-ec-corrupt](../scripts/demo-anti-entropy-repair-ec-corrupt.sh)

---

## 4 종류 detection, 4 종류 repair

P8-10 시점의 self-heal coverage matrix:

| | repair via | ADR |
|---|---|---|
| Replication missing | replication copy | ✓ ADR-055 |
| Replication corrupt | force overwrite | ✓ ADR-056 |
| EC missing | RS Reconstruct via repair.Run | ✓ ADR-057 |
| **EC corrupt** | — | **빈 칸** |

EC corrupt 는 까다롭다:
- File 이 디스크에 존재 — chunk_id 와 일치하는 path 에
- 그러나 byte 가 잘못 (bit-rot)
- 다른 replica 가 없음 (EC shards 는 unique — 각 shard 가 stripe 에서 distinct)

**복구 = K survivor 에서 RS Reconstruct → 잃은 shard recovers → file
overwrite**

ADR-057 의 EC inline repair 가 reconstruct 부분 가졌다 — 하지만 그 worker
의 PUT 은 idempotent (file 존재 시 skip). corrupt 시나리오는 file IS
디스크에 → skip → corrupt 안 고쳐짐.

ADR-056 의 corrupt force overwrite 는 replication 만. EC 면 ec-deferred.

ADR-058 이 두 갈래를 합친다.

## 변경 — DeadShard.Force

repair 패키지의 `DeadShard` 에 `Force bool` 필드 추가. repairStripe 가 분기:

```go
put := coord.PutChunkTo
if d.Force {
    put = coord.PutChunkToForce
}
if err := put(ctx, d.NewAddr, d.ChunkID, data); err != nil {
    ...
}
```

`Coordinator` 인터페이스에 `PutChunkToForce` 메서드 추가. internal/coordinator
는 이미 P8-09 에서 구현해뒀으니 자동.

anti-entropy 의 EC plan 빌더가 새 case 처리:

```go
switch {
case missingHere:
    rep.DeadShards += { Force: false }  // ADR-057
case corruptHere:                        // ADR-058 신규
    rep.DeadShards += { Force: true }
case healthyHere:
    rep.Survivors += { ... }
}

// Survivor 후보에서 corrupt 제외 — 자기 source 가 corrupt 면 사용 X
if !hasCorrupt(corruptByDN[addr], chunkID) {
    rep.Survivors += { ... }
}
```

per-shard granularity. stripe 의 일부 shard 만 corrupt 면 그 shard 만
Force=true. healthy 다른 shard 들은 영향 0.

## Self-heal full coverage

```
operator: kvfs-cli anti-entropy repair --coord URL [--corrupt] [--ec]

flag 조합               다루는 detection
(none)                  replication missing
--corrupt               replication missing + corrupt
--ec                    replication missing + EC missing
--corrupt --ec          모든 4 종류 (ADR-058)
```

operator 가 `--corrupt --ec` 한 번 = full cluster self-heal. 새 endpoint 0,
새 cli flag 0 (기존 두 flag 조합).

## 데모

```
$ ./scripts/demo-anti-entropy-repair-ec-corrupt.sh

==> stage 1: PUT 1 MB EC (4+2)
==> stage 2: overwrite one shard file with garbage on dn1
==> stage 3: wait 4s → scrubber flags 805049aa..
==> stage 4: ec=1 alone → ec-inline outcomes: 0
    ✓ audit doesn't see file-existing-but-corrupt
==> stage 5: ec=1 + corrupt=1
    {"ec_inline":1,"ec_summary":{"ok":true}}
    ✓ corrupt EC shard reconstructed + force-overwritten
==> stage 6: next scrub → corrupt_count=0
==> stage 7: GET via edge → 1MB sha256 intact

=== PASS ===
```

7 stages. 4 detection 채널 모두 자동 복구.

## "왜 ec=1 alone 이 corrupt 안 잡나"

audit 의 `MissingFromDN` 은 Merkle tree comparison 결과. file 이 디스크에
있으면 Merkle bucket 에 들어감 → audit 입장에서는 missing 아님.

corrupt 는 scrubber 의 별도 detection 채널. anti-entropy/repair 가 corrupt
처리하려면 `corrupt=1` 필요 — 그제서야 `/chunks/scrub-status` 를 fetch.

opt-in 의 의미: corrupt detection 이 audit 보다 비싸 (DN 마다 추가 RTT).
운영자가 명시적으로 활성.

## "Force PUT 이 위험하지 않나"

DN 의 `handlePut` 가 force 모드에서도 sha256(body) == chunk_id 강제 검증.
잘못된 body 는 force 든 아니든 통과 못 함. ADR-005 의 invariant 가 force
path 에서도 보호.

추가로: ADR-058 의 corrupt repair 는 RS Reconstruct 의 output 을 PUT 함.
RS 알고리즘은 deterministic — K survivors 만 정확하면 missing/corrupt
shard 의 정확한 byte 를 만든다. force PUT 의 body 는 항상 정확한 sha256.

## ADR-005 의 dividend, 또

이 ep 가 ADR-005 dividend 의 마지막 모습:

- repair worker 의 PUT 도 sha256 검증 — reconstruct 가 잘못해도 DN 이 거부
- force overwrite 도 안전 — DN 이 sha 검증
- scrubber 의 corrupt detection 도 sha 비교 — chunk_id 가 진실의 표

ADR-005 (2 줄 짜리 결정) 이 ADR-008/025/046/054/055/056/057/058 까지 거듭
가치. **content-addressable + sha256 chunk_id** 가 분산 storage 의 가장
근본적 invariant 였음을 retrospect 가 확정.

## 정량

| | |
|---|---|
| 코드 | repair package +5 LOC (interface + DeadShard field + repairStripe 분기), anti_entropy.go +30 LOC (corrupt switch case + survivor exclusion), repair_test.go +5 LOC (fakeCoord.PutChunkToForce) |
| 새 endpoints / cli flags | 0 (기존 `corrupt=1 + ec=1` 조합) |
| 데모 | 7 stages, 1MB EC (4+2) full corrupt → repair → verify |
| 테스트 | 회귀 0 (174 PASS) |

## P8 wave 의 functional milestone

ADR-055 + 056 + 057 + 058 = anti-entropy 의 detection-action 4 채널 모두
closed. 더 이상 self-heal 의 functional gap 없음.

남은 polish (P8-12+ 후보):
- per-shard 정밀 success/failure
- Concurrent repair parallel
- Auto-repair on schedule (operator-explicit)
- Repair throttling / rate-limit
- Persistent scrubber state
- Multi-tier hierarchical Merkle
- Peer-to-peer DN self-heal
- Notify on unrecoverable

모두 functional 이 아닌 operational quality. polish 의 한계 효용 점점
줄어드는 시점.

## 다음

P8-12 polish ep 또는 wave 닫기. 자연스러운 정리 시점.
