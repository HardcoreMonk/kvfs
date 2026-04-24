# ADR-009 — Consistent Hashing (Rendezvous / HRW)

## 상태
Accepted · 2026-04-25 · Season 2 진입 첫 결정

## 맥락

Season 1 MVP 의 Coordinator 는 **모든 DN 에 쓰기**:

```go
for _, dn := range c.dns {
    putChunk(dn, ...)
}
```

DN 이 3개일 땐 문제 없음. 그러나 DN 추가·제거 시:
- **추가**: 새 DN 에 기존 청크 없음. 일부 청크 재배치 필요 (누가 누구로?)
- **제거**: 죽은 DN에 있던 청크의 replica 수 부족. 복구 필요
- **확장**: DN 이 10, 20, 100 개로 늘면 "모든 DN 에 쓰기" 불가능 — R 개만 선택해야

"몇 개 DN 에, **어느 DN**에 쓸까?" 가 이 ADR 의 문제.

### 대안 검토

| 방식 | 장점 | 단점 |
|---|---|---|
| **Rendezvous Hashing (HRW)** | 코드 ~20줄, 가상 노드 불필요, 균등 분포 자동 | O(N) per lookup (N 작을 때 무관) |
| 고전 Ring (Dynamo·Cassandra) | O(log N) lookup | Virtual node 100~1000개 필요, 구현 복잡 |
| 중앙 placement DB | 제약 최적화 (rack-aware 등) 가능 | 중앙 SPOF, 복잡도 높음 |
| 무작위 + 메타 기록 | 단순 | DN 변동 시 rebalance 로직 별도 |

kvfs 규모(수십 DN 이하) 에선 **Rendezvous 가 압도적으로 단순** · 검증된 표준(1998년 논문, Ceph CRUSH 의 기초).

## 결정

**Rendezvous Hashing (HRW)** 을 `internal/placement` 패키지로 구현.

### 핵심 공식

```
score(chunkID, nodeID) = uint64(sha256(chunkID + "|" + nodeID)[0:8])
targets(chunkID, R)    = top-R nodes by score
```

### API

```go
package placement

type Node struct { ID, Addr string }
type Placer struct { ... }

func New(nodes []Node) *Placer
func (p *Placer) Pick(chunkID string, n int) []Node
```

### Coordinator 통합

```go
// 이전
for _, dn := range c.dns { putChunk(dn, ...) }

// 지금
targets := c.placer.Pick(chunkID, c.replicationFactor)
for _, n := range targets { putChunk(n.Addr, ...) }
```

`ReplicationFactor` (기본 3) 가 Config 로 추가. `QuorumWrite` 는 R/2+1 기본.

## 결과

### 긍정

- **N → N+1 추가 시 이동률 ≈ R/(N+1)** — 실측 확인:
  - N=5, R=3, +1 추가 → 47.6% 이동 (이론 50%)
  - N=10, R=3, -1 제거 → 29.1% 이동 (이론 30%)
- **결정론적** — (chunkID, 노드목록) 만 같으면 어디서든 같은 선택. 메타 없이 재현 가능
- **코드 단순** — 전체 ~90 LOC, 비전공자 이해 목표 충족
- **외부 의존 0** — Go stdlib `crypto/sha256` + `sort`
- **테스트 7개 PASS** — 결정성·균등분포·저변동·순서독립 전부 커버

### 부정

- **O(N) lookup** — chunkID 1개당 N개 노드와 점수 계산. N=100 이상에서는 미미한 CPU 사용 (마이크로초 단위). N=1000+ 되면 ring 방식 고려
- **Rack awareness 미지원** — 단순 점수 기반이라 "같은 rack 에 replica 2개 있으면 안 됨" 같은 제약 표현 불가. CRUSH 스타일 계층 확장은 별도 ADR
- **기존 데이터 자동 rebalance 없음** — DN 추가·제거 시 **새 쓰기** 는 즉시 올바른 자리에 가지만, **기존 청크** 는 그 자리에 머문다. 명시적 rebalance 작업 필요 (Season 2+ ADR-010 예상)

### 트레이드오프 인정

- Season 1 α 데모 (3 DN · 3-way replication) 에서는 placement 효과 **관찰 불가** — R=N=3 이므로 모든 DN 이 항상 선택됨. 효과는 4+ DN 에서 나타남
- 청크 메타데이터에 저장된 `replicas` 배열은 **쓰기 시점** 의 선택. 이후 DN 변동 후 재조회해도 이 배열은 유효 (해당 DN 들이 살아있다면). placement.Pick 은 **새 쓰기** 에만 사용

## 데모

### CLI — `placement-sim`

```bash
kvfs-cli placement-sim --nodes 10 --chunks 10000 --replicas 3 --remove 1
```

출력 (실측):

```
After removing 1 DN(s) → 9 total
  dn1  3330 chunks (33.3%) │█████████·····················│
  dn2  3321 chunks (33.2%) │█████████·····················│
  ...
📊 Chunks moved: 2911 / 10000 = 29.11%
📐 Theoretical lower bound (R/N): 30.00%
```

이론값과 실측이 일치 — "consistent" 의 경험적 증명.

### α 데모 영향

3 DN 환경에선 동작 변화 없음 (모든 DN 선택). 4+ DN 에서는 자동으로 상위 3개만 선택. 복제 정합성 유지.

## 관련

- `internal/placement/placement.go` — 구현 (주석 포함 ~220 LOC)
- `internal/placement/placement_test.go` — 7 tests, 변동률 테스트 포함
- `internal/coordinator/coordinator.go` — 통합
- `cmd/kvfs-cli/main.go` — `placement-sim` 서브커맨드

## Season 2 예상 후속 ADR

- **ADR-010 — Rebalance worker**: 기존 청크의 자동 재배치 (DN 추가 후 trickle migration)
- **ADR-011 — Chunking**: 대용량 객체 분할 (placement 는 chunk 단위로 자연 확장)
- **ADR-008 — Reed-Solomon EC**: placement 가 (K+M) 개 노드 선택하도록 확장. N ≥ K+M 필요
