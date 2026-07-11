#!/usr/bin/env bash
# =============================================================================
# llmsafespace → llmsafespaces rename: dry-run reporter + executor.
#
# Usage:
#   DRY_RUN=1 ./rename-to-llmsafespaces.sh   # default — report only, no edits
#   DRY_RUN=0 ./rename-to-llmsafespaces.sh   # execute the rename
#
# Policy (per user decision, 2026-06-18):
#   * K8s API group: llmsafespace.dev → llmsafespaces.dev     (rename)
#   * GitHub repo : lenaxia/LLMSafeSpace → LLMSafeSpaces      (rename)
#   * History docs: worklogs/, design/                        (LEAVE ALONE)
#
# Excludes (no edits): .git/, worklogs/, design/, bin/, node_modules/,
#   root binaries (workspace-agentd, redact, tools), go.sum (regenerated),
#   lockfiles (regenerated).
#
# Re-run safety:
#   The perl substitution uses lookbehind/lookahead (?<![sS])...(?![sS])
#   so the singular pattern never matches inside an already-pluralised token
#   (llmsafespace will not match within llmsafespaces). A startup guard
#   aborts if any non-excluded file already contains the plural form, so
#   re-running after a successful rename errors out cleanly rather than
#   no-op'ing or corrupting.
# =============================================================================

set -euo pipefail

# F8 — require perl (the rewrite engine).
command -v perl >/dev/null 2>&1 || {
  echo "ERROR: perl is required but not found on PATH." >&2
  exit 1
}

# F2 — resolve repo root from git, do not hardcode a checkout path.
ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" || {
  echo "ERROR: not inside a git work tree." >&2
  exit 1
}
cd "$ROOT"

DRY_RUN="${DRY_RUN:-1}"

# ---------- Phase 1: directory renames (git mv) ------------------------------
# Three directories whose name is part of an import path / chart name / pkg id.
DIR_RENAMES=(
  "pkg/apis/llmsafespace:pkg/apis/llmsafespaces"
  "charts/llmsafespace:helm"
  "sdks/vscode-llmsafespace:sdks/vscode-llmsafespaces"
)

# ---------- Phase 2: content rewrite rule ------------------------------------
# A single alternation of all five case-variants. The five sources are
# disjoint (verified: zero existing 'llmsafespaces' / 'LLMSafeSpaces' in
# tree), so one pass suffices. Lookbehind/lookahead (?<![sS])...(?![sS])
# prevent the pattern from matching inside its own pluralised output, making
# a re-run safe (no llmsafespace → llmsafespacess corruption).
#
# The replacement appends a case-matched 's': uppercase 'S' if the matched
# token ends in 'E' (i.e. the ALL_CAPS variant), lowercase 's' otherwise.
#
# NOTE: There are FIVE case variants, not three. The initial rename run
# only handled three and missed the K8s client-gen naming conventions:
#   Llmsafespace  (initial-cap: LlmsafespaceV1() accessor methods)
#   LLMSafespace  (LLM+lower:   LLMSafespaceV1Client type names)
# These were added after PR #248 review caught the gap.
PAT='(?<![sS])(llmsafespace|LLMSAFESPACE|LLMSafeSpace|Llmsafespace|LLMSafespace)(?![sS])'

# ---------- Files to skip entirely -------------------------------------------
SKIP_PATH_RE='(^|/)(\.git|worklogs|design|bin|node_modules)/'
SKIP_FILES_RE='(^|/)(go\.sum|package-lock\.json|workspace-agentd|redact|tools)$'

# Files whose name itself contains llmsafespace (caught by Phase 1 dir renames
# already for the 3 known dirs; any stray files flagged here for manual review).
NAMED_FILES_RE='llmsafespace'

# ============================================================================
# Helpers
# ============================================================================

is_skipped() {
  local p="$1"
  [[ "$p" =~ $SKIP_PATH_RE ]] && return 0
  [[ "$p" =~ $SKIP_FILES_RE ]] && return 0
  return 1
}

# F6 — count *occurrences* (not lines) of all five case-variants in a file,
# using the same boundary-aware regex ($PAT) as the rewriter so dry-run
# counts match the actual number of substitutions on DRY_RUN=0.
count_hits() {
  perl -e '
    my $n = 0;
    open(my $fh, "<", $ARGV[0]) or die "open $ARGV[0]: $!";
    binmode($fh);
    while (<$fh>) {
      while (/(?<![sS])(llmsafespace|LLMSAFESPACE|LLMSafeSpace|Llmsafespace|LLMSafespace)(?![sS])/g) { $n++; }
    }
    print $n;
  ' "$1"
}

# ============================================================================
# Guard — refuse to run if already renamed (F1)
# ============================================================================

# Check non-excluded tracked files for any pre-existing plural form; if found,
# the rename has likely already been applied and re-running risks confusion.
already_renamed=""
while IFS= read -r f; do
  if is_skipped "$f"; then continue; fi
  # Skip the rename tool + its own artifacts (they legitimately mention the
  # target name and would false-positive the guard).
  case "$f" in hack/rename-to-llmsafespaces*) continue;; esac
  if [ -f "$f" ] && grep -qI -- 'llmsafespaces' "$f" 2>/dev/null; then
    already_renamed="$f"
    break
  fi
done < <(git ls-files)

if [ -n "$already_renamed" ]; then
  echo "ERROR: plural form already present in '$already_renamed'." >&2
  echo "       Rename appears to have been applied already; refusing to run" >&2
  echo "       to prevent double-pluralisation. Inspect that file and either" >&2
  echo "       revert it or delete this guard if a partial run occurred." >&2
  exit 2
fi

# ============================================================================
# Main
# ============================================================================

echo "=================================================================="
echo " llmsafespace → llmsafespaces rename"
echo " mode: $([ "$DRY_RUN" = "1" ] && echo 'DRY-RUN (no edits)' || echo 'EXECUTE')"
echo " root: $ROOT"
echo "=================================================================="
echo

# ----- Phase 1: directory renames --------------------------------------------
echo "### Phase 1: directory renames (git mv)"
echo
for entry in "${DIR_RENAMES[@]}"; do
  src="${entry%%:*}"; dst="${entry##*:}"
  if [ -d "$src" ]; then
    if [ "$DRY_RUN" = "1" ]; then
      files=$(git ls-files "$src" | wc -l)
      printf "  [DRY] git mv '%s' '%s'   (%s tracked files)\n" "$src" "$dst" "$files"
    else
      git mv "$src" "$dst"
      printf "  [OK]  %s → %s\n" "$src" "$dst"
    fi
  else
    printf "  [SKIP] %s (not a directory or already moved)\n" "$src"
  fi
done
echo

# ----- Phase 2a: enumerate files needing content edits -----------------------
echo "### Phase 2: content rewrites"
echo
echo "Pattern (single alternation, case-sensitive, boundary-guarded):"
echo "   $PAT"
echo "   → append case-matched 's' (S if token ends in E, else s)"
echo
echo "Excluded paths: worklogs/, design/, .git/, bin/, node_modules/"
echo "Excluded files: go.sum, package-lock.json, workspace-agentd, redact, tools"
echo

# Build list of candidate files (tracked, text, not skipped).
mapfile -t ALL_FILES < <(git ls-files)

declare -a EDIT_FILES=()
declare -a NAMED_LIKE=()
total_hits=0

for f in "${ALL_FILES[@]}"; do
  if is_skipped "$f"; then continue; fi
  # Never rewrite the rename tool itself or its artifacts.
  case "$f" in hack/rename-to-llmsafespaces*) continue;; esac
  # Flag stray files whose NAME contains the token (won't be auto-renamed).
  base="${f##*/}"
  if [[ "$base" =~ $NAMED_FILES_RE ]] && \
     [[ "$f" != pkg/apis/llmsafespa* ]] && \
     [[ "$f" != charts/llmsafespa* ]] && \
     [[ "$f" != sdks/vscode-llmsafespa* ]]; then
    NAMED_LIKE+=("$f")
  fi
  if [ ! -f "$f" ]; then continue; fi
  # F7 — binary detection via a single-char pattern (portable across greps).
  if ! grep -qI . "$f" 2>/dev/null; then continue; fi
  n=$(count_hits "$f")
  if [ "$n" -gt 0 ]; then
    EDIT_FILES+=("$f|$n")
    total_hits=$((total_hits + n))
  fi
done

# ----- Phase 2b: report (or execute) -----------------------------------------
if [ "$DRY_RUN" = "1" ]; then
  printf "Files needing edits: %d\n" "${#EDIT_FILES[@]}"
  printf "Total occurrences across all files: %d\n" "$total_hits"
  echo
  echo "Top 30 files by occurrence count:"
  echo "-----------------------------------------------"
  printf "%6s  %s\n" "HITS" "FILE"
  printf "%6s  %s\n" "-----" "----------------------------------------"
  printf "%s\n" "${EDIT_FILES[@]}" \
    | sort -t'|' -k2 -nr | head -30 \
    | awk -F'|' '{ printf "%6d  %s\n", $2, $1 }' || true
  echo

  # Per-pattern totals (occurrence counts via git grep -o, not lines).
  echo "Per-pattern occurrence counts (non-excluded tree):"
  for pat in llmsafespace LLMSAFESPACE LLMSafeSpace Llmsafespace LLMSafespace; do
    # git grep -o prints each match on its own line; wc -l counts them.
    # Pathspec excludes keep worklogs/ and design/ out of the count.
    c=$(git grep -Ioh -- "$pat" -- . \
        ':(exclude)worklogs/' ':(exclude)design/' \
        ':(exclude)hack/rename-to-llmsafespaces.sh' \
        ':(exclude)hack/rename-to-llmsafespaces.dryrun.txt' \
        2>/dev/null | wc -l || echo 0)
    printf "   %-15s %5d occurrences\n" "$pat" "$c"
  done
  echo
else
  printf "Rewriting %d files...\n" "${#EDIT_FILES[@]}"
  replaced=0
  for entry in "${EDIT_FILES[@]}"; do
    f="${entry%%|*}"
    # Single perl pass over the 5-alternation; appends case-matched 's'.
    # -i: in-place edit; -pe: print+exec per line; /g: all matches; /e: eval
    #   the replacement as perl code (string concat).
    before=$(count_hits "$f")
    perl -i -pe 's/(?<![sS])(llmsafespace|LLMSAFESPACE|LLMSafeSpace|Llmsafespace|LLMSafespace)(?![sS])/$1 . (substr($1,-1) eq "E" ? "S" : "s")/ge' "$f"
    replaced=$((replaced + before))
  done
  printf "  [OK] rewrote %d files (%d occurrences replaced)\n" \
    "${#EDIT_FILES[@]}" "$replaced"
  echo
fi

# ----- Phase 3: files whose NAME contains the token (manual review) ----------
echo "### Phase 3: stray files whose NAME contains 'llmsafespace'"
echo "            (not under one of the 3 renamed dirs — review individually)"
echo
if [ "${#NAMED_LIKE[@]}" -eq 0 ]; then
  echo "  (none)"
else
  for f in "${NAMED_LIKE[@]}"; do
    if [ "$DRY_RUN" = "1" ]; then
      printf "  [DRY] review: %s\n" "$f"
    else
      printf "  [MANUAL] %s (not auto-renamed)\n" "$f"
    fi
  done
fi
echo

# ----- Phase 4: post-rewrite regeneration commands --------------------------
echo "### Phase 4: regeneration commands to run after edits"
echo "            (run these manually; they regenerate derived artifacts)"
echo
cat <<'EOF'
   # --- Go modules (root + SDK) ---
   go mod edit -module github.com/lenaxia/llmsafespaces
   (cd sdks/go && go mod edit -module github.com/lenaxia/llmsafespaces/sdk/go)
   go mod tidy
   (cd sdks/go && go mod tidy)

   # --- CRD YAML (controller-gen → config/crd/bases) ---
   make -C controller manifests

   # --- zz_generated.deepcopy.go (root target → hack/update-deepcopy.sh) ---
   make deepcopy

   # --- npm lockfiles (3 package.json files were rewritten; their
   #     sibling package-lock.json is skipped in Phase 2 and must be
   #     regenerated or `npm ci` fails on name/hash mismatch) ---
   (cd frontend && npm install --package-lock-only)
   (cd sdks/typescript && npm install --package-lock-only)
   (cd sdks/vscode-llmsafespaces && npm install --package-lock-only)

   # --- verify ---
   make test lint
EOF
echo

# ----- Phase 5: external (manual) steps --------------------------------------
echo "### Phase 5: external manual steps"
echo
cat <<'EOF'
   1. GitHub: Settings → Repository → Rename
        lenaxia/LLMSafeSpace → lenaxia/LLMSafeSpaces
      (GitHub auto-redirects old URL; existing clones keep working.)

   2. Container registry (ghcr.io): future pushes use new repo path.
      Old image tags (ghcr.io/lenaxia/llmsafespace/*) become orphans.

   3. npm: publish @llmsafespaces/sdk and vscode-llmsafespaces
      when cutting next release. Old packages can be deprecated.

   4. PyPI: publish llmsafespaces (sdks/python/pyproject.toml)
      when cutting next release. Old 'llmsafespace' can be yanked.

   5. Cloudflare Worker: rename 'llmsafespace-inference-relay' to
      'llmsafespaces-inference-relay' in wrangler.toml + redeploy.

   6. Local Postgres (dev cluster): drop+recreate DB/role as
      'llmsafespaces' (or update values.yaml + local/bootstrap.sh).
EOF
echo

# ----- Summary ---------------------------------------------------------------
echo "=================================================================="
echo " Summary"
echo "=================================================================="
printf "  Dirs to rename        : %d\n" "${#DIR_RENAMES[@]}"
printf "  Files to edit         : %d\n" "${#EDIT_FILES[@]}"
printf "  Total occurrences     : %d\n" "$total_hits"
printf "  Stray-named files     : %d (manual review)\n" "${#NAMED_LIKE[@]}"
if [ "$DRY_RUN" = "1" ]; then
  echo
  echo "  Re-run with DRY_RUN=0 to execute."
fi
echo "=================================================================="
