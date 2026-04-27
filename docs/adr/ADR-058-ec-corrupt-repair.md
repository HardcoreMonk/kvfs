# ADR-058 — Anti-entropy EC corrupt repair (closes self-heal coverage)

상태: Accepted (2026-04-27)
시즌: P8-11 (ADR-055/056/057 후속)

## 배경

Self-heal coverage matrix 가 P8-10 시점에 다음과 같았다:

| detection | repair | status |
|---|---|---|
| Replication missing (audit) | ADR-055 copy | ✓ |
| Replication corrupt (scrubber) | ADR-056 force overwrite | ✓ |
| EC missing (audit) | ADR-057 inline RS Reconstruct | ✓ |
| **EC corrupt (scrubber)** | — | **빈 칸** |

EC corrupt 가 마지막 빈 칸. 시나리오:
- DN 의 file 이 디스크에 존재 — chunk_id 와 일치하는 path 에
- 그러나 byte 가 잘못 (bit-rot)
- 다른 replica 가 없음 (EC shards 는 unique — 각 shard 가 stripe 에서 distinct)
- **Reconstruct 로만 복구 가능**: K survivor 에서 RS Reconstruct → 잃은 shard
  recovers → 잘못된 file 위에 force overwrite

이전 wave 의 두 갈래:
- ADR-056 corrupt 처리: replication 만. EC 면 ec-deferred skip.
- ADR-057 EC inline: missing 만 처리. 기존 file 존재 시 PutChunkTo
  idempotent skip → corrupt 안 고쳐짐.

ADR-058 이 마지막 빈 칸 메움.

## 결정

`/anti-entropy/repair?ec=1&corrupt=1` 조합:

1. ADR-054 audit + ADR-056 corrupt 수집 (각 DN 의 `/chunks/scrub-status`)
2. ObjectMeta walk → EC stripe 단위 `repair.StripeRepair` 빌드 (ADR-057 패턴)
3. **ADR-058 신규**: 각 shard 가 corrupt set 에 있으면 `DeadShard{Force: true}` 로 등록
4. survivor 후보에서 corrupt 제외 (자기 자신이 corrupt 인 source 사용 안 함)
5. `repair.Run` 호출 → repairStripe 가 `Force` 보고 `PutChunkToForce` 사용 →
   기존 idempotent skip 우회 → 잘못된 file 위에 healthy bytes 작성

### Code surface

| 파일 | 변경 |
|---|---|
| `internal/repair/repair.go` | `Coordinator` 에 `PutChunkToForce` 메서드 추가, `DeadShard` 에 `Force bool` 필드 추가, `repairStripe` 가 `d.Force` 분기로 PUT 메서드 선택 |
| `internal/coord/anti_entropy.go` | EC plan 빌더 의 switch 에 `corruptHere` case 추가 (Force=true 등록), survivor 후보에서 corrupt 제외 |
| `internal/repair/repair_test.go` | `fakeCoord.PutChunkToForce` 추가 (의미상 PutChunkTo 와 동일 — 단위 test 가 idempotent skip 모델링 안 함) |

신규 cli flag 0 — 기존 `--corrupt --ec` 조합으로 활성. operator surface 단순.

### Self-heal 풀 커버리지

```
operator: kvfs-cli anti-entropy repair --coord URL [--corrupt] [--ec]

  flag 조합 → 다루는 detection
  (none)        → replication missing (ADR-055)
  --corrupt     → replication missing + corrupt (ADR-056)
  --ec          → replication missing + EC missing (ADR-057)
  --corrupt --ec → 모든 4 종류 (ADR-058 = 본 ADR)
```

운영 명령 한 번 (`--corrupt --ec`) = full cluster self-heal.

## 비채택 대안

| 대안 | 기각 사유 |
|---|---|
| ADR-056 의 corrupt path 가 EC 도 처리 (force overwrite + 자체 reconstruct) | reconstruct 코드 복제. ADR-057 의 repair.Run 위임 패턴 깸 |
| 새 query param `?ec_corrupt=1` (corrupt 와 ec 조합 대신) | 운영자 surface 더 복잡. 기존 두 flag 조합으로 자연스러움 |
| repair package 에 별도 `RunCorrupt` 함수 | 코드 중복. flag/필드로 충분 |
| corrupt EC 도 force=false (현재 path) 시 skip 발생 무시 | 의미가 깨짐 — operator 가 `--corrupt --ec` 보내면 실제 fix 기대 |
| Force per-stripe (모든 shard 강제 overwrite) 가 아닌 per-shard 선택 | 현재 구현 — DeadShard.Force 가 per-shard. healthy 인 다른 shard 들은 default PUT (file 없으니 정상). corrupt 인 shard 만 force. precise |

## 결과

+ **Self-heal coverage 100%**: replication missing/corrupt + EC missing/corrupt
  모두 anti-entropy 단일 명령으로.
+ **No new endpoint**: 기존 `/repair` 의 query param 조합 (`ec=1`, `corrupt=1`)
  으로 활성. operator surface stable.
+ **No new reconstruct code**: ADR-057 의 repair.Run 위임 그대로. ADR-058 가
  추가한 것은 force flag 의 per-shard plumbing 뿐.
+ **demo 7 stages PASS**: corrupt EC inject → scrubber detect → ec=1 alone
  은 안 잡음 (audit 만 보니까) → ec=1+corrupt=1 → reconstruct + force
  overwrite → next scrub clean → GET sha256 정상.
+ **per-shard granularity**: stripe 의 일부 shard 만 corrupt 면 그 shard 만
  Force=true. healthy 다른 shard 들은 영향 0.
- **per-shard success 정밀도**: ADR-057 의 한계 그대로 (RunStats 가 stripe
  단위). 후속 ADR 후보.
- **repair package interface 변경**: `Coordinator.PutChunkToForce` 추가 —
  internal interface 만 영향, fakeCoord 도 같이 업데이트.

## 호환성

- 새 cli flag 0 — `--corrupt` + `--ec` 조합으로 활성.
- 기존 cli `--corrupt` 단독 (replication 만) 또는 `--ec` 단독 (missing 만)
  동작 0 변경.
- `repair.Coordinator` 인터페이스 1 메서드 추가 — internal 사용처 두 군데
  (internal/coord/anti_entropy.go, internal/repair/repair_test.go) 모두
  업데이트.
- `repair.DeadShard.Force` 필드 추가 — JSON `omitempty` 라 기존 plan
  serialize 영향 0.

## 검증

- `scripts/demo-anti-entropy-repair-ec-corrupt.sh` 7 stages live:
  1. PUT 1MB EC (4+2)
  2. overwrite shard file with garbage
  3. wait 4s → scrubber flags
  4. ec=1 alone → 0 work (audit doesn't see file-existing-but-corrupt)
  5. ec=1 + corrupt=1 → ec-inline 1 OK + ec-summary OK
  6. next scrub pass → corrupt_count=0
  7. GET via edge → 1MB sha256 intact
- 전체 test suite **174 PASS** (회귀 0).

## 후속 (P8-12 후보)

- per-shard 정밀 success/failure
- Concurrent repair parallel
- `COORD_AUTO_REPAIR_INTERVAL`
- Repair throttling
- Persistent scrubber state
- Multi-tier hierarchical Merkle
- Peer-to-peer DN self-heal
- Notify on unrecoverable

각 별도 ADR. ADR-058 이 self-heal coverage 빈 칸 모두 채웠으니 P8 wave 의
**functional milestone**.
