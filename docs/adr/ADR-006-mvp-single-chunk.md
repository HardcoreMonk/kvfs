# ADR-006 — MVP: 1 object = 1 chunk

## 상태
Superseded by [ADR-011](ADR-011-chunking.md) · 2026-04-26
(원 결정: Accepted · 2026-04-25 — Season 1 MVP 동안 유효)

## 맥락

MVP 범위 결정. chunking(객체를 여러 chunk로 분할) 구현 여부:

포함 시 필요한 것:
- 청크 경계 결정 (고정 크기 vs FastCDC·Rabin fingerprint)
- 청크 순서 메타데이터 (object → [chunk1, chunk2, ...])
- 부분 읽기 (HTTP Range) — 청크 경계 매핑
- 재구성 로직 — 여러 chunk → streaming body

MVP 목적(공개 데모)에서 chunking 가치:
- α 데모 (3-way replication)에 무관 — chunk 1개나 N개나 같은 원리
- ε 데모 (UrlKey)에 무관
- 블로그 시리즈 시즌 1 3편 분량을 넘김

기존 reference(legacy DFS) 평가 `KEEP-06` (IOD POLICY — SMALL_PIECE·MED_PIECE·LARGE_PIECE 등 워크로드 튜닝)이 풍부한 아이디어 제공 — 별도 에피소드 감.

## 결정

**MVP는 1 object = 1 chunk**. 대용량 객체 분할은 **Season 2 별도 ADR**.

- 권장 객체 크기 ≤ 64MB
- 더 큰 업로드 시 HTTP body 전체를 메모리에 로드 후 PUT → 실용 상한은 서버 메모리·타임아웃에 의존
- chunk_id = sha256(전체 body)
- 객체 메타데이터 단순화: `chunks: [chunk_id]` 형태지만 길이 1 고정

## 결과

**긍정**:
- 구현 단순 — `internal/edge/edge.go` handlePut 이 한 번의 PUT + 한 번의 fanout으로 완결
- `internal/coordinator/coordinator.go` 가 chunk 배열 처리 없음 → `WriteChunk` 60줄로 수렴
- 독자가 "한 객체는 한 청크" 멘탈로 따라오기 쉬움
- Content-addressable dedup(ADR-005)이 "동일 내용 객체 전체가 dedup됨" 직관적 의미

**부정**:
- **64MB+ 객체 실패 가능** — 메모리 부담, HTTP 타임아웃. 데모 범위에선 수용
- 부분 read (HTTP Range) 불가 — 전체 body 전송
- 대용량 전송 중 장애 시 재시도 비용 = 전체 객체

**트레이드오프**:
- 미래 chunking 도입 시 `chunks: []` 배열로 자연 확장 — 메타 스키마 forward-compat
- 블로그 Season 2 Ep.1: "1 object = 1 chunk 를 깨부숴보자 — FastCDC 도입기" 가 자연 후속

## Season 2 ADR 예상

- ADR-011 (예상) — chunking 도입: 고정 크기 4MB piece vs FastCDC
- ADR-008 (예상) — Reed-Solomon erasure coding (chunk 3-way replica를 6+3 EC로 대체)

## 관련

- `internal/edge/edge.go` — `ObjectMeta.Chunks` 필드는 MVP에서 길이 1 배열 또는 단일 field
- `internal/store/store.go` — ObjectMeta 정의
