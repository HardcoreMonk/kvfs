# ADR-019 — WAL / incremental backup (write-ahead log of metadata mutations)

## 상태
Accepted · 2026-04-26

## 맥락

ADR-014/016 의 snapshot 기반 backup 은 **RPO ≤ snapshot interval** 이 한계.
default 1h interval 이면 leader 죽기 직전 1h 동안의 write 는 잃는다.

ADR-031 (auto leader election) 이 failover 를 자동화했지만 그 데이터 손실
window 는 그대로 — 새 leader 가 옛 leader 의 마지막 snapshot 시점까지만 알고
있다.

## 결정

### 메타 mutation 의 append-only WAL

`internal/store/wal.go` 새 모듈 (~270 LOC):

```go
type WALEntry struct {
    Seq       int64           `json:"seq"`
    Timestamp time.Time       `json:"ts"`
    Op        string          `json:"op"`        // put_object / delete_object / add_runtime_dn / ...
    Args      json.RawMessage `json:"args"`      // op-specific JSON
}

type WAL struct {
    path string
    mu   sync.Mutex
    f    *os.File
    w    *bufio.Writer
    seq  atomic.Int64
}

func (w *WAL) Append(op string, args any) (seq int64, err error)  // bbolt commit 후 fsync
func (w *WAL) Since(sinceSeq int64) ([]WALEntry, error)            // 읽기
func (w *WAL) WriteSinceTo(sinceSeq int64, dst io.Writer) (int, error)  // streaming
func (w *WAL) Truncate() (prevSeq int64, err error)                 // snapshot 시 회전
func (w *WAL) LastSeq() int64
```

각 entry = 한 줄 JSON. Append 마다 fsync — durability 보장 (bbolt commit 직후
실패 시에도 WAL 에 entry 가 남아있음).

### MetaStore 통합

mutation 메서드 4개 (PutObject / DeleteObject / AddRuntimeDN / RemoveRuntimeDN)
를 internal version 으로 분리:

```go
func (m *MetaStore) PutObject(obj) error {
    return m.putObjectInternal(obj, true)  // writeWAL=true
}

func (m *MetaStore) putObjectInternal(obj, writeWAL bool) error {
    err := m.db.Load().Update(...)         // 1. bbolt commit
    if err == nil && writeWAL && m.wal != nil {
        m.wal.Append("put_object", obj)    // 2. WAL append (after commit)
    }
    return err
}
```

**Order matters**: bbolt commit 먼저, WAL append 나중. 둘 다 실패 가능하므로:
- bbolt OK + WAL 실패 → 데이터는 살아있고 WAL 만 누락 (다음 snapshot 이 회복)
- bbolt 실패 → 둘 다 안 함 (정상 에러 경로)

### Apply API — follower replay

```go
func (m *MetaStore) ApplyEntry(e WALEntry) error {
    switch e.Op {
    case "put_object": ... putObjectInternal(obj, false)   // writeWAL=false
    case "delete_object": ... deleteObjectInternal(b, k, false)
    case "add_runtime_dn": ... addRuntimeDNInternal(addr, false)
    case "remove_runtime_dn": ... removeRuntimeDNInternal(addr, false)
    default: error  // unknown op = noisy fail (schema bump signal)
    }
}
```

`writeWAL=false` 로 호출 — follower 가 받은 entry 를 재호출하면 안 됨 (loop 방지).

### HTTP endpoints

- `GET /v1/admin/wal?since=N` — JSON-lines stream of entries with seq > N.
  Header `X-KVFS-WAL-Last-Seq` = 현재 last seq
- `GET /v1/admin/wal/info` — `{enabled, last_seq, recent: [...]}` 진단용

snapshot endpoint 도 `X-KVFS-WAL-Seq` 헤더로 snapshot 시점 의 WAL seq 동봉.
follower 는 이를 통해 "이 snapshot 부터 + 이후 WAL since=X" 로 catch-up 가능.

### 환경 변수

- `EDGE_WAL_PATH` — WAL 파일 경로. 비어있으면 WAL 비활성 (default)

### CLI

- `kvfs-cli wal info --edge URL` — last_seq + recent tail
- `kvfs-cli wal stream --edge URL --since N` — JSON-lines streaming dump

### Follower auto-pull integration 비범위

본 ADR 은 **WAL 인프라 + endpoint** 까지. follower 가 자동으로 WAL pull 해서
ApplyAll 하는 통합은 후속 ADR. 현재는:
- ADR-022 follower sync = snapshot pull only (그대로 유지)
- WAL 은 audit log + 외부 도구 (rclone) backup + 미래 incremental sync 의 토대

## 결과

**긍정**:
- 모든 mutation 이 durable audit log 로 추적 — 사고 분석·forensics 가능
- snapshot 간 손실 보완 토대 마련 — 후속 follower 통합 시 RPO 분 → 초
- WAL 은 optional — 안 켜면 동작 0 변경 (기존 모든 데모 그대로 PASS)
- 6 신규 unit tests PASS (append + Since + LastSeq recovery + truncate +
  WriteSinceTo streaming + store mutation emits + ApplyAll round-trip)

**부정**:
- 매 mutation 마다 fsync — 쓰기 지연 ~수 ms 추가 (SSD 기준). 워크로드에 따라 batched flush 옵션 후속
- WAL 파일 무한 증가 — Truncate API 는 있으나 호출하는 정책 미정 (snapshot scheduler 통합 후속)
- ApplyEntry 는 4 mutation 만 — UpdateChunkReplicas / UpdateShardReplicas /
  PutURLKey 등은 WAL 안 됨 (운영 영향 적음, 후속 확장)

**트레이드오프**:
- JSON-lines = 사람-친화 + 작은 의존성. binary format (protobuf) 보다 5-10× 큼.
  교육 reference 에서는 가독성이 중요해 JSON 채택
- per-entry fsync = max durability. batched 는 RPO 향상 vs latency 향상의 trade-off

## 보안 주의

- WAL 은 **모든 메타 mutation** 노출 — UrlKey 등 시크릿은 args 에 hex 만 들어가지만
  여전히 reproducible. 파일 권한 0600 + 외부 sync 시 암호화 필수
- WAL endpoint (/v1/admin/wal) 도 admin 망 격리 가정

## 데모 시나리오 (`scripts/demo-phi.sh`)

WAL enabled 한 edge → PUT 5개 / DELETE 2개 → `kvfs-cli wal info` 로 7 entries
확인 → `kvfs-cli wal stream --since 0` 으로 raw JSON-lines 스트리밍 → 두 번째
edge 가 ApplyAll 로 동일 상태 재생.

## 관련

- `internal/store/wal.go` — WAL + ApplyEntry/ApplyAll (~270 LOC)
- `internal/store/wal_test.go` — 6 tests
- `internal/store/store.go` — 4 mutation 메서드 internal split
- `internal/edge/edge.go` — handleWAL / handleWALInfo / snapshot 헤더
- `cmd/kvfs-edge/main.go` — `EDGE_WAL_PATH` env wiring
- `cmd/kvfs-cli/main.go` — `wal {info,stream}` 서브커맨드
- `scripts/demo-phi.sh` — WAL append + replay 라이브
- 후속 ADR 후보: follower auto WAL-pull, batched fsync, WAL → snapshot 자동 회전,
  더 많은 mutation op 커버 (UpdateChunkReplicas 등)
