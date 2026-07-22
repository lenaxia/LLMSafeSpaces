#!/usr/bin/env bash
# Hermetic test for the wait-for-ci jq filters in
# .github/workflows/release.yml. Pure bash + jq — no external test
# framework, no network, no GitHub API calls.
#
# The wait-for-ci job polls commits/{ref}/check-runs and derives four
# signals (FAILED, ALL_DONE, CI_CHECKS, RUNNING) via jq. This test
# extracts the EXACT jq expressions from release.yml (no copy/paste
# drift) and asserts they behave correctly on three payloads that have
# bitten the gate in production:
#
#   1. Stale-failure + new-success (retried CI job) — the v0.4.3 block.
#      The API persists BOTH attempts; without dedup-by-latest, FAILED
#      honours the stale failure and blocks a green retry.
#   2. Self-reference (wait-for-ci's own in-progress check run on the
#      ref) — the v0.4.2 block (#577). Without the Release-name filter,
#      ALL_DONE counts itself and the success branch is unreachable.
#   3. Ancillary nightly workflow (gremlins mutation tests) on the SHA
#      — the v0.4.2 block (#578). Not a tag-push gate; must be excluded.
#
# Usage: ./hack/release-wait-for-ci.test.sh
set -uo pipefail

WORKFLOW="$(dirname "$0")/../.github/workflows/release.yml"
PASS=0
FAIL=0

# Release-workflow job names excluded from every computation. Extracted
# from the workflow so a rename there is caught here.
EXCLUDE_RE='Verify CHANGELOG|Wait for CI|Sign images \(cosign|Publish Helm|Generate SBOM|Build relay-proxy|Create GitHub Release|gremlins|Scan images \(Trivy\)'

# Extract the four jq EXPRESSIONS from release.yml by anchored matches on
# their assignment line. Each expression is a contiguous jq program in a
# $(printf '%s' "$RESPONSE" | jq -r ' ... ') block; the jq program starts
# after the opening quote on the assignment line and ends at the line that
# is exactly `"')"`. We pull the inner jq and feed it payloads directly —
# no copy/paste drift between this test and the workflow.
extract_jq() {
  local marker="$1"
  python3 - "$WORKFLOW" "$marker" <<'PY'
import sys
path, marker = sys.argv[1], sys.argv[2]
lines = open(path).read().splitlines()
try:
    start = next(i for i, l in enumerate(lines) if l.lstrip().startswith(marker))
except StopIteration:
    print(f"::error::marker not found: '{marker}'", file=sys.stderr)
    sys.exit(2)
# opening quote is the trailing "'" on the assignment line; collect until
# the closing line which is exactly `"')"` (possibly indented).
buf = []
for l in lines[start:]:
    buf.append(l)
    if l.strip() == "')":
        break
block = "\n".join(buf)
# Capture between the single quote after "jq -r " and the closing "')".
import re
m = re.search(r"jq -r '\n?(.*?)\n?'\s*\)\s*$", block, re.S)
if not m:
    print(f"::error::could not extract jq after marker '{marker}'", file=sys.stderr)
    sys.exit(2)
print(m.group(1))
PY
}

# Each computation's assignment-line prefix (unique per variable).
JQ_FAILED=$(extract_jq "FAILED=\$(printf")
JQ_ALL_DONE=$(extract_jq "ALL_DONE=\$(printf")
JQ_CI_CHECKS=$(extract_jq "CI_CHECKS=\$(printf")

run_jq() {
  local expr="$1"
  local payload="$2"
  printf '%s' "$payload" | jq -r "$expr"
}

assert_eq() {
  local actual="$1" expected="$2" label="$3"
  if [ "$actual" = "$expected" ]; then
    echo "  OK   ${label} (got='${actual}')"
    PASS=$((PASS + 1))
  else
    echo "  FAIL ${label} — expected='${expected}' got='${actual}'"
    FAIL=$((FAIL + 1))
  fi
}

assert_empty() {
  local actual="$1" label="$2"
  if [ -z "$actual" ]; then
    echo "  OK   ${label} (empty)"
    PASS=$((PASS + 1))
  else
    echo "  FAIL ${label} — expected empty, got='${actual}'"
    FAIL=$((FAIL + 1))
  fi
}

# --- Payloads (real check-run shapes; only the fields the filters read) ---

# v0.4.3 scenario: race detector failed on attempt 1, succeeded on retry.
# The API returns both records; the gate must honour the latest (success).
PAYLOAD_RETRY='{"check_runs":[
  {"name":"Test (full suite, race detector)","status":"completed","conclusion":"failure","started_at":"2026-07-22T16:01:26Z"},
  {"name":"Test (full suite, race detector)","status":"completed","conclusion":"success","started_at":"2026-07-22T16:01:44Z"},
  {"name":"Lint","status":"completed","conclusion":"success","started_at":"2026-07-22T16:00:00Z"},
  {"name":"Verify CHANGELOG section exists","status":"completed","conclusion":"success"},
  {"name":"Wait for CI workflow","status":"in_progress","conclusion":null}
]}'

# v0.4.2 #577 scenario: the job's own in-progress check run is on the ref.
# ALL_DONE must exclude it or the success branch is unreachable.
PAYLOAD_SELF='{"check_runs":[
  {"name":"Lint","status":"completed","conclusion":"success"},
  {"name":"Wait for CI workflow","status":"in_progress","conclusion":null},
  {"name":"Verify CHANGELOG section exists","status":"completed","conclusion":"success"}
]}'

# v0.4.2 #578 scenario: nightly gremlins failure on the SHA. Not a tag
# gate; must be excluded from FAILED.
PAYLOAD_GREMLINS='{"check_runs":[
  {"name":"Lint","status":"completed","conclusion":"success"},
  {"name":"gremlins (mutation tests) (pkg/secrets)","status":"completed","conclusion":"failure"},
  {"name":"gremlins (mutation tests) (api/internal/services/auth)","status":"completed","conclusion":"failure"},
  {"name":"Verify CHANGELOG section exists","status":"completed","conclusion":"success"},
  {"name":"Wait for CI workflow","status":"in_progress","conclusion":null}
]}'

# A genuine CI failure (not stale, not excluded) must still be reported.
PAYLOAD_REAL_FAIL='{"check_runs":[
  {"name":"Lint","status":"completed","conclusion":"success"},
  {"name":"Frontend (unit + typecheck + e2e)","status":"completed","conclusion":"failure"},
  {"name":"Verify CHANGELOG section exists","status":"completed","conclusion":"success"},
  {"name":"Wait for CI workflow","status":"in_progress","conclusion":null}
]}'

echo "Test 1: retried CI job — stale failure is superseded by later success"
assert_empty "$(run_jq "$JQ_FAILED" "$PAYLOAD_RETRY")" "FAILED empty on green retry"
assert_eq "$(run_jq "$JQ_ALL_DONE" "$PAYLOAD_RETRY")" "0" "ALL_DONE=0 (no real CI running)"

echo "Test 2: self-reference — wait-for-ci's own in-progress run excluded from ALL_DONE"
assert_eq "$(run_jq "$JQ_ALL_DONE" "$PAYLOAD_SELF")" "0" "ALL_DONE=0 (does not count itself)"

echo "Test 3: nightly gremlins failure excluded from FAILED"
assert_empty "$(run_jq "$JQ_FAILED" "$PAYLOAD_GREMLINS")" "FAILED empty (gremlins excluded)"

echo "Test 4: genuine non-excluded CI failure still reported"
assert_eq "$(run_jq "$JQ_FAILED" "$PAYLOAD_REAL_FAIL")" "Frontend (unit + typecheck + e2e)" "FAILED names real failure"

echo "Test 5: CI_CHECKS counts unique CI jobs, not Release jobs or duplicates"
assert_eq "$(run_jq "$JQ_CI_CHECKS" "$PAYLOAD_RETRY")" "2" "CI_CHECKS=2 (Lint + Test, deduped)"

echo ""
echo "=== summary ==="
echo "PASS: ${PASS}"
echo "FAIL: ${FAIL}"
[ "${FAIL}" -eq 0 ]
