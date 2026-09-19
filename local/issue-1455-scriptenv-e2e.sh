#!/usr/bin/env bash
# Issue #1455 — the script-node ENVIRONMENT CONTRACT, exercised against
# a real workspace pod through the platform API. agentd serves
# /v1/workflow/node/execute from the container it lives in: in
# single-container mode that IS the workspace container (interpreters +
# writable /tmp via the PVC — scripts must still RUN there, guarding
# EnvCheck against false positives in the real toolchain env); in
# sidecar mode (agentdSidecar.enabled, pre-flip clusters lack it) it is
# the FROM-scratch sidecar, where a script node must fail LOUD with
# errorCode script_env_unavailable naming the known cause — never the
# pre-#1455 incidental "create temp dir: stat /tmp" script_failed.
#
#   R1 — mode contract (adaptive, never silently skipped): a one-node
#        python script workflow runs to a terminal state that is EITHER
#        (a) succeeded with the handler's marker output (single-
#        container mode: script nodes still execute post-EnvCheck), OR
#        (b) failed with script_env_unavailable + the sidecar-naming
#        detail (sidecar mode: the loud #1455 contract). Any other
#        terminal shape — the old script_failed stat error, timeouts,
#        fork/exec failures — fails the row.
#
# The http-node {{secrets.*}} coordinate half of #1455 is covered
# piecewise: the controller→sidecar env wiring is pinned by
# TestUS4B_Enabled_SidecarPathEnv, the agentd reader honoring it by the
# handler tests in cmd/workspace-agentd/workflow_execute_test.go (a
# live row would need a bound secret + a cluster-reachable echo
# receiver the e2e harness does not provide).
#
# Environment: same conventions as local/issue1452-routine-session-index-e2e.sh
# (local/lib/us70-common.sh). Runs on the pool/nightly kind cluster.
# No LLM_MODEL required — the script node never calls a model.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/us70-common.sh
source "${SCRIPT_DIR}/lib/us70-common.sh"

WS_BASE="${WS_BASE:-e2e145500-0000-4000-8000-000000000000}"
WS="$(ws_id 1)"
RUN_WAIT_S="${RUN_WAIT_S:-300}"
failures=0
note_fail() { failures=$((failures + 1)); warn "FAIL: $*"; }
created_workflows=()

cleanup() {
    local id
    for id in ${created_workflows[*]:-}; do
        curl -sfm 10 -X DELETE -H "Authorization: Bearer ${API_KEY}" \
            "http://127.0.0.1:${PORTFWD_PORT}/api/v1/me/workflows/${id}" >/dev/null 2>&1 || true
    done
}
trap cleanup EXIT

curl -sfm 2 "http://127.0.0.1:${PORTFWD_PORT}/livez" >/dev/null \
    || die "API /livez unreachable on ${PORTFWD_PORT} (is the e2e cluster port-forward up?)"

api() { # method path [body] -> response body; status in ${api_status}
    local method="$1" path="$2" body="${3:-}"
    local args=(-s -m 20 -X "${method}" -H "Authorization: Bearer ${API_KEY}" \
        -H "Content-Type: application/json" -w '\n%{http_code}' \
        "http://127.0.0.1:${PORTFWD_PORT}${path}")
    [[ -n "${body}" ]] && args+=(-d "${body}")
    local out; out=$(curl "${args[@]}")
    api_status="${out##*$'\n'}"
    printf '%s' "${out%$'\n'*}"
}

log "R0 — workspace ${WS} up"
seed_workspace "${WS}"
wait_phase "${WS}" Active 360 || die "R0: workspace never Active"
ok "workspace Active"

# -----------------------------------------------------------------------------
log "R1 — script-node mode contract (execute OR fail loud, nothing between)"

WF_BODY=$(jq -nc --arg w "${WS}" '{name:"e2e-1455-scriptenv",targetWorkspaceId:$w,
    specYaml:"{\"nodes\":[{\"id\":\"s1\",\"type\":\"script\",\"data\":{\"language\":\"python\",\"handler\":\"def handler(input):\\n    return {\\\"marker\\\": \\\"e2e-1455-scriptenv-ran\\\"}\\n\"}}],\"edges\":[]}"}')
WF_RESP=$(api POST /api/v1/me/workflows "${WF_BODY}")
if [[ "${api_status}" != "201" ]]; then
    die "R1 setup: workflow create failed: ${api_status} ${WF_RESP}"
fi
WF_ID=$(printf '%s' "${WF_RESP}" | jq -r '.id')
created_workflows+=("${WF_ID}")

RUN_RESP=$(api POST "/api/v1/me/workflows/${WF_ID}/runs" '{"input":{}}')
[[ "${api_status}" == "201" || "${api_status}" == "202" ]] \
    || die "R1 setup: run create failed: ${api_status} ${RUN_RESP}"
RUN_ID=$(printf '%s' "${RUN_RESP}" | jq -r '.id // empty')

# Poll the run to a terminal state (succeeded | failed), bounded.
r1_row=""
deadline=$(( $(date +%s) + RUN_WAIT_S ))
while true; do
    runs_json=$(api GET "/api/v1/me/workflows/${WF_ID}/runs")
    r1_row=$(printf '%s' "${runs_json}" | jq -c --arg r "${RUN_ID}" \
        '[.runs[] | select(.id == $r)] | first // empty')
    r1_status=$(printf '%s' "${r1_row}" | jq -r '.status // empty')
    if [[ "${r1_status}" == "succeeded" || "${r1_status}" == "failed" ]]; then
        break
    fi
    if [[ $(date +%s) -ge ${deadline} ]]; then
        note_fail "R1: run never reached a terminal state within ${RUN_WAIT_S}s (last: ${r1_status:-none})"
        break
    fi
    sleep 5
done

if [[ -n "${r1_row}" ]]; then
    r1_dump=$(printf '%s' "${r1_row}" | jq -r 'tostring')
    if [[ "${r1_status}" == "succeeded" && "${r1_dump}" == *"e2e-1455-scriptenv-ran"* ]]; then
        ok "R1a: single-container mode — script node executed post-EnvCheck (marker output present)"
    elif [[ "${r1_status}" == "failed" && "${r1_dump}" == *"script_env_unavailable"* ]]; then
        if [[ "${r1_dump}" == *"sidecar"* ]]; then
            ok "R1b: sidecar mode — script node failed LOUD (script_env_unavailable naming the sidecar cause)"
        else
            note_fail "R1b: failed with script_env_unavailable but the detail does not name the sidecar cause: ${r1_dump}"
        fi
    elif [[ "${r1_status}" == "succeeded" ]]; then
        note_fail "R1a: run succeeded but the handler marker is missing from the run row: ${r1_dump}"
    else
        note_fail "R1: terminal shape outside the mode contract (status=${r1_status}): ${r1_dump}"
    fi
fi

if [[ "${failures}" -gt 0 ]]; then
    die "${failures} scriptenv e2e row(s) failed"
fi
log "issue-1455 scriptenv e2e: all rows green"
