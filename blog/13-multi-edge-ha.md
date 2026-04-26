# Episode 13 — Multi-edge HA: read-replica with snapshot-pull

> **Series**: kvfs — distributed object storage from scratch
> **Season**: 3 (operability) **— closes here**
> **Episode**: 7 (final) · **ADR**: [022](../docs/adr/ADR-022-multi-edge-ha.md) · **Demo**: `./scripts/demo-rho.sh`

---

## Season 3 의 마지막 비대칭

운영성 트랙 12부터 본 적 있다:
- **DN heartbeat (Ep.11)** — DN 죽음을 즉시 안다
- **Meta backup (Ep.10) + Auto-snapshot (Ep.12)** — bbolt 손실 안전망

남은 비대칭: **edge 자체**. edge 가 죽으면? 클러스터 전체 read/write 불가.

`bbolt` 는 single-writer file lock. 두 edge 가 같은 메타 파일을 동시에 열 수
없다. 진짜 active-active multi-write 는 별도 분산 메타 (etcd / Raft) 또는
쓰기를 한 edge 로 funnel 하는 모델.

이 ep 는 가장 단순한 **read-replica HA**:
- write 처리는 1대 (primary)
- read 처리는 N대 (primary + follower들)
- follower 는 primary snapshot 을 주기 pull 해서 자기 bbolt 갱신
- failover = 운영자 명시 절차

이 정도만 해도 read 부하 분산 + primary 단일 장애 시 read 만이라도 살아있는
의미. Postgres 의 streaming replication MVP 와 같은 모델.

## 디자인 — 단순한 두 컴포넌트

### 1. atomic.Pointer hot-swap (store)

기존 `MetaStore` 는 `*bbolt.DB` 필드를 직접 보유. follower 가 새 snapshot 을
받았을 때 이 포인터를 갈아끼우려면 모든 read/write 가 lock 잡아야 한다.

대신 `atomic.Pointer[bbolt.DB]`:

```go
type MetaStore struct {
    db atomic.Pointer[bbolt.DB]
}

func (m *MetaStore) Reload(path string) error {
    newDB, err := openInitialized(path)
    if err != nil { return err }
    old := m.db.Swap(newDB)
    if old != nil {
        // Async close: in-flight tx finishes first, then bbolt.Close returns.
        go func() { _ = old.Close() }()
    }
    return nil
}
```

모든 메서드는 매 호출마다 현재 포인터를 load:

```go
func (m *MetaStore) PutObject(obj *ObjectMeta) error {
    return m.db.Load().Update(func(tx *bbolt.Tx) error {
        // ...
    })
}
```

Swap 시점:
- 이미 시작된 read tx 는 old pointer 그대로 (bbolt 의 read-tx 는 snapshot
  isolation)
- 새 호출은 신 pointer 사용
- bbolt.Close 는 in-flight tx 가 끝날 때까지 block — 비동기 goroutine 으로
  처리해 caller 안 막음

→ **read interruption 0**. atomic ops 로 lock 없이.

### 2. Follower sync loop (replica.go)

```go
func (s *Server) followerSyncOnce(ctx context.Context, client *http.Client, cfg FollowerConfig) error {
    seq := atomic.AddUint64(&s.followerSt.syncSeq, 1)
    tmp := filepath.Join(cfg.DataDir, fmt.Sprintf("edge.db.sync.%d", seq))
    
    // 1. download snapshot from primary
    resp, _ := client.Get(cfg.PrimaryURL + "/v1/admin/meta/snapshot")
    f, _ := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
    io.Copy(f, resp.Body)
    f.Close()
    
    // 2. hot-swap: open new bbolt, atomic.Pointer.Swap, async-close old
    if err := s.Store.Reload(tmp); err != nil { /* roll back */ }
    
    // 3. unlink previous sync file (Linux keeps inode alive for open fd)
    if prevActive != "" { os.Remove(prevActive) }
}
```

핵심 디자인:
- **per-pull unique filename** — bbolt 가 같은 path 를 두 번 열 수 없으니
  `edge.db.sync.<seq>` 로 회전. seq 는 atomic counter
- **Linux unlink-while-open** — 이전 파일을 unlink 해도 bbolt 의 open fd 가 살아
  있으면 inode 는 free 안 됨 (POSIX 보장). 다음 close 시점에 정리됨

### 3. Write rejection (middleware)

```go
func (s *Server) rejectIfFollowerWrite(w http.ResponseWriter, r *http.Request) bool {
    if s.Role != RoleFollower { return false }
    switch r.Method {
    case http.MethodPut, http.MethodDelete, http.MethodPost, http.MethodPatch:
        if strings.HasPrefix(r.URL.Path, "/v1/o/") {
            w.Header().Set("X-KVFS-Primary", s.followerSt.cfg.PrimaryURL)
            writeError(w, http.StatusServiceUnavailable, "edge is in follower role; route writes to primary")
            return true
        }
    }
    return false
}
```

`handlePut`, `handleDelete` 첫 줄에 추가:
```go
func (s *Server) handlePut(w, r) {
    if s.rejectIfFollowerWrite(w, r) { return }
    // ... existing logic
}
```

503 + `X-KVFS-Primary` 헤더 — 클라이언트 SDK 가 알아서 primary 로 retry 가능.

## 환경변수 — 한 줄로 follower 만들기

```bash
docker run kvfs-edge:dev \
  -e EDGE_ROLE=follower \
  -e EDGE_PRIMARY_URL=http://primary:8000 \
  -e EDGE_FOLLOWER_PULL_INTERVAL=30s \
  ...
```

기본값은 `primary` — 기존 single-edge 배포 영향 0.

## 라이브 데모 (`./scripts/demo-rho.sh`)

primary + 1 follower, pull interval 2s:

```
--- step 3: roles ---
role:           primary
---
role:           follower
primary URL:    http://edge:8000
pull interval:  2s
last sync:      2026-04-26T08:35:36Z (1s ago, 24576 bytes)
total syncs:    2 (errors: 0)

--- step 4: GET both objects from FOLLOWER ---
  GET obj-1 from follower → primary-write-1-1777192534…
  GET obj-2 from follower → primary-write-2-1777192534…

--- step 5: PUT to FOLLOWER must 503 + X-KVFS-Primary header ---
  HTTP code: 503
  X-KVFS-Primary: http://edge:8000
  Body: {"error":"edge is in follower role; route writes to primary"}

--- step 6: PUT 1 more object via primary, then re-sync follower ---
  PUT obj-3 to primary (follower hasn't synced yet)
  waiting 2s for next pull cycle...

--- step 7: GET obj-3 from follower → should now exist ---
  GET obj-3 from follower → primary-write-3-1777192537…

--- step 8: final follower role stats ---
total syncs:    4 (errors: 0)

✅ ρ demo PASS
```

4 syncs, 0 errors, hot-swap 정상 동작. read 도중 file lock conflict 0.

## Failover (manual MVP)

운영자 절차:
1. primary 장애 확인 (heartbeat 또는 외부 모니터링)
2. follower 한 대 선택, env 변경:
   ```bash
   EDGE_ROLE=primary
   EDGE_PRIMARY_URL=  # 비움
   ```
3. 해당 edge 재시작 → primary 로 동작
4. 다른 follower 들의 `EDGE_PRIMARY_URL` 을 새 primary 로 갱신 + 재시작

자동 election 비범위. 이유:
- consensus algorithm (Raft) 또는 외부 coordination (etcd/Consul) 필요
- split-brain 방지 위한 quorum / lease 메커니즘 추가 surface
- 본 ADR 은 MVP — 자동 election 은 후속 ADR (예: ADR-031)

## Trade-off — 솔직한 한계

**얻는 것**:
- ✅ Read 부하 분산 (follower N 대로) — read-heavy 워크로드 (CDN 백엔드 등) 가치
- ✅ Primary 죽어도 follower 가 (stale) read 처리 — partial 가용성
- ✅ Write 가용성 영향 X — primary 살아있으면 그대로

**잃는 것**:
- ❌ Write HA — primary 죽으면 write 불가 (manual failover 필요)
- ❌ Read consistency — follower read 는 `pull_interval` 만큼 stale (default 30s)
- ❌ Read-after-write — 클라이언트가 primary PUT 후 follower GET 하면 못 찾을 수
  있음. sticky session 또는 client 가 primary read 해야

**부하**:
- snapshot 사이즈 × N follower × pull_freq = primary outbound. 큰 메타에서는
  WAL/incremental (ADR-019 후속) 로 진화 필요

## 보안

- `/v1/admin/meta/snapshot` 은 메타 전체 (UrlKey 시크릿 포함) 노출
- primary↔follower 통신은 admin 망 격리 + TLS (ADR-029) 필수
- mTLS 켜져있으면 follower→primary 호출도 같은 cert chain 으로 인증

## 비범위 (별도 ADR)

- **ADR-031 자동 leader election** — Raft 또는 etcd 통합으로 자동 failover
- **ADR-019 WAL/incremental** — snapshot pull 대신 변경분만 stream (RPO 분 → 초)
- **Client primary discovery** — 현재 운영자가 primary 주소 client 에 명시. service discovery 통합 별도

## 코드·테스트

- `internal/store/store.go` — `atomic.Pointer[bbolt.DB]` 리팩토링 + `Reload(path)`
  (~30 LOC 변경, 모든 m.db 호출이 m.db.Load() 로)
- `internal/edge/replica.go` — `Role`, `FollowerConfig`, sync loop, write guard (~210 LOC)
- `internal/edge/edge.go` — `Server.Role`, `SetFollowerConfig`, handlers
- `cmd/kvfs-edge/main.go` — env wiring (`EDGE_ROLE/PRIMARY_URL/PULL_INTERVAL`)
- `cmd/kvfs-cli/main.go` — `role` 서브커맨드
- `scripts/demo-rho.sh` — 라이브 검증

총 변경: ~400 LOC.

## Season 3 닫음

운영성 트랙 7개 ep 완료:

| Ep | 주제 | 핵심 |
|---|---|---|
| 7 | Auto-trigger | edge가 스스로 rebalance/GC |
| 8 | EC stripe rebalance | EC 객체도 자동 정렬 |
| 9 | EC repair | dead DN 의 shard 를 K survivors 로 reconstruct |
| 10 | Meta backup | bbolt hot snapshot |
| 11 | DN heartbeat | edge 가 DN 죽음 즉시 인지 |
| 12 | Auto-snapshot scheduler | edge ticker 가 주기 backup + 회전 |
| 13 | Multi-edge HA | read-replica + snapshot-pull |

**Season 3 closed**. 다음은 성능 트랙 (Season 4) — streaming PUT/GET (ADR-017),
content-defined chunking (ADR-018), incremental backup (ADR-019), 자동 leader
election (ADR-031).

---

*kvfs 는 교육적 레퍼런스. 본 ep 의 multi-edge MVP 는 production HA 가 아니다 —
자동 failover, split-brain 방지, write HA 는 별도 작업.*
