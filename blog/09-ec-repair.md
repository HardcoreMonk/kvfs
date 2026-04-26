# kvfs from scratch, Ep.9 — dn5+dn6 영구 사망에도 데이터가 살아남는 이유

> Season 3 · Episode 3. ADR-008 의 EC 가 약속한 "K of K+M survives = recoverable" 을 운영 경로에서 활용. ADR-024 가 못 풀던 dead-source 한계를 같은 stripe 의 4 surviving shards 로 푼다.

## ADR-024 가 닫지 못한 갭

Ep.8 (`demo-mu.sh`) 의 끝에 작은 활자로 인정한 한계:

```go
data, _, err := coord.ReadChunk(ctx, ChunkID, []string{OldAddr})
if err != nil {
    // dn5 dead → 다음 cycle 도 같은 fail
    return failed
}
```

ADR-024 stripe rebalance 는 shard 의 OldAddr 가 살아있을 때만 동작. dn5 가 영구 사망 + 운영자가 ADR-027 으로 registry 에서 제거하면:

```
desired_set = {dn1..dn4, dn7, dn8}  ← dn5/dn6 빠진 새 placement
actual_set  = {dn1..dn6}            ← 메타에 남아있는 옛 위치
              ↑ shard 들이 dn5, dn6 에 있다고 메타가 주장
              
ReadChunk(shard.ChunkID, [dn5]) → 즉시 fail
```

데이터는 같은 stripe 의 다른 4 shard 에 살아있는데, ADR-024 의 read-1-write-1 패턴으론 그 K 개 survivors 를 모아 재구성하는 길이 없음.

EC 의 본래 약속이 회복 경로에서 활용되지 않는 상태.

## ADR-025 의 답 — 같은 stripe, 다른 4 shards

```
for each EC stripe:
    survivors = shards whose Replicas[0] is in current cluster DNs
    if len(survivors) >= K:
        # 1. Fetch K survivors → 메모리에 K shards
        # 2. reedsolomon.Reconstruct → 모든 데이터 shard 위치가 채워짐
        # 3. dead shard 가 parity 자리면 Encode 로 parity 재계산
        # 4. 각 dead shard 의 NewAddr 에 PUT (sorted unused desired DN)
        # 5. 메타 갱신: shard.Replicas = [NewAddr]
```

핵심 통찰 두 개:

**(a) chunk_id 보존** — content-addressable (sha256) 이라 reconstruct 결과의 sha256 = 원본 sha256. 메타에 저장된 ChunkID 그대로 유효 → DN PUT 의 idempotent integrity check 통과 (`if got_sha != id { reject }` 가 그대로 통과).

**(b) "dead" 판정 = registry 외부** — 운영자가 `kvfs-cli dns remove dn5:8080` (ADR-027) 명시. health-check / heartbeat 인프라 없이도 명확한 의도 표현. False-positive 위험 0.

## 코드의 30줄 핵심

`internal/repair/repair.go` 의 `repairStripe`:

```go
func repairStripe(ctx, coord, st, rep) (bool, int64, error) {
    enc, _ := reedsolomon.NewEncoder(rep.K, rep.M)
    n := rep.K + rep.M
    shards := make([][]byte, n)

    // 1. K survivors → shards (rest nil)
    for i := 0; i < rep.K; i++ {
        s := rep.Survivors[i]
        data, _, _ := coord.ReadChunk(ctx, s.ChunkID, []string{s.Addr})
        shards[s.ShardIndex] = data
    }

    // 2. Reed-Solomon Reconstruct → fills nil data positions (0..K-1)
    enc.Reconstruct(shards)

    // 3. dead 가 parity 자리면 Encode 로 M parity 재계산
    if anyDeadIsParity(rep) {
        parity, _ := enc.Encode(shards[:rep.K])
        for i := 0; i < rep.M; i++ { shards[rep.K+i] = parity[i] }
    }

    // 4. 각 dead shard PUT + 메타 갱신
    for _, d := range rep.DeadShards {
        coord.PutChunkTo(ctx, d.NewAddr, d.ChunkID, shards[d.ShardIndex])
        st.UpdateShardReplicas(rep.Bucket, rep.Key, rep.StripeIndex, d.ShardIndex, []string{d.NewAddr})
    }
    return true, ...
}
```

ADR-008 (Reed-Solomon) 의 `Encoder.Reconstruct` 와 `Encoder.Encode` 가 그대로 재사용됨. 그 위에 fetch + place + meta-update 의 30 줄.

## 테스트가 검증한 8 케이스

`internal/repair/repair_test.go`:

| 케이스 | 검증 |
|---|---|
| `TestComputePlan_HealthyStripe_NoRepair` | 모든 shard 가 cluster DN 에 있을 때 plan 비움 |
| `TestComputePlan_OneDeadShard_Repairable` | dn5 제거 → 1 dead shard, 5 survivor, K=4 충분 → repair plan |
| `TestComputePlan_TwoDeadShards_BothMustRepair` | dn5+dn6 제거 → 2 dead, 4 survivor (= K), repair 가능. NewAddr sorted unused 순서 |
| `TestComputePlan_TooManyDead_Unrepairable` | survivor < K → Unrepairable 분류 (data-loss alert) |
| `TestRun_HappyPath_ReconstructsAndUpdates` | 실제 RS Reconstruct + PUT + 메타 갱신 round-trip |
| `TestRun_SurvivorReadFailure` | 중도에 dn1 까지 죽으면 → meta 무변경, retry 가능 |
| `TestRun_Idempotent_SecondRunIsNoop` | 같은 plan 두 번 → 두 번째 0 |
| `TestComputePlan_NoECObjects_NoRepairs` | replication-only 객체는 scan 안 함 (EC 만 처리) |

8/8 PASS. 인터페이스 분리 (Coordinator, ObjectStore) 로 fake 주입 — 네트워크/디스크 없이 알고리즘 검증.

## 라이브 데모 — `demo-nu.sh`

```
=== ν demo: EC repair queue (ADR-025) ===

[1/8] Reset cluster + start 8 DNs (6 active + 2 spare)
[2/8] Generate 131072-byte random body
[3/8] PUT with X-KVFS-EC: 4+2 (2 stripes × 6 shards)

  Disk before kill:
  dn1  shards = 2    dn5  shards = 1
  dn2  shards = 1    dn6  shards = 2
  dn3  shards = 2    dn7  shards = 1
  dn4  shards = 1    dn8  shards = 2

[4/8] Kill dn5 + dn6 + remove from DN registry (simulate permanent loss)
6 DN(s)  quorum_write=2
  dn1:8080  dn2:8080  dn3:8080  dn4:8080  dn7:8080  dn8:8080
```

dn5 + dn6 제거 후 cluster 가 6 DN.

```
[5/8] kvfs-cli repair --plan
🔧 Repair plan — scanned 2 EC stripe(s)
   repairable:    2
   unrepairable:  0

First 2 repair(s):
  demo-nu/ec-repair.bin stripe=0  alive=4  dead=2
    shard[2]  size=16384  dn5:8080 → dn2:8080
    shard[4]  size=16384  dn6:8080 → dn7:8080
  demo-nu/ec-repair.bin stripe=1  alive=5  dead=1
    shard[0]  size=16384  dn6:8080 → dn4:8080
```

각 stripe 의 dead shard 식별 + 새 destination (sorted unused desired):
- stripe 0: shard[2]@dn5 → dn2, shard[4]@dn6 → dn7  (4 alive, K=4 = 정확히 임계치)
- stripe 1: shard[0]@dn6 → dn4 (5 alive)

```
[6/8] kvfs-cli repair --apply
🔧 Repair applied (concurrency=2)
   scanned:         2
   planned:         2
   unrepairable:    0
   repaired:        2
   failed:          0
   bytes_written:   49152
```

3 shards × 16 KiB = 49152 bytes 정확히 일치.

```
  Disk after repair:
  dn1  shards = 2
  dn2  shards = 2    ← +1 (stripe 0 shard[2])
  dn3  shards = 2
  dn4  shards = 2    ← +1 (stripe 1 shard[0])
  dn7  shards = 2    ← +1 (stripe 0 shard[4])
  dn8  shards = 2

[7/8] GET still works — body integrity check
  ✅ GET sha256 matches → EC stripe successfully repaired

[8/8] Idempotency: second --plan = 0 repairs
   ✅ All EC stripes healthy.
```

세 가지 신호:

1. **repaired 2, failed 0, 49152 bytes** — 3 shards × 16 KiB 정확히 매칭
2. **GET sha256 일치** — Reconstruct 가 정확히 원본 데이터 재현
3. **두 번째 plan = 0** — 멱등성, 재실행 무위 (이미 dn5/dn6 가 placement 에서 빠지고 새 위치가 desired)

이 세 신호가 ADR-025 의 약속을 그대로 시연.

## ADR-024 + ADR-025 의 직교성

| | rebalance (024) | repair (025) |
|---|---|---|
| 트리거 | desired ≠ actual, OldAddr 살아있음 | shard 의 OldAddr 가 cluster 에서 빠짐 |
| Source | OldAddr 의 그 shard | 같은 stripe 의 K survivors |
| 비용 | shard 1 개 데이터 read | K × shard 데이터 read + Reconstruct |
| 실패 시 | OldAddr 죽음 → 무력 (이게 repair 의 영역) | survivor < K → unrepairable alert |

운영자 흐름:
1. `dns remove dn5:8080` → registry 에서 제거
2. `rebalance --apply` → ADR-024 시도. 일부 stripe 는 ReadChunk(dn5) fail 로 failed 카운트
3. `repair --apply` → ADR-025 가 fail 한 것들 처리, K survivors 로 reconstruct

또는 단순히 `repair --apply` 만 호출 (rebalance 가 안 풀린 것을 한 번에 푼다).

## ADR-013 (auto-trigger) 와의 미통합 — 의도

이번 ADR 은 운영자 수동 호출 only. 자동화 안 하는 이유:
- **Unrepairable 처리 정책 부재** — 자동으로 돌릴 때 unrepairable 이 발견되면 어떻게 할지 (alert? log? page?) 가 별도 결정
- **K 배 read traffic** — 자동 cycle 이 무차별로 돌면 cluster 부하

ADR-013 보강 episode 또는 ADR-031 등 후속에서 정책 + alert 인프라와 함께.

## 솔직한 한계

1. **K 배 read traffic** — 100 stripes 영향이면 100 × K reads. survivors 가 균등 분포면 부하 분산이지만, 한 DN 에 몰리면 hotspot
2. **Unrepairable = 운영자 책임** — survivor < K 면 backup/외부 경로 필요. 이 ADR 은 alert 만
3. **mid-repair 중 더 많은 DN 사망** — 다음 cycle retry 가능 (idempotent: chunk_id 보존). 그러나 retry 도중 추가 사망 = 데이터 손실 위험
4. **Replication 모드 미적용** — chunk 의 모든 R replica DN 이 동시 죽으면 복구 불가. 이는 EC 만의 약속이지 replication 의 약속이 아님

이런 한계를 알면서 한 episode 로 끝낸 이유는 같은 원칙:

> **하나의 결정 = 하나의 보장**

ADR-025 는 "EC stripe 에서 K survivors 가 있으면 dead shard 를 reconstruct 한다" 만 약속. 자동화, alert 정책, replication repair 는 별개 약속.

## Season 3 운영성 트랙 — 5/?

| Ep | ADR | 주제 |
|---|---|---|
| 7 | 013 | Auto-trigger (시간 기반) |
| 8 | 024 | EC stripe rebalance (set-based) |
| 9 (this) | 025 | EC repair (Reed-Solomon Reconstruct) |
| ?  | 027 | Dynamic DN registry (P2-05) |
| ?  | 028 | UrlKey kid rotation (P2-08) |
| ?  | 029 | Optional TLS / mTLS (P2-09) |

P2 트랙은 별도지만 운영성 본질은 같음. blog 에는 향후 통합 정리 episode 가능.

## 다음 편 예고

- **Ep.10 후보**: ADR-022 Multi-edge leader election — single-edge 가정 깨기. ADR-013/024/025/027/028 모두 단일 edge 에서 동작 가정 → multi-edge 시 동기화 필요
- **Ep.11 후보**: ADR-014 Meta backup/HA — bbolt 메타 손상 시 복구
- **Ep.12 후보**: ADR-030 DN heartbeat — registry-removal 외 자동 dead 감지

## 직접 돌려보기

```bash
git clone https://github.com/HardcoreMonk/kvfs   # public 시점 기준
cd kvfs
docker build -t kvfs-edge:dev --target kvfs-edge .
docker build -t kvfs-dn:dev   --target kvfs-dn   .
docker run --rm -v "$PWD:/src" -w /src -e CGO_ENABLED=0 \
  golang:1.26-alpine sh -c 'go build -o ./bin/kvfs-cli ./cmd/kvfs-cli'

./scripts/demo-nu.sh
```

매번 chunk_id / sha256 / 어느 DN 이 어느 shard 받는지 다릅니다. 그러나 항상:

- 8 DN cluster, dn5+dn6 제거 후 6 DN
- repair plan 의 dead shard 수 = (각 stripe 가 dn5+dn6 에 가졌던 shard 합)
- GET sha256 == PUT sha256
- 두 번째 plan = 0
- bytes_written = dead shards × shard_size

위 5 불변식이 ADR-025 의 약속과 일치.

## 참고 자료

- 이 ADR: [`docs/adr/ADR-025-ec-repair.md`](../docs/adr/ADR-025-ec-repair.md)
- 구현: [`internal/repair/repair.go`](../internal/repair/repair.go)
- 테스트: [`internal/repair/repair_test.go`](../internal/repair/repair_test.go) — 8 케이스
- edge handler: [`internal/edge/edge.go`](../internal/edge/edge.go) `handleRepair{Plan,Apply}`
- CLI: [`cmd/kvfs-cli/main.go`](../cmd/kvfs-cli/main.go) `cmdRepair`
- 데모: [`scripts/demo-nu.sh`](../scripts/demo-nu.sh)
- 의존: ADR-008 (Reed-Solomon) + ADR-024 (rebalance) + ADR-027 (dynamic DN)
- 이전 episode (rebalance): [`blog/08-ec-rebalance.md`](08-ec-rebalance.md)

---

*kvfs는 교육적 레퍼런스입니다. 프로덕션에 배포하지 마세요. ADR-008 의 수학이 ADR-025 의 운영성으로 연결됨 — "K of K+M survives → recoverable" 이 추상적 보장에서 데이터를 실제로 살리는 명령으로.*
