#!/usr/bin/env bash
# Issue #1452 — preserved routine sessions must be discoverable in the
# platform session list. The routine fire path goes engine → agentd
# directly and never rides the adapter routes that populate the
# session_index; before the fix, a PreserveAlways session was real
# pod-side (store row + transcript + live GET /session entry) but
# GET /workspaces/:id/sessions — which serves ONLY the index — never
# learned it existed.
#
#   R1 — happy path: a webhook routine trigger with preserveSession
#        "always" fires (signed HMAC delivery, deterministic — no cron
#        tick wait); the delivered fire's result carries a session_id;
#        GET /workspaces/:id/sessions contains that session with the
#        trigger's name as title.
#   R2 — negative: a preserveSession "never" routine fires and delivers;
#        its ephemeral session is deleted agent-side, and the platform
#        session list gains NO row for it.
#
# Environment: same conventions as local/issue-1342-graceful-restart-e2e.sh
# (see local/lib/us70-common.sh). Runs on the pool/nightly kind cluster.
# No LLM_MODEL required — the routine agent node resolves the workspace
# default model (the cluster's free-tier relay default).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/us70-common.sh
source "${SCRIPT_DIR}/lib/us70-common.sh"

WS_BASE="e2e14520-0000-4000-8000-000000000000"
WS="$(ws_id 1)"
FIRE_WAIT_S="${FIRE_WAIT_S:-300}"
failures=0
note_fail() { failures=$((failures + 1)); warn "FAIL: $*"; }
created_triggers=()

cleanup() {
    local id
    for id in ${created_triggers[*]:-}; do
        curl -sfm 10 -X DELETE -H "Authorization: Bearer ${API_KEY}" \
            "http://127.0.0.1:${PORTFWD_PORT}/api/v1/me/triggers/${id}" >/dev/null 2>&1 || true
    done
}
trap cleanup EXIT

# harness_start FIRST — it establishes the API port-forward and then
# livez-gates internally (us70-common.sh). A standalone livez pre-check
# before it died forwardless in the nightly (nothing else forwards
# 18087 in that step); 1417's pattern is harness_start as the first
# statement. It also seeds the session user + API key and sets OWNER_ID
# — without it seed_workspace dies (OWNER_ID is blanked at source time;
# the script had never been executable end to end, #1474 r4's class).
harness_start

api() { # method path [body] -> body on stdout; api_status + api_body globals
    # No-subshell contract: capture-style callers must use
    # `api M P B; var="${api_body}"` — `var=$(api ...)` runs in a
    # subshell and the status side-channel dies with it (set -u then
    # aborts on the stale variable; see the 1410 harness fix, #1474 r4).
    local method="$1" path="$2" body="${3:-}"
    local args=(-s -m 20 -X "${method}" -H "Authorization: Bearer ${API_KEY}" \
        -H "Content-Type: application/json" -w '\n%{http_code}' \
        "http://127.0.0.1:${PORTFWD_PORT}${path}")
    [[ -n "${body}" ]] && args+=(-d "${body}")
    local out
    out=$(curl "${args[@]}") || out=$'\n000'
    api_status="${out##*$'\n'}"
    api_body="${out%$'\n'*}"
    printf '%s' "${api_body}"
}

sessions_json() { # -> the platform session list for the row workspace
    curl -sfm 20 -H "Authorization: Bearer ${API_KEY}" \
        "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WS}/sessions" \
        2>/dev/null || echo '[]'
}

# fire_session_id <trigger-id> -> session_id of the first delivered fire
# ("" while none delivered or capture carried no session_id).
fire_session_id() {
    api GET "/api/v1/me/triggers/${1}/fires" \
        | jq -r '[.fires[] | select(.status=="delivered")] | first | .result.session_id // empty'
}

wait_for() { # <desc> <timeout_s> <predicate-string> — eval'd until true
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

# signed_fire <webhook-url> <secret> <body> -> http code
signed_fire() {
    local url="$1" secret="$2" body="$3"
    curl -s -m 20 -o /dev/null -w '%{http_code}' -X POST \
        -H "Content-Type: application/json" \
        -H "X-Hub-Signature-256: sha256=$(printf '%s' "${body}" \
            | openssl dgst -sha256 -hmac "${secret}" | awk '{print $NF}')" \
        -d "${body}" "${url}"
}

# make_routine_trigger <name> <preserve> -> trigger id (echoed), 201 enforced
make_routine_trigger() {
    local name="$1" preserve="$2" resp
    api POST /api/v1/me/triggers "$(jq -nc --arg n "${name}" --arg p "${preserve}" --arg w "${WS}" \
        '{name:$n,sourceType:"webhook",sourceConfig:{},workspaceId:$w,prompt:"Reply with exactly the single word: indexed",captureMode:"full",preserveSession:$p}')" >/dev/null
    resp="${api_body}"
    [[ "${api_status}" == "201" ]] || die "routine trigger create (${name}) failed: ${api_status} ${resp}"
    created_triggers+=("$(printf '%s' "${resp}" | jq -r '.id')")
    printf '%s' "${resp}" | jq -r '.id'
}

log "R0 — workspace ${WS} up"
seed_workspace "${WS}"
wait_phase "${WS}" Active 360 || die "R0: workspace never Active"
ok "workspace Active"

# -----------------------------------------------------------------------------
log "R1 — PreserveAlways routine fire surfaces in the platform session list"

R1_ID=$(make_routine_trigger "e2e-1452-preserve-always" "always")
api POST "/api/v1/me/triggers/${R1_ID}/rotate-secret" >/dev/null
r1_rot="${api_body}"
R1_SECRET=$(printf '%s' "${r1_rot}" | jq -r '.webhookSecret // empty')
R1_HOOK_URL="http://127.0.0.1:${PORTFWD_PORT}$(printf '%s' "${r1_rot}" | jq -r '.webhookUrl // empty')"
[[ -n "${R1_SECRET}" && "${R1_HOOK_URL}" != "http://127.0.0.1:${PORTFWD_PORT}" ]] \
    || die "R1: rotate-secret failed: ${api_status} ${r1_rot}"

r1_code=$(signed_fire "${R1_HOOK_URL}" "${R1_SECRET}" '{"e2e":"1452-r1"}')
if [[ "${r1_code}" == "202" ]]; then
    ok "R1a: signed webhook delivery accepted (202)"
else
    note_fail "R1a: signed webhook delivery returned ${r1_code}, expected 202"
fi

if wait_for "R1b: delivered fire with captured session_id" "${FIRE_WAIT_S}" \
    '[[ -n "$(fire_session_id "${R1_ID}")" ]]'; then
    R1_SID=$(fire_session_id "${R1_ID}")
    ok "R1b: fire delivered, preserved session ${R1_SID}"
else
    note_fail "R1b: no delivered fire with a session_id within ${FIRE_WAIT_S}s"
    R1_SID=""
fi

if [[ -n "${R1_SID}" ]]; then
    # THE bug's acceptance row: the preserved session must be in the
    # platform list (index-fed), not just in the agent's own list.
    # jq failures degrade to empty/note_fail rows, never script aborts.
    r1_listed=$(sessions_json | jq -r --arg s "${R1_SID}" \
        '[.[] | select(.id == $s)] | length' 2>/dev/null || echo 0)
    if [[ "${r1_listed}" == "1" ]]; then
        ok "R1c: preserved routine session appears in GET /workspaces/:id/sessions"
    else
        note_fail "R1c: session ${R1_SID} absent from the platform session list"
    fi
    r1_title=$(sessions_json | jq -r --arg s "${R1_SID}" \
        'first(.[] | select(.id == $s) | .title) // empty' 2>/dev/null || echo '')
    if [[ "${r1_title}" == "e2e-1452-preserve-always" ]]; then
        ok "R1d: indexed row carries the trigger name as title"
    else
        note_fail "R1d: indexed title '${r1_title}' != trigger name 'e2e-1452-preserve-always'"
    fi
fi

# -----------------------------------------------------------------------------
log "R2 — PreserveNever routine fire adds no platform session row"

r2_before=$(sessions_json | jq 'length')
R2_ID=$(make_routine_trigger "e2e-1452-preserve-never" "never")
api POST "/api/v1/me/triggers/${R2_ID}/rotate-secret" >/dev/null
r2_rot="${api_body}"
R2_SECRET=$(printf '%s' "${r2_rot}" | jq -r '.webhookSecret // empty')
R2_HOOK_URL="http://127.0.0.1:${PORTFWD_PORT}$(printf '%s' "${r2_rot}" | jq -r '.webhookUrl // empty')"
[[ -n "${R2_SECRET}" && "${R2_HOOK_URL}" != "http://127.0.0.1:${PORTFWD_PORT}" ]] \
    || die "R2: rotate-secret failed: ${api_status} ${r2_rot}"

r2_code=$(signed_fire "${R2_HOOK_URL}" "${R2_SECRET}" '{"e2e":"1452-r2"}')
if [[ "${r2_code}" == "202" ]]; then
    ok "R2a: signed webhook delivery accepted (202)"
else
    note_fail "R2a: signed webhook delivery returned ${r2_code}, expected 202"
fi

if wait_for "R2b: delivered fire" "${FIRE_WAIT_S}" \
    '[[ -n "$(api GET "/api/v1/me/triggers/${R2_ID}/fires" | jq -r "[.fires[] | select(.status==\"delivered\")] | first | .id // empty")" ]]'; then
    ok "R2b: PreserveNever fire delivered"
else
    note_fail "R2b: no delivered fire within ${FIRE_WAIT_S}s"
fi

# Give a stray index write (the bug this row guards against) a beat to
# land, then assert the list grew by nothing.
sleep 5
r2_after=$(sessions_json | jq 'length')
r2_session=$(api GET "/api/v1/me/triggers/${R2_ID}/fires" \
    | jq -r '[.fires[] | select(.status=="delivered")] | first | .result.session_id // empty' 2>/dev/null || echo '')
if [[ "${r2_after}" -eq "${r2_before}" ]]; then
    ok "R2c: ephemeral routine session (id '${r2_session:-<deleted>}') left no platform row"
else
    note_fail "R2c: session list grew ${r2_before} -> ${r2_after} for a PreserveNever fire"
fi

# -----------------------------------------------------------------------------
if [[ ${failures} -eq 0 ]]; then
    ok "issue-1452 e2e: all rows passed"
else
    warn "issue-1452 e2e: ${failures} row(s) failed"
    exit 1
fi
