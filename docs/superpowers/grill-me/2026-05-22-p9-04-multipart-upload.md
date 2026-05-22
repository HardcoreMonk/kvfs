# P9-04 Multipart Upload Grill-Me

## Context

P9-03 added the first successful S3 bucket/object workflow:

- bucket registry in bbolt/WAL
- coord bucket RPCs
- S3 XML success responses
- edge S3 bucket/object dispatch
- coord-proxy metadata ownership for S3 object metadata

P9-04 adds multipart upload:

- CreateMultipartUpload
- UploadPart
- ListParts
- CompleteMultipartUpload
- AbortMultipartUpload
- incomplete upload cleanup

The key design pressure is avoiding a shortcut that works for small demos but
creates a bad production-track storage model.

## Question

Should multipart uploads store each part as a hidden temporary object and then
read/rewrite data during complete, or should they persist dedicated multipart
metadata whose parts directly reference DN chunks and synthesize final object
metadata at complete time?

## Recommended Answer

Persist dedicated multipart metadata and synthesize final object metadata from
part chunk references at complete time.

Reason: hidden temporary objects would reuse some store code but force complete
to either rename across object metadata in a way the model does not currently
support, or read every part back from DNs and rewrite the final object. That is
unacceptable for the P9 envelope. Multipart complete should be metadata-heavy,
not data-copy-heavy.

The implementation should instead:

- add `multipart_uploads` metadata records;
- store each part's ETag, size, chunk references, and creation time;
- keep part chunk writes on the existing edge/DN replication path;
- make complete a bbolt transaction that validates requested parts, writes the
  final `ObjectMeta`, and deletes the pending upload record;
- expose coord RPCs for the same metadata operations so coord-proxy remains the
  production baseline;
- let abort/cleanup best-effort delete part chunks, with existing GC as the
  final orphan cleanup path.

## Decision

Use the recommended dedicated multipart metadata model.

P9-04 must not implement complete by reading all uploaded parts and rewriting
the object. It may reuse lower-level chunk write/delete helpers, but object
metadata and multipart metadata stay separate until complete.

## Residual Risk

Part overwrite and failed abort can orphan old part chunks. This is acceptable
only because kvfs already has a GC path for orphan chunks. The implementation
plan must include abort and cleanup tests, and the operation handoff must
explicitly document that chunk GC remains part of the cleanup story.
