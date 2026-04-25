# ADR-012 — Surplus Chunk GC (rebalance 가 남긴 잉여 청크 청소)

## 상태
Accepted · 2026-04-26 · Season 2 Ep.3 첫 결정

## 맥락

ADR-010 (Rebalance worker) 의 안전 규칙은 한 줄이었다:

> **copy-then-update, never delete**

덕분에 rebalance 도중 어떤 시점에 죽어도 데이터 손실이 없다. 그러나 `demo-eta.sh` 의 마지막 출력이 솔직하게 인정한 부작용:

```
[7/7] Disk-level confirmation
  dn1  chunk count = 4
  dn2  chunk count = 4   ← 이제 메타가 dn2 를 안 가리키는 청크도 들고 있음
  dn3  chunk count = 4
  dn4  chunk count = 2
```

dn2 가 가진 청크 4개 중 2개는 **메타에서 빠졌다**. `meta.Replicas = [dn1, dn3, dn4]` 인데 dn2 디스크에 그 청크 데이터가 남아 있다. 이것이 surplus.

문제:
1. **디스크 사용량 누적** — DN 추가·이동을 반복할수록 surplus 가 쌓인다
2. **불일치한 진실 소스** — "dn2 디스크에 chunk-X 가 있는데 메타는 모른다" 는 운영자 직관과 어긋남
3. **dedup 효과 약화** — 같은 chunk_id 가 의도치 않게 5개, 6개 DN 에 흩뿌려져 있는 상태가 누적

해결책: 별도의 **GC 패스** 가 메타와 DN 디스크를 동기화. 단, **데이터 손실 위험은 여전히 0** 이어야 한다.

### 대안 검토

| 방식 | 장점 | 단점 |
|---|---|---|
| **별도 GC 패스 (이번 ADR)** | rebalance 와 책임 분리, 자체 안전망 보유 | 운영자가 명시 트리거 필요 |
| Rebalance 끝에 즉시 delete | 한 트랜잭션 | rebalance 의 "데이터 손실 0" 약속을 깨트릴 위험 |
| Reference counting | 메타 갱신 시 즉시 삭제 가능 | 스키마 변경, 카운터 불일치 시 영구 누락 |
| Tombstone + 지연 삭제 | 분산 친화 | tombstone 메타 도입 → 복잡도 ↑ |
| Background sweeper | 운영자 개입 0 | 정책·관찰성·실패 알림 별도 ADR |

GC 의 책임은 rebalance 의 책임과 다르다 — 한 ADR 한 약속 원칙(ADR-010 의 결론) 을 따른다.

## 결정

### 핵심 모델 — 두 단계 안전망

```
claimed = { (chunk_id, addr) : ∀ obj ∈ meta, ∀ a ∈ obj.Replicas }
            ↑ 메타가 "있어야 한다" 고 주장하는 모든 (청크, DN) 페어

for each DN d:
    on_disk = d.GET /chunks   →  [{id, mtime, size}, ...]
    for each c in on_disk:
        if (c.id, d.addr) ∈ claimed:    continue   # 메타가 보호 중
        if now - c.mtime < min_age:     continue   # 신선해서 보호 (race 방지)
        emit Sweep{addr=d.addr, id=c.id, size=c.size, age=now-c.mtime}
```

### 두 안전망의 의미

1. **claimed-set 안전망** — 메타가 단 하나라도 (chunk_id, addr) 페어를 들고 있으면 절대 삭제 안 함. dedup 친화적: 같은 chunk_id 가 다른 객체의 replica 로도 등장하면 모두 보호됨
2. **min-age 안전망** — 청크 mtime 기준 N 초 이내면 보호. **race 방지**:
   - 시나리오: GC 가 disk list 를 찍은 직후, 새 PUT 이 DN 에 도달. 메타 갱신은 PUT 직후 일어남. 이 PUT 의 chunk_id 가 우연히 GC 대상이었다면?
   - min-age 가 그 PUT 을 보호. 기본값 60초 면 정상 PUT/메타 갱신 모두 미일치 윈도우보다 훨씬 김

이 두 안전망은 **AND** 로 결합한다. 한쪽이라도 보호하면 삭제 안 함.

### 알고리즘 (`gc.ComputePlan` + `gc.Run`)

```
ComputePlan(coord, store, minAge):
    # 1. 메타 스냅샷 → claimed set
    objs := store.ListObjects()
    claimed := buildClaimedSet(objs)              # map[chunkID]map[addr]bool

    # 2. 각 DN 의 on-disk 청크 enumerate
    plan := Plan{Scanned: 0, Sweeps: nil}
    for each addr in coord.DNs():
        chunks := coord.ListChunks(ctx, addr)     # [{id, mtime, size}]
        plan.Scanned += len(chunks)
        for each c in chunks:
            if claimed[c.id][addr]:    continue
            age := now - c.mtime
            if age < minAge:           continue
            plan.Sweeps = append(plan.Sweeps, Sweep{addr, c.id, c.size, age})
    return plan

Run(coord, plan, concurrency):
    sem := make(chan struct{}, concurrency)
    for each s in plan.Sweeps:
        acquire(sem)
        go:
            err := coord.DeleteChunkFrom(ctx, s.Addr, s.ID)
            release(sem)
            record(err)
```

### 명시적 비범위

- **메타 손상 복구** — claimed set 이 잘못되면 (메타가 청크를 잃었다면) 그 청크는 GC 대상이 됨. 이건 GC 의 책임이 아님. 메타 백업·재구축은 별도 ADR 후보
- **DN 가 모두 죽어 있을 때** — claimed 가 비어 있으면 모든 청크가 후보. 안전망은 작동하지만 의도치 않은 대량 삭제 가능. CLI 가 "전체 청크의 X% 가 sweep 대상" 임을 표시하고 운영자 확인 필요 → MVP 에서는 단순 비율 출력
- **Cross-DN 중복 검사** — 같은 chunk_id 가 의도치 않게 5개 DN 에 있어도 메타가 [dn1, dn2, dn3] 만 가리키면 dn4·dn5 청크는 surplus 로 삭제됨. 이는 의도된 동작 (dedup 정상화)
- **삭제 후 메타 갱신** — claimed set 만 보고 삭제하므로 메타는 안 건드림. Rebalance 가 메타를 정답으로 만든 후 GC 가 디스크를 정답에 맞춤

## 결과

### 긍정

- **데이터 손실 위험 0** — 메타가 claim 한 모든 청크는 보호. min-age 가 race window 까지 막음
- **재실행 안전 (idempotent)** — 같은 plan 두 번 돌리면 두 번째는 0 sweeps (이미 삭제됨)
- **rebalance 와 직교** — 서로 메타·디스크 한쪽씩만 건드림. 동시 실행도 안전
- **dedup 친화** — claimed set 에서 chunk_id 단위가 아닌 (chunk_id, addr) 단위 매칭
- **추가 의존 0** — DN 에 list 엔드포인트 1개 추가가 전부

### 부정

- **메타 단일 진실 가정** — bbolt 메타가 정확하지 않으면 GC 가 의도치 않게 삭제. 메타 백업·HA 는 별도 주제
- **scan 비용 O(전체 청크 × DN)** — 청크 100만 개 × 10 DN = 1000만 회 lookup. 메모리 hash map 으로는 마이크로초지만 디스크 enumerate 는 IO 비용. MVP 에서는 OK, 대규모는 페이징 필요
- **min-age 가 운영 불편 유발 가능** — 60초 기본값이면 매우 짧은 cycle (rebalance 직후 GC) 시 일부 청크 보호. 운영자가 `--min-age 0` 으로 강제 가능 (위험 인지 필요)

### 트레이드오프 인정

- "왜 자동 트리거 없나?" — rebalance 와 같은 이유. 정책·알림·관찰성은 별도 ADR (ADR-013 후보)
- "왜 ref counting 안 쓰나?" — 스키마 변경 + 카운터 불일치 시 영구 누락 위험. claimed set 은 매번 재계산이라 자기 치유적

## 데모 시나리오 (`scripts/demo-theta.sh`)

```
1. ./scripts/demo-eta.sh       # rebalance 까지 끝낸 상태 (surplus 존재)
2. kvfs-cli gc --plan           # surplus 청크 N개 식별 (예: dn2 의 2개)
3. kvfs-cli gc --apply --min-age 5
4. 디스크 검사 → dn2 chunk count 가 줄어들고 메타 의도와 일치
5. kvfs-cli gc --plan 다시      # 0 sweeps (멱등성)
```

## 관련

- ADR-010 — Rebalance worker (이 ADR 의 동기 — surplus 발생 원인)
- ADR-005 — Content-addressable chunks (chunk_id 안정성, 같은 id 면 같은 데이터)
- `internal/gc/` — 구현 위치 (후속 커밋)
- `internal/dn/dn.go` `handleListChunks` — DN list 엔드포인트
- `scripts/demo-theta.sh` — 시연
- `blog/04-gc.md` — narrative episode

## 후속 ADR 예상

- **ADR-013 — Auto-trigger policy**: rebalance + GC 자동화 정책
- **ADR-014 — Meta backup/HA**: bbolt 메타 손실 시 복구
