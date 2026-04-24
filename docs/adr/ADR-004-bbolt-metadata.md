# ADR-004 — 메타데이터 저장 bbolt

## 상태
Accepted · 2026-04-25

## 맥락

edge는 **object_key → (chunk_id, replicas, version, ...)** 매핑을 저장해야 한다. 재시작 후 복원 필수 (α 데모 신뢰성).

대안:

| 후보 | 장점 | 단점 |
|---|---|---|
| **bbolt** (go.etcd.io/bbolt) | pure Go, 트랜잭션, etcd/Consul 검증, 외부 의존 0 | 단일 writer, 바이너리 포맷 (inspect 도구 필요) |
| SQLite | SQL로 디버그, 대중적 | CGO 필요 (or pure-Go SQLite 추가 의존) |
| JSON file + in-memory | 가장 단순, `cat` 디버그 | 동시성 어려움, 쓰기마다 전체 flush |
| in-memory only | 최단순 | 재시작 시 메타 소실 → α 데모 불가 |
| RocksDB / Pebble | 고성능 | 데모 범위에 과함, CGO or 추가 의존 |

edge 의 쓰기 QPS는 데모에서 < 수십. 단일 writer 제약 무해.

디버그 가능성은 **CLI 서브커맨드**로 해결 가능 (`kvfs-cli inspect` JSON 덤프).

## 결정

**`go.etcd.io/bbolt` 단일 파일 embedded DB** 사용.

- bucket 구조:
  - `objects`: `"<bucket>/<key>"` → JSON ObjectMeta
  - `dns`: `"<dn_id>"` → JSON DNInfo (미래 registry용)
- 파일 경로: `EDGE_DATA_DIR/edge.db` (기본 `./edge-data/edge.db`)
- 트랜잭션 API: `db.Update(func(tx){...})` / `db.View(...)`
- 디버그: `kvfs-cli inspect --db edge.db [--object bucket/key]` JSON 출력

## 결과

**긍정**:
- pure Go, 외부 의존 1개(`bbolt` 자체)만 추가 → Alpine static 빌드 유지
- 트랜잭션 ACID — version bump·replica list 원자적 업데이트
- etcd·Consul 프로덕션 레퍼런스로 신뢰성 검증됨
- 바이너리 포맷이지만 bbolt 자체에 bucket dump 유틸 존재 (+ 자체 CLI)

**부정**:
- 단일 writer — edge 수평 확장 불가 (Season 2 Raft 소재)
- 디버그 시 `bbolt` CLI 또는 `kvfs-cli inspect` 필요 (`cat` 불가)
- 파일 포맷 업그레이드 시 마이그레이션 주의

**트레이드오프**:
- edge HA가 필요해지면 → Raft-replicated meta store로 교체 (Season 2 ADR-010)
- SQLite도 충분 후보였으나 CGO 회피 + pure Go 일관성으로 bbolt 선택
- 미래에 Pebble(CockroachDB의 RocksDB-like pure Go) 대체도 가능 — 지금은 bbolt가 단순성 승

## 관련

- `internal/store/store.go` — bbolt 래퍼
- `cmd/kvfs-cli/main.go` — inspect 서브커맨드
