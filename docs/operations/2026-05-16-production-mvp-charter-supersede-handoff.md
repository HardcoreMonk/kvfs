# Production MVP Charter Supersede Handoff

## Release Scope

Documentation and governance change for P9-01:

- ADR-064 defines the production MVP track.
- Public docs describe kvfs as educational core plus production MVP track.
- Architecture docs define 3-daemon coord-proxy topology as the production
  baseline.
- AGENTS guidance prevents current-revision production overclaiming.
- FOLLOWUP records P9 implementation slices.

No Go behavior, runtime API, wire protocol, data format, dependency, or demo
changed in this release.

## Verification

- `./scripts/check-doc-drift.sh` — PASS, no drift detected.
- `git diff --check` — PASS.
- P9 wording scan — PASS, production MVP track / ADR-064 / P9 surface present
  across public docs, ADR docs, lifecycle artifacts, and handoff.
- production overclaim scan — PASS, matches are explicit non-production-ready or
  full-production non-goal guardrails.
- stale marker scan from `AGENTS.md` — PASS, no matches.
- `git status --short --branch` — PASS, clean worktree; branch ahead of
  `origin/main`.

Go tests are not required for this release because the diff is documentation-only.

## Audit

- ADR-064 is a new decision and does not rewrite accepted historical ADRs.
- The production claim is scoped to a production MVP track and not the current
  revision.
- No secrets, generated local state, binary artifacts, or runtime databases were
  introduced.

## Blockers

None.

## Warnings

- P9-01 creates a production target before production code exists. This is
  intentional; wording must keep "track" and "gate" language.

## Residual Risk

- Readers may infer the current binary is production-ready. README, GUIDE,
  ARCHITECTURE, AGENTS, and ADR-064 must all state that production claim is
  earned only by gated releases.
- S3 compatibility implementation can sprawl unless P9-02 keeps the supported
  operation list tied to ADR-064.

## Current Lifecycle Stage

Operate has been entered for the P9-01 production MVP charter supersede.

## Next Action

Start P9-02 S3 compatibility foundation after this charter change is reviewed
and committed.

## Follow-Up Tasks

- P9-02: S3 compatibility foundation.
- P9-03: bucket and object API.
- P9-04: multipart upload.
- P9-05: production profile enforcement.
- P9-06: operational release gate.
