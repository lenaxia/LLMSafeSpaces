#!/usr/bin/env bash
# Issue #1808 — zero-fill corruption audit, live e2e against a harness
# cluster. Parity with local/1601-disk-recovery-e2e.sh (the #1601 twin
# this feature explicitly mirrors).
#
#   R1 — THE HAPPY PATH: a workspace seeded with (a) a real all-NUL
#        allocated file (the #1808 dead-file shape), (b) a SPARSE
#        all-NUL file (the false-positive trap — ftruncate-created,
#        legitimate), and (c) a clean text file. The API audit reports
#        EXACTLY (a) as nul_filled: (b) reports sparse and is never
#        flagged, (c) is invisible. A sub-root audit stays confined.
#   R2 — THE UNHAPPY PATHS: anonymous call → 401; a workspace the
#        caller does not own (random UUID) → 404.
#
# Environment: same conventions as local/1601-disk-recovery-e2e.sh
# (local/lib/us70-common.sh; kind cluster with the API service).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/us70-common.sh
source "${SCRIPT_DIR}/lib/us70-common.sh"

W1_WS="e21801fsaudit"

harness_start

seed_workspace "${W1_WS}" >/dev/null
wait_phase "${W1_WS}" Active 600
ok "workspace ${W1_WS} Active"

W1_POD=$(kc get pods -l "llmsafespaces.dev/workspace=${W1_WS}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
[[ -n "${W1_POD}" ]] || W1_POD=$(kc get pods -o name | grep -m1 "${W1_WS}" | cut -d/ -f2)
[[ -n "${W1_POD}" ]] || die "no pod found for ${W1_WS}"

api() { # method path [curl-args...]
    local method="$1" path="$2"; shift 2
    curl -sm 60 -X "${method}" \
        -H "Authorization: Bearer ${AUTH_TOKEN:?}" -H "Content-Type: application/json" \
        "$@" "http://127.0.0.1:${PORTFWD_PORT}/api/v1${path}"
}

# --- seed: the three-way discrimination fixture, live on the real PVC ---
# dead.yaml   : 64KiB of WRITTEN NUL bytes (allocated — the corruption shape)
# sparse.bin  : 2MiB ftruncate hole (all-NUL, NOT allocated — the trap)
# live.txt    : ordinary text
kc exec "${W1_POD}" -c workspace -- sh -c '
    mkdir -p /workspace/sub &&
    dd if=/dev/zero of=/workspace/dead.yaml bs=4k count=16 status=none &&
    dd if=/dev/zero of=/workspace/sparse.bin bs=1 count=0 seek=2M status=none &&
    printf "# pin v1.2.3\n" > /workspace/live.txt &&
    dd if=/dev/zero of=/workspace/sub/dead2.yaml bs=4k count=8 status=none
' >/dev/null || die "seed failed (pod ${W1_POD})"

# --- R1a: the API audit flags EXACTLY the dead files ---
AUDIT=$(api POST "/workspaces/${W1_WS}/fs-audit")
[[ "$(jq -r '.nulFilledCount' <<<"${AUDIT}")" == "2" ]] \
    || die "nulFilledCount: want 2 (dead.yaml + sub/dead2.yaml), got: ${AUDIT}"
[[ "$(jq -r '.nulFilled[] | select(.path=="dead.yaml") | .verdict' <<<"${AUDIT}")" == "nul_filled" ]] \
    || die "dead.yaml must be nul_filled: ${AUDIT}"
[[ "$(jq -r '.nulFilled[] | select(.path=="dead.yaml") | .size' <<<"${AUDIT}")" == "65536" ]] \
    || die "dead.yaml size must be preserved-length 65536: ${AUDIT}"
[[ "$(jq -r '.sparseCount' <<<"${AUDIT}")" == "1" ]] \
    || die "sparse.bin must report sparse (the false-positive trap, live): ${AUDIT}"
[[ -n "$(jq -r '.nulFilled[] | select(.path=="sparse.bin")' <<<"${AUDIT}")" ]] \
    && die "BOUNDARY: sparse.bin must NEVER be flagged nul_filled: ${AUDIT}" || true
[[ -z "$(jq -r '.nulFilled[] | select(.path=="live.txt")' <<<"${AUDIT}")" ]] \
    || die "live.txt must be invisible to the dead-file list: ${AUDIT}"
[[ "$(jq -r '.errorCount' <<<"${AUDIT}")" == "0" ]] || die "no read errors expected: ${AUDIT}"
[[ "$(jq -r '.truncated' <<<"${AUDIT}")" == "false" ]] || die "small fixture must not truncate: ${AUDIT}"
ok "R1a API audit: 2 dead flagged, sparse reported-not-flagged, clean invisible"

# --- R1b: sub-root confinement (the root param stays inside the workspace) ---
SUB=$(api POST "/workspaces/${W1_WS}/fs-audit?root=sub")
[[ "$(jq -r '.nulFilledCount' <<<"${SUB}")" == "1" ]] \
    || die "sub-root audit must see exactly sub/dead2.yaml: ${SUB}"
[[ "$(jq -r '.nulFilled[0].path' <<<"${SUB}")" == "sub/dead2.yaml" ]] \
    || die "sub-root path shape: ${SUB}"
ESCAPE=$(api POST "/workspaces/${W1_WS}/fs-audit?root=../../etc")
[[ "$(jq -r '.errorCount' <<<"${ESCAPE}")" == "1" ]] \
    || die "escape-attempt root must ground to a missing subdir and report exactly 1 walk error: ${ESCAPE}"
[[ "$(jq -r '.nulFilledCount' <<<"${ESCAPE}")" == "0" ]] \
    || die "escape-attempt root must flag nothing (grounded inside the workspace): ${ESCAPE}"
ok "R1b confinement: sub-root scoped; ../../etc grounded + loud, no scan outside"

# --- R1c: the audit writes nothing (response-only ruling, live) ---
kc exec "${W1_POD}" -c workspace -- sh -c '
    test -f /workspace/dead.yaml && test -f /workspace/sparse.bin && test -f /workspace/live.txt
' >/dev/null || die "RESPONSE-ONLY VIOLATION: the audit mutated the fixture tree"
ok "R1c response-only: fixture tree untouched by three audits"

# --- R2: the unhappy paths ---
CODE=$(curl -sm 15 -o /dev/null -w '%{http_code}' -X POST \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${W1_WS}/fs-audit")
[[ "${CODE}" == "401" ]] || die "anonymous call: want 401, got ${CODE}"

CODE=$(curl -sm 15 -o /dev/null -w '%{http_code}' -X POST \
    -H "Authorization: Bearer ${AUTH_TOKEN}" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/00000000-0000-0000-0000-000000000000/fs-audit")
[[ "${CODE}" == "404" ]] || die "foreign workspace: want 404, got ${CODE}"

CODE=$(curl -sm 15 -o /dev/null -w '%{http_code}' -X PUT \
    -H "Authorization: Bearer ${AUTH_TOKEN}" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${W1_WS}/fs-audit")
[[ "${CODE}" == "404" ]] || die "non-POST method on the facade: want 404 (no route), got ${CODE}"
ok "R2 authz: anonymous 401, foreign workspace 404, non-POST 404"

ok "ALL ROWS GREEN: 1808-fs-audit-e2e"
