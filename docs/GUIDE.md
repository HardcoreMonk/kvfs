# kvfs 아키텍처 가이드

> 분산 object storage 가 **작동하는 원리**를 따라가는 가이드.
> 짧은 reference 가 필요하면 [`ARCHITECTURE.md`](ARCHITECTURE.md) — 이 문서는 **읽으며 따라가는 walkthrough**.

---

## 0. 누구를 위한 문서인가

- 분산 storage 가 어떻게 만들어지는지 **코드를 읽으며** 이해하고 싶은 개발자
- Ceph · MinIO · S3 의 동작을 작은 모델로 **재현**해보고 싶은 학생
- 오너십·일관성·운영성의 트레이드오프를 **체감**하고 싶은 SRE

전제 지식: HTTP, hash 함수, goroutine, `bbolt`/SQLite 같은 KV 저장소가 무엇인지.
필요 없는 것: Ceph CRUSH, Raft 논문 정독, Reed-Solomon 수학.

---

## 1. 한 장 요약

```
        ┌──────────┐                                       ┌──────────┐
Client ─┤  HTTP    ├──► kvfs-edge (×1 또는 N) ──HTTP REST──┤ kvfs-dn  │ ×N
        │ + UrlKey │       │                               │  (chunk) │
        └──────────┘       │                               └──────────┘
                           ├─ placement (HRW)
                           ├─ chunker (fixed | CDC) → EC encoder
                           ├─ rebalance · GC · repair
                           ├─ heartbeat · auto-snapshot · WAL
                           └─ bbolt: objects · dns_runtime · urlkey_secrets
```

- **2 종류의 daemon**: `kvfs-edge` (게이트웨이 + 인라인 coordinator) · `kvfs-dn` (스토리지 노드).
- **객체 모델**: replication (default 3-way) **또는** Reed-Solomon EC — `X-KVFS-EC: K+M` 헤더로 PUT 마다 선택.
- **메타데이터**: `bbolt` 단일 파일. `objects` 버킷이 single source of truth.
- **HA**: 멀티 edge — primary 1 + read-replica N (snapshot pull) 또는 Raft-style auto-elected leader.
- **운영성**: edge 가 자기 cluster 의 정렬·청소·복구·snapshot·heartbeat 를 **스스로** 한다.

---

## 2. 기본 구조

### kvfs-edge — 게이트웨이

- 포트: `:8000` (HTTP 또는 HTTPS, ADR-029).
- 역할: 클라이언트 요청 받음 → URL 서명 검증 → chunker/EC 처리 → placement → DN fanout → 메타 저장.
- 상태 파일: `<EDGE_DATA_DIR>/edge.db` (bbolt) + 선택적 `<EDGE_SNAPSHOT_DIR>/` + 선택적 WAL 파일.
- 외부 의존: DN 들. 그 외 0 (no etcd, no zookeeper, no DB).

### kvfs-dn — 스토리지 노드

- 포트: `:8080` (HTTP 또는 HTTPS).
- 역할: chunk 단위 PUT/GET/DELETE. 한 chunk = 한 파일.
- 상태 파일 경로: `<DN_DATA_DIR>/chunks/<sha256[0:2]>/<rest-of-sha>`. 첫 2자리 prefix 로 디렉토리 분산.
- 외부 의존: 0. DN 끼리 서로 모름.

### 데이터 비대칭

> **edge 가 똑똑하고, dn 은 멍청하다.** dn 은 자기가 어느 object 의 일부인지 모르고, 어느 DN 들과 한 stripe 인지도 모른다. 모든 의사결정은 edge 가 한다.

이 비대칭이 kvfs 단순함의 핵심이다. dn 이 "self-organizing" 이 아니라, edge 가 placement 알고리즘으로 **결정** 하고 dn 은 byte container 일 뿐.

---

## 3. 핵심 컨셉 5개

### 3.1 Content-addressable chunk (ADR-005)

```
chunk_id = hex(sha256(chunk_bytes))
```

- chunk 의 **내용** 이 곧 **주소**. 같은 byte 는 어디서 나와도 같은 chunk_id.
- **무료 dedup**: 두 객체가 같은 chunk 를 공유하면 disk 위에 하나만 존재.
- **무료 integrity**: GET 시 sha256 다시 계산해서 ID 와 비교 (`chunker.Join`).
- DN 은 ID → byte 를 저장만 한다. ID 가 어떻게 생겼는지 모른다.

### 3.2 Rendezvous Hashing (HRW, ADR-009)

> 같은 chunk 는 항상 **같은 DN 3개** 에 가야 한다 (재현 가능 + 메타 절약).
> DN 이 추가/제거되어도 **대부분의 chunk 는 그 자리** (consistent).

알고리즘:

```
for each DN:
  score = hash(chunk_id || "|" || dn_id)
choose top R by score
```

- N 개 DN 중 어느 R 개로 갈지 — 어디서나 동일한 결정 (chunk_id + DN 목록만 있으면 누구나 재현).
- DN N → N+1 시 평균 R/(N+1) chunk 만 이동. 전체가 흔들리지 않는다.
- Cassandra/DynamoDB 의 ring 기반 일관성 해싱과 같은 목표, 다른 알고리즘. 코드 ~30 줄.

[`internal/placement/placement.go`](../internal/placement/placement.go)

### 3.3 Reed-Solomon Erasure Coding (ADR-008)

```
K data shards + M parity shards = K+M 개 shards 가 stripe 하나
```

- 디스크 사용량: replication 3× → EC (10+4) 1.4×.  같은 내구도, **2배 효율**.
- 어떤 K 개 shards 만 살아있어도 원본 복구 가능 (Reconstruct).
- GF(2^8) 위에서 Vandermonde 행렬 + Gauss-Jordan 으로 from-scratch 구현 (외부 RS 라이브러리 0).
- per-PUT 옵션: `X-KVFS-EC: 4+2` 헤더 있으면 EC, 없으면 replication.

[`internal/reedsolomon/`](../internal/reedsolomon/)

### 3.4 Content-defined Chunking (FastCDC, ADR-018)

> 고정 4 MiB chunking (ADR-011) 으로 자르면 1 byte 만 끼워넣어도 모든 chunk_id 가 달라져 dedup 0%. CDC 는 chunk 경계를 **content** 로 결정해서 shift 에 강하다.

```
fp = 0
for each byte b in stream:
  fp = (fp << 1) + GearTable[b]
  if fp & mask == 0:  cut here.
```

- rolling hash → 같은 32-byte window 가 들어오면 같은 fp → 같은 cut.
- 결과: 99% 동일 + 1% 달라진 두 객체가 ~99% chunk 를 공유 (Borg/Restic 핵심 기법).
- 옵션: `EDGE_CHUNK_MODE=cdc`. 기본은 fixed (Season 2 호환).

### 3.5 HMAC Presigned URL (UrlKey, ADR-007)

```
signature = hex(HMAC-SHA256(secret, "GET\nbucket/key\nexpiry"))
URL: /v1/o/bucket/key?sig=<hex>&exp=<unix-ts>
```

- 서명 1번 발급 → 만료까지 Client 가 직접 GET, edge 로 매번 인증 안 옴.
- S3 presigned URL 과 같은 패턴. ~90 줄 코드.
- `kid` 회전 (ADR-028) 으로 키 노출 시 무중단 교체.

---

## 4. 객체 라이프사이클

### 4.1 PUT (replication, default)

```
Client ──PUT /v1/o/b/k?sig=...&exp=...──► edge
                                            │
                                            ├─ verify HMAC
                                            ├─ chunker.Split(body) → [c1, c2, ...]
                                            │
                                            └─ for each chunk:
                                                ├─ id = sha256(chunk)
                                                ├─ targets = placement.Pick(id, R)
                                                ├─ parallel PUT → R/2+1 ack 대기
                                                │
                                                └─ bbolt: objects[b/k] = ObjectMeta{
                                                       Chunks: [{id, replicas, size}, ...]
                                                   }
                                            │
Client ◄── 201 Created ◄────────────────────┘
```

핵심:
- **Quorum**: R=3 일 때 2 ack 시 성공 처리. 3번째는 background.
- **Streaming** (ADR-017): body 를 메모리에 다 안 받고 `chunker.Reader` 로 한 chunk 씩 읽고 보내고 버린다 → edge RAM = chunkSize 와 무관.

### 4.2 PUT (EC, ADR-008)

```
1. UrlKey 검증
2. chunker → N stripes (각 stripe 의 K data shards)
3. RS Encode → M parity shards 추가 → K+M shards / stripe
4. placement.PlaceN(stripe_id, K+M) → K+M 개 distinct DN
5. 모든 shard 병렬 PUT (실패 시 전체 stripe 실패)
6. bbolt: ObjectMeta{
     EC: {K, M, shard_size, data_size},
     Stripes: [{stripe_id, Shards: [{id, replicas:[dn], size}, ...]}]
   }
```

streaming EC (Ep.6 follow-up): stripe 단위 pump — `stripeBytes = K * shardSize` 만큼만 메모리 점유.

### 4.3 GET (replication / EC 공통 형태)

```
edge: bbolt[b/k] → ObjectMeta
  ├─ replication: for each chunk → replicas[0] 시도 → 실패시 [1] → ...
  │              성공 byte → sha256 재계산 → ID 매치 확인 → write
  │
  └─ EC: for each stripe →
         alive shards 에서 K 개 fetch (replicas[0] 우선)
         → RS Reconstruct → data shards K 개 복원
         → chunker.Join → write
```

GET 는 자가 치유 안 함 — 손상 chunk 발견하면 그냥 다음 replica 로 fallback. 영속 복구는 별도 worker (ADR-025 EC repair).

### 4.4 DELETE

```
edge: bbolt[b/k] → ObjectMeta → replicas/shards DN 들에 DELETE 병렬
      bbolt 에서 (b/k) 제거
```

- 이미 안 사는 chunk 가 디스크에 남으면 ⇒ surplus chunk → GC 가 청소 (ADR-012).

---

## 5. 알고리즘 깊이

### 5.1 Quorum 정확히

- replicas R = 3 (default) → write quorum = ⌊R/2⌋+1 = 2.
- 2 DN 에 PUT 성공 → edge 가 200 응답 후 3번째 DN 은 best-effort.
- 3번째가 실패해도 데이터는 2개에 안전. rebalance worker (ADR-010) 이 나중에 R 회복.
- read quorum = 1 — 한 DN 만 살아있어도 GET 성공.

### 5.2 Reed-Solomon 직관

> "K shards 로 K+M shards 를 만들고, 어떤 K shards 만 있어도 원본 복원."

- generator matrix G = [I_K | V] — 위 K×K 는 단위행렬 (=data 그대로), 아래 M×K 는 Vandermonde (=parity 계산).
- shard 손실 N 개 → G 에서 그 row 들 빼고 K×K 정방행렬 G' → Gauss-Jordan 으로 G'⁻¹ 구함 → 원본 복원.
- GF(2^8) = 1 byte 단위 산술. 곱셈은 log/exp 표 lookup, 덧셈은 XOR.

### 5.3 FastCDC 3-region scan

```
0       MinSize        NormalSize       (Normal+Max)/2       MaxSize
├──skip──┼──maskS──────┼──maskL─────────┼──maskR (옵션)─────┤
        warm-up    (strict)         (loose)            (very loose)
```

- 3 region: Min 까지 fingerprint 만 굴리고 cut 비교 안 함 → 너무 작은 chunk 방지.
- Normal 까지 strict mask (cut 잘 안 남) → 평균 길이가 NormalSize 근처로 모임.
- 그 이후 looser mask → MaxSize 도달 전 cut.
- 옵션 (ADR-035) 4번째 region: MaxSize 직전 더 약한 mask → MaxSize cap 빈도 ↓ → variance ↓.

### 5.4 HRW 의 "consistent" 증명 직관

DN N → N+1 추가 시:
- 새 DN 의 점수가 기존 top-R 안에 들어가는 chunk 만 영향 받음.
- chunk 별로 점수 분포는 균등 → 새 DN 이 top-R 일 확률 = R/(N+1).
- 영향 받은 chunk 는 1개 이동 (가장 낮았던 기존 DN 1개 밀려남).
- 기댓값 이동량 = 전체 chunk × R/(N+1) — 균형점.

---

## 6. 운영성 (Season 3)

> **edge 가 스스로 자기 cluster 를 돌본다.** 외부 cron, 외부 controller 없음.

### 6.1 Auto-trigger (ADR-013)

```
StartAuto(ctx):
  go ticker(EDGE_AUTO_REBALANCE_INTERVAL) → executeRebalance()
  go ticker(EDGE_AUTO_GC_INTERVAL)       → executeGC()
```

두 ticker 가 같은 mutex 공유 → 동시 실행 금지. 수동 admin call 도 같은 mutex 으로 직렬화. Ring buffer (size 32) 에 결과 저장 → `/v1/admin/auto/status` 로 조회.

### 6.2 Rebalance worker (ADR-010, ADR-024)

- **Replicated chunk**: ChunkRef.replicas 가 placement.Pick 결과와 다르면 → 누락 DN 으로 copy → 메타 업데이트 → 잉여 DN 의 chunk 는 GC 대상 (no-delete MVP).
- **EC stripe** (ADR-024): set-based 비교. 현재 shard 위치 set vs 이상적 set. 차집합으로 최소 이동만.

### 6.3 GC (ADR-012)

> surplus chunk = 메타에서 참조 안 되는데 DN 디스크에 남은 chunk.

두 안전망:
1. **claimed-set**: edge 가 살아있는 모든 chunk_id 를 set 으로 만들어 DN 에 보냄. 거기 없으면 candidate.
2. **min-age**: 그 chunk 가 disk 에 N 분 미만으로 있으면 보호 (PUT 직후 메타 commit 직전의 race 방지).

두 조건 모두 통과한 것만 삭제.

### 6.4 EC repair (ADR-025)

- 메타에 있는 stripe 마다 alive shard 수 점검.
- K 미만이면 unrecoverable, K 이상이면 → K 개 fetch → Reed-Solomon Reconstruct → 누락 shard 다시 PUT.
- 큐 기반: worker goroutine 이 큐에서 stripe 하나씩 처리.

### 6.5 Snapshot + Restore (ADR-014, ADR-016)

- **Hot snapshot**: bbolt `tx.WriteTo` — read-only tx 동안 snapshot 파일에 직접 stream. 운영 중 가능.
- **Auto scheduler** (ADR-016): ticker 로 주기 snapshot, 보관 정책 (`EDGE_SNAPSHOT_KEEP=N`) 으로 가장 오래된 것부터 prune.
- **Restore**: edge 정지 → snapshot 파일을 edge.db 로 교체 → 재기동. **offline-only** (운영 중 restore 안 함).

### 6.6 WAL (ADR-019)

- 매 mutation (PutObject / DeleteObject / AddRuntimeDN / RemoveRuntimeDN) 마다 JSON 한 줄 append → fsync.
- snapshot interval 이 1h 여도 WAL pull 을 1s 로 돌리면 RPO ≈ pull interval.
- **group commit** (ADR-035): `EDGE_WAL_BATCH_INTERVAL=5ms` — 다수의 동시 Append 가 한 번의 fsync 를 공유. throughput ~15× ↑, latency 5ms ↑.

### 6.7 Heartbeat (ADR-030)

```
goroutine: tickerLoop(EDGE_HEARTBEAT_INTERVAL) {
  for each DN: GET /healthz with timeout
  consec_fails ≥ threshold ⇒ Healthy = false
}
```

- pull-based — DN 은 자기가 죽었다고 알리지 않는다 (당연).
- `Healthy` 는 placement 결정에 직접 안 쓰임. 운영자에게 시그널 (admin endpoint + alerting).

### 6.8 Dynamic DN registry (ADR-027)

- 시작 시 `EDGE_DNS=dn1:8080,dn2:8080,...` 로 부트.
- 이후 admin `POST /v1/admin/dns?addr=dn4:8080` 로 추가, `DELETE` 로 제거.
- bbolt `dns_runtime` 버킷에 영속 → edge 재시작해도 보존.

---

## 7. HA 와 일관성

### 7.1 Multi-edge read-replica (ADR-022)

```
                     ┌─ kvfs-edge (primary, role=primary)
                     │     └─ writes go here
Client ──GET──┬──────┘
              │      ┌─ kvfs-edge (follower, role=follower)
              └─GET──┤     └─ pulls primary snapshot every PULL_INTERVAL
                     │     └─ writes → 503 + X-KVFS-Primary 헤더
                     └─...
```

- follower 의 `Store.Reload(path)` → `atomic.Pointer.Swap(newDB)` → in-flight read 무중단.
- 이전 DB 핸들은 background 로 close (Linux unlink-while-open 으로 inode 살아있음).
- WAL auto-pull (Ep.5 follow-up): snapshot 전에 WAL since lastSeq 시도 → snapshot fallback. RPO ≤ pull interval.

### 7.2 Auto leader election (ADR-031)

- Raft 의 election 부분만 차용. log replication 은 별도 (ADR-034).
- 3 state machine: follower / candidate / leader. term + voting invariants.
- HTTP RPC 2개: `POST /v1/election/vote`, `POST /v1/election/heartbeat`.
- `EDGE_PEERS=edge1:8000,edge2:8000,edge3:8000` 으로 활성. 기본은 single-edge.

### 7.3 Synchronous replication (ADR-031 follow-up = Ep.8) · informational strict (ADR-033)

- leader 가 매 WAL Append 마다 peers 에 `POST /v1/election/append-wal` push.
- follower 가 즉시 `ApplyEntry` → 메타 동기화. RPO 0 (best-effort).
- `EDGE_STRICT_REPL=1` (ADR-033) — quorum 실패 시 client 에 503 응답 (informational only; bbolt commit 은 이미 발생했으므로 follower 는 다음 sync 에 catch up). 진짜 commit-before-quorum 은 §7.4.

### 7.4 Transactional Raft (ADR-034)

> "best-effort sync replication" 은 commit 후에 push → leader crash 시 commit 됐지만 followers 못 받은 entry 가 손실 가능 (write loss window).

해결:
1. `MarshalPutObjectEntry(meta)` — 미리 entry serialize.
2. `Elector.ReplicateEntry(body)` — peers 에 push, **quorum ack 대기**.
3. quorum 받으면 → `PutObjectAfterReplicate(meta)` 로 local commit.
4. quorum 못 받으면 → 503, local 도 commit 안 함.

`EDGE_TRANSACTIONAL_RAFT=1` 로 활성. PutObject 만 적용 (다른 mutation 은 best-effort).

### 7.5 일관성 모델 정리

| 모드 | RPO | Write 가용성 | Read |
|---|---|---|---|
| single edge | 0 (snapshot interval 까지) | leader = self | linearizable |
| multi-edge read-replica | ≤ pull_interval | primary 만 | follower = stale |
| election + best-effort sync | 0 (대부분), drift 가능 | leader 1개 | leader = fresh, follower = stale |
| transactional Raft | 0 (write loss 없음) | quorum 필요 | leader = fresh, follower = quorum 후 |

---

## 8. 성능·효율 (Season 4 + post-wave)

### 8.1 Streaming PUT/GET (ADR-017)

- 1 GiB PUT 이 와도 edge RAM ≈ chunkSize (4 MiB).
- `chunker.Reader` / `CDCReader` 가 io.Reader 위에서 한 chunk 씩 emit.
- EC 도 같은 패턴 (Ep.6): stripe 단위 pump.

### 8.2 CDC + EC (Ep.7 follow-up)

- variable-size stripe → `Stripe.DataLen` 에 실제 byte 수 기록 → GET 시 padding trim.
- shift-invariant dedup + EC 효율 둘 다 얻음.

### 8.3 WAL group commit (ADR-035)

- `EDGE_WAL_BATCH_INTERVAL=5ms` — 동시 Append 들이 한 fsync 공유.
- background flusher: `time.NewTicker(interval)` → flush + fsync → cond.Broadcast.
- Append 측: write line under mu → cond.Wait → durable 확인 후 return.
- mu 는 fsync 동안 release (다음 batch 가 bufio 채울 수 있게).

### 8.4 Hot/cold tier (ADR-035 follow-up · P5-04)

- DN 마다 class 라벨 (`hot` / `cold`) — admin endpoint 로 등록.
- `EDGE_PLACEMENT_PREFER=hot` → 신규 PUT 은 hot DN 들로 우선 placement.
- **Rebalance 통합 (P5-04)**: `ObjectMeta.Class` 가 PUT 시점 의 class 를 기록 → rebalance 가 ideal placement 계산 시 그 class 의 DN subset 으로 한정. 토폴로지 drift 자동 회복. resolver 가 R 미만 DN 만 알면 default placement 로 fallback (deadlock 방지).
- 배치 변경 없음 — placement 알고리즘은 동일, **노드 부분집합** 만 바꾼다.

### 8.5 sync.Pool 버퍼 재사용

- chunker 내부 scratch buffer 를 pool 화 → 초당 수백 PUT 시 GC 압력 해소.
- pool slab 의 cap 이 요청보다 2× 초과면 reject (메모리 낭비 방지).

### 8.6 Reed-Solomon 최적화

- `mul-by-constant` table + 8-byte loop unrolling.
- (4+2) 64KiB encode: 515 → 1174 MB/s (2.3× ↑). SIMD 없이.

### 8.7 Metrics (`/metrics`)

- Prometheus text format, stdlib only (외부 client lib 없음).
- Counter / Gauge 만. Histogram 은 없음 (overkill).
- `EDGE_METRICS=1` (default on).
- WAL 운영 가시성 (ADR-036): `kvfs_wal_batch_size` (last fsync cycle entry count) + `kvfs_wal_durable_lag_seconds` (oldest unsynced entry age). group commit 효과 외부 검증.
- 메모리 가시성 (ADR-037): `kvfs_chunker_pool_bytes` (scratch pool 누적 cap). cgroup limit 친화 — `EDGE_CHUNKER_POOL_CAP_BYTES` 로 soft cap 설정.

---

## 9. 데이터 모델

### 9.1 bbolt 버킷

| 버킷 | key | value | 용도 |
|---|---|---|---|
| `objects` | `bucket/key` | JSON `ObjectMeta` | 객체 메타 (single source of truth) |
| `dns_runtime` | DN addr | JSON `{registered_at, class}` | 동적 DN registry (ADR-027) + 클래스 라벨 |
| `urlkey_secrets` | `kid` | JSON `{secret, is_primary}` | UrlKey rotation (ADR-028) |

### 9.2 ObjectMeta (replication)

```go
type ObjectMeta struct {
    Bucket, Key  string
    Size         int64
    ContentType  string
    CreatedAt    time.Time
    Chunks       []ChunkRef    // replication mode
    EC           *ECParams     // nil = replication; non-nil = EC
    Stripes      []Stripe      // EC mode
}

type ChunkRef struct {
    ChunkID  string   // hex(sha256(chunk_bytes))
    Size     int64
    Replicas []string // DN addrs (placement.Pick 결과)
}
```

### 9.3 EC stripe

```go
type ECParams struct {
    K, M     int   // K data + M parity
    ShardSize int  // padded shard size in bytes
    DataSize  int64 // total raw data size (for trimming)
}
type Stripe struct {
    StripeID string      // hex(sha256(K data shards concat))
    Shards   []ChunkRef  // length K+M
    DataLen  int64       // CDC mode: actual data length in this stripe (for padding trim)
}
```

### 9.4 WAL entry

```json
{"seq": 42, "ts": "2026-04-26T12:34:56Z", "op": "put_object", "args": {...ObjectMeta...}}
```

- 4가지 op: `put_object`, `delete_object`, `add_runtime_dn`, `remove_runtime_dn`.
- `seq` 단조 증가, snapshot 으로 rotate (truncate + epoch bump).

---

## 10. 환경 변수 (간략)

> 전체 표는 `README.md` § "환경 변수". 여기는 **모드 결정에 영향** 주는 것만.

| 변수 | 기본 | 영향 |
|---|---|---|
| `EDGE_REPLICATION_FACTOR` | 3 | replication 모드의 R |
| `EDGE_QUORUM_WRITE` | ⌊R/2⌋+1 | write ack 임계 명시 override (보통 자동) |
| `EDGE_CHUNK_SIZE` | 4 MiB | fixed chunk size |
| `EDGE_CHUNK_MODE` | `fixed` | `cdc` 로 설정 시 FastCDC 활성 |
| `EDGE_WAL_PATH` | (off) | 설정 시 ADR-019 WAL 활성 |
| `EDGE_WAL_BATCH_INTERVAL` | (off) | 설정 시 group commit 활성 (e.g. `5ms`) |
| `EDGE_PEERS` / `EDGE_SELF_URL` | (off) | 설정 시 leader election 활성 (ADR-031) |
| `EDGE_TRANSACTIONAL_RAFT` | 0 | 1 = ADR-034 commit-before-quorum |
| `EDGE_PLACEMENT_PREFER` | (off) | DN class 라벨 ("hot" 등) bias |
| `EDGE_METRICS` | 1 | `/metrics` Prometheus endpoint |
| `EDGE_AUTO_REBALANCE_INTERVAL` | (off) | 설정 시 auto-rebalance ticker |
| `EDGE_AUTO_GC_INTERVAL` | (off) | 설정 시 auto-GC ticker |
| `EDGE_AUTO_GC_MIN_AGE` | 5m | GC 안전망 — 이보다 어린 chunk 보호 |
| `EDGE_AUTO_CONCURRENCY` | 1 | rebalance/GC worker 동시 실행 수 |
| `EDGE_SNAPSHOT_DIR / INTERVAL / KEEP` | (off) | 설정 시 auto-snapshot scheduler |
| `EDGE_HEARTBEAT_INTERVAL` | (off) | 설정 시 DN heartbeat |
| `EDGE_ROLE` / `EDGE_PRIMARY_URL` / `EDGE_FOLLOWER_PULL_INTERVAL` | `primary` / — / 30s | follower 모드: read-only + snapshot pull (ADR-022) |
| `EDGE_TLS_CERT` / `EDGE_TLS_KEY` | (off) | 설정 시 HTTPS 활성 (ADR-029, `internal/tlsutil`) |
| `EDGE_COORD_URL` | (off) | Season 5 Ep.2 (ADR-015): 설정 시 edge 가 메타 RPC 를 coord 로 위임 (proxy mode) |
| `COORD_PEERS` / `COORD_SELF_URL` | (off) | Season 5 Ep.3 (ADR-038): coord-side election. 설정 시 coord 가 HA cluster 일원이 됨 |
| `COORD_WAL_PATH` | (off) | Season 5 Ep.4 (ADR-039): coord-to-coord WAL sync 활성. peer 들 간 메타 일관성 보장 |
| `COORD_TRANSACTIONAL_RAFT` | 0 | Season 5 Ep.5 (ADR-040): replicate-then-commit. quorum 실패 시 503 + leader bbolt 무변화 (Elector + WAL 필수) |
| `COORD_DN_IO` | 0 | Season 6 Ep.2 (ADR-044): coord 가 chunk I/O — rebalance/gc/repair apply paths runnable on coord |
| `EDGE_COORD_URLKEY_POLL_INTERVAL` | 30s | Season 6 Ep.7 (ADR-049): edge 가 coord 의 urlkey 변경을 polling 주기로 sync |
| `EDGE_COORD_LOOKUP_CACHE_TTL` | (off) | P6-10 (opt-in): coord lookup 결과 per-(bucket,key) 캐싱. 권장 1-5s. CommitObject/DeleteObject 가 invalidate |

> 운영 디테일 (election timing, DN-side TLS CA 등) 은 `README.md` § "환경 변수" 의 전체 표.

---

## 11. 모드 선택 가이드

### 11.1 replication vs EC

| 기준 | replication (R=3) | EC (K=4, M=2) |
|---|---|---|
| 디스크 사용량 | 3.0× | 1.5× |
| 같은 내구도 | DN 2개 손실 견딤 | DN 2개 손실 견딤 |
| GET latency | replicas[0] = 1 RTT | K shards = 1 RTT (병렬) |
| GET CPU | sha256 검증 | RS Reconstruct (손실 시) |
| PUT latency | R/2+1 ack = 2 RTT | K+M ack (병렬) |
| PUT CPU | 0 | encode 1 GB/s 정도 |
| dedup | per-chunk 가능 | per-stripe (실용성 ↓) |
| **언제 쓰나** | 핫 데이터, dedup | 콜드 데이터, 디스크 절약 |

### 11.2 fixed vs CDC chunking

| 기준 | fixed | CDC |
|---|---|---|
| chunk 경계 | 4 MiB offset 고정 | content-driven |
| dedup (수정 후) | 0% (1 byte shift 시) | ~99% |
| chunker CPU | 거의 없음 | gear hash per byte |
| 코드 복잡도 | 30 줄 | 120 줄 |
| **언제 쓰나** | append-only / 변경 없는 데이터 | backup, 버전 관리 데이터 |

### 11.3 inline fsync vs group commit

| 기준 | inline (default) | group commit (5ms) |
|---|---|---|
| 단일 PUT latency | ~1 ms (fsync) | ~5-10 ms (batch wait) |
| 동시 PUT throughput | ~480/s | ~7400/s (15× ↑) |
| RPO | 0 | ≤ batch interval |
| 코드 위험 | 단순 | cond/Broadcast/epoch |
| **언제 쓰나** | 트래픽 적고 RPO 0 필요 | 동시 write 많을 때 |

### 11.4 single edge vs multi-edge HA

| 기준 | single | replica | election + sync | transactional Raft |
|---|---|---|---|---|
| HA | 0 | read-replica only | full | full |
| Write loss window | 0 (snap interval 까지) | 0 | very small | 0 |
| 운영 복잡도 | 낮음 | 중 | 중 | 높음 |
| 신규 사용자 | ✅ | | | |
| 운영 트래픽 | | ✅ | ✅ | |
| 금융·결제 | | | | ✅ |

---

## 12. 한계와 비목표

### 12.1 한계

- **단일 edge = SPOF** (multi-edge HA 안 쓸 때). 메타 손실 시 chunks 는 살아있어도 객체 list 손실.
- **edge 메모리 = 활성 PUT 의 chunk size 합**. streaming 으로 object size 와는 무관해졌지만 동시 PUT × chunk size 는 든다.
- **bbolt single-writer**. 모든 mutation 직렬화. 1 edge 가 처리할 수 있는 메타 mutation/sec 한계 (~10k 인라인, ~100k batched).
- **DN 간 통신 없음**. cross-DN replication, DN 자가 치유 불가. 모든 복구는 edge 가 주도.
- **버킷 = 단순 prefix**. ACL, 정책, lifecycle 없음.

### 12.2 명시적 비목표

- **production replacement** for S3/MinIO/Ceph. kvfs 는 reference, 그쪽은 운영.
- **multi-region**. 단일 cluster 만.
- **encryption at rest**. dn 디스크는 평문 (운영자가 LUKS 등으로 처리).
- **versioning, MFA delete, lifecycle policy**. S3 의 그 features 들은 없음.
- **NFS / POSIX gateway** — 한번 평가 후 deferred (ADR-032). FUSE 마운트는 메타데이터 모델이 안 맞고, NFS 서버를 박으면 kvfs 가 stateful filesystem 로 변질됨. 후속 검토 가능성만 열어둠.

---

### 12.3 Season 5 — coord 분리 (closed, Ep.1~7)

ADR-015 Accept (2026-04-26). `kvfs-coord` daemon 신설 — placement + 메타 ownership 이 edge 에서 떨어져 나간다. ADR-002 supersede.

- Ep.1: `cmd/kvfs-coord/` skeleton + 4 RPC. 데모 `scripts/demo-aleph.sh`.
- Ep.2: edge → coord meta client. `EDGE_COORD_URL`. 데모 demo-bet (ב).
- Ep.3: coord HA via Raft (ADR-038). 데모 demo-gimel (ג).
- Ep.4: coord WAL replication (ADR-039). 데모 demo-dalet (ד).
- Ep.5: coord transactional commit (ADR-040). 데모 demo-he (ה).
- Ep.6: edge → coord placement RPC (ADR-041). 데모 demo-vav (ו).
- Ep.7: cli direct coord admin (ADR-042, read-only inspect). 데모 demo-zayin (ז).

이 트랙 완료 시 cluster 는 3-daemon (edge + coord + dn) — coord 가 placement·메타·일관성의 owner, edge 는 thin gateway.

### 12.4 Season 6 — coord operational migration (in progress)

Season 5 가 read-side 를 옮겼다면 Season 6 은 worker + mutating admin 을 옮긴다. 끝나면 cli 가 모든 admin 을 coord 통해 함, edge 가 진짜 thin.

- Ep.1: rebalance plan on coord (ADR-043). `/v1/coord/admin/rebalance/plan`. 데모 demo-chet (ח).
- Ep.2: rebalance apply on coord (ADR-044). `COORD_DN_IO=1` → coord 가 DN HTTP I/O 보유. cli `rebalance --apply --coord`. 데모 demo-tet (ט).
- Ep.3: GC plan + apply on coord (ADR-045). `/v1/coord/admin/gc/{plan,apply}`. cli `gc --coord`. 데모 demo-yod (י).
- Ep.4: EC repair on coord (ADR-046). K survivors → RS Reconstruct → 누락 shard 재배포. cli `repair --coord`. 데모 demo-kaf (כ).
- Ep.5: DN registry mutation on coord (ADR-047). add/remove/class. cli `dns --coord {add|remove|class}`. 데모 demo-lamed (ל).
- Ep.6: URLKey kid registry on coord (ADR-048). cli `urlkey --coord`. 데모 demo-mem (מ).
- Ep.7 (현재): Edge urlkey.Signer propagation from coord (ADR-049). polling (`EDGE_COORD_URLKEY_POLL_INTERVAL`, default 30s). edge 가 coord 의 새 kid 를 자동 반영. 데모 demo-nun (히브리 נ).

## 13. 다음에 읽을 것

### 13.1 코드 진입점 (추천 순서)

1. `cmd/kvfs-edge/main.go` — 모든 wiring 의 출발점.
2. `internal/edge/edge.go` — HTTP handlers (`handlePut`, `handleGet` 부터).
3. `internal/coordinator/coordinator.go` — fanout / quorum.
4. `internal/placement/placement.go` — HRW 30 줄.
5. `internal/store/store.go` + `wal.go` — bbolt 래퍼 + WAL.
6. `internal/reedsolomon/rs.go` — EC 수학.

### 13.2 시즌별 walkthrough — blog episode

- Ep.1~6 (Season 1~2): MVP 부터 EC 까지 — `blog/01..06`.
- Ep.7~13 (Season 3): 운영성 7개 — `blog/07..13`.
- Ep.14~17 (Season 4): 성능·효율 + Raft — `blog/14..17`.

### 13.3 ADR

[`docs/adr/README.md`](adr/README.md) — 35 ADR, 시즌별 표, 번호 오름차순. 결정의 근거가 모두 여기.

### 13.4 라이브 데모

`./scripts/demo-*.sh` — α 부터 ω 까지 21개. ADR README 의 시즌 표에서 episode → 데모 매핑 확인.

---

> **이 문서는 살아있는 가이드.** 새 ADR/episode 추가 시 §3·§6·§7·§8 에 한 줄 갱신.
> 결정의 디테일은 ADR 본문, 동작의 증명은 demo, 해석은 blog. 이 문서는 그 셋을 **잇는 다리**.
