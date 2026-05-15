# Production MVP Replacement Grill-Me

## Context

The approved production replacement design targets an internal, single-region
MinIO/S3-compatible replacement MVP for 6-12 DNs and 10-100 TB. The approved
approach is a production hardening track over the existing
`kvfs-edge -> kvfs-coord -> kvfs-dn` architecture.

## Question

Should the first implementation plan combine the project charter change with S3
protocol code, or should it keep P9-01 limited to the charter supersede and
production-profile ADR?

## Recommended Answer

Keep P9-01 limited to charter supersede and production-profile documentation.

Reason: the current repository explicitly says kvfs is not a production
replacement. Before adding S3 compatibility code, the project needs a new
accepted ADR that defines the production envelope, production profile, explicit
non-goals, and release gates. This keeps later S3 work from expanding into a
vague "full S3 clone" commitment.

## Decision

Use the approved P9 sequence from the design spec. P9-01 is documentation and
governance only. S3 protocol implementation starts in P9-02 after the production
profile ADR is accepted.

## Residual Risk

The P9-01 docs may create a stronger production promise than the current code can
honor. The wording must say "production MVP track" and "first target" rather than
claiming the current HEAD is already production-ready.
