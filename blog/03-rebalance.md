# kvfs from scratch, Ep.3 — "정지된 청크"를 옮기는 가장 안전한 알고리즘

> Season 2 · Episode 2. ADR-009가 만든 "새 자리"로, ADR-010이 기존 청크를 데려갑니다. **never-delete** 규칙 하나로 데이터 손실 위험을 0으로 유지하면서.

## Ep.2의 미해결 — 한 줄로

Ep.2(`02-consistent-hashing.md`)에서 우리는 **새 쓰기**가 HRW에 따라 상위 R개 DN으로 가는 걸 시연했습니다. 그러나 demo-zeta의 마지막 메시지가 솔직하게 인정했죠:

> ℹ️ Pre-existing 4 seed chunks remain on dn1/dn2/dn3 only — no auto-rebalance.
> That migration is the job of ADR-010 (Rebalance worker) in a future episode.

이번 episode가 그 약속을 지킵니다. 코드 ~200줄, 안전 규칙 하나, 그리고 5분 라이브 데모.

## 문제 — 또 다시 손으로 따라가기

3 DN 클러스터에 객체 4개를 넣었다고 칩시다 (R=N=3 → 모두 dn1·dn2·dn3에 존재). 메타DB에는:

```
seed-1.txt  → chunk=767764..  replicas=[dn1, dn2, dn3]
seed-2.txt  → chunk=9f8725..  replicas=[dn1, dn2, dn3]
seed-3.txt  → chunk=f349c3..  replicas=[dn1, dn2, dn3]
seed-4.txt  → chunk=3a72d0..  replicas=[dn1, dn2, dn3]
```

이제 dn4를 추가하고 edge가 4-DN 구성으로 다시 떠오릅니다. HRW는 **각 청크에 대해** 상위 3개를 다시 골라줍니다:

```
seed-1: HRW(c=767764, [dn1..dn4]) → [dn1, dn2, dn3]   ← 변화 없음
seed-2: HRW(c=9f8725, [dn1..dn4]) → [dn1, dn3, dn4]   ← dn4 진입, dn2 탈락
seed-3: HRW(c=f349c3, [dn1..dn4]) → [dn1, dn2, dn3]   ← 변화 없음
seed-4: HRW(c=3a72d0, [dn1..dn4]) → [dn1, dn3, dn4]   ← dn4 진입, dn2 탈락
```

**4개 중 2개만 misplaced**입니다. 이론값 R/(N+1) = 3/4 = 75% 와 어긋나 보이지만, 표본 4의 분산 안. Ep.2의 N=10/10000 케이스에서는 29.11% 가 깔끔하게 R/N 에 수렴했죠. 작을수록 흔들림이 큰 것 뿐.

## 안전 규칙 한 줄 — copy-then-update, never delete

마이그레이션을 흔히 떠올리는 방식:
1. 새 자리에 복사
2. 메타 갱신
3. 옛 자리에서 삭제

3단계가 사고의 근원입니다. 복사 후·메타 갱신 후·삭제 전·삭제 도중·삭제 후 — 각 시점에 서버가 죽으면 청크 일부가 영원히 사라질 가능성이 있습니다. "분산 시스템"에서 가장 비싼 한 줄은 **삭제**입니다.

ADR-010의 결정은 단순합니다 — **3단계를 하지 않는다**.

```
1. 새 자리에 복사
2. 메타에 새 자리 추가 (옛 자리는 유지)
   끝.
```

결과: 일시적으로 R+1 또는 R+2 replica를 가진 청크가 디스크에 머뭅니다. 디스크 사용량이 잠시 늘죠. 그 대신:

- **데이터 손실 위험 0** — 마이그레이션 도중 어디서 죽어도 청크는 살아있음
- **멱등성** — 같은 plan 두 번 돌려도 동일 chunk_id PUT은 덮어쓰기 (Content-addressable의 선물)
- **재시도 자동** — 부분 실패 시 다음 run이 자연스럽게 메움

옛 자리 정리(GC)는 **다른 ADR의 책임**입니다 (ADR-012 예상). 한 가지 결정에 한 가지 보장. 분산 시스템 1번 교훈.

## 알고리즘 (실제 코드 거의 그대로)

`internal/rebalance/rebalance.go`의 핵심:

```go
func ComputePlan(coord Coordinator, st ObjectStore) (Plan, error) {
    objs, _ := st.ListObjects()
    plan := Plan{Scanned: len(objs)}
    for _, obj := range objs {
        desired := sortedCopy(coord.PlaceChunk(obj.ChunkID))
        actual  := sortedCopy(obj.Replicas)
        missing := setDiff(desired, actual)   // 복사할 곳
        surplus := setDiff(actual, desired)   // 정보용 (삭제 안 함)
        if len(missing) == 0 { continue }
        plan.Migrations = append(plan.Migrations, Migration{
            Bucket: obj.Bucket, Key: obj.Key, ChunkID: obj.ChunkID,
            Actual: actual, Desired: desired, Missing: missing, Surplus: surplus,
        })
    }
    return plan, nil
}
```

`Run`은 plan을 받아 동시성 N으로 실행:

```go
func migrateOne(ctx context.Context, coord Coordinator, st ObjectStore, mg Migration) (out result) {
    // 1. 살아있는 1곳에서 청크 GET
    data, _, err := coord.ReadChunk(ctx, mg.ChunkID, mg.Actual)
    if err != nil { ... }

    // 2. Missing 의 각 DN 으로 PUT
    successful := append([]string(nil), mg.Actual...)
    for _, dst := range mg.Missing {
        if err := coord.PutChunkTo(ctx, dst, mg.ChunkID, data); err != nil {
            ...continue
        }
        successful = append(successful, dst)
    }

    // 3. 메타 갱신 — actual ∪ successful (옛 자리 유지)
    finalReplicas := uniqueSorted(successful)
    st.UpdateReplicas(mg.Bucket, mg.Key, finalReplicas)
    return
}
```

전체 패키지 ~270줄 (주석 포함). 외부 의존 0개. `crypto/sha256` 도 안 씁니다 — 이미 chunk_id가 그 결과니까.

## 라이브 데모 — `demo-eta.sh`

`./scripts/demo-eta.sh` 한 방으로:

```
=== η demo: rebalance worker (ADR-010) ===

[1/7] Reset cluster from scratch
  bringing up edge with EDGE_DNS=dn1:8080,dn2:8080,dn3:8080
  ✅ 3 DNs + edge up

[2/7] Seed 4 objects on the 3-DN cluster
  seed-1.txt  chunk=767764254a5f2ddf..
  seed-2.txt  chunk=9f872546d77d9dd9..
  seed-3.txt  chunk=f349c3e3341f541e..
  seed-4.txt  chunk=3a72d075a4c02d3e..

[3/7] Add dn4 and restart edge with 4-DN config
  bringing up edge with EDGE_DNS=dn1:8080,dn2:8080,dn3:8080,dn4:8080
  dn4 chunk count after cluster expansion: 0

[4/7] Compute rebalance plan (dry-run)
📋 Rebalance plan — scanned 4 objects, 2 need migration

  demo-eta/seed-2.txt  size=38  chunk=9f872546d77d9dd9..
    actual:  dn1:8080, dn2:8080, dn3:8080
    desired: dn1:8080, dn3:8080, dn4:8080
    missing: dn4:8080
    surplus: dn2:8080  (kept — never delete in MVP)
  demo-eta/seed-4.txt  size=38  chunk=3a72d075a4c02d3e..
    actual:  dn1:8080, dn2:8080, dn3:8080
    desired: dn1:8080, dn3:8080, dn4:8080
    missing: dn4:8080
    surplus: dn2:8080  (kept — never delete in MVP)
```

읽는 법:
- **scanned 4, 2 need migration** — HRW가 dn4를 상위 3에 넣은 청크 = 정확히 2개
- **actual / desired** 가 어디서 어디로 바뀌는지 직접 보여줌
- **surplus = dn2** — dn2는 데이터를 **계속 가지고 있음**. 메타에서 빠졌을 뿐 (다음 GC ADR이 청소할 자료)

```
[5/7] Apply rebalance
⚙️  Rebalance applied (concurrency=4)
   scanned:      4
   planned:      2
   migrated:     2
   failed:       0
   bytes_copied: 76

[6/7] Verify zero migrations remain (idempotency check)
📋 Rebalance plan — scanned 4 objects, 0 need migration
   ✅ Cluster is in HRW-desired state. No work to do.

[7/7] Disk-level confirmation
  dn1  chunk count = 4
  dn2  chunk count = 4
  dn3  chunk count = 4
  dn4  chunk count = 2
```

세 가지 신호:

1. **2 migrated, 0 failed, 76 bytes copied** — 2개 청크 × 38바이트씩 정확
2. **두 번째 --plan 이 0** — 멱등성. 같은 명령 100번 더 돌려도 0
3. **dn4 = 2 chunks**, **dn1/dn2/dn3 = 4 chunks each** — over-replicated 상태가 디스크 레벨로 보임. dn2 는 데이터를 들고 있지만 메타에는 없음 (surplus)

이 3개 신호가 ADR-010의 약속을 그대로 시연합니다.

## 단위 테스트 — 부분 실패까지 포함 8 케이스

`internal/rebalance/rebalance_test.go` 가 검증하는 시나리오:

| 케이스 | 검증 |
|---|---|
| `TestComputePlan_AllOK` | desired == actual 일 때 plan 비움 |
| `TestComputePlan_OneMisplaced` | missing/surplus 정확히 분리 |
| `TestRun_HappyPath_CopiesAndUpdatesMeta` | 정상 흐름 — 디스크 + 메타 |
| `TestRun_PartialCopyFailure_KeepsOldRetriesNext` | dst 일부 실패 시 부분 갱신 |
| `TestRun_SourceUnavailable_FailsCleanly` | 소스 모두 죽으면 깔끔히 실패 카운트 |
| `TestRun_Idempotent_SecondRunZeroMigrations` | 재실행 안전 |
| `TestRun_EmptyPlan_NoOp` | 빈 plan → 즉시 zero stats |
| `TestRun_Concurrency_ParallelBatch` | 20개 청크 × 동시성 8 |

8/8 PASS. 인터페이스 분리(`Coordinator`, `ObjectStore`) 덕에 네트워크·디스크 없이 알고리즘만 검증 가능.

## edge에 추가된 단 두 개의 핸들러

```
POST /v1/admin/rebalance/plan         ← read-only, 안전
POST /v1/admin/rebalance/apply?N=4    ← single-flight mutex 보호
```

`/apply`는 `sync.Mutex`로 직렬화. 두 번째 호출은 첫 번째가 끝날 때까지 대기. 동일 청크에 대한 race 방지의 가장 단순한 답.

```go
// edge.go
func (s *Server) handleRebalanceApply(w http.ResponseWriter, r *http.Request) {
    // ... concurrency 파싱
    s.rebalanceMu.Lock()
    defer s.rebalanceMu.Unlock()
    plan, _ := rebalance.ComputePlan(s.Coord, s.Store)
    stats := rebalance.Run(ctx, s.Coord, s.Store, plan, concurrency)
    writeJSON(w, http.StatusOK, ...)
}
```

운영자가 두 터미널에서 동시에 `--apply`를 쳐도 안전합니다. 첫 번째가 끝난 뒤 두 번째가 새 plan(보통 0개)을 받음.

## CLI — `kvfs-cli rebalance`

```
$ kvfs-cli rebalance --plan        # 미리보기
$ kvfs-cli rebalance --apply       # 실제 실행
$ kvfs-cli rebalance --plan -v     # 모든 항목 상세 출력
$ kvfs-cli rebalance --apply --concurrency 8
```

edge에 HTTP로 붙는 얇은 클라이언트 — 165줄 정도. JSON 디코딩 + 표 출력이 전부.

## 솔직한 한계 — Season 2가 더 풀어야 할 것들

1. **Surplus 청크가 누적** — 위 데모에서도 dn2는 메타에서 빠진 청크를 계속 디스크에 보관. 백그라운드 GC가 필요. → ADR-012 후보
2. **자동 트리거 없음** — DN 추가/제거가 일어나도 운영자가 명시적으로 `--apply` 해야 함. 폴링·DN 변경 감지·정책은 별도 주제 → ADR-013 후보
3. **ListObjects가 한 번에 메모리에 다 올림** — 청크 100만 개 이상에선 페이지네이션·스트리밍 필요
4. **단일 edge에서만 동작** — 동일 메타DB 접근. HA edge 시나리오는 ADR-013(coordinator daemon 분리) 와 함께 풀릴 문제

이런 한계가 있다는 걸 알면서 이번 ADR에 담지 않은 이유는 한 줄로:

> **하나의 결정 = 하나의 보장**

ADR-010은 "데이터 손실 위험 0인 채로 청크를 옮긴다"만 약속합니다. 이 약속을 깨끗이 지키는 가장 단순한 알고리즘이 위 코드입니다. GC, 자동화, 페이징은 각각 별개의 약속.

## 다음 편 예고

- **Ep.4**: ADR-011 Chunking — 큰 객체 분할. rebalance·placement는 chunk 단위로 자연 확장 (한 객체가 100 청크면 placement 결정 100번 + rebalance plan에 100 entry)
- **Ep.5**: ADR-008 Reed-Solomon EC — placement가 (K+M)개 노드 선택. N ≥ K+M 필요. Season 2의 보스전
- **Ep.6 후보**: ADR-012 GC — 이번 episode가 미룬 surplus 청크 청소

## 직접 돌려보기

```bash
git clone https://github.com/HardcoreMonk/kvfs   # 공개 시점 기준
cd kvfs
docker run --rm -v "$PWD:/src" -w /src -e CGO_ENABLED=0 \
  golang:1.26-alpine sh -c 'go build -o ./bin/kvfs-cli ./cmd/kvfs-cli'
docker build -t kvfs-edge:dev --target kvfs-edge .
docker build -t kvfs-dn:dev   --target kvfs-dn   .

./scripts/demo-eta.sh
```

`scripts/demo-eta.sh`는 매번 down → up 으로 시작하므로 클러스터 상태와 무관하게 깨끗한 결과를 줍니다. 출력의 모든 숫자가 위와 일치하지는 않을 겁니다 — chunk_id가 시각 의존(body에 timestamp)이라 매번 달라집니다. 하지만:

- "scanned 4, X need migration" 의 X 는 0~3 사이 (4 모두 misplaced 될 일은 거의 없음)
- 두 번째 --plan은 항상 0
- dn1/dn2/dn3 chunk_count는 항상 4 (over-replicate 보존)
- dn4 chunk_count는 X (마이그레이션된 청크 수)

위 4개 불변식이 ADR-010의 약속과 일치합니다.

## 참고 자료

- 이 ADR: [`docs/adr/ADR-010-rebalance-worker.md`](../docs/adr/ADR-010-rebalance-worker.md)
- 구현: [`internal/rebalance/rebalance.go`](../internal/rebalance/rebalance.go)
- 테스트: [`internal/rebalance/rebalance_test.go`](../internal/rebalance/rebalance_test.go) — 8 케이스
- edge 핸들러: [`internal/edge/edge.go`](../internal/edge/edge.go) `handleRebalance{Plan,Apply}`
- CLI: [`cmd/kvfs-cli/main.go`](../cmd/kvfs-cli/main.go) `cmdRebalance`
- 데모: [`scripts/demo-eta.sh`](../scripts/demo-eta.sh)
- 동기 부여 데모 (Ep.2): [`scripts/demo-zeta.sh`](../scripts/demo-zeta.sh)

---

*kvfs는 교육적 레퍼런스입니다. 프로덕션에 배포하지 마세요. Season 2의 약속 — "라우팅이 되면, 다음에 무엇이 깨지는가" — 가 한 episode 더 이행되었습니다.*
