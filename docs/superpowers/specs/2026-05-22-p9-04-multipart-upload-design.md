# P9-04 Multipart Upload Design

## Purpose

P9-04 adds the first S3-compatible multipart upload workflow on top of the
P9-03 bucket/object API. After this slice, path-style S3 clients can initiate a
multipart upload, upload numbered parts, list pending parts, complete the upload
into a normal object, abort an incomplete upload, and age out abandoned upload
metadata.

This is still a production MVP track slice, not a production-ready release.
Production profile enforcement, admin auth, readiness, compatibility gates,
chaos gates, and backup/restore release gates remain P9-05 and P9-06.

## Scope

In scope:

- `CreateMultipartUpload`
- `UploadPart`
- `ListParts`
- `CompleteMultipartUpload`
- `AbortMultipartUpload`
- incomplete upload cleanup worker and store cleanup primitive
- S3 XML request parsing and success responses for multipart operations
- S3 XML errors for missing uploads, invalid parts, invalid ordering,
  malformed complete XML, missing buckets, and auth failures
- coord-proxy metadata ownership for multipart upload metadata
- WAL replay for multipart upload metadata
- smoke-script extension for an `aws s3api` multipart workflow when tools and
  credentials are present

Out of scope:

- multipart copy operations
- per-part checksum algorithms beyond the P9 sha256 ETag convention
- AWS opaque continuation tokens for part listings
- bucket policies, IAM parity, ACLs, tags, versioning, lifecycle rules, object
  lock, legal hold, and request-payer semantics
- virtual-hosted style addressing
- production profile enforcement and release gates

## Current Baseline

P9-03 already provides:

- path-style S3 routing and SigV4 verification
- bucket registry metadata
- S3 object PUT/GET/HEAD/DELETE
- ListBuckets and ListObjectsV2 XML responses
- coord-proxy metadata ownership for buckets and objects
- object metadata WAL replay
- chunk write/read/delete helpers through edge and coordinator

`internal/s3api.Operation` already classifies multipart operations:

- `CreateMultipartUpload`
- `UploadPart`
- `ListParts`
- `CompleteMultipartUpload`
- `AbortMultipartUpload`

P9-04 replaces the current S3-shaped `NotImplemented` responses for these
operations with real handlers.

## Architecture

The multipart data path stays inside `kvfs-edge`, while multipart metadata is
owned by the same metadata owner as bucket/object metadata.

```text
S3 client
  -> kvfs-edge S3 multipart handler
  -> DN chunk writes for each uploaded part
  -> store/coord multipart metadata commit
  -> CompleteMultipartUpload validates requested parts
  -> final ObjectMeta commit using existing object metadata path
```

`internal/s3api` remains protocol-only:

- XML request structs for complete multipart upload.
- XML response structs for initiate, list parts, and complete.
- multipart S3 error codes.

`internal/store` owns durable multipart metadata:

- pending upload identity
- upload creation time
- per-part ETag, size, content type, and chunk references

`internal/coord` exposes multipart metadata RPCs so coord-proxy remains the
production baseline.

`internal/edge` owns request orchestration:

- verify bucket existence.
- write each uploaded part to DN chunks.
- persist part metadata.
- validate complete XML against persisted parts.
- synthesize the final `store.ObjectMeta` on complete.
- delete best-effort part chunks on abort and cleanup; existing GC remains the
  final safety net for orphan chunks.

## Metadata Model

Add a bbolt bucket named `multipart_uploads`.

Key:

```text
<bucket>/<key>/<upload_id>
```

Record shape:

```go
type MultipartUploadMeta struct {
    Bucket      string                      `json:"bucket"`
    Key         string                      `json:"key"`
    UploadID    string                      `json:"upload_id"`
    ContentType string                      `json:"content_type,omitempty"`
    CreatedAt   time.Time                   `json:"created_at"`
    Parts       map[int]MultipartPartMeta   `json:"parts,omitempty"`
}

type MultipartPartMeta struct {
    PartNumber int              `json:"part_number"`
    ETag       string           `json:"etag"`
    Size       int64            `json:"size"`
    Chunks     []store.ChunkRef `json:"chunks"`
    CreatedAt  time.Time        `json:"created_at"`
}
```

Store methods:

- `CreateMultipartUpload(bucket, key, contentType string) (*MultipartUploadMeta, error)`
- `GetMultipartUpload(bucket, key, uploadID string) (*MultipartUploadMeta, error)`
- `PutMultipartPart(bucket, key, uploadID string, part MultipartPartMeta) error`
- `ListMultipartParts(bucket, key, uploadID string) ([]MultipartPartMeta, error)`
- `CompleteMultipartUpload(bucket, key, uploadID string, parts []CompletePart) (*ObjectMeta, []MultipartPartMeta, error)`
- `AbortMultipartUpload(bucket, key, uploadID string) (*MultipartUploadMeta, error)`
- `AbortExpiredMultipartUploads(cutoff time.Time, limit int) ([]*MultipartUploadMeta, error)`

Error contract:

- `store.ErrNotFound` for missing upload.
- `store.ErrInvalidPart` for missing part number or ETag mismatch.
- `store.ErrInvalidPartOrder` for non-ascending complete part order.
- validation errors for invalid part numbers and malformed upload metadata.

`CompleteMultipartUpload` is transactional inside bbolt: it writes the final
object metadata and deletes the multipart upload record in one update.

## ETag Policy

kvfs continues the P9 sha256 ETag convention.

- `UploadPart` returns the quoted hex sha256 of the uploaded part body.
- `CompleteMultipartUpload` returns a quoted sha256 over the concatenated part
  ETag bytes in completed order plus `-N`, where `N` is the part count.

This is not AWS MD5 multipart ETag parity. It is deterministic and explicit for
kvfs's sha256-content-addressed storage model. The behavior is documented as a
P9 MVP compatibility choice.

## Edge Semantics

`CreateMultipartUpload`:

- requires an existing bucket.
- creates a pending upload record.
- preserves `Content-Type` from the request.
- returns `200 OK` with `InitiateMultipartUploadResult`.

`UploadPart`:

- requires existing bucket and upload.
- accepts part numbers `1..10000`.
- rejects empty part bodies for consistency with current P9 object PUT.
- writes the part body through the same replication chunk path as P9-03
  `PutObject`.
- stores part metadata under the pending upload.
- overwriting a part number replaces the pending part metadata; old part chunks
  become orphan candidates for GC.
- returns `200 OK` with the part ETag header.

`ListParts`:

- requires existing upload.
- returns parts sorted by part number.
- supports `part-number-marker` and `max-parts`, capped at 1000.

`CompleteMultipartUpload`:

- parses the XML body.
- requires at least one part.
- validates strictly ascending part numbers.
- validates every requested part exists and ETag matches when the request
  includes an ETag.
- commits the final object metadata with chunks concatenated in requested order.
- deletes the pending upload record in the same metadata transaction.
- returns `200 OK` with `CompleteMultipartUploadResult`.

`AbortMultipartUpload`:

- deletes the pending upload metadata.
- best-effort deletes uploaded part chunks from DNs.
- returns `204 No Content`.
- missing upload returns `NoSuchUpload`.

## Coord RPCs

Add coord endpoints under `/v1/coord/multipart`:

- `POST /v1/coord/multipart/initiate`
- `GET /v1/coord/multipart?bucket=&key=&uploadId=`
- `POST /v1/coord/multipart/part`
- `GET /v1/coord/multipart/parts?bucket=&key=&uploadId=`
- `POST /v1/coord/multipart/complete`
- `POST /v1/coord/multipart/abort`
- `POST /v1/coord/multipart/cleanup`

Mutating endpoints require leader when coord HA is active. Read endpoints can
serve from local coord state, matching existing bucket/object lookup behavior.

Extend `edge.CoordClient` with multipart methods mirroring store methods.

## Cleanup Worker

P9-04 adds a conservative cleanup primitive and optional background loop:

- default TTL: 24 hours.
- default disabled unless `EDGE_MULTIPART_CLEANUP=1` or equivalent main wiring
  is explicitly configured.
- cleanup deletes pending upload metadata older than cutoff.
- cleanup attempts best-effort chunk deletion when edge has DN I/O.
- existing GC remains responsible for any orphan chunks that could not be
  deleted during abort/cleanup.

The cleanup store method is required in P9-04 even if daemon env wiring remains
minimal, so P9-05 can enforce production profile expectations without changing
metadata shape.

## Error Handling

Add S3 error codes:

- `NoSuchUpload`
- `InvalidPart`
- `InvalidPartOrder`
- `MalformedXML`

All multipart S3 routes return XML errors, never native JSON/text errors.

Important mappings:

- missing bucket -> `NoSuchBucket`
- missing upload -> `NoSuchUpload`
- missing requested part -> `InvalidPart`
- ETag mismatch -> `InvalidPart`
- non-ascending complete XML -> `InvalidPartOrder`
- malformed complete XML -> `MalformedXML`
- unsupported multipart query shape -> `NotImplemented` or `InvalidRequest`

## Testing Strategy

Use TDD for every behavior change.

Store tests:

- initiate/get upload.
- upload/list parts sorted by number.
- overwrite part number.
- complete validates order and ETag.
- complete writes final object and deletes upload record transactionally.
- abort deletes upload record.
- cleanup removes old uploads and preserves new uploads.
- WAL replay covers initiate, part, complete, abort.

Coord tests:

- multipart initiate/part/list/complete/abort RPCs.
- mutating endpoints respect leader gate.
- status codes map store sentinel errors.

S3 API tests:

- initiate response XML.
- list parts response XML.
- complete request XML parser.
- complete result XML.
- HEAD/no-body behavior remains unchanged.

Edge handler tests:

- initiate/upload/list/complete/get happy path.
- abort removes pending upload.
- missing upload maps to `NoSuchUpload`.
- invalid part order maps to `InvalidPartOrder`.
- ETag mismatch maps to `InvalidPart`.
- coord-proxy multipart metadata works with empty edge local store.
- native `/v1/*` behavior remains unchanged.

Smoke test:

- extend `scripts/smoke-s3-compat.sh` to run an `aws s3api`
  multipart workflow when endpoint, credentials, `aws`, and `mc` are available:
  initiate, upload two small parts, list parts, complete, get object, delete.

Final verification:

- `go test ./internal/store -run 'TestMultipart|TestWAL' -count=1`
- `go test ./internal/s3api -run 'Test.*Multipart|TestWriteXML' -count=1`
- `go test ./internal/coord -run 'TestMultipart' -count=1`
- `go test ./internal/edge -run 'TestS3Multipart|TestCoordClient_Multipart' -count=1`
- `go test ./...`
- `go vet ./...`
- `bash -n scripts/smoke-s3-compat.sh`
- `./scripts/smoke-s3-compat.sh`
- `./scripts/check-doc-drift.sh`
- `git diff --check`

## Documentation Updates

Update:

- `README.md` P9 status and S3 behavior note.
- `docs/GUIDE.md` and `docs/guide.html` with P9-04 behavior.
- `docs/ARCHITECTURE.md` S3 endpoint subset.
- `docs/FOLLOWUP.md` to mark P9-04 done after implementation and verification.
- `docs/operations/YYYY-MM-DD-p9-04-multipart-upload-handoff.md`.

## Risks And Decisions

- The ETag is kvfs sha256 multipart ETag, not AWS MD5 multipart parity. This is
  explicit and acceptable for the first MVP slice.
- `CompleteMultipartUpload` does not rewrite data; it reuses part chunk
  references in the final object metadata. This avoids read/write amplification.
- Part overwrite can orphan old chunks. Existing GC plus cleanup is the safety
  path.
- Upload metadata must be WAL-replayable before P9-05 production profile can
  enforce durability claims.
- The cleanup worker must be conservative. It should never delete recent
  uploads by default.

## Success Criteria

- AWS CLI can complete a path-style multipart upload against kvfs-edge with
  SigV4.
- Completed multipart objects are normal kvfs objects and work with P9-03
  GetObject, HeadObject, DeleteObject, and ListObjectsV2.
- Aborted uploads do not appear as objects and no longer list parts.
- Coord-proxy mode works for multipart metadata ownership.
- Multipart success and error responses are S3 XML-shaped.
- P9-05 can build production profile enforcement without changing the multipart
  metadata model.
