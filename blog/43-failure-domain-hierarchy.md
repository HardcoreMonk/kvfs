# Episode 43 — failure domain hierarchy: rack 한 개 죽여도 살아남기

> **Series**: kvfs — distributed object storage from scratch
> **Season**: 7 (textbook primitives) · **Episode**: 1
> **연결**: [ADR-051](../docs/adr/ADR-051-failure-domain-hierarchy.md) · [demo-samekh](../scripts/demo-samekh.sh)

---

## Season 7 의 정체

Season 1~6 가 분산 storage 의 architectural 기둥 (placement · EC · HA · WAL ·
coord 분리) 을 다 박았다. 그러나 textbook (Dynamo · CRUSH · Cassandra) 의 일부
primitive 는 의도적으로 단순화돼 빠져있었다. 그 단순화의 boundary 가 chaos-
mixed (P8-02) 에서 노출됐고, P8-06 가 일부 메웠다.

**Season 7 = textbook primitive 4개 채워넣기**:
- Ep.1 (samekh ס): failure domain hierarchy ← **this episode**
- Ep.2 (ayin ע): degraded read
- Ep.3 (pe פ): tunable consistency (Dynamo W+R>N)
- Ep.4 (tsadi צ): anti-entropy / Merkle tree

S7 끝나면 frame 2 (학부 분산 시스템 textbook 100%) 도달.

## 한 rack 의 위험

S2 의 HRW 알고리즘 (Ep.2) 을 회상. chunk 의 placement 는 score 순:

```
hash("abc" | "dn1") = 0x1234
hash("abc" | "dn2") = 0xabcd
hash("abc" | "dn3") = 0xfedc  ← top 1
hash("abc" | "dn4") = 0x9876
hash("abc" | "dn5") = 0x5432
hash("abc" | "dn6") = 0xeeee  ← top 2
                       ────────
top-3 = [dn3, dn6, dn2]
```

deterministic + 균등. 그러나 **물리 토폴로지** 모름. 만약:
- dn3, dn6, dn2 모두 같은 rack 안에 있다면?
- 그 rack 의 power outage / network outage / 화재 → 3 replica 동시 잃음
- R=3 cluster 가 한 rack 잃어 chunk 일부 영구 손실

production 시스템은 이걸 막는다. CRUSH (Ceph) 는 hierarchical buckets, Dynamo
는 preference list 의 "skip same-rack", Cassandra 는 `NetworkTopologyStrategy`.
모두 placement 가 **failure domain diversity** 를 강제.

## 구현 — 단순한 변형

핵심 insight: HRW 의 결정성을 깨지 않고 domain 다양성만 추가.

`placement.Node` 에 `Domain string` 필드:

```go
type Node struct {
    ID     string
    Addr   string
    Domain string  // "rack1", "us-west-2a", "" (= default)
}
```

새 함수 `PickFromNodesByDomain`:

```go
1. score 계산 (HRW 그대로)
2. score 내림차순 정렬 (HRW 그대로)
3. ★ Greedy walk: 위에서 아래로
     - 노드의 Domain 이 아직 안 나왔으면 pick
     - 모든 distinct domain 한 번씩 채우면 stop
4. n 이 distinct domain 수보다 많으면:
     - score 순으로 fallback fill (같은 domain 중복 OK, 같은 노드 X)
```

**Determinism 보존**: greedy walk 가 score-sorted 위에서 도므로, 같은 (chunkID,
node-set, domain-tags) 면 같은 결과. randomness 0.

코드 ~50 LOC 추가. fast-path: 모든 노드의 Domain 이 빈 문자열이면 그냥
기존 `PickFromNodes` 호출 (back-compat).

## ס samekh 데모

```
$ ./scripts/demo-samekh.sh
==> 6 DN across 3 racks
    dn1@rack1  dn4@rack1
    dn2@rack2  dn5@rack2
    dn3@rack3  dn6@rack3

==> tagging via cli
    kvfs-cli dns domain dn1:8080 rack1 --coord http://coord1:9000
    ... (×6)

==> PUT replicated object (R=3)
    {chunks: 1, replicas: ["dn6:8080", "dn4:8080", "dn5:8080"]}
    replica racks: rack3 rack1 rack2
    distinct racks: 3
    ✓ all 3 replicas in distinct racks

==> PUT EC (4+2) — 6 shards, 3 racks
    stripe 0 shards: dn1@rack1, dn4@rack1, dn2@rack2, dn5@rack2, dn3@rack3, dn6@rack3
    distinct racks: 3
    ✓ every EC stripe spans all 3 racks

=== ס PASS: topology-aware HRW ===
```

R=3 replicated PUT 이 항상 3 distinct racks 에. EC (4+2) 의 6 shards 도 3 racks
에 분포 (rack 당 2 shards). 한 rack 잃어도:
- replication 모드: 2 replicas 살아남 (R=3 → 1 rack 잃기 OK)
- EC 모드: 4 shards 살아남 (K=4, M=2 → 2 shards 잃기 = 1 rack 잃기 OK)

## domains 수 < R 시

만약 R=3 인데 distinct domain 이 2 개라면? Greedy walk 가 2 개 채우고 멈춤.
fallback walk 가 score 순으로 3번째 슬롯을 채움 — 같은 domain 중복 됨.

```
domains: rack1 (3 nodes), rack2 (3 nodes)
chunk → top-3 by score → dn1@rack1, dn4@rack1, dn2@rack2
                          ↑ 두 replica 한 rack
```

이게 **정직한 한계**. ADR-051 - 항목에 명시: "운영자에게 domain 수 ≥ R 권장
가이드". rack 한 개 잃으면 R-2 만 살아남 — degraded 이지만 GET 가능 (read
quorum 1).

## CRUSH 와의 비교

[CRUSH](https://www.ssrc.ucsc.edu/Papers/weil-sc06.pdf) (Ceph) 는 multi-tier
hierarchical:

```
root
├── region us-west
│   ├── rack r1
│   │   ├── host h1 → dn1
│   │   └── host h2 → dn2
│   └── rack r2 ...
└── region us-east ...
```

placement 가 각 tier 마다 distinct 강제 가능. 강력하지만 복잡 — bucket type,
weight, rule, ruleset.

ADR-051 은 **single-tier**. region/rack/host 를 한 string 으로 평탄화 ("us-west-
r1-h1" 같이). kvfs scale 에선 충분. 다중 tier 의 needs 명확해지면 후속 ADR.

## 운영 워크플로우

```
운영자가 새 DN 추가 시:
1. dn7:8080 컨테이너 기동
2. cli `dns add dn7:8080 --coord URL`         (registry 등록)
3. cli `dns domain dn7:8080 rack1 --coord URL` (failure domain 라벨)
4. (옵션) cli `dns class dn7:8080 hot --coord URL` (tier 라벨)
```

라벨은 bbolt 에 영속 (dns_runtime bucket 의 entry 에 `domain` 키로). HA mode
에서는 ADR-039 의 WAL replication 으로 follower coord 들 자동 sync.

label 잘못 (실제 rack 다른데 같은 domain 으로 표기) 시 가짜 안전감 줌. ADR-
051 - 항목이 정직하게 명시.

## frame 2 진척

- **있음 ✓** (이 글 이후): failure domain hierarchy
- 다음: degraded read · tunable consistency · anti-entropy

S7 4개 ep 끝나면 frame 2 (학부 textbook primitive 100%) 도달. ADR-009 의
HRW 가 textbook 의 가장 단순한 형태였다면, S7 ep 들이 그 위로 production-
class 의 4 primitive 를 한 단계씩 쌓는다.

## 다음

Ep.44 — Degraded read. 현재 GET 은 첫 chunk read 실패 시 다음 replica fallback.
EC 의 가치 (K survivors → reconstruct) 가 read path 에 즉시 활용 안 됨 — 별도
worker (ADR-025 repair) 가 나중에. textbook 패턴은 read 시점에 reconstruct.
ADR-052. demo-ayin (ע).
