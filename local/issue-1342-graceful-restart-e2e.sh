#!/usr/bin/env bash
# Issue #1342 — graceful restart interrupt + orphan sweep + progress-keyed
# defer: the 2026-09-11 incident replay at cluster scale.
#
#   R1 — the incident replay (happy path): a long-running tool command
#        STREAMING output while a credential change lands must NEVER be
#        force-killed (the 40-min build rule); the apply rides the
#        maintenance window (CredentialsApplyPending on the CR), and
#        when the stream goes silent past the stall bound the
#        interrupt-first force path applies the credential and the
#        session's history shows the tool terminal — no eternal spinner.
#   R2 — the kill-9 backstop (unhappy path): a harness killed mid-tool
#        (the orphaned-running-part class) is repaired: the
#        generation-change reseed sweeps the projection and the history
#        read closes the part as aborted with reason "harness restart".
#
# Environment: same conventions as local/us-70-secret-delivery-e2e.sh
# (see local/lib/us70-common.sh). Runs on the pool/nightly kind cluster.
#   LLM_MODEL      - provider/model the turn runs on (required; the
#                    cluster's free-tier relay model, e.g. the us-63
#                    suite's litellm default)
#   R1_STREAM_S    - seconds the R1 tool streams output (default 40)
#   R1_SLEEP_S     - seconds the R1 tool sleeps silently after streaming
#                    (default 300; the stall bound is 30s)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/us70-common.sh
source "${SCRIPT_DIR}/lib/us70-common.sh"

[[ -n "${LLM_MODEL:-}" ]] || die "LLM_MODEL must name the model turns run on (e.g. litellm/<free-model>)"
R1_STREAM_S="${R1_STREAM_S:-40}"
R1_SLEEP_S="${R1_SLEEP_S:-300}"

WS_BASE="${WS_BASE:-e2e134200-0000-4000-8000-000000000000}"
WS="$(ws_id 1)"
failures=0
note_fail() { failures=$((failures + 1)); warn "FAIL: $*"; }

# --- shared row helpers -----------------------------------------------------

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

# running_tool_parts <sid> -> count of tool parts whose state is "running"
# (the eternal-spinner shape) in the session's served history.
running_tool_parts() {
    get_history "$1" | jq '[.[].parts[]? | select(.tool != null and .tool.state.status == "running")] | length'
}

# harness_restarted_parts <sid> -> count of tool parts closed by the
# platform's repair with the synthetic reason.
harness_restarted_parts() {
    get_history "$1" | jq '[.[].parts[]? | select(.tool != null and .tool.state.error == "harness restart")] | length'
}

opencode_pid() { # -> PID of the running opencode serve ("" when absent)
    kc exec "${POD}" -- sh -c 'pgrep -f "opencode serve" | head -1' 2>/dev/null || echo ""
}

pending_apply_condition() { # -> "True" | "" (the CR condition)
    kc get workspace "${WS}" -o json \
        | jq -r '.status.conditions[]? | select(.type == "CredentialsApplyPending") | .status' 2>/dev/null | head -1 || echo ""
}

# wait_for <desc> <timeout_s> <predicate-string> — eval'd in THIS shell
# (the row helpers must be visible) until true or budget out.
wait_for() {
    local desc="$1" timeout="$2" pred="$3"
    local deadline=$(( $(date +%s) + timeout ))
    until eval "${pred}" >/dev/null 2>&1; do
        if [[ $(date +%s) -ge ${deadline} ]]; then
            warn "timeout (${timeout}s): ${desc}"
            return 1
        fi
        sleep 3
    done
    return 0
}

log "R0 — workspace ${WS} up"
seed_workspace "${WS}"
wait_phase "${WS}" Active 360 || die "R0: workspace never Active"
POD=$(pod_of "${WS}")
[[ -n "${POD}" ]] || die "R0: no pod on CR"
ok "workspace Active, pod ${POD}"

# -----------------------------------------------------------------------------
log "R1 — incident replay: credential change during a streaming tool run"

SID1=$(create_session)
TURN_PID=""
if [[ -z "${SID1}" ]]; then
    note_fail "R1: no session created"
else
    # A long turn: the tool streams output for R1_STREAM_S then goes
    # silent (sleep) — streaming = progressing, silence = stalled.
    send_message "${SID1}" "Run this exact bash command and do nothing else: for i in \$(seq 1 ${R1_STREAM_S}); do echo tick \$i; sleep 1; done; sleep ${R1_SLEEP_S}" >/dev/null &
    TURN_PID=$!

    PID_BEFORE=""
    if wait_for "R1: tool part running" 120 '[[ $(running_tool_parts '"${SID1}"') -ge 1 ]]'; then
        PID_BEFORE=$(opencode_pid)
    else
        note_fail "R1: tool never reached running state"
    fi

    if [[ -n "${PID_BEFORE}" ]]; then
        # The credential change lands MID-TURN (the incident's trigger).
        bind_env "${WS}" "SD_1342_R1" "r1-value"
        ok "credential change delivered mid-turn"

        # While the tool streams: NO restart, and the defer surfaces.
        sleep 15
        if [[ "$(opencode_pid)" == "${PID_BEFORE}" ]]; then
            ok "R1: streaming turn NOT force-killed by the credential change"
        else
            note_fail "R1: opencode restarted while the turn was streaming (the 40-min build rule)"
        fi
        if [[ "$(pending_apply_condition)" == "True" ]]; then
            ok "R1: CredentialsApplyPending surfaced on the CR"
        else
            note_fail "R1: CredentialsApplyPending never surfaced (operator blind to the defer)"
        fi

        # The stream ends; silence crosses the 30s stall bound → the
        # interrupt-first force path applies the credential.
        log "R1: waiting for stall → interrupt → restart (stream ${R1_STREAM_S}s + stall 30s + grace 5s)"
        if wait_for "R1: credential-apply restart fired" $((R1_STREAM_S + 150)) \
            '[[ "$(opencode_pid)" != "'"${PID_BEFORE}"'" ]]'; then
            ok "R1: deferred credential applied via restart"
        else
            note_fail "R1: the deferred credential never applied (no restart observed)"
        fi
        if [[ "$(pending_apply_condition)" != "True" ]]; then
            ok "R1: pending condition cleared after the apply"
        else
            note_fail "R1: CredentialsApplyPending still True after the restart applied"
        fi

        # No eternal spinner: the interrupted tool renders terminal.
        if wait_for "R1: orphaned tool terminal in history" 90 \
            '[[ $(running_tool_parts '"${SID1}"') -eq 0 ]]'; then
            ok "R1: history renders no running tool part (no eternal spinner)"
        else
            note_fail "R1: history still renders a running tool part (eternal spinner)"
        fi
        if [[ "$(harness_restarted_parts "${SID1}")" -ge 1 ]]; then
            ok "R1: history closes the orphaned tool with the harness-restart reason"
        else
            warn "R1: tool terminal but not via the harness-restart reason (the harness may have written terminal state at the interrupt — acceptable)"
        fi
    fi
fi
[[ -n "${TURN_PID}" ]] && kill "${TURN_PID}" 2>/dev/null || true

# -----------------------------------------------------------------------------
log "R2 — kill -9 mid-tool: the orphan sweep repairs the transcript"

SID2=$(create_session)
TURN2_PID=""
if [[ -z "${SID2}" ]]; then
    note_fail "R2: no session created"
else
    send_message "${SID2}" "Run this exact bash command and do nothing else: echo started; sleep 600" >/dev/null &
    TURN2_PID=$!

    if wait_for "R2: tool part running" 120 '[[ $(running_tool_parts '"${SID2}"') -ge 1 ]]'; then
        # Kill the harness mid-tool — the orphaning event (uncontrolled
        # pod death, OOM, node drain: the 2026-09-11 residue class).
        kc exec "${POD}" -- sh -c 'kill -9 $(pgrep -f "opencode serve")' 2>/dev/null || true
        ok "harness killed mid-tool (kill -9)"

        # agentd (the parent) respawns the child: a new generation.
        if wait_for "R2: opencode respawned" 90 '[[ -n "$(opencode_pid)" ]]'; then
            ok "R2: supervisor respawned opencode"
        else
            note_fail "R2: supervisor never respawned opencode"
        fi

        # The orphan sweep + transcript repair make the history honest.
        if wait_for "R2: orphaned tool repaired in history" 90 \
            '[[ $(running_tool_parts '"${SID2}"') -eq 0 ]]'; then
            ok "R2: history renders no running tool part"
        else
            note_fail "R2: history still renders the killed tool as running (eternal spinner survived the sweep)"
        fi
        if [[ "$(harness_restarted_parts "${SID2}")" -ge 1 ]]; then
            ok "R2: tool part closed with reason 'harness restart'"
        else
            note_fail "R2: tool terminal but WITHOUT the harness-restart reason (repair path did not run)"
        fi

        # The sweep is observable on the pod's agentd metrics.
        SWEEPS=$(kc exec "${POD}" -- sh -c \
            'wget -qO- http://127.0.0.1:4098/metrics 2>/dev/null | grep "^llmsafespaces_orphan_parts_aborted_total" | grep -v "^#"' \
            2>/dev/null | tail -1 | awk '{print $2}')
        if [[ -n "${SWEEPS:-}" && "${SWEEPS}" != "0" ]]; then
            ok "R2: orphan sweep metric observed (${SWEEPS})"
        else
            note_fail "R2: llmsafespaces_orphan_parts_aborted_total missing or zero on the pod"
        fi
    else
        note_fail "R2: tool never reached running state"
    fi
fi
[[ -n "${TURN2_PID}" ]] && kill "${TURN2_PID}" 2>/dev/null || true

# -----------------------------------------------------------------------------
if [[ ${failures} -eq 0 ]]; then
    ok "issue-1342 e2e: all rows green"
else
    warn "issue-1342 e2e: ${failures} failure(s)"
    exit 1
fi
