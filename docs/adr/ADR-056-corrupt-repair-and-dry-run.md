# ADR-056 — Anti-entropy corrupt-repair + dry-run preview

상태: Accepted (2026-04-27)
시즌: P8-09 (ADR-055 후속)

## 배경

[ADR-055](ADR-055-anti-entropy-auto-repair.md) 가 anti-entropy/repair 를
도입했지만 **inventory missing 만** 처리. ADR-054 의 다른 detection 채널
(scrubber 의 corrupt set) 은 보고만 하고 자동 복구 안 됨. 운영자가 매
DN 의 `/chunks/scrub-status` 를 폴링 + 수동 명령 만들어야.

또 ADR-055 의 repair 는 `dry-run` 없음 — 큰 cluster 에서 audit 결과 보고
"이거 진짜 실행해도 안전?" 의 **preview** 가 없어 운영자가 risk 평가
어려움.

ADR-056 이 두 갭 모두 메움.

## 결정

### Part 1 — Corrupt-chunk repair

`POST /v1/coord/admin/anti-entropy/repair?corrupt=1`:

1. ADR-055 의 missing-detection + repair 그대로 실행
2. 추가로: 각 reachable DN 의 `/chunks/scrub-status` fetch → corrupt
   chunk_id list 수집
3. 각 corrupt chunk 에 대해 healthy source 찾아 PUT — **단, force 모드**:
   - DN 의 기존 PUT 은 idempotent (file 존재 시 200 OK + skip). 그러나
     corrupt 시나리오에서 file IS 존재 — 다만 byte 가 잘못됨. idempotent
     skip 하면 corrupt 가 영원히 남음.
   - DN 에 **`?force=1` query param** 추가 (`Coordinator.PutChunkToForce`)
     — existence skip 우회, 항상 overwrite. body 가 chunk_id 와 sha 일치
     를 이미 검증하므로 overwrite 안전.
4. corrupt source 후보 제외: scrubber 가 source DN 에서도 corrupt 라고
   보고했으면 그 source 는 skip.

ChunkRepairOutcome 에 `Reason` 필드 추가 (`"missing"` | `"corrupt"`) —
operator 가 어느 detection channel 이 trigger 했는지 식별.

### Part 2 — Dry-run preview

`POST /v1/coord/admin/anti-entropy/repair?dry_run=1`:

같은 pipeline 진행 — source 선택 등 모두 — 그러나 **ReadChunk + PutChunkTo
호출 안 함**. outcome 에 `Planned: true` 표시. byte movement 0.

운영자가 "이 audit 의 repair 가 어느 chunk 를 어디서 어디로 옮길지"
정확히 미리 본 후 실제 실행 결정.

`?corrupt=1&dry_run=1` 조합 가능 — corrupt + missing 모두 preview.

### cli flags

```
kvfs-cli anti-entropy repair --coord URL [--corrupt] [--dry-run]
```

기본값 둘 다 false (back-compat with ADR-055).

## 자가 치유 루프

```
scrubber (DN, ADR-054)  → corrupt set
                         ↓
anti-entropy/repair?corrupt=1 (coord, ADR-056)
                         ↓
  per-corrupt-chunk: read healthy → force-overwrite target
                         ↓
scrubber 다음 pass        → corrupt set empty
```

operator 가 명령어 한 번 실행 + 다음 scrub pass 확인 = bit-rot 자동 회복.

## 비채택 대안

| 대안 | 기각 사유 |
|---|---|
| DN handlePut 가 항상 sha 검증 + 자동 overwrite | 매 PUT 마다 기존 file 의 full sha256 — hot path 부담 ↑↑. force-only 가 cost localized |
| Corrupt chunk 발견 시 DN 이 자동 DELETE | 다른 replica 와 비교 안 함. 위험. detection-only 원칙 위배 |
| Scrubber 가 healthy 다른 replica 를 자동 fetch (peer-to-peer DN) | DN-to-DN 통신 도입. stateless DN 모델 깨짐. ADR-054 -항목 그대로 |
| Dry-run 별도 endpoint (`/repair-plan`) | 같은 logic — flag 가 더 단순 |
| Source 도 corrupt 일 수 있는 chunk 의 fallback 전략 | 모든 replica 가 corrupt = unrecoverable. operator 알림 + 수동. 자동 logic 없음 |

## 결과

+ **Bit-rot 자동 회복**: scrubber detection (ADR-054) → repair (ADR-055)
  → 자가 치유 loop 닫힘.
+ **Operator preview**: 큰 audit 결과의 영향 미리 보기 — confidence ↑.
+ **Force flag 가 작은 코드**: DN handlePut 에 한 줄, Coordinator 에 새
  메서드 ~25 LOC. 기존 PUT 동작 0 변경.
+ **Reason field**: outcome 분류 명확 — missing/corrupt 별 운영 metrics
  / log 가능.
+ **demo 7 stages PASS**: bit-rot inject → scrubber detect → dry-run
  preview (no byte movement 검증) → real repair → next scrub clean.
- **Source 도 corrupt 시 unrecoverable**: 모든 replica 가 corrupt 면 자동
  복구 불가. operator alert + 수동.
- **Force flag 의 위험**: 잘못된 caller 가 force=1 보내면 healthy file
  덮어쓰기. 다만 DN 이 sha256(body) == chunk_id 강제 검증하므로 잘못된
  body 는 통과 못 함 — invariant 그대로.
- **DN scrub-status 의 in-memory only**: DN restart 시 corrupt 정보 lost.
  operator 가 scrubber pass 한 바퀴 기다려야 다시 채워짐. file-backed
  state 후속 후보.
- **Concurrent repair 미포함**: 본 ADR 도 직렬 실행 — concurrent 는 P8-09
  의 다음 후보.

## 호환성

- `?corrupt=1` 미설정 시 ADR-055 동작 그대로 (back-compat).
- `?dry_run=1` 미설정 시 actual repair (back-compat).
- DN handlePut: `?force=1` 미설정 시 기존 idempotent path. 기존 caller
  영향 0.
- `Coordinator.PutChunkTo` API 무변경; 새 `PutChunkToForce` 추가.
- ChunkRepairOutcome 에 `Reason` 필드 추가 (omitempty 아님 — 새 응답마다
  포함). 기존 ADR-055 client 가 이 필드 무시 가능.

## 검증

- `scripts/demo-anti-entropy-repair-corrupt.sh` 7 stages live:
  1. clean PUT seed
  2. bit-rot on dn2 (overwrite file with garbage) → scrubber 4s 안에
     corrupt set 등록
  3. default repair → 0 work (audit 가 missing 안 봄)
  4. dry-run + corrupt → planned=1, dn2 디스크 변경 0 확인
  5. real repair + corrupt → repaired_ok=1, reason=corrupt
  6. scrubber 다음 pass → corrupt_count=0
  7. GET via edge → 정상 body
- 전체 test suite **174 PASS** (회귀 0).

## 후속

- **Concurrent repair**: 직렬 → parallel (concurrency parameter).
- **Persistent scrubber state**: DN 재시작 시 corrupt set 보존 (file-backed).
- **EC inline repair**: ADR-046 worker 통합.
- **Auto-repair on schedule**: `COORD_AUTO_REPAIR_INTERVAL` 옵션.
- **Repair throttling**: 큰 audit 의 byte movement rate-limit.
- **Notify on unrecoverable**: 모든 replica corrupt → operator alert hook.
