# ADR-016 — Auto-snapshot scheduler (in-edge ticker + retention)

## 상태
Accepted · 2026-04-26

## 맥락

ADR-014 가 manual snapshot endpoint (`GET /v1/admin/meta/snapshot`) 를 줬다.
운영자는 외부 cron / systemd timer / k8s CronJob 으로 주기 backup 을 자동화할
수 있다.

문제:
- single-node 데모 / homelab 에서는 외부 cron 부담이 과함
- 외부 스케줄러 사고 (cron 중단·timezone 오류·user 권한 불일치) 시 backup 누락
  → 운영자가 ADR-014 endpoint 가 있는 줄 알면서도 정작 호출 안 됨
- ADR-013 (auto-trigger) 에서 동일 문제를 in-edge ticker 로 풀었음 — 일관 패턴

## 결정

### in-edge ticker, 디스크에 timestamped snapshot

ADR-013 의 ticker 패턴 그대로:
- env `EDGE_SNAPSHOT_DIR` 설정 시 `SnapshotScheduler` 생성
- env `EDGE_SNAPSHOT_INTERVAL` (default `1h`) 마다 다음 수행:
  1. `snap-YYYYMMDDTHHMMSSZ.bbolt.tmp` 로 atomic write (ADR-014 `Snapshot()` 활용)
  2. `rename(tmp → snap-YYYYMMDDTHHMMSSZ.bbolt)`
  3. 디렉토리 정리 — mtime 정렬 후 `EDGE_SNAPSHOT_KEEP` (default 7) 초과분 삭제
- 첫 tick 은 즉시 (cold start backup 보장)
- 에러는 stats 에 기록 + 다음 tick 으로 진행 (loop 죽이지 않음)

### Endpoint + CLI

- `GET /v1/admin/snapshot/history` — `{enabled, dir, interval, keep, last_run, last_size, total_runs, total_errors, history: [...]}`
- `kvfs-cli meta history` — 사람-친화 표 출력 + `--json`

### 외부 백업 정책 — 의도적으로 비범위

`EDGE_SNAPSHOT_DIR` 만 dir 에 항상 fresh snapshot 보장. 외부 보관 (S3 sync /
rsync / NAS) 은 운영자가 별도 도구로 (예: `rclone sync EDGE_SNAPSHOT_DIR
s3://bucket/kvfs-backups/` 를 hourly cron). 본 ADR 은 endpoint + 디스크 회전만.

이유: edge 가 외부 storage credential 을 보관·관리하면 보안 surface 폭증. ADR-029
(TLS) · ADR-028 (urlkey) 와 동일한 minimalism — edge 는 자기 데이터만 본다.

## 결과

**긍정**:
- 외부 의존 0 — single-binary kvfs-edge 만으로 backup loop 동작
- ADR-013 / ADR-027 / ADR-030 와 동일 패턴 — 학습 곡선 X
- 디스크 회전 (default 7) 으로 무한 증가 차단
- 4 unit tests PASS (one-run / prune / fresh stats / non-snapshot 보존)

**부정**:
- RPO = interval (default 1h). 더 짧은 RPO 는 ADR-019 (WAL/incremental) 후속
- 외부 보관 누락 시 디스크 손상 = 동시 전 backup 손실 — 외부 sync 권장 명시
- Snapshot 직전 tx commit 까지만 포함 — 멈춘 사이의 in-flight 업데이트 (PUT
  진행 중 등) 는 제외 (bbolt 의 read-tx 일관성 보장)

**트레이드오프**:
- `interval` 짧게 잡으면 디스크 I/O 증가 (snapshot ~수십 MB 매번). 단일 노드
  데모에선 1h 가 충분
- `keep=7` default 는 일주일 hourly retention 을 가정 — 운영 환경별 조정

## 보안 주의

- `EDGE_SNAPSHOT_DIR` 은 메타 전체 (UrlKey 시크릿 포함) 가 plain bbolt 로
  들어가는 디렉토리 — `chmod 700` + edge user 전용 권장
- 외부 sync 시에도 같은 규칙 — S3 bucket 은 SSE 활성, NAS 는 OS-level encrypted

## 관련

- `internal/store/scheduler.go` — `SnapshotScheduler` (~190 LOC)
- `internal/store/scheduler_test.go` — 4 tests PASS
- `internal/edge/edge.go` — `StartSnapshotScheduler`, `handleSnapshotHistory`
- `cmd/kvfs-edge/main.go` — env wiring
- `cmd/kvfs-cli/main.go` — `meta history` 서브커맨드
- `scripts/demo-pi.sh` — auto-snapshot 라이브 데모
