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

## Plan Grilling
- `grill-me`는 원본 installer를 설치하지 않고 Codex zone의 `Plan Grilling` workflow로 사용한다.
- 신규 기능/프로젝트 설계는 `superpowers:brainstorming` 뒤, `superpowers:writing-plans` 전에 `grill-me 방식으로 검토해줘`라고 호출한다.
- 질문은 한 번에 하나만 하고, 각 질문에는 Codex의 추천 답을 함께 제시한다.
- 코드/문서로 확인 가능한 내용은 사용자에게 묻지 않고 직접 확인한다.
- `CONTEXT.md`, `CONTEXT-MAP.md`, `docs/adr/`가 있으면 용어 충돌과 ADR 후보를 함께 검토한다.
- `npx skills@latest add mattpocock/skills`, `scripts/link-skills.sh`, Claude hook installer는 실행하지 않는다.

## Lifecycle Control Plane
- 표준 lifecycle contract는 zone 상대 경로 `codex-project-mgmt/docs/codex-lifecycle-control-plane.md`를 따른다.
- 기본 순서: `intake -> superpowers:brainstorming -> domain-architecture -> grill-me -> plan-design-review -> superpowers:writing-plans -> plan-eng-review -> implement -> code-review -> release -> operate`.
- 실제 spec, grill-me 기록, plan, handoff는 해당 project root의 project-local 산출물로 둔다.
- 새 기능, behavior change, workflow contract change, multi-file change는 lightweight path를 사용하지 않는다.
- `release` 이후에는 `docs/operations/YYYY-MM-DD-<topic>-handoff.md` 또는 project-equivalent handoff로 운영 진입 상태를 기록한다.
