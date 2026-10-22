#!/usr/bin/env bash
# Issue #1812 — the trigger INLINE GATE SCRIPT, exercised against a real
# workspace pod through the platform API. The gate script runs as an
# agentd script node in the routine's target workspace before every
# fire's LLM call; rows pin the three gate paths with a zero-sessions
# assertion as the no-LLM-call evidence (a spawned agent turn would
# leave a session behind).
#
#   R0 — create-path validation (no execution; mode-independent):
#        R0a script+workflowId → 400 naming script/routine;
#        R0b unsupported language → 400 naming python, node;
#        R0c partial script → 400 naming the missing field.
#   R1 — false-gate: a script-only trigger whose handler returns
#        proceed=false records a SKIPPED fire carrying the gate payload
#        {gate:{proceed:false,vars}} — and ZERO agent sessions.
#   R2 — true-gate (script-only): proceed=true + vars records a
#        DELIVERED fire with {gate:{proceed:true,vars}} plus a
#        workspace side-effect marker (the script ran IN the workspace);
#        still ZERO sessions (script-only triggers have no LLM leg).
#   R3 — script failure fails CLOSED: a raising handler records a
#        FAILED fire with the named error in gate.error — ZERO sessions.
#
# SCOPE NOTE (r2 review): the open-gate-WITH-PROMPT path ({{key}}
# substitution into a real agent leg) has NO row here BY DESIGN: this
# harness is API-only and runs without LLM_MODEL, and that path
# consumes a model call on exec-mode clusters. It is pinned at engine
# level instead — TestScheduler_RoutineGateOpenSubstitutesVars captures
# the exact dispatched agent spec (sentSpecs) and asserts the
# substituted values, the literal unmatched tokens, and the trailing
# metadata block. R4 likewise pins create-compat only, not a
# prompt-only fire, for the same budget reason.
#
# ADAPTIVE ARMS (#1455 precedent): the nightly installs
# controller.agentdSidecar.enabled=true, and in sidecar mode agentd
# serves script nodes from a scratch container without /tmp or
# interpreters — script dispatch there fails LOUD with
# script_env_unavailable. Exec-mode clusters prove the gate outcomes;
# sidecar-mode clusters prove the SAME fail-closed contract + the
# no-LLM-call invariant (every execution arm ends failed/skipped with
# zero sessions). Any other terminal shape fails the row.
#
# Environment: same conventions as local/issue-1455-scriptenv-e2e.sh
# (local/lib/us70-common.sh). Runs on the pool/nightly kind cluster.
# No LLM_MODEL required — script-only triggers never spawn agent legs.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/us70-common.sh
source "${SCRIPT_DIR}/lib/us70-common.sh"

harness_start

# Per-script workspace isolation (the 1455 r21 pattern: the lib's own
# WS_BASE default must never silently apply).
WS_BASE="e2e1812gat-0000-4000-8000-000000000000"
WS="$(ws_id 1)"
FIRE_WAIT_S="${FIRE_WAIT_S:-210}"
failures=0
note_fail() { failures=$((failures + 1)); warn "FAIL: $*"; }
created_triggers=()

cleanup() {
    # ${API_KEY:-}: the EXIT trap can fire before harness_start seeds it.
    local id
    for id in ${created_triggers[*]:}; do
        curl -sfm 10 -X DELETE -H "Authorization: Bearer ${API_KEY:-}" \
            "http://127.0.0.1:${PORTFWD_PORT}/api/v1/me/triggers/${id}" >/dev/null 2>&1 || true
    done
    kc delete workspace "${WS}" --ignore-not-found >/dev/null 2>&1 || true
}
trap cleanup EXIT

api() { # method path [body] — sets api_status/api_body (the 1455 shape)
    local method="$1" path="$2" body="${3:-}"
    local args=(-sS -m 30 -X "${method}"
        -H "Authorization: Bearer ${API_KEY}" -H "Content-Type: application/json"
        -w $'\n%{http_code}'
        "http://127.0.0.1:${PORTFWD_PORT}${path}")
    local out
    if [[ -n "${body}" ]]; then
        out=$(curl "${args[@]}" -d "${body}") || out=$'\n000'
    else
        out=$(curl "${args[@]}") || out=$'\n000'
    fi
    api_status="${out##*$'\n'}"
    api_body="${out%$'\n'*}"
    printf '%s' "${api_body}"
}

# wait_fire <trigger-id> <timeout_s> — poll until the trigger has ≥1
# terminal (non-fired) fire row; sets fire_row/fire_status.
wait_fire() {
    local tid="$1" timeout_s="$2" i fires_json
    fire_row=""
    fire_status=""
    for ((i = 0; i < timeout_s; i += 6)); do
        api GET "/api/v1/me/triggers/${tid}/fires"
        fires_json="${api_body}"
        # Fires list newest-first; find the first row past "fired".
        fire_status=$(printf '%s' "${fires_json}" | jq -r '[.fires[] | select(.status != "fired")][0].status // empty')
        if [[ -n "${fire_status}" ]]; then
            fire_row=$(printf '%s' "${fires_json}" | jq -c '[.fires[] | select(.status != "fired")][0]')
            return 0
        fi
        sleep 6
    done
    return 1
}

# assert_no_sessions <label> — CORROBORATING no-LLM evidence (r2
# review note: not proof — an agent leg with preserveSession:never
# deletes its ephemeral session, so an empty list does not exclude an
# LLM call on its own). The engine dispatch-counting pins
# (api/internal/workflows/engine_gate_test.go — zero "routine-agent"
# dispatches per path) are the primary no-LLM proof; this row pins the
# observable end-state a caller sees.
assert_no_sessions() {
    api GET "/api/v1/workspaces/${WS}/sessions"
    if [[ "${api_status}" != "200" ]]; then
        note_fail "$1: sessions list unavailable (${api_status})"
        return
    fi
    local n
    n=$(printf '%s' "${api_body}" | jq '.sessions | length')
    if [[ "${n}" != "0" ]]; then
        note_fail "$1: expected ZERO agent sessions (no LLM call), found ${n}"
    else
        ok "$1: zero agent sessions recorded (corroborates the engine dispatch pins)"
    fi
}

# gate_action <field-path-in-actionResult> — extract from fire_row.
# NOTE: jq's `// empty` swallows FALSE (false is falsy in //), so
# `gate_field proceed` would return "" for a closed gate — the r2
# review's Finding 1. `tostring` renders false as "false" and missing
# paths as "null"; comparisons account for both.
gate_field() { printf '%s' "${fire_row}" | jq -r ".actionResult.gate.${1} | tostring"; }

# marker_read — 1 if the side-effect file exists in the workspace pod.
marker_present() {
    local pod rc
    pod=$(pod_of "${WS}")
    [[ -n "${pod}" ]] || return 1
    rc=$(runtime_container "${pod}")
    kc exec "${pod}" ${rc:+-c "$rc"} -- cat /workspace/issue-1812-marker >/dev/null 2>&1
}

log "R0 — workspace ${WS} up"
seed_workspace "${WS}"
wait_phase "${WS}" Active 360 || die "R0: workspace never Active"
ok "workspace Active"

# -----------------------------------------------------------------------------
log "R0v — create-path validation (self-teaching 400s)"

api POST /api/v1/me/triggers "$(jq -nc \
    --arg w "${WS}" \
    '{name:"e2e-1812-v-wf", sourceType:"cron", sourceConfig:{expr:"0 2 * * *", tz:"UTC"},
      workflowId:"00000000-0000-4000-8000-0000000000ff",
      script:{language:"python", handler:"def handler(input):\n    return {\"proceed\": True}\n"}}')"
if [[ "${api_status}" == "400" && "${api_body}" == *script* && "${api_body}" == *routine* ]]; then
    ok "R0a: script + workflowId rejected naming script/routine"
else
    note_fail "R0a: expected 400 naming script+routine, got ${api_status}: ${api_body:0:200}"
fi

api POST /api/v1/me/triggers "$(jq -nc \
    --arg w "${WS}" \
    '{name:"e2e-1812-v-lang", sourceType:"cron", sourceConfig:{expr:"0 2 * * *", tz:"UTC"},
      workspaceId:$w, prompt:"p",
      script:{language:"bash", handler:"echo hi"}}')"
if [[ "${api_status}" == "400" && "${api_body}" == *python* && "${api_body}" == *node* ]]; then
    ok "R0b: unsupported language rejected naming python, node"
else
    note_fail "R0b: expected 400 naming the valid languages, got ${api_status}: ${api_body:0:200}"
fi

api POST /api/v1/me/triggers "$(jq -nc \
    --arg w "${WS}" \
    '{name:"e2e-1812-v-half", sourceType:"cron", sourceConfig:{expr:"0 2 * * *", tz:"UTC"},
      workspaceId:$w, prompt:"p",
      script:{language:"python"}}')"
if [[ "${api_status}" == "400" && "${api_body}" == *handler* ]]; then
    ok "R0c: partial script rejected naming handler"
else
    note_fail "R0c: expected 400 naming handler, got ${api_status}: ${api_body:0:200}"
fi

# -----------------------------------------------------------------------------
log "R1 — false-gate: proceed=false exits with NO LLM call"

R1_ID=""
api POST /api/v1/me/triggers "$(jq -nc --arg w "${WS}" \
    '{name:"e2e-1812-false", sourceType:"cron", sourceConfig:{expr:"* * * * *", tz:"UTC"},
      workspaceId:$w,
      script:{language:"python",
              handler:"def handler(input):\n    return {\"proceed\": False, \"vars\": {\"topic\": \"nothing-new\"}}\n"}}')"
[[ "${api_status}" == "201" ]] || die "R1: trigger create failed: ${api_status} ${api_body:0:300}"
R1_ID=$(printf '%s' "${api_body}" | jq -r '.id')
created_triggers+=("${R1_ID}")

if wait_fire "${R1_ID}" "${FIRE_WAIT_S}"; then
    if [[ "${fire_status}" == "skipped" && "$(gate_field proceed)" == "false" && "$(gate_field vars.topic)" == "nothing-new" ]]; then
        ok "R1 (exec mode): skipped fire with {gate:{proceed:false,vars}} in the audit"
    elif [[ "${fire_status}" == "failed" && "$(gate_field error)" == *"script_env_unavailable"* ]]; then
        ok "R1 (sidecar mode): fail-closed with the named script_env_unavailable cause"
    else
        note_fail "R1: terminal shape outside the gate contract (status=${fire_status}): ${fire_row:0:300}"
    fi
    assert_no_sessions "R1"
else
    note_fail "R1: no terminal fire within ${FIRE_WAIT_S}s"
fi

# -----------------------------------------------------------------------------
log "R2 — true-gate script-only: proceed=true runs side effects, NO LLM leg"

R2_ID=""
api POST /api/v1/me/triggers "$(jq -nc --arg w "${WS}" \
    '{name:"e2e-1812-true", sourceType:"cron", sourceConfig:{expr:"* * * * *", tz:"UTC"},
      workspaceId:$w,
      script:{language:"python",
              handler:"def handler(input):\n    open(\"/workspace/issue-1812-marker\", \"w\").write(\"ran\")\n    return {\"proceed\": True, \"vars\": {\"count\": \"3\"}}\n"}}')"
[[ "${api_status}" == "201" ]] || die "R2: trigger create failed: ${api_status} ${api_body:0:300}"
R2_ID=$(printf '%s' "${api_body}" | jq -r '.id')
created_triggers+=("${R2_ID}")

if wait_fire "${R2_ID}" "${FIRE_WAIT_S}"; then
    if [[ "${fire_status}" == "delivered" && "$(gate_field proceed)" == "true" && "$(gate_field vars.count)" == "3" ]]; then
        ok "R2 (exec mode): delivered fire with {gate:{proceed:true,vars}}"
        if marker_present; then
            ok "R2 (exec mode): side-effect marker present in the workspace — the script ran IN the sandbox"
        else
            note_fail "R2 (exec mode): gate reported proceed=true but the side-effect marker is missing"
        fi
    elif [[ "${fire_status}" == "failed" && "$(gate_field error)" == *"script_env_unavailable"* ]]; then
        ok "R2 (sidecar mode): fail-closed with the named script_env_unavailable cause"
    else
        note_fail "R2: terminal shape outside the gate contract (status=${fire_status}): ${fire_row:0:300}"
    fi
    assert_no_sessions "R2"
else
    note_fail "R2: no terminal fire within ${FIRE_WAIT_S}s"
fi

# -----------------------------------------------------------------------------
log "R3 — script failure fails closed"

R3_ID=""
api POST /api/v1/me/triggers "$(jq -nc --arg w "${WS}" \
    '{name:"e2e-1812-err", sourceType:"cron", sourceConfig:{expr:"* * * * *", tz:"UTC"},
      workspaceId:$w,
      script:{language:"python",
              handler:"def handler(input):\n    return 1 / 0\n"}}')"
[[ "${api_status}" == "201" ]] || die "R3: trigger create failed: ${api_status} ${api_body:0:300}"
R3_ID=$(printf '%s' "${api_body}" | jq -r '.id')
created_triggers+=("${R3_ID}")

if wait_fire "${R3_ID}" "${FIRE_WAIT_S}"; then
    gate_err="$(gate_field error)"
    if [[ "${fire_status}" == "failed" && ( "${gate_err}" == *"script_failed"* || "${gate_err}" == *"script_env_unavailable"* ) ]]; then
        ok "R3: fail-closed failed fire with the named gate error (${gate_err:0:80})"
    elif [[ "${fire_status}" == "failed" && -n "${gate_err}" ]]; then
        note_fail "R3: failed (good) but the error is not a recognized gate cause: ${gate_err:0:200}"
    else
        note_fail "R3: terminal shape outside the fail-closed contract (status=${fire_status}): ${fire_row:0:300}"
    fi
    assert_no_sessions "R3"
else
    note_fail "R3: no terminal fire within ${FIRE_WAIT_S}s"
fi

# -----------------------------------------------------------------------------
log "R4 — prompt-only trigger unchanged (backward compat)"

R4_ID=""
api POST /api/v1/me/triggers "$(jq -nc --arg w "${WS}" \
    '{name:"e2e-1812-legacy", sourceType:"cron", sourceConfig:{expr:"0 3 * * *", tz:"UTC"},
      workspaceId:$w, prompt:"legacy prompt-only trigger"}')"
if [[ "${api_status}" == "201" && "$(printf '%s' "${api_body}" | jq -r '.script // empty')" == "" ]]; then
    ok "R4: prompt-only create still works, no script in the response"
    R4_ID=$(printf '%s' "${api_body}" | jq -r '.id')
    created_triggers+=("${R4_ID}")
else
    note_fail "R4: prompt-only create regressed: ${api_status} ${api_body:0:200}"
fi

# -----------------------------------------------------------------------------
if [[ "${failures}" -gt 0 ]]; then
    die "${failures} row(s) failed"
fi
log "all rows passed"
