# ADR-022 — Multi-edge HA: read-replica follower (snapshot-pull)

## 상태
Accepted · 2026-04-26

## 맥락

지금까지 모든 운영성 ADR (013/014/016/024/025/027/028/030) 이 **single-edge**
가정. edge 가 죽으면 클러스터 전체가 read/write 불가.

bbolt = single-writer file lock. 두 edge 가 같은 bbolt 파일을 동시에 열 수
없다. 즉 진짜 active-active 는 별도 분산 메타 (etcd / Raft) 또는 쓰기를 한
edge 로 funnel 하는 모델 필요.

본 ADR 의 목표는 **MVP 수준 read-replica HA**:
- write 처리 1대 (primary), read 처리 N대 (primary + followers)
- follower 는 primary snapshot 을 주기 pull 해서 자기 bbolt 갱신
- failover = 운영자 명시 절차 (자동 election 비범위, 후속 ADR)

이 정도만 해도 read 부하 분산 + primary 단일 장애 시 read 만이라도 살아있는 의미.

## 결정

### Roles + 환경 변수

```
EDGE_ROLE=primary          (default)
EDGE_ROLE=follower
  EDGE_PRIMARY_URL=http://primary:8000      (required)
  EDGE_FOLLOWER_PULL_INTERVAL=30s            (default)
```

### Primary 동작
변경 없음 — 기존 모든 데이터/관리 endpoint 정상 동작.

### Follower 동작

**초기화 시**:
- `SetFollowerConfig` → `Server.Role = RoleFollower`
- `StartFollowerSync(ctx)` → 백그라운드 goroutine 시작 (즉시 1회 + 매 interval)

**Sync loop** (`runFollowerSync`):
1. `GET <primary>/v1/admin/meta/snapshot` → `<datadir>/edge.db.sync` 파일에 streaming
2. 다운로드 성공 시 `MetaStore.Reload(<datadir>/edge.db.sync)` 호출
3. `Reload` 는 새 bbolt 를 열고 `atomic.Pointer[bbolt.DB].Swap` → 즉시 활성, 옛
   bbolt 는 in-flight 트랜잭션이 끝난 뒤 비동기 close
4. 실패는 stats 에 기록만 + 다음 tick

**Write rejection** (`rejectIfFollowerWrite` middleware):
- `PUT/POST/DELETE/PATCH /v1/o/...` → `503 Service Unavailable` + `X-KVFS-Primary: <url>` 헤더
- Read (`GET /v1/o/...`) 정상 — local (stale) bbolt 사용
- Admin endpoint (`/v1/admin/...`) 영향 없음 — kid rotation, dns admin 등은 follower 에 직접 호출 무의미하나 차단도 X

### 핵심 구현 — atomic Pointer hot-swap

```go
// internal/store/store.go
type MetaStore struct {
    db atomic.Pointer[bbolt.DB]
}

func (m *MetaStore) Reload(path string) error {
    newDB, err := openInitialized(path)
    if err != nil { return err }
    old := m.db.Swap(newDB)
    if old != nil { go func() { _ = old.Close() }() }
    return nil
}
```

모든 read/write 메서드 (`m.db.View(...)` → `m.db.Load().View(...)`) 는 매 호출
시 현재 pointer 를 읽음. swap 시점:
- 새 read 는 신 pointer 사용
- 진행 중 read (ongoing tx) 는 old pointer 그대로 유지 — bbolt.Close 는 in-flight
  tx 가 끝나야 반환되므로 비동기 goroutine 에서 close

→ **read interruption 0**.

### Failover (manual MVP)

운영자 절차:
1. primary 장애 확인 (heartbeat / 외부 모니터링)
2. follower 한 대 선택 → `EDGE_ROLE=primary`, `EDGE_PRIMARY_URL=` 비워서 환경 갱신
3. 해당 edge 재시작 → primary 로 동작
4. 다른 follower 들의 `EDGE_PRIMARY_URL` 을 새 primary 로 갱신 + 재시작

비자동 election 의 이유:
- consensus algorithm (Raft) 또는 외부 coordination (etcd/Consul) 필요
- split-brain 방지 위한 quorum / lease 메커니즘 추가 surface
- 본 ADR 은 MVP — 진짜 자동 election 은 별도 ADR (e.g., ADR-031)

### 보안

- `/v1/admin/meta/snapshot` 은 메타 전체 (UrlKey 시크릿 포함) 노출
  → primary↔follower 통신은 반드시 admin 네트워크 격리 + TLS (ADR-029)
- mTLS 가 켜져 있으면 follower→primary 호출도 같은 cert chain 으로 인증

## 결과

**긍정**:
- Read 부하 분산 (follower N 대로) — 캐시·CDN·search 같은 read-heavy 워크로드에 유효
- Primary 죽어도 follower 가 (stale) read 처리 가능 — partial 가용성
- atomic.Pointer hot-swap 으로 reload 중 read interruption 0
- 새 의존성 0 (etcd 등 외부 coord 불필요)

**부정**:
- Write 가용성은 single-primary 그대로 — primary 죽으면 write 불가
- Follower read 는 `pull_interval` 만큼 stale (default 30s)
- Failover 수동 — 운영자 개입 필요
- Snapshot 사이즈 × N follower × pull_freq = primary outbound 부하 — 큰 메타에서는
  WAL/incremental (ADR-019 후속) 로 진화 필요

**트레이드오프**:
- `pull_interval` 짧게 = freshness ↑, primary 부하 ↑. 30s default 는 metadata
  스토리지 패턴으로 reasonable
- read-after-write consistency 깨짐: 클라이언트가 primary 에 PUT 후 follower 에서
  GET 하면 못 찾을 수 있음 (다음 sync 까지). sticky session / read-your-writes
  를 원하면 client 측에서 primary 직접 read 또는 follower 로 read 하지 말 것

## 보안 주의

- Primary URL 은 admin 망에서만 reachable — public 노출 금지 (ADR-014 와 동일)
- Follower 의 `edge.db.sync` 파일은 transient — 다음 reload 시 덮어쓰지만, 외부
  사용자 read 권한 0o600

## 비범위 (별도 ADR 후보)

- 자동 leader election — ADR-031 (Raft 또는 etcd 통합)
- WAL/incremental backup — ADR-019 (snapshot 부하 감소)
- Client-side primary discovery — 현재 운영자가 primary 주소 client 에 명시.
  service discovery (DNS SRV / Consul) 는 별도

## 관련

- `internal/store/store.go` — `atomic.Pointer[bbolt.DB]` 리팩토링 + `Reload(path)`
- `internal/edge/replica.go` — `Role`, `FollowerConfig`, sync loop, write guard (~200 LOC)
- `internal/edge/edge.go` — `Server.Role` + `SetFollowerConfig` + handlers
- `cmd/kvfs-edge/main.go` — env wiring (`EDGE_ROLE/PRIMARY_URL/PULL_INTERVAL`)
- `cmd/kvfs-cli/main.go` — `role` 서브커맨드
- `scripts/demo-rho.sh` — primary + follower live: PUT to primary → wait sync →
  GET from follower (stale OK) → PUT to follower → 503 + X-KVFS-Primary
