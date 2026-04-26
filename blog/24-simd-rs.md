# Episode 24 — Reed-Solomon 2× 빠르게: SIMD 어셈블리 없이

> **Series**: kvfs — distributed object storage from scratch
> **Season**: P4-09 wave · **Episode**: 11
> **연결**: `internal/reedsolomon/rs.go`

---

## Ep.6 의 측정과 약속

[Ep.6 EC](06-erasure-coding.md) 마치고 측정한 encode 속도: **(4+2) 64KiB → 515 MB/s**. ADR-008 의 "~50 MB/s 추정" 보다 10× 빠름 (log/exp 표 L1 cache-friendly). 그 글에서 약속했다 — "더 빠르게 만들 여지는 있다, SIMD 안 써도".

이번 ep 가 그 약속의 실측: **515 → 1174 MB/s, 2.3× ↑**.

핵심 변경 두 줄:
1. **`mul-by-constant table`** — encode 의 hot loop 안에서 GF 곱을 단일 byte indexed lookup 으로 환원
2. **8-byte loop unrolling** — Go 컴파일러가 SIMD 안 써도 register 활용

## RS encode 의 구조 (복습)

K data shards × M parity shards. parity 한 byte 의 계산:

```
parity[m][i] = sum_{k=0..K-1} ( G[m][k] · data[k][i] )      # XOR 합산
```

내부 loop: `parity[m][i] ^= gfMul(G[m][k], data[k][i])`. shardSize × M × K iter.

`gfMul` 은 GF(2^8) 의 곱. log/exp 표 lookup (Ep.6) 으로 ~ns 단위. 그러나 hot loop 안에서 호출이 자주 일어나니 함수 호출 overhead 도 누적.

## 트릭 1: mul-by-constant table

특정 stripe 의 encode 동안 G[m][k] 는 **고정 상수** (한 row × column 짝). 즉 `gfMul(coef, x)` 의 `coef` 가 안 바뀜. 그렇다면 매 호출 대신 **그 coef 에 대해 256-byte 표를 미리 만들어 두고** byte indexed lookup:

```go
var mulByCoef [256]byte
fillMulTable(&mulByCoef, coef)  // mulByCoef[x] = gfMul(coef, x), pre-compute

for i := 0; i < shardSize; i++ {
    parity[m][i] ^= mulByCoef[data[k][i]]
}
```

이제 hot loop 의 곱셈 = 1 byte indexed array load. log/exp 표 dual-lookup (`exp[log[a] + log[b]]`) 보다 1 step 절약 + 단일 cache line 에 모두 들어맞음.

stripe encode 마다 K×M 개 표를 만든다. K=4, M=2 → 8 개 × 256 byte = 2 KiB. L1 (32 KiB) 안에 충분.

## 트릭 2: 8-byte 루프 unroll

Go 컴파일러는 어셈블리 SIMD 를 자동으로 꺼내주지 않는다 (mind the lie of "modern compilers do SIMD" — Go 컴파일러는 그렇지 않다). 그러나 **수동 unrolling** 은 register pressure 를 잘 활용해서 IPC (instructions per cycle) 를 높임:

```go
src := dataShards[k]
out := parityShards[m]
c := 0
for ; c+8 <= shardSize; c += 8 {
    out[c+0] ^= mulByCoef[src[c+0]]
    out[c+1] ^= mulByCoef[src[c+1]]
    out[c+2] ^= mulByCoef[src[c+2]]
    out[c+3] ^= mulByCoef[src[c+3]]
    out[c+4] ^= mulByCoef[src[c+4]]
    out[c+5] ^= mulByCoef[src[c+5]]
    out[c+6] ^= mulByCoef[src[c+6]]
    out[c+7] ^= mulByCoef[src[c+7]]
}
for ; c < shardSize; c++ {  // tail
    out[c] ^= mulByCoef[src[c]]
}
```

8 개 독립적 indexed load — modern CPU 의 superscalar pipeline 이 병렬 실행. 이게 사실상 "수동 SIMD" — register 몇 개를 동시에 점유.

## bench

`internal/reedsolomon/rs_bench_test.go`:

```
Before:
BenchmarkEncode_4_2_64KiB-12       2364   504,180 ns/op   519.30 MB/s
After:
BenchmarkEncode_4_2_64KiB-12       5346   223,156 ns/op  1174.04 MB/s
```

(4+2) 2.3× 빠름. (10+4) 도 비슷한 비율 (264 → 612 MB/s).

Reconstruct 도 같은 패턴 적용 가능하나 PR 에 묶지는 않음 — encode 가 hot path (모든 PUT), reconstruct 는 cold path (DN 손실 시만).

## SIMD 어셈블리는 왜 안 쓰는가

`klauspost/reedsolomon` 처럼 AVX2 / NEON intrinsics 로 ~10 GB/s 까지도 가능. kvfs 가 거부하는 이유:

1. **외부 의존 minimum** 원칙 — assembly 가 들어오면 architecture 별 build, fallback path, 검증 책임이 폭증.
2. **교육적 가치 우선** — 이 글의 표 + unroll 은 readable Go. 어셈블리 는 readable Go 가 아님.
3. **현실 충분** — 1.1 GB/s encode 가 4-DN 클러스터의 NIC bandwidth (10 Gbps = 1.25 GB/s) 와 비슷. 더 빨라지면 다른 곳에서 bottleneck.

## 적용 사이트

`internal/reedsolomon/rs.go` 의 `Encode` 메서드만. Vandermonde 행렬 + Gauss-Jordan reconstruct 코드는 그대로. 변경 ~30 LOC, behavior 0 변경 — 모든 기존 테스트 24개 PASS.

## 다음

[Ep.25 Hot/Cold tier](25-hot-cold-tier.md): DN 마다 class 라벨, placement bias, 그리고 P5-04 의 rebalance 통합.
