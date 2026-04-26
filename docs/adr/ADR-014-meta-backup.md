# ADR-014 — Metadata backup (snapshot + offline restore)

## 상태
Accepted · 2026-04-26

## 맥락

bbolt 메타 = 단일 파일. 손상 시 chunk 데이터(레플리카·EC stripe)는 살아있어도
"어떤 객체가 어떤 chunk 묶음인가" 인지가 사라지면 클러스터 dead. 운영성
트랙(Season 3)에서 가장 단순한 안전망부터 추가한다.

요구:
- **Hot snapshot** — edge 동작 중에도 가능. writer 잠깐 대기로 충분
- **Atomic** — bbolt page 의 일관된 point-in-time copy (반쯤 쓰여진 page 금지)
- **Bit-identical roundtrip** — restore 후 bbolt re-open 시 모든 객체·DN·urlkey
  레코드 그대로 재현
- **Operator workflow** 단순 — `kvfs-cli meta snapshot --out X.bbolt` 1줄

비범위 (별도 ADR 후보):
- HA replication — multi-edge active-active (ADR-022)
- WAL/incremental — point-in-time recovery 로 복구 시점 분 단위 (ADR-016)
- 자동 스냅샷 스케줄 — 본 ADR 은 manual + cron-friendly endpoint 만 제공

## 결정

### Snapshot (online, hot)

`MetaStore.Snapshot(w io.Writer) (int64, error)` — bbolt 의 read transaction
안에서 `tx.WriteTo(w)`. bbolt 자체가 single-writer + many-reader 라
스냅샷 중에도 PUT/GET 정상 동작.

HTTP: `GET /v1/admin/meta/snapshot` — `application/octet-stream` 으로
바이트 그대로 stream. `Content-Disposition: attachment` 헤더 동반.

### Restore (offline)

In-process restore 안 함 — bbolt file lock conflict + 메모리 상태 invalidation
위험. 운영자 절차:

1. `systemctl stop kvfs-edge` (또는 docker stop)
2. `kvfs-cli meta restore --from X.bbolt --datadir /var/lib/kvfs-edge/`
   - destination 의 bbolt lock 시도 → lock 가능하면 edge stopped 확인
   - 기존 `edge.db` → `edge.db.bak` 으로 자동 백업 (`--keep-backup=false` 로 끔)
   - snapshot 파일 → `edge.db` 복사 (0o600 권한)
3. edge 재시작

### Stats endpoint

`GET /v1/admin/meta/info` — 객체 / EC 객체 / chunk / stripe / shard / DN
/ runtime DN / urlkey 카운트 + bbolt logical size. 운영자 capacity check 용.

`kvfs-cli meta info` — 사람-친화 출력 + `--json` 으로 raw.

## 결과

**긍정**:
- 단일 파일 손상 안전망. 클러스터 lifetime 동안 정기 snapshot 1줄
- bbolt 내장 기능 활용 — 추가 코드 ~80 LOC, 의존성 0
- Restore 가 명시적 운영자 단계라 사고 위험 낮음 (실수로 prod 메타 덮어쓸 수 없음)

**부정**:
- Restore = downtime. 분 단위 RPO 는 후속 WAL 작업 필요
- Snapshot 중 HTTP body 가 커지면 (수 GB 가정 시) timeout 주의 — 현재 5분 클라이언트 timeout 으로 ~수 GB 까지 OK
- Snapshot 파일 자체 외부 보관 정책은 운영자 몫 (S3 sync / rsync to NAS 등)

**트레이드오프**:
- 자동 스케줄러 미포함 — 외부 cron + `curl http://edge:8000/v1/admin/meta/snapshot` 권장.
  ADR-013 auto-trigger 패턴을 그대로 차용해 in-process scheduler 추가는 후속 (P3 후보)
- Multi-edge 는 본 ADR 가정 외. ADR-022 가 별도로 메타 sync 다룸

## 보안 주의

- `/v1/admin/meta/snapshot` 은 메타 전체 노출 — UrlKey 시크릿 (kid → secret) 포함.
  반드시 admin 네트워크 한정 + TLS (ADR-029) + auth proxy 권장
- Restore 시 destination 의 기존 메타가 바뀌므로 backup 자동 보존 (`--keep-backup`) default on

## 관련

- `internal/store/snapshot.go` — `Snapshot(io.Writer)` + `Stats()` 구현
- `internal/store/snapshot_test.go` — round-trip + Stats 검증
- `internal/edge/edge.go` — `handleMetaSnapshot`, `handleMetaInfo`
- `cmd/kvfs-cli/main.go` — `meta snapshot/restore/info`
- `scripts/demo-xi.sh` — PUT → snapshot → corrupt → restore → GET 라이브
