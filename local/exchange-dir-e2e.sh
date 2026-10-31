#!/usr/bin/env bash
# Design 0070 §2.4a — the payload exchange dir, executed at the kind
# layer: the REAL dual-uid mechanism no in-process test can run
# (single-uid test processes cannot cross uids; the agentd sidecar
# image is FROM scratch — no shell to exec). The executor is the
# production path itself: the SIDECAR agentd's user mux /v1/mcp
# (port 4097), whose board payload cleanup hook unlinks the consumed
# file through the dual-rw /exchange mount the runtime container's
# uid-1000 write side created.
#
#   X1 — workspace Active, pod found (both topologies).
#   X2 — /exchange exists in the runtime container at mode 0770
#        (drwxrwx---, no sticky) — the dual-uid contract mode,
#        created by the platform init containers (both topologies).
#   X3 — THE core mechanism (sidecar topology): a payload file
#        created by the RUNTIME container (uid 1000) directly under
#        /exchange, consumed by a real board_create_task call through
#        the sidecar's mux — the call succeeds, the response carries
#        NO cleanup_warning, and the file is GONE from the runtime
#        container's own view. Single-container topology: the same
#        flow executes the mode-uniform delete (uid 1000 holds both
#        roles) — the contract must hold in BOTH modes.
#   X4 — the flat contract (review r1 finding 2), executed: a NESTED
#        payload file (runtime-created subdirectory, umask 0755 — the
#        real degradation shape) is REFUSED at read time by the
#        sidecar's own gate with the "no subdirectories" message —
#        never silently accepted into the warning status quo.
#
# Topology note: the nightly installs controller.agentdSidecar.enabled=true
# (e2e-nightly.yml) — the X3/X4 rows are the sidecar-mode rows. On a
# single-container cluster X3 still runs (mode-uniform delete) and X4
# still runs (the read gate is mode-independent); the script records
# which topology it saw.
#
# Environment: local/lib/us70-common.sh conventions (harness_start,
# seed_workspace, wait_phase, pod_of, kc). No LLM model needed — the
# board manager tools are deterministic platform calls.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/us70-common.sh
source "${SCRIPT_DIR}/lib/us70-common.sh"

harness_start

# Hex-only UUID workspace prefix (review r2 finding 1: the first cut
# carried a literal 'x' — ws_id seeds it into workspaces.id, a uuid
# column, and ON_ERROR_STOP killed the script at X1 before any row).
WS_BASE="e2e070e0-0000-4000-8000-000000000000"
WS="$(ws_id 1)"
TOOL_PF_PORT="${TOOL_PF_PORT:-18220}"
failures=0
note_fail() { failures=$((failures + 1)); warn "FAIL: $*"; }

cleanup() {
    kc delete workspace "${WS}" --ignore-not-found >/dev/null 2>&1 || true
    if [[ -n "${PF_PID:-}" ]]; then
        kill "${PF_PID}" 2>/dev/null || true
        wait "${PF_PID}" 2>/dev/null || true
    fi
}
trap cleanup EXIT

MCP_CALL_SEQ=0
mcp_call() { # tool-name json-args → prints the text of content[0]
    MCP_CALL_SEQ=$((MCP_CALL_SEQ + 1))
    local pfport=$((TOOL_PF_PORT + MCP_CALL_SEQ)) pf _i _out=""
    kc port-forward "pod/${POD}" "${pfport}:4097" >/dev/null 2>&1 &
    pf=$!
    for _i in $(seq 1 15); do
        _out=$(curl -sfm 20 -X POST "http://127.0.0.1:${pfport}/v1/mcp" \
            -u "opencode:${WS_PW}" \
            -H "Content-Type: application/json" \
            -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"'"$1"'","arguments":'"$2"'}}' 2>/dev/null || true)
        [[ -n "${_out}" ]] && break
        sleep 4
    done
    kill "${pf}" 2>/dev/null || true
    wait "${pf}" 2>/dev/null || true
    printf '%s' "${_out}" | jq -r '.result.content[0].text // empty'
}

runtime_exec() { # args... → stdout of the runtime container
    local rc
    rc=$(runtime_container "${POD}")
    kc exec "${POD}" ${rc:+-c "$rc"} -- "$@"
}

log "X1 — workspace ${WS} up"
seed_workspace "${WS}"
wait_phase "${WS}" Active 420 || die "X1: workspace never Active"
POD=$(pod_of "${WS}")
[[ -n "${POD}" ]] || die "X1: no pod for ${WS}"
WS_PW=$(kc get secret "workspace-pw-${WS}" -o jsonpath='{.data.password}' | base64 -d)
ok "X1: workspace Active (pod ${POD})"

if kc get pod "${POD}" -o jsonpath='{.status.initContainerStatuses[?(@.name=="agentd")].state.running}' 2>/dev/null | grep -q started; then
    TOPOLOGY="sidecar"
else
    TOPOLOGY="single-container"
fi
warn "X1: topology detected: ${TOPOLOGY}"

log "X2 — /exchange dual-uid mode (0770, no sticky) in the runtime container"
X2_MODE=$(runtime_exec stat -c '%a' /exchange 2>/dev/null || true)
if [[ "${X2_MODE}" == "770" ]]; then
    ok "X2: /exchange mode 0770 — the shared-gid dual-uid contract (init-fs created)"
else
    note_fail "X2: /exchange mode is '${X2_MODE:-absent}' — want 770 (group rwx, no sticky)"
fi

log "X3 — the core mechanism: uid-1000 create → board payload call → file DELETED, no cleanup_warning"
PROBE="/exchange/probe-$(date +%s).json"
EPICFILE="/exchange/epic-$(date +%s).json"
runtime_exec sh -c "printf '%s' '{\"title\":\"E-exchange\",\"outcome\":\"kind execution of 0070 §2.4a\"}' > '${EPICFILE}'" >/dev/null
runtime_exec sh -c "printf '%s' '{\"title\":\"T-exchange\",\"description\":\"created by the runtime uid 1000, deleted through the platform\",\"acceptanceCriteria\":[{\"text\":\"file deleted on success\"}]}' > '${PROBE}'" >/dev/null
[[ -n "$(runtime_exec cat "${PROBE}" 2>/dev/null)" ]] || note_fail "X3 setup: probe payload not written by the runtime container"

PROJECT_OUT="$(mcp_call board_create_project '{"name":"exchange-e2e"}')"
PROJECT_ID=$(printf '%s' "${PROJECT_OUT}" | jq -r '.project.id // .id // .project_id // empty')
[[ -n "${PROJECT_ID}" ]] || note_fail "X3: board_create_project returned no project id: ${PROJECT_OUT:0:200}"
EPIC_OUT="$(mcp_call board_create_epic "{\"projectId\":\"${PROJECT_ID}\",\"bodyFile\":\"${EPICFILE}\"}")"
EPIC_ID=$(printf '%s' "${EPIC_OUT}" | jq -r '.epic.id // .id // .epic_id // empty')
[[ -n "${EPIC_ID}" ]] || note_fail "X3: board_create_epic returned no id: ${EPIC_OUT:0:200}"

TASK_OUT="$(mcp_call board_create_task "{\"epicId\":\"${EPIC_ID}\",\"bodyFile\":\"${PROBE}\"}")"
if printf '%s' "${TASK_OUT}" | grep -q '"short_id"'; then
    if printf '%s' "${TASK_OUT}" | grep -q 'cleanup_warning'; then
        note_fail "X3: the create succeeded but STILL warned: $(printf '%s' "${TASK_OUT}" | jq -r '.cleanup_warning // empty' | head -c 300)"
    else
        ok "X3: board_create_task succeeded with NO cleanup_warning (topology: ${TOPOLOGY})"
    fi
else
    note_fail "X3: board_create_task did not succeed: ${TASK_OUT:0:300}"
fi
if [[ -z "$(runtime_exec cat "${PROBE}" 2>/dev/null)" ]]; then
    ok "X3: the runtime-created payload file is GONE from the runtime container's own view — the ${TOPOLOGY} delete executed"
else
    note_fail "X3: the payload file SURVIVED its successful call (the pre-fix symptom)"
fi

log "X4 — the flat contract: a NESTED payload file is refused at read time (never silently degraded)"
NESTED="/exchange/nested-$(date +%s)/probe.json"
runtime_exec sh -c "umask 022; mkdir -p '$(dirname "${NESTED}")' && printf '%s' '{\"title\":\"T-nested\",\"description\":\"d\",\"acceptanceCriteria\":[{\"text\":\"c\"}]}' > '${NESTED}'" >/dev/null
NESTED_MODE=$(runtime_exec stat -c '%a' "$(dirname "${NESTED}")" 2>/dev/null || true)
if [[ "${NESTED_MODE}" != "770" ]]; then
    ok "X4 setup: runtime-created subdirectory is ${NESTED_MODE:-?} (no group-write) — the real degradation shape"
else
    warn "X4 setup: subdirectory unexpectedly group-writable (${NESTED_MODE}) — the degradation premise did not reproduce; the refusal row below still must hold"
fi
NESTED_OUT="$(mcp_call board_create_task "{\"epicId\":\"${EPIC_ID}\",\"bodyFile\":\"${NESTED}\"}")"
if printf '%s' "${NESTED_OUT}" | grep -q 'no subdirectories'; then
    ok "X4: the nested payload path is REFUSED with the flat-contract message (the read-time gate, ${TOPOLOGY} topology)"
else
    note_fail "X4: nested payload path was not refused by the flat-contract gate: ${NESTED_OUT:0:300}"
fi

if [[ "${failures}" != "0" ]]; then
    die "${failures} row(s) failed"
fi
log "exchange-dir e2e: all rows passed (topology: ${TOPOLOGY})"
