# Episode 15 — Content-defined chunking: 1 byte 끼워도 chunk 가 정렬되는 이유

> **Series**: kvfs — distributed object storage from scratch
> **Season**: 4 (performance/efficiency) · **Episode**: 2
> **ADR**: [018](../docs/adr/ADR-018-content-defined-chunking.md) · **Demo**: `./scripts/demo-tau.sh`

---

## Fixed chunking 의 작은 죄

ADR-011 (chunking) 의 fixed-size 분할은 단순하고 빠르다. 객체를 `chunkSize` 바이트
단위로 잘라 각 chunk 의 sha256 을 chunk_id 로 사용. content-addressable storage
와 합쳐지면 **자동 dedup**:

```
file F1 = [4 MiB random][4 MiB random][4 MiB random][4 MiB random]
                C1            C2            C3            C4

file F1' = (정확히 같은 내용 다시 PUT)
              C1            C2            C3            C4    ← 동일 sha256
                                                                   → DN 디스크 X 4
                                                                   → 메타 ref만 추가
```

완벽해 보인다. 그런데 **shift sensitivity** 가 숨어있다:

```
file F2 = [1 byte][4 MiB random][4 MiB random][4 MiB random][4 MiB random]
              D1            D2            D3            D4            D5(짧음)
```

D1 은 1 byte + 4MiB-1 의 새 조합이라 다른 sha256.
D2 는 F1 의 `C1[1:] + C2[0]` — 다른 boundary.
**모든 chunk 가 다름**. dedup 0.

backup 시스템 (Restic, Borg) 에서는 이 시나리오가 흔하다:
- log 파일 앞에 timestamp prefix 1 줄 추가
- VM disk image 의 metadata block 1 byte 변경
- Database WAL prepend

fixed chunking 으로는 매번 전체 파일을 새 chunk 로 저장. 디스크 폭발.

## CDC = boundary 가 content 를 따라간다

핵심 아이디어: chunk 경계를 **offset 으로 박지 말고 content 가 결정하게**.

```
매 byte i 에서 rolling hash fp 계산
fp = (fp << 1) + GearTable[byte_i]
fp 가 어떤 마스크 조건 만족하면 → boundary
```

`fp` 는 마지막 N byte (window) 의 효과만 반영하는 sliding hash. 즉 같은
N-byte window 가 들어오면 같은 fp 값이 나온다. **window 내에서 어떤 byte
시퀀스가 boundary 인지** 가 content 의 함수 → **shift 해도 같은 boundary**.

F1 과 F2 의 boundary:

```
F1: |---------|------|----------|--------|------|
F2: X|---------|------|----------|--------|------|
    ↑
    1 byte 삽입 후, 두 번째 boundary 부터는 F1 과 정렬
```

첫 chunk 만 다르고 나머지는 동일. dedup 거의 100%.

## FastCDC — 산업 표준 (Restic, Borg)

여러 CDC 알고리즘 중 FastCDC (Xia et al. 2016) 채택:

| 알고리즘 | 속도 | 코드 | 채택 |
|---|---|---|---|
| Rabin polynomial | 1× | 다항식 산술 | LBFS, rsync |
| buzhash | 2× | XOR 테이블 | restic 초기 |
| **FastCDC** | **3×** | **gear 테이블** | **Restic, Borg, rdedup** |

FastCDC 의 핵심 코드 (~50 LOC):

```go
// Phase 1: warm fp without testing (skip up to MinSize)
for i := 0; i < MinSize; i++ {
    fp = (fp << 1) + GearTable[data[i]]
}
// Phase 2: MinSize..NormalSize — strict mask (chunk 가 평균 크기에 도달하기 어려움)
for i := MinSize; i < NormalSize; i++ {
    fp = (fp << 1) + GearTable[data[i]]
    if fp & maskS == 0 { return i + 1 }       // boundary!
}
// Phase 3: NormalSize..MaxSize — loose mask (cap 전에 cut 유도)
for i := NormalSize; i < MaxSize; i++ {
    fp = (fp << 1) + GearTable[data[i]]
    if fp & maskL == 0 { return i + 1 }
}
return MaxSize  // 강제 cut
```

3-region scan 의 의미:
- **Phase 1 (skip)** — 너무 작은 chunk 방지 (1 KB 짜리 수만 개 = 메타 폭발)
- **Phase 2 (strict)** — 목표 평균 크기에 도달하도록 boundary 를 일부러 찾기 어렵게
- **Phase 3 (loose)** — 너무 커지지 않도록 boundary 발견 확률 4× 증가
- **MaxSize cap** — 끝까지 boundary 못 찾아도 강제 cut (한 chunk 16 MiB+ 방지)

GearTable 은 byte → uint64 무작위 룩업 (deterministic seed 0xCAFEBABE).
같은 cluster 의 모든 edge 가 같은 boundary 를 만들어내야 dedup 작동.

## 코드 — 공통 인터페이스로 fixed/CDC 통합

```go
// internal/edge/edge.go
type pieceReader interface {
    Next() (*chunker.StreamPiece, error)
}

func (s *Server) newPutReader(src io.Reader) pieceReader {
    if s.CDCEnabled {
        return chunker.NewCDCReader(src, s.CDCConfig)
    }
    return chunker.NewReader(src, s.chunkSize())
}
```

`chunker.Reader` (fixed, ADR-017) 와 `chunker.CDCReader` (ADR-018) 가 같은
`Next()` 시그니처. handlePut 의 루프 코드는 100% 그대로 — 모드만 다르고
처리 로직 동일.

## env wiring

```bash
EDGE_CHUNK_MODE=fixed   # default — ADR-011 그대로
EDGE_CHUNK_MODE=cdc     # ADR-018 활성화
```

EC mode 는 CDC 무시 (encoder 가 uniform shard size 필요 — ADR-008).
replication path 만 분기.

## 라이브 데모 (`./scripts/demo-tau.sh`)

```
==================================
 mode = fixed
==================================
--- PUT F1 (16 MiB random) ---
  chunks: 4
    176d76ef5b1bbbba 4194304
    f83a6259b6324784 4194304
    bbdff49964080da3 4194304
    5512ff3f8210e61d 4194304
--- PUT F2 (1 byte + F1) ---
  chunks: 5
    390b89865f090acb 4194304    ← 모두 다른 chunk_id
    079ab43d8a9934a0 4194304
    68d75fe9819e1333 4194304
    284d6c38d2378ede 4194304
    a318c24216defe20 1
--- dedup analysis ---
  total chunk refs : 9
  unique chunks    : 9
  shared chunks    : 0           ← 0 overlap
  dedup ratio      : 0.0%

==================================
 mode = cdc
==================================
--- PUT F1 (16 MiB random) ---
  chunks: 5
    aaf09633d44dd936 1925206
    c8651faebf4486f9 4865030
    3fe1796c2df088a1 4234670
    841db28563d0febb 4850614
    e1274943c07105e0  901696
--- PUT F2 (1 byte + F1) ---
  chunks: 5
    76b19c777ed43411 1925207    ← 첫 chunk만 다름 (1 byte 더 길음)
    c8651faebf4486f9 4865030    ← 동일!
    3fe1796c2df088a1 4234670    ← 동일!
    841db28563d0febb 4850614    ← 동일!
    e1274943c07105e0  901696    ← 동일!
--- dedup analysis ---
  total chunk refs : 10
  unique chunks    : 6
  shared chunks    : 4           ← 4/5 chunk 재사용
  dedup ratio      : 40.0%

✅ τ demo PASS
```

CDC 의 마법:

1. F1 의 boundary 가 content 에 의해 정해짐 → variable size (1.9 / 4.9 / 4.2 / 4.9 / 0.9 MiB)
2. F2 = 1 byte + F1 → 첫 boundary 도 1 byte 미루고 나머지 boundary **정확히
   F1 과 같은 byte offset** (단, file 시작에서 +1)
3. 첫 chunk 만 다르고 나머지 4 chunk 의 byte 내용이 정확히 일치 → 같은 sha256

10 chunk ref 중 6 개만 unique 디스크 — **40% dedup** (1 byte shift 시나리오).

## 분산 측정 — chunk size variance

위 출력에서 CDC chunk 크기: 1.9 / 4.9 / 4.2 / 4.9 / 0.9 MiB. 평균 약 3.4 MiB.
NormalSize=4 MiB 설정이지만 mask 의 통계적 특성상 정확히 4 MiB 는 아님.

운영자가 알아둘 것:
- Min 1 MiB ~ Max 16 MiB 사이 어디든 가능 (보장: 그 범위 안)
- 평균 ~ NormalSize 근처 수렴 (큰 객체일수록 안정)
- 마지막 chunk 는 보통 짧음 (남은 bytes < MinSize)

## 비범위

- **EC + CDC 조합** — RS encoder 가 uniform shard size 필요. variable
  shard size 는 별도 디자인 (hash-based + fixed-padded 등). 후속 ADR
- **Multi-window FastCDC** — chunk size 분산을 더 좁히는 변형. 현재 implementation
  으로 충분
- **Adaptive parameter tuning** — workload 따라 NormalSize 자동 조정. 운영
  복잡도 증가 — 측정 후 결정

## 코드·테스트

- `internal/chunker/cdc.go` — CDCReader + GearTable + cutpoint (~180 LOC)
- `internal/chunker/cdc_test.go` — 7 tests (deterministic / shift / boundaries
  / round-trip / empty / short / smaller-than-min)
- `internal/edge/edge.go` — `pieceReader` interface + newPutReader 분기
- `cmd/kvfs-edge/main.go` — `EDGE_CHUNK_MODE` env
- `scripts/demo-tau.sh` — fixed vs CDC dedup 라이브

총 변경: ~400 LOC.

## 다음 ep ([P4-03](../docs/FOLLOWUP.md))

- **ADR-019 — WAL / incremental backup** (분 단위 RPO)
- **ADR-031 — Auto leader election** (Raft / etcd, ADR-022 후속)
- **EC streaming** (ADR-017 follow-up)
- **EC + CDC 조합** (ADR-018 follow-up)

---

*kvfs 는 교육적 레퍼런스. 본 ep 의 FastCDC 구현은 학습용 — production 등급은
multi-window, adaptive parameter, SIMD 가속 등 추가 작업 필요.*
