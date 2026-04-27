# Episode 46 — anti-entropy: 안 읽혀지는 chunk 의 무결성 + S7 close

> **Series**: kvfs — distributed object storage from scratch
> **Season**: 7 (textbook primitives) · **Episode**: 4 (S7 close · frame 2 = 100%)
> **연결**: [ADR-054](../docs/adr/ADR-054-anti-entropy-merkle.md) · [demo-tsadi](../scripts/demo-tsadi.sh)

---

## 보이지 않는 손상

지금까지 kvfs 의 무결성 보장은 **읽기 시점** 에만 묶였다:

- 매 GET → DN 응답의 sha256 == chunk_id 검증 ([Ep.1](01-hello-kvfs.md) content-
  addressable 결정)
- ADR-040 transactional commit + ADR-039 WAL repl → 메타 일관성

**그러나 안 읽혀지는 chunk** 는? 디스크에 1년 동안 존재만 하다가 자연 열화 (bit
rot — 자기 매체의 미세한 자기 강도 약화) 로 1 비트 뒤집혀도 다음 GET 까지
모름. 그 다음 GET 도 그 replica 가 안 골라지면 영원히 모름.

또는 **inventory drift**: ObjectMeta 가 chunk X 가 dn1 에 있다고 기록, 그러나
dn1 의 디스크에 X 가 없다 (디스크 교체 / 운영 사고 / 부팅 실패 / 사람이 rm).
GET 은 다른 replica 로 fallback 하니 동작은 하지만, 그 chunk 의 redundancy
가 R-1 로 저하. 다른 replica 도 죽으면 데이터 손실.

textbook 패턴 (Cassandra anti-entropy, Dynamo gossip, Ceph scrub):

1. **DN-side scrubber** — 주기적 chunk 재읽기 + sha256 재검증
2. **Cross-node compare** — Merkle tree 로 inventory 효율적 비교

## Merkle 256-bucket flat tree

`GET /chunks/merkle` on DN:

```json
{
  "dn": "dn1",
  "root": "...sha256 hex...",
  "total": 12345,
  "buckets": [
    {"idx": 0,   "hash": "...", "count": 47},
    {"idx": 1,   "hash": "...", "count": 51},
    ...
    {"idx": 255, "hash": "...", "count": 49}
  ]
}
```

256 buckets keyed by `chunk_id[0:2]` (hex prefix). 각 bucket 의 hash =
sha256("\n"-joined sorted ids in bucket). Root = sha256(concat of 256 bucket
hashes).

**왜 single-tier?** kvfs scale (≤10⁵ chunks/DN) 에서 충분. healthy compare =
32-byte root. divergence 시 정확히 1 bucket fetch (~N/256 chunks).

multi-tier hierarchical (Cassandra) 는 10⁷+/DN 에서 의미. 후속 ADR.

## DN-side scrubber

```
DN_SCRUB_INTERVAL=100ms   # per-chunk 페이스
```

background goroutine 이:
1. chunks/ 디렉토리 walk
2. 각 chunk 한 개씩 (interval 마다) 재읽기 + sha256 재계산
3. 불일치 시 in-memory `corrupt` set 에 등록
4. healthy 재확인 시 자동 제거 (transient error 회복)

`GET /chunks/scrub-status`:
```json
{
  "dn": "dn1",
  "running": true,
  "interval_seconds": 0.1,
  "chunks_scrubbed_total": 1234,
  "corrupt_count": 1,
  "corrupt": ["abc12345..."]
}
```

**Pacing 의 의미**: 10⁵ chunks @ 100ms ≈ 3시간/full pass. operator 가 RTBF
(recovery time before failure) ↔ disk IO budget trade-off 결정.

**Detection only**: scrubber 가 corrupt 발견해도 chunk 삭제 안 함. 다른
replica 와 비교한 후 결정해야 안전. 복구는 후속 ADR (또는 rebalance worker).

## Coord anti-entropy worker

`POST /v1/coord/admin/anti-entropy/run`:

```
1. ObjectMeta 전체 walk → expected: dn → bucket → sorted chunk_ids
2. 각 live DN 에 대해 GET /chunks/merkle
3. expected root vs actual root:
   match → 1 RTT 끝
   diff  → 256 buckets 비교, 다른 hash 인 bucket 만 enumerate
4. 각 DN 의 missing / extra 보고
```

응답:
```json
{
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
    }
  ]
}
```

**Bandwidth efficient**: healthy cluster → DN 당 ~32-byte audit. divergence
는 정확히 affected bucket 만 fetch.

## צ tsadi 데모

```
$ ./scripts/demo-tsadi.sh

==> stage 1: PUT 3 small objects
    PUT tsadi/obj1..obj3 ✓

==> stage 2: anti-entropy run (clean)
    {"dns":[
      {"dn":"dn1:8080","reachable":true,"expected":3,"actual":3,"root_match":true},
      {"dn":"dn2:8080","reachable":true,"expected":3,"actual":3,"root_match":true},
      {"dn":"dn3:8080","reachable":true,"expected":3,"actual":3,"root_match":true}
    ]}
    ✓ all DN inventories match

==> stage 3: rm chunk from dn1 (simulate disk loss)
    removed 85e08f28...

==> stage 4: anti-entropy localizes
    {"dns":[
      {"dn":"dn1:8080","root_match":false,"missing":1,"extra":0},
      {"dn":"dn2:8080","root_match":true,"missing":0,"extra":0},
      {"dn":"dn3:8080","root_match":true,"missing":0,"extra":0}
    ]}
    ✓ Merkle 가 dn1 의 bucket 만 다르다고 정확히 식별 — dn2, dn3 변경 0

==> stage 5: bit-rot on dn2 (overwrite chunk file with garbage)
    {"corrupt":["135a3964..."]}
    ✓ scrubber 가 50ms × 몇 chunks 안에 sha mismatch 잡음

==> stage 6: anti-entropy + scrubber 동시 — both findings persist
    ✓ inventory drift + bit-rot 둘 다 detected

=== צ PASS ===
```

evidence:
- Merkle 의 bucket-level localization — 3 DN 중 dn1 만 mismatch, 다른 둘은
  byte-identical 유지
- Scrubber 가 bit-rot 을 read 없이 background 에서 detect
- 두 메커니즘 직교 — inventory drift (Merkle) ↔ bit-rot (scrubber)

## ADR-005 의 dividend, 또

[Ep.1 의 sha256 chunk_id](01-hello-kvfs.md) 결정이 ADR-054 까지 살아있다.
"chunk_id 가 곧 sha256" 이 invariant 라:

- scrubber: "이 파일의 sha256 == 파일이름" 만 검증하면 됨. 외부 메타 안 봐도
  bit-rot 발견.
- anti-entropy: chunk_id 만 비교하면 컨텐츠 일치 자동 보장 (id 충돌 = 우주
  종말급 sha256 충돌).

ADR-005 (2 줄 짜리 결정) 이 이 시점에서 또 한 번 가치 입증. **좋은 architecture
는 후속 features 가 거의 공짜로 만들어진다**.

## frame 2 (textbook primitives) 100% 도달

S7 4 episodes 회고:

| Ep | Hebrew | ADR | 주제 |
|---|---|---|---|
| 1 | samekh ס | 051 | Failure domain hierarchy |
| 2 | ayin ע | 052 | Degraded read |
| 3 | pe פ | 053 | Tunable consistency |
| **4** | **tsadi צ** | **054** | **Anti-entropy / Merkle** |

학부 분산 시스템 textbook 의 핵심 4 primitive 가 모두 kvfs 에 들어옴. 더
이상 "textbook 에 있는데 kvfs 엔 없는 것" 카테고리에 큰 항목 없음.

ADR-009 의 HRW 가 가장 단순한 형태였다면, S7 ep 들이 그 위로:
- failure domain (CRUSH single-tier)
- read-time RS reconstruct (textbook EC)
- Dynamo W+R>N
- anti-entropy (Cassandra/Dynamo classic)

production-class 분산 storage 의 모양이 하나의 reference 안에 모임.

## 남은 갈래

ADR-054 의 -항목 + 후속:

- **Auto-repair** — anti-entropy report 가 직접 rebalance plan 생성 → apply.
- **Scheduled audit** — coord 자체에 ticker (또는 k8s CronJob 가이드).
- **Multi-tier Merkle** — 10⁷+ chunks/DN 으로 가면 hierarchical 도입.
- **Per-replica self-heal** — scrubber 가 corrupt 발견 시 다른 replica 에서
  자가 fetch (peer-to-peer DN 통신 도입).

이들 모두 P8 의 다음 wave 또는 별도 시즌의 일.

## frame 1 + frame 2 모두 100%

P8 wave 의 원래 목표:
- ✅ frame 1 (헌장 — 살아있는 reference) = **100%** (S5/S6 blog backfill)
- ✅ frame 2 (textbook primitives) = **100%** (S7 4 ep)

두 frame 의 100% 가 같은 commit 시리즈에 도달.

## 다음

이제 P8 다음 wave 를 결정할 시점. 후보:
- frame 3 (production-grade) 진입 — 그러나 헌장이 production 아님 명시.
  fork 추천.
- 미세 보강 — auto-repair, multi-tier Merkle, hedged read 등 ADR-054 의
  후속들.
- 새 시즌 — ?

frame 1+2 의 끝은 산이 아니라 능선. 어디로 갈지는 다음 글의 결정.
