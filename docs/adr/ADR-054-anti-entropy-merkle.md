# ADR-054 — Anti-entropy / Merkle tree (silent corruption + inventory drift)

상태: Accepted (2026-04-27)
시즌: Season 7 Ep.4 — frame 2 (textbook primitives) 마감

## 배경

지금까지 kvfs 의 무결성 보장은 **읽기 시점** 에 묶여있었다:

- 매 GET 마다 DN 응답의 sha256 검증 (chunk_id 일치 — Ep.1 의 content-addressable
  결정 덕분)
- ADR-040 transactional commit + ADR-039 WAL replication 으로 메타 일관성 강화

그러나 **읽혀지지 않는 chunk** 의 무결성은? 디스크에 있지만 GET 안 오는 chunk
가 silent 하게 비트 손상 (bit rot — 디스크 매체의 자연 열화) 을 일으키면
다음 GET 까지 발견 안 됨. 그 다음 GET 도 그 replica 가 선택되지 않으면 영원히
모름.

또한 **inventory drift**: ObjectMeta 는 chunk X 가 dn1 에 있다고 기록, 그러나
dn1 의 실제 디스크에 X 가 없다 (디스크 교체 / 운영 사고 / 부팅 실패). GET 시
다른 replica 로 fallback 되어 동작은 하지만, 그 chunk 의 redundancy 는 R-1 로
저하. 다른 replica 가 죽으면 데이터 손실.

textbook 패턴 (Cassandra anti-entropy, Dynamo gossip, Ceph scrub):

1. **DN-side scrubber** — 주기적으로 chunk 다시 읽고 sha256 재계산, 손상 발견.
2. **Cross-node compare** — Merkle tree 로 inventory 차이 검출. 효율적 ↑.

이 ADR 이 두 갈래 모두 도입.

## 결정

### Part 1 — DN 측 Merkle tree

`GET /chunks/merkle` — 256 buckets (chunk_id[0:2] 으로 bucketing) × 각 bucket
의 sha256("\n"-joined sorted ids). Root = sha256(concat of 256 bucket hashes).

```json
{
  "dn": "dn1",
  "root": "...32 bytes hex...",
  "total": 12345,
  "buckets": [
    {"idx": 0, "hash": "...", "count": 47},
    ...
    {"idx": 255, "hash": "...", "count": 51}
  ]
}
```

`GET /chunks/merkle/bucket?idx=N` — bucket 내 sorted chunk_ids list (diverging
bucket 의 enumeration 용).

**왜 single-tier (flat 256 bucket)?** kvfs scale (DN 당 ≤10⁵ chunks) 에서
- root 비교 = 32 bytes round trip
- divergence 발견 시 정확히 1 bucket fetch ≈ N/256 chunks
- 운영자가 디버그 / 시각화 쉬움

multi-tier hierarchical (Cassandra) 는 10⁷+ chunks/DN scale 에서 의미 — 후속
ADR 후보.

### Part 2 — DN 측 bit-rot scrubber

`DN_SCRUB_INTERVAL=100ms` env opt-in. background goroutine 이 chunk 1개씩
페이스 (interval 마다) 다시 읽고 sha256 재계산. 불일치 시 `corrupt` set 에
등록. `GET /chunks/scrub-status` 가 노출.

```
{
  "dn": "dn1",
  "running": true,
  "interval_seconds": 0.1,
  "chunks_scrubbed_total": 1234,
  "corrupt_count": 2,
  "corrupt": ["abc...", "def..."]
}
```

**Pacing 정당화**: 10⁵ chunks @ 100ms = ~3 시간 / full pass. 운영자가 RTBF
(recovery time before failure) 와 disk IO budget 사이에서 조정.

**Healthy read 가 corrupt set 자동 정리**: 같은 chunk 가 다음 scrub 패스에서
healthy 로 확인되면 corrupt set 에서 자동 제거 (transient read error 회복
시나리오).

**Corrupt 발견 시 chunk 삭제 안 함**: 다른 replica 와 비교 후 결정해야 함.
ADR-054 는 detection only — 삭제 / 복구는 anti-entropy worker 또는 후속 ADR
의 일.

### Part 3 — Coord 측 anti-entropy worker

`POST /v1/coord/admin/anti-entropy/run` — 한 번 audit 실행, 결과 반환:

```json
{
  "started_at": "2026-04-27T10:30:00Z",
  "duration": "2.5ms",
  "dns": [
    {
      "dn": "dn1:8080",
      "reachable": true,
      "expected_total": 1234,
      "actual_total": 1233,
      "root_match": false,
      "missing_from_dn": ["abc..."],
      "extra_on_dn": [],
      "buckets_examined": 1
    },
    ...
  ]
}
```

**Algorithm**:
1. Walk every ObjectMeta → expected map (`dn_addr → bucket → sorted ids`).
2. For each live DN: fetch `/chunks/merkle`. Compare expected root vs actual.
3. If roots match → done (1 round trip).
4. If differ: walk 256 buckets, fetch only buckets whose hashes differ
   (`/chunks/merkle/bucket?idx=N`) → symmetric diff → missing/extra lists.

**Leader-only**: ObjectMeta 는 leader 권한. follower 는 503 + redirect.

**Detection only**: missing chunks 는 보고. 실제 repair 는 기존 worker
(rebalance for replication, ADR-046 for EC) 또는 후속 ADR. 분리 = 검증
단위 격리.

### Part 4 — cli

`kvfs-cli anti-entropy run --coord URL` — JSON pretty-print 결과.

## 비채택 대안

| 대안 | 기각 사유 |
|---|---|
| Multi-tier Merkle (rack → DN → bucket → chunk) | scale 에 비해 과잉. 256-bucket flat 으로 10⁵/DN 충분 |
| 운영자 직접 DN 디스크 walk + sha256 재계산 | shell 노력 + DN 마다 따로. anti-entropy 가 cluster 단일 명령 |
| Scrubber 가 corrupt 발견 시 자동 chunk 삭제 | 다른 replica 와 비교 안 했으니 위험. detection 만 |
| Scrubber 가 다른 replica 에서 chunk 다시 fetch (자가 치유) | 별도 결정 — peer-to-peer DN 통신 도입 (현재는 edge/coord 만 DN 호출). ADR-054 는 detection 분리 |
| Coord 가 ObjectMeta 마다 Merkle 가지고 있음 | bbolt 위에 Merkle 메타 layer. 매 PutObject 마다 incremental 갱신 — DB 부담. flat compute-on-demand 가 단순 |
| Anti-entropy 자동 cron / 주기 실행 | scheduling 분리 — k8s job, cron, systemd timer 가 적절. ADR-054 는 trigger endpoint 만 |

## 결과

+ **Silent corruption 자동 발견**: scrubber 가 bit-rot 검출. 매 chunk 의 sha256
  주기적 검증.
+ **Inventory drift 자동 발견**: anti-entropy 가 각 DN 의 expected vs actual
  비교 (Merkle 효율).
+ **Bandwidth efficient**: healthy state 에선 ~32 bytes/DN audit. divergence
  시 정확히 affected bucket 만 fetch.
+ **No data loss path**: detection only — accidental delete 위험 0.
+ **8 unit tests** (4 dn merkle + 4 기존 dn) PASS · live demo 6 stages PASS.
- **Repair 미포함**: missing chunks 발견해도 자동 복구 안 함. operator 또는
  rebalance worker 가 plan-apply. 후속 ADR (auto-repair on detection) 후보.
- **Scrubber pace trade-off**: 너무 빠름 → IO 부담, 너무 느림 → corruption
  발견 지연. operator 가 cluster 특성 보고 조정.
- **Multi-replica chunk 의 미묘한 fairness**: replica A 의 chunk_id 가 sha256
  검증 통과, replica B 가 corrupt — 둘 다 같은 sha 이지만 B 의 디스크가 다름.
  ADR-054 는 per-DN scrub 으로 둘 다 검증. cross-DN 합의 (어느 쪽이 진짜?)
  는 sha256 = chunk_id invariant 가 자동 해결 — corrupt 가 즉시 식별.
- **Coord anti-entropy 가 EC stripe 의 K survivors 미고려**: ADR-054 는 단순
  chunk-level. EC stripe 의 "K alive 면 OK" 의미는 ADR-046 EC repair worker
  의 책임. 두 ADR 의 책임 분리 명확.

## 호환성

- DN 의 새 endpoint 3 개 (`/chunks/merkle`, `/chunks/merkle/bucket`,
  `/chunks/scrub-status`) — 기존 caller 영향 0.
- Coord 의 새 endpoint 1 개 (`/v1/coord/admin/anti-entropy/run`) — 기존
  영향 0.
- DN_SCRUB_INTERVAL 미설정 시 scrubber off (기존 dn 동작 0 변경).
- cli 새 subcommand `anti-entropy run` — 기존 subcommand 영향 0.

## 검증

- `internal/dn/merkle_test.go` 4 신규 unit test:
  - **TestMerkle_RootStableUnderSameInventory** — 두 DN, 같은 inventory →
    같은 root.
  - **TestMerkle_RootDivergesOnAddRemove** — 1 chunk 차이 → 정확히 그
    bucket 만 hash 다름. 다른 255 bucket 은 byte-identical.
  - **TestMerkle_HTTPRoundTrip** — endpoint 통한 end-to-end.
  - **TestScrubber_DetectsCorruptChunk** — corrupt 한 byte 쓰고 scrubOne
    호출 → corrupt set 에 등장.
- `scripts/demo-tsadi.sh` (히브리 צ, 18번째 letter): 3-DN cluster +
  scrubber 50ms. 6 stages:
  - clean state → all root_match=true ✓
  - rm chunk on dn1 → anti-entropy localizes missing=1 on dn1 only ✓
  - bit-rot on dn2 → scrubber marks corrupt within ~3-4s ✓
  - both findings persist across audit runs ✓
- 전체 test suite **174 PASS** (dn +4).

## 후속

- **Auto-repair**: anti-entropy report 의 missing/extra 로 rebalance plan
  생성. 1-step "audit-and-fix" 명령.
- **Scheduled audit**: anti-entropy 주기 실행을 coord 자체 ticker 로 (또는
  k8s CronJob 안내).
- **Per-replica scrubber 가 자가 치유**: corrupt 발견 시 다른 replica 에서
  fetch 재기록. peer-to-peer DN 통신 도입 (DN 이 현재는 stateless).
- **Multi-tier hierarchical Merkle**: 10⁷+ chunks/DN scale 에서 의미.
