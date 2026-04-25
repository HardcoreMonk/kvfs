# ADR-010 — Rebalance Worker (정지된 청크를 desired placement로 옮기기)

## 상태
Accepted · 2026-04-25 · Season 2 Ep.2 첫 결정

## 맥락

ADR-009 (Rendezvous Hashing) 이후, **새 쓰기**는 항상 현재 노드 토폴로지에 맞춰 상위 R개 DN을 고른다. 그러나 `demo-zeta.sh` 가 디스크 레벨로 보여주듯:

- DN 추가 직후, 기존 청크의 메타(`ObjectMeta.Replicas`)는 **쓰기 시점의 DN 주소**를 그대로 가지고 있음
- 새 DN(예: dn4)에는 **기존 청크 0개**. 모든 신규 쓰기에만 참여함
- GET은 여전히 동작 (기존 replica 살아있음). 그러나 **현재 placement.Pick과 actual replicas가 불일치**하는 청크가 누적됨

장기적 영향:
1. **불균등 부하** — 오래 살아남은 DN일수록 더 많은 청크 보관
2. **장애 대비 약화** — DN 1개가 영구 사망하면, 그 DN에 있던 청크의 replica 수가 R 미만으로 떨어진 채 방치됨
3. **placement 변경 효과 소실** — DN 추가의 의미가 "신규 트래픽 분담"으로 한정됨

이 ADR은 "**기존 청크를 어떻게 desired placement로 이사시키는가**"를 정의한다.

### 대안 검토

| 방식 | 장점 | 단점 |
|---|---|---|
| **Edge 내장 admin 잡 (이번 ADR)** | 추가 daemon 없음, 현재 메타·coordinator 재활용 | 대규모 마이그레이션 시 edge에 부하 집중 |
| 별도 rebalance daemon | edge 부하 격리, 독립 스케일 | 새 바이너리·배포·메타 접근 권한 설계 필요 |
| DN→DN P2P 전송 | edge 대역폭 절약 | DN 간 인증·경로 설계 필요 |
| Read-time lazy migration | 별도 잡 불필요 | Cold 청크 영원히 미이동, GET 레이턴시 증가 |
| 자동 백그라운드 polling | 운영자 개입 불필요 | "언제 시작/멈추나?" 결정·관찰성 부담 |

kvfs 규모(수십 DN, 수만 청크)에서 **edge 내장 + 명시적 트리거**가 압도적으로 단순하다. P2P·전용 daemon은 Season 3+ 주제.

## 결정

### 핵심 모델

각 청크 메타에 대해 두 집합 비교:

```
desired = set(coord.PlaceChunk(chunkID))   # 지금 다시 계산
actual  = set(meta.Replicas)               # 메타에 적힌 사실
```

상태 분기:

| 상태 | 조건 | 액션 |
|---|---|---|
| **OK** | desired == actual | 무시 |
| **Misplaced** | desired ≠ actual & len(actual) ≥ R | 누락 DN으로 복사 후 메타 갱신 (overshoot 허용) |
| **UnderReplicated** | len(actual) < R | 동일 — 누락 DN 복사 |
| **Orphan** | actual에만 있고 desired에 없는 DN | **이번 단계에서는 삭제 안 함**. 다음 GC 패스의 일 (별도 ADR 또는 future P2) |

"copy-then-update-metadata, never delete" 규칙. **항상 over-replicate 방향으로만 움직인다** — 데이터 손실 위험을 0으로 유지.

### 알고리즘 (`rebalance.Plan` + `rebalance.Run`)

```
Plan(coord, store):
  for obj in store.ListObjects():
    desired = set(coord.PlaceChunk(obj.ChunkID))
    actual  = set(obj.Replicas)
    missing = desired - actual                # 복사해야 할 DN
    surplus = actual - desired                # 정보용 — 삭제 안 함
    if missing: emit Migration{obj, source∈actual, missing, surplus}

Run(plan, concurrency):
  semaphore = make(chan struct{}, concurrency)
  for m in plan.Migrations:
    acquire(semaphore)
    go:
      data, _ := coord.ReadChunk(ctx, m.ChunkID, [m.Source])  # 살아있는 1곳에서 GET
      for dst in m.Missing:
        coord.PutToOne(ctx, dst, m.ChunkID, data)             # PUT
      newReplicas = unique(m.Actual ∪ m.Missing)              # over-replicated
      store.PutObject(meta with Replicas=newReplicas)
      release(semaphore)
```

복사 실패 정책:
- **부분 실패** (일부 dst 성공, 일부 실패): 성공한 dst만 메타에 추가. 실패한 dst는 다음 run에서 재시도
- **소스 실패** (모든 actual DN GET 실패): 청크 자체가 위험 상태. 이 경우는 ADR-010 범위 밖 — 별도 alert + 수동 개입 (Season 3 repair queue)

### API 표면

**internal/rebalance** 패키지 (pure logic):
```go
type Migration struct {
    Bucket, Key, ChunkID string
    Actual, Desired      []string  // 정렬된 슬라이스
    Missing              []string  // desired - actual
    Surplus              []string  // actual - desired (정보용)
}

type Plan struct {
    Scanned    int
    Migrations []Migration
}

func ComputePlan(coord, store) (Plan, error)

type RunStats struct {
    Migrated, Failed int
    BytesCopied      int64
    Errors           []string
}

func Run(ctx, coord, store, plan, concurrency) RunStats
```

**Coordinator 추가 메서드** (PUT을 단일 DN에 보내는 헬퍼):
```go
func (c *Coordinator) PutChunkTo(ctx, addr, chunkID, data) error
```

**Edge admin 엔드포인트**:
- `POST /v1/admin/rebalance/plan` — JSON으로 Plan 반환 (no side effect, dry-run)
- `POST /v1/admin/rebalance/apply?concurrency=4` — Plan 계산 후 Run, JSON으로 RunStats 반환

**CLI**:
```
kvfs-cli rebalance --edge http://localhost:8000 --plan
kvfs-cli rebalance --edge http://localhost:8000 --apply --concurrency 4
```

## 결과

### 긍정

- **데이터 손실 위험 0** — copy-then-update + never-delete
- **재실행 안전 (idempotent)** — 같은 plan 반복 실행해도 over-replicate 방지 (이미 있으면 스킵 또는 멱등 PUT)
- **메타 = 진실** — 메타가 가장 최신. GET 경로는 변경 없음 (기존 코드 그대로)
- **추가 의존 0** — coordinator + store + placement만 사용
- **단순 — ~150 LOC 예상** — 비전공자 이해 목표 충족

### 부정

- **Surplus 청크 누적** — DN 추가 후 rebalance 만 돌리면 over-replicated 청크가 디스크 점유. 별도 GC 패스 필요 (현 ADR 범위 밖)
- **단일 edge 직렬 실행** — 동일 edge 인스턴스에서 두 번 동시에 apply 호출 시 동작 미정의. **First-mover 락 또는 single-flight** 필요 → 구현에서 단순 mutex 추가
- **메타 stale 윈도우** — Run 중간에 다른 PUT이 같은 객체를 덮어쓰면 race. 현 MVP는 PutObject가 마지막 승자가 되므로 데이터는 안전, 단 새 replicas 정보가 삭제될 수 있음 (다음 rebalance가 다시 잡음). **알고리즘적으로 eventual consistency**
- **대규모 마이그레이션 미고려** — 청크 100만 개+에서는 메모리 사용량·진행률 표시·resumability 필요. MVP에서는 ListObjects 일괄 로드. Season 3+ 주제

### 트레이드오프 인정

- "왜 over-replicate?" — 일시적 디스크 추가 사용을 받아들이는 대신 마이그레이션 도중 서버 죽어도 청크가 살아남음. 교과서적 안전 우선
- "왜 자동 트리거가 아닌가?" — 운영자가 "지금이 안전" 판단 후 명시 실행이 더 단순. 자동화는 알림·정책·관찰성 한 묶음이라 별도 ADR 후보
- "왜 surplus 삭제 안 함?" — 삭제는 매우 다른 보장 (ref counting, 다른 객체와의 청크 공유 — ADR-005 dedup 참고). GC는 그 자체로 ADR감

## 데모 시나리오 (`scripts/demo-eta.sh`, 후속 작업)

```
1. ./scripts/up.sh                   # 3 DN
2. PUT 4 seed objects                # 모두 dn1/2/3 에
3. dn4 추가 + edge 재시작            # demo-zeta 와 동일 setup
4. PUT 4 추가 객체 (분산 확인)
5. kvfs-cli rebalance --plan         # 4 seed 청크가 misplaced 로 보임
6. kvfs-cli rebalance --apply        # dn4 로 복사, 메타 갱신
7. 다시 --plan → 0 migrations         # consistent 상태 도달
8. dn4 디스크 검사 → seed 청크들도 보유
```

## 관련

- ADR-009 — Consistent Hashing (이 ADR의 desired placement 계산 출처)
- ADR-005 — Content-addressable chunks (chunk_id 가 변하지 않으므로 idempotent PUT 가능)
- `internal/rebalance/` — 구현 위치 (후속 커밋)
- `scripts/demo-zeta.sh` — 이 ADR의 동기 부여 데모 (rebalance가 풀어야 할 갭 시연)
- `scripts/demo-eta.sh` — 이 ADR의 시연 (후속)
- `blog/03-rebalance.md` — 이 ADR의 narrative episode (후속)

## 후속 ADR 예상

- **ADR-012 — Garbage Collection / Surplus Reaper**: rebalance 후 누적된 over-replicated 청크 정리
- **ADR-013 — Auto-trigger policy**: edge 시작·DN 변경 감지 시 자동 rebalance 정책
- **ADR-011 — Chunking**: 큰 객체 분할. rebalance는 chunk 단위 그대로 적용 가능 (자연 확장)
