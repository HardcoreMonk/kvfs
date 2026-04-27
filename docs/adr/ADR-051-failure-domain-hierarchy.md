# ADR-051 — Failure domain hierarchy (topology-aware HRW)

상태: Accepted (2026-04-27)
시즌: Season 7 Ep.1 — frame 2 (textbook primitives) wave 시작

## 배경

Season 1~6 의 placement 알고리즘 (HRW, ADR-009) 은 chunk_id 와 DN ID 만 입력
받는다. 노드의 **물리적 토폴로지** (rack, AZ, region) 는 모름. 결과:

```
DN: dn1..dn6 (rack1: dn1,dn4 / rack2: dn2,dn5 / rack3: dn3,dn6)
chunk = abc, R=3
HRW score 계산 → 우연히 dn1, dn4, dn5 가 top-3 → 두 replica 가 rack1
rack1 전체 outage → chunk 중 일부가 R replica 모두 잃음
```

production-class 분산 storage 는 이 케이스를 막는다. CRUSH (Ceph) 의 hierarchical
bucket, Cassandra 의 `NetworkTopologyStrategy`, S3 의 AZ-aware placement 등.
정통 textbook primitive 인데 kvfs 에 없었음.

`docs/GUIDE.md §12.1` 한계 첫 줄도 "DN 간 통신 없음. ... 모든 복구는 edge 가
주도" — 그러나 placement 다양성 자체가 부재했다. P8-04 (Season 7 textbook
primitives) 의 첫 ep 이 이 갭 메움.

## 결정

`placement.Node` 에 `Domain string` 필드 추가. 빈 문자열은 "default" 로
취급 (back-compat — pre-S7 cluster 가 그대로 작동).

`PickFromNodesByDomain` 신규 — 같은 HRW score 정렬 위에 **failure-domain
greedy** 선택:

```
1. score 내림차순으로 nodes 정렬
2. 위에서부터 walk:
   - 노드의 Domain 이 아직 안 쓰였으면 pick
   - 모든 distinct domain 이 한 번씩 채워질 때까지 반복
3. n 이 distinct domain 수보다 크면 score 순으로 fallback fill
   (같은 domain 중복 허용, 같은 노드 중복은 금지)
```

핵심 invariant: **결정적**. 같은 (chunkID, node-set) → 같은 결과. greedy walk
가 score-sorted 위에서 도므로 randomness 0.

`Placer.PickByDomain(chunkID, n)` 와 `internal/coordinator.Coordinator.PlaceN/
PlaceChunk/WriteChunk` 가 이 path 를 default 로 사용. 도메인 라벨 0 인
cluster 는 자동 fast-path → 기존 `PickFromNodes` 와 동일 결과 (back-compat).

운영자 인터페이스:

- `PUT /v1/coord/admin/dns/domain?addr=X&domain=Y` (leader-only). 빈 domain 은
  unlabel.
- `kvfs-cli dns domain <addr> <rack> --coord URL`.
- `GET /v1/coord/admin/dns?detailed=1` 가 [{addr, domain}] shape 반환 (기존
  flat array shape 는 default 로 보존 — cli inspect back-compat).
- coord daemon boot 시 `dns_runtime` 이 비어있으면 `COORD_DNS` env 로 seed
  (edge 의 ADR-027 패턴 mirror — 운영자가 매 DN 마다 `dns add` 안 해도 됨).

WAL replication (ADR-039) 자동 동기화 — `SetRuntimeDNDomain` 도 bbolt mutation
이라 leader 의 walHook 이 followers 에 push.

## 알고리즘 detail

```go
func PickFromNodesByDomain(chunkID string, n int, nodes []Node) []Node {
    if !anyDomainSet(nodes) { return PickFromNodes(...) }  // fast-path

    sort by HRW score desc, ID asc tiebreaker

    // First pass: top-scored node per distinct domain
    for s in scoreds:
        if usedDomain[s.Domain] continue
        out += s; usedDomain[s.Domain] = true
        if len(out) == n break

    // Second pass: fill remaining slots by score
    for s in scoreds:
        if usedNode[s.ID] continue
        out += s
        if len(out) == n break

    return out
}
```

복잡도 O(N log N) (sort) + O(N) (두 walk). N = DN 수 (수백 이하 가정).
hot path 가 아니므로 추가 비용 무시.

## 비채택 대안

| 대안 | 기각 사유 |
|---|---|
| Hierarchical buckets (CRUSH-style: rack > host > device) | YAGNI. kvfs scale 에선 single-tier (rack) 충분. 다중 tier 는 운영 복잡도 ↑↑. 후속 ADR 후보 |
| Score 직접 변형 (domain 기반 weight) | 결정적이긴 하나 **분포 균등성** 깨짐 — 한 domain 에 노드 많으면 그 domain bias. greedy 가 더 명확하고 분석 쉬움 |
| 운영자가 DN ID 에 prefix encoding (dn-rack1-1) + Placer 가 parse | 라벨이 ID 와 결합 → ID 변경 비용 ↑. Domain 별 필드가 깨끗 |
| HRW 자체 폐기 → CRUSH port | 너무 큰 변경. HRW 의 단순함이 kvfs 의 educational value. CRUSH 는 산업 강도지만 reference 가치 낮음 |

## 결과

+ **rack-level outage 내성**: R=3 cluster + 3 racks → 한 rack 죽어도 나머지
  2 rack 에 R-1 (= 2) replica 살아있음. read quorum 1 이면 GET 성공.
+ **EC stripe diversity**: K+M shards 가 distinct racks 에 spread → rack 1
  개 잃어도 K survivor 충족 (K+M 중 M 까지 잃어도 reconstruct 가능 + rack
  단위 보존 강화).
+ **Determinism 보존**: HRW 의 reproducibility 그대로 — 같은 (chunkID, nodes,
  domains) 면 같은 결과.
+ **opt-in**: domain 라벨 0 인 cluster 는 동작 0 변경 (back-compat).
- **Operational burden**: 운영자가 정확한 domain 라벨 설정 책임. 잘못된 라벨
  (실제 같은 rack 인데 다른 domain 으로 표기) 이면 가짜 안전감 줌.
- **Domain 수 < R 시**: 모든 replica 가 distinct domain 에 못 감. fallback
  으로 같은 domain 두 개 picks — 안전성 일부 회복 못 함. 정직한 한계: 운영자
  에게 "domain ≥ R 권장" 가이드.
- **Class 와의 interplay**: PlacementPreferClass + Domain 은 직교. class
  subset 안에서 domain diversity 적용. fallback 시 class 우선.
- **Single-tier**: 다중 hierarchy (rack→host→device) 미지원. 큰 cluster 운영
  시 needs 명확해질 때 ADR-052+ 후보.

## 호환성

- `placement.Node.Domain` 신규 필드 — zero value "" 가 unlabeled 의미. 기존
  caller 가 Domain 안 채워도 동일 동작.
- `PickByDomain` 새 메서드 — 기존 `Pick` 그대로 보존. 두 함수 결과는 도메인
  라벨 0 인 cluster 에선 identical.
- `coord.Server.Routes` 에 새 endpoint 1개 추가. 기존 endpoint 0 영향.
- 새 query 파라미터 `detailed=1` on `/v1/coord/admin/dns` — 기본 응답 shape
  (`[]string`) 은 cli inspect back-compat.
- Coord daemon boot 의 dns_runtime auto-seed — 첫 boot 후 bbolt 가 채워지면
  이후 boot 는 그 list 사용 (기존 운영자 manual `dns add` 도 그대로 작동).

## 검증

`internal/placement/placement_test.go` 에 3 신규 unit test:
- `TestPickByDomain_DistinctDomainsWhenPossible` — 6 nodes / 3 domains, R=3
  → 100 chunks 모두 distinct rack. 0 violations.
- `TestPickByDomain_FallsBackWhenDomainsScarce` — 6 nodes / 2 domains, R=3
  → 두 domain 모두 등장 (어느 도메인이든 ≥ 1) + 같은 노드 0 dup.
- `TestPickByDomain_BackCompat_NoDomainTags` — Domain "" everywhere → Pick
  과 PickByDomain 결과 byte-identical.

`scripts/demo-samekh.sh` (히브리 ס, 15번째 letter): 6 DN x 3 rack live
cluster. R=3 replicated PUT 의 chunk replicas → 3 distinct racks 검증.
EC (4+2) PUT 의 매 stripe → 6 shards 가 3 distinct racks 에 분포 (실측
2 shards/rack 수준). 강한 invariant 시연.

전체 test suite **164 PASS** (placement +3).

## 후속

- **Multi-tier hierarchy** (region → AZ → rack → host): single-tier 가 충분
  하지 않은 운영 측정 보고 후 ADR-055 후보.
- **EC-aware domain weighting**: M parity shards 우선 distinct domain, K
  data shards 는 score 우선 — read latency 와 안전성의 trade-off. 후속
  measurement 후.
- **Auto-detect domain from DN env** (`DN_DOMAIN=rack1`): 현재는 admin 으로
  set. DN 자기-신고 + coord 가 healthz 에서 수집하면 운영 부담 ↓.
