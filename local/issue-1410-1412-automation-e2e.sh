#!/usr/bin/env bash
# Issues #1410/#1411/#1412/#1413 — automation trigger + workflow-run e2e
# rows for the defects the 2026-09-17 live session proved broken. All rows
# are API-only (no LLM turns, no workspace pods): the cron/trigger surfaces
# under test resolve server-side, and the workflow-targeted triggers point
# at a deliberately nonexistent workflow so no DAG ever executes.
#
#   R1 — create validation (#1411): invalid cron expr is rejected with 400
#        (never stored), and a valid create's nextFireAt is the first real
#        occurrence of the schedule — NOT creation time (the old
#        fire-immediately defect).
#   R2 — reschedule takes effect immediately (#1410): patching the expr
#        moves nextFireAt to the NEW schedule's next slot at once (the old
#        code kept firing at the OLD slot).
#   R3 — no-op enable keeps the slot (#1410 review guard): enabled:true on
#        an already-enabled trigger never reschedules an imminent fire.
#   R4 — missing-workflow fires are loud (#1412): a workflow-targeted
#        trigger whose DAG does not exist records a FAILED fire with a
#        workflow-not-found payload and drives consecutiveFailures — the
#        old code ticked silently forever.
#   R5 — run input obeys inputSchema (#1413): a workflow with a required
#        field rejects non-conforming run input with 400 BEFORE queueing;
#        conforming input progresses past schema validation.
#
# Environment: same conventions as local/issue-1342-graceful-restart-e2e.sh
# (see local/lib/us70-common.sh). Runs on the pool/nightly kind cluster.
# No LLM_MODEL required — no agent turns are executed.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/us70-common.sh
source "${SCRIPT_DIR}/lib/us70-common.sh"

GHOST_WF="deadbeef-0000-4000-8000-000000000000"
R4_WAIT_S="${R4_WAIT_S:-150}"
failures=0
note_fail() { failures=$((failures + 1)); warn "FAIL: $*"; }
created_triggers=()
created_workflows=()

cleanup() {
    local id
    for id in ${created_triggers[*]:-}; do
        curl -sfm 10 -X DELETE -H "Authorization: Bearer ${API_KEY}" \
            "http://127.0.0.1:${PORTFWD_PORT}/api/v1/me/triggers/${id}" >/dev/null 2>&1 || true
    done
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

create_trigger() { # name expr -> trigger id (echoed), 201 enforced
    local name="$1" expr="$2" body resp
    body=$(jq -nc --arg n "${name}" --arg e "${expr}" --arg w "${GHOST_WF}" \
        '{name:$n,sourceType:"cron",sourceConfig:{expr:$e,tz:"UTC"},workflowId:$w}')
    resp=$(api POST /api/v1/me/triggers "${body}")
    [[ "${api_status}" == "201" ]] || die "trigger create (${name}) failed: ${api_status} ${resp}"
    local id; id=$(printf '%s' "${resp}" | jq -r '.id')
    created_triggers+=("${id}")
    printf '%s' "${id}"
}

trigger_field() { # id field -> value
    api GET "/api/v1/me/triggers/$1" | jq -r ".$2 // empty"
}

slot_hm() { # rfc3339 -> "HH:MM" (UTC)
    date -u -d "$1" +%H:%M
}

# --- R1: create-path validation + first-occurrence slot (#1411) ----------

r1_resp=$(api POST /api/v1/me/triggers \
    '{"name":"e2e-bad-cron","sourceType":"cron","sourceConfig":{"expr":"not-a-cron","tz":"UTC"},"workflowId":"'"${GHOST_WF}"'"}')
if [[ "${api_status}" == "400" ]]; then ok "R1a: invalid cron expr rejected (400)"; else
    note_fail "R1a: invalid cron expr returned ${api_status}, expected 400 (${r1_resp})"
fi

R1_ID=$(create_trigger "e2e-first-slot" "0 3 1 * *")
r1_next=$(trigger_field "${R1_ID}" nextFireAt)
if [[ -n "${r1_next}" ]] && [[ "$(date -u -d "${r1_next}" +%s)" -ge "$(date -u +%s)" ]]; then
    ok "R1b: nextFireAt is a future scheduled occurrence (${r1_next})"
else
    note_fail "R1b: nextFireAt not a future slot: '${r1_next}'"
fi

# --- R2: reschedule moves the slot immediately (#1410) --------------------

R2_ID=$(create_trigger "e2e-reschedule" "0 3 1 * *")
r2_old=$(trigger_field "${R2_ID}" nextFireAt)
api PUT "/api/v1/me/triggers/${R2_ID}" \
    '{"sourceConfig":{"expr":"0 4 1 * *","tz":"UTC"}}' >/dev/null
r2_new=$(trigger_field "${R2_ID}" nextFireAt)
if [[ "$(slot_hm "${r2_old}")" == "03:00" ]] && [[ "$(slot_hm "${r2_new}")" == "04:00" ]]; then
    ok "R2: reschedule moved nextFireAt 03:00→04:00 immediately"
else
    note_fail "R2: reschedule slots wrong: old='$(slot_hm "${r2_old}")' new='$(slot_hm "${r2_new}")' (want 03:00→04:00 on the 1st)"
fi

# --- R3: no-op enable never reschedules (#1410 review guard) --------------

r3_before=$(trigger_field "${R2_ID}" nextFireAt)
api PUT "/api/v1/me/triggers/${R2_ID}" '{"enabled":true}' >/dev/null
r3_after=$(trigger_field "${R2_ID}" nextFireAt)
if [[ "${r3_before}" == "${r3_after}" ]]; then
    ok "R3: no-op enabled:true kept the slot (${r3_after})"
else
    note_fail "R3: no-op enable moved the slot: '${r3_before}' → '${r3_after}'"
fi

# --- R4: missing workflow → failed fire, failure counter (#1412) ----------

R4_ID=$(create_trigger "e2e-ghost-workflow" "* * * * *")
r4_failed=0
for _ in $(seq 1 75); do
    sleep 2
    if [[ "$(api GET "/api/v1/me/triggers/${R4_ID}/fires" | jq '[.fires[] | select(.status=="failed")] | length')" -ge 1 ]]; then
        r4_failed=1; break
    fi
done
if [[ "${r4_failed}" -ne 1 ]]; then
    note_fail "R4a: no failed fire within ${R4_WAIT_S}s for the ghost-workflow trigger"
else
    ok "R4a: failed fire recorded for missing workflow"
fi
r4_result=$(api GET "/api/v1/me/triggers/${R4_ID}/fires" \
    | jq -r '.fires[] | select(.status=="failed") | .actionResult // empty' | head -1)
if [[ "${r4_result}" == *"workflow not found"* ]]; then
    ok "R4b: failed fire carries workflow-not-found payload"
else
    note_fail "R4b: failed fire payload wrong: '${r4_result}'"
fi
r4_cf=$(trigger_field "${R4_ID}" consecutiveFailures)
if [[ "${r4_cf}" -ge 1 ]]; then
    ok "R4c: consecutiveFailures incremented (${r4_cf})"
else
    note_fail "R4c: consecutiveFailures not incremented: '${r4_cf}'"
fi

# --- R5: run input obeys inputSchema (#1413) ------------------------------

R5_BODY=$(jq -nc '{name:"e2e-schema-run",targetWorkspaceId:"00000000-0000-4000-8000-000000000001",
    inputSchema:{type:"object",required:["topic"],properties:{topic:{type:"string"}}},
    specYaml:"{\"nodes\":[{\"id\":\"n1\",\"type\":\"script\",\"data\":{\"language\":\"python\",\"handler\":\"def handler(input):\\n    return {}\"}}],\"edges\":[]}"}')
R5_RESP=$(api POST /api/v1/me/workflows "${R5_BODY}")
if [[ "${api_status}" != "201" ]]; then
    note_fail "R5 setup: workflow create failed: ${api_status} ${R5_RESP}"
else
    R5_ID=$(printf '%s' "${R5_RESP}" | jq -r '.id')
    created_workflows+=("${R5_ID}")

    api POST "/api/v1/me/workflows/${R5_ID}/runs" '{"input":{"wrong":true}}' >/dev/null
    if [[ "${api_status}" == "400" ]]; then
        ok "R5a: schema-violating run input rejected (400)"
    else
        note_fail "R5a: schema-violating input returned ${api_status}, expected 400"
    fi

    # Conforming input passes VALIDATION and fails later (dummy target
    # workspace) — any non-schema rejection proves validation let it through.
    r5b_resp=$(api POST "/api/v1/me/workflows/${R5_ID}/runs" '{"input":{"topic":"ship"}}')
    if [[ "${r5b_resp}" != *"inputSchema"* ]]; then
        ok "R5b: conforming input passed schema validation (status ${api_status}: $(printf '%s' "${r5b_resp}" | jq -r '.error // empty' | head -c 60))"
    else
        note_fail "R5b: conforming input rejected by schema validation: ${r5b_resp}"
    fi
fi

# --- verdict ---------------------------------------------------------------

if [[ "${failures}" -ne 0 ]]; then
    die "automation e2e: ${failures} row(s) failed"
fi
ok "automation e2e: all rows passed (R1-R5)"
