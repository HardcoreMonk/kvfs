# ADR-024 — EC Stripe Rebalance (set-based, minimum-migration)

## 상태
Accepted · 2026-04-26 · Season 3 Ep.2 · ADR-008 의 명시적 비범위 보완

## 맥락

ADR-010 (rebalance) 은 `obj.Chunks` (replication mode) 만 처리한다. ADR-008 (Reed-Solomon EC) 가 추가한 `obj.Stripes` (EC mode) 는 **rebalance 가 무시** — 즉:

- 6 DN 클러스터에 EC(4+2) 객체를 PUT 한 후
- dn7 추가 + edge 재시작
- **rebalance --apply 가 EC 객체에 대해 0 migration** (무시됨)

ADR-008 의 명시적 비범위 ("EC stripe rebalance 는 별도 ADR-024 후보") 가 이번 ADR 의 출발점이다.

ADR-013 (auto-trigger) 는 rebalance.ComputePlan/Run 을 호출. EC 지원 추가 후 자동 으로도 EC 객체 정렬됨.

### 대안 검토

EC stripe 의 K+M shard 가 K+M DN 에 분산되어 있을 때, 어떤 마이그레이션 전략을 쓸 것인가:

| 방식 | 장점 | 단점 |
|---|---|---|
| **Position-tied (strict)** — shard[i] 가 항상 Pick(stripe_id, K+M)[i] 위치 | 결정적, "메타 없이 재현" 강한 보장 | DN 추가 시 cascade 이동 (1 DN 추가 → 다수 shard 이사) |
| **Set-based (이번 ADR)** — shard 가 desired set 에 속하기만 하면 OK, 위치 무관 | **최소 데이터 이동** (1 DN 추가 ≈ 1 shard 이사 per stripe) | 같은 stripe_id 라도 shard ↔ DN 매핑이 placement 결과와 다를 수 있음 |
| Position-canonicalize on every rebalance | 강한 결정성 | set-based 의 cascade 단점 그대로 |
| Per-shard 독립 placement (Pick 따로) | 단순 | shard 들이 같은 DN 에 콜리전 가능 → MDS 보장 깨짐 |

**Set-based 채택 근거**:
- EC 의 핵심 보장은 "어떤 K shard 도 데이터 복원 가능". **위치는 무관** (read 시 stripe.Shards[i].Replicas[0] 만 알면 됨)
- DN 추가는 잦지 않음 — 한 cycle 의 데이터 이동 비용을 최소화하는 것이 운영 친화
- 결정성은 stripe 수준에서 유지 (desired_set = sorted Pick(stripe_id, K+M) 는 결정적)
- shard 위치 매핑은 메타에 영구 저장 (이미 ChunkRef.Replicas 가 truth)

### Position-tied vs Set-based 의 양적 비교

(K=4, M=2) 클러스터에 dn7 추가 시:
- **Position-tied**: dn7 이 Pick 결과의 position k 에 삽입되면, 위치 k..K+M-1 의 shard 모두 한 칸씩 밀려 이동. 평균 ~3 shards/stripe migration
- **Set-based**: dn7 이 desired_set 에 들어가면 surplus DN 1개의 shard 만 이동. 정확히 1 shard/stripe migration (평균)

**Set-based 가 ~3배 효율**. 100 stripes × 16 KiB shard = 1.6 MiB vs 4.8 MiB.

## 결정

### 핵심 모델 — 세트 비교 + 결정적 unused 할당

```
for each EC object obj:
    for each stripe s in obj.Stripes:
        desired = coord.PlaceN(s.StripeID, K+M)        # 정렬된 K+M DN
        desired_set = set(desired)
        actual_set = { sh.Replicas[0] for sh in s.Shards }

        # 한 cycle 에 옮길 surplus DN 들의 shard
        unused_desired = sorted(desired_set − actual_set)

        for shard_index, sh in enumerate(s.Shards):
            addr = sh.Replicas[0]
            if addr in desired_set:        # shard 의 현 위치가 OK
                continue
            if not unused_desired:         # 비정상: 클러스터에 빈자리 없음
                continue
            new_addr = unused_desired.pop(0)
            emit Migration{Kind: "shard", StripeIndex: s_idx, ShardIndex: shard_index,
                           OldAddr: addr, NewAddr: new_addr,
                           ChunkID: sh.ChunkID, Size: sh.Size}
```

**결정성**: `sorted(desired_set − actual_set)` + shard index 순 iteration → 같은 입력 = 같은 plan.

### Migration 구조 확장

```go
type MigrationKind string
const (
    KindChunk MigrationKind = "chunk"  // replication
    KindShard MigrationKind = "shard"  // EC
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

JSON `omitempty` 로 무관 필드 숨김.

### migrateShard 흐름

```
1. data, _ := coord.ReadChunk(ctx, mg.ChunkID, [mg.OldAddr])
2. coord.PutChunkTo(ctx, mg.NewAddr, mg.ChunkID, data)
3. store.UpdateShardReplicas(bucket, key, stripeIdx, shardIdx, [mg.NewAddr])
4. ✓ migrated
```

부분 실패 처리:
- ReadChunk 실패 → 그 stripe 의 그 shard 는 그대로 (data unrecoverable from this addr — 별도 alert 후보)
- PUT 실패 → 메타 미갱신 → 다음 cycle 에서 재시도 (idempotent: 같은 chunk_id, 같은 data)
- UpdateShardReplicas 실패 → 새 자리에 데이터 있고 메타는 옛 자리. 다음 cycle 의 ComputePlan 이 같은 migration 을 다시 식별 → PUT 멱등 성공 → 메타 갱신 재시도

### Coordinator 인터페이스 확장 (rebalance 패키지의 mock-friendly subset)

```go
type Coordinator interface {
    PlaceChunk(chunkID string) []string
    PlaceN(key string, n int) []string                                              // 신규 (ADR-008 의 PlaceN 그대로)
    ReadChunk(ctx, chunkID, candidates) ([]byte, addr, error)
    PutChunkTo(ctx, addr, chunkID, data) error
}

type ObjectStore interface {
    ListObjects() ([]*store.ObjectMeta, error)
    UpdateChunkReplicas(bucket, key string, chunkIndex int, replicas []string) error
    UpdateShardReplicas(bucket, key string, stripeIndex, shardIndex int, replicas []string) error  // 신규 (ADR-008 의 store.UpdateShardReplicas 그대로)
}
```

### 변경 없음

- ADR-010 의 replication 알고리즘 (chunk 단위)
- ADR-012 의 GC (claimed-set 이미 Stripes iterate)
- ADR-013 의 auto-trigger (rebalance 인터페이스 재사용)
- 메타 스키마 (Migration 은 in-memory + JSON wire 형식만 변경, 영속 X)

## 결과

### 긍정

- **EC 객체도 cluster topology 변경에 자동 정렬** — 운영자 명령 또는 auto-trigger 로 동작
- **최소 데이터 이동** — set-based 가 position-tied 대비 ~3배 효율
- **알고리즘 0 변경** (replication side) — 기존 78 LOC 그대로, 새 dispatch 분기만 추가
- **자동 재시도** — 부분 실패 시 다음 cycle 에서 자연스럽게 따라잡음
- **테스트 격리** — Coordinator/ObjectStore 인터페이스 확장만, fake 로 EC plan 시뮬레이션 가능

### 부정

- **Position drift** — 같은 stripe 의 shard ↔ DN 매핑이 Pick 의 position-순서와 일치 안 할 수 있음. read 시 무관하지만 디버깅 시 헷갈림 가능. 메타가 진실
- **MultiPath 미지원** — shard 는 단일 DN 에 있음. 그 DN 죽으면 reconstruct 필요. R-way replication 처럼 "다른 replica 시도" 가 없음 (EC 의 본질)
- **K+M > N 시 unused 부족** — DN 이 K+M 미만이면 일부 shard 옮길 곳 없음. 현 cycle 은 skip, 다음에 재시도. 이는 클러스터 under-provisioning 상태이므로 운영자 alert 필요
- **partial migration 시 over-replication** — PUT 성공 후 meta 갱신 실패 → 같은 chunk_id 가 두 DN 에. 다음 rebalance + GC 가 정리 (rebalance 가 메타 갱신, GC 가 옛 자리 디스크 정리)

### 트레이드오프 인정

- "왜 position-tied 가 더 자연스럽지 않나?" — read/reconstruct 가 position 을 메타에서 가져오므로 추가 보장 무가치. set-based 가 데이터 이동 비용 절감
- "왜 unused 를 sorted 로 정렬?" — 결정성 (같은 입력 = 같은 plan). 무작위는 디버깅 어려움
- "왜 K+M < N 케이스를 안 다루나?" — Pick(K+M) 이 항상 K+M 개 distinct DN 반환 (N >= K+M 가정). 그렇지 않으면 EC PUT 자체가 실패. 이 ADR 은 정상 운영 상황을 가정

## 데모 시나리오 (`scripts/demo-mu.sh`)

```
1. ./scripts/down.sh + start dn1~dn6 + edge with chunk_size=16384
2. PUT 128 KiB body with X-KVFS-EC: 4+2 → 2 stripes × 6 shards
3. Disk before: dn1~dn6 각 2 shards = 12 total
4. Add dn7 + restart edge with EDGE_DNS=dn1..dn7
5. rebalance --plan → 일부 stripe 의 shard 가 dn7 로 이동 plan
6. rebalance --apply → 실제 이동
7. Disk after: dn7 가 일부 shard 보유, 한 DN 의 shard 수 감소
8. dn5+dn6 kill 시뮬레이션 → GET 여전히 성공 (RS reconstruct)
9. 두 번째 plan = 0 (멱등)
```

## 관련

- ADR-008 — Reed-Solomon EC (이 ADR 의 동기, 비범위 였던 stripe rebalance)
- ADR-009 — Rendezvous Hashing (PlaceN 의 결정적 출처)
- ADR-010 — Rebalance worker (ChunkIndex 패턴 + 안전 규칙 재활용)
- ADR-012 — GC (PUT 후 옛 자리 디스크 정리, 변경 0)
- ADR-013 — Auto-trigger (rebalance 인터페이스 재사용, EC 자동 적용)
- `internal/rebalance/rebalance.go` — Migration + ComputePlan + migrateShard 추가
- `scripts/demo-mu.sh` — 시연 (후속)
- `blog/08-ec-rebalance.md` — narrative episode (후속)

## 후속 ADR 예상

- **ADR-025 — EC repair queue**: shard 가 dead DN 에 있는 상태 감지 + reconstruct 후 새 DN 에 복구
- **ADR-026 — EC re-encoding (K, M 변경)**: 운영 중 (4+2) → (6+3) 같은 정책 변경
