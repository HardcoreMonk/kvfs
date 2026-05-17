# S3 Compatibility Foundation Grill-Me

## Context

P9-01 accepted the production MVP track and ADR-064. The next approved slice is
P9-02 S3 compatibility foundation:

- `internal/s3api`
- SigV4 canonical request verification
- S3 XML response and error shape
- route mapping
- `aws s3` / `mc` smoke suite skeleton

The later P9 slices are separate:

- P9-03 bucket and object API behavior
- P9-04 multipart upload behavior
- P9-05 production profile enforcement
- P9-06 operational release gate

## Question

Should P9-02 wire the S3 routes all the way into bucket/object storage behavior,
or should it stop at a protocol foundation that authenticates, classifies, and
returns S3-shaped `NotImplemented` for recognized operations?

## Recommended Answer

Stop P9-02 at the protocol foundation.

Reason: SigV4 canonicalization, S3 XML errors, and S3 route mapping are their
own compatibility boundary. Mixing them with bucket/object semantics in the
same slice would make it harder to review whether S3 protocol concerns stay in
`internal/s3api` and whether existing `/v1/*` behavior remains unchanged.

P9-02 should still be runnable and testable:

- unit tests verify SigV4 canonical request behavior;
- unit tests verify S3 XML errors;
- unit tests verify S3 operation classification;
- edge handler tests verify native routes still work and S3 routes emit
  S3-shaped authenticated foundation responses;
- a smoke script skeleton is added for `aws` and `mc`, with foundation-mode
  expectations that P9-03 will later flip to success expectations.

## Decision

Use the recommended foundation-only scope for P9-02. Recognized S3 operations
may route to the edge S3 front door, but they must return S3-shaped
`NotImplemented` until P9-03 implements bucket and object behavior.

## Residual Risk

Clients may see a visible S3 endpoint before operations are implemented. The
docs and smoke skeleton must call this a compatibility foundation, not usable
S3 object storage. P9-03 owns the first successful S3 bucket/object workflow.
