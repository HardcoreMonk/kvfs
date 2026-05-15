# Production MVP Replacement Design

## Purpose

Turn kvfs from an educational distributed object storage reference into an
educational core with a production MVP track that can replace an internal
single-region MinIO deployment.

The first production target is a MinIO/S3-compatible object storage service for
team or service-level use: 6-12 DNs, 10-100 TB, internal network deployment,
SDK/CLI workflows, and explicit operational gates.

## Approved Direction

The approved approach is **Production MVP hardening track**:

- Keep the existing `kvfs-edge -> kvfs-coord -> kvfs-dn` architecture.
- Promote `kvfs-edge` into an S3-compatible front door.
- Keep metadata ownership and placement decisions in `kvfs-coord`.
- Add production profile enforcement instead of relying on operator convention.
- Add compatibility, chaos, backup, and readiness gates before calling the
  project production-capable.

## Scope

In scope for the first production MVP:

- AWS SDK, `aws s3`, and `mc` usable for core object workflows.
- S3 SigV4 authentication for data-plane requests.
- S3 XML response and error shape for the supported API surface.
- Bucket create, list, and delete.
- Object `PUT`, `GET`, `HEAD`, `DELETE`, and `ListObjectsV2`.
- Multipart upload: initiate, upload part, list parts, complete, abort, and
  cleanup of incomplete uploads.
- Production profile validation through `KVFS_PROFILE=production`.
- Authenticated admin plane through admin token or mTLS client cert.
- Production readiness checks for coord connectivity, DN quorum, WAL,
  transactional commit, TLS, auth, and metrics.
- Compatibility smoke tests and chaos/durability tests as release gates.

Out of scope for the first production MVP:

- Full AWS IAM parity.
- External customer multi-tenancy.
- Billing, quotas, object lock, legal hold, lifecycle policy, tagging, and
  bucket policy language.
- Cross-region replication.
- POSIX, NFS, or FUSE gateway.
- Ceph-like general storage platform features.
- Production envelope above 12 DNs or 100 TB without a new ADR.

## Domain Architecture

Production replacement introduces explicit domain terms that must drive module
boundaries and public contracts:

- `production profile`: a boot-time configuration mode selected by
  `KVFS_PROFILE=production`. It validates required safety settings and rejects
  demo-only shortcuts.
- `S3 front door`: the client-facing HTTP surface that speaks S3-compatible
  routes, SigV4, XML errors, and XML success responses where S3 expects them.
- `native API`: the existing `/v1/o` and `/v1/admin` surface. It remains useful
  for demos, tests, and internal tools, but it is not the production customer
  contract.
- `data plane`: S3-compatible bucket/object requests served by `kvfs-edge`.
- `admin plane`: operator and automation endpoints for cluster mutation,
  inspection, repair, rebalance, GC, and key management.
- `control plane`: metadata ownership, placement, transactional commit,
  anti-entropy, and operational workers owned by `kvfs-coord`.
- `storage plane`: chunk and shard bytes, scrubber state, and Merkle inventory
  owned by `kvfs-dn`.
- `compatibility gate`: automated smoke coverage against AWS-compatible clients
  such as `aws s3` and `mc`.
- `operational gate`: readiness, chaos, backup/restore rehearsal, and metrics
  checks required before release-to-operate.

Package and API boundary impact:

- Add `internal/s3api` for SigV4 parsing, canonical request validation, S3 route
  mapping, XML response generation, and S3 error codes.
- Keep request orchestration in `internal/edge`; edge maps S3 operations onto
  existing object PUT/GET/HEAD/DELETE flows and future multipart flows.
- Keep object and multipart metadata ownership in `internal/coord` and
  `internal/store`.
- Keep byte storage and integrity verification in `internal/dn`.
- Add production profile validation in command wiring, with shared validation
  helpers if the edge and coord checks converge.

The new domain boundary must prevent S3 compatibility logic from leaking into
placement, repair, anti-entropy, or DN storage code. Those lower layers should
continue to reason in bucket/key, chunk, stripe, shard, placement, and metadata
terms.

## Architecture

Production MVP uses the 3-daemon mode as the required topology:

```text
S3 client / SDK / mc / aws s3
  -> kvfs-edge S3 front door
  -> kvfs-coord metadata/control plane
  -> kvfs-dn chunk/shard storage
```

`EDGE_COORD_URL` unset remains a legacy/demo mode. Production profile requires
coord-proxy mode so metadata and placement have a single owner.

`internal/s3api` is a translation layer, not a separate gateway process. This
keeps deployment simple and avoids a second source of truth for authentication,
errors, and request identity.

## S3 Compatibility Contract

The first supported S3 surface is:

- `CreateBucket`
- `ListBuckets`
- `DeleteBucket`
- `PutObject`
- `GetObject`
- `HeadObject`
- `DeleteObject`
- `ListObjectsV2`
- `CreateMultipartUpload`
- `UploadPart`
- `ListParts`
- `CompleteMultipartUpload`
- `AbortMultipartUpload`

The compatibility target is practical SDK/CLI use, not full AWS parity. The
compatibility suite must include both path-style and documented endpoint usage
supported by the chosen client configuration. Unsupported S3 operations should
return S3-shaped errors rather than native JSON errors.

## Security Contract

Production profile rejects unsafe demo defaults:

```text
KVFS_PROFILE=production
EDGE_COORD_URL required
EDGE_SKIP_AUTH forbidden
EDGE_TLS_CERT/KEY required
EDGE_DN_TLS_CA required
COORD_WAL_PATH required
COORD_TRANSACTIONAL_RAFT=1 required
COORD_DN_IO=1 required
COORD_METRICS enabled
```

For HA production profile, coord peers and self URL are also required:

```text
COORD_PEERS required
COORD_SELF_URL required
```

Data-plane authentication uses S3 SigV4 with access key and secret credentials.
The first access model is intentionally smaller than AWS IAM: an access key
registry plus bucket-level allow rules. Admin endpoints require a separate admin
token or mTLS client certificate.

`EDGE_SKIP_AUTH=1` remains a demo/test shortcut only and must fail startup under
`KVFS_PROFILE=production`.

## Multipart Design

Multipart upload state belongs to coord/store:

- initiate creates an upload record with bucket, key, upload ID, headers, and
  creation time.
- upload part stores part bytes through the same chunk or EC machinery used by
  normal objects and records part number, size, checksum/ETag, and chunk refs.
- complete validates the submitted part list and atomically publishes object
  metadata.
- abort removes upload metadata and schedules chunk cleanup for parts not
  published into an object.
- cleanup worker expires stale incomplete uploads and makes progress observable
  through metrics and admin inspection.

`CompleteMultipartUpload` is the publish boundary. Before completion, uploaded
parts are not visible as an object.

## Error Handling

S3 requests must return S3-shaped XML errors for the supported API surface.
Native `/v1/*` endpoints may keep their existing JSON/text response behavior.

Readiness failures must be explicit:

- `/healthz`: process is alive.
- `/readyz`: production dependencies and safety contracts are currently met.
- S3 data-plane requests fail closed when auth, coord, WAL, or quorum
  prerequisites are not met.

## Testing And Gates

Implementation plans must add tests in layers:

- Unit tests for SigV4 canonical request verification and S3 XML shape.
- Handler tests for S3 route mapping and error behavior.
- Store/coord tests for bucket and multipart metadata.
- Integration smoke tests using `aws s3` and `mc`.
- Production profile startup tests for required env validation.
- Chaos tests for coord leader kill, DN loss, quorum loss, bit-rot repair, and
  multipart abort cleanup.
- Backup/restore rehearsal for snapshot plus WAL replay.

Release-to-operate requires:

- `go test ./...`
- `go vet ./...`
- `./scripts/check-doc-drift.sh`
- compatibility smoke suite
- relevant chaos suite
- `git diff --check`

## Migration And Documentation

P9 should start with a charter supersede rather than code first:

1. Add `ADR-064-production-mvp-profile.md`.
2. Update `README.md`, `docs/GUIDE.md`, `docs/ARCHITECTURE.md`, and
   `AGENTS.md` so the project is no longer described as strictly
   non-production.
3. Keep the educational identity, but clarify the new split:
   educational core plus production MVP track.
4. Add `docs/operations/` handoff for the production-profile design once the
   first release gate is reached.

## Proposed P9 Sequence

1. **P9-01 Charter Supersede**: production MVP ADR and documentation update.
2. **P9-02 S3 Compatibility Foundation**: `internal/s3api`, SigV4, XML errors,
   route mapping, smoke suite skeleton.
3. **P9-03 Bucket + Object API**: bucket operations and S3-compatible object
   operations.
4. **P9-04 Multipart Upload**: multipart state, part storage, complete/abort,
   cleanup worker.
5. **P9-05 Production Profile Enforcement**: boot-time validation, admin auth,
   readiness.
6. **P9-06 Operational Release Gate**: compatibility suite, chaos expansion,
   backup/restore rehearsal, and operate handoff.

## Risks

- S3 compatibility can sprawl. The first MVP must keep the supported operation
  list explicit.
- bbolt single-writer metadata is a real ceiling. It is acceptable for the
  approved 6-12 DN, 10-100 TB envelope but requires measurement before any
  larger claim.
- Multipart cleanup can leak storage if abort and expiry are not tested under
  failure.
- Dual native/S3 surfaces can diverge. Production docs must name S3 as the
  customer contract and native API as legacy/internal.
- Admin auth is a security boundary. It cannot be deferred past production
  profile enforcement.

## Success Criteria

- The production MVP profile is accepted in a new ADR.
- The docs no longer claim kvfs is only non-production; they describe the
  production MVP scope and limits.
- `internal/s3api` owns S3 protocol concerns.
- AWS-compatible clients can run the approved smoke workflows.
- Production profile refuses unsafe config.
- Release gates include compatibility, chaos, backup/restore, doc drift, and Go
  verification.
