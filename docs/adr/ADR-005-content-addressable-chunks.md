# ADR-005 — Content-addressable sha256 chunks

## 상태
Accepted · 2026-04-25

## 맥락

chunk ID 체계 선택. 대안:

| 방식 | 장점 | 단점 |
|---|---|---|
| **sha256(body)** | 자동 dedup, 무료 무결성 검증, 결정적 | 업로드 시 해시 계산 비용 (작은 객체엔 무시할 수준) |
| UUID v7 | 시간순 정렬 쉬움 | dedup 불가, integrity 별도 |
| 중앙 시퀀스 ID | 단순 | 중앙 allocator 필요, dedup 불가 |

산업 레퍼런스: git (blob), IPFS, Restic, Borg — 전부 content-addressable.

## 결정

**`chunk_id = hex(sha256(body))`**, 64자 hex.

- edge가 PUT 시 body 해시 → chunk_id 생성
- dn에서 `PUT /chunk/{id}` 수신 시 서버가 **`sha256(body)` 재검증** (id 위조 방지)
- 디스크 레이아웃: `<data_dir>/chunks/<id[0:2]>/<id[2:]>` — 2자 샤딩
- **MVP는 1 object = 1 chunk** (ADR-006). 대용량 chunking은 Season 2

## 결과

**긍정**:
- **무료 dedup**: 동일 내용 N번 업로드해도 디스크 1개만. 데모에서 실측 확인 (4 objects → 3 unique chunks)
- **무료 integrity**: read 경로에서 `sha256(body) == chunk_id` 재검증. bit flip 탐지
- 2자 샤딩으로 10만+ chunks 성능 확보 (ext4/xfs readdir)
- git·IPFS 멘탈 모델로 독자 학습 곡선 없음

**부정**:
- **append/수정 불가** — 객체 편집 = 새 chunk 생성. S3·git 동일 한계
- 업로드마다 sha256 연산 — 작은 객체(~MB)는 무시, 대용량(GB)은 streaming 해시 필요 (Season 2 chunking과 함께 해결)
- chunk 가 전 세계 공유되므로 삭제 시 **reference counting** 필요. MVP는 각 object가 독립 chunk 참조로 단순화 (중복 삭제는 멱등)

**트레이드오프**:
- 영구 immutable 특성 = 삭제 정확성의 난도 증가 → Season 2에서 reference counting 필요 시 ADR 추가

## 관련

- `internal/dn/dn.go` — `validChunkID`, `handlePut` 재검증 로직
- `internal/edge/edge.go` — upload 시 chunk_id 계산, read 시 재검증
