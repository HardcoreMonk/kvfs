#!/usr/bin/env bash
# add-license-headers.sh — prepend SPDX-License-Identifier + Apache 2.0
# short header to every .go file under the project (idempotent).
set -euo pipefail

cd "$(dirname "$0")/.."

HEADER_FILE=$(mktemp)
trap 'rm -f "$HEADER_FILE"' EXIT
cat > "$HEADER_FILE" <<'EOF'
// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

EOF

count=0
while IFS= read -r f; do
  # idempotent: skip if first line already has SPDX
  if head -n 1 "$f" | grep -q "SPDX-License-Identifier"; then
    continue
  fi
  # Preserve trailing newline of original (cat does, $(...) command-sub doesn't).
  cat "$HEADER_FILE" "$f" > "$f.tmp"
  mv "$f.tmp" "$f"
  count=$((count + 1))
  echo "  + $f"
done < <(find . -name "*.go" -not -path "./bin/*" -not -path "./vendor/*")

echo
echo "Added headers to $count file(s)."
