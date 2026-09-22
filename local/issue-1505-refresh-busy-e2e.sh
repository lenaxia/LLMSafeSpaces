#!/usr/bin/env bash
# Issue #1505 — refresh-compute on a busy pod: the owner's live repro
# (Refresh Compute hangs forever behind the #761 drain when sessions
# are perpetually busy). The user-consented force path must recycle the
# pod immediately despite the busy turn.
#
#   R1 — THE REPRO: a session holds an in-flight turn (a long bash
#        tool). Refresh Compute via the API while the turn runs.
#        PRE-#1505 the drain defers the pod deletion until quiet — on a
#        multi-agent pod that never comes (the repro: refresh hangs
#        indefinitely). POST-#1505 the generation-keyed force marker
#        bypasses the drain: old pod deleted on the first reconcile,
#        replacement pod Active again within the budget, PVC retained.
#   R2 — audit gate (fail-closed): the controller log must show the
#        user-consented force line (reason restart_generation_user_forced)
#        and NOT the drain-defer line for the recycle. Log-fetch failure
#        is a FAIL, never a skip.
#
# Environment: same conventions as local/issue-1507-bounded-suspend-e2e.sh
# (see local/lib/us70-common.sh). Runs on the pool/nightly kind cluster.
# The nightly wiring rides the #1456 lane (the documented follow-up);
# this script is runnable standalone against any harness cluster.
#   LLM_MODEL      - provider/model the turn runs on (required; the
#                    cluster's free-tier relay model)
#   R1_TURN_S      - seconds the R1 tool sleeps (default 300; must
#                    exceed the recycle budget so the turn is
#                    definitionally in-flight when refresh lands)
#   R1_BUDGET_S    - max seconds from refresh call back to Active phase
#                    (default 300; delete + reschedule + boot — the
#                    pre-fix repro never completed at all)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/us70-common.sh
source "${SCRIPT_DIR}/lib/us70-common.sh"

harness_start

[[ -n "${LLM_MODEL:-}" ]] || die "LLM_MODEL must name the model turns run on (e.g. litellm/<free-model>)"
R1_TURN_S="${R1_TURN_S:-300}"
R1_BUDGET_S="${R1_BUDGET_S:-300}"

# Per-script isolation (the #1342 UNCONDITIONAL pattern — the lib's own
# WS_BASE default must never silently apply).
WS_BASE="e2e15050-0000-4000-8000-000000000000"
WS="$(ws_id 1)"
failures=0
note_fail() { failures=$((failures + 1)); warn "FAIL: $*"; }

send_message() { # sid text -> raw response (blocking turn)
    local sid="$1" text="$2" body
    body=$(jq -nc --arg m "${LLM_MODEL}" --arg t "${text}" \
        '{model:{providerID:($m|split("/")[0]),modelID:(($m|split("/")[1:])|join("/"))},parts:[{type:"text",text:$t}]}')
    curl -sfm 240 -X POST \
        -H "Authorization: Bearer ${API_KEY}" \
        -H "Content-Type: application/json" \
        -d "$body" \
        "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WS}/sessions/${sid}/message" \
        2>/dev/null || echo '{}'
}

create_session() {
    curl -sfm 15 -X POST \
        -H "Authorization: Bearer ${API_KEY}" -H "Content-Type: application/json" \
        -d '{}' \
        "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WS}/sessions/new" \
        2>/dev/null | jq -r '.sessionId // .id // .info.id // empty'
}

get_history() { # sid -> raw JSON array of contract messages
    curl -sfm 30 -H "Authorization: Bearer ${API_KEY}" \
        "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WS}/sessions/${1}/message" \
        2>/dev/null || echo '[]'
}

running_tool_parts() { # sid -> count of running tool parts
    get_history "$1" | jq '[.[].parts[]? | select(.tool != null and .tool.state.status == "running")] | length'
}

wait_for() { # <desc> <timeout_s> <predicate-string>
    local desc="$1" timeout="$2" pred="$3"
    local deadline=$(( $(date +%s) + timeout ))
    while (( $(date +%s) < deadline )); do
        if eval "${pred}"; then return 0; fi
        sleep 3
    done
    warn "TIMEOUT(${timeout}s): ${desc}"
    return 1
}

controller_pod() { # -> the controller deployment's pod name ("" when absent)
    kubectl --context "${CTX}" -n "${NS}" get pods \
        -l app.kubernetes.io/component=controller \
        -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo ""
}

# -----------------------------------------------------------------------------
log "setup — seed workspace, wait Active"

seed_workspace "${WS}"
wait_phase "${WS}" Active 240 || die "setup: workspace never Active"
OLD_POD=$(kc get workspace "${WS}" -o jsonpath='{.status.podName}')
OLD_RC=$(kc get workspace "${WS}" -o jsonpath='{.status.restartCount}' 2>/dev/null || echo 0)
[[ "${OLD_RC}" =~ ^[0-9]+$ ]] || OLD_RC=0 # absent field reads as empty-with-exit-0
PVC="workspace-${WS}"
[[ -n "${OLD_POD}" ]] || die "setup: no podName on the CR"
ok "workspace Active, pod ${OLD_POD} (restartCount ${OLD_RC})"

# -----------------------------------------------------------------------------
log "R1 — busy-refresh: in-flight turn + Refresh Compute → recycled within ${R1_BUDGET_S}s"

SID=$(create_session)
if [[ -z "${SID}" ]]; then
    note_fail "R1: no session created"
else
    # The in-flight turn: sleeps well past every budget in this row —
    # definitionally busy when the refresh lands (the repro state: the
    # #761 drain would legitimately wait for it forever on a pod that
    # never goes quiet).
    send_message "${SID}" "Run this exact bash command and do nothing else: echo started; sleep ${R1_TURN_S}" >/dev/null &

    if wait_for "R1: tool part running" 120 '[[ $(running_tool_parts '"${SID}"') -ge 1 ]]'; then
        ok "in-flight turn running (session ${SID})"
    else
        note_fail "R1: tool never reached running state — cannot test busy-refresh without a busy session"
    fi

    CTL=$(controller_pod)
    refresh_t0=$(date +%s)
    refresh_elapsed=0
    curl -sfm 15 -X POST -H "Authorization: Bearer ${API_KEY}" \
        "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WS}/refresh-compute" >/dev/null \
        || note_fail "R1: refresh-compute call failed"

    if wait_phase "${WS}" Active "${R1_BUDGET_S}"; then
        refresh_elapsed=$(( $(date +%s) - refresh_t0 ))
        ok "R1 PASS: back to Active in ${refresh_elapsed}s (budget ${R1_BUDGET_S}s; the repro never completed)"
    else
        note_fail "R1: not Active within ${R1_BUDGET_S}s — the drain gate is back or the wiring diverged"
    fi

    # The recycle must be PROVEN. podName() is deterministic per
    # workspace UID (constants.go: workspaceName + uid[:8]) — the
    # replacement pod carries the SAME NAME, so a name comparison can
    # never distinguish recycle from no-op. restartCount is the
    # identity that actually changes (bumped on every pod
    # replacement): assert the bump.
    NEW_RC=$(kc get workspace "${WS}" -o jsonpath='{.status.restartCount}' 2>/dev/null || echo -1)
    if [[ "${NEW_RC}" =~ ^[0-9]+$ ]] && (( NEW_RC > OLD_RC )); then
        ok "pod recycled: restartCount ${OLD_RC} → ${NEW_RC}"
    else
        note_fail "R1: restartCount did not bump (${OLD_RC} → ${NEW_RC}) — the recycle never happened"
    fi

    # PVC retained — refresh deletes compute, never data.
    if kc get pvc "${PVC}" >/dev/null 2>&1; then
        ok "PVC ${PVC} retained"
    else
        note_fail "R1: PVC ${PVC} gone — refresh must never delete data"
    fi

    # R2 — the audit gate: the forced bypass is VISIBLE (positive log
    # line) and the drain-defer line never fired for the recycle.
    # Fail-closed: no controller pod or failed log fetch is a FAIL, not
    # a skip (the r6 standard from the #1507 row).
    if [[ -z "${CTL}" ]]; then
        note_fail "R2: controller pod not found — the audit assertion cannot be skipped (fail-closed)"
    else
        R2_LOGS="$(mktemp)"
        if ! kubectl --context "${CTX}" -n "${NS}" logs "${CTL}" \
                --since=$((refresh_elapsed + 120))s >"${R2_LOGS}" 2>"${R2_LOGS}.err"; then
            note_fail "R2: controller log fetch failed: $(head -c 200 "${R2_LOGS}.err")"
        elif grep -q 'deferring pod deletion behind busy sessions.*"reason": "restart_generation"' "${R2_LOGS}"; then
            note_fail "R2: the recycle drain-defer line fired — the force marker was not honored"
        elif ! grep -q 'bypassing session drain.*restart_generation_user_forced' "${R2_LOGS}"; then
            note_fail "R2: the user-consented force line is absent from the log — the bypass audit trail is missing"
        else
            ok "R2 PASS: forced bypass logged, no drain-defer (audit trail complete)"
        fi
        rm -f "${R2_LOGS}" "${R2_LOGS}.err"
    fi
fi

# -----------------------------------------------------------------------------
if (( failures > 0 )); then
    die "${failures} row(s) failed"
fi
ok "all rows passed"
