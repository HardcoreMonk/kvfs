# ADR-011 — Chunking (큰 객체를 작은 청크로 쪼개기)

## 상태
Accepted · 2026-04-26 · Season 2 Ep.4 첫 결정 · **ADR-006 supersede**

## 맥락

ADR-006 은 MVP 단순함을 위해 "1 object = 1 chunk" 로 결정했다. 한 객체 전체가 하나의 sha256 으로 식별되고, 단일 chunk 가 R 개 DN 에 복제된다. 이 결정이 가능했던 이유:

- MVP 시연용 객체는 모두 작음 (수십 바이트 ~ 수 KiB)
- placement·replication·dedup·rebalance·GC 의 핵심 알고리즘을 청크 단위 추가 복잡도 없이 보여줄 수 있음

그러나 한 객체가 100 MiB, 1 GiB 가 되면:

1. **메모리 폭발** — edge 가 PUT body 전체를 RAM 에 로드, sha256 계산. GET 도 동일
2. **단일 DN 부하** — 1 GiB 객체의 모든 트래픽이 R 개 DN 에만 집중. 클러스터 다른 노드의 디스크·NIC 유휴
3. **dedup 무효화** — 두 객체가 99% 일치하고 마지막 1% 다르면 sha256 이 완전히 달라져 **dedup 효과 0**. 청크 단위라면 99% 청크 공유 가능
4. **부분 복구 불가** — DN 1개가 다운되는 동안 1 GiB 청크 1개를 통째로 다시 복사해야 함. 청크 단위라면 손상된 청크만 복구
5. **재배치 비용** — rebalance/gc 가 청크 단위로 동작하지만 "청크 = 객체" 라 큰 객체가 통째로 이사

ADR-009 (placement), ADR-010 (rebalance), ADR-012 (gc) 모두 이미 chunk 단위 알고리즘으로 짜여 있다 — 단지 청크 수가 1 일 뿐. **chunking 은 상위 알고리즘에 추가 변경 없이 N 청크를 활성화**한다.

### 대안 검토

| 방식 | 장점 | 단점 |
|---|---|---|
| **고정 크기 청크 (이번 ADR)** | 단순, 결정적, dedup 친화 (정렬된 데이터에 강함) | 비정렬 데이터 (파일 내부 변경) 의 dedup 효율 낮음 |
| Content-defined chunking (CDC, e.g. rabin/buzhash) | 비정렬 변경에도 dedup 효과 유지 | rolling hash 구현 복잡, 평균 chunk 크기 보장 어려움 |
| Erasure coding (K+M) | 디스크 사용 효율 (R-way 의 절반) | placement·repair·읽기 모두 복잡도 폭증 → ADR-008 별도 |
| 단일 chunk 유지 + 스트리밍 PUT | RAM 폭발 해결 | dedup·재배치 효율 문제 그대로 |
| 외부 chunker (e.g. IPFS/Restic 라이브러리) | 검증된 코드 | 외부 의존 +1, 투명성 ↓ |

kvfs 의 교육적 목표상 **고정 크기가 압도적**. CDC 는 흥미롭지만 구현이 ADR 한 페이지를 넘어선다. CDC 채택은 별도 ADR 후보.

## 결정

### 핵심 모델

PUT body 를 **고정 크기 N 바이트** 단위로 자른다. 마지막 청크는 잔여 크기 (N 이하).

```
body 크기 250 KiB, chunk_size 64 KiB
  → chunks: [64KiB, 64KiB, 64KiB, 58KiB]
  → chunk_id 4개: 각각 sha256(chunk_data)
```

각 chunk_id 는 **독립적으로 placement.Pick 통과** → R 개 DN 에 fanout. 같은 객체의 청크들이 **서로 다른 DN 집합** 에 흩뿌려질 가능성 높음 (load balancing).

### ObjectMeta 스키마 (breaking change)

이전 (ADR-006):
```go
type ObjectMeta struct {
    ChunkID  string
    Replicas []string
    Size     int64
    ...
}
```

이후 (ADR-011):
```go
type ObjectMeta struct {
    Chunks []ChunkRef  // 순서 보존 (concat 시 사용)
    Size   int64       // 전체 객체 크기 = sum(Chunks[*].Size)
    ...
}

type ChunkRef struct {
    ChunkID  string   // hex sha256 of this chunk's bytes
    Size     int64    // 이 청크의 바이트 수
    Replicas []string // 이 청크를 ack 한 DN 주소들
}
```

**호환성**: bbolt 메타 파일을 in-place migration 하지 않는다. 기존 클러스터는 `down.sh && up.sh` 로 리셋. 데모 환경이라 이 트레이드오프 수용.

### 설정

```
EDGE_CHUNK_SIZE   bytes, default 4194304 (4 MiB)
```

운영 권장:
- 4 MiB ~ 64 MiB: 일반 객체 스토리지
- 작은 값(1 KiB ~ 64 KiB): 데모·테스트
- 너무 작으면 메타 폭증 (1 GiB 객체에 16k 청크), 너무 크면 dedup·load balancing 효과 ↓

### 처리 순서

**PUT**:
1. body 전체 read (MVP — 스트리밍은 후속 ADR)
2. `chunker.Split(body, chunkSize)` → []Chunk{Data, ChunkID}
3. 각 chunk 에 대해 `coord.WriteChunk(chunk_id, data)` (병렬 fanout + quorum)
4. 모든 chunk 가 quorum 통과 → `ObjectMeta.Chunks` 구성, `Store.PutObject`
5. 어느 chunk 라도 quorum 실패 → 전체 PUT 실패 (이미 쓰인 청크는 GC 가 청소)

**GET**:
1. `Store.GetObject` → `meta.Chunks`
2. 각 chunk 에 대해 `coord.ReadChunk(chunk_id, replicas)` 순차 (또는 병렬 후 정렬)
3. 각 chunk 의 sha256 검증
4. 청크들을 `Chunks` 순서대로 concat → response body
5. 전체 size 검증

**DELETE**:
- 모든 chunk 에 대해 `coord.DeleteChunk(chunk_id, replicas)` best-effort
- 메타 삭제

### Rebalance / GC 영향

- Rebalance: `Migration` 에 `ChunkIndex` 추가. `ComputePlan` 이 `obj.Chunks` 를 iterate. `migrateOne` 이 `Store.UpdateChunkReplicas(bucket, key, chunkIdx, replicas)` 호출
- GC: `claimed-set` 이 모든 청크를 union. 알고리즘 변경 없음

두 알고리즘 모두 **이미 청크 단위 사고 모델** 이라 변경 최소.

## 결과

### 긍정

- **메모리 사용 상한** — chunk_size 가 RAM 사용 cap (현재 MVP 는 여전히 body 전체 read 하지만, 미래에 streaming 추가 시 chunk_size 만 in-flight)
- **클러스터 부하 분산** — N 청크가 N 개 DN 집합으로 분산 가능
- **Block-level dedup** — 정렬된 데이터 (예: tar 파일 일부 변경) 에서 변경되지 않은 청크는 그대로 공유
- **부분 복구** — 손상된 청크 1개만 다시 복사 (전체 객체 X)
- **알고리즘 무변경** — placement/rebalance/gc 가 chunk_id 단위로 이미 동작 중
- **테스트 커버 확장** — 청크 1개 (작은 객체) ~ N개 (큰 객체) 모두 동일 코드 경로

### 부정

- **메타 크기 증가** — 1 GiB 객체 + 4 MiB chunk = 256 청크 메타 ≈ ~30 KB JSON. bbolt 가 처리 가능하지만 객체 수 100만 개 이상에선 의미 있음
- **읽기 latency** — N 청크 순차 읽기는 N × roundtrip. 병렬 읽기는 메모리·복잡도 ↑. MVP 는 순차 (가독성 우선)
- **부분 PUT 실패 처리** — 5 청크 중 3 청크 quorum 통과 후 4번째 실패: 전체 PUT 실패하고 1~3 청크는 orphan → GC 가 청소 (min-age 통과 후). 일시적 디스크 사용
- **breaking schema 변경** — 기존 edge-data 와 비호환. 운영자가 reset 필요

### 트레이드오프 인정

- "왜 streaming PUT 안 함?" — chunker 가 io.Reader 받게 만들면 됨. MVP 는 명료성 우선, 후속 ADR 가능
- "왜 CDC 안 함?" — rabin/buzhash 가 흥미롭지만 ADR 한 페이지 넘어섬. 별도 ADR
- "왜 EC 안 같이 함?" — EC (ADR-008) 는 chunk 단위 위에서 동작 (chunking + EC). 별도 ADR

## 데모 시나리오 (`scripts/demo-iota.sh`)

```
1. EDGE_CHUNK_SIZE=65536 (64 KiB) 로 클러스터 기동
2. 256 KiB 랜덤 객체 PUT → 4 청크 생성
3. PUT 응답에 chunk 4개 + 각 replicas 표시
4. GET → body == 원본 (sha256 일치)
5. kvfs-cli inspect 로 메타 확인 — Chunks 배열 4 entries
6. 청크별 replicas 가 다른 DN 집합인지 확인 (load balancing)
7. dn1 kill → GET 여전히 성공 (quorum)
8. rebalance/gc 평소처럼 동작 (이미 chunk 단위)
```

## 관련

- ADR-006 (1 object = 1 chunk) — **superseded by this ADR**
- ADR-005 (content-addressable) — chunk_id 가 여전히 sha256, 청크 단위로 작동
- ADR-009 (placement) — chunk_id 별 독립 Pick (변경 없음)
- ADR-010 (rebalance) — `Migration` 에 `ChunkIndex` 추가, 알고리즘 동일
- ADR-012 (gc) — claimed-set 이 모든 청크 union (변경 최소)
- `internal/chunker/` — Split/Join 구현 (후속)
- `scripts/demo-iota.sh` — 시연 (후속)
- `blog/05-chunking.md` — narrative episode (후속)

## 후속 ADR 예상

- **ADR-013 — Auto-trigger policy**: rebalance/GC 자동화 (별도)
- **ADR-008 — Reed-Solomon EC**: chunk 단위 위에서 동작
- **ADR-017 — Streaming PUT/GET**: io.Reader 기반 chunker
- **ADR-018 — Content-defined chunking**: 비정렬 데이터 dedup 효율
