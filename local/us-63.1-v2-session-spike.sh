#!/usr/bin/env bash
# US-63.1 Verification Spike — Confirm bundled opencode serves V2 session API.
#
# This is exploratory verification, NOT production code. It exercises the V2
# endpoints documented in Epic 63 / design/stories/epic-63-inboard-session-queue/
# against a live kind workspace pod and records the actual responses, status
# codes, and event-type strings into a spike report. The findings feed:
#   - US-63.2 (V2 client contract)        — response shapes
#   - US-63.5 (SSE event bridge)          — actual event type strings on /event
#   - US-63.9 (stranded-input recovery)   — OOM-restart drain behavior
#
# Prerequisites (assumed already up via local/bootstrap.sh + a workspace):
#   - kind cluster named $CLUSTER_NAME
#   - a Workspace in $NS at Phase=Active with a running pod
#   - kubectl on PATH pointed at kind-$CLUSTER_NAME
#   - curl on PATH
#   - optional: jq (for pretty-printing; script falls back to raw output)
#
# Usage:
#   ./local/us-63.1-v2-session-spike.sh [WORKSPACE_NAME]
#
#   Default WORKSPACE_NAME=e2e-workspace. Override if your test workspace has
#   a different name.
#
# Environment overrides:
#   CLUSTER_NAME   (default: llmsafespaces)
#   NS             (default: llmsafespaces)
#   CTX            (default: kind-$CLUSTER_NAME)
#   LLM_MODEL      (optional; default is read from the workspace's agent-config.json)
#   REPORT         (optional; default: worklogs/spike-us-63.1-report.md)
#
# Exit codes: 0 = spike ran to completion (regardless of pass/fail findings);
#             1 = setup/infra failure (no pod, no password, kubectl broken).
# Findings are recorded as PASS/FAIL/INFO in the report file. A FAIL finding
# is a real contract mismatch that blocks the epic; it does NOT fail this
# script's exit code, because the spike's job is to surface the truth.
set -Eeuo pipefail

if [[ -t 1 ]]; then
    BOLD=$'\033[1m'; DIM=$'\033[2n'; RED=$'\033[31m'; GREEN=$'\033[32m'
    YELLOW=$'\033[33m'; CYAN=$'\033[36m'; RESET=$'\033[0m'
else
    BOLD=''; DIM=''; RED=''; GREEN=''; YELLOW=''; CYAN=''; RESET=''
fi
log()  { printf '%s==>%s %s\n' "${CYAN}${BOLD}" "${RESET}" "$*"; }
ok()   { printf '%s ✓%s %s\n' "${GREEN}" "${RESET}" "$*"; }
warn() { printf '%s !%s %s\n' "${YELLOW}" "${RESET}" "$*" >&2; }
die()  { printf '%s ✗%s %s\n' "${RED}${BOLD}" "${RESET}" "$*" >&2; exit 1; }

CLUSTER_NAME="${CLUSTER_NAME:-llmsafespaces}"
CTX="${CTX:-kind-${CLUSTER_NAME}}"
NS="${NS:-llmsafespaces}"
WS_NAME="${1:-e2e-workspace}"
REPORT="${REPORT:-worklogs/spike-us-63.1-report.md}"
PORT_OPENCODE=4096
PORT_AGENTD_ADMIN=4098
AUTH_USER="opencode"

command -v kubectl >/dev/null || die "kubectl not on PATH"
command -v curl    >/dev/null || die "curl not on PATH"
HAS_JQ=0; command -v jq >/dev/null 2>&1 && HAS_JQ=1

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

mkdir -p "$(dirname "${REPORT}")"

# -----------------------------------------------------------------------------
# Phase 1: locate the workspace, pod, pod IP, and password
# -----------------------------------------------------------------------------
log "Phase 1 — locate workspace '${WS_NAME}'"

PHASE="$(kubectl --context "${CTX}" -n "${NS}" get workspace "${WS_NAME}" \
    -o 'jsonpath={.status.phase}' 2>/dev/null || true)"
[[ -n "${PHASE}" ]] || die "workspace ${WS_NAME} not found in ns ${NS}"
ok "workspace phase=${PHASE}"
[[ "${PHASE}" == "Active" ]] || die "workspace not Active (phase=${PHASE}); resume it first"

POD_IP="$(kubectl --context "${CTX}" -n "${NS}" get workspace "${WS_NAME}" \
    -o 'jsonpath={.status.podIP}' 2>/dev/null || true)"
[[ -n "${POD_IP}" ]] || die "no status.podIP on workspace ${WS_NAME}"
ok "pod IP=${POD_IP}"

# Password lives in Secret workspace-pw-<ws-name>, key 'password'
# (see controller/internal/workspace/constants.go:71 passwordSecretName).
PW_SECRET="workspace-pw-${WS_NAME}"
PASSWORD="$(kubectl --context "${CTX}" -n "${NS}" get secret "${PW_SECRET}" \
    -o 'jsonpath={.data.password}' 2>/dev/null | base64 -d 2>/dev/null || true)"
[[ -n "${PASSWORD}" ]] || die "no password in Secret ${PW_SECRET}/password"
ok "password retrieved (${#PASSWORD} chars)"

# Verify pod is reachable by hitting the V1 health endpoint first.
log "Phase 1b — sanity: V1 healthz on the pod"
HTTP_CODE="$(curl -s -o /dev/null -w '%{http_code}' \
    --max-time 5 "http://${POD_IP}:${PORT_OPENCODE}/global/health" || true)"
if [[ "${HTTP_CODE}" == "200" ]]; then
    ok "V1 health reachable (200)"
else
    die "V1 /global/health on ${POD_IP}:${PORT_OPENCODE} returned ${HTTP_CODE}; pod not ready for V2 spike"
fi

# -----------------------------------------------------------------------------
# Helpers
# -----------------------------------------------------------------------------
SPIKE_DATE="$(date -u +%Y-%m-%d)"
SPIKE_LOG="$(mktemp)"
trap 'rm -f "${SPIKE_LOG}"' EXIT

# report writes a markdown line into the report file.
report() {
    # $1 = section, $2 = body (may be multiline)
    printf '### %s\n\n%s\n\n' "$1" "$2" >> "${REPORT}"
}
report_kv() {
    # $1 = key, $2 = value
    printf -- '- **%s:** `%s`\n' "$1" "$2" >> "${REPORT}"
}

# req records a curl request+response. $1=label, rest=args to curl (excluding auth+url)
req() {
    local label="$1"; shift
    local url="$1"; shift
    {
        echo "==== ${label} ===="
        echo "URL: ${url}"
        echo "ARGS: $*"
        echo "--- request body (if any) ---"
    } >> "${SPIKE_LOG}"
    local body_file status_file hdrs_file
    body_file="$(mktemp)"; status_file="$(mktemp)"; hdrs_file="$(mktemp)"
    curl -sS \
        -u "${AUTH_USER}:${PASSWORD}" \
        -w "\n__HTTP_CODE=%{http_code}\n" \
        -D "${hdrs_file}" \
        --max-time 30 \
        "$@" "${url}" \
        > "${body_file}" 2>&1 || true
    local code
    code="$(grep -oE '__HTTP_CODE=[0-9]+' "${body_file}" | tail -1 | cut -d= -f2 || echo '?')"
    sed -i '/__HTTP_CODE=/d' "${body_file}"
    {
        echo "--- response status: ${code} ---"
        echo "--- response headers ---"
        cat "${hdrs_file}"
        echo "--- response body ---"
        if [[ "${HAS_JQ}" == "1" ]] && head -c1 "${body_file}" | grep -q '{'; then
            jq . "${body_file}" 2>/dev/null || cat "${body_file}"
        else
            cat "${body_file}"
        fi
        echo ""
    } >> "${SPIKE_LOG}"
    rm -f "${body_file}" "${status_file}" "${hdrs_file}"
    echo "${code}"
}

# -----------------------------------------------------------------------------
# Initialize report
# -----------------------------------------------------------------------------
cat > "${REPORT}" <<EOF
# US-63.1 Spike Report — V2 Session API Verification

**Date:** ${SPIKE_DATE}
**Workspace:** ${WS_NAME} (ns ${NS})
**Pod IP:** ${POD_IP}
**opencode port:** ${PORT_OPENCODE}
**Auth:** Basic \`opencode:<password>\` (Secret \`workspace-pw-${WS_NAME}/password\`)
**Raw request/response log:** appended to this file (search "Raw HTTP capture").

> This report is the empirical record for Epic 63. Every claim below was
> observed against a live pod; nothing is inferred from code reading.

EOF

# -----------------------------------------------------------------------------
# Phase 2: V2 session creation + prompt (idle path)
# -----------------------------------------------------------------------------
log "Phase 2 — V2 prompt (idle session)"

# We need a session ID. Try the V2 create endpoint first; if the workspace
# already has sessions, fall back to listing.
SID="$(curl -sS -u "${AUTH_USER}:${PASSWORD}" --max-time 10 \
    -X POST "http://${POD_IP}:${PORT_OPENCODE}/api/session" \
    -H 'Content-Type: application/json' \
    -d '{"title":"us-63.1-spike"}' \
    | { ${HAS_JQ:+jq -r .id 2>/dev/null} || true; } \
    || true)"
if [[ -z "${SID}" || "${SID}" == "null" ]]; then
    warn "V2 POST /api/session did not return an id; trying V1 fallback"
    SID="$(curl -sS -u "${AUTH_USER}:${PASSWORD}" --max-time 10 \
        -X POST "http://${POD_IP}:${PORT_OPENCODE}/session" \
        -H 'Content-Type: application/json' -d '{"title":"us-63.1-spike"}' \
        | { ${HAS_JQ:+jq -r .id 2>/dev/null} || true; } || true)"
fi
[[ -n "${SID}" && "${SID}" != "null" ]] || die "could not obtain a session id (V2 or V1)"
ok "session id=${SID}"
report_kv "Session ID" "${SID}"

# Idle-path prompt with delivery:queue.
IDLE_CODE="$(req "V2 prompt idle, delivery=queue" \
    "http://${POD_IP}:${PORT_OPENCODE}/api/session/${SID}/prompt" \
    -X POST -H 'Content-Type: application/json' \
    -d "{\"prompt\":{\"parts\":[{\"type\":\"text\",\"text\":\"reply with the single word: ack\"}]},\"delivery\":\"queue\"}")"
report "Idle-path V2 prompt (delivery:queue)" \
    "HTTP ${IDLE_CODE}. If 2xx: queue-admit works on an idle session (US-63.3 happy path confirmed). See raw log for body."

# Wait briefly for the turn to start, then mark the session as "busy" for the
# next phase. We poll statusz.
sleep 2

# -----------------------------------------------------------------------------
# Phase 3: V2 prompt while busy (the load-bearing 409 question)
# -----------------------------------------------------------------------------
log "Phase 3 — V2 prompt while turn in flight (the 409 question)"
warn "this phase races the in-flight turn; timing-sensitive"

# Fire a long prompt (ask for a paragraph) to occupy the session...
LONG_BODY='{"prompt":{"parts":[{"type":"text","text":"write a 200-word essay about the ocean"}]},"delivery":"queue"}'
req "V2 prompt A (long, to occupy)" \
    "http://${POD_IP}:${PORT_OPENCODE}/api/session/${SID}/prompt" \
    -X POST -H 'Content-Type: application/json' -d "${LONG_BODY}" >/dev/null
sleep 1   # let opencode pick up the prompt

# ...then enqueue B while A is in flight. The expected behavior is admission
# without error (V2 queue). A 409 here would contradict F2/F4 and BLOCK US-63.3.
BUSY_CODE="$(req "V2 prompt B (queued while A busy)" \
    "http://${POD_IP}:${PORT_OPENCODE}/api/session/${SID}/prompt" \
    -X POST -H 'Content-Type: application/json' \
    -d '{"prompt":{"parts":[{"type":"text","text":"reply with: ack2"}]},"delivery":"queue"}')"
report "Busy-path V2 prompt (delivery:queue)" \
    "HTTP ${BUSY_CODE}. PASS=2xx (admits without 409, US-63.3 confirmed). FAIL=409 (V2 not honoring queue admission; epic BLOCKED)."

# -----------------------------------------------------------------------------
# Phase 4: V2 interrupt — non-destructive
# -----------------------------------------------------------------------------
log "Phase 4 — V2 interrupt (non-destructive)"

# Give A a moment to make progress, then interrupt.
sleep 2
INT_CODE="$(req "V2 interrupt (in-flight)" \
    "http://${POD_IP}:${PORT_OPENCODE}/api/session/${SID}/interrupt" \
    -X POST -H 'Content-Type: application/json' -d '{}')"
report "Interrupt in-flight turn" \
    "HTTP ${INT_CODE}. Per F8 the queued message B must STILL run after interrupt (non-destructive). Verify in Phase 6 events."

# Interrupt on idle session — record whether no-op-success or error.
sleep 1
IDLE_INT_CODE="$(req "V2 interrupt (idle session)" \
    "http://${POD_IP}:${PORT_OPENCODE}/api/session/${SID}/interrupt" \
    -X POST -H 'Content-Type: application/json' -d '{}')"
report "Interrupt idle session" \
    "HTTP ${IDLE_INT_CODE}. Proxy should treat both 2xx and 4xx as success (US-63.4); record which we got."

# -----------------------------------------------------------------------------
# Phase 5: SSE event capture (the US-63.5 input)
# -----------------------------------------------------------------------------
log "Phase 5 — SSE /event capture (10s window)"
SSE_FILE="$(mktemp)"
# Subscribe in background, capture for 10s, then read the file.
( curl -sS -N -u "${AUTH_USER}:${PASSWORD}" --max-time 12 \
    "http://${POD_IP}:${PORT_OPENCODE}/event" > "${SSE_FILE}" 2>&1 || true ) &
SSE_PID=$!
# Trigger one more prompt to generate events while subscribed.
sleep 1
req "V2 prompt C (to generate SSE events)" \
    "http://${POD_IP}:${PORT_OPENCODE}/api/session/${SID}/prompt" \
    -X POST -H 'Content-Type: application/json' \
    -d '{"prompt":{"parts":[{"type":"text","text":"reply with: ack3"}]},"delivery":"queue"}' >/dev/null
wait "${SSE_PID}" 2>/dev/null || true

# Extract the event-type strings observed (line looks like: event: <type>).
EVENT_TYPES="$(grep -E '^event:' "${SSE_FILE}" | sort -u | sed 's/^event: */  - /' || echo '  (none observed)')"
EVENT_COUNT="$(grep -cE '^event:' "${SSE_FILE}" || echo 0)"
report "SSE event capture" \
    "Observed ${EVENT_COUNT} event(s) in 10s. Event type strings:
${EVENT_TYPES}

PASS if the list includes V2 admission/promotion types (per F14: \`session.next.prompt.admitted\`, \`session.next.prompted\`).
FAIL if those strings do NOT appear — US-63.5 must bridge from a different event source. Record the actual strings."

# Save raw SSE for the report appendices.
{
    echo ""
    echo "## Raw SSE capture (Phase 5)"
    echo ""
    echo '```'
    head -c 8000 "${SSE_FILE}"
    echo ""
    echo '```'
} >> "${REPORT}"
rm -f "${SSE_FILE}"

# -----------------------------------------------------------------------------
# Phase 6: OOM-restart drain (the US-63.9 input — load-bearing)
# -----------------------------------------------------------------------------
log "Phase 6 — OOM-restart drain (LOAD-BEARING for US-63.9)"
warn "this phase kills the opencode process inside the pod"

# Queue a message while busy (with a fresh long prompt), then SIGKILL opencode.
req "V2 prompt D (long, before OOM)" \
    "http://${POD_IP}:${PORT_OPENCODE}/api/session/${SID}/prompt" \
    -X POST -H 'Content-Type: application/json' \
    -d '{"prompt":{"parts":[{"type":"text","text":"write a 300-word essay about the moon"}]},"delivery":"queue"}' >/dev/null
sleep 1
req "V2 prompt E (queued before OOM, should drain after restart)" \
    "http://${POD_IP}:${PORT_OPENCODE}/api/session/${SID}/prompt" \
    -X POST -H 'Content-Type: application/json' \
    -d '{"prompt":{"parts":[{"type":"text","text":"reply with: ack-oom-restart"}]},"delivery":"queue"}' >/dev/null
sleep 1

# Identify the pod and SIGKILL opencode (PID via pgrep). agentd will restart it.
POD_NAME="$(kubectl --context "${CTX}" -n "${NS}" get pod -l \
    "llmsafespaces.dev/workspace=${WS_NAME}" \
    -o 'jsonpath={.items[0].metadata.name}' 2>/dev/null || true)"
[[ -n "${POD_NAME}" ]] || die "could not find pod for workspace ${WS_NAME}"
ok "pod name=${POD_NAME}"

log "  killing opencode process in ${POD_NAME} (agentd will restart it)"
kubectl --context "${CTX}" -n "${NS}" exec "${POD_NAME}" -c main -- \
    sh -c 'pkill -9 -f "opencode serve" || pkill -9 -f opencode || true' \
    >/dev/null 2>&1 || warn "kill command returned non-zero (may already have exited)"

# Wait for agentd to notice and restart opencode (~5-15s based on prior worklogs).
log "  waiting up to 30s for opencode to restart"
for i in $(seq 1 15); do
    HC="$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 \
        "http://${POD_IP}:${PORT_OPENCODE}/global/health" || echo 000)"
    [[ "${HC}" == "200" ]] && { ok "opencode back at ${i}*2s (health=200)"; break; }
    sleep 2
done
[[ "${HC}" == "200" ]] || warn "opencode did not return to health=200 in 30s; pod IP may have changed"

# Re-read pod IP in case the pod was restarted (in-place vs full restart).
NEW_POD_IP="$(kubectl --context "${CTX}" -n "${NS}" get workspace "${WS_NAME}" \
    -o 'jsonpath={.status.podIP}' 2>/dev/null || true)"
if [[ -n "${NEW_POD_IP}" && "${NEW_POD_IP}" != "${POD_IP}" ]]; then
    warn "pod IP changed: ${POD_IP} → ${NEW_POD_IP}; using new IP for drain check"
    POD_IP="${NEW_POD_IP}"
fi

# Capture 30s of SSE to see if the queued message E drains on its own.
log "  30s SSE capture to detect autonomous drain of queued message E"
DRAIN_FILE="$(mktemp)"
( curl -sS -N -u "${AUTH_USER}:${PASSWORD}" --max-time 32 \
    "http://${POD_IP}:${PORT_OPENCODE}/event" > "${DRAIN_FILE}" 2>&1 || true ) &
DRAIN_PID=$!
# We do NOT send a new prompt — the question is whether E drains WITHOUT one.
wait "${DRAIN_PID}" 2>/dev/null || true

if grep -q "ack-oom-restart" "${DRAIN_FILE}" || \
   grep -qE 'session.next.prompt(ed|admitted)' "${DRAIN_FILE}"; then
    DRAIN_VERDICT="PASS — queued input drained autonomously after restart. US-63.9 can accept the status quo (no upstream resume endpoint needed)."
else
    DRAIN_VERDICT="FAIL (expected outcome per F16) — queued input did NOT drain autonomously. US-63.9 MUST ship a drain trigger (upstream resume endpoint OR proxy-side wake) before US-63.7 deletes the stranded-queue sweep."
fi
report "OOM-restart drain behavior (US-63.9 input)" "${DRAIN_VERDICT}"

{
    echo ""
    echo "## Raw SSE capture (Phase 6 — post-restart drain watch, 30s)"
    echo ""
    echo '```'
    head -c 8000 "${DRAIN_FILE}"
    echo ""
    echo '```'
} >> "${REPORT}"
rm -f "${DRAIN_FILE}"

# -----------------------------------------------------------------------------
# Phase 7: append the raw HTTP capture and finalize
# -----------------------------------------------------------------------------
{
    echo ""
    echo "## Raw HTTP capture"
    echo ""
    echo '```'
    cat "${SPIKE_LOG}"
    echo '```'
} >> "${REPORT}"

ok "spike complete; report at ${REPORT}"
echo ""
echo "${BOLD}Next step:${RESET} read ${REPORT} and update Epic 63 findings F1–F17 in"
echo "design/stories/epic-63-inboard-session-queue/README.md with the empirical results."
