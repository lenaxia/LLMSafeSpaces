#!/usr/bin/env bash
# Epic 72 / design 0061 §10 — the M2/M4 e2e migration story (owns #1548's
# AC1 + AC4):
#
#   R1 — THE MIGRATION SCENARIO (AC4, superseded text recorded in design
#        §10): credentials bound pre-flip are staged on the first
#        post-flip reconcile; the batch carries TOKENS; the FIRST
#        workspace-relay-* handoff appears; zero
#        relay_staging_not_ready / relay_fallback_delivery degrades.
#        (AC1's final clause — "the first handoff appears" — is the
#        same event.)
#   R2 — THE FALLBACK ARM (M2's headline behavior, the fail-open path):
#        delete the workspace's handoff Secret (staging torn) → the next
#        resync delivers the PRE-FLIP RAW-KEY batch (the provider key
#        readable in agent-config.json — the migration trade, DELIBERATE
#        and observable), relay_fallback_deliveries_total increments,
#        and CredentialsStaged=False/relay_fallback_delivery lands on
#        the Workspace CRD (M4's condition, wt-1453's seam).
#
# Environment: the standing post-flip install (relayOnlyKeyDelivery
# defaults ON since #1534; fallbackMode defaults migration since M2) —
# same conventions as local/us-72-relay-only-flip-drill.sh (see
# local/lib/us70-common.sh). First recorded execution rides the
# reviewer runner / the #1456 wiring lane (the standing disposition);
# standalone-runnable against any harness cluster.
#   CANARY_KEY - the planted key (default sk-M2E2E-CANARY-0fallback4row)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/us70-common.sh
source "${SCRIPT_DIR}/lib/us70-common.sh"

harness_start

CANARY_KEY="${CANARY_KEY:-sk-M2E2E-CANARY-0fallback4row}"
failures=0
note_fail() { failures=$((failures + 1)); warn "FAIL: $*"; }

# Per-script isolation (the #1342 pattern).
WS_BASE="e2e07250-0000-4000-8000-000000000000"
WS="$(ws_id 1)"

api() { # method path [json] -> dies on non-2xx
    local method="$1" path="$2" body="${3:-}" out code
    out=$(mktemp)
    if [[ -n "${body}" ]]; then
        code=$(curl -sm 30 -o "${out}" -w '%{http_code}' -X "${method}" \
            -H "Authorization: Bearer ${API_KEY}" \
            -H "Content-Type: application/json" -d "${body}" \
            "http://127.0.0.1:${PORTFWD_PORT}${path}" 2>/dev/null || code=000)
    else
        code=$(curl -sm 30 -o "${out}" -w '%{http_code}' -X "${method}" \
            -H "Authorization: Bearer ${API_KEY}" \
            "http://127.0.0.1:${PORTFWD_PORT}${path}" 2>/dev/null || code=000)
    fi
    [[ "${code}" == 2* ]] || die "api ${method} ${path}: HTTP ${code}: $(head -c 300 "${out}")"
    cat "${out}"; rm -f "${out}"
}

config_field() { # pod field -> the canary provider's rendered value (or "")
    local pod
    pod=$(pod_of "${WS}")
    [[ -n "${pod}" ]] || { echo ""; return; }
    kubectl --context "${CTX}" -n "${NS}" exec "${pod}" -c workspace -- bash -c '
        for p in /sandbox-runtime/agent-config.json /agentd-config/agent-config.json; do
            [[ -r "$p" ]] && cat "$p" && exit 0
        done
        echo "{}"
    ' 2>/dev/null | jq -r --arg f "$1" '.provider["m2e2e"].options[$f] // ""' || echo ""
}

condition_row() { # -> "STATUS REASON" of CredentialsStaged (or "None")
    (kc get workspace "${WS}" -o jsonpath='{.status.conditions}' 2>/dev/null || echo '[]') \
        | jq -r '[.[] | select(.type == "CredentialsStaged")][0] | (.status // "None") + " " + (.reason // "")' 2>/dev/null || echo "None "
}

# -----------------------------------------------------------------------------
log "setup — seed workspace (post-flip defaults), bind the canary credential"

seed_workspace "${WS}"
login_harness_user >/dev/null 2>&1 || true
[[ -n "${AUTH_TOKEN}" ]] || die "setup: harness JWT login failed"

CRED_ID=$(curl -sm 30 -X POST \
    -H "Authorization: Bearer ${AUTH_TOKEN}" \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"m2 e2e\",\"kind\":\"openai_compatible\",\"slug\":\"m2e2e\",\"apiKey\":\"${CANARY_KEY}\",\"baseURL\":\"http://mock-llm.${NS}.svc.cluster.local/v1\",\"modelAllowlist\":[\"mockmodel\"]}" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/provider-credentials" 2>/dev/null \
    | jq -r '.id // .credential.id // empty')
[[ -n "${CRED_ID}" ]] || die "setup: credential create returned no id"

bind_code=$(curl -sm 30 -o /dev/null -w '%{http_code}' -X POST \
    -H "Authorization: Bearer ${AUTH_TOKEN}" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/provider-credentials/${CRED_ID}/bind/${WS}" 2>/dev/null || echo 000)
[[ "${bind_code}" == 2* ]] || die "setup: bind failed: HTTP ${bind_code}"

wait_phase "${WS}" Active 300 || die "setup: workspace never Active"

# -----------------------------------------------------------------------------
log "R1 — the migration scenario: staged on first reconcile, TOKENS deliver, the first handoff appears"

# The controller staged on the first post-flip reconcile: the handoff
# Secret exists in the workspace namespace.
HANDOFF="workspace-relay-${WS}"
for _ in $(seq 1 60); do
    if kc get secret "${HANDOFF}" >/dev/null 2>&1; then break; fi
    sleep 4
done
if kc get secret "${HANDOFF}" >/dev/null 2>&1; then
    ok "R1: the FIRST ${HANDOFF} handoff appeared (staging armed and ran)"
else
    note_fail "R1: the handoff Secret never appeared — staging is not arming (the #1548 class)"
fi

# CredentialsStaged=True (the M4 condition, staged).
staged="None"
for _ in $(seq 1 60); do
    row="$(condition_row)"
    [[ "${row}" == "True "* ]] && break
    sleep 4
done
if [[ "$(condition_row)" == "True "* ]]; then
    ok "R1: CredentialsStaged=True"
else
    note_fail "R1: CredentialsStaged='$(condition_row)' (never True)"
fi

# The batch carries the TOKEN, not the raw key — POLLED (a boot batch
# predating staging leaves the raw canary standing until the resync
# applies; a single shot false-fails on the race).
key=""; base=""
for _ in $(seq 1 30); do
    key="$(config_field apiKey)"
    base="$(config_field baseURL)"
    [[ -n "${key}" && "${key}" != "${CANARY_KEY}" && "${base}" == *"llm-relay"* ]] && break
    sleep 4
done
if [[ -n "${key}" && "${key}" != "${CANARY_KEY}" ]]; then
    ok "R1: the batch carries the token (not the canary key)"
else
    note_fail "R1: apiKey is empty or the RAW canary on the token path — staging did not deliver"
fi
if [[ "${base}" == *"llm-relay"* ]]; then
    ok "R1: baseURL points at the router"
else
    note_fail "R1: baseURL '${base:-empty}' is not the router"
fi

# Zero degrades: the bootstrap log carries no relay degrade for this WS.
sleep 10 # let a resync settle
BOOTSTRAP_LOGS="$(kubectl --context "${CTX}" -n "${NS}" logs "deployment/llmsafespaces-api" --since=600s 2>/dev/null || true)"
DEGRADED=0
if printf '%s' "${BOOTSTRAP_LOGS}" | grep -F "relay_staging_not_ready" | grep -qF "${WS}"; then
    note_fail "R1: a relay_staging_not_ready degrade fired for this workspace (AC4's literal text — superseded by M2, still must NOT fire when staging is healthy)"
    DEGRADED=1
else
    ok "R1: zero relay_staging_not_ready degrades"
fi
# Design §10 requires BOTH reasons absent in the migration scenario: a
# boot-time fallback (staging armed late) could deliver the raw key
# while R1's later single-shot token read still passed on a stale
# config — the counter catches it.
PRE_METRICS="$(curl -sm 15 "http://127.0.0.1:${PORTFWD_PORT}/metrics" 2>/dev/null || true)"
if printf '%s' "${PRE_METRICS}" | grep -E "^relay_fallback_deliveries_total\{" | grep -qF "workspace=\"${WS}\""; then
    note_fail "R1: a relay_fallback_delivery already fired pre-tear — a boot-time fallback stands undetected by the token read"
    DEGRADED=1
else
    ok "R1: zero relay_fallback_delivery firings pre-tear"
fi

# -----------------------------------------------------------------------------
log "R2 — the fallback arm: tear the handoff → raw keys deliver, the counter increments, the condition flips"

kc delete secret "${HANDOFF}" >/dev/null 2>&1 || note_fail "R2: could not delete the handoff (already gone?)"

# Force a batch rebuild: the resync pull (suspend/activate is heavier;
# the secrets resync endpoint is the direct trigger — the drill's
# precedent). Fallback: suspend/activate.
POD=$(pod_of "${WS}")
PW=$(kc get secret "workspace-pw-${WS}" -o jsonpath='{.data.password}' 2>/dev/null | base64 -d 2>/dev/null || true)
RESYNC_OK=0
if [[ -n "${POD}" && -n "${PW}" ]]; then
    kubectl --context "${CTX}" -n "${NS}" port-forward "pod/${POD}" 18099:4097 >/dev/null 2>&1 &
    PF=$!
    sleep 3
    code=$(curl -sm 30 -o /dev/null -w '%{http_code}' -X POST \
        -u "opencode:${PW}" "http://127.0.0.1:18099/v1/resync-secrets" 2>/dev/null || echo 000)
    kill "${PF}" 2>/dev/null || true
    [[ "${code}" == 2* ]] && RESYNC_OK=1
fi
if (( RESYNC_OK == 0 )); then
    warn "R2: the direct resync failed — falling back to suspend/activate"
    api POST "/api/v1/workspaces/${WS}/suspend" >/dev/null
    sleep 5
    api POST "/api/v1/workspaces/${WS}/activate" >/dev/null
    wait_phase "${WS}" Active 300 || note_fail "R2: workspace never Active after the fallback cycle"
fi

# The batch now carries the RAW key (the migration trade — deliberate,
# counted, observable) — POLLED (the resync's apply is asynchronous).
key=""
for _ in $(seq 1 30); do
    key="$(config_field apiKey)"
    [[ "${key}" == "${CANARY_KEY}" ]] && break
    sleep 4
done
if [[ "${key}" == "${CANARY_KEY}" ]]; then
    ok "R2: the fallback delivered the RAW canary key (the pre-flip batch — the migration's availability half)"
else
    note_fail "R2: apiKey is not the raw canary ('${key:0:12}…') — the fallback did not deliver"
fi

# The counter DELTA — the pre-tear baseline was captured in R1
# (PRE_METRICS); a stale series from a prior run on a persistent
# harness cluster satisfies an existence grep without a fresh
# increment. Extract the baseline for THIS series, then compare.
pre_val=$(printf '%s' "${PRE_METRICS}" | grep -E "^relay_fallback_deliveries_total\{.*provider_slug=\"m2e2e\".*workspace=\"${WS}\"" | grep -oE '[0-9]+$' || echo 0)
METRICS="$(curl -sm 15 "http://127.0.0.1:${PORTFWD_PORT}/metrics" 2>/dev/null || true)"
post_val=$(printf '%s' "${METRICS}" | grep -E "^relay_fallback_deliveries_total\{.*provider_slug=\"m2e2e\".*workspace=\"${WS}\"" | grep -oE '[0-9]+$' || echo 0)
if (( post_val > pre_val )); then
    ok "R2: relay_fallback_deliveries_total{workspace=${WS},provider_slug=m2e2e} INCREMENTED (${pre_val} → ${post_val})"
else
    note_fail "R2: the counter did not increment (existence alone: ${pre_val} → ${post_val}) — a stale series is not a fresh delivery"
fi

# CredentialsStaged=False/relay_fallback_delivery (the M4 seam contract).
fell=0
for _ in $(seq 1 45); do
    if [[ "$(condition_row)" == "False relay_fallback_delivery" ]]; then fell=1; break; fi
    sleep 4
done
if (( fell )); then
    ok "R2: CredentialsStaged=False/relay_fallback_delivery on the CRD (the M4 condition, the seam contract)"
else
    note_fail "R2: CredentialsStaged='$(condition_row)' — the fallback outcome never surfaced on the CRD"
fi

# -----------------------------------------------------------------------------
if (( failures > 0 )); then
    die "${failures} row(s) failed"
fi
ok "all rows passed — the migration scenario and both delivery modes, live"
