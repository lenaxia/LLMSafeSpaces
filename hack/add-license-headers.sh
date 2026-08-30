#!/usr/bin/env bash
# Adds SPDX license header to all Go source files.
# Idempotent: skips files that already contain an SPDX-License-Identifier line.

set -euo pipefail

HEADER='// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
'

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

added=0
skipped=0
total=0

while IFS= read -r f; do
  total=$((total+1))
  if grep -q "SPDX-License-Identifier" "$f"; then
    skipped=$((skipped+1))
    continue
  fi
  # Prepend header + blank line; preserves whatever followed (including build tags).
  printf '%s\n' "$HEADER" | cat - "$f" > "$f.tmp" && mv "$f.tmp" "$f"
  added=$((added+1))
done < <(find . -type f -name "*.go" \
            -not -path "./.git/*" \
            -not -path "*/vendor/*" \
            -not -path "*/node_modules/*" \
            -not -path "./pkg/abi/v1/*")

echo "Total: $total  Added: $added  Skipped (already had SPDX): $skipped"
