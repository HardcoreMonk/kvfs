# ADR-057 — Anti-entropy EC inline repair

상태: Accepted (2026-04-27)
시즌: P8-10 (ADR-055/056 후속)

## 배경

[ADR-055](ADR-055-anti-entropy-auto-repair.md) 가 anti-entropy/repair 를
도입했지만 EC chunks 는 `"ec-deferred"` 로 skip — operator 가 별도 cli
명령 (`kvfs-cli repair --apply --coord`) 으로 ADR-046 worker 실행 필요.

운영 friction:
- 한 cluster 의 self-heal 이 두 개 명령 (anti-entropy + repair)
- ADR-046 의 ComputePlan 은 "DN 자체가 cluster 에서 사라진" 시나리오 (registry
  에서 빠진 DN의 chunks) 만 detect — anti-entropy 가 발견한 "DN alive 인데
  chunk file missing" 시나리오는 안 잡음

ADR-057 이 두 문제를 한 번에 해결.

## 결정

`POST /v1/coord/admin/anti-entropy/repair?ec=1` 신규 flag:

```
ec=1 미설정 (default) → EC chunks 'ec-deferred' (ADR-055 back-compat)
ec=1 설정             → EC chunks inline reconstruct + force-write
```

흐름:

1. ADR-054 audit 가 per-DN missing chunk_id 식별 (replication + EC 모두)
2. ADR-055/056 의 replication repair 그대로 실행
3. **ADR-057 신규**: ObjectMeta walk → 영향 받은 EC stripe 단위로
   `repair.StripeRepair` 빌드:
   - **Survivors**: shard 가 audit 의 missing 에 없는 항목 (anti-entropy
     가 healthy 라고 판정)
   - **DeadShards**: missing 항목. NewAddr = OldAddr (원위치 복원)
4. 빌드된 `repair.Plan` 을 기존 `repair.Run` 에 위임 → RS Reconstruct +
   PUT
5. per-shard outcome `mode: "ec-inline"` + 종합 `mode: "ec-summary"` 로
   응답

## ADR-046 과의 관계

| | ADR-046 (existing) | ADR-057 (this) |
|---|---|---|
| trigger | `kvfs-cli repair --plan/--apply --coord` | `anti-entropy/repair?ec=1` |
| dead 판정 | shard.Replicas[0] not in coord.DNs() | anti-entropy audit 의 MissingFromDN |
| use case | DN registry 에서 떨어진 chunks | DN alive 인데 file 사라짐 |
| reconstruct + write | `repair.Run` 의 repairStripe | 동일 (`repair.Run` 그대로 호출) |

두 trigger 가 같은 worker 를 다른 입력으로 호출. 코드 중복 0.

## DryRun 동작

`?ec=1&dry_run=1` 조합:
- ec-inline outcomes 가 `Planned: true` 표시
- `repair.Run` 호출 **안 함** — actual reconstruct + PUT 0
- ec-summary 도 생성 안 함 (run 없으므로 stats 없음)

operator 가 EC 영향 미리 본 후 실제 실행 결정.

## Force overwrite

ADR-046 의 `repairStripe` 는 `coord.PutChunkTo` 사용 — idempotent
(file 존재 시 skip). 우리의 anti-entropy 시나리오에서:
- File missing → file 없음 → PUT 정상 작성 ✓
- File corrupt → ADR-057 본 ADR 의 scope 아님 (ADR-056 의 corrupt-repair 가
  replication 만 처리; EC corrupt 는 별도 후속)

본 ADR 은 EC **missing only**. EC corrupt 는 P8-11 후보.

## 비채택 대안

| 대안 | 기각 사유 |
|---|---|
| anti-entropy 가 ADR-046 ComputePlan 을 그대로 호출 | dead 판정 다름 — DN registry 에서 빠진 case 만 catch. anti-entropy 의 file-missing case 안 잡힘 |
| anti-entropy 가 ec=1 시 무조건 ADR-046 ComputePlan + Run 도 추가로 실행 | 두 worker 가 같은 stripe 처리 시도 → 중복. 게다가 ADR-046 가 "DN alive 인데 chunk missing" 안 잡으니 anti-entropy 작업도 안 됨 |
| repair package 에 새 ComputePlanFromAudit 추가 | 본 ADR 의 anti_entropy.go 안에 inline 빌드 — repair package 의 책임 일관성 (registry-driven) 보존, anti-entropy 의 audit-driven 입력은 자기가 변환 |
| ec=1 default on | back-compat 깨짐 — 기존 ADR-055 client 가 갑자기 EC 자동 repair 받음. opt-in 가 안전 |
| ec=1 시 corrupt 도 자동 (ADR-056 + ADR-057 합치기) | 두 mode 의 trade-off 다름. corrupt 는 force overwrite 필요, EC missing 은 PutChunkTo 일반. 분리 유지 |

## 결과

+ **Single command self-heal**: 한 명령이 replication + EC 모두 처리.
  운영 단순화.
+ **ADR-046 worker 재사용**: `repair.Run` 그대로 호출. 새 reconstruct
  코드 0 LOC.
+ **anti-entropy 와 ADR-046 의 입력 차이 해결**: file-missing 시나리오를
  ADR-046 worker 가 인지 가능하도록 anti-entropy 가 변환 + 위임.
+ **back-compat**: ec=1 미설정 시 ADR-055 동작 그대로.
+ **demo 6 stages PASS**: 1MB EC (4+2) PUT → shard rm → audit detect
  → ec-deferred (default) → ec=1 inline reconstruct → audit clean → GET sha256 정상.
- **EC corrupt 미포함**: 본 ADR 은 missing only. corrupt EC 는 후속.
- **per-shard outcome 의 success 정확도**: 현재 stats 의 errors 가 비면
  모든 ec-inline outcome OK 로 mark, 있으면 ambiguous. stripe 단위 stats
  → shard 단위 outcome 매핑이 brittle. operator 가 audit 결과 + ec-summary
  를 같이 봐야 정확. 후속 refinement 후보.
- **ec-summary 의 outcome shape**: TargetDN="(ec-summary)" 가 sentinel
  값. 운영자 / 도구 입장에서는 shape 변경 — 새 클라이언트가 적응 필요.
- **DryRun 시 ec-summary 부재**: 실제 run 안 했으니 stats 없음. ec-inline
  Planned outcomes 만 있음. 일관성을 위해 placeholder summary 도 가능
  하나 정직한 절제 선택.

## 호환성

- `?ec=1` 미설정 시 동작 0 변경 (back-compat with ADR-055/056).
- `repair.Run` API 무변경 — 새 caller 추가만.
- ChunkRepairOutcome 의 `mode` 새 값 두 개: `"ec-inline"`, `"ec-summary"`.
  기존 client 가 새 값 무시 가능.
- cli `--ec` flag 추가 — 미설정 시 기존 동작.

## 검증

- `scripts/demo-anti-entropy-repair-ec.sh` 6 stages live:
  1. PUT 1MB EC (4+2)
  2. rm one shard file from one of the placement DN
  3. default repair → ec-deferred 1 (back-compat)
  4. repair?ec=1 → ec-inline 1 OK + ec-summary OK
  5. audit run → all root_match=true
  6. GET via edge → sha256 intact
- 전체 test suite **174 PASS** (회귀 0).

## 후속

- **EC corrupt 자동 repair**: ADR-056 의 corrupt 채널을 EC 에 확장. force
  overwrite + RS reconstruct.
- **per-shard 정확한 success/failure**: stats 매핑 정밀화.
- **EC stripe 의 batch size limit**: 큰 audit 에서 모든 affected stripe 한
  번에 처리 — concurrency control 후속.
- **Auto-repair on schedule** + EC: scheduled audit 이 ec=1 도 자동
  trigger 옵션.
