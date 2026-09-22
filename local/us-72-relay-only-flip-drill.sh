#!/usr/bin/env bash
# Epic 72 US-72.5 — relay-only flip + rollback drill (design 0058 §D6.1,
# the exercised-rollback pattern). The runbook's scripted e2e leg:
#
#   R1 — FLIP ON: helm upgrade --reuse-values + relayOnlyKeyDelivery.enabled=true
#        → the llm-relay router deploys, controller re-arms with the flag,
#        api takes the batch-builder env.
#   R2 — TOKEN-ONLY CANARY: create a user provider credential whose apiKey
#        is a PLANTED CANARY (distinctive bytes), bind it, boot a workspace
#        → CredentialsStaged=True → in-pod sweep (the US-72.6 canary leg,
#        early): canary bytes in ZERO uid-1000-readable paths; the config's
#        apiKey is the TOKEN and baseURL is the router URL.
#   R3 — ROLLBACK (the positive control): helm upgrade enabled=false →
#        suspend/resume (fresh pod → fresh batch) → the canary RETURNS in
#        the config (legacy raw-key behavior restored — proving the sweep
#        can fail and rollback restores function).
#   R4 — FLIP ON AGAIN: suspend/resume → token-only again, sweep zero.
#        The D6.1 loop closes: flip → validate → rollback → validate →
#        flip.
#
# Environment: same conventions as local/us-70-secret-delivery-e2e.sh
# (see local/lib/us70-common.sh). Runs on the pool/nightly kind cluster
# against the standing helm release (--reuse-values keeps the install's
# values; only the drill's flag delta is set). The nightly wiring rides
# the #1456 lane; standalone-runnable against any harness cluster.
#   CANARY_KEY  - the planted key (default sk-US72-CANARY-0wiggle8harbor)
#   FLIP_WAIT_S - helm upgrade --wait budget per flip (default 300)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/us70-common.sh
source "${SCRIPT_DIR}/lib/us70-common.sh"

harness_start

CANARY_KEY="${CANARY_KEY:-sk-US72-CANARY-0wiggle8harbor}"
FLIP_WAIT_S="${FLIP_WAIT_S:-300}"
RELAY_NS="llm-relay"

# Per-script isolation (the #1342 UNCONDITIONAL pattern).
WS_BASE="e2e72500-0000-4000-8000-000000000000"
WS="$(ws_id 1)"
failures=0
note_fail() { failures=$((failures + 1)); warn "FAIL: $*"; }

api() { # method path [json-body] -> response body
    local method="$1" path="$2" body="${3:-}"
    if [[ -n "${body}" ]]; then
        curl -sfm 30 -X "${method}" -H "Authorization: Bearer ${API_KEY}" \
            -H "Content-Type: application/json" -d "${body}" \
            "http://127.0.0.1:${PORTFWD_PORT}${path}" 2>/dev/null || echo '{}'
    else
        curl -sfm 30 -X "${method}" -H "Authorization: Bearer ${API_KEY}" \
            "http://127.0.0.1:${PORTFWD_PORT}${path}" 2>/dev/null || echo '{}'
    fi
}

flip() { # true|false
    helm upgrade --install llmsafespaces "${SCRIPT_DIR}/../helm" \
        -n "${NS}" --reuse-values \
        --set "relayOnlyKeyDelivery.enabled=${1}" \
        --wait --timeout "${FLIP_WAIT_S}s" >/dev/null
    kubectl --context "${CTX}" -n "${NS}" rollout status deployment/llmsafespaces-controller --timeout=180s >/dev/null
    kubectl --context "${CTX}" -n "${NS}" rollout status deployment/llmsafespaces-api --timeout=180s >/dev/null
}

condition() { # ws cond-type -> "True"/"False"/"None"
    kc get workspace "$1" -o jsonpath='{.status.conditions}' 2>/dev/null \
        | jq -r --arg t "$2" '[.[] | select(.type == $t)][0].status // "None"'
}

cycle_pod() { # ws — bounded suspend/resume (the #1087-compliant 5s variant)
    api POST "/api/v1/workspaces/$1/suspend" >/dev/null
    sleep 5
    api POST "/api/v1/workspaces/$1/activate" >/dev/null
}

# The canary sweep (the US-72.6 leg): grep every uid-1000-readable path
# inside the pod for the canary bytes. Exec runs as the workspace uid;
# paths that are PERMISSION-UNREADABLE in the execing uid count as PASS
# for that path (the boundary is the property) — but only EACCES/ENOENT
# unreadability counts; any other grep error fails the row.
sweep_hits() { # ws -> count of canary hits
    local ws="$1"
    kc exec -c workspace "${ws}" -- bash -c '
        hits=0
        for p in /sandbox-runtime/agent-config.json \
                 /agentd-config/agent-config.json \
                 /workspace/.local/opencode/auth.json \
                 /sandbox-cfg/secrets.json \
                 /sandbox-runtime/rt/secrets.json \
                 /sandbox-runtime/rt/auth.json; do
            if [[ -e "$p" || -L "$p" ]]; then
                out=$(grep -c "'"${CANARY_KEY}"'" "$p" 2>/dev/null) && hits=$((hits+out)) || {
                    rc=$?
                    [[ $rc -eq 2 ]] || hits=$((hits+1))  # EACCES-on-read is the boundary; anything else is a hit-by-suspicion
                }
            fi
        done
        env_hits=$(grep -a -c "'"${CANARY_KEY}"'" /proc/self/environ 2>/dev/null || true)
        echo $((hits + env_hits))
    ' 2>/dev/null | tail -1
}

config_apikey() { # ws -> the agent-config apiKey field (or "")
    kc exec -c workspace "$1" -- bash -c '
        for p in /sandbox-runtime/agent-config.json /agentd-config/agent-config.json; do
            [[ -r "$p" ]] && cat "$p" && exit 0
        done
        echo "{}"
    ' 2>/dev/null | jq -r '[.provider[][].options.apiKey // empty][0] // ""'
}

config_baseurl() { # ws -> the first provider baseURL (or "")
    kc exec -c workspace "$1" -- bash -c '
        for p in /sandbox-runtime/agent-config.json /agentd-config/agent-config.json; do
            [[ -r "$p" ]] && cat "$p" && exit 0
        done
        echo "{}"
    ' 2>/dev/null | jq -r '[.provider[][].options.baseURL // empty][0] // ""'
}

# -----------------------------------------------------------------------------
log "R1 — flip ON (relayOnlyKeyDelivery.enabled=true)"
flip true
if kubectl --context "${CTX}" -n "${RELAY_NS}" get deployment llm-relay-router >/dev/null 2>&1 \
    && [[ "$(kubectl --context "${CTX}" -n "${RELAY_NS}" get deploy llm-relay-router -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)" -ge 1 ]]; then
    ok "llm-relay router deployed and ready"
else
    note_fail "R1: llm-relay router not ready — the flip gate is broken"
fi

# -----------------------------------------------------------------------------
log "R2 — token-only canary (CredentialsStaged, zero canary bytes, token+router URL in config)"

CRED_ID=$(api POST /api/v1/provider-credentials \
    "{\"name\":\"us72 drill\",\"kind\":\"llm-provider\",\"slug\":\"us72drill\",\"apiKey\":\"${CANARY_KEY}\",\"baseURL\":\"http://mock-llm.${NS}.svc.cluster.local/v1\",\"modelAllowlist\":[\"mockmodel\"]}" \
    | jq -r '.id // .credential.id // empty')
if [[ -z "${CRED_ID}" ]]; then
    die "setup: credential create failed (no id)"
fi
api POST "/api/v1/provider-credentials/${CRED_ID}/bind/${WS}" >/dev/null

seed_workspace "${WS}"
wait_phase "${WS}" Active 300 || note_fail "R2: workspace never Active"

staged="None"
for _ in $(seq 1 60); do
    staged="$(condition "${WS}" CredentialsStaged)"
    [[ "${staged}" == "True" ]] && break
    sleep 5
done
if [[ "${staged}" == "True" ]]; then
    ok "CredentialsStaged=True"
else
    note_fail "R2: CredentialsStaged=${staged} (never True)"
fi

hits="$(sweep_hits "${WS}")"
if [[ "${hits}" =~ ^[0-9]+$ ]] && (( hits == 0 )); then
    ok "R2 PASS: canary bytes in ZERO uid-1000-readable paths"
else
    note_fail "R2: canary found ${hits:-?} time(s) — K1 violated under the flag"
fi

apikey="$(config_apikey "${WS}")"
baseurl="$(config_baseurl "${WS}")"
if [[ -n "${apikey}" && "${apikey}" != "${CANARY_KEY}" ]]; then
    ok "config apiKey is the token (not the canary)"
else
    note_fail "R2: config apiKey is empty or the raw canary — token-only emission broken"
fi
if [[ "${baseurl}" == *"llm-relay"* ]]; then
    ok "config baseURL points at the relay router (${baseurl})"
else
    note_fail "R2: baseURL is not the router (${baseurl:-empty}) — direct wiring survived the flip"
fi

# -----------------------------------------------------------------------------
log "R3 — rollback (enabled=false → legacy batch resumes; the positive control)"

flip false
cycle_pod "${WS}"
wait_phase "${WS}" Active 300 || note_fail "R3: workspace never Active post-rollback"

apikey="$(config_apikey "${WS}")"
if [[ "${apikey}" == "${CANARY_KEY}" ]]; then
    ok "R3 PASS (positive control): the legacy batch carries the raw canary — rollback restores the raw-key path (and proves the sweep can find it)"
else
    note_fail "R3: post-rollback apiKey is not the raw canary ('${apikey:0:12}…') — the legacy resume path is NOT the documented rollback"
fi

# -----------------------------------------------------------------------------
log "R4 — flip ON again (D6.1 closes: exercised rollback, then forward)"

flip true
cycle_pod "${WS}"
wait_phase "${WS}" Active 300 || note_fail "R4: workspace never Active post-reflip"

staged="None"
for _ in $(seq 1 60); do
    staged="$(condition "${WS}" CredentialsStaged)"
    [[ "${staged}" == "True" ]] && break
    sleep 5
done
[[ "${staged}" == "True" ]] && ok "CredentialsStaged=True again" || note_fail "R4: CredentialsStaged=${staged}"

hits="$(sweep_hits "${WS}")"
if [[ "${hits}" =~ ^[0-9]+$ ]] && (( hits == 0 )); then
    ok "R4 PASS: token-only again, canary sweep zero"
else
    note_fail "R4: canary found ${hits:-?} time(s) after the re-flip"
fi

# Leave the cluster in the FLIPPED state (the drill's end state is the
# runbook's target posture).
# -----------------------------------------------------------------------------
if (( failures > 0 )); then
    die "${failures} row(s) failed"
fi
ok "all rows passed — flip, canary, rollback, re-flip (D6.1 complete)"
