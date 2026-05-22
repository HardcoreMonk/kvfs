# P9-04 Multipart Upload Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement S3-compatible multipart upload on top of the P9-03 bucket/object API.

**Architecture:** Store pending multipart upload metadata separately from final object metadata. Each uploaded part writes chunks through the existing edge/DN path, persists part chunk references, and complete commits the final `ObjectMeta` transactionally without rewriting object data. Coord-proxy mode owns multipart metadata through coord RPCs.

**Tech Stack:** Go 1.26 standard library, bbolt via `internal/store`, existing edge/coord HTTP RPC patterns, existing S3 SigV4/XML helpers, existing shell smoke script.

---

## Scope Check

This plan implements P9-04 only:

- `CreateMultipartUpload`
- `UploadPart`
- `ListParts`
- `CompleteMultipartUpload`
- `AbortMultipartUpload`
- conservative incomplete upload cleanup primitive and optional edge loop
- S3 XML request/response shapes and S3 error mapping
- coord-proxy metadata ownership
- smoke/docs/handoff updates

P9-05 production profile enforcement and P9-06 release gates remain separate
slices.

## File Structure

- Modify `internal/store/store.go`: multipart metadata structs, bbolt bucket,
  CRUD/complete/abort/cleanup methods, sentinel errors.
- Modify `internal/store/wal.go`: multipart WAL ops and replay.
- Create `internal/store/multipart_test.go`: metadata and WAL tests.
- Modify `internal/s3api/errors.go`: multipart error codes.
- Modify `internal/s3api/responses.go`: multipart XML response/request structs.
- Create `internal/s3api/multipart_test.go`: XML parser/response tests.
- Modify `internal/coord/coord.go`: multipart RPC routes and handlers.
- Modify `internal/coord/coord_test.go`: multipart RPC tests.
- Modify `internal/edge/coord_client.go`: multipart RPC client methods.
- Modify `internal/edge/coord_client_test.go`: multipart client tests.
- Modify `internal/edge/edge.go`: multipart S3 dispatch and cleanup loop.
- Modify `internal/edge/s3_test.go`: edge multipart tests.
- Modify `scripts/smoke-s3-compat.sh`: optional multipart smoke section.
- Modify docs: `README.md`, `docs/GUIDE.md`, `docs/guide.html`,
  `docs/ARCHITECTURE.md`, `docs/FOLLOWUP.md`.
- Create `docs/operations/2026-05-22-p9-04-multipart-upload-handoff.md`.

## Task 1: Store Multipart Metadata

**Files:**
- Modify: `internal/store/store.go`
- Modify: `internal/store/wal.go`
- Create: `internal/store/multipart_test.go`

- [ ] **Step 1: Write failing store tests**

Add tests:

- `TestMultipartUploadLifecycle`
- `TestMultipartCompleteValidatesOrderAndETag`
- `TestMultipartAbortExpiredUploads`
- `TestMultipartWALReplay`

Run:

```bash
go test ./internal/store -run 'TestMultipart|TestWAL' -count=1
```

Expected: FAIL because multipart types and methods do not exist.

- [ ] **Step 2: Implement metadata model**

Add `MultipartUploadMeta`, `MultipartPartMeta`, `CompletePart`,
`ErrInvalidPart`, `ErrInvalidPartOrder`, and bbolt bucket
`multipart_uploads`. Include a random hex upload ID generator and part-number
validation for `1..10000`.

- [ ] **Step 3: Implement lifecycle methods**

Add:

- `CreateMultipartUpload`
- `GetMultipartUpload`
- `PutMultipartPart`
- `ListMultipartParts`
- `CompleteMultipartUpload`
- `AbortMultipartUpload`
- `AbortExpiredMultipartUploads`

Complete writes final object metadata and deletes upload metadata in one bbolt
update.

- [ ] **Step 4: Implement WAL support**

Add WAL ops for initiate, part, complete, and abort. Replay must preserve
upload IDs and part metadata. Complete replay applies the final object and
removes the upload record.

- [ ] **Step 5: Verify and commit**

Run:

```bash
go test ./internal/store -run 'TestMultipart|TestWAL' -count=1
```

Expected: PASS.

Commit:

```bash
git add internal/store
git commit -m "feat: add multipart metadata registry"
```

## Task 2: S3 Multipart XML Shapes

**Files:**
- Modify: `internal/s3api/errors.go`
- Modify: `internal/s3api/responses.go`
- Create: `internal/s3api/multipart_test.go`

- [ ] **Step 1: Write failing XML tests**

Add tests:

- `TestWriteXML_InitiateMultipartUploadShape`
- `TestWriteXML_ListPartsShape`
- `TestParseCompleteMultipartUpload`
- `TestWriteXML_CompleteMultipartUploadShape`

Run:

```bash
go test ./internal/s3api -run 'Test.*Multipart|TestWriteXML' -count=1
```

Expected: FAIL because multipart XML structs/parsers do not exist.

- [ ] **Step 2: Implement XML structs and parser**

Add:

- `InitiateMultipartUploadResult`
- `Part`
- `ListPartsResult`
- `CompletedPart`
- `CompleteMultipartUploadRequest`
- `CompleteMultipartUploadResult`
- `ParseCompleteMultipartUpload`

Extend namespace injection for the new response types.

- [ ] **Step 3: Verify and commit**

Run:

```bash
go test ./internal/s3api -run 'Test.*Multipart|TestWriteXML' -count=1
```

Expected: PASS.

Commit:

```bash
git add internal/s3api
git commit -m "feat: add s3 multipart xml shapes"
```

## Task 3: Coord And CoordClient Multipart RPCs

**Files:**
- Modify: `internal/coord/coord.go`
- Modify: `internal/coord/coord_test.go`
- Modify: `internal/edge/coord_client.go`
- Modify: `internal/edge/coord_client_test.go`

- [ ] **Step 1: Write failing coord tests**

Add `TestMultipartRPCs_Lifecycle` covering initiate, part, list, complete,
missing upload, and abort.

Run:

```bash
go test ./internal/coord -run 'TestMultipart' -count=1
```

Expected: FAIL because routes do not exist.

- [ ] **Step 2: Implement coord handlers**

Add routes under `/v1/coord/multipart`. Mutating routes call `requireLeader`.
Map `ErrNotFound`, `ErrInvalidPart`, and `ErrInvalidPartOrder` to stable
non-2xx status codes for CoordClient.

- [ ] **Step 3: Write failing CoordClient tests**

Add `TestCoordClient_MultipartRoundTripAgainstRealCoord`.

Run:

```bash
go test ./internal/edge -run 'TestCoordClient_Multipart' -count=1
```

Expected: FAIL because client methods do not exist.

- [ ] **Step 4: Implement CoordClient multipart methods**

Add methods mirroring the store API and preserving store sentinel errors.

- [ ] **Step 5: Verify and commit**

Run:

```bash
go test ./internal/coord -run 'TestMultipart' -count=1
go test ./internal/edge -run 'TestCoordClient_Multipart' -count=1
```

Expected: PASS.

Commit:

```bash
git add internal/coord internal/edge/coord_client.go internal/edge/coord_client_test.go
git commit -m "feat: add coord multipart metadata rpc"
```

## Task 4: Edge S3 Multipart Handlers

**Files:**
- Modify: `internal/edge/edge.go`
- Modify: `internal/edge/s3_test.go`

- [ ] **Step 1: Write failing edge tests**

Add tests:

- `TestS3MultipartAPI_PutListCompleteGet`
- `TestS3MultipartAPI_Abort`
- `TestS3MultipartAPI_InvalidPartOrder`
- `TestS3MultipartAPI_ETagMismatch`
- `TestS3MultipartAPI_CoordProxyMetadata`

Run:

```bash
go test ./internal/edge -run 'TestS3Multipart' -count=1
```

Expected: FAIL because multipart operations still return NotImplemented.

- [ ] **Step 2: Extract part write helper**

Refactor the P9-03 object PUT chunking path into a helper returning size,
chunks, and quoted sha256 ETag. Keep native and S3 response rendering separate.

- [ ] **Step 3: Implement multipart dispatcher**

Wire S3 operations:

- `OperationCreateMultipartUpload`
- `OperationUploadPart`
- `OperationListParts`
- `OperationCompleteMultipartUpload`
- `OperationAbortMultipartUpload`

Map store errors to S3 XML errors.

- [ ] **Step 4: Add cleanup primitive on edge**

Add an edge helper that calls store/coord cleanup and best-effort deletes part
chunks when DN I/O is available. Keep env wiring conservative.

- [ ] **Step 5: Verify and commit**

Run:

```bash
go test ./internal/edge -run 'TestS3Multipart|TestS3ObjectAPI|TestS3BucketAPI' -count=1
go test ./internal/edge -count=1
```

Expected: PASS.

Commit:

```bash
git add internal/edge
git commit -m "feat: wire s3 multipart upload api"
```

## Task 5: Smoke, Docs, And Release Handoff

**Files:**
- Modify: `scripts/smoke-s3-compat.sh`
- Modify: `README.md`
- Modify: `docs/GUIDE.md`
- Modify: `docs/guide.html`
- Modify: `docs/ARCHITECTURE.md`
- Modify: `docs/FOLLOWUP.md`
- Create: `docs/operations/2026-05-22-p9-04-multipart-upload-handoff.md`

- [ ] **Step 1: Extend smoke script**

Add a multipart workflow guarded by the same credential/tool skip-safe logic:
initiate, upload two parts, list parts, complete, get object, compare body,
delete object, delete bucket.

- [ ] **Step 2: Update docs**

Mark P9-04 done only after code verification passes. Update counts with:

```bash
rg -n '^func Test' -g '*_test.go' | wc -l
```

- [ ] **Step 3: Run final verification**

Run:

```bash
go test ./...
go vet ./...
bash -n scripts/smoke-s3-compat.sh
./scripts/smoke-s3-compat.sh
./scripts/check-doc-drift.sh
git diff --check
run the AGENTS.md stale-marker scan over markdown/html files
git status --short --branch
```

Expected:

- Go tests and vet pass.
- smoke script syntax passes.
- smoke script is skip-safe when credentials/tools are absent.
- doc drift and diff checks pass.
- stale marker scan returns no matches.
- worktree contains only intended P9-04 changes before commit.

- [ ] **Step 4: Commit docs and push**

Commit:

```bash
git add scripts/smoke-s3-compat.sh README.md docs/GUIDE.md docs/guide.html docs/ARCHITECTURE.md docs/FOLLOWUP.md docs/operations/2026-05-22-p9-04-multipart-upload-handoff.md
git commit -m "docs: close p9-04 multipart upload"
git push origin main
```

## Self-Review

- Spec coverage: all P9-04 operations, coord-proxy ownership, cleanup, XML
  shapes, smoke, docs, and verification are mapped to tasks.
- Placeholder scan: no open implementation markers are used.
- Type consistency: store multipart types are the source for coord and edge
  client request/response payloads; S3 XML types stay in `internal/s3api`.
