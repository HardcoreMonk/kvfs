# Episode 27 — WAL log compaction: snapshot 직후 truncate

> **Series**: kvfs — distributed object storage from scratch
> **Season**: P4-09 wave · **Episode**: 14
> **연결**: `internal/store/scheduler.go` `SnapshotScheduler.Run` · `internal/store/wal.go` `Truncate`

---

## WAL 이 자라는 이유

[Ep.17 WAL](17-wal.md) 의 모델: 매 mutation = 한 줄 JSON append. 하루 100k mutation = 100k 줄. 한 줄 평균 ~500 byte → 50 MB/일. 1년 = 18 GB. 메타 자체 (bbolt) 가 100 MB 인데 WAL 이 100배.

해결 방향:
- snapshot 이 모든 상태를 capture → snapshot 시점 이전의 WAL 은 redundant.
- `WAL.Truncate()` ([Ep.17](17-wal.md)) 가 이미 존재 — 호출하기만 하면 됨.

## 문제: 언제 truncate 하나

순서 잘못 잡으면 데이터 잃음:

```
WRONG:
  1. Truncate WAL  (durable snapshot 없는 상태로 reset)
  2. crash         (snapshot 안 만들어짐)
  3. recover → WAL 비어있음, snapshot 도 없음 → 데이터 손실

CORRECT:
  1. Snapshot durable (atomic temp → rename, fsync)
  2. snapshot 안에 WAL_seq 포함 → "snapshot is complete through seq N"
  3. Truncate WAL    (이제 안전)
```

## SnapshotScheduler 의 hook

[Ep.12 auto-snapshot](12-auto-snapshot.md) 의 `SnapshotScheduler.Run` 이 ticker 마다:

```go
1. snapshot 작성 (atomic temp + rename)
2. retention prune (mtime 기준 가장 오래된 것부터 제거)
3. (NEW) WAL truncate
```

코드 추가:

```go
func (s *SnapshotScheduler) runOnce() {
    if err := s.snapshot(); err != nil { return }
    s.prune()
    // ↓ 추가
    if w := s.store.WAL(); w != nil {
        if _, err := w.Truncate(); err != nil {
            s.recordErr(fmt.Errorf("wal truncate: %w", err))
        }
    }
}
```

`Truncate` 가 prev seq 를 돌려주므로 로그에 "WAL truncated through seq N" 같은 기록 가능.

## 안전 보장

`snapshot()` 이 fsync 까지 완료한 후에만 `Truncate()` 호출. snapshot 파일 = `bbolt tx.WriteTo` 결과 + atomic rename. POSIX rename 은 atomic, 같은 디렉토리 내에서. crash 가 그 사이 발생하면:

- snapshot temp 만 있는 상태 → recovery 시 partial file 무시, 이전 snapshot + 기존 WAL 사용
- snapshot rename 완료 + truncate 미실행 → recovery 시 새 snapshot + WAL replay (idempotent — 같은 mutation 두 번 적용해도 안전)

worst case = WAL 두 번 replay. 데이터 손실 0.

## 보존 기간 vs WAL 사이즈

interval 짧게 잡으면 WAL 작음 + snapshot 자주 (디스크 I/O ↑). 트레이드오프:

| EDGE_SNAPSHOT_INTERVAL | WAL 크기 (100k mut/일 기준) | snapshot rate |
|---|---|---|
| 1h | ~2 MB | 24×/일 |
| 6h | ~12 MB | 4×/일 |
| 24h | ~50 MB | 1×/일 |

대부분 1~6h 면 WAL 이 메가바이트 단위로 안정. Lambda follower 의 WAL pull 도 이 크기만 받음 — 빠름.

## Follower 와의 시점 동기화

문제: leader 가 Truncate 했는데 follower 가 아직 그 seq 에 도달 못 했으면? snapshot pull 로 catch up — `follower 가 GET /v1/admin/meta/snapshot` → 새 snapshot + WAL_seq header → follower 의 lastSeq 점프.

```
T0: leader snapshot (WAL through seq 100)
T1: leader Truncate WAL → seq 0
T2: follower 가 GET /v1/admin/wal?since=50 시도 → 50~100 가 사라짐 → fall back to snapshot pull
T3: follower 가 snapshot 받고 lastSeq=100 으로 set
T4: 이후 WAL pull 정상
```

`Ep.17 follow-up (Ep.5)` 의 follower auto-pull 에 이 fallback 이 이미 있음.

## prune 와의 상호작용

snapshot retention (`EDGE_SNAPSHOT_KEEP=N`) 이 가장 오래된 snapshot 을 제거할 때, 그 snapshot 에만 의존하던 WAL 구간이 있으면 데이터 사라짐? 그렇지 않다 — Truncate 는 가장 최신 snapshot 까지만 안전을 보장. 더 오래된 snapshot 은 historical 백업이지 recovery 의 단일 source 가 아님 (오래된 snapshot 으로 recover 하면 그 시점 이후 mutation 모두 잃음 — 의도된 동작).

## 측정

`/metrics`:
- `kvfs_wal_last_seq` — Truncate 직후 0 으로 reset → 0 부터 다시 증가
- 디스크 사용량: WAL 파일 크기 = `du -h wal.log`. interval 마다 0 으로 떨어졌다가 다시 자람.

```
00:00 wal.log = 2.3 MB
00:30 wal.log = 4.1 MB
01:00 snapshot fired → wal.log = 0 → 50 KB (그 30초간 누적)
01:30 wal.log = 1.8 MB
...
```

## 한 줄 요약

scheduler 가 snapshot 끝나면 WAL truncate. 데이터 안전 (snapshot 이 단일 source), WAL 무한 성장 방지, follower 의 fallback 경로는 기존 snapshot pull 그대로.

코드 변경 ~5 LOC. 운영 효과 18 GB/년 → bounded.

## 다음

[Ep.28 strict vs transactional](28-strict-vs-txn.md): replication mode 두 종류 — informational 503 vs 진짜 commit-before-quorum.
