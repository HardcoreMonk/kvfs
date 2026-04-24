# kvfs from scratch, Ep.1 — 3-way replication을 60줄 Go로

> Season 1 · Episode 1. 공개 오픈소스 데모로서 kvfs의 첫 인사.

## 이 글이 보여주는 것

`./scripts/up.sh` 한 방으로 **4 노드 분산 스토리지 클러스터**가 올라오고, 그중 한 노드를 죽여도 데이터가 살아남는 것을 3분 안에 확인합니다. 핵심 로직은 Go 60줄.

## 왜 또 하나의 분산 스토리지?

Ceph·MinIO·SeaweedFS·JuiceFS 이미 많습니다. kvfs는 **경쟁자가 아닙니다**. **설계 원리를 눈으로 보게 하는 레퍼런스**입니다.

프로덕션 스토리지의 교과서적 개념 두 가지 — **3-way replication + quorum**, **presigned URL** — 을 **각각 100줄 이내**로 시연하는 것이 Season 1의 전부.

## 5분 실습

```bash
git clone https://github.com/HardcoreMonk/kvfs
cd kvfs
./scripts/up.sh                  # edge × 1 + dn × 3
./scripts/demo-alpha.sh          # 3-way replication 시연
./scripts/demo-epsilon.sh        # UrlKey presigned URL 시연
```

### α 데모 실제 출력 (2026-04-25 로컬 실행 기록)

```
=== α demo: 3-way replication durability ===

[1/4] Signing PUT URL...
     → http://localhost:8000/v1/o/demo/hello.txt?sig=9d5962f9ddadb19aa48febf36620c3f7212eeec169fb819e9a9b257c9c271b08&exp=1777062366

[2/4] PUT object...
{
  "bucket": "demo",
  "chunk_id": "68367ef0584f4e2c76b1b2deecaa0b86b9ca0c25487c8ca5e946b73d5630eae1",
  "key": "hello.txt",
  "replicas": ["dn2:8080", "dn3:8080", "dn1:8080"],
  "size": 66,
  "version": 1
}
     chunk_id=68367ef0584f4e2c76b1b2deecaa0b86b9ca0c25487c8ca5e946b73d5630eae1

[3/4] Verify all 3 DNs have the chunk on disk...
     ✅ dn1 has 68/367ef0584f4e2c76...
     ✅ dn2 has 68/367ef0584f4e2c76...
     ✅ dn3 has 68/367ef0584f4e2c76...

[4/4] Kill dn1, GET should still succeed...
     ✅ GET after dn1 kill succeeded, body matches

✅ 3-way replication verified: object survived 1 DN failure
```

포인트 3가지:

- **`chunk_id = hex(sha256(body))`** — 객체 ID가 콘텐츠 해시. `ls` 로도 확인 가능
- **`replicas` 배열** — 정확히 3개 DN이 ack. 모두 2-of-3 quorum 이상
- **`dn1` kill 후 GET 성공** — 이게 핵심 데모. 복제가 실제로 동작함

### ε 데모 실제 출력

```
=== ε demo: UrlKey presigned URL ===

[1/3] Sign a valid PUT URL (TTL 60s)...
     → http://localhost:8000/v1/o/demo/sign-test.txt?sig=cd90618fd35975e3dbebed0b5d08b2f8f303d462d034bc137df4016e25e4b584&exp=1777058828

[2/3] PUT with valid URL → should succeed...
     ✅ valid URL accepted

[3/3] Sign an already-expired URL → should fail with 401...
     ✅ expired URL rejected (HTTP 401)

✅ UrlKey presigned URL verified
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

### 실제 dedup 검증 (같은 내용 2번 PUT)

```
PUT /demo/dup-a → chunk_id=5fc39811886a0a88...
PUT /demo/dup-b → chunk_id=5fc39811886a0a88...   ← 같은 chunk_id!

DN disk after 4 PUTs (hello + sign-test + dup-a + dup-b):
  dn1: chunk_count=3 bytes_total=138
  dn2: chunk_count=3 bytes_total=138
  dn3: chunk_count=3 bytes_total=138
```

4개 object 를 PUT 했는데 **DN마다 chunk 3개만 존재**. `dup-a` 와 `dup-b` 가 동일 sha256 이라 **1개 chunk 공유**. 이게 "무료 dedup" 의 의미.

### 이로부터 얻는 것

1. **무료 dedup**: 동일 내용 N번 올려도 DN 디스크 1배
2. **무료 무결성**: 읽어온 바이트가 실제로 그 ID인지 검증 가능 (edge 가 read 경로에서 `sha256(body) == chunk_id` 재검증)
3. **디버그 쉬움**: `docker exec dn1 ls /var/lib/kvfs-dn/chunks/68/` → chunk 파일이 진짜 거기 있음

## 설계 결정 배경

왜 이런 구조인가? 브레인스토밍 Phase 1~6 에서 6개 트레이드오프를 결정했고, 전부 `docs/adr/` 에 ADR로 박아뒀습니다:

- **ADR-002**: 2-daemon MVP (coordinator 는 edge 안 library) — "복잡도는 학습의 적"
- **ADR-003**: 모든 통신 HTTP REST — `curl` 로 각 계층 디버그
- **ADR-004**: bbolt 메타데이터 — pure Go, 트랜잭션, 외부 의존 1개
- **ADR-005**: Content-addressable sha256 — 자동 dedup + integrity
- **ADR-006**: 1 object = 1 chunk MVP — chunking 은 Season 2

각 ADR 은 한 페이지. "왜" 와 "포기한 것" 명시.

## 다음 편 예고

- **Ep.2**: UrlKey presigned URL — 100줄 HMAC 구현과 상수 시간 비교의 세부
- **Ep.3**: dedup 이 정말 무료인지 — reference counting · 삭제 동작 해부
- **Ep.4**: 1 DN 영구 사망 시나리오 — repair queue 와 자동 복구 설계 (Season 2 프리뷰)
- **Ep.5**: HTTP vs gRPC vs raw TCP — 왜 MVP는 HTTP 뿐인가, 언제 바꿀까

## 참고 자료

- 전체 아키텍처: [`docs/ARCHITECTURE.md`](../docs/ARCHITECTURE.md)
- ADR 전체: [`docs/adr/`](../docs/adr/)
- kvfs가 파생된 과거 운영 사례에서 분산 시스템 분석: [`NAMING.md`](../NAMING.md) 의 "기원" 섹션

---

*kvfs는 교육적 레퍼런스입니다. 프로덕션에 배포하지 마세요. 블로그 시리즈 전체가 "여기까지는 충분히 단순하다"의 한계를 탐색합니다.*
