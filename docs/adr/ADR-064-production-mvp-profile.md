# ADR-064 — Production MVP profile

## 상태

Accepted · 2026-05-16

## 맥락

kvfs 는 지금까지 "분산 object storage 설계 원리를 살아있는 데모로
보여주는 educational reference" 로 정의됐다. README, GUIDE, AGENTS 는
명시적으로 production replacement 가 아니라고 말한다.

사용자는 kvfs 를 내부 single-region MinIO/S3-compatible object storage
대체재로 전환하길 원한다. 승인된 1차 목표는 다음이다.

- 내부 팀/서비스 단위 사용
- 6-12 DN
- 10-100 TB
- AWS SDK, `aws s3`, `mc` 의 core object workflow
- S3 SigV4, bucket/object API, multipart upload
- admin auth, readiness, compatibility, chaos, backup/restore gate

## 결정

kvfs 의 정체성을 "educational reference only" 에서
"educational core + production MVP track" 으로 확장한다.

첫 production target 은 내부 single-region MinIO/S3-compatible replacement
MVP 다. Production MVP 는 별도 track 이며, 현재 HEAD 가 곧바로
production-ready 라는 뜻이 아니다.

Production MVP track 의 필수 방향:

1. 3-daemon topology 를 production 기준으로 삼는다.
   `kvfs-edge -> kvfs-coord -> kvfs-dn`.
2. `kvfs-edge` 는 S3-compatible front door 를 제공한다.
3. `kvfs-coord` 는 metadata, placement, transactional commit,
   anti-entropy, admin workers 의 owner 다.
4. `kvfs-dn` 은 chunk/shard bytes, scrubber state, Merkle inventory 의
   owner 다.
5. Production profile 은 unsafe demo shortcut 을 거부한다.
6. S3 compatibility 는 AWS 전체 parity 가 아니라 SDK/CLI core workflow
   호환을 1차 계약으로 한다.

## Production MVP envelope

첫 production envelope:

- single region
- 6-12 DNs
- 10-100 TB
- internal network deployment
- team/service-level usage
- S3-compatible core object workflows

이 envelope 를 넘는 claim 은 새 ADR 이 필요하다.

## Production profile requirements

P9 에서 도입할 production profile 은 다음 계약을 검증해야 한다.

- coord-proxy mode required
- demo auth bypass forbidden
- TLS for client-facing edge
- TLS or mTLS for edge-to-DN path
- coord WAL enabled
- coord transactional commit enabled
- coord DN I/O enabled for operational workers
- metrics enabled
- admin plane authenticated by admin token or mTLS client cert

HA production profile 은 coord peer configuration and self URL 도 요구한다.

## S3 compatibility contract

첫 supported S3 surface:

- CreateBucket
- ListBuckets
- DeleteBucket
- PutObject
- GetObject
- HeadObject
- DeleteObject
- ListObjectsV2
- CreateMultipartUpload
- UploadPart
- ListParts
- CompleteMultipartUpload
- AbortMultipartUpload

Unsupported operations return S3-shaped errors instead of native JSON errors.

## 명시적 비범위

- Full AWS IAM parity
- External customer multi-tenancy
- Billing, quota, object lock, legal hold, lifecycle policy, tagging
- Cross-region replication
- POSIX, NFS, FUSE gateway
- Ceph-like general storage platform
- 12 DN 또는 100 TB 초과 production claim

## 결과

+ kvfs 는 production 전환의 기준선을 명확히 갖는다.
+ S3 work 는 `internal/s3api` 같은 dedicated boundary 에서 시작할 수 있다.
+ 기존 educational docs 와 demos 는 유지하면서 production target 을 추가한다.
+ Release gate 는 compatibility, chaos, backup/restore, readiness 를 포함한다.
- 문서상 약속이 코드보다 앞서간다. 그래서 모든 문구는 "production MVP
  track" 으로 써야 하며 현재 바이너리를 production-ready 라고 말하지 않는다.
- bbolt single-writer metadata ceiling 은 1차 envelope 안에서만 허용된다.
- full S3 parity 를 기대하는 사용자는 명시적 비범위를 읽어야 한다.
