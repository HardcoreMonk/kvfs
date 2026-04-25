# ADR-008 — Reed-Solomon Erasure Coding (R-way 의 50% 디스크로 같은 내구성)

## 상태
Accepted · 2026-04-26 · Season 2 Ep.5 · Season 2의 보스전

## 맥락

ADR-002 의 3-way replication 은 **단순하고 빠르지만** 디스크 사용이 200% overhead. 1 PiB 사용자 데이터 = 3 PiB 물리 디스크. 산업 규모에서는 받아들이기 어려운 비용.

내구성을 유지하면서 디스크를 줄이는 표준 답: **Reed-Solomon Erasure Coding (RS EC)**.

|  방식 | 내구성 (M failure 까지) | 디스크 overhead | 인코딩 비용 | 복구 비용 |
|---|---|---|---|---|
| 3-way replication | 2 failures | 200% | 0 (그냥 복사) | 1 shard 다시 복사 |
| RS(4+2) | 2 failures | 50% | encode (CPU) | K shards 읽고 decode |
| RS(6+3) | 3 failures | 50% | encode | K shards 읽고 decode |
| RS(10+4) | 4 failures | 40% | encode | K shards 읽고 decode |

**같은 내구성에 더 적은 디스크.** Backblaze (Reed-Solomon (17+3)), Ceph (CRUSH + EC), MinIO (RS), HDFS-EC 등이 이미 사용. 표준 분산 스토리지의 중요한 원리.

### EC의 핵심 직관 (비전공자 해설)

K 개의 데이터 shard 가 있을 때, **수학적으로** M 개의 추가 "parity" shard 를 계산해 두면, 원본 K + 추가 M = **총 K+M shard 중 어느 K 개로도 원본을 복원** 할 수 있다.

비유: 3개 숫자 (a, b, c) 가 있다. 두 개를 잃어도 1개로 복원하려면? 안 됨 (1 < 3). 하지만 (a, b, c) 와 함께 sum = a+b+c, sum2 = a+2b+3c 도 보관해 두면? 5개 중 어느 3개만 살아남아도 연립방정식으로 (a, b, c) 복원 가능.

RS 는 이 직관을 GF(2^8) 위에서 실행. 곱셈·나눗셈이 정확하게 정의된 유한체에서 Vandermonde 행렬로 parity 를 계산.

### 대안 검토

| 방식 | 장점 | 단점 |
|---|---|---|
| **RS (이번 ADR, from-scratch)** | 표준 (1960년 논문), 교과서 알고리즘, GF/행렬 가시화 | 구현 ~500 LOC, GF 산술 버그 위험 |
| RS via klauspost/reedsolomon | SIMD 가속, battle-tested, MIT | 외부 의존 +1 (kvfs 첫 비-stdlib 의존, bbolt 다음) |
| XOR parity (RAID-5 스타일) | 매우 단순 | 1 failure 만 견딤 |
| Replicate + secondary cluster | 운영 단순 | overhead 그대로 |
| LRC (Local Reconstruction Codes) | 부분 복구 효율 | 더 복잡 |

**from-scratch 결정 근거**: kvfs 의 정체성 = "분산 스토리지 내부 가시화". placement·urlkey·rebalance·gc·chunker 모두 hand-rolled. RS 의 GF(2^8) 산술이 본 episode 의 교육 핵심. klauspost 는 **프로덕션 스토리지 권장** 으로 ADR 에 명시.

## 결정

### 핵심 모델 — Layering

기존(replication mode):
```
body → chunker.Split → N chunks → 각 chunk × R replicas
```

신규(EC mode):
```
body → chunker.Split → N chunks (각 chunkSize)
      → groupBy K (K data chunks per stripe)
      → reedsolomon.Encode → M parity chunks per stripe
      → K+M shards per stripe, 각 shard ONE DN 에 배치
```

**stripe** = K 개 data shard + M 개 parity shard 묶음. 한 객체에는 여러 stripe 가 존재 가능.

### 배치 (stripe-level placement)

각 stripe 의 K+M shard 는 **서로 다른 K+M 개 DN** 에 배치되어야 함 (M failure tolerance 의 기본 가정).

```
stripe_id = sha256(K개 data shard 를 순서대로 concat)
desired_dns = placement.Pick(stripe_id, K+M)   # 결정적, K+M 개 distinct DN
shard[i] → desired_dns[i]   for i in [0, K+M)
```

ADR-009 (Rendezvous Hashing) 의 Pick 이 자연스럽게 K+M 개를 돌려줌. 같은 stripe_id 면 같은 DN 집합. 메타 없이도 재현 가능 (rebalance 친화).

### ObjectMeta 스키마 추가

```go
type ObjectMeta struct {
    Bucket, Key string
    Size        int64
    
    // Replication mode (mutually exclusive with EC)
    Chunks []ChunkRef
    
    // EC mode (mutually exclusive with Chunks)
    EC      *ECParams `json:"ec,omitempty"`
    Stripes []Stripe  `json:"stripes,omitempty"`
    
    Version     int64
    CreatedAt   time.Time
    ContentType string
}

type ECParams struct {
    K         int `json:"k"`           // data shards per stripe
    M         int `json:"m"`           // parity shards per stripe
    ShardSize int `json:"shard_size"`  // bytes per shard (= chunkSize)
    DataSize  int64 `json:"data_size"` // total bytes of data shards (last stripe padded)
}

type Stripe struct {
    StripeID string     `json:"stripe_id"`
    Shards   []ChunkRef `json:"shards"`  // length K+M; data first then parity
}
```

`ChunkRef.Replicas` 는 EC mode 에서 길이 1 (single addr per shard).

### PUT 흐름 (EC mode)

요청 헤더 `X-KVFS-EC: K+M` (예: `4+2`) 가 있으면 EC mode. 없으면 replication.

1. body 를 chunkSize 단위로 split
2. K 개씩 묶어 stripe 구성. 마지막 stripe 의 모자란 데이터는 0 패딩
3. 각 stripe 에 대해:
   - reedsolomon.Encode(data shards, K, M) → M parity shards
   - stripe_id = sha256(data shards concat)
   - desired_dns = placement.Pick(stripe_id, K+M)
   - 각 shard[i] 에 대해 PutChunkTo(desired_dns[i].Addr, shard[i].chunk_id, shard[i].data)
4. ObjectMeta.{EC, Stripes, Size} 구성, Store.PutObject

### GET 흐름 (EC mode)

1. Store.GetObject → meta.EC, meta.Stripes
2. 각 stripe 에 대해:
   - 각 shard 에 대해 GET (단일 DN, single addr) — 일부 실패 가능
   - 살아있는 shard 가 K 개 미만이면 데이터 손실 (HTTP 503 + log)
   - 살아있는 K 개 shard 로 reedsolomon.Reconstruct → K data shards 복원
   - data shards concat → stripe payload
3. 모든 stripe 의 payload concat → 마지막 stripe 의 패딩 제거 → response body

### DELETE 흐름

각 stripe 의 모든 shard DELETE (best-effort). 메타 삭제.

### Rebalance 영향

`rebalance.ComputePlan` 이 obj.Stripes 를 iterate (chunked replication 의 obj.Chunks 와 비슷):
- 각 stripe 의 stripe_id 로 desired_dns = Pick(stripe_id, K+M) 재계산
- 각 shard[i] 의 actual addr 와 desired_dns[i] 비교
- 다르면 migration

EC stripe 의 migration 은 shard 단위 — chunk replication 의 청크 단위와 동일 패턴. `Migration` 에 `StripeIndex` 추가.

### GC 영향

`gc.buildClaimedSet` 이 obj.Stripes 도 iterate. (chunk_id, addr) 페어를 union. 알고리즘 변경 0.

### 설정

CLI / 헤더로 per-object 선택:
- `X-KVFS-EC: K+M` PUT 요청 → EC mode
- 없음 → replication (기존)

전체 클러스터 강제 정책은 ADR-013 (auto-trigger) 와 함께 future.

## 결과

### 긍정

- **디스크 effizienz 50%** (RS(4+2) 기준) vs replication 200% — **75% 절감**
- **같은 M failure tolerance** — RS(K+M) 은 M failure 까지 견딤, R-way 는 R-1 까지
- **stripe-level placement** — Pick(stripe_id, K+M) 한 번이 자연스러움
- **rebalance/gc 변경 최소** — stripe.Shards 가 chunk 단위 iterable
- **알고리즘 가시화** — GF(2^8) + Vandermonde + Gauss-Jordan 가 코드로

### 부정

- **encode CPU 비용** — RS(4+2) 는 byte 당 ~6 GF mul. SIMD 없이 ~50 MB/s 수준. SIMD 사용 production 라이브러리는 1 GB/s+
- **decode 시 K shard fetch + matrix invert** — 단일 shard miss 도 K shard fetch 필요 (replication 은 1 shard fetch). 읽기 latency 증가
- **메타 폭증** — replication 은 chunk N 개, EC 는 stripe M=ceil(N/K) × (K+M) shard. 1 GiB / 16 KiB chunk / K=4 M=2 → 16384 shards 메타 (replication 이라면 65536 chunk × 3 replicas = 196608 entries 나 stripe 단위는 더 적음. 케이스마다 다름)
- **partial-stripe 손실 시 전체 데이터 손실** — K 미만 survive 면 복원 불가. 메타 backup 별도 필요
- **PUT 부분 실패 처리 복잡** — 6개 shard 중 4개 PUT 후 2개 실패: 메타 commit 안 함, orphan shards 발생, GC 가 청소 (replication 과 동일 패턴이지만 shard 수 ↑)

### 트레이드오프 인정

- "왜 from-scratch?" — kvfs 의 교육적 목표. klauspost/reedsolomon 은 **프로덕션 권장** 으로 코드 주석에 명시
- "왜 K+M=4+2 default?" — 디스크 효율 50% + 2 failure tolerance + chunk 수 적정. (10+4) 는 큰 클러스터에서 더 효율적이지만 demo 부적합
- "왜 stripe-id 가 data shards concat 의 sha256 인가?" — 결정적 + 같은 데이터면 같은 stripe_id (재현 가능). parity 까지 포함하면 padding 등 복잡. data shards 만으로 충분
- "왜 replication 과 EC mode 둘 다 유지?" — 작은 객체 (chunkSize 미만) 는 EC overhead (K+M shards) 가 비효율. 운영 정책 (cold/hot, 객체 크기) 으로 선택. 둘 다 핵심 원리

## 데모 시나리오 (`scripts/demo-kappa.sh`)

```
1. ./scripts/down.sh + start dn1~dn6 (6 DN 필요: K+M=4+2)
2. PUT 128 KiB body with X-KVFS-EC: 4+2 → 2 stripes × 6 shards = 12 shards
3. 각 stripe 의 6 shards 가 6 distinct DN 에 분산 확인 (placement.Pick)
4. PUT 응답 메타에 EC.K=4, EC.M=2, Stripes 2 entries 표시
5. dn5, dn6 kill (2 failures)
6. GET 동일 객체 → reedsolomon.Reconstruct 가 살아있는 4 shards 로 데이터 복원
7. body sha256 == 원본 sha256 확인
8. dn5, dn6 살리고 다시 GET → 동일 결과
```

## 관련

- ADR-002 — 3-way replication MVP (이 ADR 의 효율 비교 대상)
- ADR-009 — Rendezvous Hashing (stripe-level Pick(stripe_id, K+M) 직접 활용)
- ADR-011 — Chunking (EC stripe 가 chunk 들로 구성)
- `internal/reedsolomon/` — GF(2^8) + 행렬 + Encode/Reconstruct (후속)
- `scripts/demo-kappa.sh` — 시연 (후속)
- `blog/06-erasure-coding.md` — narrative episode (후속)

## 후속 ADR 예상

- **ADR-019 — SIMD 가속 RS**: 프로덕션 성능 필요 시 klauspost/reedsolomon 통합 또는 직접 SIMD
- **ADR-020 — Hybrid storage policy**: 객체 크기 / age 기반 자동 replication↔EC 마이그레이션
- **ADR-021 — Local Reconstruction Codes (LRC)**: M+1 shard 만 읽고 부분 복구
