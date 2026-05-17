# Project Design Governance Design

## Purpose

Align kvfs project-local design rules with the Codex zone lifecycle contract and
remove stale ADR state wording found during the governance check on 2026-05-15.

## Scope

This is a documentation and workflow-contract hardening change. It does not
change Go behavior, runtime APIs, demos, or dependency policy.

In scope:

- Add the missing `domain-architecture` gate to the project-local lifecycle
  sequence in `AGENTS.md`.
- Normalize ADR wording where the body, index, and current project status already
  agree on the decision but stale text makes the design state ambiguous.
- Record project-local lifecycle artifacts for this governance change.
- Record release-to-operate state in `docs/operations/`.

Out of scope:

- New lifecycle tooling.
- Rewriting historical ADR decisions.
- Retrofitting old release handoffs for work completed before this lifecycle
  governance rule was adopted.

## Domain Architecture

The domain terms for this change are documentation governance terms, not storage
runtime terms:

- `lifecycle contract`: the shared zone workflow in
  `../codex-project-mgmt/docs/codex-lifecycle-control-plane.md`.
- `project-local guidance`: this repository's `AGENTS.md`.
- `decision record`: `docs/adr/ADR-*.md` plus `docs/adr/README.md`.
- `operate handoff`: `docs/operations/YYYY-MM-DD-<topic>-handoff.md`.

The change does not alter package boundaries or public function signatures. It
does affect agent workflow gates and documentation source-of-truth routing.

## Design

Use a minimal consistency patch:

1. Update `AGENTS.md` to mirror the canonical lifecycle order:
   `intake -> superpowers:brainstorming -> domain-architecture -> grill-me -> ...`.
2. Update `ADR-032` status from `Proposed (deferred...)` to
   `Accepted ... — deferred`, because the repository already treats the NFS
   deferral as an accepted scope decision in README and the ADR index.
3. Update `ADR-015` title from proposed/supersede-candidate wording to accepted
   supersede wording. Keep the historical note that it was proposed before user
   acceptance.
4. Update `docs/FOLLOWUP.md` so the current state notes this lifecycle
   governance alignment.
5. Add a release-to-operate handoff after verification.

## Plan Design Review

This is not a visual UI change. The relevant design review surface is information
architecture and gate clarity. The patch improves clarity by making the local
workflow order match the canonical order and by eliminating mixed ADR state
signals. No design-blocking issue remains.

## Engineering Review

Risk is limited to documentation interpretation. The main regression risk is
breaking doc drift checks or introducing stale status language. Verification must
run:

- `./scripts/check-doc-drift.sh`
- `git diff --check`
- the legacy stale-marker scan from `AGENTS.md` Finish Checklist
- targeted searches for remaining stale ADR/lifecycle wording
- `git status --short --branch`

Go tests are not required because no Go code changes are planned.

## Success Criteria

- `AGENTS.md` includes `domain-architecture` in the lifecycle sequence.
- ADR-032 body and ADR index no longer disagree on status.
- ADR-015 title no longer advertises a stale proposed state.
- Project-local lifecycle artifacts exist for this governance change.
- Verification commands pass or any limitation is recorded.
