# Episode 12 — Auto-snapshot scheduler: edge가 자기 backup도 알아서

> **Series**: kvfs — distributed object storage from scratch
> **Season**: 3 (operability) · **Episode**: 6
> **ADR**: [016](../docs/adr/ADR-016-auto-snapshot-scheduler.md) · **Demo**: `./scripts/demo-pi.sh`

---

## Manual endpoint은 실수의 자유를 준다

ADR-014 (Ep.10) 가 manual snapshot endpoint 를 줬다. 운영자는 외부 cron 으로
주기 backup 을 자동화할 수 있다:

```cron
0 * * * * curl -s http://edge:8000/v1/admin/meta/snapshot > /backups/$(date +\%F-\%H).bbolt
```

작동한다. 그러나 외부 의존이 늘어난다:

- `cron` 서비스가 실수로 disabled (시스템 업그레이드 후)
- timezone 설정 오류로 매일 같은 시각에 안 뜨고 한 번도 못 돌아감
- 운영자 user 권한 변경 — 백업 디렉토리 write fail
- k8s CronJob 의 RBAC / ServiceAccount 갱신 누락

**모두 운영자가 알기 전까지 backup 누락**. ADR-014 endpoint 는 거기 있는데
정작 호출이 안 된다.

ADR-016 의 목표: edge 본인이 알아서 backup 한다. ADR-013 의 in-edge ticker 패턴
그대로 — 외부 의존 0.

## 디자인 — ADR-013 패턴 재활용

```go
// internal/store/scheduler.go
type SnapshotScheduler struct {
    store    *MetaStore
    dir      string
    interval time.Duration
    keep     int
    // ... stats fields
}

func (s *SnapshotScheduler) Run(stop <-chan struct{}) {
    if s.interval <= 0 { return }
    os.MkdirAll(s.dir, 0o755)
    s.runOnce()  // immediate cold-start backup
    t := time.NewTicker(s.interval)
    defer t.Stop()
    for {
        select {
        case <-stop:
            return
        case <-t.C:
            s.runOnce()
        }
    }
}
```

매 tick:

1. `snap-YYYYMMDDTHHMMSSZ.bbolt.tmp` 에 atomic write (`store.Snapshot()` 호출)
2. `rename(tmp → snap-YYYYMMDDTHHMMSSZ.bbolt)` (POSIX atomic rename)
3. dir 정리 — mtime 정렬 후 `keep` 초과분 삭제
4. 에러는 stats 에 기록만, loop 죽이지 않음

ADR-013 auto-trigger 와 정확히 같은 모양. select / for / Ticker — Go 의
`time.Ticker` 는 정직하다. 수십 줄로 자동화가 끝난다.

## 회전 (rotation) — 디스크 폭주 방지

```go
func (s *SnapshotScheduler) prune() error {
    var snaps []snap
    for _, e := range entries {
        name := e.Name()
        if !strings.HasPrefix(name, snapshotPrefix) || !strings.HasSuffix(name, snapshotSuffix) {
            continue  // ⭐ 다른 파일 (README 등) 절대 안 건드림
        }
        // ...
    }
    sort.Slice(snaps, func(i, j int) bool { return snaps[i].mod.After(snaps[j].mod) })
    for _, sn := range snaps[s.keep:] {
        os.Remove(sn.path)
    }
}
```

핵심 안전장치:

- **prefix + suffix 매칭** — `snap-`로 시작하고 `.bbolt`로 끝나는 파일만 후보.
  운영자가 같은 디렉토리에 README.txt 둬도 prune 이 안 건드림 (테스트로 검증)
- **mtime 정렬** — newest first. `keep` 만큼 보존, 나머지 삭제
- **atomic op** — temp+rename 으로 partial write 노출 X

## env 만으로 켜기

```bash
docker run kvfs-edge:dev \
  -e EDGE_DATA_DIR=/var/lib/kvfs-edge \
  -e EDGE_SNAPSHOT_DIR=/var/lib/kvfs-edge/snapshots \
  -e EDGE_SNAPSHOT_INTERVAL=1h \
  -e EDGE_SNAPSHOT_KEEP=24 \
  ...
```

`EDGE_SNAPSHOT_DIR` 비어있으면 모듈 로드 X (zero-cost off). 설정하면 즉시 첫
backup + 매 시각 hourly. 24개 보존 = 하루치.

## Endpoint + CLI

`GET /v1/admin/snapshot/history`:
```json
{
  "enabled": true,
  "dir": "/var/lib/kvfs-edge/snapshots",
  "interval": "1h",
  "keep": 24,
  "last_run": "2026-04-26T08:23:49Z",
  "last_size_bytes": 24576,
  "total_runs": 6,
  "total_errors": 0,
  "history": [
    {"name": "kvfs-meta-snap-20260426T082349Z.bbolt", "size_bytes": 24576, "mod_time": "..."},
    ...
  ]
}
```

`kvfs-cli meta history`:
```
dir:          /var/lib/kvfs-edge/snapshots
interval:     2s
keep:         3
total runs:   6 (errors: 0)
last run:     2026-04-26T08:23:49Z (24576 bytes)

history (3 files, newest first):
  kvfs-meta-snap-20260426T082349Z.bbolt             24576 bytes  2026-04-26T08:23:49Z
  kvfs-meta-snap-20260426T082347Z.bbolt             24576 bytes  2026-04-26T08:23:47Z
  kvfs-meta-snap-20260426T082345Z.bbolt             24576 bytes  2026-04-26T08:23:45Z
```

운영자 health check 1줄. error count 가 0 이 아니면 즉시 alert.

## 외부 백업은 의도적으로 비범위

`EDGE_SNAPSHOT_DIR` 만 dir 에 fresh copy 보장. **외부 보관** (S3 / NAS) 은
운영자 별도 도구 권장:

```bash
# crontab on backup host
*/15 * * * * rclone sync edge:/var/lib/kvfs-edge/snapshots s3://my-backups/kvfs/
```

이유: edge 가 외부 storage credential 을 들면:
- AWS access key / Azure SAS / GCS service account 키 보관 surface 폭증
- ADR-029 (TLS) · ADR-028 (urlkey) 와 동일한 minimalism 위배
- 외부 vendor lock-in (S3 specific code 등)

edge 는 자기 데이터만 본다. 외부 sync 는 외부 도구 — 잘 분리된 관심사.

## 라이브 데모 (`./scripts/demo-pi.sh`)

interval=2s, keep=3 (회전 빨리 보려고 짧게):

```
--- step 2: wait 8s for several scheduler ticks ---

--- step 3: meta history — should show 3 files (rotation working) ---
dir:          /var/lib/kvfs-edge/snapshots
interval:     2s
keep:         3
total runs:   5 (errors: 0)

history (3 files, newest first):
  kvfs-meta-snap-20260426T082347Z.bbolt   24576 bytes  ...
  kvfs-meta-snap-20260426T082345Z.bbolt   24576 bytes  ...
  kvfs-meta-snap-20260426T082343Z.bbolt   24576 bytes  ...

✅ π demo PASS
```

5 ticks 돌고 keep=3 적용 → newest 3 file 만 남음. 회전 작동 확인.

## RPO 트레이드오프

| interval | 디스크 부하 | 데이터 손실 최대 |
|---|---|---|
| `1m` | 매분 ~수십 KB write | 1분 |
| `1h` (default) | 매시 ~수 MB write | 1시간 |
| `1d` | 매일 ~수십 MB write | 24시간 |

정답은 워크로드 의존. 1h default 는 homelab / small server 에 reasonable. 실
프로덕션에선 분 단위 RPO 필요 시 ADR-019 (WAL/incremental) 후속.

## 비범위 (다음 ep 후보)

- **WAL / incremental backup** — bbolt write tx log 를 별도로 stream 해
  RPO 를 분 → 초 단위로. 별도 ADR
- **외부 sync** — S3/NAS adaptors. edge 가 들지 않고 외부 도구로 (rclone 등)
- **Snapshot encryption** — 현재 plain bbolt. KMS 통합은 별도 ADR
- **Multi-edge sync** — ADR-022 multi-edge HA 와 함께 다룰 가능성

## 코드·테스트

- `internal/store/scheduler.go` — `SnapshotScheduler` (~190 LOC)
- `internal/store/scheduler_test.go` — 4 tests PASS (one-run / prune retention
  / fresh stats / non-snapshot 보존)
- `internal/edge/edge.go` — `StartSnapshotScheduler`, `handleSnapshotHistory`
- `cmd/kvfs-edge/main.go` — env wiring (`EDGE_SNAPSHOT_*`)
- `cmd/kvfs-cli/main.go` — `meta history` 서브커맨드
- `scripts/demo-pi.sh` — 라이브 회전 검증

총 변경: ~350 LOC.

## 다음 ep ([P3-13](../docs/FOLLOWUP.md))

- **ADR-022 — Multi-edge leader election** (HA 본격 — Season 3 닫는 마무리)
- **ADR-017 — Streaming PUT/GET**
- **ADR-018 — Content-defined chunking**
- **ADR-019 — WAL / incremental backup** (ADR-016 위에 분 단위 RPO)

---

*kvfs 는 교육적 레퍼런스. 본 ep 의 회전 backup 패턴은 단일 edge 한정 — multi-edge
backup consistency 는 별도 작업.*
