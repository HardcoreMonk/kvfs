# kvfs from scratch, Ep.5 — 1 GiB 객체를 부수는 30줄, 그리고 placement·rebalance·gc가 그대로 일하는 이유

> Season 2 · Episode 4. ADR-006의 "1 object = 1 chunk" MVP 가정을 ADR-011이 supersede합니다. 더 놀라운 건 placement·rebalance·gc 알고리즘이 **단 한 줄도 변하지 않았다**는 점입니다.

## Ep.4가 닫지 못한 가정

Ep.1~Ep.4가 시연한 모든 객체는 38바이트 짜리 텍스트였습니다. 그 동안 분명히 보지 못한 척한 가정 하나:

> "한 객체 = 한 청크" (ADR-006)

3-way replication 시연에는 무관했고 (1 chunk × 3 replicas = 3 PUT), HRW placement에도 무관 (Pick 한 번), rebalance·gc에도 무관 (객체 하나에 chunk_id 하나).

이 가정이 깨지는 순간이 1 GiB 객체입니다:

1. **메모리 폭발** — edge가 PUT body 전체를 RAM에 로드, sha256
2. **단일 DN 부하** — 1 GiB 트래픽이 R 개 DN 에만 집중
3. **dedup 무효** — 1 GiB 중 1% 다른 두 객체 → sha256 완전히 달라 dedup 0
4. **부분 복구 불가** — DN 1개 다운 시 1 GiB 통째로 재복사
5. **재배치 비용** — rebalance/gc가 큰 객체를 통째로 이사

답은 30년 된 표준: **고정 크기 청크로 자르기**.

## ADR-006 → ADR-011, 한 줄짜리 의미 변화

```
이전: object_id == chunk_id (1:1)
이후: object_id → [chunk_id_1, chunk_id_2, ..., chunk_id_N] (1:N, 순서 보존)
```

각 chunk_id 는:
- 여전히 `sha256(chunk_data)` (ADR-005, 변경 없음)
- 여전히 `placement.Pick` 의 입력 (ADR-009, 변경 없음)
- 따라서 R개 DN에 fanout (코디네이터, 변경 없음)

placement는 chunk_id 별로 **독립적**으로 Pick 됩니다. 같은 객체의 청크 4개가 서로 다른 DN 집합으로 흩뿌려질 가능성 매우 높음 (load balancing 부수효과).

## 가장 단순한 답이 정답

CDC (content-defined chunking, rabin/buzhash) 같은 우아한 방식들이 있습니다. 그러나 kvfs의 약속은 한 페이지 ADR. **고정 크기**가 압도적으로 단순하고, 정렬된 데이터(같은 파일을 PUT/PUT) 의 dedup엔 충분합니다. 비정렬 변경(파일 내부 byte 추가)에 대한 dedup은 CDC가 더 잘 합니다 — ADR-018 후보로 적어둠.

```go
// internal/chunker/chunker.go (전체 핵심)
func Split(body []byte, chunkSize int) []Chunk {
    if len(body) == 0 { return nil }
    n := (len(body) + chunkSize - 1) / chunkSize
    out := make([]Chunk, 0, n)
    for off := 0; off < len(body); off += chunkSize {
        end := off + chunkSize
        if end > len(body) { end = len(body) }
        data := body[off:end]
        sum := sha256.Sum256(data)
        out = append(out, Chunk{
            Data: data,
            ID:   hex.EncodeToString(sum[:]),
            Size: int64(end - off),
        })
    }
    return out
}
```

12줄. 마지막 청크는 잔여 크기. ID는 콘텐츠 해시. 끝.

`Join`은 정확히 reverse — 청크들을 순서대로 받아 sha256 검증 후 concat. 이것도 ~25줄.

## ObjectMeta 스키마 변경 (breaking)

```go
// 이전 (ADR-006)
type ObjectMeta struct {
    ChunkID  string
    Replicas []string
    Size     int64
    ...
}

// 이후 (ADR-011)
type ObjectMeta struct {
    Chunks []ChunkRef    // 순서 보존 (concat 시 사용)
    Size   int64         // 전체 객체 크기
    ...
}
type ChunkRef struct {
    ChunkID  string
    Size     int64
    Replicas []string
}
```

기존 bbolt 메타 파일은 **in-place migration 안 함**. 데모 환경이라 `down.sh && up.sh` 로 리셋. `LegacyChunkID` / `LegacyReplicas` 필드를 struct에 남겨 옛 JSON이 decode는 되도록 했고, `normalizeLegacy()` 가 read 시점에 새 shape로 변환. **첫 write 시점에 옛 필드는 디스크에서 제거**됩니다.

## edge handler — split / join 호출만

```go
// PUT — 핵심 흐름
pieces := chunker.Split(body, s.chunkSize())
chunkRefs := make([]store.ChunkRef, len(pieces))
for i, p := range pieces {
    replicas, _ := s.Coord.WriteChunk(ctx, p.ID, p.Data)
    chunkRefs[i] = store.ChunkRef{ChunkID: p.ID, Size: p.Size, Replicas: replicas}
}
meta := &store.ObjectMeta{Chunks: chunkRefs, Size: int64(len(body)), ...}
s.Store.PutObject(meta)

// GET — 핵심 흐름
specs := make([]chunker.JoinSpec, len(meta.Chunks))
chunkReplicas := map[string][]string{}
for i, c := range meta.Chunks {
    specs[i] = chunker.JoinSpec{ChunkID: c.ChunkID, Size: c.Size}
    chunkReplicas[c.ChunkID] = c.Replicas
}
body, _ := chunker.Join(specs, meta.Size, func(id string) ([]byte, error) {
    data, _, err := s.Coord.ReadChunk(ctx, id, chunkReplicas[id])
    return data, err
})
```

PUT 루프 5줄, GET 변환 4줄 + Join 1줄. 코디네이터·placement·store 의 인터페이스는 **하나도 변하지 않음**.

부분 PUT 실패 시 (5청크 중 4청크 quorum 후 마지막 실패): 메타를 commit 하지 않음. 이미 쓰인 청크들은 orphan → **GC가 청소**. 자연스럽게 ADR-012가 일을 받음.

## Rebalance/GC가 변하지 않는 이유

이 두 패키지는 **이미 chunk_id 단위 사고 모델** 입니다. ADR-009 시점에 "1 object = 1 chunk" 였으니 chunk가 의식되지 않았을 뿐, 실제 알고리즘 변경은 미미합니다:

```go
// rebalance ComputePlan — 이전
desired := coord.PlaceChunk(obj.ChunkID)
actual  := obj.Replicas

// rebalance ComputePlan — 이후
for ci, chunk := range obj.Chunks {
    desired := coord.PlaceChunk(chunk.ChunkID)
    actual  := chunk.Replicas
    ...
    Migration{ChunkIndex: ci, ChunkID: chunk.ChunkID, ...}
}
```

inner loop이 추가되고 `ChunkIndex` 가 Migration 에 들어간 게 전부. `migrateOne` 은 `Store.UpdateReplicas` 대신 `UpdateChunkReplicas(bucket, key, chunkIndex, ...)` 호출. **로직 변경 0**.

GC는 더 작은 변경:

```go
// claimed-set — 이전
for _, addr := range obj.Replicas { claimed[obj.ChunkID][addr] = true }

// claimed-set — 이후
for _, c := range obj.Chunks {
    for _, addr := range c.Replicas { claimed[c.ChunkID][addr] = true }
}
```

inner loop 하나 추가. **GC의 두 안전망**(claimed-set + min-age) 은 그대로 유효.

이것이 **올바른 추상화의 보상**입니다. ADR-009를 청크 단위로 짤 때 "어차피 chunk_id가 placement key" 로 결정한 덕분에, ADR-011 추가가 거의 비어있는 PR.

## 라이브 데모 — `demo-iota.sh`

```
=== ι demo: chunking (ADR-011) ===

[1/6] Reset cluster with EDGE_CHUNK_SIZE=65536 bytes (64 KiB)
  ✅ 4 DNs + edge up (chunk_size=65536)

[2/6] Generate 262144-byte random body
  body bytes:  262144
  body sha256: 814c1bd778ba0eb0ba17631787a5f177..

[3/6] PUT object (expecting 4 chunks)
{
  "bucket": "demo-iota",
  "chunk_size": 65536,
  "chunks": [
    {"chunk_id": "c4939f0dd7db37f3...", "size": 65536, "replicas": ["dn1","dn4","dn2"]},
    {"chunk_id": "7c62c47c7dc286de...", "size": 65536, "replicas": ["dn3","dn2","dn1"]},
    {"chunk_id": "f2b0943e7e5bb0dc...", "size": 65536, "replicas": ["dn1","dn4","dn2"]},
    {"chunk_id": "c8793d19e61f201c...", "size": 65536, "replicas": ["dn2","dn1","dn4"]}
  ],
  "key": "big-blob.bin",
  "size": 262144,
  "version": 1
}
```

256 KiB 랜덤 객체 → 정확히 4개 청크. 각 chunk_id 는 다른 sha256 (랜덤이니까), 따라서 **각각 독립적으로 HRW 배치**.

```
[4/6] Per-chunk replica placement (each chunk independently HRW-placed)
  chunk[0]  id=c4939f0dd7db37f3..  size= 65536  replicas=['dn1', 'dn4', 'dn2']
  chunk[1]  id=7c62c47c7dc286de..  size= 65536  replicas=['dn3', 'dn2', 'dn1']
  chunk[2]  id=f2b0943e7e5bb0dc..  size= 65536  replicas=['dn1', 'dn4', 'dn2']
  chunk[3]  id=c8793d19e61f201c..  size= 65536  replicas=['dn2', 'dn1', 'dn4']
```

흥미로운 관찰:
- **chunk[0] 과 chunk[2] 는 같은 replica 셋** (둘 다 [dn1, dn4, dn2]). HRW 가 우연히 같은 상위 3개를 골랐을 뿐. **chunk_id 가 다르면 independent decision** — 그저 우연 일치
- **dn3 는 단 하나의 청크에만** 등장 (chunk[1]). 다른 3 청크는 dn3 를 상위 3에서 제외. 이게 다음 disk count 의 dn3=1 의 원인

```
[5/6] GET object → reassemble + integrity check (sha256)
  got bytes:   262144  (want 262144)
  got sha256:  814c1bd778ba0eb0ba17631787a5f177..
  ✅ round-trip identical

[6/6] Disk distribution across 4 DNs
  dn1  chunk count = 4
  dn2  chunk count = 4
  dn3  chunk count = 1
  dn4  chunk count = 3
  total replica copies on disk: 12  (expected 4 chunks × 3 replicas = 12)
```

세 가지 신호:

1. **GET sha256 일치** — 4 청크 reassemble + 각 청크별 sha256 검증 + 전체 size 검증 모두 통과
2. **chunk distribution이 DN 별로 균등하지 않음** — N=4 / 청크=4 의 작은 표본에서는 자연스러움. 청크 수가 많아지면 균등에 수렴 (Ep.2 의 N=10000 케이스 참고)
3. **총 12 디스크 카피 = 4 × 3** — 메타 의도와 정확히 일치 (rebalance/gc 안 돌렸으니 over-replicate 없음)

## 단위 테스트 — 13 케이스로 chunker만 검증

`internal/chunker/chunker_test.go`:

| 카테고리 | 케이스 |
|---|---|
| Split | empty, exact-1, multiple-even, with-remainder, dedup-same-id, panic-on-zero, huge-chunk-size |
| Join | happy, wrong-hash-rejected, wrong-size-rejected, fetch-error-propagates, total-size-mismatch |
| Both | round-trip on 1 KiB pseudo-random |

13/13 PASS. 인터페이스는 작고 (Split/Join + types), 검증은 boundary 위주. 운영 시 chunker 자체 버그가 가장 무서운 곳이라 테스트가 두꺼움.

전체 풀: chunker 13 + placement 7 + rebalance 8 + gc 9 + urlkey 5 = **42 PASS**.

## 솔직한 한계 — 더 풀어야 할 것들

1. **메모리 사용은 여전히 O(전체 body)** — `io.ReadAll` 후 `Split`. 진짜 streaming 은 chunker가 `io.Reader` 받아 청크 단위로 emit해야 함 → ADR-017 후보
2. **읽기 latency = N × roundtrip** — N 청크 순차 읽기. 병렬 읽기는 RAM 사용량 ↑ + 순서 메타 추적. MVP 는 순차 (가독성)
3. **dedup 효율은 정렬된 데이터에 한정** — 1 GiB 파일 중간에 byte 1개 추가 → 그 위치 이후 모든 청크 경계 어긋남 → dedup 0. CDC (rabin/buzhash) 가 답 → ADR-018 후보
4. **메타 크기** — 1 TiB 객체 + 4 MiB chunk = 256k 청크 ≈ 25 MB JSON. bbolt 가 잘 처리하지만 객체 수 100만 시점에 의미

이런 한계가 있다는 걸 알면서 ADR-011 한 페이지로 끝낸 이유는 같은 원칙:

> **하나의 결정 = 하나의 보장**

ADR-011은 "큰 객체를 작은 청크로 자르고 dedup·placement·rebalance·gc가 chunk 단위로 동작" 만 약속. streaming, CDC, 메타 스케일링은 별도 약속.

## 다음 편 예고

- **Ep.6**: ADR-013 Auto-trigger policy — DN 변경 감지 + rebalance + GC 자동화. 운영자 개입 없이 클러스터가 스스로 정렬
- **Ep.7**: ADR-008 Reed-Solomon EC — placement가 (K+M)개 노드 선택. R-way replication 의 디스크 비효율 문제. Season 2 의 보스전
- **Ep.8 후보**: ADR-017 Streaming PUT/GET — io.Reader 기반 chunker, 진짜 메모리 상한
- **Ep.9 후보**: ADR-018 Content-defined chunking — 비정렬 데이터 dedup 효율

## 직접 돌려보기

```bash
git clone https://github.com/HardcoreMonk/kvfs   # 공개 시점 기준
cd kvfs
docker build -t kvfs-edge:dev --target kvfs-edge .
docker build -t kvfs-dn:dev   --target kvfs-dn   .
docker run --rm -v "$PWD:/src" -w /src -e CGO_ENABLED=0 \
  golang:1.26-alpine sh -c 'go build -o ./bin/kvfs-cli ./cmd/kvfs-cli'

./scripts/demo-iota.sh
```

`demo-iota.sh` 는 매번 down → up. 출력의 chunk_id 는 매번 다릅니다 (랜덤 body). 그러나:

- 항상 정확히 4 청크 (256 KiB / 64 KiB)
- 각 청크 사이즈 = 65536
- GET sha256 == PUT sha256
- 총 디스크 카피 = 12 (4 청크 × 3 replicas, rebalance/gc 안 돌렸으면)

위 4개 불변식이 ADR-011 의 약속과 일치합니다.

큰 객체로 시도해 보세요:

```bash
EDGE_CHUNK_SIZE=$((4*1024*1024)) ./scripts/up.sh    # 4 MiB chunks (default)
# 16 MiB random object → 4 chunks
head -c $((16*1024*1024)) /dev/urandom | curl -X PUT --data-binary @- ...
```

## 참고 자료

- 이 ADR: [`docs/adr/ADR-011-chunking.md`](../docs/adr/ADR-011-chunking.md) (supersedes ADR-006)
- 구현: [`internal/chunker/chunker.go`](../internal/chunker/chunker.go)
- 테스트: [`internal/chunker/chunker_test.go`](../internal/chunker/chunker_test.go) — 13 케이스
- ObjectMeta 스키마: [`internal/store/store.go`](../internal/store/store.go) `ObjectMeta` + `ChunkRef`
- edge handler: [`internal/edge/edge.go`](../internal/edge/edge.go) `handlePut`/`handleGet`/`handleDelete`
- rebalance 변경: [`internal/rebalance/rebalance.go`](../internal/rebalance/rebalance.go) `ComputePlan` inner loop
- gc 변경: [`internal/gc/gc.go`](../internal/gc/gc.go) `buildClaimedSet` inner loop
- 데모: [`scripts/demo-iota.sh`](../scripts/demo-iota.sh)
- 이전 episode (gc): [`blog/04-gc.md`](04-gc.md)

---

*kvfs는 교육적 레퍼런스입니다. 프로덕션에 배포하지 마세요. Season 2의 약속 — "라우팅이 되면, 다음에 무엇이 깨지는가" — 가 또 한 번 이행되었습니다. chunking 이 들어왔으니 다음 episode는 자동화, 그 다음은 EC.*
