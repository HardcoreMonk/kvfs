# kvfs from scratch, Ep.8 — EC stripe 도 따라온다: dn7 추가에 정확히 N개만 이동

> Season 3 · Episode 2. ADR-008 의 EC 객체에 ADR-010 의 rebalance 능력을 부여. **set-based** 알고리즘으로 position-tied 대비 ~3배 효율. 알고리즘 변경 ~80 LOC, 테스트 +7, 라이브 데모 한 흐름 PASS.

## Ep.6 가 남긴 그늘

ADR-008 Reed-Solomon EC 의 명시적 비범위 (그 ADR 의 Negative 항목):

> **EC rebalance 미구현** — 이번 episode 범위 밖. shard 의 desired DN 이 변할 때 (DN 추가) 마이그레이션 필요. ADR-013 후보

Ep.7 (auto-trigger) 가 자동화를 깔았지만, EC 객체 자체가 rebalance 대상이 아니었으니 자동도 무관. 즉 **demo-zeta 패턴이 EC 에는 그대로 깨진 채** 였습니다:

```
6 DN EC(4+2) 클러스터 → PUT → 12 shards 분산
dn7 추가 → kvfs-cli rebalance --plan → 0 migrations  ← bug
```

ADR-024 가 닫는 그늘.

## 가장 단순한 해법은 거의 항상 잘못된 답

처음 떠오르는 EC rebalance 디자인: "Pick(stripe_id, K+M) 의 결과 순서대로 shard[i] 가 desired_dns[i] 위치에 있도록 강제." 깔끔해 보입니다.

문제: 6 DN 에 dn7 추가 시 Pick 결과의 순서가 어떻게 변하는지 봅시다.
- 이전: `[dn1, dn2, dn3, dn4, dn5, dn6]`
- 추가 후: `[dn1, dn2, dn3, dn7, dn4, dn5]` (dn7 이 점수 4위로 삽입, dn6 탈락)

Position-tied strict 라면:
- shard[3] 위치가 dn4 → dn7 (이동)
- shard[4] 위치가 dn5 → dn4 (이동)  ← 데이터는 그대로인데 이사
- shard[5] 위치가 dn6 → dn5 (이동)  ← 또 이사

**3 shards 이사**. dn4 의 shard[3] 데이터가 dn7 으로 가고, dn5 의 shard[4] 가 dn4 로 가고, dn6 의 shard[5] 가 dn5 로 가는 cascade. 한 DN 추가에 3배 트래픽.

실제로 필요한 것: **dn6 가 들고 있던 shard 만 dn7 으로 이사**. 1 shard 면 충분.

## Set-based — 위치 무관, 멤버십만 본다

EC 의 read 경로는 "shard[i] 가 어느 DN 에 있는가" 를 메타에서 가져옵니다 (`stripe.Shards[i].Replicas[0]`). **위치 (index i) 와 DN 의 매핑은 메타가 진실** — placement 결과와 일치할 필요 없음.

이 사실이 ADR-024 의 핵심 통찰:

```
desired_set = set(Pick(stripe_id, K+M))   # 정렬된 K+M DN
actual_set  = set(shard.Replicas[0] for shard in stripe.Shards)

surplus = actual_set - desired_set   # 떠나야 할 DN
unused  = desired_set - actual_set   # 새로 받을 DN

for shard_index, shard in enumerate(stripe.Shards):
    if shard.Replicas[0] in desired_set:
        continue   # 멤버십 OK, 위치 무관
    new_addr = unused.pop_first()  # 정렬된 unused 첫 요소
    migrate(stripe_index, shard_index, old=shard.Replicas[0], new=new_addr)
```

dn7 추가 시:
- desired_set = {dn1..dn5, dn7} (dn6 빠짐)
- actual_set = {dn1..dn6}
- surplus = {dn6}, unused = {dn7}
- shard[5] 가 dn6 에 있음 → desired_set 에 없음 → dn7 으로 migrate
- 다른 shard[0..4] 는 desired_set 멤버 → 그대로 유지

**정확히 1 migration per stripe**. position-tied 대비 ~3배 효율.

## Migration 구조 확장 — Kind 로 분기

```go
type MigrationKind string
const (
    KindChunk MigrationKind = "chunk"  // ADR-010 replication
    KindShard MigrationKind = "shard"  // ADR-024 EC
)

type Migration struct {
    Bucket, Key string
    Kind MigrationKind
    ChunkID string
    Size int64

    // KindChunk
    ChunkIndex int
    Actual, Desired, Missing, Surplus []string

    // KindShard
    StripeIndex int
    ShardIndex  int
    OldAddr, NewAddr string
}
```

JSON `omitempty` 로 무관 필드 숨김. `Run` 의 dispatch:

```go
func migrateOne(ctx, coord, st, mg) migrateResult {
    switch mg.Kind {
    case KindShard:
        return migrateShard(ctx, coord, st, mg)
    default:
        return migrateChunk(ctx, coord, st, mg)
    }
}
```

`migrateShard` 는 단일 source / 단일 dest:

```go
func migrateShard(ctx, coord, st, mg) (out migrateResult) {
    data, _, err := coord.ReadChunk(ctx, mg.ChunkID, []string{mg.OldAddr})
    if err != nil { ... }                   // dn dead → next cycle retry
    if err := coord.PutChunkTo(ctx, mg.NewAddr, mg.ChunkID, data); err != nil {
        ...                                 // PUT fail → meta untouched
    }
    if err := st.UpdateShardReplicas(mg.Bucket, mg.Key, mg.StripeIndex, mg.ShardIndex,
                                      []string{mg.NewAddr}); err != nil {
        ...                                 // over-replicated; next cycle re-detects
    }
    return migrateResult{ok: true, bytesCopied: int64(len(data))}
}
```

50줄 안 됨. 안전성: PUT 실패 시 메타 미갱신, 메타 갱신 실패 시 over-replicate 일시. 둘 다 다음 cycle 의 idempotent retry 로 자연 복구.

## 테스트가 검증한 7 케이스

`internal/rebalance/rebalance_test.go` 추가:

| 케이스 | 검증 |
|---|---|
| `TestECPlan_AllInDesired_NoMigrations` | desired_set 포함 시 plan 0 |
| `TestECPlan_OneSurplusOneMissing_OneMigration` | dn7 추가 = 정확히 1 migration |
| `TestECPlan_DeterministicAssignment` | sorted unused → 결정적 (dn7, dn8 두 개일 때) |
| `TestECRun_HappyPath_MovesShardAndUpdatesMeta` | dn7 disk 에 shard, 옛 disk (dn6) 도 보존 (rebalance never deletes) |
| `TestECRun_ReadFails_NoMetaChange` | dn 죽으면 메타 안 건드림, 다음 cycle retry |
| `TestECRun_Idempotent_SecondRunZero` | 같은 입력 두 번 → 두 번째 0 |
| `TestECPlan_MultipleStripes` | 2 stripe 객체, 한 stripe 만 migration 필요 → 정확히 1 |

기존 chunk rebalance 8 + EC 7 = **15 tests PASS**. 전체 73 unit tests.

## 라이브 데모 — `demo-mu.sh`

```
=== μ demo: EC stripe rebalance (ADR-024) ===

[1/8] Reset cluster + start 6 DNs (K+M=6)
  ✅ 6 DNs + edge up

[3/8] PUT object with X-KVFS-EC: 4+2 (2 stripes × 6 shards)
  ✅ PUT done

[4/8] Disk before adding dn7 (each DN holds 2 shards = 12 total)
  dn1  shards = 2
  dn2  shards = 2
  dn3  shards = 2
  dn4  shards = 2
  dn5  shards = 2
  dn6  shards = 2

[5/8] Add dn7 + restart edge with 7-DN config
  dn7  shards = 0     ← write-time placement only

[6/8] kvfs-cli rebalance --plan -v
📋 Rebalance plan — scanned 1 objects, 2 total migrations (0 chunks, 2 EC shards)

  [shard]  demo-mu/ec-rebal.bin  stripe=0 shard=5  size=16384  dn3:8080 → dn7:8080
           chunk=b6335d27a43db742..
  [shard]  demo-mu/ec-rebal.bin  stripe=1 shard=5  size=16384  dn1:8080 → dn7:8080
           chunk=d9763b0ff3b7cd9f..
```

각 stripe 정확히 1 shard 만 옮김 — set-based 의 약속 그대로. CLI 가 신규 `[shard]` 렌더링 (vs 기존 `[chunk]`) 으로 표시.

```
[7/8] kvfs-cli rebalance --apply
⚙️  Rebalance applied (concurrency=4)
   scanned:      1
   planned:      2
   migrated:     2
   failed:       0
   bytes_copied: 32768

  Disk after rebalance:
  dn1  shards = 2     ← 옛 shard 그대로 (rebalance never deletes)
  dn2  shards = 2
  dn3  shards = 2
  dn4  shards = 2
  dn5  shards = 2
  dn6  shards = 2
  dn7  shards = 2     ← 신규 도착

[8/8] GET still works + idempotency check
  ✅ GET sha256 matches

📋 Rebalance plan — scanned 1 objects, 0 total migrations (0 chunks, 0 EC shards)
   ✅ Cluster is in HRW-desired state. No work to do.
```

세 가지 신호:

1. **planned 2, migrated 2, 32768 bytes** — 정확히 2 stripe × 16 KiB shard
2. **dn7 = 2 shards** — set-based 가 양 stripe 에 1 shard 씩 정확히 할당
3. **dn1, dn3 = 2 still** — rebalance 가 옛 shard 유지 (GC 가 별개로 청소). over-replicated 상태 유지

총 disk = 14 shards (= 12 + 2 over-replicated). `kvfs-cli gc --apply --min-age 0` 후 12 로 정착.

## ADR-013 (auto-trigger) 와의 자연 연결

ADR-013 은 `rebalance.ComputePlan/Run` 인터페이스를 호출. 이 episode 의 ADR-024 가 같은 인터페이스 안에서 EC 분기를 추가 → **auto-trigger 가 자동으로 EC 객체 정렬**.

EDGE_AUTO=1 으로 클러스터 띄워 두면 dn7 추가 후 다음 ticker 사이클 (기본 5분) 내에 EC stripe 가 자동 균형 잡힘. 운영자 명령 0번.

## 명시적 비범위 — 다음 ADR 들

1. **EC repair queue (ADR-025 후보)** — shard 의 OldAddr DN 이 영구 죽었을 때 reconstruct 후 새 자리에 복구. 현재는 ReadChunk 실패 → Failed 카운트만 (다음 cycle retry 도 또 죽으면 의미 없음). repair 는 K shards 모아 reconstruct + 새 자리 PUT 의 다른 흐름
2. **EC re-encoding (ADR-026 후보)** — (4+2) → (6+3) 같은 운영 중 정책 변경
3. **K+M > N 시나리오** — 클러스터 under-provisioning 상태. 이번 ADR 은 정상 운영 가정 (skip)
4. **Position canonicalization** — set-based 가 position drift 를 허용. 일관성을 더 강하게 원하면 별도 옵션. 현 트레이드오프 유지

## 다음 편 예고

- **Ep.9 후보**: ADR-025 EC repair queue — DN 영구 사망 시 reconstruct 후 자동 복구
- **Ep.10 후보**: ADR-022 Multi-edge leader election — auto loop 의 single-instance 가정 깨기
- **Ep.11 후보**: ADR-014 Meta backup/HA — bbolt 메타 손상 대비

## 직접 돌려보기

```bash
git clone https://github.com/HardcoreMonk/kvfs   # public 시점 기준
cd kvfs
docker build -t kvfs-edge:dev --target kvfs-edge .
docker build -t kvfs-dn:dev   --target kvfs-dn   .
docker run --rm -v "$PWD:/src" -w /src -e CGO_ENABLED=0 \
  golang:1.26-alpine sh -c 'go build -o ./bin/kvfs-cli ./cmd/kvfs-cli'

./scripts/demo-mu.sh
```

매번 chunk_id / stripe_id 는 다릅니다. 그러나 항상:

- dn7 추가 전 — dn1..dn6 각 2 shards (균등)
- rebalance plan = 정확히 2 (한 stripe 당 1)
- apply 후 — dn7 = 2 shards, 옛 자리 그대로 (over-replicated)
- 두 번째 plan = 0 (멱등)
- GET sha256 일치

위 5 불변식이 ADR-024 의 약속과 일치합니다.

## 참고 자료

- 이 ADR: [`docs/adr/ADR-024-ec-stripe-rebalance.md`](../docs/adr/ADR-024-ec-stripe-rebalance.md)
- 구현 변경: [`internal/rebalance/rebalance.go`](../internal/rebalance/rebalance.go) `planEC` + `migrateShard`
- 테스트: [`internal/rebalance/rebalance_test.go`](../internal/rebalance/rebalance_test.go) — `TestEC*` 7 케이스
- CLI 렌더링: [`cmd/kvfs-cli/main.go`](../cmd/kvfs-cli/main.go) `printMigration`
- 데모: [`scripts/demo-mu.sh`](../scripts/demo-mu.sh)
- 의존: ADR-008 EC + ADR-010 rebalance + ADR-009 placement (PlaceN)
- 이전 episode (auto-trigger): [`blog/07-auto-trigger.md`](07-auto-trigger.md)

---

*kvfs는 교육적 레퍼런스입니다. 프로덕션에 배포하지 마세요. Season 3 운영성 트랙이 한 episode 더 진행됐습니다 — auto-trigger + EC rebalance 둘이 결합하면, EDGE_AUTO=1 클러스터가 EC 객체까지 자동으로 균형 잡습니다. 다음은 EC repair queue 또는 multi-edge HA — 어느 쪽이든 "혼자 살아있는 클러스터" 의 또 한 조각.*
