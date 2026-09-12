#!/usr/bin/env bash
# Epic 71 / 3a (#1313) — unanswered-question inbox, cluster-bound rows.
#
# Precondition staging is honest about what it simulates: a pending inbox
# record is seeded DIRECTLY into valkey (the state a dead live ask leaves
# behind — the recording/death mechanisms are pinned by the unit suites
# and the G1 fixtures; pkg/agent/opencode/testdata/
# ask_terminal_states_1_18_15.json). This script exercises the CLUSTER
# surfaces: SSE re-presentation latency (L7), the late-answer outbox
# route (L8-accept), S2 idempotency, dismiss (S11), and suspension
# survival (A3 at cluster scale).
#
#   W1 — re-presentation: seed → user-SSE connect → agent.question
#        (whileAway) event ≤2s (L7).
#   W2 — late answer: POST /question/{id}/reply on the dead ask → 202 +
#        outbox entry; duplicate re-POST → same entry (S2, cmid dedupe);
#        record terminal answered.
#   W3 — dismiss: DELETE /sessions/{sid}/inbox/{id} → 204; record
#        terminal dismissed; snapshot no longer re-presents it.
#   W4 — suspension variant: seed → suspend → wait Suspended → activate
#        → wait Active → W1 assert again (the inbox is API-side; pod
#        death must not lose it).
#
# Environment: same conventions as local/us-70-secret-delivery-e2e.sh
# (see local/lib/us70-common.sh). The pool workflow runs this BEFORE the
# fault seam is armed.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/us70-common.sh
source "${SCRIPT_DIR}/lib/us70-common.sh"

# Distinct workspace base (uuid column): the sibling suites run on the
# same pool cluster; a shared base would collide on ws_id suffixes.
WS_BASE="${WS_BASE:-e2e71000-0000-4000-8000-000000000000}"

L7_BUDGET_S="${L7_BUDGET_S:-2}"
W4_SUSPEND_S="${W4_SUSPEND_S:-10}"

failures=0
note_fail() { failures=$((failures + 1)); warn "FAIL: $*"; }

valkey_pod() {
    kc get pods -l app=valkey --field-selector=status.phase=Running \
        -o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

# Valkey requires auth (G26): same llmsafespaces-credentials Secret the
# postgres path reads (PG_PWD precedent in us70-common.sh).
redis_pw() {
    [[ -n "${REDIS_PW:-}" ]] && { echo "${REDIS_PW}"; return; }
    kc get secret llmsafespaces-credentials \
        -o jsonpath='{.data.redis-password}' 2>/dev/null | base64 -d 2>/dev/null || true
}

vkey_exec() { # args... — authenticated redis-cli inside the valkey pod
    local vp pw
    vp="$(valkey_pod)" || true
    [[ -n "${vp}" ]] || die "no running valkey pod found (label app=valkey)"
    pw="$(redis_pw)"
    kc exec "${vp}" -- redis-cli ${pw:+-a "${pw}"} --no-auth-warning "$@"
}

seed_inbox_record() { # ws ses ask_id question — HSETs one pending record, LOUDLY
    local ws="$1" ses="$2" ask="$3" question="$4" now_ns payload hset_rc
    now_ns=$(date +%s%3N)000000
    payload=$(jq -nc \
        --arg id "${ask}" --arg ses "${ses}" --arg q "${question}" \
        --argjson ns "${now_ns}" --arg ts "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
        '{id:$id, sessionID:$ses, kind:"question", status:"pending",
          recordedNs:$ns, recordedAt:$ts, question:$q, header:"Walk-away",
          options:[{label:"Yes, deploy",description:"ship"},{label:"No, hold",description:"wait"}],
          custom:true}')
    # A silently-failed seed (auth, wrong pod, typo'd key) makes every
    # later assert pass or fail vacuously — the first pool run died on
    # exactly this (NOAUTH swallowed by >/dev/null). The seed must prove
    # itself: HSET integer reply + read-back of the ask ID.
    hset_rc=$(vkey_exec HSET "inboxq:${ws}:${ses}" "${ask}" "${payload}" 2>&1)
    [[ "${hset_rc}" == "1" || "${hset_rc}" == "0" ]] \
        || die "seed_inbox_record: HSET failed for ${ask}: ${hset_rc}"
    vkey_exec EXPIRE "inboxq:${ws}:${ses}" 3600 >/dev/null
    local readback
    readback=$(vkey_exec --raw HGET "inboxq:${ws}:${ses}" "${ask}" 2>/dev/null | jq -r '.id // empty' 2>/dev/null)
    [[ "${readback}" == "${ask}" ]] || die "seed_inbox_record: readback mismatch for ${ask}: '${readback}'"
}

inbox_status() { # ws ses ask → prints the record's status field
    vkey_exec --raw HGET "inboxq:$1:$2" "$3" 2>/dev/null \
        | jq -r '.status // "absent"' 2>/dev/null || echo absent
}

# sse_wait_event <type> <timeout_s> <workspace marker — reads the user
# SSE stream in the background and greps for the first matching frame.
sse_capture() { # out_file — start a detached user-events capture
    : >"$1"
    curl -sN -m 30 -H "Authorization: Bearer ${AUTH_TOKEN:?}" \
        "http://127.0.0.1:${PORTFWD_PORT}/api/v1/events" >"$1" 2>/dev/null &
    SSE_PID=$!
    sleep 1
}
sse_stop() { [[ -n "${SSE_PID:-}" ]] && kill "${SSE_PID}" 2>/dev/null || true; }

# ----------------------------------------------------------------------------

log "epic71/3a walk-away rows: workspace + session setup"
harness_start
seed_session "${USER_ID}"

# Precondition: authenticated valkey reachability — every row depends on
# it; dying here beats nine vacuous failures downstream.
vkey_exec PING >/dev/null 2>&1 || die "authenticated valkey PING failed (check redis-password in llmsafespaces-credentials)"
ok "valkey reachable (authenticated)"

W1_WS=$(ws_id 71)
W1_SES="ses_e71w1deadbeef"
seed_workspace "${W1_WS}"
wait_phase "${W1_WS}" Active 300

# --- W1: re-presentation within L7 -----------------------------------------
log "W1: seed a pending inbox record; SSE connect must re-present it whileAway ≤${L7_BUDGET_S}s"
seed_inbox_record "${W1_WS}" "${W1_SES}" que_e71w1aaa "Deploy the blue widget?"
CAP=/tmp/e71w1_sse.txt
T0=$(date +%s%3N)
sse_capture "${CAP}"
grep -q '"que_e71w1aaa"' "${CAP}" 2>/dev/null || true
deadline=$(( $(date +%s%3N) + L7_BUDGET_S * 1000 + 3000 ))
until grep -q 'que_e71w1aaa' "${CAP}" 2>/dev/null; do
    [[ $(date +%s%3N) -lt ${deadline} ]] || break
    sleep 0.2
done
T1=$(date +%s%3N)
sse_stop
if grep -q '"id":"que_e71w1aaa"' "${CAP}" 2>/dev/null \
    && grep -q '"whileAway":true' "${CAP}" 2>/dev/null; then
    ELAPSED=$(( (T1 - T0) ))
    if [[ ${ELAPSED} -le $(( L7_BUDGET_S * 1000 + 3000 )) ]]; then
        ok "W1 re-presented whileAway in ${ELAPSED}ms (L7 ≤${L7_BUDGET_S}s + connect grace)"
    else
        note_fail "W1 event arrived but took ${ELAPSED}ms (L7 budget ${L7_BUDGET_S}s)"
    fi
else
    note_fail "W1: no whileAway re-presentation on SSE connect (see ${CAP})"
fi

# --- W2: late answer through the outbox (S2 idempotent) --------------------
log "W2: late answer on the dead ask routes through the outbox exactly once"
code=$(curl -s -o /tmp/e71w2_resp.json -w '%{http_code}' -m 15 \
    -H "Authorization: Bearer ${AUTH_TOKEN}" \
    -H 'Content-Type: application/json' \
    -X POST "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${W1_WS}/question/que_e71w1aaa/reply" \
    -d '{"answers":[["Yes, deploy"]]}')
if [[ "${code}" == "202" ]]; then
    ok "W2 late answer accepted (202)"
else
    note_fail "W2 expected 202, got ${code}: $(head -c 300 /tmp/e71w2_resp.json)"
fi
MSG1=$(jq -r '.messageID // empty' /tmp/e71w2_resp.json 2>/dev/null)
[[ "${MSG1}" == ob_* ]] && ok "W2 outbox entry minted (${MSG1})" \
    || note_fail "W2 response carries no outbox messageID: $(head -c 200 /tmp/e71w2_resp.json)"
# S2 evidence is the ACCEPT, not the queue depth: the delivery worker
# moves entries queue→staging concurrently with this assert, so LLEN is
# timing-dependent by design. The dedupe marker (set AFTER a successful
# push, value = entry ID) is the stable exactly-once proof.
MARKER=$(vkey_exec --raw GET "outboxdedupe:${W1_WS}:${W1_SES}:inbox-que_e71w1aaa-answer" 2>/dev/null)
if [[ -n "${MARKER}" && "${MARKER}" == ob_* ]]; then
    ok "W2 dedupe marker anchored (${MARKER})"
else
    note_fail "W2 dedupe marker missing/invalid: '${MARKER}'"
fi
ST=$(inbox_status "${W1_WS}" "${W1_SES}" que_e71w1aaa)
[[ "${ST}" == "answered" ]] && ok "W2 record terminal answered" || note_fail "W2 status '${ST}', want answered"

code2=$(curl -s -o /tmp/e71w2b_resp.json -w '%{http_code}' -m 15 \
    -H "Authorization: Bearer ${AUTH_TOKEN}" \
    -H 'Content-Type: application/json' \
    -X POST "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${W1_WS}/question/que_e71w1aaa/reply" \
    -d '{"answers":[["Yes, deploy"]]}')
DUP=$(jq -r '.duplicate // empty' /tmp/e71w2b_resp.json 2>/dev/null)
MSG2=$(jq -r '.messageID // empty' /tmp/e71w2b_resp.json 2>/dev/null)
if [[ "${code2}" == "202" && "${DUP}" == "true" && "${MSG2}" == "${MSG1}" ]]; then
    ok "W2 duplicate re-POST idempotent (S2: duplicate=true, same entry)"
else
    note_fail "W2 duplicate: code=${code2} duplicate=${DUP} msg=${MSG2} (want 202/true/${MSG1})"
fi

# --- W3: dismiss exit (S11) -------------------------------------------------
log "W3: dismiss terminalizes the record and stops re-presentation"
seed_inbox_record "${W1_WS}" "${W1_SES}" que_e71w1bbb "Second stale ask?"
code=$(curl -s -o /dev/null -w '%{http_code}' -m 15 \
    -H "Authorization: Bearer ${AUTH_TOKEN}" \
    -X DELETE "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${W1_WS}/sessions/${W1_SES}/inbox/que_e71w1bbb")
[[ "${code}" == "204" ]] && ok "W3 dismiss 204" || note_fail "W3 expected 204, got ${code}"
ST=$(inbox_status "${W1_WS}" "${W1_SES}" que_e71w1bbb)
[[ "${ST}" == "dismissed" ]] && ok "W3 record terminal dismissed" || note_fail "W3 status '${ST}', want dismissed"

# Re-connect: the dismissed record must NOT re-present (terminal records
# never re-present; the answered record neither).
CAP3=/tmp/e71w3_sse.txt
sse_capture "${CAP3}"
sleep 3
sse_stop
if grep -q 'que_e71w1bbb' "${CAP3}" 2>/dev/null; then
    note_fail "W3 dismissed record re-presented on connect"
else
    ok "W3 dismissed record absent from the snapshot flight"
fi

# --- W4: suspension survival ------------------------------------------------
log "W4: the inbox record survives suspend/resume (API-side state)"
seed_inbox_record "${W1_WS}" "${W1_SES}" que_e71w1ccc "Survives suspension?"
code=$(curl -s -o /dev/null -w '%{http_code}' -m 30 \
    -H "Authorization: Bearer ${AUTH_TOKEN}" \
    -X POST "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${W1_WS}/suspend")
[[ "${code}" == "202" ]] || note_fail "W4 suspend: code ${code}"
wait_phase "${W1_WS}" Suspended 300
ok "W4 suspended (pod deleted)"
sleep "${W4_SUSPEND_S}"
code=$(curl -s -o /dev/null -w '%{http_code}' -m 30 \
    -H "Authorization: Bearer ${AUTH_TOKEN}" \
    -X POST "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${W1_WS}/activate")
[[ "${code}" == "202" || "${code}" == "200" ]] || note_fail "W4 activate: code ${code}"
wait_phase "${W1_WS}" Active 600
ok "W4 resumed Active"

CAP4=/tmp/e71w4_sse.txt
sse_capture "${CAP4}"
deadline=$(( $(date +%s%3N) + L7_BUDGET_S * 1000 + 3000 ))
until grep -q 'que_e71w1ccc' "${CAP4}" 2>/dev/null; do
    [[ $(date +%s%3N) -lt ${deadline} ]] || break
    sleep 0.2
done
sse_stop
if grep -q '"id":"que_e71w1ccc"' "${CAP4}" 2>/dev/null && grep -q '"whileAway":true' "${CAP4}" 2>/dev/null; then
    ok "W4 record re-presented whileAway after resume (inbox survives pod death)"
else
    note_fail "W4: no whileAway re-presentation after resume (see ${CAP4})"
fi

# Cleanup: leave the workspace suspended to free capacity.
curl -s -o /dev/null -m 30 -H "Authorization: Bearer ${AUTH_TOKEN}" \
    -X POST "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${W1_WS}/suspend" || true

# ----------------------------------------------------------------------------
if [[ ${failures} -gt 0 ]]; then
    die "epic71/3a walk-away rows: ${failures} failure(s)"
fi
log "epic71/3a walk-away rows: ALL GREEN"
