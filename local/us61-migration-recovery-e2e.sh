#!/usr/bin/env bash
# Epic 72 / design 0061 §10 — the migration-recovery + arming + posture
# e2e (the formally-closing half of #1546/#1548's evidence, riding the
# M2 fallback arm's script as its precondition-builder):
#
#   R1 — THE FALLBACK PRECONDITION (compact re-seat of the M2 arm —
#        standalone-runnable needs the degraded state reached): tear the
#        handoff Secret → resync → the raw canary delivers,
#        relay_fallback_deliveries_total increments,
#        CredentialsStaged=False/relay_fallback_delivery.
#   R2 — RECOVERY (first live pin of the recovery half of the arc): the
#        controller's next reconcile RE-CREATES the deleted handoff
#        (upsertRelayHandoff's NotFound→Create path, staging.go) → the
#        fresh batch carries the TOKEN again → CredentialsStaged flips
#        back to True (the M4 heal arm). The full arc — healthy →
#        fallback → recovered — closes: a migration-mode blip is a
#        bounded window, not a stranded workspace (#1548's DOA class).
#   R3 — EXIT 85 (M1's crash-loud arming, §11 ruling 2, live): point the
#        controller's router URL at an unreachable endpoint → the pod
#        TERMINATES with exit code 85 (not a generic 1 crashloop — one
#        describe-pod away from the diagnosis) with the refusing line in
#        its log; restore → all-Ready again.
#   R4 — POSTURE CONVERGENCE (§11 ruling 3's gate assertions against
#        the exercised cluster): every Deployment Ready in both rendered
#        namespaces + a 45s stability window (identical restart
#        snapshots), the armed line in the controller log, and ZERO
#        forbidden lines in any pod log (prior crashed containers
#        included — R3's exit-85 corpse must be denial-free too).
#
# Environment: the standing post-flip install (relayOnlyKeyDelivery ON,
# migration fallback default), same conventions as
# local/us-72-relay-only-flip-drill.sh (local/lib/us70-common.sh).
#   CANARY_KEY - the planted key (default sk-US61RC-CANARY-0recover9arc)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/us70-common.sh
source "${SCRIPT_DIR}/lib/us70-common.sh"

harness_start

CANARY_KEY="${CANARY_KEY:-sk-US61RC-CANARY-0recover9arc}"
failures=0
note_fail() { failures=$((failures + 1)); warn "FAIL: $*"; }

# Per-script isolation (the #1342 pattern; the r5 all-hex lesson — a
# non-hex digit in the UUID base is a runtime-only death).
WS_BASE="e2e0610a-0000-4000-8000-000000000000"
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
    ' 2>/dev/null | jq -r --arg f "$1" '.provider["us61a"].options[$f] // ""' || echo ""
}

condition_row() { # -> "STATUS REASON" of CredentialsStaged (or "None")
    (kc get workspace "${WS}" -o jsonpath='{.status.conditions}' 2>/dev/null || echo '[]') \
        | jq -r '[.[] | select(.type == "CredentialsStaged")][0] | (.status // "None") + " " + (.reason // "")' 2>/dev/null || echo "None "
}

force_resync() { # direct secrets-resync via a pod port-forward; suspend/activate fallback
    local pod pw code pf
    pod=$(pod_of "${WS}")
    pw=$(kc get secret "workspace-pw-${WS}" -o jsonpath='{.data.password}' 2>/dev/null | base64 -d 2>/dev/null || true)
    if [[ -n "${pod}" && -n "${pw}" ]]; then
        kubectl --context "${CTX}" -n "${NS}" port-forward "pod/${pod}" 18099:4097 >/dev/null 2>&1 &
        pf=$!
        sleep 3
        code=$(curl -sm 30 -o /dev/null -w '%{http_code}' -X POST \
            -u "opencode:${pw}" "http://127.0.0.1:18099/v1/resync-secrets" 2>/dev/null || echo 000)
        kill "${pf}" 2>/dev/null || true
        [[ "${code}" == 2* ]] && return 0
    fi
    warn "direct resync unavailable — falling back to suspend/activate"
    api POST "/api/v1/workspaces/${WS}/suspend" >/dev/null
    sleep 5
    api POST "/api/v1/workspaces/${WS}/activate" >/dev/null
    wait_phase "${WS}" Active 300 || return 1
}

# -----------------------------------------------------------------------------
log "setup — seed workspace (post-flip defaults), bind the canary credential"

seed_workspace "${WS}"
login_harness_user >/dev/null 2>&1 || true
[[ -n "${AUTH_TOKEN}" ]] || die "setup: harness JWT login failed"

CRED_ID=$(curl -sm 30 -X POST \
    -H "Authorization: Bearer ${AUTH_TOKEN}" \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"us61 rc\",\"kind\":\"openai_compatible\",\"slug\":\"us61a\",\"apiKey\":\"${CANARY_KEY}\",\"baseURL\":\"http://mock-llm.${NS}.svc.cluster.local/v1\",\"modelAllowlist\":[\"mockmodel\"]}" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/provider-credentials" 2>/dev/null \
    | jq -r '.id // .credential.id // empty')
[[ -n "${CRED_ID}" ]] || die "setup: credential create returned no id"

bind_code=$(curl -sm 30 -o /dev/null -w '%{http_code}' -X POST \
    -H "Authorization: Bearer ${AUTH_TOKEN}" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/provider-credentials/${CRED_ID}/bind/${WS}" 2>/dev/null || echo 000)
[[ "${bind_code}" == 2* ]] || die "setup: bind failed: HTTP ${bind_code}"

wait_phase "${WS}" Active 300 || die "setup: workspace never Active"

HANDOFF="workspace-relay-${WS}"

# Reach the staged state (the R1 precondition): handoff present + True.
staged=0
for _ in $(seq 1 60); do
    if kc get secret "${HANDOFF}" >/dev/null 2>&1 && [[ "$(condition_row)" == "True "* ]]; then
        staged=1; break
    fi
    sleep 4
done
(( staged )) || die "setup: never reached the staged state (handoff + CredentialsStaged=True) — staging is not arming"

# Baseline counter for THIS series (delta-pinned, the M2 script's lesson:
# a stale series from a prior run is not a fresh delivery).
PRE_METRICS="$(curl -sm 15 "http://127.0.0.1:${PORTFWD_PORT}/metrics" 2>/dev/null || true)"
fallback_count() { # -> current relay_fallback_deliveries_total for THIS ws+slug
    curl -sm 15 "http://127.0.0.1:${PORTFWD_PORT}/metrics" 2>/dev/null \
        | grep -E "^relay_fallback_deliveries_total\{.*provider_slug=\"us61a\".*workspace=\"${WS}\"" \
        | grep -oE '[0-9]+$' || echo 0
}
pre_val=$(printf '%s' "${PRE_METRICS}" | grep -E "^relay_fallback_deliveries_total\{.*provider_slug=\"us61a\".*workspace=\"${WS}\"" | grep -oE '[0-9]+$' || echo 0)

# -----------------------------------------------------------------------------
log "R1 — the fallback precondition: tear the handoff → raw canary delivers, the counter increments, the condition falls"

kc delete secret "${HANDOFF}" >/dev/null 2>&1 || note_fail "R1: could not delete the handoff (already gone?)"
force_resync || note_fail "R1: no batch rebuild path succeeded"

key=""
for _ in $(seq 1 30); do
    key="$(config_field apiKey)"
    [[ "${key}" == "${CANARY_KEY}" ]] && break
    sleep 4
done
if [[ "${key}" == "${CANARY_KEY}" ]]; then
    ok "R1: the fallback delivered the RAW canary key (migration mode's availability half)"
else
    note_fail "R1: apiKey is not the raw canary ('${key:0:12}…') — the fallback did not deliver"
fi

post_val="$(fallback_count)"
if (( post_val > pre_val )); then
    ok "R1: relay_fallback_deliveries_total incremented (${pre_val} → ${post_val})"
else
    note_fail "R1: the counter did not increment (${pre_val} → ${post_val})"
fi

fell=0
for _ in $(seq 1 45); do
    [[ "$(condition_row)" == "False relay_fallback_delivery" ]] && { fell=1; break; }
    sleep 4
done
if (( fell )); then
    ok "R1: CredentialsStaged=False/relay_fallback_delivery (the degraded state reached)"
else
    note_fail "R1: CredentialsStaged='$(condition_row)' — the fallback outcome never surfaced"
fi

# -----------------------------------------------------------------------------
log "R2 — RECOVERY: the reconcile re-creates the handoff → tokens deliver again → CredentialsStaged heals to True"

# Force the controller's reconcile (suspend/activate: generation bump +
# fresh pod → fresh batch) — the deleted-handoff NotFound→Create path.
api POST "/api/v1/workspaces/${WS}/suspend" >/dev/null
sleep 5
api POST "/api/v1/workspaces/${WS}/activate" >/dev/null
wait_phase "${WS}" Active 300 || note_fail "R2: workspace never Active after the recovery cycle"

reborn=0
for _ in $(seq 1 60); do
    if kc get secret "${HANDOFF}" >/dev/null 2>&1; then reborn=1; break; fi
    sleep 4
done
if (( reborn )); then
    ok "R2: the controller RE-CREATED the deleted ${HANDOFF} (the NotFound→Create path, live)"
else
    note_fail "R2: the handoff Secret never reappeared — recovery does not converge (the #1548 stranded class)"
fi

key=""
for _ in $(seq 1 30); do
    key="$(config_field apiKey)"
    [[ -n "${key}" && "${key}" != "${CANARY_KEY}" ]] && break
    sleep 4
done
if [[ -n "${key}" && "${key}" != "${CANARY_KEY}" ]]; then
    ok "R2: the fresh batch carries the TOKEN again (the raw canary is gone)"
else
    note_fail "R2: apiKey is empty or still the raw canary — the recovered handoff did not deliver"
fi

healed=0
for _ in $(seq 1 45); do
    [[ "$(condition_row)" == "True "* ]] && { healed=1; break; }
    sleep 4
done
if (( healed )); then
    ok "R2: CredentialsStaged healed back to True (the M4 heal arm, live — the arc closes: healthy → fallback → recovered)"
else
    note_fail "R2: CredentialsStaged='$(condition_row)' — the condition never healed"
fi

# -----------------------------------------------------------------------------
log "R3 — EXIT 85: the unarmable controller terminates with the DISTINCT code + the refusing line; restore → Ready"

ORIG_URL=$(kubectl --context "${CTX}" -n "${NS}" get deployment llmsafespaces-controller \
    -o jsonpath='{.spec.template.spec.containers[0].args}' 2>/dev/null \
    | tr ' ' '\n' | grep -F -- '--llm-relay-router-url=' | cut -d= -f2- || true)
[[ -n "${ORIG_URL}" ]] || die "R3: could not read the live router URL from the controller args"

helm --kube-context "${CTX}" upgrade --install llmsafespaces "${SCRIPT_DIR}/../helm" \
    -n "${NS}" --reuse-values \
    --set relayOnlyKeyDelivery.workspaceRouterURL="http://127.0.0.1:9" \
    >/dev/null 2>&1 || true # deliberately NO wait flag: the controller will NOT converge — the exit code IS the assertion

exit85=0
for _ in $(seq 1 45); do
    code85=$(kc get pods -l app.kubernetes.io/name=llmsafespaces,app.kubernetes.io/component=controller \
        -o jsonpath='{.items[0].status.containerStatuses[0].lastState.terminated.exitCode}' 2>/dev/null || true)
    [[ "${code85}" == "85" ]] && { exit85=1; break; }
    sleep 4
done
if (( exit85 )); then
    ok "R3: the unarmable controller TERMINATED with exit code 85 (crash-loud, not a generic 1)"
else
    note_fail "R3: no exit-85 termination observed (last exitCode: '${code85:-none}') — the arming contract is not crash-loud"
fi

refused=0
if kubectl --context "${CTX}" -n "${NS}" logs deployment/llmsafespaces-controller --previous 2>/dev/null \
    | grep -F "relay-only key delivery: refusing to start (not armed)" >/dev/null; then
    refused=1
fi
if (( refused )); then
    ok "R3: the refusing line is in the crashed controller's log (one describe-pod from the diagnosis)"
else
    note_fail "R3: the 'refusing to start (not armed)' line not found in the previous container's log"
fi

helm --kube-context "${CTX}" upgrade --install llmsafespaces "${SCRIPT_DIR}/../helm" \
    -n "${NS}" --reuse-values \
    --set relayOnlyKeyDelivery.workspaceRouterURL="${ORIG_URL}" \
    --wait --timeout 10m >/dev/null 2>&1 \
    || note_fail "R3: the restore upgrade did not converge (check the controller)"
kubectl --context "${CTX}" -n "${NS}" rollout status deployment/llmsafespaces-controller --timeout=300s >/dev/null 2>&1 \
    && ok "R3: restored — the controller is Ready again (the arming failure is fully reversible)" \
    || note_fail "R3: the controller did not return to Ready after the restore"

# -----------------------------------------------------------------------------
log "R4 — POSTURE CONVERGENCE: all-Ready + stable, the armed line, zero forbidden (both namespaces, crashed corpses included)"

GATE_NSS=("${NS}" "llm-relay")
ready_ok=1
for gate_ns in "${GATE_NSS[@]}"; do
    if kubectl --context "${CTX}" -n "${gate_ns}" wait --for=condition=available deployment --all --timeout=120s >/dev/null 2>&1; then
        ok "R4: every Deployment Ready in ${gate_ns}"
    else
        note_fail "R4: a Deployment in ${gate_ns} is not Ready after the full exercise"
        ready_ok=0
    fi
done

# The 45s stability window (M3's second clause: identical restart
# snapshots — a crashloop cannot stay quiet; R3's exit-85 restarts are
# EXPECTED and pre-window, so the window compares post-restore state).
snap() { kubectl --context "${CTX}" -n "${NS}" get pods -o jsonpath='{range .items[*]}{.metadata.name}={.status.containerStatuses[0].restartCount} {end}' 2>/dev/null; }
s1="$(snap)"; sleep 45; s2="$(snap)"
if [[ -n "${s1}" && "${s1}" == "${s2}" ]]; then
    ok "R4: 45s stability window — restart counters identical (no crashloop residue)"
else
    note_fail "R4: restart counters moved inside the stability window ('${s1}' → '${s2}')"
fi

if kubectl --context "${CTX}" -n "${NS}" logs deployment/llmsafespaces-controller --tail=5000 2>/dev/null \
    | grep -F "relay-only key delivery enabled" >/dev/null; then
    ok "R4: the armed line is in the live controller log"
else
    note_fail "R4: the armed line ('relay-only key delivery enabled') not found in the controller log"
fi

forbids=0
for gate_ns in "${GATE_NSS[@]}"; do
    # Failure-checked enumeration (the M3 gate lesson): a
    # process-substitution feed is invisible to set -e — a namespace
    # whose pods cannot be enumerated would be silently cleared.
    PODS="$(kubectl --context "${CTX}" -n "${gate_ns}" get pods -o name 2>/dev/null || true)"
    if [[ -z "${PODS}" ]] && ! kubectl --context "${CTX}" -n "${gate_ns}" get pods >/dev/null 2>&1; then
        note_fail "R4: could not enumerate pods in ${gate_ns} — an unenumerable namespace cannot be cleared"
        forbids=$((forbids + 1)); continue
    fi
    while read -r pod; do
        [[ -n "${pod}" ]] || continue
        if ! kubectl --context "${CTX}" -n "${gate_ns}" logs "${pod}" --all-containers=true >/dev/null 2>&1; then
            note_fail "R4: could not fetch logs for ${gate_ns}/${pod} — a pod whose logs cannot be read cannot be cleared"
            forbids=$((forbids + 1)); continue
        fi
        if kubectl --context "${CTX}" -n "${gate_ns}" logs "${pod}" --all-containers=true 2>/dev/null | grep -iq forbidden; then
            note_fail "R4: forbidden line in ${gate_ns}/${pod} — RBAC starvation"
            forbids=$((forbids + 1))
        fi
        if kubectl --context "${CTX}" -n "${gate_ns}" logs "${pod}" --all-containers=true --previous >/dev/null 2>&1; then
            if kubectl --context "${CTX}" -n "${gate_ns}" logs "${pod}" --all-containers=true --previous 2>/dev/null | grep -iq forbidden; then
                note_fail "R4: forbidden line in ${gate_ns}/${pod} (previous container)"
                forbids=$((forbids + 1))
            fi
        fi
    done <<< "${PODS}"
done
if (( forbids == 0 )); then
    ok "R4: zero forbidden lines in any pod log, both namespaces, crashed corpses included"
fi

# -----------------------------------------------------------------------------
if (( failures > 0 )); then
    die "${failures} row(s) failed"
fi
ok "all rows passed — the migration arc (fallback → recovery), the exit-85 arming contract, and posture convergence, live"
