#!/usr/bin/env bash
# Epic 72 US-72.6 — the rogue-agent sweep (design 0058 §8, #820 close-out).
#
#   R1 — THE EXIT-CRITERION SWEEP: a post-flip pod with a bound CANARY
#        credential. Exec in as the workspace uid and grep EVERY
#        uid-1000-readable surface (the design 0058 §1.2 inventory +
#        /proc/*/environ) for the canary's bytes. ZERO hits is the
#        epic's exit criterion: K1 — no provider-key plaintext in
#        uid-1000-reachable space.
#   R2 — THE POSITIVE CONTROL + THE SCRUB: plant the canary in the
#        PRE-US-35.7 legacy shape (a REGULAR FILE at
#        /workspace/.local/opencode/auth.json) → the sweep FINDS it
#        (proving the sweep can fail) → exec the agentd
#        scrub-legacy-keys subcommand in-pod → the sweep reads ZERO and
#        the LegacyKeysScrubbed condition appears on the Workspace.
#
# Environment: the standing post-flip install (relayOnlyKeyDelivery
# defaults on since US-72.5); same conventions as
# local/us-72-relay-only-flip-drill.sh (see local/lib/us70-common.sh).
# The nightly wiring rides the #1456 lane; standalone-runnable against
# any harness cluster.
#   CANARY_KEY - the planted key (default sk-US72-SWEEP-0canary7ledger)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/us70-common.sh
source "${SCRIPT_DIR}/lib/us70-common.sh"

harness_start

CANARY_KEY="${CANARY_KEY:-sk-US72-SWEEP-0canary7ledger}"
failures=0
note_fail() { failures=$((failures + 1)); warn "FAIL: $*"; }

api() { # method path [json-body] -> response body (dies on non-2xx)
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
    if [[ "${code}" != 2* ]]; then
        die "api ${method} ${path}: HTTP ${code}: $(head -c 300 "${out}")"
    fi
    cat "${out}"; rm -f "${out}"
}

# Per-script isolation (the #1342 pattern).
WS_BASE="e2e72600-0000-4000-8000-000000000000"
WS="$(ws_id 1)"

pod_of_ws() { pod_of "${WS}"; }

# THE SWEEP (the drill's sweep_hits, widened): grep every readable
# surface for the canary. Counted NUMERIC outputs only (grep -c prints 0
# and exits 1 on the healthy case); unreadable paths are the boundary
# working. /proc/*/environ for EVERY pod process (the story's rogues'
# gallery). A transport failure counts as a hit (cannot prove clean).
sweep_hits() {
    local pod
    pod=$(pod_of_ws)
    [[ -n "${pod}" ]] || { echo 1; return; }
    kubectl --context "${CTX}" -n "${NS}" exec "${pod}" -c workspace -- bash -c '
        hits=0
        for p in /sandbox-runtime/agent-config.json \
                 /agentd-config/agent-config.json \
                 /workspace/.local/opencode/auth.json \
                 /sandbox-cfg/secrets.json \
                 /sandbox-runtime/rt/secrets.json \
                 /sandbox-runtime/rt/auth.json; do
            out=$(grep -ac "'"${CANARY_KEY}"'" "$p" 2>/dev/null || true)
            [[ "${out}" =~ ^[0-9]+$ ]] && hits=$((hits + out))
        done
        for env in /proc/[0-9]*/environ; do
            out=$(grep -ac "'"${CANARY_KEY}"'" "$env" 2>/dev/null || true)
            [[ "${out}" =~ ^[0-9]+$ ]] && hits=$((hits + out))
        done
        echo "${hits}"
    ' 2>/dev/null | tail -1 || echo 1
}

condition_status() { # cond-type -> status (None when absent)
    (kc get workspace "${WS}" -o jsonpath='{.status.conditions}' 2>/dev/null \
        || echo '[]') | jq -r --arg t "$1" '[.[] | select(.type == $t)][0].status // "None"'
}

# -----------------------------------------------------------------------------
log "setup — seed workspace (post-flip install), bind the canary credential"

seed_workspace "${WS}"
login_harness_user >/dev/null 2>&1 || true
[[ -n "${AUTH_TOKEN}" ]] || die "setup: harness JWT login failed"

CRED_ID=$(curl -sm 30 -X POST \
    -H "Authorization: Bearer ${AUTH_TOKEN}" \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"us72 sweep\",\"kind\":\"openai_compatible\",\"slug\":\"us72sweep\",\"apiKey\":\"${CANARY_KEY}\",\"baseURL\":\"http://mock-llm.${NS}.svc.cluster.local/v1\",\"modelAllowlist\":[\"mockmodel\"]}" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/provider-credentials" 2>/dev/null \
    | jq -r '.id // .credential.id // empty')
[[ -n "${CRED_ID}" ]] || die "setup: credential create returned no id"

bind_code=$(curl -sm 30 -o /dev/null -w '%{http_code}' -X POST \
    -H "Authorization: Bearer ${AUTH_TOKEN}" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/provider-credentials/${CRED_ID}/bind/${WS}" 2>/dev/null || echo 000)
[[ "${bind_code}" == 2* ]] || die "setup: bind failed: HTTP ${bind_code}"

wait_phase "${WS}" Active 300 || die "setup: workspace never Active"

# -----------------------------------------------------------------------------
log "R1 — the exit-criterion sweep: zero canary bytes in uid-1000 space"

hits="$(sweep_hits)"
if [[ "${hits}" =~ ^[0-9]+$ ]] && (( hits == 0 )); then
    ok "R1 PASS: K1 holds — the canary key is NOWHERE the agent can read"
else
    note_fail "R1: canary found ${hits:-?} time(s) — K1 violated (the epic's exit criterion)"
fi

# -----------------------------------------------------------------------------
log "R2 — positive control: plant the LEGACY shape, find it, scrub it, find nothing"

POD=$(pod_of_ws)
[[ -n "${POD}" ]] || die "R2: no pod"

# Plant the pre-US-35.7 legacy residue: a REGULAR FILE at the auth path.
kubectl --context "${CTX}" -n "${NS}" exec "${POD}" -c workspace -- bash -c '
    mkdir -p /workspace/.local/opencode
    printf "{\"legacy\": {\"type\": \"api\", \"key\": \"%s\"}}" "'"${CANARY_KEY}"'" \
        > /workspace/.local/opencode/auth.json
' >/dev/null 2>&1 || die "R2: planting the legacy residue failed"

hits="$(sweep_hits)"
if [[ "${hits}" =~ ^[0-9]+$ ]] && (( hits >= 1 )); then
    ok "R2 PASS (positive control): the sweep FINDS the planted legacy canary — the sweep can fail"
else
    note_fail "R2: the planted legacy canary was NOT found — the sweep is decorative (a false-pass machine)"
fi

# Scrub: the agentd subcommand, in-pod.
kubectl --context "${CTX}" -n "${NS}" exec "${POD}" -c workspace -- \
    /agentd/usr/local/bin/workspace-agentd scrub-legacy-keys --workspace-root /workspace \
    >/dev/null 2>&1 || note_fail "R2: the scrub subcommand exited non-zero"

hits="$(sweep_hits)"
if [[ "${hits}" =~ ^[0-9]+$ ]] && (( hits == 0 )); then
    ok "R2 PASS: post-sweep zero — the legacy residue is gone"
else
    note_fail "R2: canary still present ${hits:-?} time(s) after the scrub"
fi

# The scrub's condition mirror (the report rides statusz → the
# controller; give the reconcile a beat).
scrub_status="None"
for _ in $(seq 1 30); do
    scrub_status="$(condition_status LegacyKeysScrubbed)"
    [[ "${scrub_status}" != "None" ]] && break
    sleep 4
done
if [[ "${scrub_status}" == "True" ]]; then
    ok "LegacyKeysScrubbed=True on the Workspace (the statusz mirror landed)"
else
    note_fail "R2: LegacyKeysScrubbed=${scrub_status} (never observed)"
fi

# -----------------------------------------------------------------------------
if (( failures > 0 )); then
    die "${failures} row(s) failed"
fi
ok "all rows passed — the rogue agent finds nothing (K1, #820's exit criterion)"
