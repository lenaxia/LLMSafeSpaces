#!/usr/bin/env bash
# US-63 V2 Session Queue — Behavioral E2E Assertions
#
# Hard-assert script (exit non-zero on any failure) that exercises the three
# behavioral properties US-63.3/63.4/63.9 require, against a real opencode
# running in a kind workspace pod. Designed to run AFTER local/test.sh has
# brought up the cluster, workspace, and API port-forward.
#
# Prerequisites:
#   - kind cluster with LLMSafeSpaces installed (local/bootstrap.sh or nightly CI)
#   - API port-forward on $PORTFWD_PORT (set up by test.sh)
#   - LLM credentials configured (LLM_BASE_URL, LLM_API_KEY, LLM_MODEL)
#   - A workspace named $WORKSPACE_NAME at Phase=Active with LLM creds staged
#   - kubectl pointed at the cluster
#
# What it does:
#   1. Enables the V2 flag on the API deployment (kubectl set env + rollout)
#   2. Creates a fresh session
#   3. US-63.3: enqueue while busy → message runs after current turn
#   4. US-63.4: abort mid-turn → queued messages survive and run after
#   5. US-63.9: queue messages → kill opencode → restart → messages drain
#   6. Disables the V2 flag (cleanup)
#
# Exit codes: 0 = all assertions passed; 1 = assertion failure.
set -Eeuo pipefail

if [[ -t 1 ]]; then
    BOLD=$'\033[1m'; RED=$'\033[31m'; GREEN=$'\033[32m'
    YELLOW=$'\033[33m'; CYAN=$'\033[36m'; RESET=$'\033[0m'
else
    BOLD=''; RED=''; GREEN=''; YELLOW=''; CYAN=''; RESET=''
fi
log()  { printf '%s==>%s %s\n' "${CYAN}${BOLD}" "${RESET}" "$*"; }
ok()   { printf '%s ✓%s %s\n' "${GREEN}" "${RESET}" "$*"; }
warn() { printf '%s !%s %s\n' "${YELLOW}" "${RESET}" "$*" >&2; }
die()  { printf '%s ✗%s %s\n' "${RED}${BOLD}" "${RESET}" "$*" >&2; exit 1; }

CLUSTER_NAME="${CLUSTER_NAME:-llmsafespaces-ci}"
CTX="${CTX:-kind-${CLUSTER_NAME}}"
NS="${NS:-llmsafespaces}"
WORKSPACE_NAME="${WORKSPACE_NAME:-e2e-workspace}"
PORTFWD_PORT="${PORTFWD_PORT:-18080}"
API_KEY="${API_KEY:-lsp_e2etestkey1234567890abcdef}"
LLM_MODEL="${LLM_MODEL:-${LLM_DEFAULT_MODEL:-default}}"

# EXTRA_CURL_HEADERS: extra curl header args for clusters whose API enforces
# HTTPS behind the port-forward (security.go SSLRedirect 301s /api/* to
# https). For such clusters, run with EXTRA_CURL_HEADERS='-H X-Forwarded-Proto:
# https' so the secure middleware treats the request as already-TLS. kind
# deployments (RequireHTTPS=false) leave this empty.
# shellcheck disable=SC2206
EXTRA_CURL_HEADERS=( ${EXTRA_CURL_HEADERS:-} )

kc() { kubectl --context "${CTX}" -n "${NS}" "$@"; }

command -v kubectl >/dev/null || die "kubectl not on PATH"
command -v curl    >/dev/null || die "curl not on PATH"

# -----------------------------------------------------------------------------
# Phase 0: enable the V2 flag
# -----------------------------------------------------------------------------
log "Phase 0 — enable V2 session queue flag on API deployment"

kc set env deployment/llmsafespaces-api LLMSAFESPACES_V2_SESSION_QUEUE=true
log "  waiting for API rollout (up to 120s)"
kc rollout status deployment/llmsafespaces-api --timeout=120s
ok "API restarted with V2 flag enabled"

# Re-establish port-forward if it died during rollout.
if ! curl -sfm 2 "http://127.0.0.1:${PORTFWD_PORT}/livez" >/dev/null 2>&1; then
    warn "port-forward died during rollout; re-establishing"
    kc port-forward svc/llmsafespaces-api "${PORTFWD_PORT}:8080" >/dev/null 2>&1 &
    PF_PID=$!
    for i in $(seq 1 15); do
        curl -sfm 1 "http://127.0.0.1:${PORTFWD_PORT}/livez" >/dev/null 2>&1 && break
        sleep 1
    done
fi

# -----------------------------------------------------------------------------
# Helpers
# -----------------------------------------------------------------------------

# Create a session and echo the session ID.
#
# FIX: the real route is POST /sessions/new (router.go:1276), NOT POST
# /sessions — the latter 404s. The response key is "sessionId", not "id".
create_session() {
    local resp
    resp=$(curl -sfm 15 -X POST \
        -H "Authorization: Bearer ${API_KEY}" "${EXTRA_CURL_HEADERS[@]}" \
        -H "Content-Type: application/json" \
        -d '{}' \
        "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/sessions/new" \
        2>/dev/null || echo '{}')
    echo "$resp" | python3 -c "import json,sys;d=json.load(sys.stdin);print(d.get('sessionId') or d.get('id') or d.get('info',{}).get('id') or '')" 2>/dev/null || echo ""
}

# Send a message (blocking until the turn completes).
send_message() {
    local sid="$1" text="$2"
    local body
    body=$(python3 -c "import json;print(json.dumps({'model':{'providerID':'litellm','modelID':'${LLM_MODEL}'},'parts':[{'type':'text','text':'''${text}'''}]}))")
    curl -sfm 180 -X POST \
        -H "Authorization: Bearer ${API_KEY}" "${EXTRA_CURL_HEADERS[@]}" \
        -H "Content-Type: application/json" \
        -d "$body" \
        "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/sessions/${sid}/message" \
        2>/dev/null || echo '{}'
}

# Enqueue a message (non-blocking; V2 path via /queue endpoint).
#
# FIX: drop curl's -f here. With -f, an HTTP 5xx (e.g. a transient cold-start
# 500 right after the V2-flag rollout) makes curl exit non-zero AND still emit
# the http_code via -w; the trailing `|| echo "000"` then appends "000",
# producing a garbled code like "500000". Using -s (no -f) lets curl exit 0 on
# HTTP errors so -w yields the true code; the `|| echo "000"` only fires on
# connection failures (exit 7).
enqueue_message() {
    local sid="$1" text="$2"
    curl -sm 15 -X POST \
        -H "Authorization: Bearer ${API_KEY}" "${EXTRA_CURL_HEADERS[@]}" \
        -H "Content-Type: application/json" \
        -d "{\"text\":\"${text}\"}" \
        "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/sessions/${sid}/queue" \
        -o /dev/null -w "%{http_code}" 2>/dev/null || echo "000"
}

# Abort a session (V2 path → interrupt). Same -f fix as enqueue_message.
abort_session() {
    local sid="$1"
    curl -sm 15 -X POST \
        -H "Authorization: Bearer ${API_KEY}" "${EXTRA_CURL_HEADERS[@]}" \
        "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/sessions/${sid}/abort" \
        -o /dev/null -w "%{http_code}" 2>/dev/null || echo "000"
}

# Get session history — returns the raw JSON.
get_history() {
    local sid="$1"
    curl -sfm 15 \
        -H "Authorization: Bearer ${API_KEY}" "${EXTRA_CURL_HEADERS[@]}" \
        "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/sessions/${sid}/message" \
        2>/dev/null || echo '[]'
}

# Check whether a specific text marker was ECHOED by the assistant in history.
#
# FIX: the previous version substring-matched the marker anywhere in history.
# That is a false positive: the user's own queued message ("reply with
# exactly: <MARKER>") is itself stored in history, so a substring match always
# succeeds the moment the message is enqueued — making US-63.3/63.4/63.9 appear
# to PASS before the assistant has produced any reply. opencode 1.18.10 returns
# message role as null in this view (not "assistant"), so we cannot key on
# role. Instead we require a part whose stripped text equals the marker exactly
# — the assistant echoes the bare token, the user prompt does not ("reply with
# exactly: …"). Marker strings must therefore be bare tokens (no spaces).
history_contains() {
    local sid="$1" marker="$2"
    local hist
    hist=$(get_history "$sid")
    MARKER="$marker" echo "$hist" | python3 -c "
import json, os, sys
marker = os.environ['MARKER']
try:
    d = json.load(sys.stdin)
except:
    print('NO'); sys.exit()
msgs = d if isinstance(d, list) else d.get('data', d.get('messages', []))
if not isinstance(msgs, list):
    print('NO'); sys.exit()
for m in msgs:
    parts = m.get('parts', []) if isinstance(m, dict) else []
    for p in parts:
        if isinstance(p, dict) and p.get('text', '').strip() == marker:
            print('YES'); sys.exit()
print('NO')
" 2>/dev/null || echo "NO"
}

# Wait for a marker to appear in history, polling every 3s up to $1 seconds.
wait_for_marker() {
    local timeout_s="$1" sid="$2" marker="$3"
    local elapsed=0
    while (( elapsed < timeout_s )); do
        [[ "$(history_contains "$sid" "$marker")" == "YES" ]] && return 0
        sleep 3
        (( elapsed += 3 ))
    done
    return 1
}

# -----------------------------------------------------------------------------
# Phase 1: US-63.3 — enqueue while busy → runs after current turn
# -----------------------------------------------------------------------------
log "Phase 1 — US-63.3: enqueue while busy → runs after current turn"

SID_1=$(create_session)
[[ -n "$SID_1" ]] || die "failed to create session for US-63.3"
ok "session: ${SID_1}"

# Start a long turn (200-word essay) — this is blocking, so we background it.
log "  starting long turn (200-word essay) in background"
(send_message "$SID_1" "write a 200-word essay about the ocean" >/dev/null 2>&1) &
LONG_PID=$!
sleep 2  # let opencode pick up the prompt and start the LLM call

# Enqueue a short message while the long turn is in flight.
log "  enqueueing short message while busy"
MARKER_63_3="ACK_63_3_$(date +%s)"
ENQUEUE_CODE=$(enqueue_message "$SID_1" "reply with exactly: ${MARKER_63_3}")
[[ "$ENQUEUE_CODE" == "202" ]] \
    || die "US-63.3: enqueue while busy returned HTTP ${ENQUEUE_CODE} (expected 202)"
ok "enqueue returned 202 (V2 admits without 409)"

# Wait for the long turn to finish.
wait $LONG_PID 2>/dev/null || true
ok "long turn completed"

# Now wait for the queued message to run (up to 60s).
log "  waiting for queued message to run (up to 60s)"
if wait_for_marker 60 "$SID_1" "$MARKER_63_3"; then
    ok "US-63.3 PASS: queued message ran after the in-flight turn"
else
    die "US-63.3 FAIL: queued message did not appear in history within 60s"
fi

# -----------------------------------------------------------------------------
# Phase 2: US-63.4 — abort mid-turn → queued messages survive and run
# -----------------------------------------------------------------------------
log "Phase 2 — US-63.4: abort mid-turn → queued messages survive"

SID_2=$(create_session)
[[ -n "$SID_2" ]] || die "failed to create session for US-63.4"
ok "session: ${SID_2}"

# Queue two messages.
MARKER_A="ACK_63_4_A_$(date +%s)"
MARKER_B="ACK_63_4_B_$(date +%s)"
log "  enqueueing two messages: A=${MARKER_A}, B=${MARKER_B}"

# Start a long turn to occupy the session.
(send_message "$SID_2" "write a 200-word essay about mountains" >/dev/null 2>&1) &
LONG_PID_2=$!
sleep 2

# Queue A and B while the session is busy.
ENQ_A=$(enqueue_message "$SID_2" "reply with exactly: ${MARKER_A}")
ENQ_B=$(enqueue_message "$SID_2" "reply with exactly: ${MARKER_B}")
[[ "$ENQ_A" == "202" && "$ENQ_B" == "202" ]] \
    || die "US-63.4: enqueue returned ${ENQ_A}/${ENQ_B} (expected 202/202)"
ok "both messages enqueued (202/202)"

# Abort mid-turn.
sleep 2  # let the long turn make progress
log "  aborting mid-turn"
ABORT_CODE=$(abort_session "$SID_2")
[[ "$ABORT_CODE" == "204" ]] \
    || die "US-63.4: abort returned HTTP ${ABORT_CODE} (expected 204)"
ok "abort returned 204 (non-destructive interrupt)"

# The long turn was interrupted. The queued messages should survive and run.
log "  waiting for queued messages A and B to run (up to 90s)"
if wait_for_marker 90 "$SID_2" "$MARKER_A" && wait_for_marker 30 "$SID_2" "$MARKER_B"; then
    ok "US-63.4 PASS: both queued messages ran after abort (non-destructive)"
else
    warn "queued messages may still be processing; checking once more..."
    if wait_for_marker 30 "$SID_2" "$MARKER_A" && wait_for_marker 30 "$SID_2" "$MARKER_B"; then
        ok "US-63.4 PASS: both queued messages ran after abort (delayed)"
    else
        die "US-63.4 FAIL: queued messages did not run after abort"
    fi
fi

# -----------------------------------------------------------------------------
# Phase 3: US-63.9 — kill opencode → restart → queued messages drain
# -----------------------------------------------------------------------------
log "Phase 3 — US-63.9: kill opencode → restart → queued messages drain"

SID_3=$(create_session)
[[ -n "$SID_3" ]] || die "failed to create session for US-63.9"
ok "session: ${SID_3}"

# Queue a message, then kill opencode mid-turn.
MARKER_63_9="ACK_63_9_$(date +%s)"

# Start a long turn + queue a marker.
(send_message "$SID_3" "write a 300-word essay about the moon" >/dev/null 2>&1) &
LONG_PID_3=$!
sleep 2
ENQ_C=$(enqueue_message "$SID_3" "reply with exactly: ${MARKER_63_9}")
[[ "$ENQ_C" == "202" ]] \
    || die "US-63.9: enqueue returned ${ENQ_C} (expected 202)"
ok "marker message enqueued: ${MARKER_63_9}"

# Kill opencode inside the pod. agentd will restart it in-place.
POD_NAME=$(kc get pod -l "llmsafespaces.dev/workspace=${WORKSPACE_NAME}" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
[[ -n "$POD_NAME" ]] || die "could not find pod for workspace ${WORKSPACE_NAME}"
ok "pod: ${POD_NAME}"

log "  killing opencode (simulating OOM)"
# The workspace pod's runtime container name is deployment-specific (observed
# as both "main" and "workspace"). Resolve it from the pod spec rather than
# hard-coding, then exec into it. Falls back to kubectl's default container.
RUNTIME_C=$(kc get pod "${POD_NAME}" -o jsonpath='{.spec.containers[0].name}' 2>/dev/null || true)
kc exec "${POD_NAME}" ${RUNTIME_C:+-c "${RUNTIME_C}"} -- sh -c 'pkill -9 -f "opencode serve" || pkill -9 -f opencode || true' \
    >/dev/null 2>&1 || warn "kill command returned non-zero"

# Wait for opencode to restart (health check).
log "  waiting for opencode to restart (up to 30s)"
HC="000"
for i in $(seq 1 15); do
    HC=$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 \
        "http://127.0.0.1:${PORTFWD_PORT}/livez" 2>/dev/null || echo "000")
    # The API livez doesn't tell us about opencode. We need the workspace
    # to be Active with a healthy pod. Check status instead.
    PHASE=$(kc get workspace "${WORKSPACE_NAME}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
    if [[ "$PHASE" == "Active" ]]; then
        sleep 5  # give agentd time to restart opencode + reconnect SSE
        ok "workspace Active after restart"
        break
    fi
    sleep 2
done

# Trigger an SSE reconnect by re-listing sessions (the proxy's SSE tracker
# will reconnect on the next event subscription).
log "  triggering SSE reconnect"
curl -sfm 5 \
    -H "Authorization: Bearer ${API_KEY}" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/sessions" \
    >/dev/null 2>&1 || true

# The wake should fire on reconnect. Wait for the marker to drain.
log "  waiting for stranded marker to drain (up to 120s)"
if wait_for_marker 120 "$SID_3" "$MARKER_63_9"; then
    ok "US-63.9 PASS: stranded queued message drained after restart"
else
    die "US-63.9 FAIL: stranded message did not drain after restart + reconnect"
fi

# -----------------------------------------------------------------------------
# Cleanup: disable V2 flag
# -----------------------------------------------------------------------------
log "Cleanup — disabling V2 flag"
kc set env deployment/llmsafespaces-api LLMSAFESPACES_V2_SESSION_QUEUE=false
kc rollout status deployment/llmsafespaces-api --timeout=120s || true

ok "all V2 behavioral assertions PASSED"
