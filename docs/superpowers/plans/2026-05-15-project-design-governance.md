# Project Design Governance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Align kvfs project-local design governance with the zone lifecycle contract and remove stale ADR state wording.

**Architecture:** This is a documentation-only governance patch. `AGENTS.md` owns agent workflow rules, `docs/adr/` owns architectural decision state, `docs/FOLLOWUP.md` owns current project status, and `docs/operations/` records release-to-operate handoff.

**Tech Stack:** Markdown, shell verification, existing `scripts/check-doc-drift.sh`.

---

### Task 1: Lifecycle Artifacts

**Files:**
- Create: `docs/superpowers/specs/2026-05-15-project-design-governance-design.md`
- Create: `docs/superpowers/grill-me/2026-05-15-project-design-governance.md`
- Create: `docs/superpowers/plans/2026-05-15-project-design-governance.md`

- [x] **Step 1: Write the design spec**

Record purpose, scope, domain architecture evidence, design review, engineering
review, and success criteria in the spec file.

- [x] **Step 2: Write the grill-me record**

Record the ADR-032 status question and the recommended `Accepted ... — deferred`
answer.

- [x] **Step 3: Write this implementation plan**

Record the exact files, edits, and verification commands.

### Task 2: Patch Governance Drift

**Files:**
- Modify: `AGENTS.md`
- Modify: `docs/adr/ADR-015-coordinator-daemon-split.md`
- Modify: `docs/adr/ADR-032-nfs-gateway-deferred.md`
- Modify: `docs/FOLLOWUP.md`

- [x] **Step 1: Update lifecycle sequence**

In `AGENTS.md`, insert `domain-architecture` between
`superpowers:brainstorming` and `grill-me`.

- [x] **Step 2: Normalize ADR-015 title**

Change the title to describe the accepted coordinator daemon split and ADR-002
supersede state. Do not rewrite the historical note explaining that the ADR was
originally proposed.

- [x] **Step 3: Normalize ADR-032 status**

Change the status line to `Accepted ... — deferred` so the ADR body agrees with
the ADR index and README treatment.

- [x] **Step 4: Update follow-up state**

Update `docs/FOLLOWUP.md` with a 2026-05-15 governance alignment entry while
leaving existing low-priority follow-up items unchanged.

### Task 3: Release-To-Operate Handoff

**Files:**
- Create: `docs/operations/2026-05-15-project-design-governance-handoff.md`

- [x] **Step 1: Run verification**

Run the verification commands from Task 4.

- [x] **Step 2: Record handoff**

Write `Release Scope`, `Verification`, `Audit`, `Blockers`, `Warnings`,
`Residual Risk`, `Current Lifecycle Stage`, `Next Action`, and `Follow-Up Tasks`.

### Task 4: Verification

**Files:**
- No source changes.

- [x] **Step 1: Run doc drift check**

Run: `./scripts/check-doc-drift.sh`
Expected: `OK — no drift detected.`

- [x] **Step 2: Run whitespace check**

Run: `git diff --check`
Expected: exit 0 with no output.

- [x] **Step 3: Run stale marker scan**

Run the legacy stale-marker scan from `AGENTS.md` Finish Checklist.
Expected: exit 1 with no matches.

- [x] **Step 4: Run governance wording scan**

Run:

```bash
rg -n "domain-architecture|Proposed \\(deferred|Proposed, ADR-002 supersede 후보|Current Lifecycle Stage" AGENTS.md docs/adr/ADR-015-coordinator-daemon-split.md docs/adr/ADR-032-nfs-gateway-deferred.md docs/operations/2026-05-15-project-design-governance-handoff.md
```

Expected: `domain-architecture` and handoff stage are present; stale proposed
phrases are absent.

- [x] **Step 5: Check worktree**

Run: `git status --short --branch`
Expected: only intentional documentation changes are listed.
