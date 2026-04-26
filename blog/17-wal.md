# Episode 17 — WAL: snapshot 사이의 mutation 손실 메우기

> **Series**: kvfs — distributed object storage from scratch
> **Season**: 4 (performance/efficiency) · **Episode**: 4
> **ADR**: [019](../docs/adr/ADR-019-wal-incremental-backup.md) · **Demo**: `./scripts/demo-phi.sh`

---

## RPO 의 기둥뿌리

지금까지 메타 안전망 누적:

| ep | 메커니즘 | RPO 상한 |
|---|---|---|
| 10 | ADR-014 manual snapshot | 운영자 호출 간격 |
| 12 | ADR-016 auto-snapshot scheduler | snapshot interval (default 1h) |
| 13 | ADR-022 multi-edge follower | follower pull interval (default 30s) ≤ snapshot |
| 16 | ADR-031 auto leader election | failover 자동 — 그러나 RPO 는 위와 동일 |

**leader 가 죽으면 마지막 snapshot 이후 write 는 잃는다**. 1h interval 이면 1h
window. ADR-031 가 failover 를 자동화했지만 그 window 자체는 안 줄어듦.

ADR-019 의 목표: 그 window 를 **분 단위 → 초 단위로**.

## WAL 의 단순한 약속

매 mutation 마다 **append-only log file** 에 한 줄 JSON. bbolt commit 직후
fsync 보장. follower 는 마지막 본 seq 이후 entries 를 stream 으로 가져와
자기 bbolt 에 적용.

```
PUT /v1/o/b/k1 → bbolt commit → wal: {seq:1, op:put_object, args:{bucket:b, key:k1, ...}}
DELETE /v1/o/b/k1 → bbolt commit → wal: {seq:2, op:delete_object, args:{bucket:b, key:k1}}
```

snapshot 사이의 모든 변화가 sequential, replayable.

## WAL entry — JSON-lines

```go
type WALEntry struct {
    Seq       int64           `json:"seq"`        // monotonic
    Timestamp time.Time       `json:"ts"`
    Op        string          `json:"op"`         // put_object / delete_object / ...
    Args      json.RawMessage `json:"args"`       // op-specific
}
```

JSON-lines (newline-delimited JSON) — `tail -f`, `jq`, `grep` 모두 동작. binary
format (protobuf 등) 보다 5-10× 크지만 교육 reference 에서는 가독성 우선.

## Append 의 핵심 — bbolt 후 fsync

```go
func (m *MetaStore) putObjectInternal(obj *ObjectMeta, writeWAL bool) error {
    err := m.db.Load().Update(...)  // 1. bbolt commit (msync 됨)
    if err == nil && writeWAL && m.wal != nil {
        m.wal.Append("put_object", obj)  // 2. WAL append + fsync
    }
    return err
}
```

**Order matters**:
- bbolt OK + WAL 실패 → 데이터 살아있고 WAL 만 누락. 다음 snapshot 이 회복.
- bbolt 실패 → WAL 안 함 (정상 에러).

이 순서가 "consistent state ≤ WAL ≤ bbolt" invariant 를 보장 — WAL 이 bbolt 의
앞을 추월할 수 없음.

`Append` 는 매 호출 fsync. write latency 가 ~수 ms 늘어나지만 RPO 는 0 (디스크
손실 직전까지의 entry 가 살아있음).

## Apply API — follower replay

```go
func (m *MetaStore) ApplyEntry(e WALEntry) error {
    switch e.Op {
    case "put_object":
        var obj ObjectMeta
        json.Unmarshal(e.Args, &obj)
        return m.putObjectInternal(&obj, false)  // ← writeWAL=false
    case "delete_object":
        ...
    case "add_runtime_dn":
        ...
    case "remove_runtime_dn":
        ...
    default:
        return fmt.Errorf("apply: unknown op %q", e.Op)
    }
}
```

`writeWAL=false` 이 핵심. follower 가 받은 entry 를 자기 WAL 에 또 append 하면
무한 loop 되거나 seq 충돌. internal version 은 bbolt commit 만 수행.

unknown op 는 silent skip 아니라 error — schema bump (새 mutation 추가) 시
follower 가 즉시 알아챔.

## HTTP endpoints

`GET /v1/admin/wal?since=N`:
- JSON-lines stream of entries with seq > N
- Header `X-KVFS-WAL-Last-Seq` = 현재 last seq (follower 가 다음 since 로 사용)

`GET /v1/admin/wal/info`:
- `{enabled, last_seq, recent: [...]}` 진단용 (마지막 10 entries)

**snapshot endpoint 도 헤더 동봉**:
```
GET /v1/admin/meta/snapshot
  → 200 OK
  → X-KVFS-WAL-Seq: 7
  → application/octet-stream (snapshot bytes)
```

snapshot 은 WAL seq 7 시점의 상태. follower 가 "snapshot 부터 + ?since=7 이후
WAL" 으로 fully catch-up.

## CLI

```
$ kvfs-cli wal info
WAL last seq: 7
recent (7 entries):
  seq=1  2026-04-26T11:09:50.882Z  put_object
  seq=2  2026-04-26T11:09:50.900Z  put_object
  ...
  seq=6  2026-04-26T11:09:51.016Z  delete_object
  seq=7  2026-04-26T11:09:51.030Z  delete_object

$ kvfs-cli wal stream --since 0
{"seq":1,"ts":"2026-04-26T11:09:50.882Z","op":"put_object","args":{"bucket":"demo-phi",...}}
{"seq":2,"ts":"2026-04-26T11:09:50.900Z","op":"put_object","args":{"bucket":"demo-phi",...}}
...
```

audit log 그 자체. 사고 분석에 즉시 사용 가능.

## 라이브 데모 (`./scripts/demo-phi.sh`)

```
--- step 1: PUT 5 objects ---
  PUT obj-1 .. obj-5

--- step 2: DELETE 2 objects ---
  DELETE obj-2, obj-4

--- step 3: kvfs-cli wal info ---
WAL last seq: 7
recent (7 entries):
  seq=1  put_object
  seq=2  put_object
  seq=3  put_object
  seq=4  put_object
  seq=5  put_object
  seq=6  delete_object
  seq=7  delete_object

--- step 4: raw stream (first 3 entries) ---
{"seq":1,"op":"put_object","args":{"bucket":"demo-phi","key":"obj-1","size":30,
 "chunks":[{"chunk_id":"79dd...","size":30,"replicas":["dn1:8080","dn2:8080","dn3:8080"]}],
 "version":1,"created_at":"..."}}
{"seq":2,"op":"put_object","args":{"bucket":"demo-phi","key":"obj-2",...
{"seq":3,"op":"put_object","args":{"bucket":"demo-phi","key":"obj-3",...

--- step 5: snapshot endpoint includes X-KVFS-WAL-Seq header ---
  X-Kvfs-Wal-Seq: 7

✅ φ demo PASS
```

## ApplyAll round-trip 검증 (unit test)

```go
src := store.Open("src.db"); src.SetWAL(wal)
src.PutObject({k1, ...})
src.AddRuntimeDN(":8001")
src.AddRuntimeDN(":8002")
src.PutObject({k2, ...})

entries := wal.Since(0)  // 4 entries

dst := store.Open("dst.db")  // 비어있음
dst.ApplyAll(entries)        // replay

// 검증: dst 에 k1, k2 + 2 DNs 모두 존재
```

snapshot 없이 WAL 만으로도 메타 재구성 가능.

## Trade-off

**얻는 것**:
- ✅ 모든 mutation 의 durable audit log — forensics
- ✅ snapshot 사이의 손실 0 (WAL 가 잡힘) — 후속 follower 통합 시 RPO 분 → 초
- ✅ Optional — 안 켜면 동작 0 변경 (WAL 코드 path 안 탐)
- ✅ JSON-lines = tail/jq/grep 친화

**잃는 것**:
- ❌ 매 mutation 마다 fsync — 쓰기 ~수 ms 추가 (SSD)
- ❌ WAL 무한 증가 — Truncate API 있으나 자동 회전 정책 미정
- ❌ ApplyEntry 가 4 mutation 만 — UpdateChunkReplicas, PutURLKey 등 미커버

**Follower auto-pull 비범위**:
- 본 ep 는 WAL 인프라 + endpoint 까지
- follower 가 자동으로 WAL pull → ApplyAll 하는 통합은 후속 ADR
- 현재는 audit log + 외부 backup 도구 (rclone) 가 사용 가능

## 코드·테스트

- `internal/store/wal.go` — WAL + ApplyEntry/ApplyAll (~270 LOC)
- `internal/store/wal_test.go` — 6 tests (append + LastSeq recovery + truncate +
  WriteSinceTo streaming + store mutation emits + ApplyAll round-trip)
- `internal/store/store.go` — 4 mutation 메서드 internal split
- `internal/edge/edge.go` — handleWAL / handleWALInfo / snapshot 헤더
- `cmd/kvfs-edge/main.go` — `EDGE_WAL_PATH` env wiring
- `cmd/kvfs-cli/main.go` — `wal {info,stream}` 서브커맨드
- `scripts/demo-phi.sh` — WAL append + snapshot header 라이브

총 변경: ~700 LOC.

## 다음 ep ([P4-05](../docs/FOLLOWUP.md))

- **EC streaming** (ADR-017 follow-up): per-stripe streaming, encoder 인터페이스 변경
- **EC + CDC 조합** (ADR-018 follow-up): variable shard size 디자인
- **진짜 Raft log replication** (ADR-031 follow-up): WAL 기반 follower 자동 pull,
  write loss window 제거 (본 ADR-019 인프라 활용)

---

*kvfs 는 교육적 레퍼런스. 본 ep 의 WAL 은 단순 JSON-lines — production 등급은
binary encoding (protobuf), batched fsync, log truncation 등 추가 작업 필요.*
