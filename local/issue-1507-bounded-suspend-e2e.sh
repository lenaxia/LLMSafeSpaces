#!/usr/bin/env bash
# Issue #1507 — bounded suspend at cluster scale: the 2026-09-21 incident
# replay (a busy in-flight turn must NOT block suspend; the pod's
# termination grace is the bounded window; the PVC survives).
#
#   R1 — THE INCIDENT REPLAY (busy-suspend, the owner's repro): a
#        session holds an in-flight turn (a long-running bash tool —
#        the same in-flight-turn shape the #761 drain was built
#        around, and the exact state the wedged-opencode incident
#        faked forever). Suspend via the API while the turn runs.
#        PRE-#1507 the controller deferred pod deletion behind busy
#        sessions and Suspending hung indefinitely (the incident: 1h+,
#        manual kubectl delete the only fix). POST-#1507 the suspend
#        must complete within grace + reconcile latency: phase
#        Suspended, pod gone, PVC retained.
#   R2 — controller-log gate: no "deferring pod deletion behind busy
#        sessions" with reason=suspend appears during the flow (AC1's
#        log assertion — the drain line is unreachable from suspend).
#
# Environment: same conventions as local/us-70-secret-delivery-e2e.sh
# (see local/lib/us70-common.sh). Runs on the pool/nightly kind
# cluster. The nightly wiring rides the next #1456-lane touch (the
# documented follow-up); this script is runnable standalone against any
# harness cluster.
#   LLM_MODEL      - provider/model the turn runs on (required; the
#                    cluster's free-tier relay model)
#   R1_TURN_S      - seconds the R1 tool sleeps (default 300; must
#                    exceed the suspend-completion budget so the turn
#                    is definitionally in-flight when suspend lands)
#   R1_BUDGET_S    - max seconds from suspend call to Suspended phase
#                    (default 120; grace 40s + reconcile latency +
#                    margin — the incident was 3600+)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")")" && pwd
# shellcheck source=lib/us70-common.sh
source "${SCRIPT_DIR}/lib/us70-common.sh"

harness_start

[[ -n "${LLM_MODEL:-}" ]] || die "LLM_MODEL must name the model turns run on (e.g. litellm/<free-model>)"
R1_TURN_S="${R1_TURN_S:-300}"
R1_BUDGET_S="${R1_BUDGET_S:-120}"

# Per-script isolation (the #1342 UNCONDITIONAL pattern — the lib's own
# WS_BASE default must never silently apply).
WS_BASE="e2e15070-0000-4000-8000-000000000000"
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
    # The chart labels controller pods app.kubernetes.io/component=
    # controller (helm _helpers controller.selectorLabels).
    kubectl --context "${CTX}" -n "${NS}" get pods \
        -l app.kubernetes.io/component=controller \
        -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo ""
}

# -----------------------------------------------------------------------------
log "setup — seed workspace, wait Active"

seed_workspace "${WS}"
wait_phase "${WS}" Active 240 || die "setup: workspace never Active"
POD=$(kc get workspace "${WS}" -o jsonpath='{.status.podName}')
PVC="workspace-${WS}"
[[ -n "${POD}" ]] || die "setup: no podName on the CR"
ok "workspace Active, pod ${POD}"

# -----------------------------------------------------------------------------
log "R1 — busy-suspend: in-flight turn + API suspend → Suspended within ${R1_BUDGET_S}s"

SID=$(create_session)
if [[ -z "${SID}" ]]; then
    note_fail "R1: no session created"
else
    # The in-flight turn: sleeps well past every budget in this row —
    # definitionally busy when the suspend lands (the incident state,
    # minus needing a genuinely wedged opencode: a live busy turn is
    # the STRONGER case — pre-#1507 the drain would legitimately wait
    # for it, exactly what the incident faked forever).
    send_message "${SID}" "Run this exact bash command and do nothing else: echo started; sleep ${R1_TURN_S}" >/dev/null &

    if wait_for "R1: tool part running" 120 '[[ $(running_tool_parts '"${SID}"') -ge 1 ]]'; then
        ok "in-flight turn running (session ${SID})"
    else
        note_fail "R1: tool never reached running state — cannot test busy-suspend without a busy session"
    fi

    CTL=$(controller_pod)
    suspend_t0=$(date +%s)
    curl -sfm 10 -X POST -H "Authorization: Bearer ${API_KEY}" \
        "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WS}/suspend" >/dev/null \
        || note_fail "R1: suspend call failed"

    if wait_phase "${WS}" Suspended "${R1_BUDGET_S}"; then
        suspend_elapsed=$(( $(date +%s) - suspend_t0 ))
        ok "R1 PASS: Suspended in ${suspend_elapsed}s (budget ${R1_BUDGET_S}s; the incident was 3600+)"
    else
        note_fail "R1: not Suspended within ${R1_BUDGET_S}s — the busy gate is back or the wiring diverged"
    fi

    # Pod gone.
    if kc get pod "${POD}" >/dev/null 2>&1; then
        note_fail "R1: pod ${POD} still exists after Suspended"
    else
        ok "pod deleted"
    fi

    # PVC retained — suspend deletes compute, never data.
    if kc get pvc "${PVC}" >/dev/null 2>&1; then
        ok "PVC ${PVC} retained"
    else
        note_fail "R1: PVC ${PVC} gone — suspend must never delete data"
    fi

    # R2 — AC1's log assertion: the drain-defer line never fired for
    # suspend. Only checkable when the controller pod is discoverable;
    # skip-with-note otherwise (the phase/timing assertions above carry
    # the row even without log access).
    if [[ -n "${CTL}" ]]; then
        if kubectl --context "${CTX}" -n "${NS}" logs "${CTL}" --since=$((suspend_elapsed + 60))s 2>/dev/null \
            | grep -q 'deferring pod deletion behind busy sessions.*"reason": "suspend"'; then
            note_fail "R2: the suspend drain-defer line fired — the busy gate is back (AC1 violated)"
        else
            ok "R2 PASS: no suspend drain-defer in the controller log (AC1)"
        fi
    else
        warn "R2: controller pod not found — log assertion skipped"
    fi
fi

# -----------------------------------------------------------------------------
if (( failures > 0 )); then
    die "${failures} row(s) failed"
fi
ok "all rows passed"
