# Episode 19 — CDC × EC: variable stripe 합치기

> **Series**: kvfs — distributed object storage from scratch
> **Season**: 4 (performance/efficiency) · **Episode**: 6 (Ep.2 CDC + Ep.5 EC streaming follow-up)
> **ADR**: [018](../docs/adr/ADR-018-content-defined-chunking.md) follow-up · **Demo**: `./scripts/demo-psi.sh`

---

## 두 매력의 충돌

- [Ep.15 CDC](15-cdc.md): 같은 콘텐츠가 1 byte 어긋나도 ~99% chunk 공유 (shift-invariant dedup).
- [Ep.18 EC streaming](18-ec-streaming.md): 1.5× 디스크 사용량으로 같은 내구도.

같은 cluster 에서 둘 다 켜고 싶으면? **CDC 가 나누면 chunk 크기가 들쭉날쭉**, EC 는 **stripe 단위 (= K × shardSize) 로 정렬**. 충돌.

## 변동 stripe

해결 방향: stripe 마다 크기가 다른 걸 인정한다. 한 CDC chunk 가 곧 한 stripe — chunk size 가 K × shardSize 일 필요 없음.

알고리즘:

```
for piece := range cdcReader {
    rawLen := len(piece.Data)
    // K 등분. 마지막 shard 만 짧을 수 있음. 같은 길이 (= padded) 로 만든다.
    ssz := ceil(rawLen / K)
    padded := zeros(ssz * K)
    copy(padded, piece.Data)
    dataShards := split(padded, K, ssz)
    parityShards := RS.Encode(dataShards)
    HRW.PlaceN(stripeID, K+M)  // fanout
    // 메타: Stripe.DataLen = rawLen (실제 byte), Shards 6개, ShardSize는
    //       per-stripe varies → 메타에 ShardSize 도 stripe 별로 박을 수도
}
```

핵심 추가: `Stripe.DataLen int64` — 이 stripe 의 실제 데이터 byte. GET 시 trim 기준.

## 메타 모델

기존 `ECParams.ShardSize` 는 **고정** 가정이었다. CDC + EC 에선 더 이상 의미 없음 (stripe 마다 다름). 옵션 두 개:

1. `ECParams.ShardSize = 0` 로 두고 `Stripe.Shards[0].Size` 가 진실.
2. `Stripe.ShardSize int` 추가.

kvfs 는 1 채택 — 이미 Shards[i].Size 가 byte 단위. 메타 schema 추가 0.

`Stripe.DataLen` 만 추가 (`omitempty`, fixed-size mode 에서는 0).

## GET 시 trim

```go
for _, stripe := range obj.Stripes {
    var dataBytes int64
    if stripe.DataLen > 0 {
        dataBytes = stripe.DataLen      // CDC mode
    } else {
        // fixed mode: 마지막 stripe 만 짧음
        dataBytes = remaining(obj.EC.DataSize, written)
    }
    // K data shards fetch + RS Reconstruct (필요 시) + first dataBytes write.
}
```

CDC 모드는 stripe 별 trim. fixed 모드는 마지막 stripe 만 trim — 두 path 가 한 GET handler 안에 공존.

## Encode 의 cost

CDC chunk 가 평균 4 MiB 라면 stripe 당 raw 4 MiB → padded ceil(4MiB/4) × 4 = 4 MiB. K=4, M=2 면 RS encode 가 처리하는 데이터는 stripe 당 4 MiB × 4 (data) = 16 MiB. encode 속도 ([Ep.6](06-erasure-coding.md) 의 (4+2) 1174 MB/s) 면 ~14 ms / stripe.

CDC 가 fixed 보다 짧은 chunk 도 만들 수 있으므로 (Min/Normal/Max 분포) 평균 stripe 처리 시간은 분포의 평균에 비례. dedup 으로 deduplicate 된 stripe 는 second PUT 부터는 encode 자체가 skip 되지는 않지만 (chunk_id 가 같아도 RS encode 는 매번) — 이는 향후 최적화 여지.

## 데모: 변경된 객체로 dedup 효과

`scripts/demo-psi.sh` 시나리오:

1. F1 = 4 MiB random + repeated marker. PUT with `X-KVFS-EC: 4+2` + `X-KVFS-CHUNK-MODE: cdc`.
2. F2 = F1 의 첫 1 byte 만 변경. 동일 PUT.
3. DN 디스크의 chunk 수 측정.

```
PUT F1 → 1 stripe, 6 shards stored
PUT F2 → 1 stripe, 6 shards stored
        그러나 shard sha256 은 F1 의 일부 (CDC 첫 chunk 만 다름) 와 공유
DN 디스크 chunk count: F1 만 6, F2 추가 후 7 (5 개 dedup, 1 개 신규)
```

shift 무관 dedup 이 EC 안에서도 동작. `dedup ratio` = (F1 + F2 의 shard 수) − (실제 disk shard 수) ÷ 합계 = (12 − 7) / 12 = **42% 절감**.

## 메모리는?

stripe 당 max = MaxSize (CDC 의 16 MiB 기본) + padded encoded shards. 즉 ~16 MiB + K+M shard 들. EC streaming (Ep.18) 의 16 MiB 와 거의 같은 수준 — CDC 의 가변성이 덜 들었을 뿐.

## 호환성

- `Stripe.DataLen` 는 `omitempty`. 기존 fixed-mode 객체 = 0 = trim 로직이 자연 fallback.
- `EDGE_CHUNK_MODE=cdc` + `X-KVFS-EC: K+M` 조합으로 활성. 둘 중 하나만 있으면 기존 path.

## 다음

[Ep.20](20-sync-replication.md): leader 가 매 mutation 마다 follower 들에 push. snapshot pull 보다 RPO 가 0 에 더 가까워짐.
