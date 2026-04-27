# ADR-055 — Anti-entropy auto-repair + scheduled audit

상태: Accepted (2026-04-27)
시즌: P8-08 (post-S7 follow-ups)

## 배경

[ADR-054](ADR-054-anti-entropy-merkle.md) 가 detection 만 — 운영자가 수동
trigger + 결과 보고 수동 해석. real-world 운영자는 다음 둘 모두 원함:

1. 발견된 missing chunk 가 자동으로 복구되길 — operator 가 매 audit 결과를
   분석 + 수동 명령 만드는 toil 0.
2. Audit 자체가 주기 실행 — cron 또는 k8s CronJob 안 깔아도 cluster 가
   self-monitor.

ADR-054 의 -항목 두 개를 모두 매핑.

## 결정

### Part 1 — Auto-repair endpoint

`POST /v1/coord/admin/anti-entropy/repair` — leader-only, COORD_DN_IO 필수.

흐름:
1. 내부적으로 `runAntiEntropy()` 호출 → 현재 audit report
2. ObjectMeta 한 번 walk → `chunkOwners[chunk_id] → []dn` (어느 DN 들이
   이 chunk 의 owner 인지) + `ecChunks` set (스킵 대상 식별용)
3. audit 의 reachable DN 별 `MissingFromDN` 순회:
   - chunk 가 EC 면 skip + "ec-deferred" 메시지 (ADR-046 worker 위임)
   - 그 외: chunkOwners 에서 healthy source 찾기 (target 아니고, 자신이
     missing 안 보고한 DN)
   - source 에서 ReadChunk → target 에 PutChunkTo → outcome 기록
4. 결과: per-(target, chunk_id) outcome list.

```json
{
  "audit": {...},                                  // 원본 ADR-054 audit
  "repairs": [
    {
      "target_dn": "dn1:8080",
      "chunk_id": "e8058c7c...",
      "source_dn": "dn2:8080",
      "mode": "replication",
      "ok": true
    }
  ],
  "skipped": [
    {"target_dn":"dn1","chunk_id":"...","mode":"ec-deferred",
     "err":"EC chunk — use kvfs-cli repair --apply --coord (ADR-046)"}
  ],
  "duration": "3.8ms"
}
```

cli `kvfs-cli anti-entropy repair --coord URL`.

### Part 2 — Scheduled audit (no auto-repair)

`COORD_ANTI_ENTROPY_INTERVAL=10m` env. coord daemon 이 leader 일 때 ticker
로 주기 audit 실행, 결과를 slog 로 로그 (divergent_dns + total_missing
+ duration). **Auto-repair 는 ticker 가 자동 트리거 안 함** — operator
명시적 `/repair` 호출 필요.

```
log: scheduled anti-entropy divergent_dns=1 total_missing=3 duration=15.2ms
```

이게 의도적 분리. 자동 repair 가 ticker 의 부수효과면 운영자가 일시적
divergence (예: DN restart 중) 동안 의도치 않은 byte movement 가 발생.
detection 은 안전, repair 는 의식적.

## 분리 — Detection vs Repair vs Schedule

3 layer:

| 책임 | 위치 | 트리거 |
|---|---|---|
| Detection (audit) | ADR-054 `/run` | 명시적 호출 또는 ticker (ADR-055 P2) |
| Auto-repair | ADR-055 `/repair` | 명시적 호출만 (ticker 미연결) |
| Schedule | ADR-055 P2 ticker | env opt-in |

각 layer 가 독립 — operator 가 자기 운영 모델에 맞춰 조합.

## EC 스킵의 정당화

EC stripe 의 missing shard 복구는:
1. K survivors 에서 RS Reconstruct (CPU 작업)
2. 누락 shard 를 새 위치 PlaceN 로 결정
3. 거기 PUT

이미 ADR-046 (S6 Ep.4) 가 이 흐름 구현. ADR-055 가 같은 코드 inline 호출
하면 logic 복제. 또는 worker 로 위임하면 ticker 분리. 단순함 위해 본 ADR
은 **skip + 명시적 메시지** — operator 가 `kvfs-cli repair --apply --coord`
실행. 한 cli 명령으로 EC 끝.

미래 ADR 후보: anti-entropy 가 EC repair worker 를 inline 호출 — 그러나
그 시점에 이미 워커가 background loop 에서 동일 chunk 처리 중일 가능성
→ duplicate work 위험. 지금은 명시적 분리.

## 비채택 대안

| 대안 | 기각 사유 |
|---|---|
| Detection + repair 를 한 endpoint 에 묶기 | "audit 보고 복구 결정" 의 step 분리 잃음. 운영자가 "audit 만 보고싶다" 가 흔한 use case |
| Ticker 가 audit + repair 둘 다 자동 | DN 일시 outage 동안 의도치 않은 byte movement. operator surprise |
| Repair 가 corrupt set (scrubber) 도 자동 처리 | scrubber 의 corrupt set 은 in-memory only — 별도 endpoint 로 fetch 필요. 그쪽도 구현하려면 ADR-054 의 scrubber state model 확장. 본 ADR 은 missing 만 — "scrubber-detected corrupt" 자동 repair 는 follow-up |
| 모든 auto-repair 를 ticker 에 자동 | operator agency 박탈. 비상 상황에서 auto-repair 가 더 망치는 시나리오 (e.g. 잘못된 placement 후 repair 가 잘못된 곳으로 byte 이동) |

## 결과

+ **Closes the detection→action loop**: anti-entropy 의 가치 실현. operator
  trigger 한 번에 cluster 자가 치유.
+ **Replication chunks 즉시 복구**: source 에서 read + target 으로 PUT.
  ADR-044 의 `Coord.ReadChunk/PutChunkTo` 인프라 그대로 재사용.
+ **Background scheduled audit**: cron 안 깔아도 cluster 가 자기 health
  check.
+ **EC 와 분리**: ADR-046 worker 와 책임 명확. 운영자 혼동 없음.
+ **Demo PASS** end-to-end: rm chunk → audit detect → repair 1 chunk →
  audit clean → GET 정상. 실제 self-heal 입증.
- **Source 선택의 결정성**: 현재 코드는 chunkOwners 의 첫 healthy candidate
  선택 (deterministic by ObjectMeta order + audit map iteration). load
  balancing 안 함. 큰 cluster 의 hot source DN 부담 — measurement 후
  refinement.
- **Concurrent repair 미지원**: 모든 repair 직렬 실행. 큰 missing set 시
  느림. concurrency parameter 추가 후속 후보.
- **EC chunks 미처리**: skipped 만 보고. operator 가 별도 명령 실행 필요.
- **Scrubber-corrupt 미연동**: `/scrub-status` 의 corrupt set 은 audit 의
  "missing_from_dn" 에 안 들어옴 (디스크엔 file 있음, 다만 sha mismatch).
  scrubber-detected corrupt 의 자동 repair 는 P8-09 후보.

## 호환성

- 새 endpoint 1개 (`/anti-entropy/repair`) — 기존 caller 영향 0.
- 새 env `COORD_ANTI_ENTROPY_INTERVAL` — 미설정 시 ticker off, 기존 동작
  변경 없음.
- cli 새 subcommand `anti-entropy repair` — 기존 영향 없음.
- ObjectMeta / WAL / 메타 schema 변경 0.

## 검증

`scripts/demo-anti-entropy-repair.sh` 7 stages live:
1. clean cluster + 3 obj seed
2. /run → all root_match=true
3. rm chunk on dn1
4. /run → dn1 missing=1
5. /repair → repaired_ok=1, copy from dn2
6. /run → all root_match=true again
7. GET via edge → intact body

전체 test suite **174 PASS** (회귀 0).

## 후속

- **Scrubber-corrupt 자동 repair**: ADR-054 의 scrubber 가 corrupt 표시한
  chunk 도 anti-entropy/repair 가 처리. 현재는 missing 만.
- **EC inline repair**: ADR-046 worker 를 endpoint 가 호출 — 통합 안
  되면 cli 두 단계.
- **Concurrent repair**: 큰 audit 결과의 repair 직렬 → parallel
  (concurrency parameter).
- **Repair throttling / dry-run**: operator 가 큰 변경 영향 미리 보기 +
  rate-limit.
- **Auto-repair on schedule**: 옵션 env (`COORD_AUTO_REPAIR_INTERVAL`) —
  operator 가 명시적 risk acceptance 후 ticker 에 repair 도 연결.
