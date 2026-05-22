# P9-04 Multipart Upload Handoff

## Status

P9-04 complete on 2026-05-22.

This is still the production MVP track, not a production-ready release claim.
ADR-064 gates remain ahead.

## What Changed

- Added persistent multipart upload metadata to bbolt and WAL replay.
- Added coord multipart metadata RPCs and edge CoordClient helpers.
- Added S3 XML shapes for multipart initiate, list parts, complete, and
  multipart-specific errors.
- Added edge S3 handlers for:
  - CreateMultipartUpload
  - UploadPart
  - ListParts
  - CompleteMultipartUpload
  - AbortMultipartUpload
- Upgraded `scripts/smoke-s3-compat.sh` with `aws s3api` multipart complete
  and abort checks.

## Current Runtime Behavior

Multipart UploadPart writes data chunks through the existing edge coordinator
path. Multipart metadata lives in the local store in inline mode and in
kvfs-coord when `EDGE_COORD_URL` is set. Complete validates the requested part
order and ETags, then promotes part metadata to final object metadata in the
metadata store transaction. Abort removes the multipart metadata record and
best-effort deletes uploaded chunks.

Multipart ETags follow kvfs's SHA256 convention, not AWS MD5 parity.

## Verification

- `go test ./internal/store -count=1`
- `go test ./internal/s3api -count=1`
- `go test ./internal/coord -count=1`
- `go test ./internal/edge -count=1`
- `go test ./...`
- `go vet ./...`
- `bash -n scripts/smoke-s3-compat.sh`
- `./scripts/smoke-s3-compat.sh` — skip-safe when AWS credentials, `aws`, or
  `mc` are not present.
- `./scripts/check-doc-drift.sh`
- `git diff --check`

## Next Slice

P9-05 Production profile enforcement:

- `KVFS_PROFILE=production`
- startup validation
- admin auth
- readiness checks
