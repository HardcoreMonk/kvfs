# Project Design Governance Handoff

## Release Scope

Documentation-only governance reinforcement for kvfs:

- `AGENTS.md` lifecycle sequence now includes the canonical
  `domain-architecture` gate.
- ADR-015 no longer advertises stale proposed/supersede-candidate wording in its
  title and section headings.
- ADR-032 status now matches the repository's accepted deferral treatment.
- Project-local lifecycle artifacts were added under `docs/superpowers/`.
- `docs/FOLLOWUP.md` records the 2026-05-15 governance alignment state.

No Go runtime behavior, API surface, dependency, demo, or data format changed.

## Verification

Executed on 2026-05-15:

- `./scripts/check-doc-drift.sh` — PASS, no drift detected.
- `git diff --check` — PASS.
- Legacy stale-marker scan from `AGENTS.md` — PASS, no matches.
- ADR stale-state scan for ADR-015 and ADR-032 — PASS, no stale proposed-state
  wording remains.
- `domain-architecture` scan — PASS, gate appears in `AGENTS.md` and lifecycle
  design artifacts.

Go tests were not run for this release because the diff is documentation-only.

## Audit

- Source-of-truth map remains unchanged.
- Accepted ADR decisions were not rewritten; only stale status wording was
  normalized to match existing index and README treatment.
- No secrets, generated local state, binary artifacts, or runtime databases were
  introduced.
- `CLAUDE.md` remains a compatibility shim that imports `AGENTS.md`.

## Blockers

None.

## Warnings

- Historical work before the 2026-05-02 lifecycle control plane did not have
  project-local operation handoffs. This handoff starts applying the rule going
  forward; it does not backfill every old release.

## Residual Risk

- Blog prose can still say ADR-032 deferred NFS, which is accurate. Future edits
  should avoid implying NFS is an active implementation commitment.
- Zone-level `domain-architecture` rollout was completed as a follow-up in
  `codex-project-mgmt/docs/operations/2026-05-15-domain-architecture-rollout-handoff.md`.

## Current Lifecycle Stage

Operate has been entered for the 2026-05-15 project design governance alignment.

## Next Action

Use the updated `AGENTS.md` lifecycle sequence for future kvfs feature,
behavior, workflow-contract, and multi-file changes.

## Follow-Up Tasks

- Zone-governance rollout for `domain-architecture` is complete; see
  `codex-project-mgmt/docs/operations/2026-05-15-domain-architecture-rollout-handoff.md`.
- Continue existing low-priority kvfs follow-ups in `docs/FOLLOWUP.md`:
  P6-08, P6-12, P8-05, P8-07, and P8-17.
