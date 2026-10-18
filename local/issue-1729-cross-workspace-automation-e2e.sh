#!/usr/bin/env bash
# Issue #1729 — cross-workspace routine triggers (owner-scoped) e2e rows.
# Same conventions as local/issue-1410-1412-automation-e2e.sh (see
# local/lib/us70-common.sh): API-only rows, no LLM turns, no workspace
# pods. The create gate resolves server-side; seeded workspace rows are
# METADATA-only (psql, no CR — nothing activates, nothing fires).
#
#   R1 — happy (user REST): a targetWorkspaceId naming ANOTHER workspace
#        owned by the same user creates a trigger whose routine target is
#        the NAMED workspace (201 + response/list carry it).
#   R2 — disclosure 404: a nonexistent target answers the named 404.
#   R3 — authorization 403: a REAL workspace owned by ANOTHER user
#        answers 403 (nothing persisted — verified via the list).
#   R4 — omitted-param compatibility: a plain workspaceId create keeps
#        its pre-#1729 contract (201, targets the explicit workspace).
#   R5 — workflow-targeting rules unchanged: workflowId +
#        targetWorkspaceId together answer the named 400 (real workflow,
#        per the 35597973572 create contract).
#   R6 — create-only on update: a targetWorkspaceId PATCH answers the
#        shared create-only 400 on the REST surface too (r1), while an
#        ordinary PATCH still applies.
#   R7 — org-scope rows (SKIP where org creation is platform-admin
#        gated, mirroring the #1410 R8 skip): the org route authorizes
#        the ACTING user's rights on the target (r1 BLOCKER fix) — an
#        org-attributed workspace owned by ANOTHER user is a legal target
#        for the org admin, and a non-owned workspace still 403s.
#
# Environment: nightly kind cluster (harness_start seeds the user/API
# key). No LLM_MODEL required.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/us70-common.sh
source "${SCRIPT_DIR}/lib/us70-common.sh"

failures=0
SKIPPED_ROWS=0
note_fail() { failures=$((failures + 1)); warn "FAIL: $*"; }
created_triggers=()
created_workflows=()

cleanup() {
    local id
    for id in ${created_triggers[*]:-}; do
        curl -sfm 10 -X DELETE -H "Authorization: Bearer ${API_KEY:-}" \
            "http://127.0.0.1:${PORTFWD_PORT}/api/v1/me/triggers/${id}" >/dev/null 2>&1 || true
    done
    for id in ${created_workflows[*]:-}; do
        curl -sfm 10 -X DELETE -H "Authorization: Bearer ${API_KEY:-}" \
            "http://127.0.0.1:${PORTFWD_PORT}/api/v1/me/workflows/${id}" >/dev/null 2>&1 || true
    done
}
trap cleanup EXIT

# harness_start FIRST (the #1452/#1417 pinned pattern): port-forward,
# livez retry gate, session user + API key.
harness_start

api() { # method path [body] -> body on stdout; api_status + api_body globals
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

track_trigger() { # resp -> id (echoed), tracked for cleanup
    local id; id=$(printf '%s' "$1" | jq -r '.id // empty')
    [[ -n "${id}" ]] && created_triggers+=("${id}")
    printf '%s' "${id}"
}

# Seeded workspace METADATA rows (no CRs, no pods — nothing activates at
# create time; these rows exist only for the gate's Resolve/Check calls
# and the triggers.workspace_id FK). Mirror seed_workspace_metadata's
# columns. ws-1729-evil belongs to ANOTHER user (the 403 row); the
# org-attributed row (R7) is seeded after the org exists.
WS_A="00000000-0000-4000-8000-000000172900"
WS_B="00000000-0000-4000-8000-000000172901"
WS_EVIL="00000000-0000-4000-8000-000000172902"
OTHER_USER="00000000-0000-4000-8000-00000017290f"
kc exec "${PGPOD}" -- env PGPASSWORD="${PG_PWD}" \
    psql -U llmsafespaces -d llmsafespaces -v ON_ERROR_STOP=1 -c "
INSERT INTO workspaces (id, name, user_id, namespace, runtime, storage_size) VALUES
 ('${WS_A}', 'e2e-1729-a', '${OWNER_ID}', '${NS}', 'python:3.11', '1Gi'),
 ('${WS_B}', 'e2e-1729-b', '${OWNER_ID}', '${NS}', 'python:3.11', '1Gi'),
 ('${WS_EVIL}', 'e2e-1729-evil', '${OTHER_USER}', '${NS}', 'python:3.11', '1Gi')
ON CONFLICT (id) DO UPDATE SET user_id = EXCLUDED.user_id, deleted_at = NULL;
" >/dev/null

# --- R1: happy cross-workspace create (user REST) --------------------------

api POST /api/v1/me/triggers "$(jq -nc --arg w "${WS_B}" \
    '{name:"e2e-1729-cross",sourceType:"cron",sourceConfig:{expr:"23 4 * * *",tz:"UTC"},prompt:"p",targetWorkspaceId:$w}')"
r1_resp="${api_body}"; R1_ID=$(track_trigger "${r1_resp}")
if [[ "${api_status}" == "201" && "$(printf '%s' "${r1_resp}" | jq -r '.workspaceId // empty')" == "${WS_B}" ]]; then
    ok "R1a: cross-workspace create 201, routine target is the NAMED workspace"
else
    note_fail "R1a: cross-workspace create returned ${api_status} targeting $(printf '%s' "${r1_resp}" | jq -r '.workspaceId // empty'), expected 201/${WS_B} (${r1_resp})"
fi
api GET "/api/v1/me/triggers/${R1_ID}" >/dev/null
if [[ "${api_status}" == "200" && "$(printf '%s' "${api_body}" | jq -r '.workspaceId // empty')" == "${WS_B}" ]]; then
    ok "R1b: created trigger lists with the target workspace"
else
    note_fail "R1b: GET trigger returned ${api_status} (${api_body})"
fi

# --- R2: nonexistent target answers the disclosure-policy 404 --------------

api POST /api/v1/me/triggers "$(jq -nc \
    '{name:"e2e-1729-ghost",sourceType:"cron",sourceConfig:{expr:"23 4 * * *",tz:"UTC"},prompt:"p",targetWorkspaceId:"00000000-0000-4000-8000-00000000dead"}')"
r2_resp="${api_body}"
if [[ "${api_status}" == "404" && "${r2_resp}" == *"target workspace not found"* ]]; then
    ok "R2: nonexistent target answers the named 404"
else
    note_fail "R2: ghost target returned ${api_status} (${r2_resp}), expected 404 target workspace not found"
fi

# --- R3: another user's workspace answers 403, nothing persisted -----------

api POST /api/v1/me/triggers "$(jq -nc --arg w "${WS_EVIL}" \
    '{name:"e2e-1729-evil",sourceType:"cron",sourceConfig:{expr:"23 4 * * *",tz:"UTC"},prompt:"p",targetWorkspaceId:$w}')"
r3_resp="${api_body}"
if [[ "${api_status}" == "403" && "${r3_resp}" == *"no owner or admin rights on target workspace"* ]]; then
    ok "R3a: another user's workspace answers the named 403"
else
    note_fail "R3a: non-owned target returned ${api_status} (${r3_resp}), expected 403"
fi
api GET /api/v1/me/triggers >/dev/null
if [[ "$(printf '%s' "${api_body}" | jq -r '[.triggers[] | select(.name == "e2e-1729-evil")] | length')" == "0" ]]; then
    ok "R3b: rejected create persisted nothing"
else
    note_fail "R3b: a trigger named e2e-1729-evil exists after the 403"
fi

# --- R4: omitted param keeps the explicit-workspaceId contract -------------

api POST /api/v1/me/triggers "$(jq -nc --arg w "${WS_A}" \
    '{name:"e2e-1729-plain",sourceType:"cron",sourceConfig:{expr:"23 4 * * *",tz:"UTC"},prompt:"p",workspaceId:$w}')"
r4_resp="${api_body}"; track_trigger "${r4_resp}" >/dev/null
if [[ "${api_status}" == "201" && "$(printf '%s' "${r4_resp}" | jq -r '.workspaceId // empty')" == "${WS_A}" ]]; then
    ok "R4: plain workspaceId create unchanged by #1729"
else
    note_fail "R4: plain create returned ${api_status} (${r4_resp})"
fi

# --- R5: workflowId + targetWorkspaceId stay mutually exclusive ------------

api POST /api/v1/me/workflows "$(jq -nc --arg ws "${WS_A}" '{name:"e2e-1729-real-target",
    specYaml:"{\"nodes\":[{\"id\":\"n\",\"type\":\"script\",\"data\":{\"language\":\"python\",\"handler\":\"def handler(input): return {}\"}}],\"edges\":[]}",
    targetWorkspaceId:$ws}')"
r5w_resp="${api_body}"
if [[ "${api_status}" == "201" ]]; then
    R5_WF=$(printf '%s' "${r5w_resp}" | jq -r '.id'); created_workflows+=("${R5_WF}")
    api POST /api/v1/me/triggers "$(jq -nc --arg wf "${R5_WF}" --arg w "${WS_B}" \
        '{name:"e2e-1729-both",sourceType:"cron",sourceConfig:{expr:"23 4 * * *",tz:"UTC"},workflowId:$wf,targetWorkspaceId:$w}')"
    r5_resp="${api_body}"
    if [[ "${api_status}" == "400" && "${r5_resp}" == *"cannot set both workflowId and targetWorkspaceId"* ]]; then
        ok "R5: workflowId + targetWorkspaceId answers the named 400"
    else
        note_fail "R5: both-targets create returned ${api_status} (${r5_resp}), expected 400"
    fi
else
    note_fail "R5 setup: workflow create failed: ${api_status} ${r5w_resp}"
fi

# --- R6: targetWorkspaceId is create-only on the REST update surface -------

api PUT "/api/v1/me/triggers/${R1_ID}" "$(jq -nc --arg w "${WS_A}" '{targetWorkspaceId:$w}')"
r6_resp="${api_body}"
if [[ "${api_status}" == "400" && "${r6_resp}" == *"targetWorkspaceId is create-only"* ]]; then
    ok "R6a: REST PATCH with targetWorkspaceId answers the shared create-only 400"
else
    note_fail "R6a: target-key PATCH returned ${api_status} (${r6_resp}), expected 400 create-only"
fi
api PUT "/api/v1/me/triggers/${R1_ID}" '{"enabled":false}' >/dev/null
if [[ "${api_status}" == "200" ]]; then
    ok "R6b: ordinary PATCH still applies (the pre-bind peek broke nothing)"
else
    note_fail "R6b: ordinary PATCH returned ${api_status} (${api_body})"
fi

# --- R7: org route authorizes the ACTING user (r1 BLOCKER fix, live) -------

api POST /api/v1/orgs "$(jq -nc --arg em "${USER_ID}@example.test" '{name:"E2E 1729 Org",slug:"e2e-1729-org",ownerEmail:$em}')"
R7_ORG_RESP="${api_body}"
if [[ "${api_status}" == "403" && "${R7_ORG_RESP}" == *"only platform admins"* ]]; then
    SKIPPED_ROWS=$((SKIPPED_ROWS + 2))
    warn "SKIP R7: org creation is platform-admin gated in this build — the org-scope rows run where admin access is available (the r1 fix is pinned at unit level either way)"
elif [[ "${api_status}" != "201" ]]; then
    note_fail "R7 setup: org create failed: ${api_status} ${R7_ORG_RESP}"
else
    R7_ORG=$(printf '%s' "${R7_ORG_RESP}" | jq -r '.id // .org.id // empty')
    if [[ -z "${R7_ORG}" ]]; then
        note_fail "R7 setup: org id missing from create response: ${R7_ORG_RESP}"
    else
        # An org-attributed workspace owned by ANOTHER user: the acting
        # user (org admin) holds admin rights on it via the org — the
        # exact branch the pre-r1 gate could never reach (it passed the
        # ORG id where CheckOwnership expects a user id).
        WS_ORGROW="00000000-0000-4000-8000-000000172903"
        kc exec "${PGPOD}" -- env PGPASSWORD="${PG_PWD}" \
            psql -U llmsafespaces -d llmsafespaces -v ON_ERROR_STOP=1 -c "
INSERT INTO workspaces (id, name, user_id, namespace, runtime, storage_size, org_id)
VALUES ('${WS_ORGROW}', 'e2e-1729-orgrow', '${OTHER_USER}', '${NS}', 'python:3.11', '1Gi', '${R7_ORG}')
ON CONFLICT (id) DO UPDATE SET user_id = EXCLUDED.user_id, org_id = EXCLUDED.org_id, deleted_at = NULL;
" >/dev/null
        api POST "/api/v1/orgs/${R7_ORG}/triggers" "$(jq -nc --arg w "${WS_ORGROW}" \
            '{name:"e2e-1729-org-admin",sourceType:"cron",sourceConfig:{expr:"23 4 * * *",tz:"UTC"},prompt:"p",targetWorkspaceId:$w}')"
        r7_resp="${api_body}"
        if [[ "${api_status}" == "201" && "$(printf '%s' "${r7_resp}" | jq -r '.workspaceId // empty')" == "${WS_ORGROW}" ]]; then
            ok "R7a: org-route create via the ACTING org admin targets the org workspace (r1 fix)"
        else
            note_fail "R7a: org-route admin-branch create returned ${api_status} (${r7_resp}), expected 201/${WS_ORGROW}"
        fi
        api POST "/api/v1/orgs/${R7_ORG}/triggers" "$(jq -nc --arg w "${WS_EVIL}" \
            '{name:"e2e-1729-org-evil",sourceType:"cron",sourceConfig:{expr:"23 4 * * *",tz:"UTC"},prompt:"p",targetWorkspaceId:$w}')"
        r7e_resp="${api_body}"
        if [[ "${api_status}" == "403" && "${r7e_resp}" == *"no owner or admin rights on target workspace"* ]]; then
            ok "R7b: org-route create without target rights still 403s (fail-closed preserved)"
        else
            note_fail "R7b: org-route non-owned target returned ${api_status} (${r7e_resp}), expected 403"
        fi
    fi
    api DELETE "/api/v1/orgs/${R7_ORG}" >/dev/null 2>&1 || true
fi

# --- verdict ---------------------------------------------------------------

if [[ "${failures}" -ne 0 ]]; then
    die "1729 cross-workspace e2e: ${failures} row(s) failed (${SKIPPED_ROWS} skipped)"
fi
ok "1729 cross-workspace e2e: all rows passed (${SKIPPED_ROWS} skipped — see warnings above)"
