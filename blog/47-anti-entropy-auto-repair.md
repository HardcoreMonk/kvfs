# Episode 47 — anti-entropy auto-repair: detection → action 의 loop 닫기

> **Series**: kvfs — distributed object storage from scratch
> **Wave**: P8 post-S7 follow-ups · **Episode**: 1
> **연결**: [ADR-055](../docs/adr/ADR-055-anti-entropy-auto-repair.md) · [demo-anti-entropy-repair](../scripts/demo-anti-entropy-repair.sh)

---

## ADR-054 의 보고서

[Ep.46 anti-entropy](46-anti-entropy-merkle.md) 가 만든 detection layer:

```json
{
  "dns": [
    {"dn":"dn1:8080","root_match":false,"missing_from_dn":["abc..."]}
  ]
}
```

Detection only — operator 가 받아서 직접 처리. 어떻게? 운영자 입장에서는:

```
1. report 받기
2. missing chunk_id 의 다른 replica 확인
3. 그 replica 에서 chunk 다운로드
4. 영향 받은 DN 에 업로드
5. 다시 audit 해서 fixed 확인
```

이걸 매 audit 마다 사람이 수동으로? toil 의 정의.

## 결정 — `/anti-entropy/repair`

`POST /v1/coord/admin/anti-entropy/repair` (leader-only, COORD_DN_IO=1):

```
1. runAntiEntropy() 호출 → audit
2. ObjectMeta walk → chunkOwners[chunk_id] = [dn list]
3. audit 의 missing_from_dn 순회:
   - EC chunk 면 skip + "ec-deferred" 메시지 (ADR-046 worker)
   - 그 외: chunkOwners 에서 healthy source 찾기
   - source.ReadChunk → target.PutChunkTo → outcome 기록
4. 응답: repairs[] + skipped[]
```

`source` 선택 = chunk owner 중 audit 가 "missing 안 보고" 한 첫 DN. ADR-044
의 `Coord.ReadChunk` + `Coord.PutChunkTo` 인프라 그대로 재사용 — 새 메커니즘 0.

## 데모 — full self-heal loop

```
$ ./scripts/demo-anti-entropy-repair.sh

==> stage 1: PUT 3 objects → 3 chunks per DN
==> stage 2: clean anti-entropy run
    [{"dn":"dn1","root_match":true},{"dn":"dn2","root_match":true},{"dn":"dn3","root_match":true}]
==> stage 3: rm one chunk file from dn1 (simulate disk loss)
    deleted e8058c7c5debb22e..
==> stage 4: anti-entropy detects
    [{"dn":"dn1","missing":1},{"dn":"dn2","missing":0},{"dn":"dn3","missing":0}]
==> stage 5: auto-repair
    {"duration":"3.8ms","repaired_ok":1,
     "example":{"target_dn":"dn1","chunk_id":"e8058c7c..","source_dn":"dn2",
                "mode":"replication","ok":true}}
==> stage 6: anti-entropy clean again
    [all root_match=true]
==> stage 7: GET via edge → intact body
```

7 stages. 4ms 의 repair 가 missing 을 closes. 다시 audit → clean. **Detection
→ action → verification 의 loop 닫힘**.

## EC 의 의도적 skip

EC stripe 의 missing shard 복구는 다른 mechanism:
- K survivors 에서 RS Reconstruct
- 누락 shard 의 새 위치 PlaceN 결정
- PUT

이미 ADR-046 (S6 Ep.4) 가 한다. ADR-055 가 같은 logic inline 하면 코드
복제. 결정: **skip + 명시적 메시지** — operator 가 `kvfs-cli repair
--apply --coord` 별도 실행.

```json
{
  "skipped": [
    {"target_dn":"dn1:8080","chunk_id":"...","mode":"ec-deferred",
     "err":"EC chunk — use kvfs-cli repair --apply --coord (ADR-046)"}
  ]
}
```

명료한 worker 책임 분리. 미래에 anti-entropy/repair 가 EC 도 자동
trigger 하게 통합 가능 — 그땐 이중 worker 충돌 처리 (concurrency control)
가 더 큰 결정이라 별도 ADR.

## Scheduled audit (no auto-repair)

`COORD_ANTI_ENTROPY_INTERVAL=10m` env. ticker 가 leader 에서 주기 audit
실행:

```
log: scheduled anti-entropy divergent_dns=1 total_missing=3 duration=15.2ms
```

**Auto-repair 는 ticker 가 자동 트리거 안 함**. detection 은 안전, repair
는 의식적 — 일시적 DN outage 중 의도치 않은 byte movement 회피.

3 layer 분리 명확:
- detection (ADR-054 `/run` + ticker)
- repair (ADR-055 `/repair`, 명시적 호출)
- schedule (ADR-055 ticker — detection 만 자동)

operator 가 자기 운영 모델에 맞게 조합.

## "왜 ticker 가 repair 까지 안 하나"

가능한 안전성 반례:
- DN restart 중 (10s downtime). audit 가 그 동안 실행. missing 보고 →
  repair 가 즉시 byte 복사 시작. 그 직후 DN restart 끝나면 dual-presence.
- 잘못된 placement 가 ObjectMeta 에 들어감 (예: DN registry 누락 후 PUT).
  audit 가 missing 보고 → repair 가 잘못된 위치에 복사.

이런 시나리오에서 ticker auto-repair 는 잘못된 일을 자동화. operator 명시적
호출이 필요한 이유. 운영 신뢰도가 쌓이면 옵션 env (`COORD_AUTO_REPAIR_INTERVAL`)
로 사용자 직접 risk acceptance — 후속 ADR.

## 부수 fix — ADR-005 의 dividend, 또

repair 가 원본 chunk 의 sha256 검증 자동? 안 함 — `Coord.ReadChunk` 가
이미 chunk_id 일치 확인 (`source` 가 잘못된 byte 보내면 read 자체가
failure). content-addressable 이 모든 layer 에서 invariant 보장.

## 정량

| | |
|---|---|
| 코드 | `Server.runAntiEntropyRepair` (~80 LOC) + `copyChunk` + ticker |
| 새 endpoint | 1 (`/anti-entropy/repair`) |
| 새 env | 1 (`COORD_ANTI_ENTROPY_INTERVAL`) |
| cli | `anti-entropy repair --coord URL` |
| 데모 | 7 stages, full self-heal loop |
| 테스트 | 회귀 0 (174 PASS 유지) |

## 다음 후보

ADR-055 의 -항목들 = 후속 wave 후보:
- Scrubber-detected corrupt 의 자동 repair (현재는 missing 만)
- EC inline repair (ADR-046 worker 통합 + concurrency control)
- Concurrent repair (parallel)
- Auto-repair on schedule (옵션 env)
- Repair throttling / dry-run

각 별도 ADR. 본 ADR 의 단순함을 보존.

## frame 1+2 = 100% 이후의 첫 episode

P8 wave 의 원래 목표 (frame 1+2 100%) 는 [Ep.46](46-anti-entropy-merkle.md)
에서 도달. Ep.47 부터는 그 위의 polish — frame 자체를 격상시키지 않고
이미 있는 features 의 usability 향상. ADR-054 의 detection 이 ADR-055 의
auto-repair 를 만남.

다음 글이 또 polish 일지, 새 frame 진입일지는 운영자 결정의 영역.
