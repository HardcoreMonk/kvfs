# Episode 14 — Streaming PUT/GET: 1 GB 객체를 4 MiB RAM 으로

> **Series**: kvfs — distributed object storage from scratch
> **Season**: 4 (performance/efficiency) — **Ep.1**
> **ADR**: [017](../docs/adr/ADR-017-streaming-put-get.md) · **Demo**: `./scripts/demo-sigma.sh`

---

## io.ReadAll 의 죄

Season 1-3 동안 edge 의 PUT/GET 핸들러는 다음 패턴이었다:

```go
body, _ := io.ReadAll(r.Body)        // ← 1 GB 객체 = 1 GB 메모리
pieces := chunker.Split(body, chunkSize)
for _, p := range pieces {
    coord.WriteChunk(ctx, p.ID, p.Data)
}
```

작은 객체 (~수 MB) 에는 죄가 안 된다. 그런데:

- **동시성**: 1 GB PUT 가 N 개 들어오면 edge 가 N GB 받게 됨
- **첫 바이트 지연**: client → edge → DN propagation 이 시작되기 전에 client → edge 가
  먼저 끝나야 함. 1 GB 짜리는 수십 초 동안 edge RAM 에 쌓이는 동안 DN 은 idle
- **GET client 끊김**: edge 가 모든 chunk 를 join 한 뒤 보내므로 client 가 중간에
  끊으면 모든 fetch 비용이 낭비

ADR-011 (chunking) 이 *논리적* 분할은 이미 도입했으므로 *물리적* (메모리) 흐름만
바꾸면 된다.

## Streaming primitives — `chunker.Reader`

```go
// internal/chunker/stream.go
type StreamPiece struct {
    ID   string  // hex(sha256(Data))
    Data []byte
    Size int64
}

type Reader struct {
    src       io.Reader
    chunkSize int
    buf       []byte
    done      bool
}

func NewReader(src io.Reader, chunkSize int) *Reader { ... }

func (r *Reader) Next() (*StreamPiece, error) {
    if r.done { return nil, io.EOF }
    n, err := io.ReadFull(r.src, r.buf)
    switch err {
    case nil:                                  // full chunk
    case io.EOF:                               // exact boundary
        r.done = true; return nil, io.EOF
    case io.ErrUnexpectedEOF:                  // short tail
        r.done = true
    default:
        return nil, fmt.Errorf("chunker: read: %w", err)
    }
    if n == 0 { return nil, io.EOF }
    data := make([]byte, n)                    // defensive copy
    copy(data, r.buf[:n])
    sum := sha256.Sum256(data)
    return &StreamPiece{ID: hex.EncodeToString(sum[:]), Data: data, Size: int64(n)}, nil
}
```

핵심 디자인:

- **`io.ReadFull` 활용** — full chunk → `nil`, partial tail → `io.ErrUnexpectedEOF`,
  exact boundary → `io.EOF`. 세 케이스 모두 자연스럽게 처리.
- **defensive copy** — 내부 `buf` 는 재사용하지만 returned `Data` 는 fresh slice.
  caller 가 다음 `Next()` 동안 piece 를 keep 가능.
- **0-byte body** — 첫 호출이 곧 `io.EOF`. handler 가 "empty body" 거부.

## handlePut 리팩토링

```go
reader := chunker.NewReader(r.Body, s.chunkSize())
for i := 0; ; i++ {
    piece, err := reader.Next()
    if errors.Is(err, io.EOF) { break }
    if err != nil { return 4xx }
    
    replicas, _ := s.Coord.WriteChunk(ctx, piece.ID, piece.Data)
    chunkRefs = append(chunkRefs, store.ChunkRef{...})
    totalSize += piece.Size
}
if len(chunkRefs) == 0 { return 4xx "empty body" }
Store.PutObject(meta)
```

메모리 사용량: **chunkSize bytes** (default 4 MiB) — 객체 크기 무관.

흐름:
- chunk 1 read → chunk 1 send to DNs → chunk 1 GC
- chunk 2 read → chunk 2 send → chunk 2 GC
- ...

DN 들이 chunk N 받는 동안 client 는 이미 chunk N+1 을 edge 로 보내는 중. 파이프라인.

## handleGet 리팩토링

```go
w.Header().Set("Content-Length", fmt.Sprintf("%d", meta.Size))
w.Header().Set("X-KVFS-Chunks", ...)
// (헤더 먼저 — Write 시점에 flush됨)

for i, c := range meta.Chunks {
    data, _, err := s.Coord.ReadChunk(ctx, c.ChunkID, c.Replicas)
    if err && i == 0 {
        writeError(w, 502, ...)              // first chunk: status 변경 가능
        return
    }
    if err {
        return                                // mid-stream: connection abort
    }
    w.Write(data)                              // ResponseWriter 가 flush
}
```

핵심:
- **헤더 우선 flush** — Content-Length 는 meta.Size 로 미리 알 수 있어서 chunked
  encoding 안 써도 됨
- **mid-stream error = connection abort** — HTTP 의 본질적 한계 (200 status 이미 보냄).
  client 가 sha256 검증으로 catch
- **메모리 chunkSize per iteration** — 다음 chunk 전에 이전 chunk GC

## 라이브 데모 (`./scripts/demo-sigma.sh`)

64 MiB random body PUT/GET, default chunkSize 4 MiB → 16 chunks:

```
--- step 1: generate 64 MiB random body ---
  src sha256: dfd3a32984b0c1a79c12b413a79a77fc14310463c74b1c346af58b8e24de5000

--- step 2: PUT (streaming — edge holds at most chunkSize at a time) ---
  chunks created: 16
  PUT elapsed: .232976215s

--- step 3: edge memory snapshot during PUT (docker stats) ---
NAME      MEM USAGE / LIMIT     CPU %
edge      22.74MiB / 62.56GiB   0.00%

--- step 4: GET (streaming — chunks arrive sequentially) ---
  GET elapsed: .084334371s
  dl  sha256: dfd3a32984b0c1a79c12b413a79a77fc14310463c74b1c346af58b8e24de5000

✅ σ demo PASS
```

64 MiB 객체 PUT/GET 동안 edge 메모리 **22.74 MiB**. 대부분 Go runtime base +
chunkSize buffer + replication fanout 임시 buffer. **객체 사이즈와 독립**.

io.ReadAll 시절이었다면 64 MiB heap (+ chunker.Split 의 16 × 4 MiB sub-slice
참조) = 약 80-128 MiB 사용했을 것. **3-6× 절감**.

PUT 0.23s, GET 0.08s — chunked write + sequential read 가 모두 잘 작동.

## Edge case 검증 (5 신규 unit tests)

```
TestReaderExactBoundary       — 32 bytes / chunkSize 16 = 정확히 2 chunks
TestReaderShortTail           — 28 bytes / chunkSize 10 = [10, 10, 8]
TestReaderEmpty               — 0 bytes → 첫 Next() = io.EOF
TestReaderRoundTripWithSplit  — Reader 출력 == Split() 출력 (equivalence)
TestReaderPropagatesError     — flakey reader → wrapped error
```

## 비범위 (다음 ep)

- **EC streaming PUT/GET** — Reed-Solomon encoder 가 K data shard 가 모두 있어야
  M parity 만들 수 있음 (matrix multiplication). per-stripe streaming 가능하나
  encoder 인터페이스 변경 필요. 별도 ADR
- **sync.Pool buffer reuse** — 현재 chunk 별 alloc → GC 압박. 측정 후 결정
- **HTTP/2 / chunked transfer** — 현재 Content-Length 기반. streaming-friendly이긴 하나
  chunked encoding 특별 이득 없음

## 코드·테스트

- `internal/chunker/stream.go` — Reader + StreamPiece (~80 LOC)
- `internal/chunker/stream_test.go` — 5 tests
- `internal/edge/edge.go` — handlePut/handleGet 리팩토링 (~50 LOC 변경)
- `scripts/demo-sigma.sh` — 64 MiB 라이브 검증

총 변경: ~200 LOC.

## 다음 ep ([P4-02](../docs/FOLLOWUP.md))

- **ADR-018 — Content-defined chunking** (rabin/buzhash 비정렬 dedup)
- **ADR-019 — WAL / incremental backup** (분 단위 RPO)
- **ADR-031 — Auto leader election** (Raft / etcd)

---

*kvfs 는 교육적 레퍼런스. 본 ep 의 streaming 패턴은 production-grade chunked
upload 가 아니다 — 진짜 production 은 multipart upload, resumable, etag 등 추가
필요.*
