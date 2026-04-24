# kvfs from scratch, Ep.1 — 3-way replication을 60줄 Go로

> Season 1 · Episode 1. 공개 오픈소스 데모로서 kvfs의 첫 인사.

## 이 글이 보여주는 것

`docker compose up` 한 방으로 **4 노드 분산 스토리지 클러스터**가 올라오고, 그중 한 노드를 죽여도 데이터가 살아남는 것을 3분 안에 확인합니다. 핵심 로직은 Go 60줄.

## 왜 또 하나의 분산 스토리지?

Ceph·MinIO·SeaweedFS·JuiceFS 이미 많습니다. kvfs는 **경쟁자가 아닙니다**. **설계 원리를 눈으로 보게 하는 레퍼런스**입니다.

프로덕션 스토리지의 교과서적 개념 두 가지 — **3-way replication + quorum**, **presigned URL** — 을 **각각 100줄 이내**로 시연하는 것이 Season 1의 전부.

## 5분 실습

```bash
git clone https://github.com/HardcoreMonk/kvfs
cd kvfs
docker compose up -d --build     # edge × 1 + dn × 3
./scripts/demo-alpha.sh          # 3-way replication 시연
```

기대 출력:

```
=== α demo: 3-way replication durability ===

[1/4] Signing PUT URL...
     → http://localhost:8000/v1/o/demo/hello.txt?sig=abc123...&exp=1729991234

[2/4] PUT object...
     chunk_id=e3b0c44298fc1c149afbf4c8996fb9242...
     replicas=dn1:8080,dn2:8080,dn3:8080

[3/4] Verify all 3 DNs have the chunk on disk...
     ✅ dn1 has e3b0c44...
     ✅ dn2 has e3b0c44...
     ✅ dn3 has e3b0c44...

[4/4] Kill dn1, GET should still succeed...
     ✅ GET after dn1 kill succeeded, body matches

✅ 3-way replication verified: object survived 1 DN failure
```

## 코드 핵심: `Coordinator.WriteChunk`

병렬 fanout + 쿼럼 수집이 **이 함수 하나에 담깁니다**:

```go
func (c *Coordinator) WriteChunk(ctx context.Context, chunkID string, data []byte) ([]string, error) {
    type result struct {
        dn  string
        err error
    }
    results := make(chan result, len(c.dns))
    var wg sync.WaitGroup
    for _, dn := range c.dns {
        wg.Add(1)
        go func(dn string) {
            defer wg.Done()
            err := c.putChunk(ctx, dn, chunkID, data)
            results <- result{dn: dn, err: err}
        }(dn)
    }
    go func() { wg.Wait(); close(results) }()

    var ok []string
    var errs []error
    for r := range results {
        if r.err == nil {
            ok = append(ok, r.dn)
        } else {
            errs = append(errs, fmt.Errorf("%s: %w", r.dn, r.err))
        }
    }
    if len(ok) < c.quorumWrite {
        return ok, fmt.Errorf("quorum not reached: %d/%d", len(ok), c.quorumWrite)
    }
    return ok, nil
}
```

goroutine 3개 · channel 1개 · WaitGroup 1개. **이게 분산 스토리지의 "코어"** 입니다.

## Content-addressable chunk — git·IPFS와 같은 모델

객체를 올릴 때 edge가 먼저 `sha256(body)`를 계산합니다. 이 해시가 chunk ID가 되고, DN에는 `chunks/<id[:2]>/<id[2:]>` 경로로 저장됩니다.

결과:

1. **무료 dedup**: 동일 내용 두 번 올려도 DN 디스크 사용량 1배
2. **무료 무결성**: 읽어온 바이트가 실제로 그 ID인지 검증 가능
3. **디버그 쉬움**: `ls dn1:/var/lib/kvfs-dn/chunks/e3/b0c44...`

## 다음 편 예고

- **Ep.2**: UrlKey presigned URL — 100줄 HMAC 구현
- **Ep.3**: dedup이 진짜 공짜인지 디스크 사용량으로 증명
- **Ep.4**: 1 DN이 영구적으로 죽었을 때 — repair 큐와 자동 복구 설계 (Season 2 프리뷰)

## 참고 자료

- 전체 아키텍처: [`docs/ARCHITECTURE.md`](../docs/ARCHITECTURE.md)
- kvfs가 파생된 과거 운영 사례에서 분산 시스템 분석: [`NAMING.md`](../NAMING.md) 의 "기원" 섹션

---

*kvfs는 교육적 레퍼런스입니다. 프로덕션에 배포하지 마세요. 블로그 시리즈 전체가 "여기까지는 충분히 단순하다"의 한계를 탐색합니다.*
