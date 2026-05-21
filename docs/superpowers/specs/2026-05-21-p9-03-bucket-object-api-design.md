# P9-03 Bucket And Object API Design

## Purpose

P9-03 turns the P9-02 S3 protocol foundation into a working S3 data-plane
slice for bucket and object workflows. After this slice, path-style S3 clients
can create buckets, list buckets, write objects, read objects, inspect object
metadata, delete objects, delete empty buckets, and list objects with
`ListObjectsV2`.

This is still not a production-ready release. It is the first successful
S3-compatible workflow on the production MVP track. Multipart upload,
production profile enforcement, and operational release gates remain P9-04,
P9-05, and P9-06.

## Scope

In scope:

- `CreateBucket`
- `ListBuckets`
- `DeleteBucket`
- `PutObject`
- `GetObject`
- `HeadObject`
- `DeleteObject`
- `ListObjectsV2`
- S3 XML success responses for the operations above.
- S3 XML errors for bucket/object not found, bucket already exists, non-empty
  bucket delete, invalid bucket name, unsupported query shape, and auth failures.
- Path-style requests such as `/bucket/key`.
- S3 handler tests, store tests, coord RPC tests, and smoke-script extension.

Out of scope:

- Multipart upload.
- Bucket policies, IAM parity, ACLs, object tags, versioning, lifecycle rules,
  object lock, legal hold, and request-payer semantics.
- Virtual-hosted style bucket addressing.
- Production profile enforcement, admin auth, readiness gates, chaos expansion,
  and backup/restore release gate.

## Current Baseline

P9-02 already provides:

- `internal/s3api` operation classification.
- SigV4 header verification.
- S3 XML error writer.
- Edge S3 catch-all routing that preserves native `/v1/*`, `/healthz`, and
  `/metrics`.
- Authenticated `NotImplemented` responses for recognized S3 operations.

The native object flow already supports:

- Streaming replication PUT.
- EC PUT through `X-KVFS-EC`.
- GET, HEAD, DELETE, and JSON `ListObjectsByPrefix`.
- Coord-proxy metadata ownership through `CoordClient`.

P9-03 reuses the native byte storage path but gives S3 clients an S3-shaped
contract.

## Architecture

The S3 front door remains inside `kvfs-edge`.

```text
S3 client
  -> kvfs-edge S3 handler
  -> internal/s3api XML and operation helpers
  -> existing edge object orchestration
  -> kvfs-coord metadata RPCs when EDGE_COORD_URL is set
  -> kvfs-dn chunk storage
```

`internal/s3api` owns protocol details only:

- XML response structs and writers.
- S3 error codes.
- request classification.
- small helpers for S3 headers and ETag formatting.

`internal/edge` owns request orchestration:

- verify SigV4.
- dispatch the classified operation.
- call store or coord metadata helpers.
- reuse chunk write, read, and delete paths.
- translate native success/failure into S3 status, headers, and XML.

`internal/store` owns durable bucket metadata and object metadata.

`internal/coord` exposes bucket RPCs so coord-proxy mode remains the preferred
production topology. Inline edge mode remains usable for demos and tests.

## Bucket Metadata

Add a bbolt bucket registry to `store.MetaStore`.

Record shape:

```go
type BucketMeta struct {
    Name      string    `json:"name"`
    CreatedAt time.Time `json:"created_at"`
}
```

Store methods:

- `CreateBucket(name string) (*BucketMeta, error)`
- `GetBucket(name string) (*BucketMeta, error)`
- `ListBuckets() ([]*BucketMeta, error)`
- `DeleteBucket(name string) error`
- `BucketHasObjects(name string) (bool, error)`

Error contract:

- existing `store.ErrNotFound` for missing bucket.
- new `store.ErrAlreadyExists` for duplicate bucket create.
- new `store.ErrBucketNotEmpty` for deleting a bucket with objects.
- validation errors for invalid bucket names.

Bucket naming starts with a practical S3-compatible subset:

- length 3 to 63.
- lowercase letters, digits, hyphen, and dot.
- starts and ends with a letter or digit.
- no consecutive dots.
- not formatted like an IPv4 address.

P9-03 does not implement global namespace uniqueness beyond this kvfs cluster.

## Object Semantics

S3 object operations require the bucket to exist. This check applies only to
S3 routes. Native `/v1/o` routes keep existing demo-compatible behavior.

`PutObject`:

- requires an existing bucket.
- rejects empty body the same way native PUT currently does.
- stores through the existing replication write path. S3 EC selection remains
  out of scope; native `X-KVFS-EC` remains available only on `/v1/o`.
- preserves `Content-Type`.
- returns `200 OK`.
- returns an ETag header set to the quoted hex sha256 of the full object body.
  This is intentionally not AWS MD5 parity in P9-03 because kvfs is
  sha256-content-addressed.

`GetObject`:

- requires existing bucket and object.
- streams the object body.
- returns `Content-Type`, `Content-Length`, `ETag`, and `Last-Modified` when
  metadata has enough information.

`HeadObject`:

- shares lookup and headers with `GetObject`.
- returns no body.

`DeleteObject`:

- returns success for existing object deletion.
- returns `204 NoContent`.
- for absent objects, follows S3 behavior and returns a successful no-content
  response unless a missing bucket is the actual failure.

`ListObjectsV2`:

- requires existing bucket.
- supports `prefix`, `delimiter`, `max-keys`, `continuation-token`, and
  `start-after`.
- uses lexicographic key order. `start-after` and `continuation-token` both
  mean "return keys strictly after this key"; `NextContinuationToken` is the
  last key returned when the response is truncated.
- caps `max-keys` at 1000. Missing or invalid values use S3 XML errors rather
  than native JSON.
- response is S3 XML with `Name`, `Prefix`, `KeyCount`, `MaxKeys`,
  `IsTruncated`, and `Contents` entries.

## Coord RPCs

Coord-proxy mode needs bucket operations on coord because coord owns metadata.

Add coord endpoints under `/v1/coord/bucket`:

- `POST /v1/coord/bucket`
- `GET /v1/coord/bucket?name=...`
- `GET /v1/coord/buckets`
- `DELETE /v1/coord/bucket?name=...`

Mutating endpoints require leader when coord HA is active. Read endpoints can
serve from the local coord state, matching existing object lookup behavior.

Extend `edge.CoordClient` with bucket methods mirroring store methods.

## Error Handling

S3 routes must not leak native JSON or plain text errors.

Use S3 XML errors for:

- missing credentials or invalid SigV4.
- invalid bucket name.
- duplicate bucket create.
- missing bucket.
- missing object.
- deleting a non-empty bucket.
- unsupported S3 query shape.
- internal coord or DN failures.

Native `/v1/*` routes keep current JSON/text behavior.

## Testing Strategy

Use TDD for every behavior change.

Store tests:

- create/list/get/delete bucket.
- duplicate create returns `ErrAlreadyExists`.
- delete non-empty bucket returns `ErrBucketNotEmpty`.
- invalid names are rejected.

Coord tests:

- bucket CRUD RPCs.
- leader gate on mutating bucket RPCs.
- coord-proxy object operations observe bucket existence through coord.

S3 API tests:

- XML success shapes for `ListBuckets` and `ListObjectsV2`.
- ETag and timestamp header helpers.
- S3 error XML for all P9-03 error classes.

Edge handler tests:

- unauthenticated requests fail before operation behavior.
- create/list/delete bucket happy path.
- delete non-empty bucket fails.
- put/get/head/delete object happy path.
- missing bucket/object error mapping.
- native `/v1/*` route behavior unchanged.

Smoke test:

- extend `scripts/smoke-s3-compat.sh` to run, when credentials and tools are
  present, a path-style workflow equivalent to:
  - create bucket.
  - upload a small file.
  - list bucket.
  - download or cat the object.
  - remove object.
  - remove bucket.

Final verification:

- `go test ./internal/s3api ./internal/store ./internal/coord ./internal/edge ./cmd/kvfs-edge`
- `go test ./...`
- `go vet ./...`
- `bash -n scripts/smoke-s3-compat.sh`
- `./scripts/smoke-s3-compat.sh`
- `./scripts/check-doc-drift.sh`
- `git diff --check`

## Documentation Updates

Update:

- `README.md` P9 status and S3 foundation note.
- `docs/GUIDE.md` and `docs/guide.html` with P9-03 behavior.
- `docs/ARCHITECTURE.md` if package or route boundaries change.
- `docs/FOLLOWUP.md` to mark P9-03 done only after implementation and
  verification.
- `docs/operations/YYYY-MM-DD-p9-03-bucket-object-api-handoff.md` for
  release-to-operate state of this slice.

## Risks And Decisions

- ETag is not AWS MD5 parity in the first slice. Document it as kvfs sha256
  ETag.
- Bucket existence is enforced only for S3 routes to avoid breaking native demo
  workflows.
- `DeleteObject` should be idempotent for missing objects when the bucket
  exists, matching practical S3 client expectations.
- Pagination stays key-cursor based. It is sufficient for AWS CLI and MinIO
  client smoke workflows but does not attempt AWS continuation-token opacity.
- Bucket metadata must be WAL-replayable through the same coord/store path as
  object metadata before production profile can claim durability.

## Success Criteria

- AWS CLI or MinIO client can run a create bucket, put, list, get/head, delete,
  and remove bucket workflow against kvfs-edge with SigV4.
- Existing native object routes and tests remain unchanged.
- Coord-proxy mode works for bucket and object operations.
- S3 success and error responses are XML-shaped for the supported operations.
- P9-04 can build multipart metadata on the bucket/object contracts introduced
  here without reworking P9-03 boundaries.
