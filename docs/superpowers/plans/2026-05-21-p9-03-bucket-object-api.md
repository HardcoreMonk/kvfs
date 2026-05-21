# P9-03 Bucket And Object API Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the first successful S3-compatible bucket/object workflow on top of the P9-02 SigV4/XML foundation.

**Architecture:** Keep S3 protocol shapes in `internal/s3api`, durable metadata in `internal/store`, coord-proxy metadata ownership in `internal/coord`, and request orchestration in `internal/edge`. S3 routes use their own dispatch path and reuse lower-level edge helpers, not native `/v1/*` HTTP handlers.

**Tech Stack:** Go 1.26 standard library, bbolt via existing `internal/store`, existing edge/coord HTTP RPC patterns, existing shell smoke script.

---

## Scope Check

This plan implements **P9-03 only**.

In scope:

- Bucket registry in `store.MetaStore`.
- Bucket CRUD RPCs on `kvfs-coord`.
- Bucket methods on `edge.CoordClient`.
- S3 XML success responses for bucket/object/list operations.
- S3 dispatch for `CreateBucket`, `ListBuckets`, `DeleteBucket`,
  `PutObject`, `GetObject`, `HeadObject`, `DeleteObject`, and `ListObjectsV2`.
- Smoke script upgrade from foundation `NotImplemented` to a real
  bucket/object workflow when credentials/tools/endpoint are present.
- Documentation and follow-up state updates.

Out of scope:

- Multipart upload.
- production profile enforcement.
- admin auth and readiness gates.
- virtual-hosted bucket addressing.
- IAM/policy/versioning/tagging/lifecycle/object-lock parity.

## File Structure

- Modify `internal/store/store.go`: bucket registry, bucket validation, bucket
  CRUD methods, sorted object listing helper, `ObjectMeta.ETag`.
- Modify `internal/store/wal.go`: WAL ops and replay for bucket create/delete.
- Modify `internal/store/*_test.go`: bucket registry and WAL replay tests.
- Modify `internal/coord/coord.go`: bucket CRUD RPC routes and handlers.
- Modify `internal/coord/coord_test.go`: coord bucket RPC tests.
- Modify `internal/edge/coord_client.go`: bucket RPC client methods.
- Modify `internal/edge/coord_client_test.go`: bucket RPC client tests.
- Modify `internal/s3api/errors.go`: extra S3 error codes.
- Create `internal/s3api/responses.go`: XML success response structs and helpers.
- Create `internal/s3api/responses_test.go`: XML response shape tests.
- Modify `internal/edge/edge.go`: S3 dispatcher and bucket/object handlers.
- Modify `internal/edge/s3_test.go`: P9-03 edge behavior tests.
- Modify `scripts/smoke-s3-compat.sh`: success workflow when configured.
- Modify `README.md`, `docs/GUIDE.md`, `docs/guide.html`,
  `docs/ARCHITECTURE.md`, and `docs/FOLLOWUP.md`.
- Create `docs/operations/2026-05-22-p9-03-bucket-object-api-handoff.md`.

## Task 1: Store Bucket Registry

**Files:**
- Modify: `internal/store/store.go`
- Modify: `internal/store/wal.go`
- Modify: `internal/store/store_test.go` or create `internal/store/bucket_test.go`

- [ ] **Step 1: Write failing bucket registry tests**

Cover:

- `CreateBucket` then `GetBucket` and `ListBuckets`.
- duplicate create returns `ErrAlreadyExists`.
- invalid bucket names are rejected.
- deleting a bucket with an object returns `ErrBucketNotEmpty`.
- `DeleteBucket` removes empty buckets.

Run:

```bash
go test ./internal/store -run 'TestBucket' -count=1
```

Expected: FAIL because bucket methods do not exist.

- [ ] **Step 2: Implement store bucket metadata**

Add:

- `BucketMeta`
- `ErrAlreadyExists`
- `ErrBucketNotEmpty`
- bbolt bucket `buckets`
- bucket validation helper
- `CreateBucket`, `GetBucket`, `ListBuckets`, `DeleteBucket`,
  `BucketHasObjects`
- `ObjectMeta.ETag`

- [ ] **Step 3: Add WAL support for bucket create/delete**

Add:

- `OpCreateBucket`
- `OpDeleteBucket`
- replay in `ApplyEntry`
- internal no-WAL helpers for replay.

- [ ] **Step 4: Verify store tests**

Run:

```bash
go test ./internal/store -run 'TestBucket|TestWAL' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store
git commit -m "feat: add bucket metadata registry"
```

## Task 2: Coord And CoordClient Bucket RPCs

**Files:**
- Modify: `internal/coord/coord.go`
- Modify: `internal/coord/coord_test.go`
- Modify: `internal/edge/coord_client.go`
- Modify: `internal/edge/coord_client_test.go`

- [ ] **Step 1: Write failing coord RPC tests**

Cover:

- `POST /v1/coord/bucket` creates a bucket.
- `GET /v1/coord/buckets` lists created buckets.
- `DELETE /v1/coord/bucket?name=...` deletes empty buckets.
- delete non-empty bucket returns a non-2xx response.

Run:

```bash
go test ./internal/coord -run 'TestBucket' -count=1
```

Expected: FAIL because routes do not exist.

- [ ] **Step 2: Implement coord bucket handlers**

Add routes:

- `POST /v1/coord/bucket`
- `GET /v1/coord/bucket`
- `GET /v1/coord/buckets`
- `DELETE /v1/coord/bucket`

Mutating handlers call `requireLeader`.

- [ ] **Step 3: Write failing CoordClient bucket tests**

Cover create/list/get/delete and error mapping for duplicate/missing/non-empty.

Run:

```bash
go test ./internal/edge -run 'TestCoordClient_Bucket' -count=1
```

Expected: FAIL because client methods do not exist.

- [ ] **Step 4: Implement CoordClient bucket methods**

Add:

- `CreateBucket`
- `GetBucket`
- `ListBuckets`
- `DeleteBucket`

Map coord status codes to store sentinel errors where needed.

- [ ] **Step 5: Verify coord/client tests**

Run:

```bash
go test ./internal/coord ./internal/edge -run 'Test.*Bucket|TestCoordClient_Bucket' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/coord internal/edge/coord_client.go internal/edge/coord_client_test.go
git commit -m "feat: add coord bucket metadata rpc"
```

## Task 3: S3 XML Success Shapes

**Files:**
- Modify: `internal/s3api/errors.go`
- Create: `internal/s3api/responses.go`
- Create: `internal/s3api/responses_test.go`

- [ ] **Step 1: Write failing XML response tests**

Cover:

- `ListBuckets` XML root and bucket entries.
- `ListObjectsV2` XML root and contents entries.
- `WriteXML` omits bodies for HEAD.

Run:

```bash
go test ./internal/s3api -run 'Test.*XML|TestWriteXML' -count=1
```

Expected: FAIL because success response helpers do not exist.

- [ ] **Step 2: Implement S3 success helpers**

Add:

- `WriteXML`
- `ListAllMyBucketsResult`
- `ListBucketResult`
- `ObjectContent`
- extra error codes:
  `BucketAlreadyOwnedByYou`, `BucketNotEmpty`, `InvalidBucketName`,
  `InternalError`.

- [ ] **Step 3: Verify S3 API tests**

Run:

```bash
go test ./internal/s3api -count=1
```

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/s3api
git commit -m "feat: add s3 xml success responses"
```

## Task 4: Edge S3 Bucket/Object Dispatch

**Files:**
- Modify: `internal/edge/edge.go`
- Modify: `internal/edge/s3_test.go`

- [ ] **Step 1: Write failing edge S3 tests**

Cover:

- authenticated create/list/delete bucket.
- delete non-empty bucket returns S3 `BucketNotEmpty`.
- put/get/head/delete object through S3.
- missing bucket maps to `NoSuchBucket`.
- missing object maps to `NoSuchKey`.
- `DeleteObject` on absent object returns `204` when bucket exists.
- native `/v1/*` behavior remains unchanged.

Run:

```bash
go test ./internal/edge -run 'TestS3' -count=1
```

Expected: FAIL because P9-03 still returns `NotImplemented`.

- [ ] **Step 2: Implement S3 dispatcher**

Replace the P9-02 foundation response with operation dispatch after SigV4
auth and region checks.

- [ ] **Step 3: Implement bucket metadata helpers**

Edge helpers choose `CoordClient` when set and `Store` otherwise.

- [ ] **Step 4: Implement S3 object helpers**

Reuse lower-level chunk write/read/delete and metadata helpers. Keep response
rendering separate from native handlers.

- [ ] **Step 5: Verify edge S3 tests**

Run:

```bash
go test ./internal/edge -run 'TestS3' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/edge
git commit -m "feat: implement s3 bucket object handlers"
```

## Task 5: Smoke Script And Documentation

**Files:**
- Modify: `scripts/smoke-s3-compat.sh`
- Modify: `README.md`
- Modify: `docs/GUIDE.md`
- Modify: `docs/guide.html`
- Modify: `docs/ARCHITECTURE.md`
- Modify: `docs/FOLLOWUP.md`
- Create: `docs/operations/2026-05-22-p9-03-bucket-object-api-handoff.md`

- [ ] **Step 1: Update smoke script**

When `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `S3_ENDPOINT`, and an AWS
CLI or `mc` binary are available, run a real bucket/object workflow. Without
tools or credentials, keep skip-safe exit 0.

- [ ] **Step 2: Update docs**

Document P9-03 as bucket/object success, not production-ready.

- [ ] **Step 3: Run docs checks**

```bash
./scripts/check-doc-drift.sh
git diff --check
```

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add scripts/smoke-s3-compat.sh README.md docs/GUIDE.md docs/guide.html docs/ARCHITECTURE.md docs/FOLLOWUP.md docs/operations/2026-05-22-p9-03-bucket-object-api-handoff.md
git commit -m "docs: mark p9-03 bucket object api complete"
```

## Task 6: Final Verification

**Files:**
- No new files unless verification finds required fixes.

- [ ] **Step 1: Run focused tests**

```bash
go test ./internal/s3api ./internal/store ./internal/coord ./internal/edge ./cmd/kvfs-edge -count=1
```

Expected: PASS.

- [ ] **Step 2: Run full tests and vet**

```bash
go test ./... -count=1
go vet ./...
```

Expected: PASS.

- [ ] **Step 3: Run smoke and docs checks**

```bash
bash -n scripts/smoke-s3-compat.sh
./scripts/smoke-s3-compat.sh
./scripts/check-doc-drift.sh
git diff --check
git status --short --branch
```

Expected: smoke is PASS or skip-safe exit 0, doc drift passes, diff check is
clean, and status is clean except branch ahead count.

- [ ] **Step 4: Commit verification fixes if any**

If final verification required changes:

```bash
git add internal scripts README.md docs
git commit -m "fix: stabilize p9-03 verification"
```

If no changes were required, do not create an empty commit.

## Self-Review

Spec coverage:

- Bucket CRUD: Tasks 1, 2, 4.
- Object PUT/GET/HEAD/DELETE: Task 4.
- ListObjectsV2: Tasks 3, 4.
- S3 XML success/error shape: Tasks 3, 4.
- Coord-proxy mode: Task 2.
- Smoke and docs: Task 5.

Completeness scan:

- No incomplete markers remain.

Type consistency:

- `store.BucketMeta` is used by store, coord, and edge.
- `edge.CoordClient` bucket methods mirror store method names.
- `s3api.ListAllMyBucketsResult` and `s3api.ListBucketResult` are edge output
  shapes only and do not leak into store or coord.
