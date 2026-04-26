# ADR-017 — Streaming PUT/GET (io.Reader 기반, replication mode)

## 상태
Accepted · 2026-04-26

## 맥락

기존 `handlePut` / `handleGet` 은 `io.ReadAll(r.Body)` 으로 전체 객체를 메모리에
적재한 뒤 `chunker.Split(body)` / `chunker.Join` 으로 처리했다. 이 모델은 작은
객체 (~수 MB) 에는 깔끔하지만:

- **메모리**: edge 가 동시에 큰 PUT N 개 받으면 N × object_size 가 RAM 점유
- **첫 바이트 지연**: PUT 의 client → DN propagation 이 시작되기 전에 전체 body 를
  먼저 받아야 함 (round-trip latency 누적)
- **GET 의 client 접속 끊김 비용**: edge 가 join 까지 마친 뒤 보내므로 client 가
  중간에 끊으면 모든 fetch 가 낭비

ADR-011 (chunking) 이 *논리적* 분할 이미 도입했으므로 *물리적* (메모리) 흐름만
chunk 단위로 바꾸면 된다.

## 결정

### Streaming primitives — `internal/chunker/stream.go`

```go
type StreamPiece struct { ID string; Data []byte; Size int64 }
type Reader struct { /* src + buf + done */ }
func NewReader(src io.Reader, chunkSize int) *Reader
func (r *Reader) Next() (*StreamPiece, error)  // returns io.EOF on exhaustion
```

- 내부 buffer = chunkSize (재사용)
- 각 `Next()` 가 returning piece 의 Data 는 **defensive copy** (caller 가 다음
  Next 호출 동안 보유 가능)
- `io.ReadFull` 활용: full chunk → `nil`, partial tail → `io.ErrUnexpectedEOF` →
  마지막 chunk 로 정상 처리
- 0-byte body → 첫 호출이 곧 `io.EOF`

### handlePut (replication) 리팩토링

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

메모리 사용량: **chunkSize bytes** (default 4 MiB) — 객체 크기와 무관.

### handleGet (replication) 리팩토링

```go
// 헤더 먼저 (Content-Length = meta.Size 이미 알고 있음)
w.Header().Set("Content-Length", ...)
for i, c := range meta.Chunks {
    data, _, err := s.Coord.ReadChunk(ctx, c.ChunkID, c.Replicas)
    if err && i == 0 { return 502 }      // first chunk: status 변경 가능
    if err { abort connection }           // mid-stream: header 이미 flush
    w.Write(data)                          // ResponseWriter 가 alocates
}
```

메모리 사용량: **chunkSize bytes per iteration** — 다음 chunk 전에 이전이 GC.

### EC mode (ADR-008) 는 비범위

EC encoder 는 K data shard 가 모두 있어야 M parity 를 만들 수 있다 (matrix
multiplication). per-stripe streaming 은 가능하나 Reed-Solomon Encoder 인터페이스
변경이 필요해 본 ADR 범위 밖. EC streaming 은 후속 ADR 후보.

현재 코드는 `if X-KVFS-EC: ...` 분기 시 기존 `io.ReadAll` 그대로 유지.

## 결과

**긍정**:
- 큰 객체 (1 GB+) PUT/GET 가능 — edge RAM 4 MiB × 동시 요청 수
- First-byte 지연 감소 — chunk 1 받자마자 DN 전송 시작 (PUT) / 전송 시작 (GET)
- Client 끊김 시 낭비 최소화 — GET 은 abort, PUT 은 다음 chunk 안 보냄
- 5 신규 unit tests (boundary / short tail / empty / split-equivalence / I/O error)
- 기존 모든 tests + demo (α, ι, …) PASS — 동작 변경 0

**부정**:
- GET mid-stream 실패가 200 status 로 partial body 노출 — HTTP 의 본질적 한계
  (Content-Length 와 실 byte 수 mismatch). client 측 sha256 검증 권장
- EC 는 여전히 buffered — 별도 후속 작업
- chunker.Split / chunker.Join 함수는 유지 (구 코드 호환 + 테스트 자산)

**트레이드오프**:
- chunk 별 alloc → GC 압박 약간 증가. 중간에 sync.Pool 도입 가능하나 complexity
  증가 — 측정 후 결정
- HTTP transfer-encoding: chunked vs Content-Length: 후자 (본 ADR) 가 client
  buffer 결정에 유리

## 데모 시나리오 (`scripts/demo-sigma.sh`)

64 MiB random body PUT/GET → sha256 round-trip 일치, edge memory peak 측정 (실측
~10 MiB heap).

## 관련

- `internal/chunker/stream.go` — Reader + StreamPiece (~80 LOC)
- `internal/chunker/stream_test.go` — 5 tests
- `internal/edge/edge.go` — handlePut / handleGet 리팩토링 (~50 LOC 변경)
- `scripts/demo-sigma.sh` — 64 MiB streaming 라이브
- 후속 ADR 후보: streaming EC PUT/GET, sync.Pool buffer reuse
