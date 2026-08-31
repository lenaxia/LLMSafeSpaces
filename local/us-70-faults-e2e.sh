#!/usr/bin/env bash
# Epic 70 US-70.0 — fault/partition/corruption/token/chaos e2e rows.
#
# Runs on the US-70.0 delivery pool (.github/workflows/us-70-delivery-pool.yml)
# which installs SEAM-INERT and arms the fault seam AFTER the delivery suite
# (kubectl set env deployment/llmsafespaces-api
#  LLMSAFESPACES_FAULT_INJECTION=COUNT:METHOD:PATH_PREFIX, e.g.
#  8:POST:/internal/v1/pod-bootstrap + rollout — the env is read once per
# process, so the rollout arms a fresh API process) and provisions runsc.
# Shared helpers + the env contract live in lib/us70-common.sh. Rows:
#
#   F1   — API 500s at boot (AC-8): fault-injected pod-bootstrap failures
#          never block boot; after the count exhausts, the autopush heal
#          (UserCredsPresent=false → autopush → reload-secrets) converges
#          full env delivery with no manual reload. Skipped loudly when the
#          deploy lacks the fault env (probe sees no 500s) — never a silent
#          pass.
#   F2   — partition at resume (W5): suspend, scale the API to 0, resume via
#          the API-independent kubectl patch; the sessionless boot comes up
#          degraded (env var ABSENT); API restore + port-forward
#          re-establish → autopush converges the env.
#   F3   — key-row corruption (AC-9 degrade half): corrupted user_keys.
#          wrapped_dek → sessionless degrade + the #1114 loud-degrade audit
#          row (secret_audit_log action pod_bootstrap_dek_failed); exact-byte
#          restore → converge.
#   F4   — SA-token rows (kubectl-minted tokens — the real TokenReview path
#          server-side; audience llmsafespace-api + SA-name binding checks
#          in api/internal/handlers/pod_bootstrap.go:204-230 apply to
#          minted tokens exactly as to projected ones):
#          (a) tampered mint → exactly 401, untampered mint control → NOT
#              401; (b) expired mint (sleep past the JWT's OWN .exp,
#              bounded ≤~3700s) → exactly 401, fresh mint → NOT 401. The
#              pod-side per-call fresh read is proven by every converge row
#          (each resume re-bootstraps); no pod-volume exec (agentd is
#          FROM-scratch — no cat) and no sidecar/single-container asymmetry.
#   F5   — chaos kills: (a) agentd sidecar container killed via crictl on
#          the kind node (FROM-scratch image has no kill binary; PID-1
#          SIGKILL from inside the pid-namespace is dropped) → restart +
#          converge; (b) pod-delete mid-bind → recreate + converge.
#
# Environment (beyond lib/us70-common.sh):
#   FAULT_COUNT    - expected fault-rule count (default 8); the workflow's
#                    arming step sets LLMSAFESPACES_FAULT_INJECTION from the
#                    SAME number (workflow env FAULT_COUNT) — one source.
#   WS_BASE        - distinct workspace prefix (default e2esdf0-…; the
#                    longest name is 30 chars ≤ the secret_audit_log
#                    workspace_id varchar(36) so F3's exact match works)
set -Eeuo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
source "$SCRIPT_DIR/lib/us70-common.sh"

export FAULT_COUNT="${FAULT_COUNT:-8}"
WS_BASE="${WS_BASE:-e2esdf0-0000-0000-000000000}"

PASS=0
SKIP=0
declare -a SKIPPED_ROWS=()

skip_row() { # row msg...
    local row="$1"; shift
    warn "SKIPPED ${row}: $*"
    SKIP=$((SKIP + 1))
    SKIPPED_ROWS+=("${row}")
}

psql_q() { # -Atc query (or -c for effect-only) via the postgres pod
    kc exec "${PGPOD}" -- env PGPASSWORD="${PG_PWD}" \
        psql -U llmsafespaces -d llmsafespaces "$@"
}

# reconnect_api — the port-forward died when the API scaled to 0; after the
# rollout completes, re-establish it and wait for /livez.
reconnect_api() {
    if [[ -n "${PF_PID:-}" ]]; then
        kill "${PF_PID}" 2>/dev/null || true
        wait "${PF_PID}" 2>/dev/null || true
        PF_PID=""
    fi
    kc -n "${NS}" port-forward svc/llmsafespaces-api "${PORTFWD_PORT}:8080" >/dev/null 2>&1 &
    PF_PID=$!
    local i
    for i in $(seq 1 30); do
        curl -sfm 2 "http://127.0.0.1:${PORTFWD_PORT}/livez" >/dev/null 2>&1 && return 0
        sleep 2
    done
    die "reconnect_api: API /livez unreachable after restore"
}

# wait_env_absent ws VAR timeout_s — poll until the child environ is
# non-empty (agent spawned) and assert VAR never appears in it.
wait_env_absent() {
    local ws="$1" var="$2" timeout_s="$3" i env
    for ((i = 0; i < timeout_s; i += 3)); do
        env=$(agent_environ "${ws}")
        if [[ -n "${env}" ]]; then
            if [[ "${env}" == *"${var}"* ]]; then
                return 1
            fi
            return 0
        fi
        sleep 3
    done
    warn "wait_env_absent ${ws}: child environ never became readable"
    return 1
}

# wait_env_present ws VAR timeout_s — poll for VAR in the child environ.
wait_env_present() {
    local ws="$1" var="$2" timeout_s="$3" i
    for ((i = 0; i < timeout_s; i += 3)); do
        if env_in_child "${ws}" "${var}"; then
            return 0
        fi
        sleep 3
    done
    return 1
}

suspend_ws() { # ws — API suspend + wait Suspended
    local ws="$1"
    curl -sfm 10 -X POST -H "Authorization: Bearer ${API_KEY}" \
        "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${ws}/suspend" >/dev/null \
        || die "suspend ${ws} call failed"
    wait_phase "${ws}" Suspended 180 || die "${ws} never Suspended"
}

activate_ws() { # ws — API activate + wait Active
    local ws="$1"
    curl -sfm 30 -X POST -H "Authorization: Bearer ${API_KEY}" \
        "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${ws}/activate" >/dev/null \
        || die "activate ${ws} call failed"
    wait_phase "${ws}" Active 240 || die "${ws} never re-Active after resume"
}

total_start=$(date +%s%3N)
harness_start

# -----------------------------------------------------------------------------
# F1 — API 500s at boot (AC-8): never-block-boot + autopush heal
# -----------------------------------------------------------------------------
log "F1 — fault-injected pod-bootstrap 500s → boot never blocked, autopush heals"

# Probe the fault seam first: with rule ${FAULT_COUNT}:POST:/internal/v1/
# pod-bootstrap the first matching POSTs return 500 BEFORE auth (no bearer
# needed). An inert API (no LLMSAFESPACES_FAULT_INJECTION at process start)
# answers 401 to the unauthenticated probe — that means the row can't
# exercise the seam, so it skips loudly (exit-tracked like the gVisor
# gate), never silently passes.
FAULT_SEEN=0
for _i in $(seq 1 "${FAULT_COUNT}"); do
    CODE=$(curl -sm 10 -o /dev/null -w '%{http_code}' -X POST \
        -H 'Content-Type: application/json' -d '{"workspaceID":"fault-probe"}' \
        "http://127.0.0.1:${PORTFWD_PORT}/internal/v1/pod-bootstrap" || true)
    if [[ "${CODE}" == "500" ]]; then
        FAULT_SEEN=$_i
        break
    fi
done
if (( FAULT_SEEN == 0 )); then
    skip_row "F1" "deploy lacks the fault seam (LLMSAFESPACES_FAULT_INJECTION unset on the API process — probe never saw a 500); AC-8 needs the pool's armed deploy"
else
    ok "fault seam active: 500 on try ${FAULT_SEEN} of ${FAULT_COUNT} (≈$((FAULT_COUNT - FAULT_SEEN)) remaining for the pod's bootstraps)"

    WS1=$(ws_id 1)
    log "F1: seed workspace ${WS1} + bind env-secret before boot"
    seed_workspace "${WS1}" "${USER_ID}"
    bind_env "${WS1}" "SD_F1" "fault-f1-value"

    wait_phase "${WS1}" Active 240 || die "F1: workspace never Active despite failed bootstraps (never-block-boot violated)"

    # M8: sample the degrade window ONCE — bootstrap 500s mean the pod
    # boots sessionless, so the bound env var must be ABSENT here (the
    # window is real and observed). Race guard: if autopush already healed
    # between Active and this sample, log it and continue (do NOT die) —
    # the deterministic asserts are the convergence below plus F2's burn
    # loop.
    if env_in_child "${WS1}" "SD_F1=fault-f1-value"; then
        warn "F1: degrade window closed early (SD_F1 already present at sample time) — heal raced the sample; continuing"
    else
        ok "F1: degrade-window sample: SD_F1 absent from child env while seam faults exhaust (sessionless boot observed)"
    fi
    # Count-exhaustion leg: exercised by F2's burn loop below — the
    # bootstrap client itself never retries (autopush is the retry path).

    if secrets_converged "${WS1}" 300 && wait_env_present "${WS1}" "SD_F1=fault-f1-value" 300; then
        ok "F1 PASS: Active through faulted bootstraps, autopush healed env (SD_F1 present, spawnedRev converged)"
        PASS=$((PASS + 1))
    else
        die "F1 FAIL: autopush heal did not converge env delivery within 300s"
    fi
fi

# -----------------------------------------------------------------------------
# F2 — partition at resume (W5): API scale-to-zero through suspend/resume
# -----------------------------------------------------------------------------
WS2=$(ws_id 2)
log "F2 — partition at resume: suspend → API to 0 → patch-resume → degraded boot → restore → converge"

seed_workspace "${WS2}" "${USER_ID}"
bind_env "${WS2}" "SD_F2" "partition-f2-value"
wait_phase "${WS2}" Active 240 || die "F2: workspace never Active"
secrets_converged "${WS2}" 120 || die "F2: pre-suspend secretsDelivery unhealthy"
if ! env_in_child "${WS2}" "SD_F2=partition-f2-value"; then
    die "F2: pre-suspend env missing — setup broken"
fi
ok "F2: pre-suspend env present"

suspend_ws "${WS2}"
ok "F2: suspended; partitioning the API before resume"

# Partition: scale the API to 0 (kindnet does not enforce NetworkPolicy —
# scale-to-zero is the real API-unreachable mechanism on kind).
kc scale deployment/llmsafespaces-api --replicas=0
for _i in $(seq 1 30); do
    [[ -z "$(kc get pod -l app.kubernetes.io/component=api -o name 2>/dev/null)" ]] && break
    sleep 2
done
[[ -z "$(kc get pod -l app.kubernetes.io/component=api -o name 2>/dev/null)" ]] || die "F2: API pod never gone after scale-to-0"
ok "F2: API partitioned (0 replicas, pod gone)"

# Resume API-independently: the API is down, so patch the CR directly.
kc patch workspace "${WS2}" --type=merge -p '{"spec":{"suspend":false}}' >/dev/null
wait_phase "${WS2}" Active 240 || die "F2: never re-Active after partition resume"

# Documented W5 coupling: resume without the API boots sessionless — the
# env var is ABSENT from the child until the API returns and autopush heals.
if wait_env_absent "${WS2}" "SD_F2=partition-f2-value" 120; then
    ok "F2: sessionless boot under partition (SD_F2 absent from child env)"
else
    die "F2 FAIL: env present at resume under partition — W5 coupling not as documented"
fi
POD2=$(pod_of "${WS2}")
RC2=$(runtime_container "${POD2}")
if kc exec "${POD2}" ${RC2:+-c "${RC2}"} -- sh -c 'pgrep -f "opencode serve" >/dev/null' 2>/dev/null; then
    ok "F2: opencode child alive despite the degraded boot"
else
    die "F2 FAIL: opencode child not running after partition resume"
fi

kc scale deployment/llmsafespaces-api --replicas=1
kc rollout status deployment/llmsafespaces-api --timeout=180s >/dev/null
reconnect_api
ok "F2: API restored + port-forward re-established"

# The API restart reset the fault counter (it is per-process memory): burn
# any residual count now so F3/F4/F5 fresh-pod bootstraps hit a clean API
# instead of timing through fault retries meant for F1. FAULT_COUNT+1
# probes: a seam sized ≤FAULT_COUNT must go inert inside the loop; if the
# FINAL probe still 500s the rule count exceeds FAULT_COUNT — the seam is
# misconfigured vs this script's expectation (M10), so die, don't limp on.
CODE=""
for _i in $(seq 1 "$((FAULT_COUNT + 1))"); do
    CODE=$(curl -sm 10 -o /dev/null -w '%{http_code}' -X POST \
        -H 'Content-Type: application/json' -d '{"workspaceID":"fault-probe"}' \
        "http://127.0.0.1:${PORTFWD_PORT}/internal/v1/pod-bootstrap" || true)
    [[ "${CODE}" == "500" ]] || break
done
if [[ "${CODE}" == "500" ]]; then
    die "F2 FAIL: fault seam still firing after ${FAULT_COUNT} burn probes — LLMSAFESPACES_FAULT_INJECTION count exceeds FAULT_COUNT=${FAULT_COUNT} (seam misconfigured)"
fi
ok "F2: residual fault count burned (probe now returns ${CODE})"

if secrets_converged "${WS2}" 300 && wait_env_present "${WS2}" "SD_F2=partition-f2-value" 300; then
    ok "F2 PASS: converged after API restore (autopush delivered SD_F2)"
    PASS=$((PASS + 1))
else
    die "F2 FAIL: no convergence within 300s of API restore"
fi

# -----------------------------------------------------------------------------
# F3 — key-row corruption (AC-9 degrade half): loud degrade + restore
# -----------------------------------------------------------------------------
WS3=$(ws_id 3)
log "F3 — corrupted wrapped_dek → sessionless degrade + pod_bootstrap_dek_failed audit → restore"

seed_workspace "${WS3}" "${USER_ID}"
bind_env "${WS3}" "SD_F3" "corrupt-f3-value"
wait_phase "${WS3}" Active 240 || die "F3: workspace never Active"
secrets_converged "${WS3}" 120 || die "F3: pre-suspend secretsDelivery unhealthy"
if ! env_in_child "${WS3}" "SD_F3=corrupt-f3-value"; then
    die "F3: pre-suspend env missing — setup broken"
fi
ok "F3: pre-suspend env present"

ORIG_HEX=$(psql_q -Atc "SELECT encode(wrapped_dek,'hex') FROM user_keys WHERE user_id='${USER_ID}'")
[[ -n "${ORIG_HEX}" ]] || die "F3: no user_keys.wrapped_dek row for ${USER_ID} (seed a DEK first)"

suspend_ws "${WS3}"
psql_q -c "UPDATE user_keys SET wrapped_dek=decode('00','hex') WHERE user_id='${USER_ID}'" >/dev/null
ok "F3: wrapped_dek corrupted (decode('00','hex')) while suspended"

activate_ws "${WS3}"

if wait_env_absent "${WS3}" "SD_F3=corrupt-f3-value" 120; then
    ok "F3: sessionless degrade (SD_F3 absent — user creds not delivered)"
else
    die "F3 FAIL: env present despite corrupted DEK — degrade contract violated"
fi

AUDIT_N=0
for _i in $(seq 1 20); do
    AUDIT_N=$(psql_q -Atc "SELECT count(*) FROM secret_audit_log WHERE user_id='${USER_ID}' AND action='pod_bootstrap_dek_failed' AND workspace_id='${WS3}'")
    (( AUDIT_N >= 1 )) && break
    sleep 3
done
if (( AUDIT_N >= 1 )); then
    ok "F3: loud-degrade audit row present (pod_bootstrap_dek_failed ×${AUDIT_N} for ${WS3})"
else
    die "F3 FAIL: no pod_bootstrap_dek_failed audit row — the #1114 silent-degrade regression"
fi

psql_q -c "UPDATE user_keys SET wrapped_dek=decode('${ORIG_HEX}','hex') WHERE user_id='${USER_ID}'" >/dev/null
RESTORED_HEX=$(psql_q -Atc "SELECT encode(wrapped_dek,'hex') FROM user_keys WHERE user_id='${USER_ID}'")
[[ "${RESTORED_HEX}" == "${ORIG_HEX}" ]] || die "F3: restore mismatch — user_keys left corrupted, aborting"

if secrets_converged "${WS3}" 300 && wait_env_present "${WS3}" "SD_F3=corrupt-f3-value" 300; then
    ok "F3 PASS: exact-byte restore → converged (SD_F3 delivered)"
    PASS=$((PASS + 1))
else
    die "F3 FAIL: no convergence within 300s of restore"
fi

# -----------------------------------------------------------------------------
# F4 — SA-token rows: kubectl-minted tokens on the REAL TokenReview path.
# Minting via `kubectl create token workspace-<ws> --audience=…` produces a
# credential the API validates exactly like the projected one (audience +
# SA-name binding, api/internal/handlers/pod_bootstrap.go:204-230) — no
# pod-volume exec (the FROM-scratch agentd image has no cat) and no
# sidecar/single-container asymmetry (minting works in both modes). The
# pod-side per-call fresh token read is proven by every converge row (each
# resume re-bootstraps with a then-current token).
# -----------------------------------------------------------------------------
WS4=$(ws_id 4)
log "F4 — SA-token rows: minted token tampered → 401; expired (JWT-exp time-travel) → 401 + fresh mint works"

seed_workspace "${WS4}" "${USER_ID}"
bind_env "${WS4}" "SD_F4" "token-f4-value"
wait_phase "${WS4}" Active 240 || die "F4: workspace never Active"
secrets_converged "${WS4}" 120 || die "F4: secretsDelivery unhealthy"
ok "F4: workspace healthy"

bootstrap_post() { # token → http code
    curl -sm 10 -o /dev/null -w '%{http_code}' -X POST \
        -H "Authorization: Bearer $1" -H 'Content-Type: application/json' \
        -d "{\"workspaceID\":\"${WS4}\"}" \
        "http://127.0.0.1:${PORTFWD_PORT}/internal/v1/pod-bootstrap" || true
}

mint_token() { # → fresh SA token for WS4's bootstrap SA, API audience
    kubectl --context "${CTX}" -n "${NS}" create token "workspace-${WS4}" \
        --audience=llmsafespace-api
}

# --- F4a: tampered mint → exactly 401; untampered mint control → NOT 401 ---
TOKEN=$(mint_token) || die "F4a: kubectl create token failed (needs create serviceaccounts/token RBAC)"
[[ -n "${TOKEN}" ]] || die "F4a: minted an empty token"

LAST_CH="${TOKEN: -1}"
FLIP_CH=$(printf '%s' "${LAST_CH}" | tr 'A-Za-z' 'a-zA-Z')
[[ "${FLIP_CH}" == "${LAST_CH}" ]] && FLIP_CH=x
TAMPERED="${TOKEN%?}${FLIP_CH}"

CODE=$(bootstrap_post "${TAMPERED}")
if [[ "${CODE}" == "401" ]]; then
    ok "F4a PASS: tampered minted token → 401 (TokenReview rejected the broken signature)"
    PASS=$((PASS + 1))
else
    die "F4a FAIL: tampered token returned ${CODE}, expected exactly 401"
fi

# Control with the UNTAMPERED mint: assert NOT 401 specifically — the row
# isolates the TokenReview verdict. This is a real token for the right SA
# (workspace-<WS4> exists because the pod runs under it) and the right
# audience, so auth MUST pass; the response past auth (200 on the valid
# body, 4xx on a shape mismatch) is the converge rows' territory, not
# F4a's pin. A 401 here means the audience/SA binding rejects valid mints.
CODE=$(bootstrap_post "${TOKEN}")
if [[ "${CODE}" != "401" ]]; then
    ok "F4a: untampered mint control → ${CODE} (auth passed)"
else
    die "F4a FAIL: untampered minted token → 401 — TokenReview path rejects a valid SA+audience mint"
fi

# --- F4b: expired mint (time-travel past the JWT's OWN exp) → exactly 401;
# a fresh mint must then still work (rotation/fresh-read equivalence: a
# fresh credential always works). Bounded: default mints live ~1h, so the
# sleep is ≤~3700s regardless of SUSPEND_SECONDS.
# JWT payload decode: base64url segment + jq @base64d (accepts the URL-safe
# alphabet and tolerates the "===" over-padding on any mod-4 remainder —
# validated against synthetic JWTs). Guard: a mint without .exp skips.
EXP=$(jq -R 'split(".")[1] + "===" | @base64d | fromjson | .exp' <<<"${TOKEN}")
if ! [[ "${EXP}" =~ ^[0-9]+$ ]]; then
    skip_row "F4b" "minted JWT carries no numeric .exp (got '${EXP}') — cannot time-travel past it"
else
    NOW=$(date +%s)
    WAIT_S=$(( EXP - NOW + 10 ))
    if (( WAIT_S > 3700 )); then
        skip_row "F4b" "server granted exp ${WAIT_S}s out — longer validity than the ~1h harness budget; needs a shorter-duration mint"
    else
        (( WAIT_S > 0 )) && sleep "${WAIT_S}"
        CODE=$(bootstrap_post "${TOKEN}")
        if [[ "${CODE}" != "401" ]]; then
            die "F4b FAIL: expired token (exp=${EXP}, slept ${WAIT_S}s) returned ${CODE}, expected exactly 401"
        fi
        ok "F4b: expired token → 401 (TokenReview enforces the mint's expiry)"

        FRESH=$(mint_token) || die "F4b: fresh mint failed after expiry leg"
        CODE=$(bootstrap_post "${FRESH}")
        if [[ "${CODE}" != "401" ]]; then
            ok "F4b PASS: expired → 401 AND fresh mint → ${CODE} (auth passed)"
            PASS=$((PASS + 1))
        else
            die "F4b FAIL: fresh minted token → 401 — a fresh credential must always authenticate"
        fi
    fi
fi

# -----------------------------------------------------------------------------
# F5 — chaos kills: sidecar container kill; pod-delete mid-bind
# -----------------------------------------------------------------------------
WS5=$(ws_id 5)
log "F5a — kill the agentd sidecar container → restart → converge"

seed_workspace "${WS5}" "${USER_ID}"
bind_env "${WS5}" "SD_F5" "chaos5-value"
wait_phase "${WS5}" Active 240 || die "F5a: workspace never Active"
secrets_converged "${WS5}" 120 || die "F5a: pre-kill secretsDelivery unhealthy"
if ! env_in_child "${WS5}" "SD_F5=chaos5-value"; then
    die "F5a: pre-kill env missing — setup broken"
fi
ok "F5a: pre-kill env present"

POD5=$(pod_of "${WS5}")
RS_BEFORE=$(kc get pod "${POD5}" -o jsonpath='{.status.containerStatuses[?(@.name=="agentd")].restartCount}' 2>/dev/null || echo 0)
RS_BEFORE="${RS_BEFORE:-0}"

# B3: kill the agentd container from the NODE via crictl. agentd is a
# FROM-scratch image (no kill binary) and a PID-1 SIGKILL raised from
# inside the pid-namespace is dropped — the in-pod exec can't do this.
# docker/crictl exist on kind nodes (S5.5 precedent,
# local/s5-overlay-validation.sh:413-417). Scope the lookup to POD5's own
# sandbox: plain `crictl ps --name agentd` is ambiguous by F5 time (the
# F1–F4 pods still run their own agentd sidecars).
NODE=$(kubectl --context "${CTX}" get nodes -o jsonpath='{.items[0].metadata.name}')
SBX=$(docker exec "${NODE}" crictl pods --name "^${POD5}\$" -q 2>/dev/null | head -1 || true)
CID=""
if [[ -n "${SBX}" ]]; then
    CID=$(docker exec "${NODE}" crictl ps --pod "${SBX}" --name agentd -q 2>/dev/null | head -1 || true)
fi
if [[ -z "${CID}" ]]; then
    skip_row "F5a" "no agentd container resolvable for ${POD5} on node ${NODE} (crictl pods/ps lookup empty — container-runtime access missing?)"
else
    docker exec "${NODE}" crictl stop --timeout 0 "${CID}" >/dev/null

    RS_AFTER="-1"
    for _i in $(seq 1 40); do
        RS_AFTER=$(kc get pod "${POD5}" -o jsonpath='{.status.containerStatuses[?(@.name=="agentd")].restartCount}' 2>/dev/null || echo 0)
        RS_AFTER="${RS_AFTER:-0}"
        (( RS_AFTER > RS_BEFORE )) && break
        sleep 3
    done
    if (( RS_AFTER > RS_BEFORE )); then
        ok "F5a: agentd container restarted (restartCount ${RS_BEFORE} → ${RS_AFTER})"
    else
        die "F5a FAIL: agentd restartCount never incremented after crictl stop"
    fi

    if secrets_converged "${WS5}" 120 && wait_env_present "${WS5}" "SD_F5=chaos5-value" 120; then
        ok "F5a PASS: sidecar restart converged (env re-delivered)"
        PASS=$((PASS + 1))
    else
        die "F5a FAIL: no convergence after sidecar kill"
    fi
fi

WS6=$(ws_id 6)
log "F5b — pod-delete mid-bind → recreate → converge"

seed_workspace "${WS6}" "${USER_ID}"
wait_phase "${WS6}" Active 240 || die "F5b: workspace never Active"
POD6=$(pod_of "${WS6}")
[[ -n "${POD6}" ]] || die "F5b: no pod to delete"

bind_env "${WS6}" "SD_F6B" "midbind-value"
kc delete pod "${POD6}" --ignore-not-found >/dev/null 2>&1 || true
ok "F5b: bind issued + pod deleted immediately"

wait_phase "${WS6}" Active 240 || die "F5b: workspace never re-Active after pod delete"
if secrets_converged "${WS6}" 300 && wait_env_present "${WS6}" "SD_F6B=midbind-value" 300; then
    ok "F5b PASS: pod recreated, mid-bind secret delivered (SD_F6B present)"
    PASS=$((PASS + 1))
else
    die "F5b FAIL: no convergence after pod-delete mid-bind"
fi

# -----------------------------------------------------------------------------
total_ms=$(( $(date +%s%3N) - total_start ))
log "US-70.0 fault/chaos suite complete — pass=${PASS} skip=${SKIP} fail=0 (${total_ms}ms)"
if (( ${#SKIPPED_ROWS[@]} > 0 )); then
    warn "loud skips: ${SKIPPED_ROWS[*]}"
    warn "skipped rows are environment gates (fault seam / crictl container access / token-validity shape), tracked above — never a silent pass"
fi
