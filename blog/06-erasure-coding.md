# kvfs from scratch, Ep.6 — Reed-Solomon EC: 같은 내구성에 디스크 75% 절약, 그리고 그게 어떻게 가능한지

> Season 2 · Episode 5 · Season 2의 보스전. 1968년 논문 + GF(2^8) + Vandermonde 행렬 + Gauss-Jordan 역산 — 이 셋만으로 "K+M shard 중 어느 K 개로도 원본 복원" 이 가능합니다. 라이브러리 0개로 ~700 LOC 에 구현.

## R=3 의 비싼 진실

Ep.1 부터 시연한 3-way replication 은 **단순하고 안전합니다**:
- 1 PiB 사용자 데이터 = 3 PiB 물리 디스크
- M=2 failure tolerance (1 살아있으면 read OK)
- 인코딩 비용 0 (그냥 복사)

같은 M=2 failure tolerance 를 **디스크 1.5 PiB** 만 써서 달성할 수 있다면? 75% 절감입니다. Backblaze, MinIO, Ceph 가 다 이 방향으로 갑니다.

답: **Reed-Solomon Erasure Coding**.

## RS의 약속 — 한 줄

> K 개 데이터 shard 가 있을 때, M 개의 추가 "parity" shard 를 계산해 두면, **K+M 개 중 어느 K 개로도 원본 복원**

(K=4, M=2) 의 경우:
- 디스크 사용 = (K+M)/K = 6/4 = 1.5×  → **50% overhead** (R=3 의 200% 대비 75% 절감)
- M=2 failure tolerance 동일

비유: 3개 숫자 (a, b, c). sum1 = a+b+c, sum2 = a+2b+3c 도 같이 보관. 5개 중 어느 3개만 살아남아도 연립방정식으로 (a, b, c) 복원 가능. RS 는 이 직관을 GF(2^8) 위에서 행렬 곱셈/역산으로 구현.

## 왜 GF(2^8)?

byte 산술 (uint8 mod 256) 은 곤란합니다:
- 곱셈 결과가 256 넘으면 정보 손실
- 나눗셈이 정확히 정의 안 됨 (예: 6 ÷ 4 = ?)
- 행렬 역산 보장 안 됨

**유한체 GF(2^8)** 가 답: 256 개 원소 위에 새로운 + - × ÷ 를 정의해서 모든 0 아닌 원소가 곱셈 역원을 갖도록.

핵심 트릭: 각 byte 를 GF(2)[x] 의 다항식으로 본다.

```
0b1010_1100 → x^7 + x^5 + x^3 + x^2

덧셈 = 계수별 mod-2 = bit XOR
곱셈 = 다항식 곱 후 primitive polynomial 로 mod
       primitive poly = x^8 + x^4 + x^3 + x^2 + 1 = 0x11d
       (AES, RS literature 표준)
```

매번 다항식 곱 + mod 는 느리니까 **log/exp 테이블**:
```
α = 2 (primitive element)
exp[i] = α^i for i in 0..254
log[α^i] = i

mul(a, b) = exp[(log[a] + log[b]) mod 255]   if a,b ≠ 0
div(a, b) = exp[(log[a] - log[b] + 255) mod 255]
```

`internal/reedsolomon/gf.go` 의 init() 이 이 테이블 256 entry × 2 = 512 byte 만 만들어 둠. 그 위에 mul/div/add 가 ns 단위:

```go
func gfMul(a, b byte) byte {
    if a == 0 || b == 0 { return 0 }
    return gfExp[int(gfLog[a])+int(gfLog[b])]
}
```

10 줄. 이게 **AES 와 RAID-6 와 ECC 가 모두 사용** 하는 GF(2^8) 곱셈입니다.

## RS 인코딩 = 행렬 곱

```
E (encoding matrix)         data (K shards)        shards (K+M)
┌─────────────┐             ┌──────────┐           ┌──────────┐
│  identity   │   K rows    │ shard_0  │           │ shard_0  │  ← 그대로
│   K × K     │             │ shard_1  │     =     │ shard_1  │  ← 그대로
├─────────────┤             │   ...    │           │   ...    │
│  parity     │             │ shard_K-1│           │ shard_K-1│  ← 그대로
│   M × K     │   M rows    └──────────┘           ├──────────┤
└─────────────┘                                    │ parity_0 │  ← 계산
   (K+M) × K                                       │ parity_1 │  ← 계산
                                                   └──────────┘
                                                    (K+M) × shardSize
```

systematic encoding (top K rows = identity) 의 장점: 출력 shards 의 위 K 행은 입력과 동일 → "encode" 단계가 사실상 parity M 행만 계산. 데이터 shards 는 손대지 않음.

`Encode` 의 코어 (~15 LOC):
```go
for p := 0; p < e.m; p++ {                            // each parity shard
    row := e.encMat.row(e.k + p)                      // K coefficients
    out := parity[p]
    for kIdx := 0; kIdx < e.k; kIdx++ {
        coef := row[kIdx]
        src := dataShards[kIdx]
        for c := 0; c < shardSize; c++ {
            out[c] = gfAdd(out[c], gfMul(coef, src[c]))   // GF byte mul
        }
    }
}
```

shardSize 가 16 KiB 면 16384 번의 GF mul + add. SIMD 없는 pure Go 인데도 **~515 MB/s** ((4+2) 기준, 실측 i9-12900H). log/exp 테이블이 L1 에 들어가 cache-friendly한 덕분.

## 디코딩 = 행렬 역산

K+M shard 중 일부를 잃었다고 칩시다. 살아있는 K 개의 인덱스 idx[0..K) 를 알면:

```
E_sub = E.subMatrix(idx)             # K × K submatrix
data  = E_sub^-1 × surviving_shards  # over GF(2^8)
```

여기서 결정적인 보장: **어떤 K 개 행을 골라도 E_sub 가 invertible** 이어야 함. 이를 **MDS (Maximum Distance Separable) 속성** 이라 부르고, RS의 핵심 보장입니다.

확인 방법: `internal/reedsolomon/matrix_test.go::TestMatrix_SystematicEncoding_AnyKRowsInvertible` 가 (4+2) 의 모든 C(6,4) = 15 가지 sub-matrix 를 만들어 invert 시도 → 전부 PASS.

`Reconstruct` 의 코어:
```go
basis := survIdx[:e.k]                  // first K surviving indices
subEnc := e.encMat.subMatrix(basis)     // K × K
inv, _ := subEnc.invert()               // Gauss-Jordan over GF(2^8)

for d := 0; d < e.k; d++ {
    if shards[d] != nil { continue }    // already have it
    out := make([]byte, shardSize)
    for j := 0; j < e.k; j++ {
        coef := inv.get(d, j)
        src  := shards[basis[j]]
        for c := 0; c < shardSize; c++ {
            out[c] = gfAdd(out[c], gfMul(coef, src[c]))
        }
    }
    shards[d] = out
}
```

Gauss-Jordan over GF(2^8) 자체는 ~50 LOC. 일반 부동소수점 Gauss-Jordan 과 동일한 형태, 단지 산술이 GF(2^8).

전체 패키지 ~700 LOC + 24 테스트:

| 파일 | 역할 | LOC |
|---|---|---|
| `gf.go` | GF(2^8) tables + mul/div/inv/pow | ~80 |
| `gf_test.go` | 가환성/역원/primitive element 검증 | ~120 |
| `matrix.go` | matrix + Vandermonde + Gauss-Jordan | ~180 |
| `matrix_test.go` | identity / invert / MDS subset coverage | ~150 |
| `rs.go` | NewEncoder + Encode + Reconstruct | ~170 |
| `rs_test.go` | 단일 손실 / 다중 손실 / 모든 조합 | ~200 |

## 24 테스트가 검증한 것들

가장 중요한 두 가지:

**1. `TestRoundTrip_TwoShardsLost_AllCombinations`** — (K=4, M=2) 에서 6 shard 중 2 개를 잃는 모든 C(6,2) = **15 가지 조합** 을 시도, 전부 정확히 복원. 이게 MDS 의 운영적 의미.

**2. `TestRoundTrip_ThreeShardsLost_FailsCleanly`** — M=2 인데 3 개 잃으면 `ErrTooFewShards` 깨끗히 반환. 데이터 손상 없이 깔끔히 실패.

그 외:
- 모든 K, M 조합 ((2,1), (3,2), (4,2), (6,3), (10,4), (16,4)) round-trip
- 데이터 shards 모두 잃고 parity 만으로 복원
- parity 모두 잃고 데이터로만 (no-op fast path)
- 결정성 (같은 데이터 → 같은 parity)

## kvfs 와의 통합 — placement 가 자연스럽게

각 stripe 의 shard 는 K+M 개. 이를 **K+M 개 distinct DN** 에 흩뿌려야 합니다 (M failure tolerance 의 기본 가정).

ADR-009 (Rendezvous Hashing) 이 자연스럽게 답합니다:

```go
stripe_id    = sha256(K개 data shard concat)
desired_dns  = placer.Pick(stripe_id, K+M)   // K+M 개 distinct DN
shard[i]    → desired_dns[i]   for i in [0, K+M)
```

같은 stripe_id 면 항상 같은 DN 집합 (결정적, 메타 없이도 재현 가능). 다른 stripe_id 면 일반적으로 다른 DN 집합. **load balancing 부수효과**.

## ObjectMeta 스키마 추가

기존 (Chunks 만):
```go
type ObjectMeta struct {
    Bucket, Key string
    Size int64
    Chunks []ChunkRef     // replication mode
}
```

추가 (EC mode 동시 지원):
```go
type ObjectMeta struct {
    ...
    Chunks  []ChunkRef    // replication (mutually exclusive)
    EC      *ECParams     // EC params (nil → replication)
    Stripes []Stripe      // EC stripes
}

type ECParams struct { K, M, ShardSize int; DataSize int64 }
type Stripe   struct { StripeID string; Shards []ChunkRef }
```

각 객체는 **per-PUT 모드 선택**: 헤더 `X-KVFS-EC: K+M` 있으면 EC, 없으면 replication.

## edge handler — PUT/GET 분기 한 줄

```go
// handlePut
if ecHdr := r.Header.Get("X-KVFS-EC"); ecHdr != "" {
    s.handlePutEC(w, r, bucket, key, body, ecHdr)
    return
}
// fallthrough to replication

// handleGet
if meta.IsEC() {
    s.handleGetEC(w, r, ctx, meta)
    return
}
// fallthrough to replication
```

EC 핸들러는 `chunker.Split` 대신 **stripeBytes (K × shardSize) 단위 슬라이스** + `reedsolomon.Encode` + `coord.PutChunkTo(addr, ...)` per shard. ~150 LOC.

GET 은 각 shard 를 fetch 시도 (single addr per shard, ReadChunk reusing existing helper). 살아있는 K 개 모이면 `Encoder.Reconstruct` → 데이터 shards concat → 마지막 stripe 패딩 trim.

## rebalance / GC 영향

- **rebalance**: 미구현 (이번 episode 범위 밖). EC stripe 의 shard 가 옮겨다닐 때 stripe_id 가 변하지 않으니 desired_dns 도 동일 — 다음 ADR-013 후보
- **GC**: `buildClaimedSet` 의 inner loop 을 `obj.Stripes` 까지 iterate 하도록 추가. 변경 ~5 LOC. 안전망 (claimed-set + min-age) 동작 그대로

## 라이브 데모 — `demo-kappa.sh`

```
=== κ demo: Reed-Solomon EC (K=4, M=2) — ADR-008 ===

[1/7] Reset cluster with 6 DNs (K+M=6 needs 6 distinct DNs)
  ✅ 6 DNs + edge up (chunk_size=16384)

[2/7] Generate 131072-byte random body (= 2 stripes of K=4 × 16384)
  body bytes:  131072
  body sha256: 7a59245b78c5dd99d54a799b4baf265f..

[3/7] PUT object with X-KVFS-EC: 4+2
  ec: {'k': 4, 'm': 2, 'shard_size': 16384, 'data_size': 131072}
  num_stripes: 2

  stripe[0] id=af8c1e4b9e39465a..
    shard[0] data   id=c9cea2b1b226efb6.. size=16384 → dn6:8080
    shard[1] data   id=4f6679add383561d.. size=16384 → dn4:8080
    shard[2] data   id=92857198ed3e96d2.. size=16384 → dn2:8080
    shard[3] data   id=799b41a72b19de59.. size=16384 → dn3:8080
    shard[4] parity id=ed045cf37427c2b1.. size=16384 → dn1:8080
    shard[5] parity id=7b16ccc9265aef7c.. size=16384 → dn5:8080
  stripe[1] id=dee076ca7f05a9fe..
    shard[0] data   id=86aaf3de0b070872.. size=16384 → dn3:8080
    shard[1] data   id=04e03342912b7581.. size=16384 → dn6:8080
    ... etc
```

각 shard 가 정확히 1개 DN 으로. 같은 stripe 의 6 shard 는 항상 6개 distinct DN.

```
[5/7] Disk distribution across 6 DNs (each shard on exactly 1 DN)
  dn1  shards = 2
  dn2  shards = 2
  dn3  shards = 2
  dn4  shards = 2
  dn5  shards = 2
  dn6  shards = 2
  total: 12  (expected 2 stripes × 6 shards = 12)
```

**완벽한 균등 분포.** 6 DN × 2 shards = 12 = 2 stripes × 6 shards.

```
[6/7] Kill dn5 + dn6 (M=2 failures, the maximum tolerated) → GET still works
  ✅ GET succeeded after 2 DN kills — Reed-Solomon Reconstruct rebuilt missing shards
     reconstructed sha256: 7a59245b78c5dd99d54a799b4baf265f..
```

dn5, dn6 모두 죽인 상태에서 GET → 각 stripe 에서 6 shard 중 4 개 만 살아있음 → 정확히 K=4 → Reconstruct 가 GF(2^8) 위에서 invert 후 곱해 데이터 복원. **sha256 정확 일치**.

이게 "from-scratch Reed-Solomon" 이 클러스터 위에서 일하는 모습입니다.

## 디스크 효율 비교 — 실측

같은 128 KiB 객체:

| mode | 디스크 사용 | overhead | failure tolerance |
|---|---|---|---|
| R=3 replication | 384 KiB (128 × 3) | 200% | 2 |
| RS(4+2) | 192 KiB (32 KiB shard × 6) | 50% | 2 |
| 차이 | **-50%** | **-150%pt** | 동일 |

1 PiB 사용자 데이터 기준: replication 3 PiB → EC(4+2) 1.5 PiB. **1.5 PiB 절감** (= 디스크 ~$30,000 절감 @ $20/TiB raw).

## 솔직한 한계 — 다음 episode 가 풀어야 할 것들

1. **CPU 비용** — pure Go GF mul (no SIMD): (4+2) ~515 MB/s · (6+3) ~353 MB/s · (10+4) ~264 MB/s 단일 코어 (실측 i9-12900H). 프로덕션 SIMD (klauspost/reedsolomon AVX2/NEON) 는 GB/s+
2. **읽기 amplification** — 단일 shard miss 라도 K shard fetch + invert + 곱셈 필요. R-way 는 1 shard fetch 만. EC 의 평균 read latency 가 더 큼
3. **메타 폭증** — 1 GiB / 16 KiB shard / (K=4 M=2) → 16384 shards 메타. ChunkRef per shard 라 byte 수 큼
4. **EC rebalance 미구현** — 이번 episode 범위 밖. shard 의 desired DN 이 변할 때 (DN 추가) 마이그레이션 필요. ADR-013 후보
5. **partial-stripe 손실 시 전체 데이터 손실** — K 미만 survive 면 복원 불가. RS 의 수학적 한계

이런 한계가 있다는 걸 알면서 이번 ADR 한 페이지 + 한 episode 로 끝낸 이유는 같은 원칙:

> **하나의 결정 = 하나의 보장**

ADR-008 은 "K+M shard 중 어느 K 개로도 원본 복원" 만 약속. SIMD, rebalance, hybrid policy 는 별개 약속. 각각 별도 ADR.

## Season 2 가 닫혔다 — 무엇이 가능해졌나

| Episode | ADR | 기여 |
|---|---|---|
| Ep.2 | 009 — Rendezvous Hashing | 청크가 어디 가는지 결정 |
| Ep.3 | 010 — Rebalance | DN 변동 시 청크 이사 |
| Ep.4 | 012 — Surplus GC | 디스크 = 메타 정렬 |
| Ep.5 | 011 — Chunking | 큰 객체 분할 |
| Ep.6 | **008** — Reed-Solomon EC | 디스크 75% 절약, 같은 내구성 |

**분산 object storage 의 뼈대 5개 가 모두 시연 코드로 존재**. Ceph, MinIO, Backblaze 가 다 이 5개 위에 구축. kvfs 는 그것들의 **단순화된 living reference** 입니다.

## 다음 편 예고

- **Ep.7** *(완료)*: ADR-013 Auto-trigger policy — 운영자 호출 0번에 자동 정렬. → [`blog/07-auto-trigger.md`](07-auto-trigger.md)
- **Ep.8 후보**: ADR-019 SIMD-accelerated RS — Go `_amd64.s` 또는 klauspost 통합
- **Ep.9 후보**: ADR-020 Hybrid storage — 객체 크기 / age 기반 자동 replication↔EC 마이그레이션
- **Ep.10 후보**: ADR-021 Local Reconstruction Codes (LRC)

## 직접 돌려보기

```bash
git clone https://github.com/HardcoreMonk/kvfs   # 공개 시점 기준
cd kvfs
docker build -t kvfs-edge:dev --target kvfs-edge .
docker build -t kvfs-dn:dev   --target kvfs-dn   .

./scripts/demo-kappa.sh
```

매번 chunk_id / shard_id 는 다릅니다 (랜덤 body). 그러나 항상:

- 2 stripes × 6 shards = 12 shards
- 6 DN 에 정확히 2 shards 씩 균등 분포 (HRW Pick 의 결과)
- GET sha256 == PUT sha256
- dn5 + dn6 kill 후 GET 도 sha256 일치 (Reconstruct)
- dn4, dn5, dn6 (3개) kill 시 GET 503 (M=2 한계 초과)

위 5개 불변식이 ADR-008 의 약속과 일치합니다.

다른 K/M 조합도:

```bash
# K=6, M=3 (8 DNs needed) — 디스크 50% overhead, 3 failure 허용
# K=10, M=4 (14 DNs needed) — 디스크 40% overhead, 4 failure 허용
```

## 참고 자료

- 이 ADR: [`docs/adr/ADR-008-reed-solomon-ec.md`](../docs/adr/ADR-008-reed-solomon-ec.md)
- GF(2^8): [`internal/reedsolomon/gf.go`](../internal/reedsolomon/gf.go)
- 행렬: [`internal/reedsolomon/matrix.go`](../internal/reedsolomon/matrix.go)
- Encode/Reconstruct: [`internal/reedsolomon/rs.go`](../internal/reedsolomon/rs.go)
- 24 테스트: [`internal/reedsolomon/*_test.go`](../internal/reedsolomon/)
- ObjectMeta 스키마: [`internal/store/store.go`](../internal/store/store.go) `ECParams` + `Stripe`
- edge handler: [`internal/edge/edge.go`](../internal/edge/edge.go) `handlePutEC` / `handleGetEC`
- 데모: [`scripts/demo-kappa.sh`](../scripts/demo-kappa.sh)
- 이전 episode (chunking): [`blog/05-chunking.md`](05-chunking.md)
- 원논문: I. S. Reed & G. Solomon, *Polynomial Codes Over Certain Finite Fields*, 1960

---

*kvfs는 교육적 레퍼런스입니다. 프로덕션에 배포하지 마세요. Season 2 가 약속한 5 episode 가 모두 이행되었습니다 — placement, rebalance, GC, chunking, EC. 분산 object storage 의 동기화 알고리즘 묶음이 ~5,000 LOC + 66 테스트 + 9 라이브 데모 로 살아있습니다.*
