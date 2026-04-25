# kvfs from scratch, Ep.2 — DN을 4개로 늘리던 날, Rendezvous Hashing이 한 일

> Season 2 · Episode 1. "분산 스토리지의 라우팅 테이블"이라는 신화를 20줄 Go로 해체합니다.

## 이 글이 보여주는 것

DN 5개에 청크 10,000개를 흩뿌려 둔 클러스터에 **DN을 1개 더 추가**했을 때, 청크 중 몇 %가 새 자리로 이사해야 할까요?

직관: "음... 클러스터가 다시 균등해지려면 다 옮겨야 하지 않나?"
실측: **49.26%**. 이론 하한 **R/(N+1) = 3/6 = 50%** 와 0.74%p 차이.

핵심 코드는 함수 하나, 약 20줄. 이게 Rendezvous Hashing(HRW)의 약속입니다.

```
📊 Chunks moved: 4926 / 10000 = 49.26%
📐 Theoretical lower bound (R/N): 50.00%
```

이 한 줄 안에 "분산 스토리지가 운영 가능한 시스템인가, 데이터 마이그레이션 지옥인가"의 분기점이 들어 있습니다.

## Ep.1 복기 — 3 DN의 거짓 평화

Ep.1에서 우리는 3 DN · 3-way replication을 시연했습니다. 코드는 행복했죠:

```go
// Season 1 MVP — coordinator.go
for _, dn := range c.dns {
    putChunk(dn, ...)
}
```

**모든 DN에 쓴다.** R=N=3이니 의문도 없습니다.

문제는 N=4가 되는 순간입니다. 갑자기 "어느 3개 DN에 쓸 것인가?"라는 질문이 생기고, 같은 청크를 다시 읽을 때 "어느 3개 DN에서 찾을 것인가?"가 매번 같은 답을 내야 합니다. 게다가 N이 5, 6, 10으로 늘 때마다 기존 청크가 대부분 그 자리에 머물러야 합니다 — 아니면 DN 추가가 **클러스터 전체 마이그레이션 이벤트**가 됩니다.

## 가장 단순한 답들이 망가지는 이유

### 안 1: "랜덤 3개 골라서 메타DB에 적기"

```
PUT chunk-X → random([dn2, dn4, dn5]) → 메타DB.write("chunk-X": [2,4,5])
GET chunk-X → 메타DB.read("chunk-X") → [2,4,5]
```

동작은 합니다. 하지만:
- 모든 GET이 메타DB hit. 메타DB가 핫스팟
- 메타 손실 = 데이터 어디 있는지 영영 모름 (실제로 있는데도)
- 청크 1억 개면 placement 테이블만 수 GB

### 안 2: "chunk_id mod N으로 결정"

```
PUT chunk-X → hash(X) % N → DN 선택
```

DN 1개 추가 시 N이 바뀌고 → **거의 모든 청크의 mod 결과가 바뀜** → 전체 재배치. 이게 운영 불가능의 원조 사례입니다.

### 안 3: "고전 링 기반 일관 해싱"

Dynamo · Cassandra가 쓰는 방식. 동작은 정말 잘 됩니다. 하지만:
- 균등 분포를 위해 DN 1개당 가상 노드(vnode) 100~1000개를 링에 흩뿌려야 함
- 링 자료구조 + 이진 탐색 + vnode 관리 = 코드 수백 줄
- "왜 이렇게 복잡해야 하는가"를 설명하려면 30분 넘게 걸림

kvfs의 규모(수십 DN 이하)에서는 **이게 과합니다**.

## Rendezvous Hashing의 답 — 점수 매기고 상위 N개

핵심 한 줄:

> 각 DN에 대해 청크와의 **점수**를 계산하고, **점수 높은 R개**를 고른다.

점수 함수:

```
score(chunkID, nodeID) = hash(chunkID + "|" + nodeID)
```

이게 전부입니다. **메타 없음**, **링 없음**, **vnode 없음**. 같은 입력은 항상 같은 점수를 내므로, 클러스터 어느 노드에서든 "chunk-X는 어느 DN 3개에 있어야 하는가?"의 답이 일치합니다.

### 손으로 따라가기 — 5 DN · 청크 "abc" · R=3

```
hash("abc" + "|" + "dn1") = 0x1234
hash("abc" + "|" + "dn2") = 0xabcd   ← 2위
hash("abc" + "|" + "dn3") = 0x5678
hash("abc" + "|" + "dn4") = 0xfedc   ← 1위  🥇
hash("abc" + "|" + "dn5") = 0x9876   ← 3위
                            ────────
상위 3개:                  [dn4, dn2, dn5]
```

이 결정은 chunkID `"abc"`가 변하지 않는 한 **영원히 동일**합니다.

### 왜 "consistent"가 되는가

**dn6를 추가**한다고 칩시다 (5 → 6 DN).

```
기존 점수:
  dn1: 0x1234, dn2: 0xabcd, dn3: 0x5678, dn4: 0xfedc, dn5: 0x9876
새 점수:
  dn6: 0x3333  (예시)
```

dn6의 점수(0x3333)가 기존 상위 3(0xfedc, 0xabcd, 0x9876)에 못 들어가므로 → **이 청크는 이사 안 함**.

만약 dn6 점수가 0xefff였다면? → dn5(0x9876)를 밀어내고 상위 3에 진입 → **이 청크 1개만 이사**. dn1·dn2·dn3·dn4 청크들과는 무관.

평균적으로 **R/(N+1)** 비율의 청크만 이사합니다. 5→6의 경우 3/6 = 50%. 이게 "consistent"의 수학적 의미입니다.

## 진짜 코드 (전체)

`internal/placement/placement.go`의 핵심:

```go
func (p *Placer) Pick(chunkID string, n int) []Node {
    if n <= 0 || len(p.nodes) == 0 {
        return nil
    }
    if n > len(p.nodes) {
        n = len(p.nodes)
    }

    // 1. 각 노드의 점수 계산
    type scored struct {
        node  Node
        score uint64
    }
    scoreds := make([]scored, len(p.nodes))
    for i, node := range p.nodes {
        scoreds[i] = scored{node: node, score: hrwScore(chunkID, node.ID)}
    }

    // 2. 점수 내림차순 정렬 (동점은 ID 사전순으로 결정적 처리)
    sort.Slice(scoreds, func(i, j int) bool {
        if scoreds[i].score != scoreds[j].score {
            return scoreds[i].score > scoreds[j].score
        }
        return scoreds[i].node.ID < scoreds[j].node.ID
    })

    // 3. 상위 n개 반환
    out := make([]Node, n)
    for i := 0; i < n; i++ {
        out[i] = scoreds[i].node
    }
    return out
}

func hrwScore(chunkID, nodeID string) uint64 {
    h := sha256.New()
    h.Write([]byte(chunkID))
    h.Write([]byte("|"))     // 구분자 — 없으면 ("ab","c") = ("a","bc") 충돌
    h.Write([]byte(nodeID))
    sum := h.Sum(nil)
    return binary.BigEndian.Uint64(sum[:8])  // 첫 8바이트면 충분
}
```

20줄대 + 헬퍼 1개. 이게 분산 스토리지 라우팅의 전부입니다. Ceph CRUSH도 이 핵심을 공유합니다 — 추가로 rack-awareness 같은 제약을 얹을 뿐.

## 라이브 데모 — `placement-sim`

설명은 그만하고 실측 들어갑시다. 빌드 한 번:

```bash
docker run --rm -v "$PWD:/src" -w /src -e CGO_ENABLED=0 \
  golang:1.26-alpine sh -c 'go build -o ./bin/kvfs-cli ./cmd/kvfs-cli'
```

### 케이스 A — N=3 → 4 (MVP에서 4번째 DN 추가)

```
$ ./bin/kvfs-cli placement-sim --nodes 3 --chunks 10000 --replicas 3 --add 1

Initial: 3 DNs, placing 10000 chunks with replicas=3
  dn1    10000 chunks (100.0%) │██████████████████████████████│
  dn2    10000 chunks (100.0%) │██████████████████████████████│
  dn3    10000 chunks (100.0%) │██████████████████████████████│

After adding 1 DN(s) → 4 total
  dn1     7458 chunks ( 74.6%) │██████████████████████········│
  dn2     7506 chunks ( 75.1%) │██████████████████████········│
  dn3     7486 chunks ( 74.9%) │██████████████████████········│
  dn4     7550 chunks ( 75.5%) │██████████████████████········│

📊 Chunks moved: 7550 / 10000 = 75.50%
📐 Theoretical lower bound (R/N): 75.00%
```

읽는 법:
- **R=N=3 일 때**: 모든 DN에 100% 청크가 들어감 (Ep.1의 상황)
- **N=4 가 되자**: dn4가 등장하면서 청크의 75%가 dn4에 새로 들어감(과 동시에 기존 DN 중 하나가 빠짐). 기존 25%는 그대로
- 이론값 R/N = 3/4 = 75% 와 일치

여기서 한 가지 중요한 진실: **N이 R에 가까울 땐 이동률이 클 수밖에 없습니다.** 이건 알고리즘의 약점이 아니라 **수학적 하한**입니다 — 4개 DN에 3개씩 들어가야 하면, 새 DN은 청크의 3/4를 받아야 균등해집니다. HRW는 이 하한에 거의 정확히 도달합니다.

### 케이스 B — N=5 → 6 (스케일아웃 구간)

```
$ ./bin/kvfs-cli placement-sim --nodes 5 --chunks 10000 --replicas 3 --add 1

Initial: 5 DNs, placing 10000 chunks with replicas=3
  dn1     5974 chunks ( 59.7%) │█████████████████·············│
  dn2     5960 chunks ( 59.6%) │█████████████████·············│
  dn3     6009 chunks ( 60.1%) │██████████████████············│
  dn4     6065 chunks ( 60.7%) │██████████████████············│
  dn5     5992 chunks ( 59.9%) │█████████████████·············│

After adding 1 DN(s) → 6 total
  dn1     4964 chunks ( 49.6%) │██████████████················│
  dn2     4989 chunks ( 49.9%) │██████████████················│
  dn3     5019 chunks ( 50.2%) │███████████████···············│
  dn4     5097 chunks ( 51.0%) │███████████████···············│
  dn5     5005 chunks ( 50.0%) │███████████████···············│
  dn6     4926 chunks ( 49.3%) │██████████████················│

📊 Chunks moved: 4926 / 10000 = 49.26%
📐 Theoretical lower bound (R/N): 50.00%
```

각 DN의 청크 점유율이 60% → 50%로 자연스럽게 이동. 신규 DN(dn6)이 갑자기 100% 또는 0% 같은 극단으로 가지 않습니다 — 다른 DN과 동등한 비율로 정착.

### 케이스 C — N=10 → 9 (DN 1개 영구 사망)

```
$ ./bin/kvfs-cli placement-sim --nodes 10 --chunks 10000 --replicas 3 --remove 1

Initial: 10 DNs, placing 10000 chunks with replicas=3
  dn1     3015 chunks ( 30.1%) │█████████·····················│
  dn2     2990 chunks ( 29.9%) │████████······················│
  ...
  dn10    2911 chunks ( 29.1%) │████████······················│

After removing 1 DN(s) → 9 total
  dn1     3330 chunks ( 33.3%) │█████████·····················│
  dn2     3321 chunks ( 33.2%) │█████████·····················│
  ...
  dn9     3402 chunks ( 34.0%) │██████████····················│

📊 Chunks moved: 2911 / 10000 = 29.11%
📐 Theoretical lower bound (R/N): 30.00%
```

dn10이 사라지자 정확히 dn10이 갖고 있던 청크 수(2,911 ≈ 29.1%)만큼만 재배치 대상이 됩니다. 다른 DN들은 그대로 — 단지 잃은 replica 한 자리가 살아있는 9 DN으로 골고루 흡수될 뿐.

## 실측 vs 이론 한눈에

| 시나리오 | 이론 R/N | 실측 | 오차 |
|---|---|---|---|
| 3 → 4 (+1) | 75.00% | 75.50% | +0.50%p |
| 5 → 6 (+1) | 50.00% | 49.26% | -0.74%p |
| 10 → 9 (-1) | 30.00% | 29.11% | -0.89%p |

10,000개 표본 기준 1%p 이내. SHA-256 분포의 균일성이 그대로 드러납니다.

## 고전 링 vs HRW — 같은 약속, 다른 가격

| 차원 | 고전 링 (Dynamo·Cassandra) | Rendezvous (HRW) |
|---|---|---|
| 자료구조 | 정렬된 토큰 링 + vnode 100~1000개/DN | 단순 노드 배열 |
| Lookup 비용 | O(log N) 이진 탐색 | O(N) 점수 계산 |
| 균등 분포 | vnode 수가 충분해야 보장 | 항상 균등 (해시 균일성에 의존) |
| 코드 LOC | 수백 줄 | ~20 LOC |
| 추가/제거 시 이동률 | ≈ 1/N | ≈ R/(N+1) |
| Rack-awareness | vnode 배치 전략에 녹임 | 별도 계층 필요 (CRUSH 스타일 확장) |

**O(N)이 단점인가?** N=100이라도 lookup 1번이 마이크로초 단위. N=1,000부터 의미가 생기고, 그 이상은 ring으로 갈아탈 만한 시점입니다. kvfs가 의도하는 규모(수십 DN)에서는 **HRW의 단순함이 압도적**입니다.

## 코디네이터의 단 두 줄 변경

`internal/coordinator/coordinator.go`의 변화는 정확히 이만큼:

```go
// Season 1: "모든 DN에 쓰기"
for _, dn := range c.dns {
    putChunk(dn, ...)
}

// Season 2 Ep.1: "Pick으로 R개 고른 뒤 그 R개에만 쓰기"
targets := c.placer.Pick(chunkID, c.replicationFactor)
for _, n := range targets {
    putChunk(n.Addr, ...)
}
```

`placer.Pick`이 결정적이므로 GET 경로도 같은 함수를 호출해 똑같은 R개를 알아냅니다. 메타DB는 청크 메타에 `replicas` 배열을 여전히 저장하지만, 그건 **이력**이지 **권위**가 아닙니다. 노드가 살아있는 한 placement만으로 재현 가능.

## 솔직한 한계 — Season 2가 풀어야 할 것들

HRW가 끝이 아닙니다. ADR-009의 "부정" 절에 적힌 그대로:

1. **기존 청크 자동 rebalance 없음** — 새 쓰기는 즉시 올바른 자리에 가지만, **기존 청크는 옛 자리에 머문다**. DN 추가 후 균등화하려면 명시적 마이그레이션 작업이 필요. → **ADR-010 (Rebalance worker)** 가 다음 후보
2. **Rack-awareness 없음** — "같은 rack에 replica 2개 있으면 안 됨" 같은 제약 미지원. CRUSH 스타일 계층 확장이 필요해질 때 별도 ADR
3. **R=N일 때는 placement 효과 없음** — Ep.1의 3 DN · R=3 환경에서는 모든 DN이 항상 선택. 효과는 N>R에서 나타남

이런 한계가 있다는 걸 **알면서 Season 2 Ep.1으로 채택한 이유**는 위의 표 한 줄이 설명합니다 — "20 LOC vs 수백 LOC". 학습용 데모에서 잘못된 트레이드오프가 아닙니다.

## 다음 편 예고

- **Ep.3** *(완료)*: ADR-010 Rebalance worker — "정지된 청크를 어떻게 옮기는가" + never-delete 안전 규칙. → [`blog/03-rebalance.md`](03-rebalance.md)
- **Ep.4**: ADR-011 Chunking — 1 GiB 객체를 어떻게 작은 청크로 쪼개고, placement가 chunk 단위로 자연스럽게 확장되는가
- **Ep.5**: ADR-008 Reed-Solomon EC — placement가 (K+M)개 노드를 고르도록 확장. N ≥ K+M 필요 — Season 2의 보스전

## 직접 돌려보기

```bash
git clone https://github.com/HardcoreMonk/kvfs   # 공개 시점 기준
cd kvfs
docker run --rm -v "$PWD:/src" -w /src -e CGO_ENABLED=0 \
  golang:1.26-alpine sh -c 'go build -o ./bin/kvfs-cli ./cmd/kvfs-cli'

./bin/kvfs-cli placement-sim --nodes 5  --chunks 10000 --replicas 3 --add 1
./bin/kvfs-cli placement-sim --nodes 10 --chunks 10000 --replicas 3 --remove 1
./bin/kvfs-cli placement-sim --nodes 20 --chunks 50000 --replicas 3 --add 5
```

`--nodes`, `--chunks`, `--replicas`, `--add`/`--remove`를 바꿔가며 직접 이동률이 R/N에 수렴함을 확인해 보세요. 이론을 손으로 만져 보는 5분이 책 한 챕터보다 남습니다.

## 참고 자료

- 알고리즘 결정 기록: [`docs/adr/ADR-009-consistent-hashing.md`](../docs/adr/ADR-009-consistent-hashing.md)
- 구현: [`internal/placement/placement.go`](../internal/placement/placement.go) (주석 포함 ~190 LOC)
- 테스트: [`internal/placement/placement_test.go`](../internal/placement/placement_test.go) — 7 케이스 (결정성·균등분포·저변동·순서독립)
- CLI: [`cmd/kvfs-cli/main.go`](../cmd/kvfs-cli/main.go) — `placement-sim` 서브커맨드
- 원논문: Thaler & Ravishankar, *A Name-Based Mapping Scheme for Rendezvous*, 1998

---

*kvfs는 교육적 레퍼런스입니다. 프로덕션에 배포하지 마세요. Season 2는 "라우팅이 되면, 다음에 무엇이 깨지는가"를 차례로 보여주는 시즌입니다.*
