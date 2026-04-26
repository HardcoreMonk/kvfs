# Episode 18 — EC streaming: stripe 단위로 흐르기

> **Series**: kvfs — distributed object storage from scratch
> **Season**: 4 (performance/efficiency) · **Episode**: 5 (Ep.1 streaming follow-up to ADR-008 EC)
> **ADR**: [017](../docs/adr/ADR-017-streaming-put-get.md) follow-up · **Demo**: regression in `./scripts/demo-kappa.sh`

---

## Ep.14 의 빈 자리

[Ep.14 streaming](14-streaming.md) 는 replication 모드의 PUT/GET 만 io.Reader 위로 옮겼다. 1 GiB 객체 PUT 이 와도 edge RAM 은 chunkSize (4 MiB). 그런데 같은 1 GiB 를 EC (4+2) 로 보내면 어떻게 될까?

**여전히 1 GiB 를 메모리에 올렸다.** Ep.14 의 `chunker.Reader` 는 replication 핸들러에만 박혔고, EC 핸들러 (`handlePutEC`) 는 옛날 그대로 — body 전체를 읽고 chunker.Split 한 뒤 stripe 단위로 자르고 RS Encode.

이번 ep 의 목표: EC 도 똑같이 stripe 단위 pump.

## Stripe 단위 pump

EC (K=4, M=2) 의 한 stripe = 4 data shard + 2 parity shard. 각 data shard 의 크기가 `shardSize` 라면 한 stripe 의 raw 입력은 `K × shardSize`. 따라서 io.Reader 에서 한 번에 그만큼만 빨아들이고, 그걸 4 등분해서 4 data shard 만들고 RS Encode 해서 6 shard 보내고, 다시 다음 stripe 만큼 빨아들이고…

```go
shardSize := s.chunkSize()
stripeBytes := k * shardSize
buf := make([]byte, stripeBytes)
for stripeIdx := 0; ; stripeIdx++ {
    n, err := io.ReadFull(r.Body, buf)
    if err == io.EOF { break }
    if err != nil && err != io.ErrUnexpectedEOF {
        return error
    }
    // n bytes valid (마지막 stripe 면 n < stripeBytes — pad 필요)
    padded := make([]byte, stripeBytes)
    copy(padded, buf[:n])

    // 4등분 → K data shards
    dataShards := make([][]byte, k)
    for di := 0; di < k; di++ {
        dataShards[di] = padded[di*shardSize : (di+1)*shardSize]
    }

    parityShards, _ := enc.Encode(dataShards)
    // ... HRW.PlaceN(stripeID, K+M) → 6 DN 으로 fanout PUT
}
```

`io.ErrUnexpectedEOF` 는 마지막 stripe 가 stripeBytes 보다 작을 때 io.ReadFull 이 돌려주는 신호. n 만큼만 valid, 나머지는 zero-pad.

메모리: `stripeBytes = K × shardSize = 4 × 4 MiB = 16 MiB` 만 항상 점유. object 가 1 GiB 든 100 GiB 든 동일.

## Padding 처리 (read 쪽)

마지막 stripe 의 padded zero 가 GET 시 그대로 객체에 섞이면 곤란. 해결:

- `ECParams.DataSize` 에 **객체 전체 raw byte 수** 기록.
- GET handler 가 stripe 별로 데이터 shard 를 join 하면서 누적 byte 수가 DataSize 에 도달하면 거기서 멈춘다.

```go
written := int64(0)
for _, stripe := range obj.Stripes {
    // K data shards 가져와 trim 없이 다 write 했다고 가정.
    for di := 0; di < k; di++ {
        shard := fetchShard(stripe.Shards[di])
        toWrite := int64(len(shard))
        if written + toWrite > obj.EC.DataSize {
            toWrite = obj.EC.DataSize - written
        }
        w.Write(shard[:toWrite])
        written += toWrite
        if written == obj.EC.DataSize { return }
    }
}
```

마지막 stripe 의 마지막 data shard 의 마지막 일부만 trim. 나머지는 그대로.

## 손실 시 복구는 그대로

EC 의 매력: K survivor 만 있으면 RS Reconstruct. 이 ep 의 streaming 변경은 PUT/GET 의 메모리 패턴만 바꿨을 뿐, encode/decode 알고리즘 (ADR-008, [Ep.6](06-erasure-coding.md)) 그대로. 따라서 Ep.6 에서 검증한 "DN 2 개 죽어도 복원" 성질 무손실 유지.

## 실측

`scripts/demo-kappa.sh` (Ep.6 의 EC 데모) 가 streaming 으로 바뀌어도 그대로 통과. 추가로 큰 객체로 확인:

```
$ ls -lh /tmp/big.dat
-rw-r--r-- 1 user user 256M /tmp/big.dat

$ curl -X PUT -H "X-KVFS-EC: 4+2" --data-binary @/tmp/big.dat \
    "http://edge:8000/v1/o/b/big?sig=...&exp=..."
{"bucket":"b","key":"big","stripes":64,"size":268435456}

$ ps -o rss= -p $(pgrep kvfs-edge)
22156   # ← 22 MB. object 가 256 MiB 인데 메모리는 stripe 1개 분량.

$ curl -o /tmp/big.out "http://edge:8000/v1/o/b/big?sig=...&exp=..."
$ sha256sum /tmp/big.dat /tmp/big.out
db4f...  /tmp/big.dat
db4f...  /tmp/big.out   # ← 동일
```

PUT 시 stripe 단위 pump 가 동작했고, GET 시 trim 이 정확. 64 stripes 모두 (K+M)=6 개 DN 으로 fanout 됐고 메모리는 22 MiB 에서 안 올라감.

## 트레이드오프

| 항목 | replication streaming (Ep.14) | EC streaming (이 ep) |
|---|---|---|
| 메모리 상한 | chunkSize (4 MiB) | K × shardSize (16 MiB for K=4) |
| HRW 호출 | per chunk | per stripe (= per K chunks) |
| 네트워크 | per chunk fanout (R DN) | per stripe fanout (K+M DN) |
| Last-piece padding | 자연 처리 (chunk 가 짧음) | 명시적 zero-pad + DataSize trim |
| 코드 복잡도 | ~30 LOC | ~60 LOC |

K 가 클수록 (예: K=10) 메모리는 늘지만 한 번의 "Read → encode → fanout" cycle 이 더 큰 단위로 묶여 RTT 도 줄어듦.

## 다음

[Ep.19](19-cdc-ec.md): CDC 와 EC 가 한 객체 안에서 같이 돌아가는 mode. variable-size stripe 가 필요해진다.
