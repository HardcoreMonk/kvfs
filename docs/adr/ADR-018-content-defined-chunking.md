# ADR-018 — Content-defined chunking (FastCDC, replication mode opt-in)

## 상태
Accepted · 2026-04-26

## 맥락

ADR-011 의 fixed-size chunking 은 단순하고 빠르지만 **shift sensitivity** 가 있다.
객체 시작 부분에 1 byte 를 끼워 넣으면 모든 chunk 의 sha256 이 바뀌어 dedup
효과가 0 이 된다:

```
F1 = AAAA...   (16 MiB random)         → fixed 4 MiB → 4 chunks {C1, C2, C3, C4}
F2 = X + F1    (1 byte 끼움, ~16 MiB+1) → fixed 4 MiB → 4 chunks {D1, D2, D3, D4}
                                         (모두 C 와 다름)
unique = 8 chunks → dedup 0%
```

CDC 는 chunk 경계를 **content 가 결정**:

```
매 byte i:
  fp = (fp << 1) + GearTable[byte_i]   (rolling hash)
  if i 가 어떤 마스크 조건 만족: 경계
```

`fp` 가 sliding window 라 같은 byte 시퀀스가 들어오면 같은 fp 값 → 같은 경계
지점. 이로써 F1 과 F2 의 경계가 1-byte offset 이후 자연스럽게 정렬된다.

## 결정

### FastCDC (Xia et al. 2016) 채택

대안 비교:

| 알고리즘 | 속도 | 코드 복잡도 | 산업 채택 |
|---|---|---|---|
| Rabin polynomial | 1× (baseline) | 중 (다항식 산술) | LBFS, rsync |
| buzhash | 2× | 저 (XOR 테이블) | restic 초기 |
| **FastCDC** | **3×** | **저 (gear 테이블)** | **Restic, Borg, rdedup** |

FastCDC 가 가장 빠르면서 코드가 가장 짧다 (~50 LOC core). cut 분포도 안정적
(min/normal/max 3-region scan 이 chunk size variance 를 통제).

### 알고리즘 핵심

```go
// 3-region scan
// Phase 1: i ∈ [0, MinSize)        — 무조건 skip (warm fp)
// Phase 2: i ∈ [MinSize, NormalSize) — strict mask (목표 평균에 도달하기 어려움)
// Phase 3: i ∈ [NormalSize, MaxSize) — loose mask (max 전에 cut 유도)
// 비상: i = MaxSize 도달 → 강제 cut

for i := 0; i < MinSize; i++ {
    fp = (fp << 1) + GearTable[data[i]]
}
for i := MinSize; i < NormalSize; i++ {
    fp = (fp << 1) + GearTable[data[i]]
    if fp & maskS == 0 { return i + 1 }
}
for i := NormalSize; i < MaxSize; i++ {
    fp = (fp << 1) + GearTable[data[i]]
    if fp & maskL == 0 { return i + 1 }
}
return MaxSize
```

### 패키지 구조 — 공존

`internal/chunker/cdc.go` 가 `CDCReader` 추가, 기존 `Reader` (fixed) 는 그대로
유지. 두 reader 는 같은 `Next() (*StreamPiece, error)` 시그니처 → edge 가
공통 인터페이스로 다룸:

```go
type pieceReader interface { Next() (*chunker.StreamPiece, error) }
func (s *Server) newPutReader(src) pieceReader {
    if s.CDCEnabled { return chunker.NewCDCReader(src, s.CDCConfig) }
    return chunker.NewReader(src, s.chunkSize())
}
```

### 환경 변수

- `EDGE_CHUNK_MODE` — `fixed` (default) | `cdc`
- `EDGE_CHUNK_SIZE` — fixed 모드 chunk size (변경 없음)
- CDC 모드 파라미터는 `CDCConfig` struct 로 이후 env 추가 가능 (현재는 default
  하드코딩 — MinSize 1 MiB / NormalSize 4 MiB / MaxSize 16 MiB)

### Default tuning

```
MinSize        = 1 MiB
NormalSize     = 4 MiB     (matches fixed default)
MaxSize        = 16 MiB
MaskBitsStrict = 22        (1/2^22 hit rate per byte before NormalSize)
MaskBitsLoose  = 20        (4× easier hit after NormalSize)
```

평균 chunk ~4 MiB 로 fixed mode 와 같은 메모리 footprint, 다른 boundary.

### EC mode 비범위

Reed-Solomon encoder 는 **uniform shard sizes** 가 필요 (matrix 곱). CDC 는
variable size 를 만들어내므로 EC 와 자연스럽게 호환되지 않는다. EC + CDC 조합은
hash-based + fixed-padded 등 별도 디자인 필요 — 본 ADR 비범위.

handlePutEC 는 EDGE_CHUNK_MODE 무시하고 항상 fixed shard size.

## 결과

**긍정**:
- Shift-invariant dedup — 동일 콘텐츠 + offset 변경 시 99% chunk 재사용
- 산업 표준 (Restic 등) 와 동일 알고리즘 → 학습 가치 ↑
- 7 unit tests PASS (deterministic / shift invariance / min/max bounds /
  round-trip / empty / smaller-than-min / 1 demo)
- 기존 fixed mode 는 default 유지 — 동작 변경 0
- Same Reader.Next() 시그니처 → edge 분기 한 줄

**부정**:
- Variable chunk size → 운영자 `kvfs-cli inspect` 출력에서 chunk 크기 들쭉날쭉
- GearTable seed 가 결정적 — 다른 seed 의 kvfs 인스턴스끼리는 dedup 0 (의도됨;
  cluster 내 일관성)
- EC + CDC 조합 부재 — 큰 EC 객체에서 dedup 원하면 별도 ADR

**트레이드오프**:
- Avg chunk size 가 mask 의 통계적 평균 → 항상 4 MiB 정확 X. 분산이 큼
  (MinSize = 1 MiB ~ MaxSize = 16 MiB). 평균은 NormalSize 근처
- 1 byte shift 후 첫 1-2 chunk 만 다름. 그 후 boundary 정렬 (rolling hash 의 마법)

## 보안 주의

- GearTable seed (0xCAFEBABE) 는 결정적 → 동일 cluster 의 모든 edge 가 같은
  boundary 사용. 다중 instance 배포 시 일관성 유지 필요
- chunk_id 는 여전히 sha256 (ADR-005). CDC 는 boundary 만 결정, 해시는 그대로

## 데모 시나리오 (`scripts/demo-tau.sh`)

```bash
# 1. fixed mode cluster
EDGE_CHUNK_MODE=fixed ./scripts/up.sh
PUT F1 (16 MiB random) → 4 chunks {C1..C4}
PUT F2 (1 byte + F1)   → 4 chunks {D1..D4}
unique chunks = 8

# 2. CDC mode cluster
EDGE_CHUNK_MODE=cdc ./scripts/up.sh
PUT F1 → ~4 chunks {C1..Cn}
PUT F2 → ~4 chunks {D1..Dm}, n-1 개가 C 와 동일
unique chunks ≈ n + 1
dedup ratio ≈ (n - 1) / 2n
```

기대: fixed 대비 ~50% 디스크 절약 (1 byte shift 시나리오).

## 관련

- `internal/chunker/cdc.go` — CDCReader + GearTable + cutpoint (~180 LOC)
- `internal/chunker/cdc_test.go` — 7 tests (deterministic / shift / boundaries / round-trip / empty / short / via PUT)
- `internal/edge/edge.go` — `pieceReader` 인터페이스 + `newPutReader` 분기 + `CDCEnabled/CDCConfig` 필드
- `cmd/kvfs-edge/main.go` — `EDGE_CHUNK_MODE` env wiring
- `scripts/demo-tau.sh` — fixed vs CDC dedup 비교 라이브
- 후속 ADR 후보: streaming EC + CDC 조합, multi-window FastCDC
