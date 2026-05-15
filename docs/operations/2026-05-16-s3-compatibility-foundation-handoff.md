# S3 Compatibility Foundation Handoff

## Status

P9-02 complete on 2026-05-16.

## What Changed

- Added `internal/s3api` for S3 protocol concerns:
  - SigV4 header verification
  - canonical request generation
  - S3 XML errors
  - S3 operation classification
- Added edge S3 route patterns.
- Added `EDGE_S3_ACCESS_KEY`, `EDGE_S3_SECRET_KEY`, and `EDGE_S3_REGION`.
- Added `scripts/smoke-s3-compat.sh`.

## Current Runtime Behavior

Authenticated recognized S3 operations return S3-shaped `NotImplemented`.
This is intentional for P9-02. First successful bucket/object workflows belong
to P9-03.

## Verification

- `go test ./internal/s3api ./internal/edge ./cmd/kvfs-edge`
- `go test ./...`
- `go vet ./...`
- `bash -n scripts/smoke-s3-compat.sh`
- `./scripts/smoke-s3-compat.sh`
- `./scripts/check-doc-drift.sh`
- `git diff --check`

## Next Slice

P9-03 Bucket + Object API:

- CreateBucket
- ListBuckets
- DeleteBucket
- PutObject
- GetObject
- HeadObject
- DeleteObject
- ListObjectsV2
