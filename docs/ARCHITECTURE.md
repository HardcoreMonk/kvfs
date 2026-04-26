# kvfs 아키텍처

> Season 1 MVP → Season 2 (분산 알고리즘) → Season 3 (운영성) 까지 누적 설계.
> 각 결정의 상세 근거는 `docs/adr/`. 환경변수 전체는 `../README.md` § "환경 변수".

## 한 장 요약

```
Client ─HTTP+UrlKey─► kvfs-edge ─HTTP REST─► kvfs-dn (× N)
                         │                       │
                         ├─ placement (HRW)      └─ chunks/<sha256[0:2]>/<rest>
                         ├─ chunker / EC encoder
                         ├─ rebalance / GC / repair
                         ├─ heartbeat (DN liveness)
                         ├─ snapshot scheduler
                         └─ bbolt meta
                            objects:        bucket/key → ChunkRef[] | (EC) Stripes[]
                            dns_runtime:    addr → registered_at
                            urlkey_secrets: kid → secret + is_primary
```

- **2 daemon**: `kvfs-edge` (게이트웨이 + 인라인 coordinator) · `kvfs-dn` (스토리지 노드, ×N)
- **객체 모델**: replication (default 3-way) **또는** Reed-Solomon EC (per-PUT `X-KVFS-EC: K+M` 헤더)
- **Placement**: chunkID/stripeID → top-R DN (Rendezvous Hashing). DN 추가·제거 시 약 R/N 만 이동
- **Quorum write**: `R/2+1` ack 시 성공
- **GET fallback**: replicas 순차 시도, EC 면 K survivors 로 reconstruct (ADR-025)
- **운영성**: edge 가 자기 cluster 의 정렬·청소·복구·snapshot·heartbeat 를 스스로 (ADR-013/016/030)

## 컴포넌트

### kvfs-edge (게이트웨이)

- **포트**: `:8000` (HTTP/HTTPS, ADR-029)
- **상태**: `<EDGE_DATA_DIR>/edge.db` (bbolt) + `<EDGE_SNAPSHOT_DIR>/` (선택, ADR-016)
- **외부 의존**: DN N 개 (HTTP/HTTPS), 그 외 0
- **데이터 엔드포인트**:
  - `PUT /v1/o/{bucket}/{key...}?sig=...&exp=...` (replication)
  - `PUT /v1/o/{bucket}/{key...}?sig=...&exp=...` `X-KVFS-EC: K+M` (EC, ADR-008)
  - `GET /v1/o/{bucket}/{key...}?sig=...&exp=...`
  - `DELETE /v1/o/{bucket}/{key...}?sig=...&exp=...`
- **관리 엔드포인트** (auth 없음 — admin 망 가정):
  - `GET /v1/admin/objects` · `GET /v1/admin/dns` · `GET /v1/admin/auto/status`
  - `POST /v1/admin/{rebalance,gc,repair}/{plan,apply}` (ADR-010/012/025)
  - `POST /v1/admin/dns` · `DELETE /v1/admin/dns?addr=` (ADR-027)
  - `GET /v1/admin/urlkey` · `POST /v1/admin/urlkey/rotate` · `DELETE ...?kid=` (ADR-028)
  - `GET /v1/admin/meta/snapshot|info` (ADR-014)
  - `GET /v1/admin/snapshot/history` (ADR-016)
  - `GET /v1/admin/heartbeat` (ADR-030)
  - `GET /v1/admin/role` (ADR-022)

### kvfs-dn (스토리지 노드)

- **포트**: `:8080` (HTTP/HTTPS, ADR-029)
- **상태**: `<DN_DATA_DIR>/chunks/<sha256[0:2]>/<rest>`
- **외부 의존**: 0
- **엔드포인트**: `PUT/GET/DELETE /chunk/{id}` · `GET /chunks` (list) · `GET /healthz`

## 패키지 구조

```
cmd/kvfs-edge/main.go          edge 엔트리포인트 + env wiring
cmd/kvfs-dn/main.go            dn 엔트리포인트
cmd/kvfs-cli/main.go           sign·inspect·placement-sim·rebalance·gc·auto·dns·
                               urlkey·repair·meta·heartbeat·role 서브커맨드

internal/urlkey/               HMAC-SHA256 signer · multi-kid rotation (ADR-007/028)
internal/store/                bbolt 래퍼 + Snapshot/Reload + SnapshotScheduler
                               (ADR-004/014/016, atomic.Pointer hot-swap)
internal/dn/                   DN HTTP 서버 · 디스크 저장 · /chunks 리스트
internal/placement/            Rendezvous Hashing — chunk→DN 선택 (ADR-009)
internal/coordinator/          fanout · quorum · TLS · DNScheme (ADR-029)
internal/chunker/              고정 크기 split/join (ADR-011)
internal/reedsolomon/          GF(2^8) + Vandermonde + Gauss-Jordan (ADR-008)
internal/rebalance/            chunk + EC stripe rebalance (ADR-010/024)
internal/gc/                   surplus chunk GC (ADR-012)
internal/repair/               EC repair via Reed-Solomon Reconstruct (ADR-025)
internal/heartbeat/            DN liveness monitor (ADR-030)
internal/edge/                 HTTP 서버 · 핸들러 · auth 미들웨어
internal/edge/replica.go       Multi-edge follower 로직 (ADR-022)
internal/tlsutil/              TLS 헬퍼 (ADR-029)
```

## 주요 데이터 흐름

### PUT (replication)

```
1. Client → edge: PUT /v1/o/b/k?sig=...&exp=...   [body]
2. edge: UrlKey 검증 → chunker.Split(body) → N 개 청크
3. 각 청크 대해:
   a. chunk_id = hex(sha256(chunk))
   b. placement.Pick(chunk_id, R) → R 개 DN
   c. 병렬 PUT /chunk/<id> → R/2+1 ack
4. bbolt: (b,k) → ObjectMeta{Chunks: [{id, replicas, size}, ...]}
5. edge → Client: 201 + JSON
```

### PUT (EC, ADR-008)

```
1-2. UrlKey + chunker split → N stripes (각 stripe = K data 청크)
3. Reed-Solomon Encode → K data + M parity = K+M shards / stripe
4. placement.PlaceN(stripeID, K+M) → K+M 개 DN, 각 shard 병렬 PUT
5. bbolt: ObjectMeta{EC: {K, M}, Stripes: [{id, Shards: [...]}]}
```

### GET (replication)

```
1. edge: bbolt → replicas 순회
2. 첫 성공 DN → sha256 재검증 → Client
```

### GET (EC)

```
1. edge: 각 stripe 의 alive shard 들에서 K 개 fetch
2. Reed-Solomon Reconstruct → data shards 복원 → chunker.Join → body
3. Client
```

### Auto-trigger cycle (ADR-013)

```
StartAuto(ctx) → 두 개 ticker goroutine:
  rebalance: time.NewTicker(EDGE_AUTO_REBALANCE_INTERVAL) → executeRebalance()
  gc:        time.NewTicker(EDGE_AUTO_GC_INTERVAL)        → executeGC()
공유: rebalanceMu/gcMu — 수동 admin call 과 직렬화
기록: ring buffer (32) — /v1/admin/auto/status 로 조회
```

### Heartbeat cycle (ADR-030)

```
StartHeartbeat(ctx, interval):
  goroutine: tickerLoop(interval) {
    Heartbeat.Tick(ctx, coord.DNs())  # fan-out parallel GET /healthz
  }
update(addr, latency, err) → consec_fails ≥ threshold ⇒ Healthy=false
```

### Multi-edge follower sync (ADR-022)

```
follower 기동 → SetFollowerConfig(primary URL, pull interval)
StartFollowerSync(ctx):
  immediate sync + tickerLoop(pull_interval) {
    GET <primary>/v1/admin/meta/snapshot → edge.db.sync.<seq>
    Store.Reload(path) → atomic.Pointer.Swap(newDB) → async-close old
    unlink prev (Linux unlink-while-open keeps inode alive)
  }
follower 의 PUT/DELETE → 503 + X-KVFS-Primary 헤더
```

## 의사결정 이력 (요약)

| 시점 | 결정 | ADR |
|---|---|---|
| S1 | 2-daemon · HTTP REST · bbolt · sha256 CA · UrlKey HMAC | 002·003·004·005·007 |
| S2 | Rendezvous Hashing → rebalance worker → GC → chunking → Reed-Solomon EC | 009·010·012·011·008 |
| S3 | Auto-trigger → EC stripe rebalance → EC repair → meta backup → DN heartbeat → auto-snapshot → multi-edge HA | 013·024·025·014·030·016·022 |
| 운영 보강 | Dynamic DN registry · UrlKey kid rotation · Optional TLS/mTLS | 027·028·029 |

전체: [`adr/README.md`](adr/README.md).

## 시험 가능한 가설 (블로그 글감)

| # | 가설 | 검증 |
|---|---|---|
| 1 | 3-way replication 은 복잡하지 않다 | `coordinator.WriteChunk` ~60줄 — Ep.1 ✅ |
| 2 | HMAC URL 서명은 2페이지면 충분 | `internal/urlkey` ~90 LOC — ε 데모 ✅ |
| 3 | Content-addressable chunk = 무료 dedup | 4 objects → 3 chunks 실측 ✅ |
| 4 | bbolt 로 충분 (SQLite 불필요) | MVP 패턴 자연스러움 ✅ |
| 5 | Rendezvous Hashing = 수학적으로 "consistent" | R/N ≈ 이론값 (27.4% ≈ 27.3%) ✅ |
| 6 | EC encode 가 50 MB/s 정도 — 실측 시 ~10× 빠름 | bench: (4+2) 515 MB/s, (10+4) 264 MB/s ✅ |
| 7 | atomic.Pointer hot-swap 으로 read interruption 0 가능 | demo-rho 4 syncs, 0 errors ✅ |

## 참고

- 후속 작업: [`FOLLOWUP.md`](FOLLOWUP.md)
- ADR 전체: [`adr/`](adr/)
- 환경변수: `../README.md` § "환경 변수"
- Apache 2.0 라이선스
