# Project Design Governance Grill-Me

## Context

The 2026-05-15 governance check found project-local workflow and ADR state drift.
The user asked to proceed with the reinforcement work.

## Question

Should ADR-032 remain `Proposed (deferred)` because NFS was not implemented, or
should it become `Accepted ... — deferred` because the decision was to defer NFS
as an explicit scope decision?

## Recommended Answer

Use `Accepted ... — deferred`.

Reason: the decision captured by ADR-032 is not "maybe decide later"; it is "do
not implement NFS in the current educational reference, keep it out of scope, and
prefer the smaller S3-compatible surface first." `docs/adr/README.md` and
`README.md` already treat ADR-032 as part of the accepted post-S4 decision set.
Changing the status line removes drift without rewriting the historical decision.

## Decision

Accepted the recommended answer for this maintenance change.

## Residual Risk

Old blog prose can still describe ADR-032 as a deferral, which is correct. It
should not describe it as an active implementation commitment.
