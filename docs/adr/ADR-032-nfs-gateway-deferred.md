# ADR-032 — NFS gateway (scope evaluation, deferred)

## 상태
Proposed (deferred — out of scope for current educational reference) · 2026-04-26

## 맥락

운영자가 kvfs 를 "POSIX-like 파일 시스템"처럼 mount 해서 standard tooling
(`cp`, `rsync`, `tar`, etc.) 으로 쓰기 원하는 경우. 후보:

1. **NFSv3 gateway** — kvfs-edge 옆에 NFSv3 RPC server 띄움
2. **FUSE filesystem** — `bazil.org/fuse` (외부 dep) 로 user-space mount
3. **S3-compatible API** (이미 부분적으로 가까움 — `/v1/o/...`)

## 결정 — defer NFSv3 implementation

진정한 NFSv3 구현은 다음을 모두 포함:

- **XDR encoding** — RFC 4506 binary serialization of all RPC args/replies
- **SunRPC framing** — RFC 5531, 30+ message types
- **MOUNT protocol** (RFC 1094 §A) — mount/umount/dump/export/exportall
- **NFSv3 ops** (RFC 1813) — 21 operations: NULL/GETATTR/SETATTR/LOOKUP/
  ACCESS/READLINK/READ/WRITE/CREATE/MKDIR/SYMLINK/MKNOD/REMOVE/RMDIR/
  RENAME/LINK/READDIR/READDIRPLUS/FSSTAT/FSINFO/PATHCONF/COMMIT
- **Filehandle stability** — 64-byte opaque handles that survive renames
- **Locking** (NLM protocol) — separate RPC for fcntl/flock
- **Authentication** (AUTH_SYS / AUTH_NULL / Kerberos)

추정: 6,000-10,000 LOC pure-Go without external deps. Multi-month
implementation. **이 educational reference 의 범위 외**.

## 후속 옵션 (future ADR 후보)

### Option A — S3-compatible HTTP API (가장 작음)
이미 `/v1/o/{bucket}/{key}` 가 S3 와 비슷. 추가:
- `GET /?prefix=X&delimiter=/` (ListBucket)
- `HEAD /v1/o/{bucket}/{key}` (HeadObject)
- multipart upload (선택)
- AWS SigV4 호환 서명 (선택)

`s3cmd` / `aws s3` / `mc` (MinIO client) 로 즉시 사용 가능. 외부 dep 0.
~500-1000 LOC. **권장 first step**.

### Option B — FUSE (single-process mount)
`bazil.org/fuse` 의존성 추가 (project rule 위배). 사용자 머신에서 `mount`
가능하나 NFSv3 protocol 학습 가치 ↓.

### Option C — NFSv3 (full)
가장 표준적이고 운영적으로 가치 높음. 본 ADR 의 deferred 대상.

## 결과

본 ADR 은 **결정 deferral** 그 자체를 기록. NFS gateway 는 별도 multi-month
프로젝트로 분리. kvfs core 의 "교육 reference" 정체성 유지.

권장 next step: ADR-033 (S3-compatible API) — 작은 surface 로 큰 ecosystem
호환성 확보.

## 관련

- 후속 ADR-033: S3 ListBucket / HeadObject (이미 존재하는 PUT/GET/DELETE 위에)
- 후속 ADR-034: NFSv3 gateway (multi-week, 별도 cmd/kvfs-nfs/ 바이너리)
- 후속 ADR-035: AWS SigV4 호환 서명 (UrlKey ADR-007/028 의 시그니처 옵션)
