#!/usr/bin/env bash
# Issues #1410/#1411/#1412/#1413/#1425/#1419 — automation trigger +
# workflow-run e2e rows for the defects the 2026-09-17 live session proved
# broken. Rows are API-only (no LLM turns, no workspace pods): the
# cron/trigger surfaces under test resolve server-side. R1-R4 point at a
# deliberately nonexistent workflow; R5/R6 use a REAL schema-authored
# workflow whose targetWorkspaceId is the dummy workspace, so runs queue
# but no pod ever executes a DAG node.
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
#   R6 — trigger input mapping (#1425/#1419, design 0059): R6a — an
#        envelope-mode cron trigger wired to a schema-bearing workflow is
#        rejected with 400 at create; R6b — mapped-mode static input is
#        schema-validated at create ({} 400s, {topic} 201s); R6c — a
#        webhook trigger with inputFrom "body" makes the posted payload
#        the run input (top level), and a violating payload records a
#        validation_error fire (schema_mismatch, typed violations only)
#        with NO run — signed HMAC delivery on the rotated secret.
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

# R4d — the DELETE route (#1440): deleting an EXISTING target workflow
# SET NULLs the trigger's workflow_id (migration 000020 FK), so the
# scheduler sees a targetless row — which must ALSO fail loudly
# (trigger_has_no_target) and auto-disable, not tick silently.
R4D_WF=$(api POST /api/v1/me/workflows "$(jq -nc '{name:"e2e-r4d-target",
    specYaml:"{\"nodes\":[{\"id\":\"n\",\"type\":\"script\",\"data\":{\"language\":\"python\",\"handler\":\"def handler(input): return {}\"}}],\"edges\":[]}",
    targetWorkspaceId:"00000000-0000-0000-0000-000000000001"}')")
[[ "${api_status}" == "201" ]] || die "R4d setup: workflow create failed: ${api_status} ${R4D_WF}"
R4D_WF_ID=$(printf '%s' "${R4D_WF}" | jq -r '.id')
created_workflows+=("${R4D_WF_ID}")
R4D_BODY=$(jq -nc --arg w "${R4D_WF_ID}"   '{name:"e2e-r4d-ghost",sourceType:"cron",sourceConfig:{expr:"* * * * *",tz:"UTC"},workflowId:$w,autoDisableAfter:2}')
R4D_RESP=$(api POST /api/v1/me/triggers "${R4D_BODY}")
[[ "${api_status}" == "201" ]] || die "R4d setup: trigger create failed: ${api_status} ${R4D_RESP}"
R4D_ID=$(printf '%s' "${R4D_RESP}" | jq -r '.id')
created_triggers+=("${R4D_ID}")
api DELETE "/api/v1/me/workflows/${R4D_WF_ID}" >/dev/null   # FK SET NULL -> targetless
r4d_status=""
for ((i = 0; i < R4_WAIT_S; i += 10)); do
    r4d_status=$(trigger_field "${R4D_ID}" enabled)
    [[ "${r4d_status}" == "false" ]] && break
    sleep 10
done
if [[ "${r4d_status}" == "false" ]]; then
    ok "R4d: targetless trigger auto-disabled after workflow delete"
else
    note_fail "R4d: targetless trigger still enabled — silent zombie regression (#1440)"
fi
r4d_result=$(api GET "/api/v1/me/triggers/${R4D_ID}/fires"     | jq -r '.fires[] | select(.status=="failed") | .actionResult // empty' | head -1)
if [[ "${r4d_result}" == *"trigger_has_no_target"* ]]; then
    ok "R4d: failed fire carries the targetless payload"
else
    note_fail "R4d: targetless payload wrong: '${r4d_result}'"
fi

# --- R5: run input obeys inputSchema (#1413) ------------------------------

# R5_WS: the dummy workspace UUID rows below target (R5's workflow
# targetWorkspaceId, R8's routine workspaceId). It was previously
# referenced by R8 without ever being defined — set -u aborted the
# script there on every nightly run.
R5_WS="00000000-0000-4000-8000-000000000001"
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

# --- R6: trigger input mapping (#1425/#1419, design 0059) ------------------

# R6 reuses the R5 schema workflow (required `topic`); if R5's create
# failed, stand one up standalone so the input-mapping rows still run.
R6_WF="${R5_ID:-}"
if [[ -z "${R6_WF}" ]]; then
    r6_wf_resp=$(api POST /api/v1/me/workflows "${R5_BODY}")
    if [[ "${api_status}" == "201" ]]; then
        R6_WF=$(printf '%s' "${r6_wf_resp}" | jq -r '.id')
        created_workflows+=("${R6_WF}")
    fi
fi
if [[ -z "${R6_WF}" ]]; then
    note_fail "R6 setup: no schema workflow available (R5 create failed)"
else
    # R6a — wiring guard: envelope-mode cron wiring (no input, no
    # inputFrom) to a workflow whose schema requires non-envelope fields
    # is rejected with 400 at create.
    r6a_resp=$(api POST /api/v1/me/triggers "$(jq -nc --arg w "${R6_WF}" \
        '{name:"e2e-r6a-guard",sourceType:"cron",sourceConfig:{expr:"0 5 1 * *",tz:"UTC"},workflowId:$w}')")
    r6a_id=$(printf '%s' "${r6a_resp}" | jq -r '.id // empty')
    [[ -n "${r6a_id}" ]] && created_triggers+=("${r6a_id}")
    if [[ "${api_status}" == "400" ]]; then
        ok "R6a: envelope-mode wiring to required-topic schema rejected (400)"
    else
        note_fail "R6a: envelope wiring returned ${api_status}, expected 400 (${r6a_resp})"
    fi

    # R6b — mapped-mode static input is schema-validated at create.
    r6b_resp=$(api POST /api/v1/me/triggers "$(jq -nc --arg w "${R6_WF}" \
        '{name:"e2e-r6b-mapped-bad",sourceType:"cron",sourceConfig:{expr:"0 5 1 * *",tz:"UTC"},workflowId:$w,inputFrom:"mapped",input:{}}')")
    r6b_id=$(printf '%s' "${r6b_resp}" | jq -r '.id // empty')
    [[ -n "${r6b_id}" ]] && created_triggers+=("${r6b_id}")
    if [[ "${api_status}" == "400" ]]; then
        ok "R6b: mapped input {} rejected (400, missing topic)"
    else
        note_fail "R6b: mapped input {} returned ${api_status}, expected 400 (${r6b_resp})"
    fi
    r6b_resp=$(api POST /api/v1/me/triggers "$(jq -nc --arg w "${R6_WF}" \
        '{name:"e2e-r6b-mapped-ok",sourceType:"cron",sourceConfig:{expr:"0 5 1 * *",tz:"UTC"},workflowId:$w,inputFrom:"mapped",input:{topic:"nightly"}}')")
    if [[ "${api_status}" == "201" ]]; then
        ok "R6b: mapped input {topic:\"nightly\"} accepted (201)"
        created_triggers+=("$(printf '%s' "${r6b_resp}" | jq -r '.id')")
    else
        note_fail "R6b: mapped conforming input returned ${api_status}, expected 201 (${r6b_resp})"
    fi

    # R6c — webhook body mode: inputFrom "body" makes the posted payload
    # the run input (top level), schema-validated at fire time.
    r6c_resp=$(api POST /api/v1/me/triggers "$(jq -nc --arg w "${R6_WF}" \
        '{name:"e2e-r6c-body",sourceType:"webhook",sourceConfig:{},workflowId:$w,inputFrom:"body"}')")
    if [[ "${api_status}" != "201" ]]; then
        note_fail "R6c setup: webhook trigger create failed: ${api_status} ${r6c_resp}"
    else
        R6_HOOK=$(printf '%s' "${r6c_resp}" | jq -r '.trigger.id')
        created_triggers+=("${R6_HOOK}")
        r6c_rot=$(api POST "/api/v1/me/triggers/${R6_HOOK}/rotate-secret")
        R6_SECRET=$(printf '%s' "${r6c_rot}" | jq -r '.webhookSecret // empty')
        R6_HOOK_URL="http://127.0.0.1:${PORTFWD_PORT}$(printf '%s' "${r6c_rot}" | jq -r '.webhookUrl // empty')"
        if [[ -n "${R6_SECRET}" && "${R6_HOOK_URL}" != "http://127.0.0.1:${PORTFWD_PORT}" ]]; then
            ok "R6c: rotate-secret returned webhookSecret + webhookUrl"
        else
            note_fail "R6c: rotate-secret failed: ${api_status} ${r6c_rot}"
        fi

        r6_hook_post() { # body -> http code (HMAC-signed POST to the hook URL)
            local body="$1"
            curl -s -m 20 -o /dev/null -w '%{http_code}' -X POST \
                -H "Content-Type: application/json" \
                -H "X-Hub-Signature-256: sha256=$(printf '%s' "${body}" \
                    | openssl dgst -sha256 -hmac "${R6_SECRET}" | awk '{print $NF}')" \
                -d "${body}" "${R6_HOOK_URL}"
        }

        # uq_workflow_run_single_inflight: a delivery while the workflow
        # has a queued/running run 409s with a skipped fire. Wait for the
        # slot to drain (the scheduler tick claims dummy-workspace runs
        # and fast-fails them within ~10s) before EACH signed delivery.
        r6_wait_slot() { # -> 0 once R6_WF has no queued/running run
            local i
            for i in $(seq 1 30); do
                if [[ "$(api GET "/api/v1/me/workflows/${R6_WF}/runs" \
                    | jq '[.runs[] | select(.status=="queued" or .status=="running")] | length')" -eq 0 ]]; then
                    return 0
                fi
                sleep 2
            done
            return 1
        }

        if [[ -n "${R6_SECRET}" && "${R6_HOOK_URL}" != "http://127.0.0.1:${PORTFWD_PORT}" ]]; then
            if r6_wait_slot; then
                ok "R6c: inflight slot drained before the conforming delivery"
            else
                note_fail "R6c: workflow inflight slot never drained (R5b run stuck)"
            fi
            r6c_code=$(r6_hook_post '{"topic":"e2e"}')
            if [[ "${r6c_code}" == "202" ]]; then
                ok "R6c: signed conforming payload accepted (202)"
            else
                note_fail "R6c: signed conforming payload returned ${r6c_code}, expected 202"
            fi

            # Fire recorded + run queued with the PAYLOAD as the run input.
            r6_input=0
            for _ in $(seq 1 15); do
                sleep 2
                if [[ "$(api GET "/api/v1/me/workflows/${R6_WF}/runs" \
                    | jq --arg t "${R6_HOOK}" '[.runs[] | select(.triggerId == $t and .input.topic == "e2e")] | length')" -ge 1 ]]; then
                    r6_input=1; break
                fi
            done
            if [[ "${r6_input}" -eq 1 ]]; then
                ok "R6c: payload arrived as top-level run input (input.topic == \"e2e\")"
            else
                note_fail "R6c: no queued run with input.topic == \"e2e\" (payload not mapped to the run input)"
            fi
            if [[ "$(api GET "/api/v1/me/triggers/${R6_HOOK}/fires" \
                | jq '[.fires[] | select(.status=="fired")] | length')" -ge 1 ]]; then
                ok "R6c: signed delivery recorded a fired fire"
            else
                note_fail "R6c: no fired fire recorded for the signed delivery"
            fi

            # Violating payload → 202 + validation_error fire with typed
            # violations only (no instance echo), and NO run queued.
            # Drain again first: the conforming delivery's own run holds
            # the single-inflight slot until the tick fast-fails it.
            if r6_wait_slot; then
                ok "R6c: inflight slot drained before the violating delivery"
            else
                note_fail "R6c: inflight slot never drained after the conforming delivery"
            fi
            r6c_code=$(r6_hook_post '{"wrong":true}')
            if [[ "${r6c_code}" == "202" ]]; then
                ok "R6c: signed violating payload answered 202 (delivery vs input-contract split)"
            else
                note_fail "R6c: signed violating payload returned ${r6c_code}, expected 202"
            fi
            r6_ve=0
            for _ in $(seq 1 15); do
                sleep 2
                if [[ "$(api GET "/api/v1/me/triggers/${R6_HOOK}/fires" \
                    | jq '[.fires[] | select(.status=="validation_error")] | length')" -ge 1 ]]; then
                    r6_ve=1; break
                fi
            done
            if [[ "${r6_ve}" -ne 1 ]]; then
                note_fail "R6c: no validation_error fire within 30s for the violating payload"
            else
                ok "R6c: validation_error fire recorded for the violating payload"
                r6_ar=$(api GET "/api/v1/me/triggers/${R6_HOOK}/fires" \
                    | jq -c '.fires[] | select(.status=="validation_error") | .actionResult' | head -1)
                if [[ "${r6_ar}" == *"schema_mismatch"* && "${r6_ar}" != *"wrong"* ]]; then
                    ok "R6c: validation_error actionResult carries schema_mismatch with no instance echo"
                else
                    note_fail "R6c: validation_error actionResult wrong: '${r6_ar}'"
                fi
            fi
            r6_count=$(api GET "/api/v1/me/workflows/${R6_WF}/runs" \
                | jq --arg t "${R6_HOOK}" '[.runs[] | select(.triggerId == $t)] | length')
            if [[ "${r6_count}" == "1" ]]; then
                ok "R6c: violating payload queued no run (1 hook-triggered run total)"
            else
                note_fail "R6c: expected 1 hook-triggered run after both deliveries, found ${r6_count}"
            fi
        fi
    fi
fi

# R9 — update-path memory/capture cross-constraint (#1467): create
# enforces memoryMode 'last_result' ⇒ captureMode 'full' (the #1453
# envelope-shape invariant); update must enforce the same on the
# post-patch MERGED view. Monthly expr + dummy workspace: the trigger
# never fires during the run (rows are API-only).
R9_CREATE_BAD=$(api POST /api/v1/me/triggers "$(jq -nc --arg w "${R5_WS}" \
    '{name:"e2e-r9-create-bad",sourceType:"cron",sourceConfig:{expr:"0 3 1 * *",tz:"UTC"},workspaceId:$w,memoryMode:"last_result"}')")
if [[ "${api_status}" == "400" && "${R9_CREATE_BAD}" == *"memoryMode 'last_result' requires captureMode 'full'"* ]]; then
    ok "R9a: create rejects last_result without full (400, constraint error)"
else
    note_fail "R9a: create returned ${api_status} (${R9_CREATE_BAD})"
fi

R9_RESP=$(api POST /api/v1/me/triggers "$(jq -nc --arg w "${R5_WS}" \
    '{name:"e2e-r9-flip",sourceType:"cron",sourceConfig:{expr:"0 3 1 * *",tz:"UTC"},workspaceId:$w,prompt:"r9"}')")
if [[ "${api_status}" == "201" ]]; then
    R9_ID=$(printf '%s' "${R9_RESP}" | jq -r '.id')
    created_triggers+=("${R9_ID}")
else
    die "R9 setup: routine trigger create failed: ${api_status} ${R9_RESP}"
fi

R9_FLIP=$(api PUT "/api/v1/me/triggers/${R9_ID}" '{"memoryMode":"last_result"}')
if [[ "${api_status}" == "400" && "${R9_FLIP}" == *"memoryMode 'last_result' requires captureMode 'full'"* ]] \
    && [[ "$(trigger_field "${R9_ID}" memoryMode)" == "none" ]]; then
    ok "R9b: flip to last_result without full rejected (400), stored memoryMode unchanged"
else
    note_fail "R9b: flip returned ${api_status} (${R9_FLIP}), memoryMode='$(trigger_field "${R9_ID}" memoryMode)'"
fi

api PUT "/api/v1/me/triggers/${R9_ID}" '{"memoryMode":"last_result","captureMode":"full"}' >/dev/null
if [[ "${api_status}" == "200" ]] \
    && [[ "$(trigger_field "${R9_ID}" memoryMode)" == "last_result" ]] \
    && [[ "$(trigger_field "${R9_ID}" captureMode)" == "full" ]]; then
    ok "R9c: flip with full accepted and persisted"
else
    note_fail "R9c: compliant flip returned ${api_status}, memoryMode='$(trigger_field "${R9_ID}" memoryMode)' captureMode='$(trigger_field "${R9_ID}" captureMode)'"
fi

R9_NARROW=$(api PUT "/api/v1/me/triggers/${R9_ID}" '{"captureMode":"errors_only"}')
if [[ "${api_status}" == "400" && "${R9_NARROW}" == *"memoryMode 'last_result' requires captureMode 'full'"* ]]; then
    ok "R9d: narrowing capture under last_result rejected (400)"
else
    note_fail "R9d: narrowing returned ${api_status} (${R9_NARROW})"
fi

# R8 — org-scope automation CRUD resolves the RESOURCE segment (#1449):
# on /orgs/:id/triggers/:triggerId the org id used to shadow the trigger
# id (every org GET/PUT/DELETE/fires 404'd). The API-key user creates the
# org and becomes its admin, then exercises the fixed routes end to end.
R8_ORG_RESP=$(api POST /api/v1/orgs "$(jq -nc '{name:"E2E Automation Org",slug:"e2e-automation-org",ownerEmail:"e2e-automation@example.invalid"}')")
if [[ "${api_status}" != "201" ]]; then
    note_fail "R8 setup: org create failed: ${api_status} ${R8_ORG_RESP}"
else
    R8_ORG=$(printf '%s' "${R8_ORG_RESP}" | jq -r '.id // .org.id // empty')
    if [[ -z "${R8_ORG}" ]]; then
        note_fail "R8 setup: org id missing from create response: ${R8_ORG_RESP}"
    else
        R8_TRESP=$(api POST "/api/v1/orgs/${R8_ORG}/triggers" "$(jq -nc --arg w "${R5_WS}" '{name:"e2e-org-trigger",sourceType:"cron",sourceConfig:{expr:"0 8 1 * *",tz:"UTC"},workspaceId:$w}')")
        if [[ "${api_status}" != "201" ]]; then
            note_fail "R8 setup: org trigger create failed: ${api_status} ${R8_TRESP}"
        else
            R8_ID=$(printf '%s' "${R8_TRESP}" | jq -r '.id')
            # GET must resolve the TRIGGER (the #1449 shadowing 404'd here).
            r8_get=$(api GET "/api/v1/orgs/${R8_ORG}/triggers/${R8_ID}")
            if [[ "${api_status}" == "200" ]] && [[ "$(printf '%s' "${r8_get}" | jq -r '.name // empty')" == "e2e-org-trigger" ]]; then
                ok "R8a: org trigger GET resolves the trigger (not the shadowed org id)"
            else
                note_fail "R8a: org trigger GET returned ${api_status} (${r8_get})"
            fi
            # PUT renames (404 under the shadowing).
            api PUT "/api/v1/orgs/${R8_ORG}/triggers/${R8_ID}" '{"name":"e2e-org-trigger-2"}' >/dev/null
            if [[ "${api_status}" == "200" ]]; then
                ok "R8b: org trigger PUT resolves and mutates"
            else
                note_fail "R8b: org trigger PUT returned ${api_status}, expected 200"
            fi
            # Fires lists without 404.
            api GET "/api/v1/orgs/${R8_ORG}/triggers/${R8_ID}/fires" >/dev/null
            if [[ "${api_status}" == "200" ]]; then
                ok "R8c: org trigger fires route reachable"
            else
                note_fail "R8c: org fires returned ${api_status}, expected 200"
            fi
            # DELETE removes.
            api DELETE "/api/v1/orgs/${R8_ORG}/triggers/${R8_ID}" >/dev/null
            if [[ "${api_status}" == "200" ]]; then
                ok "R8d: org trigger DELETE resolves"
            else
                note_fail "R8d: org trigger DELETE returned ${api_status}, expected 200"
            fi
            # Unhappy: a foreign org id fails closed (403/404, never 200).
            api GET "/api/v1/orgs/00000000-0000-4000-8000-000000000099/triggers/${R8_ID}" >/dev/null
            if [[ "${api_status}" != "200" ]]; then
                ok "R8e: foreign-org trigger GET fails closed (${api_status})"
            else
                note_fail "R8e: foreign-org trigger GET returned 200 — scoping regression"
            fi
        fi
        api DELETE "/api/v1/orgs/${R8_ORG}" >/dev/null 2>&1 || true
    fi
fi

# --- verdict ---------------------------------------------------------------

if [[ "${failures}" -ne 0 ]]; then
    die "automation e2e: ${failures} row(s) failed"
fi
ok "automation e2e: all rows passed (R1-R6)"
