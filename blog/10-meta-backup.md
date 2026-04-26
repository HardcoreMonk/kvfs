# Episode 10 — 메타 안전망: bbolt snapshot + offline restore

> **Series**: kvfs — distributed object storage from scratch
> **Season**: 3 (operability) · **Episode**: 4
> **ADR**: [014](../docs/adr/ADR-014-meta-backup.md) · **Demo**: `./scripts/demo-xi.sh`

---

## 왜 메타 안전망이 첫 번째인가

분산 스토리지에서 **chunk 데이터** 는 의도적으로 중복화돼있다 — 3-way
replication (ADR-005/006) 또는 EC(K+M) (ADR-008). 한두 DN 이 죽어도 다른 곳에
사본이 살아있다. 운영자는 마음이 편하다.

그런데 **메타** 는 단일 파일이다. kvfs-edge 의 `edge.db` (bbolt) 하나가 망가지면
"어떤 객체가 어떤 chunk 묶음인가" 는 사라진다. chunk 파일들은 disk 에 살아있어도
key 로 GET 할 수 없다. 사실상 클러스터 dead.

Season 3 (운영성) 의 첫 ep 는 이 비대칭성을 메우는 것 — 가장 단순한 안전망부터.

## bbolt 가 주는 무료 점심: hot snapshot

bbolt 는 single-writer + many-reader 트랜잭션 모델이다. read 트랜잭션은
**snapshot isolation** 을 제공한다 — 트랜잭션 시작 시점의 page tree 를
그대로 본다. 그동안 writer 가 page 를 갱신해도 (copy-on-write) old page 는
read tx 가 끝날 때까지 살아있다.

여기에 `tx.WriteTo(io.Writer)` 메서드가 더해지면, 한 줄로 hot snapshot 이 된다:

```go
func (m *MetaStore) Snapshot(w io.Writer) (int64, error) {
    var n int64
    err := m.db.View(func(tx *bbolt.Tx) error {
        var werr error
        n, werr = tx.WriteTo(w)
        return werr
    })
    if err != nil {
        return n, fmt.Errorf("snapshot: %w", err)
    }
    return n, nil
}
```

- `db.View` 는 read tx 안에서 실행
- `tx.WriteTo(w)` 는 page 들을 순서대로 byte stream 으로 출력
- writer 들은 동시에 PUT 가능 — 새 page 가 만들어져도 read tx 의 view 는 안 바뀜

결과: **edge 동작 중 어느 시점이든 일관된 메타 사본** 을 얻는다. 글로벌 락 X,
PUT/GET hold X, 분 단위 다운타임 X.

## HTTP endpoint — 그냥 stream 하면 끝

```go
func (s *Server) handleMetaSnapshot(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "application/octet-stream")
    w.Header().Set("Content-Disposition", `attachment; filename="kvfs-meta-snapshot.bbolt"`)
    if _, err := s.Store.Snapshot(w); err != nil {
        s.Log.Error("meta snapshot failed", slog.String("err", err.Error()))
    }
}
```

`http.ResponseWriter` 가 곧 `io.Writer`. snapshot 함수에 그대로 넘기면
client 가 `curl http://edge:8000/v1/admin/meta/snapshot -o backup.bbolt` 한 줄로
수십 MB ~ 수 GB 메타를 받아간다. 메모리 버퍼링 X, streaming.

## CLI: `kvfs-cli meta snapshot`

```
$ kvfs-cli meta snapshot --edge http://localhost:8000 --out backup-2026-04-26.bbolt
snapshot saved: backup-2026-04-26.bbolt (24576 bytes)
```

내부적으로 `GET /v1/admin/meta/snapshot` → 파일에 streaming copy → atomic
rename. 5분 client timeout 이 default 라 ~수 GB 까지 OK.

## Restore = offline (의도적 제약)

In-process restore 는 **하지 않았다**. 이유 두 가지:

1. **bbolt file lock conflict** — 같은 파일을 두 개 process 가 동시에 열면 깨짐.
2. **메모리 invalidation** — edge 가 cache 한 url-key 시크릿, runtime DN 셋
   등이 갑자기 다른 메타와 어긋난다.

운영자 워크플로우 (3 단계):

```bash
# 1. edge 정지
docker stop edge

# 2. 스냅샷을 데이터 디렉토리에 덮어쓰기 (자동 백업 동반)
kvfs-cli meta restore --from backup.bbolt --datadir /var/lib/kvfs-edge/

# 3. edge 재시작
docker start edge
```

`meta restore` 는 안전 장치를 둔다:

- **Lock 시도** — destination `edge.db` 를 잠시 열어본다. 다른 프로세스가 잡고
  있으면 (= edge 가 안 죽었다) `permission denied`/`timeout` → 즉시 abort.
- **Auto backup** — 기존 `edge.db` 를 `edge.db.bak` 으로 rename 후 덮어쓴다.
  실수로 prod 메타를 날리는 사고 방지. `--keep-backup=false` 로 끔.

## meta info — capacity check

```
$ kvfs-cli meta info
objects:        3 (EC: 0, plain: 3)
plain chunks:   3
EC stripes:     0 (shards: 0)
DNs (seed):     0
DNs (runtime):  3
URL keys:       1
bbolt size:     24576 bytes (24.0 KiB)
```

snapshot 직전·직후 동일성 검증 + 평소 capacity 모니터링 용도. `--json` 으로
script-friendly.

## 라이브 데모 출력 (`./scripts/demo-xi.sh`)

```
--- step 1: PUT 3 objects ---
  PUT obj-1 (27 bytes)
  PUT obj-2 (27 bytes)
  PUT obj-3 (27 bytes)

--- step 2: meta info before snapshot ---
objects:        3 (EC: 0, plain: 3)
plain chunks:   3
DNs (runtime):  3
URL keys:       1
bbolt size:     24576 bytes (24.0 KiB)

--- step 3: take snapshot ---
snapshot saved: /tmp/kvfs-meta-snapshot-demo-xi.bbolt (24576 bytes)

--- step 4: simulate disaster — stop edge + delete bbolt ---
  edge stopped, edge.db removed

--- step 5: restore snapshot into the volume ---
  snapshot copied to edge-data volume

--- step 6: restart edge ---

--- step 7: meta info after restore ---
objects:        3 (EC: 0, plain: 3)
plain chunks:   3
DNs (runtime):  3
URL keys:       1
bbolt size:     24576 bytes (24.0 KiB)

--- step 8: GET each object back ---
  GET obj-1 → hello-snapshot-1-1777189895…
  GET obj-2 → hello-snapshot-2-1777189895…
  GET obj-3 → hello-snapshot-3-1777189895…

✅ ξ demo PASS
```

3 객체 PUT → snapshot → bbolt 강제 삭제 → snapshot 복원 → 모든 객체 GET 성공.
chunk 데이터는 DN volume 에 그대로 살아있었기에 restored meta 가 가리키는 위치를
그대로 읽어왔다.

## 비범위 (다음 ep 후보)

- **자동 스케줄러** — 외부 cron + curl 권장. ADR-013 의 in-process ticker 패턴을
  차용해 in-edge auto-snapshot 추가 가능
- **WAL / incremental** — 분 단위 RPO 가 필요하면 별도 ADR (ADR-016 후보)
- **HA (multi-edge)** — single-writer bbolt 로 active-active 불가. ADR-022 가
  meta sync 다룰 예정
- **Snapshot 외부 보관 정책** — 운영자 몫 (S3/NAS sync). 본 ADR 은 endpoint 만 제공

## 코드·테스트

- `internal/store/snapshot.go` — Snapshot + Stats (~80 LOC)
- `internal/store/snapshot_test.go` — round-trip 검증 (3 객체 + EC 객체 → snapshot
  → 새 bbolt → 모든 레코드 복원 확인) + Stats counter 검증
- `internal/edge/edge.go` — `handleMetaSnapshot` / `handleMetaInfo` (~20 LOC)
- `cmd/kvfs-cli/main.go` — `cmdMeta` 서브커맨드 (snapshot/info/restore, ~120 LOC)

총 변경: ~250 LOC, 0 외부 의존, bbolt 내장 기능 활용.

## 다음 ep 후보 ([P3-10](../docs/FOLLOWUP.md))

- **ADR-022 — Multi-edge leader election** (HA 본격 작업)
- **ADR-030 — DN heartbeat** (registry 자동 dead 감지)
- **ADR-017 — Streaming PUT/GET** (io.ReadAll → io.Reader 진짜 streaming)
- **ADR-018 — Content-defined chunking** (rabin/buzhash 비정렬 dedup)

---

*kvfs 는 교육적 레퍼런스. 프로덕션 배포 X. 본 ep 의 backup/restore 패턴은 단일
인스턴스 한정 — multi-edge HA 는 별도 작업.*
