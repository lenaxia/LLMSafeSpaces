#!/usr/bin/env bash
# renumber-pr-num.sh — extract the PR number from a merge/squash commit
# for the worklog-renumber bot's PR comment.
#
# Bug this exists to prevent (run 33428514243): `tail -1` over all #N
# refs picked an ISSUE number out of a squash body ("... (#1182)" title
# ref + "Resolves #1183" in the body) — the bot commented on issue 1183
# (not a PR) and failed its job after the rename had already landed.
#
# Rules:
#   1. GitHub's squash/merge convention suffixes the SUBJECT with "(#PR)"
#      as the last parenthesized ref — prefer that (when several trail,
#      the LAST is the PR).
#   2. Fall back to any #N (merge-commit subjects: "Merge pull request
#      #1190 from ...").
#   3. Otherwise empty (bot commits, cron-path heads) — caller skips.
#
# Usage: renumber-pr-num.sh <subject> <full-body>
# Prints the number (no '#') or nothing. Pure: no git, no network.
set -euo pipefail
SUBJECT="${1:-}"
BODY="${2:-}"

# Rule 1: trailing (#N) at end of the subject line.
TRAILING=$(printf '%s' "$SUBJECT" | grep -oE '\(#[0-9]+\)$' | grep -oE '[0-9]+' || true)
if [ -n "$TRAILING" ]; then
  echo "$TRAILING"
  exit 0
fi

# Rule 2: any #N — SUBJECT first (merge commits: "Merge pull request
# #1190 from ..."), body only as a last resort. Subject-before-body
# order matters: a body "Closes #1182" must never beat the subject's PR.
ANY=$(printf '%s' "$SUBJECT" | grep -oE '#[0-9]+' | tail -1 | tr -d '#' || true)
if [ -z "$ANY" ]; then
  ANY=$(printf '%s' "$BODY" | grep -oE '#[0-9]+' | tail -1 | tr -d '#' || true)
fi
if [ -n "$ANY" ]; then
  echo "$ANY"
  exit 0
fi

# Rule 3: nothing — print empty, exit 0 (caller treats as skip).
exit 0
