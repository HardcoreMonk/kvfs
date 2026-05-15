# Production MVP Charter Supersede Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the P9-01 charter supersede that defines kvfs as an educational core with a production MVP track for internal MinIO/S3-compatible replacement.

**Architecture:** This is a documentation and governance slice only. It accepts the production MVP direction in ADR-064, updates the source-of-truth docs without claiming the current binary is already production-ready, and records release-to-operate status for the charter change.

**Tech Stack:** Markdown, existing ADR format, existing doc drift script, shell verification.

---

## Scope Check

The approved production replacement spec covers several independent subsystems:
S3 protocol compatibility, bucket/object API behavior, multipart upload,
production profile enforcement, admin security, and operational gates. This plan
implements only **P9-01 Charter Supersede**.

Do not add Go S3 implementation code in this plan. P9-02 begins the S3 protocol
foundation after ADR-064 is accepted.

## File Structure

- Create `docs/adr/ADR-064-production-mvp-profile.md`: accepted decision that
  defines the production MVP track, envelope, safety profile, and non-goals.
- Modify `docs/adr/README.md`: add P9 entry and keep ADR-to-code linkage
  source-of-truth in the ADR index.
- Modify `README.md`: update project identity, headline counts, season table,
  and production MVP scope without adding live env vars that are not wired in Go.
- Modify `docs/GUIDE.md`: update the audience and non-goals section.
- Modify `docs/guide.html`: mirror the `docs/GUIDE.md` wording changes.
- Modify `docs/ARCHITECTURE.md`: add production topology and legacy/native API
  boundary notes.
- Modify `AGENTS.md`: update project identity and safety notes for the new
  production MVP track.
- Modify `docs/FOLLOWUP.md`: add P9 status and keep current low-priority items.
- Create `docs/operations/2026-05-16-production-mvp-charter-supersede-handoff.md`:
  release-to-operate handoff for this docs-only charter change.

## Inputs

- Approved design spec:
  `docs/superpowers/specs/2026-05-16-production-mvp-replacement-design.md`
- Grill-me record:
  `docs/superpowers/grill-me/2026-05-16-production-mvp-replacement.md`

### Task 1: Add ADR-064 Production MVP Profile

**Files:**
- Create: `docs/adr/ADR-064-production-mvp-profile.md`
- Modify: `docs/adr/README.md`

- [ ] **Step 1: Create ADR-064**

Create `docs/adr/ADR-064-production-mvp-profile.md` with this content:

```markdown
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
```

- [ ] **Step 2: Update ADR index with P9**

In `docs/adr/README.md`, after the P8 table and before "다음 시즌 / 미작성",
insert this section:

```markdown
## P9 — Production MVP track

P9 opens the production replacement track. It does not claim the current HEAD is
already production-ready; it defines the first internal MinIO/S3-compatible
replacement target and the gates required to earn that claim.

| # | 제목 | wave 번호 | 연결 |
|---|---|---|---|
| [064](ADR-064-production-mvp-profile.md) | Production MVP profile | P9-01 | `docs/superpowers/specs/2026-05-16-production-mvp-replacement-design.md` · production MVP charter supersede |
```

- [ ] **Step 3: Update ADR summary line**

In `docs/adr/README.md`, update the final sentence that currently says the next
season candidates live in `docs/FOLLOWUP.md` so it includes P9:

```markdown
- P9 execution status and remaining polish candidates live in
  [`docs/FOLLOWUP.md`](../FOLLOWUP.md).
```

- [ ] **Step 4: Verify ADR links**

Run:

```bash
rg -n "ADR-064|Production MVP|P9" docs/adr/README.md docs/adr/ADR-064-production-mvp-profile.md
```

Expected: matches in the new ADR and ADR index.

### Task 2: Update Public Identity Docs

**Files:**
- Modify: `README.md`
- Modify: `docs/GUIDE.md`
- Modify: `docs/guide.html`

- [ ] **Step 1: Update README headline**

In `README.md`, update the opening quote and count line to:

```markdown
> **분산 object storage 설계 원리를 살아있는 데모로** 보여주는 오픈소스 레퍼런스,
> 그리고 내부 MinIO/S3-compatible replacement 를 향한 production MVP track.
> **Go 1.26 · Apache 2.0 · 60 ADR · 55 blog episode · 48 라이브 데모 · 190 unit test**
```

- [ ] **Step 2: Update README identity paragraph**

Replace the paragraph that says the goal is not production with:

```markdown
이것이 Ceph·MinIO·S3 가 하는 일의 **단순화된 핵심**. 기존 목표는
**이해 가능한 레퍼런스**였고, P9 부터는 그 educational core 위에
**내부 single-region MinIO/S3-compatible replacement MVP** track 을 추가한다.

현재 HEAD 가 곧바로 production-ready 라는 뜻은 아니다. Production claim 은
ADR-064 의 envelope 와 compatibility·chaos·backup·readiness gate 를 통과한
release 에만 붙인다.
```

- [ ] **Step 3: Add P9 row to README season table**

Add this row after P8:

```markdown
| **P9** | production MVP track | 🚧 P9-01 charter | ADR-064. internal single-region MinIO/S3-compatible replacement MVP profile |
```

- [ ] **Step 4: Add README production scope section**

After the architecture overview in `README.md`, add:

```markdown
## Production MVP track

P9 의 첫 target 은 내부 single-region MinIO/S3-compatible replacement MVP 다.

1차 envelope:

- 6-12 DN
- 10-100 TB
- internal network deployment
- AWS SDK, `aws s3`, `mc` core object workflow
- S3 SigV4, bucket/object API, multipart upload
- admin auth, readiness, metrics, compatibility, chaos, backup/restore gate

비범위: full AWS IAM parity, external customer multi-tenancy, lifecycle policy,
object lock, cross-region replication, POSIX/NFS/FUSE, Ceph-like storage
platform. 자세한 기준은 ADR-064.
```

- [ ] **Step 5: Update GUIDE audience and non-goals**

In `docs/GUIDE.md`, update the audience section so it includes operators who
want to understand the production MVP track:

```markdown
- 내부 MinIO/S3-compatible replacement 를 단계적으로 검토하는 운영자
```

In `docs/GUIDE.md` section `12.2 비목표`, replace the production replacement
bullet with:

```markdown
- **full production replacement for S3/MinIO/Ceph**. P9 는 내부
  single-region MinIO/S3-compatible replacement MVP track 이며, AWS S3 전체
  parity 나 Ceph-like platform 을 목표로 하지 않는다.
```

- [ ] **Step 6: Mirror GUIDE changes in guide.html**

Update `docs/guide.html` so the visible text mirrors Step 5. Search for the same
Korean phrases from `docs/GUIDE.md` and apply the equivalent HTML text.

- [ ] **Step 7: Verify public identity wording**

Run:

```bash
rg -n "60 ADR|Production MVP track|production MVP track|ADR-064|full production replacement|MinIO/S3-compatible replacement" README.md docs/GUIDE.md docs/guide.html
```

Expected: README, GUIDE, and guide.html all mention the P9 production MVP track
and no longer say kvfs is only non-production.

### Task 3: Update Architecture And Agent Guidance

**Files:**
- Modify: `docs/ARCHITECTURE.md`
- Modify: `AGENTS.md`

- [ ] **Step 1: Add architecture production topology note**

In `docs/ARCHITECTURE.md`, after the "모드 선택" list, add:

```markdown
- **Production MVP profile (P9)**: production claim 은 3-daemon coord-proxy
  topology 를 기준으로 한다. `EDGE_COORD_URL` unset inline mode 는 legacy/demo
  compatibility mode 로 유지한다. S3-compatible client contract 는 edge 의
  S3 front door 가 제공하고, metadata/control-plane ownership 은 coord 에 둔다.
```

- [ ] **Step 2: Add architecture API boundary note**

In `docs/ARCHITECTURE.md`, after the edge data endpoint list, add:

```markdown
P9 에서 S3-compatible surface 는 production customer contract 가 된다. 기존
`/v1/o` native API 는 demos, internal tooling, and compatibility tests 를 위한
legacy/internal surface 로 유지한다.
```

- [ ] **Step 3: Update AGENTS project goal**

In `AGENTS.md`, replace the current goal bullet with:

```markdown
- **Goal**: a living, executable reference for distributed object storage design,
  plus a P9 production MVP track for an internal single-region
  MinIO/S3-compatible replacement. The production claim applies only to releases
  that satisfy ADR-064 and the required compatibility, chaos, backup, and
  readiness gates.
```

- [ ] **Step 4: Add AGENTS production safety rule**

In `AGENTS.md` Safety Notes, keep the existing `EDGE_SKIP_AUTH=1` warning and
add:

```markdown
- Do not describe current HEAD as production-ready. Use "production MVP track"
  until ADR-064 gates are implemented and verified.
- Production-profile work must not weaken S3 SigV4, admin auth, TLS, WAL,
  transactional commit, readiness, or chaos/compatibility gates without a new
  superseding ADR.
```

- [ ] **Step 5: Verify guidance wording**

Run:

```bash
rg -n "production MVP track|ADR-064|production-ready|EDGE_COORD_URL unset|native API" AGENTS.md docs/ARCHITECTURE.md
```

Expected: AGENTS and ARCHITECTURE both describe the production MVP track and its
limits.

### Task 4: Update Follow-Up State

**Files:**
- Modify: `docs/FOLLOWUP.md`

- [ ] **Step 1: Update follow-up summary line**

In `docs/FOLLOWUP.md`, update the document status line near the top so it ends
with:

```markdown
+ P9 production MVP charter supersede (ADR-064, production MVP track) 까지 반영.
```

Keep the existing 2026-05-15 governance text and append the P9 phrase instead of
removing prior status.

- [ ] **Step 2: Add P9 priority summary**

In the priority map, add:

```markdown
- **P9**: production MVP track — P9-01 charter supersede in progress; P9-02+
  implementation slices follow ADR-064.
```

- [ ] **Step 3: Add P9 section**

Add this section before the current P8 section:

```markdown
## P9 — Production MVP track

목표: 내부 single-region MinIO/S3-compatible replacement MVP. 첫 envelope 는
6-12 DN, 10-100 TB, internal network deployment, AWS SDK/`aws s3`/`mc` core
object workflow.

### [P9-01] Charter supersede + production MVP profile

- **IN PROGRESS 2026-05-16**: ADR-064 로 project identity 를
  "educational core + production MVP track" 으로 확장. 현재 HEAD 를
  production-ready 라고 claim 하지 않고, production claim 의 gate 를
  compatibility, chaos, backup/restore, readiness 로 정의.

### [P9-02] S3 compatibility foundation

- `internal/s3api`
- SigV4 canonical request verification
- S3 XML response and error shape
- route mapping
- `aws s3` / `mc` smoke suite skeleton

### [P9-03] Bucket + object API

- CreateBucket, ListBuckets, DeleteBucket
- PutObject, GetObject, HeadObject, DeleteObject
- ListObjectsV2

### [P9-04] Multipart upload

- CreateMultipartUpload
- UploadPart
- ListParts
- CompleteMultipartUpload
- AbortMultipartUpload
- incomplete upload cleanup worker

### [P9-05] Production profile enforcement

- `KVFS_PROFILE=production`
- startup validation
- admin auth
- readiness checks

### [P9-06] Operational release gate

- compatibility smoke suite
- chaos expansion
- backup/restore rehearsal
- release-to-operate handoff
```

- [ ] **Step 4: Update current summary**

In the current summary near the bottom, update:

```markdown
- **시즌**: S1·S2·S3·S4 closed. S5 closed (Ep.1~7). S6 Ep.1~7 done. P9 production MVP track opened with ADR-064. 저우선 잔존: P6-08, P6-12, P8-05, P8-07, P8-17
```

- [ ] **Step 5: Verify follow-up state**

Run:

```bash
rg -n "P9|ADR-064|production MVP|production-ready" docs/FOLLOWUP.md
```

Expected: P9 summary, P9 section, ADR-064, and the no-overclaim wording are all
present.

### Task 5: Record Release-To-Operate Handoff

**Files:**
- Create: `docs/operations/2026-05-16-production-mvp-charter-supersede-handoff.md`

- [ ] **Step 1: Create operation handoff**

Create `docs/operations/2026-05-16-production-mvp-charter-supersede-handoff.md`
with this content:

```markdown
# Production MVP Charter Supersede Handoff

## Release Scope

Documentation and governance change for P9-01:

- ADR-064 defines the production MVP track.
- Public docs describe kvfs as educational core plus production MVP track.
- Architecture docs define 3-daemon coord-proxy topology as the production
  baseline.
- AGENTS guidance prevents current-HEAD production overclaiming.
- FOLLOWUP records P9 implementation slices.

No Go behavior, runtime API, wire protocol, data format, dependency, or demo
changed in this release.

## Verification

- `./scripts/check-doc-drift.sh` — pending execution.
- `git diff --check` — pending execution.
- P9 wording scan — pending execution.
- stale marker scan from `AGENTS.md` — pending execution.
- `git status --short --branch` — pending execution.

Go tests are not required for this release because the diff is documentation-only.

## Audit

- ADR-064 is a new decision and does not rewrite accepted historical ADRs.
- The production claim is scoped to a production MVP track and not current HEAD.
- No secrets, generated local state, binary artifacts, or runtime databases were
  introduced.

## Blockers

None once verification passes.

## Warnings

- The repository has unrelated pre-existing dirty documentation work from the
  lifecycle governance rollout. Stage only files from this P9-01 scope when
  committing.
- P9-01 creates a production target before production code exists. This is
  intentional; wording must keep "track" and "gate" language.

## Residual Risk

- Readers may infer the current binary is production-ready. README, GUIDE,
  ARCHITECTURE, AGENTS, and ADR-064 must all state that production claim is
  earned only by gated releases.
- S3 compatibility implementation can sprawl unless P9-02 keeps the supported
  operation list tied to ADR-064.

## Current Lifecycle Stage

Operate has been entered for the P9-01 production MVP charter supersede only
after verification passes.

## Next Action

Start P9-02 S3 compatibility foundation after this charter change is reviewed
and committed.

## Follow-Up Tasks

- P9-02: S3 compatibility foundation.
- P9-03: bucket and object API.
- P9-04: multipart upload.
- P9-05: production profile enforcement.
- P9-06: operational release gate.
```

- [ ] **Step 2: Replace pending verification markers after running checks**

After Task 6 verification passes, edit the handoff `Verification` section so
each pending line becomes a PASS line with the actual command result.

Example final shape:

```markdown
- `./scripts/check-doc-drift.sh` — PASS, no drift detected.
- `git diff --check` — PASS.
- P9 wording scan — PASS, production MVP track wording present.
- stale marker scan from `AGENTS.md` — PASS, no matches.
- `git status --short --branch` — PASS, only intentional P9-01 docs plus
  pre-existing unrelated dirty work listed.
```

### Task 6: Verification And Commit

**Files:**
- No new source files beyond Tasks 1-5.

- [ ] **Step 1: Run doc drift check**

Run:

```bash
./scripts/check-doc-drift.sh
```

Expected:

```text
OK — no drift detected.
```

- [ ] **Step 2: Run whitespace check**

Run:

```bash
git diff --check
```

Expected: exit 0 with no output.

- [ ] **Step 3: Run stale marker scan**

Run:

```bash
rg -n "P4-01|미커밋|HEAD [0-9a-f]|claude-zone" -g '*.md' -g '*.html' -g '!AGENTS.md'
```

Expected: exit 1 with no matches.

- [ ] **Step 4: Run production overclaim scan**

Run:

```bash
rg -n "current HEAD.*production-ready|곧바로 production-ready|production replacement for S3/MinIO/Ceph" README.md docs AGENTS.md -g '*.md' -g '*.html'
```

Expected: only wording that explicitly negates the overclaim is present.

- [ ] **Step 5: Run P9 surface scan**

Run:

```bash
rg -n "ADR-064|P9|production MVP track|MinIO/S3-compatible replacement|internal/s3api" README.md docs AGENTS.md -g '*.md' -g '*.html'
```

Expected: matches in ADR-064, README, GUIDE, guide.html, ARCHITECTURE,
FOLLOWUP, AGENTS, the design spec, and this plan.

- [ ] **Step 6: Check worktree**

Run:

```bash
git status --short --branch
```

Expected: current branch is ahead by the previously committed design spec, and
the P9-01 files are listed as intentional docs changes. Existing unrelated dirty
work from 2026-05-15 governance may still be listed.

- [ ] **Step 7: Stage only P9-01 files**

Run:

```bash
git add docs/adr/ADR-064-production-mvp-profile.md docs/adr/README.md README.md docs/GUIDE.md docs/guide.html docs/ARCHITECTURE.md AGENTS.md docs/FOLLOWUP.md docs/operations/2026-05-16-production-mvp-charter-supersede-handoff.md
```

Do not stage unrelated governance artifacts unless the user explicitly chooses
to fold them into the same commit.

- [ ] **Step 8: Commit**

Run:

```bash
git commit -m "docs: add production mvp charter"
```

Expected: commit includes only the P9-01 charter supersede files.

## Self-Review Checklist

- The plan implements only P9-01 and leaves S3 protocol code to P9-02.
- ADR-064 explicitly says current HEAD is not automatically production-ready.
- Public docs gain the production MVP target without claiming full AWS S3,
  MinIO, Ceph, NFS, or POSIX parity.
- The plan avoids adding live env vars to README before Go wiring exists.
- Verification includes doc drift, whitespace, stale marker, overclaim, P9
  surface, and worktree checks.
