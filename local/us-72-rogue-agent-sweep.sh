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
                 /workspace/.local/config/opencode/agent-config.json \
                 /sandbox-cfg/secrets.json \
                 /sandbox-runtime/rt/secrets.json \
                 /sandbox-runtime/rt/auth.json; do
            out=$(grep -ac "'"${CANARY_KEY}"'" "$p" 2>/dev/null || true)
            if [[ "${out}" =~ ^[0-9]+$ ]] && (( out > 0 )); then
                hits=$((hits + out)); echo "HIT ${p} x${out}" >&2
            fi
        done
        for env in /proc/[0-9]*/environ; do
            out=$(grep -ac "'"${CANARY_KEY}"'" "$env" 2>/dev/null || true)
            if [[ "${out}" =~ ^[0-9]+$ ]] && (( out > 0 )); then
                hits=$((hits + out)); echo "HIT ${env} x${out}" >&2
            fi
        done
        echo "${hits}"
    ' 2>&1 | tee /dev/stderr | tail -1 || echo 1
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

# The CONVERGENCE gate (run 36135708380's R1 lesson): the exit criterion
# evaluates the CONVERGED post-flip posture, not the boot transient —
# design 0061 M2's migration-mode fail-open fallback deliberately
# delivers a RAW batch on any boot where controller staging has not
# converged yet (workspaces must not strand), and that window's live
# files (agent-config.json, rt/auth.json) legitimately carry the raw
# canary until the token batch lands. The drill's own R2 gates on
# CredentialsStaged=True for exactly this reason; the sweep must too.
staged="None"
for _ in $(seq 1 60); do
    staged="$(condition_status CredentialsStaged)"
    [[ "${staged}" == "True" ]] && break
    sleep 5
done
if [[ "${staged}" == "True" ]]; then
    ok "CredentialsStaged=True (converged posture — the criterion's frame)"
else
    note_fail "setup: CredentialsStaged=${staged} (never True) — the sweep cannot evaluate the converged posture"
fi

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
# On modern pods init-fs has installed the #1296 SYMLINK there — a bare
# `>` would FOLLOW it into the live store (rt/auth.json) and the row
# would fail structurally. rm -f the link first, then write the true
# legacy regular-file shape.
kubectl --context "${CTX}" -n "${NS}" exec "${POD}" -c workspace -- bash -c '
    mkdir -p /workspace/.local/opencode
    rm -f /workspace/.local/opencode/auth.json
    printf "{\"legacy\": {\"type\": \"api\", \"key\": \"%s\"}}" "'"${CANARY_KEY}"'" \
        > /workspace/.local/opencode/auth.json
' >/dev/null 2>&1 || die "R2: planting the legacy residue failed"

hits="$(sweep_hits)"
if [[ "${hits}" =~ ^[0-9]+$ ]] && (( hits >= 1 )); then
    ok "R2 PASS (positive control): the sweep FINDS the planted legacy canary — the sweep can fail"
else
    note_fail "R2: the planted legacy canary was NOT found — the sweep is decorative (a false-pass machine)"
fi

# Scrub: the agentd subcommand, in-pod — CAPTURING its JSON report (the
# row's direct evidence of the removal; the running agentd's boot-time
# tracker fired AlreadyClean on this clean pod and its sync.Once is
# spent, so the subcommand's own report is the authoritative outcome).
SCRUB_OUT="$(kubectl --context "${CTX}" -n "${NS}" exec "${POD}" -c workspace -- \
    /agentd/usr/local/bin/workspace-agentd scrub-legacy-keys --workspace-root /workspace \
    2>/dev/null || true)"
if printf '%s' "${SCRUB_OUT}" | jq -e '.authKeysRemoved >= 1' >/dev/null 2>&1; then
    ok "R2: the scrub's own report shows the removal ($(printf '%s' "${SCRUB_OUT}" | jq -c '{authKeysRemoved, configKeysRemoved}'))"
else
    note_fail "R2: the scrub report shows no removal: ${SCRUB_OUT:-<no output>}"
fi

hits="$(sweep_hits)"
if [[ "${hits}" =~ ^[0-9]+$ ]] && (( hits == 0 )); then
    ok "R2 PASS: post-sweep zero — the legacy residue is gone"
else
    note_fail "R2: canary still present ${hits:-?} time(s) after the scrub"
fi

# The BOOT mirror's condition (independent evidence this row does NOT
# claim: the running agentd's boot-time scrub of a CLEAN pod reports
# AlreadyClean → True/Clean — it cannot and must not reflect the R2
# subcommand's separate-process scrub). Assert only that the boot
# mirror EXISTS and is healthy-shaped.
scrub_status="None"
for _ in $(seq 1 30); do
    scrub_status="$(condition_status LegacyKeysScrubbed)"
    [[ "${scrub_status}" != "None" ]] && break
    sleep 4
done
if [[ "${scrub_status}" == "True" ]]; then
    ok "LegacyKeysScrubbed=True on the Workspace (the BOOT mirror, already clean at pod start)"
else
    note_fail "R2: LegacyKeysScrubbed=${scrub_status} (the boot mirror never landed)"
fi

# -----------------------------------------------------------------------------
log "R3 — the residue-boot migration row: plant on the PVC, suspend, resume → the BOOT scrub fires"

# The actual US-72.6 migration scenario: a pod boots WITH legacy PVC
# residue and the boot-time tracker scrub (the relay monitor's first
# Present=true hook) removes it — the KeysRemoved condition is the
# evidence. Plant while RUNNING (exec), then suspend (the pod dies, the
# PVC keeps the file), then resume (the fresh pod boots with the
# residue).
#
# SURFACE 2, not Surface 1 (r4): init-fs MANAGES the auth.json path —
# its replaceSymlink DELETES any pre-existing regular file before
# installing the #1296 symlink, so a Surface-1 plant never survives to
# the scrub (AuthPathSkipped/AlreadyClean — the row was structurally
# unpassable). The agent-config.json COPY under .local/config/opencode/
# is scrub territory init-fs never touches — the plant that actually
# reaches the boot scrub.
kubectl --context "${CTX}" -n "${NS}" exec "${POD}" -c workspace -- bash -c '
    mkdir -p /workspace/.local/config/opencode
    printf "{\"provider\": {\"legacy2\": {\"options\": {\"apiKey\": \"%s\"}}}}" "'"${CANARY_KEY}"'" \
        > /workspace/.local/config/opencode/agent-config.json
' >/dev/null 2>&1 || note_fail "R3: planting the PVC residue failed"

api POST "/api/v1/workspaces/${WS}/suspend" >/dev/null
sleep 5
api POST "/api/v1/workspaces/${WS}/activate" >/dev/null
wait_phase "${WS}" Active 300 || note_fail "R3: workspace never Active after the resume"

# The boot scrub fired: poll for the KeysRemoved REASON + the config
# count — NOT for a non-None status (R2's boot-mirror already left
# True/Clean "clean" on the pre-suspend pod and nothing removes
# conditions; a non-None poll would trip on the stale message — r4
# finding 2).
boot_reason=""; boot_msg=""
for _ in $(seq 1 30); do
    boot_row="$( (kc get workspace "${WS}" -o jsonpath='{.status.conditions}' 2>/dev/null || echo '[]') \
        | jq -r '[.[] | select(.type == "LegacyKeysScrubbed")][0] | (.reason // "") + " " + (.message // "")' 2>/dev/null || echo " ")"
    boot_reason="${boot_row%% *}"
    boot_msg="${boot_row#* }"
    [[ "${boot_reason}" == "KeysRemoved" ]] && break
    sleep 4
done
if [[ "${boot_reason}" == "KeysRemoved" && "${boot_msg}" == *"config=1"* ]]; then
    ok "R3 PASS: the BOOT scrub fired on the residue-bearing resume (LegacyKeysScrubbed=True/KeysRemoved config=1)"
else
    note_fail "R3: reason='${boot_reason}' msg='${boot_msg}' — the migration trigger did not fire on the residue boot"
fi

# And the post-boot sweep is clean (the residue is gone) — GATED on
# POD-FRESH convergence evidence (the r3 review's finding: the
# CredentialsStaged condition SURVIVES suspend — conditions are never
# cleared on the suspend path — so polling it after resume reads the
# PRE-SUSPEND pod's verdict and establishes nothing about the resumed
# pod; a lastTransitionTime freshness gate has the inverse defect: the
# steady-state resumed staging pass performs ZERO writes, so a genuinely
# converged resume may carry no fresh transition). The honest pod-fresh
# evidence: read the RESUMED pod's effective config and require the
# provider's apiKey to be the token (not the canary) and baseURL to
# point at the relay router — direct proof the resumed delivery
# converged, independent of any condition's staleness.
config_field() { # field -> the sweep provider's rendered field (or "")
    local pod
    pod=$(pod_of_ws)
    [[ -n "${pod}" ]] || { echo ""; return; }
    kubectl --context "${CTX}" -n "${NS}" exec "${pod}" -c workspace -- bash -c '
        for p in /sandbox-runtime/agent-config.json /agentd-config/agent-config.json; do
            [[ -r "$p" ]] && cat "$p" && exit 0
        done
        echo "{}"
    ' 2>/dev/null | jq -r --arg f "$1" '.provider["us72sweep"].options[$f] // ""' || echo ""
}
config_apikey()  { config_field apiKey; }
config_baseurl() { config_field baseURL; }
config_converged() { # -> 0 when the pod's live config carries token+router
    local apikey baseurl
    apikey="$(config_apikey)"
    baseurl="$(config_baseurl)"
    [[ -n "${apikey}" && "${apikey}" != "${CANARY_KEY}" && "${baseurl}" == *"llm-relay"* ]]
}
converged=1
for _ in $(seq 1 60); do
    if config_converged; then converged=0; break; fi
    sleep 5
done
if (( converged == 0 )); then
    ok "the resumed pod's config carries token+router (pod-fresh convergence — the criterion's frame)"
else
    note_fail "R3: the resumed pod's config never showed token+router — the raw M2-fallback batch may still be live; cannot evaluate the converged posture"
fi
hits="$(sweep_hits)"
if [[ "${hits}" =~ ^[0-9]+$ ]] && (( hits == 0 )); then
    ok "R3: post-boot sweep zero — the PVC residue is gone"
else
    note_fail "R3: canary still present ${hits:-?} time(s) after the residue boot"
fi

# -----------------------------------------------------------------------------
if (( failures > 0 )); then
    die "${failures} row(s) failed"
fi
ok "all rows passed — the rogue agent finds nothing (K1, #820's exit criterion)"
