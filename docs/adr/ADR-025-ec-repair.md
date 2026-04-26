# ADR-025 — EC Repair Queue (죽은 shard 를 K 개 survivor 로 reconstruct)

## 상태
Accepted · 2026-04-26 · Season 3 Ep.3

## 맥락

ADR-024 (EC stripe rebalance) 는 shard 의 OldAddr 가 **여전히 살아있을 때** 만 동작:

```
desired_set ≠ actual_set → migrate(OldAddr → NewAddr)
  ↓
data, _, err := coord.ReadChunk(ctx, ChunkID, [OldAddr])
  ↓ err: dn dead
return failed   ← 데이터 손실 가까운 상태, 다음 cycle 도 같은 실패
```

ADR-027 dynamic DN registry 와 결합하면 흔한 시나리오:
1. dn4 가 hardware 영구 사망
2. 운영자가 `kvfs-cli dns remove dn4:8080` (registry 에서 제거)
3. ADR-024 rebalance: dn4 shard 들을 새 DN 으로 옮기려 시도 → ReadChunk(dn4) **모두 fail**
4. EC 객체의 일부 shard 가 dn4 에만 있던 상태 → cluster 가 손상 진입

EC 의 본래 약속 — "K+M shard 중 K 개로 데이터 복원" — 을 재배치 경로에서도 활용해야 함. 즉:
- 죽은 shard 의 데이터를 **같은 stripe 의 살아있는 K shard** 로 reconstruct
- 재구성된 shard 를 desired DN 에 배치
- 메타 갱신

이게 EC repair queue.

### Rebalance vs Repair

| | rebalance (ADR-024) | repair (이번 ADR) |
|---|---|---|
| 트리거 | desired ≠ actual | shard 의 OldAddr 가 cluster 에서 사라짐 |
| Source | OldAddr 의 그 shard | 같은 stripe 의 K survivors |
| 알고리즘 | read 1 + write 1 (단순 copy) | RS Reconstruct (K shards 모음 + 행렬 역산) + 새 위치 PUT |
| 비용 | shard 1 개 크기 데이터 | K 배 (K survivors fetch) |
| 실패 시점 | OldAddr 죽음 → 무력 | survivors 가 K 개 미만이면 무력 |

직교 — repair 가 rebalance 의 fallback path. 운영자/auto-trigger 가 둘 다 호출.

### 대안 검토

| 방식 | 장점 | 단점 |
|---|---|---|
| **In-edge plan + run (이번 ADR, ADR-024 패턴 재사용)** | 일관성, 추가 daemon 0 | edge 1개 가정 |
| Rebalance fallback ("PUT fail 시 repair") | 사용자 호출 1번 | rebalance + repair 의 실패 모드 혼재, 디버깅 어려움 |
| 자동 health-checker daemon | 즉시 반응 | DN heartbeat / 죽음 감지 별도 인프라 |
| External repair tool (cli script) | edge 코드 변경 0 | bbolt 직접 접근 = 메타 일관성 위험 |

ADR-024 와 같은 패턴 (separate ComputePlan + Run, edge admin endpoint, CLI subcommand). 통합도 명확 + 운영자가 의도 별로 호출.

## 결정

### "Dead shard" 판정

shard 가 dead 라고 판정하는 조건:
- `shard.Replicas[0]` 가 **현재 cluster DNs 셋에 없음** (`coord.DNs()` 결과 미포함)

이는 ADR-027 dynamic registry 와 자연 통합 — 운영자가 `dns remove` 한 DN 의 shard 가 자동으로 repair 대상.

대안 ("ping DN, no response 면 dead") 은 fast-fail 가능하지만 false-positive (일시 timeout) 위험. 운영자가 명시적으로 registry 에서 제거 = 명시적 의도.

### Plan 구조

```go
type StripeRepair struct {
    Bucket, Key string
    StripeIndex int

    // Survivors 는 stripe 의 살아있는 shard list (정렬). len >= K 면 repairable.
    Survivors []SurvivorRef
    // DeadShards 는 OldAddr 가 죽은 shard 들의 (index, oldAddr, newAddr).
    DeadShards []DeadShard
}

type SurvivorRef struct {
    ShardIndex int      // stripe 내 위치 (0..K+M-1)
    ChunkID    string
    Addr       string   // 살아있는 DN
}

type DeadShard struct {
    ShardIndex int
    ChunkID    string   // 옛 chunk_id (재구성 후 같은 데이터 → 같은 chunk_id)
    OldAddr    string   // 죽은 DN
    NewAddr    string   // PlaceN(stripe_id, K+M)[shard_index] 또는 첫 unused
    Size       int64
}

type Plan struct {
    Scanned int             // 검사된 stripe 수
    Repairs []StripeRepair
    Unrepairable []StripeRepair  // K 개 survivors 미만 (데이터 손실 위험 alert)
}
```

### Run 알고리즘

각 stripe repair:

```
1. Fetch K survivors → K data/parity shards (positions known)
2. Build shards array of length K+M with nil at dead positions
3. reedsolomon.Reconstruct(shards) → fills missing data shard positions
4. If any dead position is parity (>= K):
       reedsolomon.Encode(data shards) → all M parity
       fill dead parity positions
5. For each dead shard:
       coord.PutChunkTo(NewAddr, ChunkID, rebuilt_data)
       store.UpdateShardReplicas(stripe_idx, shard_idx, [NewAddr])
6. ✓ stripe repaired
```

**Chunk_id 보존**: content-addressable 이라 재구성된 shard 의 sha256 = 원본의 sha256. 메타에 저장된 ChunkID 그대로 유효 → DN PUT 의 idempotent integrity check 통과.

### NewAddr 선택

ADR-024 set-based 패턴 그대로:
```
desired_set = sorted set(PlaceN(stripe_id, K+M))
actual_set  = set(survivors[].addr) 
unused_desired = sorted(desired_set - actual_set)
```

각 dead shard 에 대해 unused_desired 에서 pop. 결정적, ADR-024 의 patterns 일관.

### Admin endpoint + CLI

```
POST /v1/admin/repair/plan          → JSON {scanned, repairs, unrepairable}
POST /v1/admin/repair/apply?conc=N

kvfs-cli repair --plan
kvfs-cli repair --apply --concurrency 4
```

ADR-013 auto-trigger 통합은 별도 ADR (ADR-013 보강) — 자동 호출 시 unrepairable 항목 alert 정책 필요.

### 명시적 비범위

- **자동 트리거** — 운영자 수동 호출 only. ADR-013 보강 후속
- **Replication 모드 repair** — chunk 에 대해 dead-replica → 다른 replica 로 자연 fallback (ADR-002 의 quorum read). Rebalance 가 충분. EC 만 재구성 필요
- **Survivors < K (unrepairable)** — Plan 에 포함 + alert. 자동 복구 불가능 (수학적 한계). 운영자가 backup 등 다른 경로 필요
- **Live DN health monitoring** — registry 에서의 명시 제거 == dead 정의. heartbeat / TTL 등 ADR-030 후속
- **Stripe ID re-derivation 검증** — 재구성된 K data shards 의 sha256 concat 이 원래 stripe_id 와 일치하는지 확인 (data 무결성 추가 검증). 현재는 Reconstruct 의 GF 수학으로 충분 가정

## 결과

### 긍정

- **EC 의 핵심 약속 활용** — "K of K+M survives → recoverable" 가 운영 경로에서 동작
- **ADR-024 + ADR-027 자연 결합** — dns remove → repair --apply 한 번
- **결정적 새 위치 선택** — set-based 패턴 일관, 디버깅 용이
- **chunk_id 보존** — 재구성 데이터의 sha256 = 원본, 메타 변경 최소
- **rebalance 와 직교** — 같은 시점 동시 실행도 mutex 로 안전 (rebalanceMu 공유 검토)

### 부정

- **K 배 read traffic** — repair 1 stripe 는 K shards fetch. dn4 dead 에 100 stripes 영향 = 100 × K reads
- **Unrepairable 처리 부담** — survivors < K 시 자동 복구 불가, 운영자 alert 후 backup 경로
- **타이밍 race** — repair 도중 다른 DN 까지 죽으면 mid-repair 실패. 다음 cycle retry 가능 (idempotent: PUT 같은 chunk_id 재시도 OK)
- **Replication mode 미적용** — chunk 1개의 모든 replica DN 이 죽으면 (전형적 R=3 에 3 DN 동시 죽음) 복구 불가. 이는 EC 의 약속이지 replication 의 약속 아님

### 트레이드오프 인정

- "왜 자동 트리거 안 함?" — alert 정책 + unrepairable 처리가 묶임. 별도 ADR
- "왜 health-check 기반 dead 판정 안 함?" — false-positive 위험 + 인프라 부담. registry 명시 제거 = 명확 의도
- "왜 replication chunk repair 별도 안 함?" — quorum read 가 자연 fallback. EC 만 재구성 수학 필요

## 데모 시나리오 (`scripts/demo-nu.sh`)

```
1. ./scripts/down.sh + start dn1~dn6 + edge
2. PUT 128 KiB EC(4+2) → 2 stripes × 6 shards
3. docker stop dn5 dn6 + ./bin/kvfs-cli dns remove dn5:8080 dn6:8080
4. repair --plan → 4 dead shards (2 stripes × 2 each), 4 alive (>= K=4)
5. repair --apply → reconstruct + place on dn7/dn8 (or 첫 unused)
6. GET 가 정상 (sha256 일치) — repair 후 stripe 가 정상 6 shards 분포
7. 두 번째 plan = 0 (멱등)
```

## 관련

- ADR-008 (Reed-Solomon EC) — 이 ADR 의 수학 토대 (Reconstruct/Encode 재사용)
- ADR-024 (EC stripe rebalance) — 같은 Migration 패턴 일관, 다른 source semantics
- ADR-027 (Dynamic DN registry) — "dead shard" 판정 기준
- ADR-013 (Auto-trigger) — 후속 통합
- `internal/repair/` — 신규 패키지
- `scripts/demo-nu.sh` — 시연
- `blog/09-ec-repair.md` — narrative

## 후속 ADR 예상

- **ADR-030 — DN health monitoring**: heartbeat + TTL 기반 dead 자동 감지
- **ADR-013 보강 — auto repair**: 정책 + alert
- **ADR-031 — Replication chunk repair**: R-way 모드의 모든 replica 죽을 때 처리 (현재 무력)
