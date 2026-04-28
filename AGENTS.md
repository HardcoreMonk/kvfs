# kvfs Agent Working Guide

Codex and other coding agents should treat this file as the project-local
working guide. `CLAUDE.md` is kept as a compatibility shim that imports this
file, so keep project-specific rules here.

## Project Identity

- **Name**: kvfs — Key-Value File System
- **Goal**: a living, executable reference for distributed object storage
  design. This is not a production replacement for S3, MinIO, or Ceph.
- **Success criteria**: clarity, reproducibility, and educational value.
- **License**: Apache 2.0.

## Runtime And Dependencies

- **Go**: 1.26.
- **CGO**: off by default (`CGO_ENABLED=0`) for static Alpine-friendly builds.
- **External dependencies**: keep them minimal. `go.etcd.io/bbolt` is the core
  persistent dependency; prefer the standard library unless a new ADR justifies
  another dependency.

## Source-Of-Truth Map

| Need | Read or update |
|---|---|
| Public overview, counts, env table | `README.md` |
| Walkthrough | `docs/GUIDE.md` and mirrored `docs/guide.html` |
| Short architecture reference | `docs/ARCHITECTURE.md` |
| Decisions and ADR/demo mapping | `docs/adr/` and `docs/adr/README.md` |
| Pending work and current status | `docs/FOLLOWUP.md` |
| Narrative episode | `blog/NN-title.md` |
| Agent working rules | `AGENTS.md` |

Avoid duplicating package-by-package ownership tables outside
`docs/adr/README.md`; that file is the single source for ADR-to-code linkage.

## Worktree Hygiene

- Do not revert user changes or unrelated local changes.
- `.codex` and `.claude/` are local runtime metadata and must stay untracked.
- Keep generated/local state out of commits: `bin/`, `edge-data/`, `dn-data*/`,
  `certs/`, `*.db`, and temporary demo output.
- Prefer small, scoped commits. Documentation-only work should not touch Go code
  unless a code reference in the docs is demonstrably wrong.

## Architecture Snapshot

```
[2-daemon default, S1~S4 compatible]
Client -> kvfs-edge -> kvfs-dn x N

[3-daemon optional, S5+]
Client -> kvfs-edge -> kvfs-coord x N -> kvfs-dn x N
```

- `kvfs-edge` (`:8000`): HTTP gateway, UrlKey verification, chunking, EC
  encoding, and inline coordinator when `EDGE_COORD_URL` is unset.
- `kvfs-coord` (`:9000`, optional): placement, metadata ownership, HA, admin
  workers, and anti-entropy self-heal.
- `kvfs-dn` (`:8080`): byte container for content-addressable chunks, Merkle
  inventory, and bit-rot scrubber.

## Common Commands

| Command | Purpose |
|---|---|
| `make build` | Build `kvfs-edge`, `kvfs-dn`, `kvfs-coord`, and `kvfs-cli` into `./bin/` |
| `make test` | Unit tests |
| `make test-race` | Race tests; requires CGO-capable local Go |
| `make fmt` | `gofmt -w .` |
| `make lint` | `go vet ./...` |
| `./scripts/check-doc-drift.sh` | GUIDE/HTML/ADR/env/package drift check |
| `./scripts/up.sh` / `./scripts/down.sh` | Local Docker cluster lifecycle |
| `./scripts/demo-*.sh` | Live demos mapped in `docs/adr/README.md` |

Docker verification without local Go:

```bash
docker run --rm -v "$PWD:/src" -w /src golang:1.26-alpine \
  sh -c 'go build ./... && go test ./... && go vet ./...'
```

## Code Conventions

- Wrap errors with context: `fmt.Errorf("%s: %w", ctx, err)`.
- Use structured `log/slog`; reserve `fmt.Printf` for explicit demo or hot-path
  instrumentation already present in the codebase.
- Put explicit timeouts on outbound HTTP and external I/O paths.
- Keep tests package-local in `_test.go`; use `httptest` for HTTP boundaries and
  avoid external network dependencies.
- Manage goroutine lifetime with `context.Context` and `sync.WaitGroup`.
- Keep file permissions consistent: chunks `0644`, bbolt files `0600`.

## Documentation Rules

- New env var: update `README.md`, `docs/GUIDE.md`, `docs/guide.html`, and the
  relevant `cmd/kvfs-*/main.go` help or wiring.
- New ADR: add `docs/adr/ADR-NNN-short-slug.md`, update
  `docs/adr/README.md`, and update `README.md` season/ADR tables when the
  public surface changes.
- Accepted ADRs are historical records. Do not rewrite their decisions. For
  changed reality, add a new ADR or append a clearly dated follow-up note only
  when the repository already uses that pattern.
- New guide content: keep `docs/GUIDE.md` and `docs/guide.html` in sync.
- New blog episode or demo: update counts and mappings in `README.md`,
  `docs/GUIDE.md`, `docs/guide.html`, and `docs/adr/README.md`.
- Follow-up state changes: update `docs/FOLLOWUP.md` immediately. Prefer stable
  release/status facts over transient worktree notes or commit SHAs to reduce
  documentation drift.
- Before finishing documentation work, run `./scripts/check-doc-drift.sh` and
  `git diff --check`.

## Finish Checklist

For documentation-only changes:

1. `./scripts/check-doc-drift.sh`
2. `git diff --check`
3. `rg -n "P4-01|미커밋|HEAD [0-9a-f]|claude-zone" -g '*.md' -g '*.html' -g '!AGENTS.md'`
4. `git status --short --branch`

For code changes, also run the focused unit tests first, then `go test ./...`
and `go vet ./...` or the Docker equivalent from the command table.

## Safety Notes

- Do not present `EDGE_SKIP_AUTH=1` as production-safe. It is demo-only.
- Do not commit real `EDGE_URLKEY_SECRET` values.
- `chunk_id` is the sha256 content address. Changing that invariant requires a
  new superseding ADR.
