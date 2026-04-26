#!/bin/sh
# Check docs/GUIDE.md and docs/guide.html for drift vs current code/ADR state.
# Exit 0 = no drift, 1 = drift detected.
#
# Run locally: ./scripts/check-doc-drift.sh
# Run in CI:   .github/workflows/doc-drift.yml invokes this verbatim.
#
# Reserved/pending ADR numbers (intentionally absent from GUIDE) are listed
# in RESERVED_ADRS and skipped — keep this in sync with docs/adr/README.md
# § "미작성 ADR".

set -eu

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
GUIDE="$ROOT/docs/GUIDE.md"
HTML="$ROOT/docs/guide.html"
EDGE_MAIN="$ROOT/cmd/kvfs-edge/main.go"

# ADR numbers exempt from §3-§8 mention check.
# Two reasons to be on this list:
#   foundational (described by concept, not by ADR ref): 001 002 003 004
#   superseded (replaced by a later ADR; older one still in repo for history): 006
#   reserved/pending (placeholder rows in adr/README.md):                     015 020 021 023 026 036 037
RESERVED_ADRS="001 002 003 004 006 015 020 021 023 026 036 037"

errors=0
warns=0

is_reserved() {
  for r in $RESERVED_ADRS; do
    if [ "$1" = "$r" ]; then return 0; fi
  done
  return 1
}

echo "==> A. ADR mention check (every ADR file → mentioned in GUIDE.md)"
for adr in "$ROOT"/docs/adr/ADR-*.md; do
  [ -f "$adr" ] || continue
  num=$(basename "$adr" | sed -E 's/^ADR-([0-9]+)-.*/\1/')
  if is_reserved "$num"; then continue; fi
  if ! grep -q "ADR-$num" "$GUIDE"; then
    echo "  DRIFT: ADR-$num not mentioned in docs/GUIDE.md"
    errors=$((errors + 1))
  fi
done

echo "==> B. Section-count parity (GUIDE.md ## == guide.html h2)"
md_sections=$(grep -c '^## ' "$GUIDE" || true)
html_sections=$(grep -cE '^<h2 [^>]*id=' "$HTML" || true)
if [ "$md_sections" != "$html_sections" ]; then
  echo "  DRIFT: GUIDE.md has $md_sections H2 sections, guide.html has $html_sections"
  errors=$((errors + 1))
fi

echo "==> C. Env-var coverage in §10 (warn — not every var belongs there)"
# Extract EDGE_* env-var literals from main.go's flag block.
code_vars=$(grep -oE '"EDGE_[A-Z_]+"' "$EDGE_MAIN" | tr -d '"' | sort -u)
for v in $code_vars; do
  if ! grep -q "$v" "$GUIDE"; then
    echo "  WARN: $v in main.go but not in GUIDE.md (intentional? confirm in §10)"
    warns=$((warns + 1))
  fi
done

echo "==> D. Internal package coverage (warn — new internal/ pkg should appear in GUIDE)"
for pkg in "$ROOT"/internal/*/; do
  pkg_name=$(basename "$pkg")
  # Accept any case-insensitive mention: `internal/X`, `X/file.go`, or the
  # bare package name as a word (the guide uses concept names like "Heartbeat"
  # rather than path names like "internal/heartbeat").
  if ! grep -qiE "internal/$pkg_name|[^a-z_]$pkg_name[^a-z_]" "$GUIDE"; then
    echo "  WARN: internal/$pkg_name not referenced in GUIDE.md (no concept name match either)"
    warns=$((warns + 1))
  fi
done

echo ""
if [ "$errors" -gt 0 ]; then
  echo "FAIL: $errors drift error(s), $warns warning(s)."
  exit 1
fi
if [ "$warns" -gt 0 ]; then
  echo "OK with $warns warning(s) — review and update GUIDE.md if appropriate."
else
  echo "OK — no drift detected."
fi
exit 0
