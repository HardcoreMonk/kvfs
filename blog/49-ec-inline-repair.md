# Episode 49 — EC inline repair: 한 명령으로 replication + EC 모두 self-heal

> **Series**: kvfs — distributed object storage from scratch
> **Wave**: P8-10 (ADR-055 후속) · **Episode**: 3
> **연결**: [ADR-057](../docs/adr/ADR-057-ec-inline-repair.md) · [demo-anti-entropy-repair-ec](../scripts/demo-anti-entropy-repair-ec.sh)

---

## 두 worker, 두 trigger

지금까지 kvfs 의 self-heal:

```
inventory missing replication → /anti-entropy/repair (ADR-055)
inventory missing EC          → kvfs-cli repair --apply --coord (ADR-046)
bit-rot replication           → /anti-entropy/repair?corrupt=1 (ADR-056)
```

운영 friction: replication 은 자동, EC 는 별도 명령. 그리고 **ADR-046 의
ComputePlan 은 anti-entropy 가 발견하는 시나리오 (DN alive but file
missing) 를 안 잡음** — DN registry 에서 빠진 chunks 만 처리.

ADR-057 이 한 번에 통합:

```
single command (anti-entropy/repair?ec=1) → replication + EC 모두
```

## 핵심 insight — 같은 worker, 다른 input

ADR-046 의 `repair.Run(plan)` 은 generic 한 reconstruct 작업자. 입력은
`Plan` (각 stripe 에 대한 SurvivorRef + DeadShard). worker 는 어떻게 dead
인지 모름 — Plan 만 받아서 Reed-Solomon Reconstruct + PUT.

기존 ADR-046: ComputePlan 이 `dead = DN registry 에서 사라짐` 으로 판정.
ADR-057: anti-entropy 가 `dead = audit 의 MissingFromDN` 으로 판정 → Plan
빌드 → **같은 repair.Run 호출**.

reconstruct 코드 중복 0. anti-entropy 가 입력만 다르게 만들어서 같은
worker 위임.

## 코드

```go
// anti_entropy.go runAntiEntropyRepair, EC=true 일 때:

if opts.EC {
    // ObjectMeta walk → 영향 받은 stripe 단위 StripeRepair 빌드
    for _, o := range objs {
        if !o.IsEC() { continue }
        for si, st := range o.Stripes {
            rep := &repair.StripeRepair{Bucket: o.Bucket, ...}
            for shi, sh := range st.Shards {
                if anti-entropy 가 missing 으로 mark:
                    rep.DeadShards += { ShardIndex: shi, NewAddr: shard's original DN }
                else:
                    rep.Survivors += { ShardIndex: shi, ChunkID: sh.ChunkID, Addr: ... }
            }
            if len(DeadShards) > 0:
                ecPlanByKey[(bucket, key, si)] = rep
        }
    }
}

// 나중에:
if opts.EC && len(ecPlanByKey) > 0 && !opts.DryRun {
    plan := repair.Plan{Repairs: ...}
    stats := repair.Run(ctx, s.Coord, s.Store, plan, 1)  // ← 기존 worker
    // outcomes 업데이트
}
```

`NewAddr = shard 의 OldAddr` (원위치 복원) — 다른 위치로 옮기지 않음.
ADR-046 의 worker 가 `coord.PutChunkTo(NewAddr, ...)` 호출하면 file 이
없는 상태이므로 그대로 작성 (idempotent skip 안 발생).

## 데모

```
$ ./scripts/demo-anti-entropy-repair-ec.sh

==> stage 1: PUT 1 MB object with EC (4+2)
    expected sha256: d32ef32f0e6cf4c2..

==> stage 2: rm one shard file from one of the DN
    deleted 851a8252.. from dn3:8080

==> stage 3: default repair (no ec=1) → ec-deferred (back-compat)
    {"repaired":0,"skipped_ec":1}

==> stage 4: repair?ec=1 → RS Reconstruct + write back
    {"ec_inline":1,
     "ec_summary":{"target_dn":"(ec-summary)","mode":"ec-summary","ok":true}}

==> stage 5: audit clean again
==> stage 6: GET via edge → 1MB body sha256 intact

=== PASS ===
```

6 stages. EC missing → audit detect → ec=1 trigger → ADR-046 worker
inline 호출 → reconstruct + PUT → audit clean → GET intact.

## 운영자 표면

cli flag 한 개:

```
$ kvfs-cli anti-entropy repair --coord URL --ec
```

`--ec` 미설정 시 ADR-055 동작 그대로 (EC 는 ec-deferred). 명시적 opt-in.

## ADR-046 ComputePlan 과의 분리

ADR-046 의 dead 판정:
```go
addr := sh.Replicas[0]
if _, ok := liveDNs[addr]; ok {
    // alive
} else {
    // dead — DN registry 에서 빠짐
}
```

ADR-057 의 dead 판정:
```go
if anti-entropy 의 dnHas[addr][chunk_id] 에 있으면 missing
```

**서로 다른 시나리오**:
- ADR-046: DN 자체가 cluster 에서 사라짐 (operator dns remove + outage)
- ADR-057: DN 은 살아있는데 chunk file 만 missing (disk 교체, rm 사고)

두 trigger 가 같은 worker (`repair.Run`) 를 다른 input 으로 호출 — generic
worker 의 가치.

## per-shard success 정확도

ADR-057 의 -항목: `repair.Run` 의 `RunStats` 는 stripe 단위 (Stripes /
Repaired / Failed / Errors). per-shard 정확한 success → 매핑이 brittle.

현재 구현: `len(stats.Errors) == 0` 면 모든 ec-inline outcome OK, 그렇지
않으면 ambiguous. 운영자가 ec-summary outcome 도 같이 보면 정확한 진단
가능.

precise per-shard 매핑은 후속 (ADR-046 worker 가 per-shard error 반환
하도록 격상 — 별도 ADR).

## EC corrupt 는 다음

본 ADR 은 EC **missing only**. corrupt EC (file 존재, 바이트 잘못) 는 더
까다로움:
- repair worker 의 `coord.PutChunkTo` 가 idempotent (file 존재 시 skip)
- corrupt 시나리오에서 file IS 존재 — PUT skip 됨

ADR-056 은 replication 의 force overwrite 만. EC corrupt 는 P8-11 후보.

## ADR-005 의 dividend, 또

repair worker 의 PUT 도 sha256(body) == chunk_id 검증. RS Reconstruct 의
output 이 우연히 wrong 한 sha256 을 만들면 PUT 자체가 fail — invariant
가 reconstruct path 에서도 보호.

ADR-005 의 단순한 결정 (chunk_id = sha256) 이 ADR-008/046/056/057 까지
거듭 dividend.

## 정량

| | |
|---|---|
| 코드 | anti_entropy.go +60 LOC (Plan 빌드 + outcome wiring), import 1줄 |
| 새 코드 in repair package | 0 (기존 `repair.Run` 그대로 사용) |
| 새 endpoints / env | 0 (`/repair` 에 `?ec=1` query param 추가만) |
| cli flag | `--ec` |
| 데모 | 6 stages, 1MB EC (4+2) full self-heal |
| 테스트 | 회귀 0 (174 PASS) |

## P8-10 의 진척

P8-08 + P8-09 + P8-10 = anti-entropy 의 detection-action 통합:

| detection | action | repair via |
|---|---|---|
| Inventory missing (replication) | ✓ ADR-055 | replication copy |
| Bit-rot (replication) | ✓ ADR-056 | force overwrite |
| Inventory missing (EC) | ✓ ADR-057 | RS Reconstruct |
| Bit-rot (EC) | (P8-11 후보) | force + RS |

cluster 의 self-heal coverage 가 거의 다 — replication 은 100%, EC 는
missing 까지. EC corrupt 만 남은 빈 칸.

## 다음

P8-11 (EC corrupt) 또는 다른 polish ep, 또는 새 wave. 운영자 결정.
