# P9-03 Bucket/Object API Handoff

## Status

P9-03 complete on 2026-05-22.

## What Changed

- Added a persistent bucket registry to bbolt and WAL replay.
- Added coord bucket metadata RPCs and edge CoordClient bucket helpers.
- Added S3 XML success response shapes for ListBuckets and ListObjectsV2.
- Replaced the P9-02 S3 foundation response with authenticated operation
  dispatch for:
  - CreateBucket, ListBuckets, DeleteBucket
  - PutObject, GetObject, HeadObject, DeleteObject
  - ListObjectsV2 with prefix and delimiter support
- Upgraded `scripts/smoke-s3-compat.sh` from foundation-mode probing to an
  `aws`/`mc` bucket/object smoke workflow by default.

## Current Runtime Behavior

P9 S3 clients can exercise the first successful path-style bucket/object
workflow through kvfs-edge with SigV4 header authentication. Object data uses
the existing chunk placement and metadata commit paths, including coord-proxy
metadata when `EDGE_COORD_URL` is set.

Multipart operations still return S3-shaped `NotImplemented`; they are the
P9-04 slice.

## Verification

- `go test ./internal/store -run 'TestBucket|TestWAL' -count=1`
- `go test ./internal/s3api -count=1`
- `go test ./internal/coord -run 'TestBucket|TestAdminEndpoints|TestCoord' -count=1`
- `go test ./internal/edge -count=1`
- `go test ./...`
- `go vet ./...`
- `bash -n scripts/smoke-s3-compat.sh`
- `./scripts/smoke-s3-compat.sh` — skip-safe when AWS credentials, `aws`, or
  `mc` are not present.
- `./scripts/check-doc-drift.sh`
- `git diff --check`

## Next Slice

P9-04 Multipart upload:

- CreateMultipartUpload
- UploadPart
- ListParts
- CompleteMultipartUpload
- AbortMultipartUpload
- incomplete upload cleanup worker
