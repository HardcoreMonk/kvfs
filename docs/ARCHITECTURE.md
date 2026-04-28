# kvfs 아키텍처

> 짧은 reference. 처음 읽는다면 [`GUIDE.md`](GUIDE.md) (또는 [`guide.html`](guide.html)) 의 walkthrough 부터.
> 각 결정의 상세 근거는 `docs/adr/`. 환경변수 전체는 `../README.md` § "환경 변수".

## 한 장 요약

```
                                      ┌─ kvfs-coord (× N) ──┐  (S5~, 옵션)
                                      │  placement·메타 owner │
                                      │  Raft + WAL replication │
Client ─HTTP+UrlKey─► kvfs-edge ──────┴─ HTTP REST ──► kvfs-dn (× N)
                         │                                 │
                         ├─ thin gateway (coord-proxy 모드)   └─ chunks/<sha256[0:2]>/<rest>
                         │   OR 인라인 coordinator (S1~S4 모드)
                         ├─ chunker / EC encoder
                         ├─ urlkey verify (multi-kid Signer)
                         └─ bbolt meta (proxy 모드면 거의 빈 채로 유지)
                            objects:        bucket/key → ChunkRef[] | (EC) Stripes[]
                            dns_runtime:    addr → registered_at + class
                            urlkey_secrets: kid → secret + is_primary
```

- **모드 선택**:
  - **2-daemon (S1~S4 호환)**: `EDGE_COORD_URL` unset. edge 안에 coordinator 인라인 — placement·rebalance·GC·repair·heartbeat 모두 edge 가 처리. 단순함.
  - **3-daemon (S5~)**: `EDGE_COORD_URL=http://coord:9000` 으로 coord 모드 활성. edge 는 thin gateway, coord 가 메타·placement·일관성 owner. cli admin 도 `--coord URL` 로 직접. HA 는 coord-side Raft (`COORD_PEERS`) + WAL replication (`COORD_WAL_PATH`) + transactional commit (`COORD_TRANSACTIONAL_RAFT`).
- **객체 모델**: replication (default 3-way) **또는** Reed-Solomon EC (per-PUT `X-KVFS-EC: K+M` 헤더)
- **Placement**: chunkID/stripeID → top-R DN (Rendezvous Hashing). DN 추가·제거 시 약 R/N 만 이동
- **Quorum write/read**: 기본 `R/2+1`, 요청별 `X-KVFS-W` / `X-KVFS-R` 로 튜닝 가능 (ADR-053)
- **GET fallback**: replication 은 replica fallback, EC 는 parallel shard fetch + K survivors reconstruct (ADR-025/052)
- **운영성**: edge (또는 coord) 가 자기 cluster 의 정렬·청소·복구·snapshot·heartbeat·anti-entropy self-heal 을 스스로 (ADR-013/016/030/043~049/054~063)

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

### kvfs-coord (옵션, S5~)

- **포트**: `:9000` (기본). HTTP.
- **상태**: `<COORD_DATA_DIR>/coord.db` (bbolt, edge 와 동일 스키마) + 선택적 WAL.
- **외부 의존**: DN N 개 (`COORD_DNS=`). HA 모드면 peer coord 들 (`COORD_PEERS=`).
- **모드 결정**: edge 가 `EDGE_COORD_URL` 설정 시 메타·placement RPC 를 coord 로 위임 → coord-proxy 모드. unset 이면 edge 인라인 (S1~S4 동작).
- **엔드포인트**: `/v1/coord/{place,commit,lookup,delete}` (메타 RPC) · `/v1/coord/admin/{objects,dns,dns/class,dns/domain,rebalance,gc,repair,urlkey,urlkey/rotate,anti-entropy}` (admin/self-heal) · `/v1/election/{vote,heartbeat,append-wal}` (HA) · `/metrics`.

### kvfs-dn (스토리지 노드)

- **포트**: `:8080` (HTTP/HTTPS, ADR-029)
- **상태**: `<DN_DATA_DIR>/chunks/<sha256[0:2]>/<rest>`
- **외부 의존**: 0
- **엔드포인트**: `PUT/GET/DELETE /chunk/{id}` · `GET /chunks` (list) · `GET /healthz`

## 패키지 구조

```
cmd/kvfs-edge/main.go          edge 엔트리포인트 + env wiring
cmd/kvfs-coord/main.go         coord 엔트리포인트 (S5~)
cmd/kvfs-dn/main.go            dn 엔트리포인트
cmd/kvfs-cli/main.go           sign·inspect·placement-sim·rebalance·gc·auto·dns·
                               urlkey·repair·meta·heartbeat·role 서브커맨드
                               (모든 mutating subcommand 가 --coord URL 지원, S6)

internal/urlkey/               HMAC-SHA256 signer · multi-kid rotation (ADR-007/028)
internal/store/                bbolt 래퍼 + Snapshot/Reload + SnapshotScheduler + WAL
                               (ADR-004/014/016/019/035, atomic.Pointer hot-swap)
internal/dn/                   DN HTTP 서버 · 디스크 저장 · /chunks 리스트
internal/placement/            Rendezvous Hashing — chunk→DN 선택 (ADR-009)
internal/coordinator/          fanout · quorum · TLS · DNScheme (ADR-029)
internal/chunker/              고정 크기 + CDC split/join + scratch pool (ADR-011/018/035/037)
internal/reedsolomon/          GF(2^8) + Vandermonde + Gauss-Jordan (ADR-008, mul-table 최적화)
internal/rebalance/            chunk + EC stripe rebalance · class-aware (ADR-010/024)
internal/gc/                   surplus chunk GC (ADR-012)
internal/repair/               EC repair via Reed-Solomon Reconstruct (ADR-025)
internal/heartbeat/            DN liveness monitor (ADR-030)
internal/election/             Raft-style election (ADR-031, edge·coord 양쪽이 재사용)
internal/edge/                 HTTP 서버 · 핸들러 · auth 미들웨어 · CoordClient (proxy 모드)
internal/edge/replica.go       Multi-edge follower 로직 (ADR-022)
internal/edge/urlkey_sync.go   coord urlkey 변경 polling 반영 (ADR-049)
internal/coord/                coord daemon 핵심 — 메타·placement·HA·admin·anti-entropy (S5~P8, ADR-015/054~063)
internal/tlsutil/              TLS 헬퍼 (ADR-029)
internal/cliutil/              EnvOr · AtoiOr · SplitCSV · Fatal (P6-09)
internal/httputil/             WriteJSON · WriteError · WriteErr (P6-09)
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
| S4 | Streaming PUT/GET → CDC → auto leader election → WAL | 017·018·031·019 |
| post-S4 wave | NFS deferred · strict repl · transactional Raft · micro-opts · WAL gauges · chunker pool cap | 032·033·034·035·036·037 |
| S5 | coord 분리 (ADR-002 supersede) → coord HA (Raft) → coord WAL repl → coord txn commit → edge → coord placement RPC → cli direct admin | 015·038·039·040·041·042 |
| S6 | rebalance/gc/repair on coord → DN registry on coord → URLKey on coord → edge urlkey polling | 043·044·045·046·047·048·049 |
| S7/P8 | failure-domain · degraded read · tunable consistency · anti-entropy/Merkle · self-heal · coord metrics/observability | 050·051·052·053·054·055·056·057·058·059·060·061·062·063 |

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
- 에이전트 작업 규약: [`../AGENTS.md`](../AGENTS.md) (Codex 기준, `CLAUDE.md` 는 호환 shim)
- Apache 2.0 라이선스
