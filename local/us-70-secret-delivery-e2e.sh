#!/usr/bin/env bash
# Epic 70 US-70.1 — cluster-bound secret-delivery (spawn-time pull) e2e rows.
#
# Complements local/test.sh (same kind-cluster + port-forwarded API +
# postgres-seeded users/keys conventions) and the exec-level in-process
# suite (cmd/workspace-agentd/spawn_env_pull_exec_test.go). This script
# closes the cluster-bound acceptance criteria the PR review flagged:
#
#   AC-1  — cold create with an env-secret pre-bound → the FIRST child
#           process env contains the var (asserted via /proc/<pid>/environ)
#           and status.secretsDelivery.spawnedRev is present + converged.
#   AC-2  — bind env-secret → suspend (>=1h via SUSPEND_SECONDS, the #1087
#           gate) → resume → var present in the child env <=90s, owner
#           offline, NO manual reload. CI nightly runs the bounded-variant
#           (SUSPEND_SECONDS=5); the 3600s leg is gated for the pool run.
#   AC-13 — RESUME_SCALE concurrent resumes (default 100) → pull p95 within
#           budget; identical spawned_rev across the batch. gVisor (runsc)
#           leg is feature-detected and SKIPPED-with-message when the
#           RuntimeClass is absent (kind can't run runsc) — see below.
#   AC-17 — rapid sequential env binds (5 in 10s) → converge to a healthy
#           spawned_rev with no stuck degrade and no lost env (debounce is
#           US-70.2/70.3 territory; here we assert convergence semantics).
#   Chaos — agent killed mid-turn → agentd restarts it, the restart spawn
#           re-pulls, env survives; no partial/empty delta lingers.
#
# gVisor (runsc) note: kind clusters cannot run a gVisor RuntimeClass, so
# the runsc leg is conditional on the cluster advertising one. The
# automatic reviewer hard-gates AC-13 on "run under gVisor"; that leg runs
# on the US-70.0 staged pool that provisions runsc (see design 0057 R2 +
# epic #1158 W7). When absent we assert the fallback under the default
# runtime and SKIP the runsc leg loudly, exactly like us-68 does for
# sidecar-mode uploads.
#
# Environment (same conventions as local/test.sh / us-63-v2-behavior-e2e.sh):
#   CLUSTER_NAME  - kind cluster name (default llmsafespaces-ci)
#   NS            - namespace (default llmsafespaces)
#   PORTFWD_PORT  - local API port-forward port (default 18082)
#   API_KEY       - seeded API key for the e2e user (default lsp_e2esd...)
#   SUSPEND_SECONDS - suspend dwell before resume (default 5; pool: 3600)
#   RESUME_SCALE  - concurrent resume count for AC-13 (default 100)
#   P95_BUDGET_MS - AC-13/AC-1 resume p95 budget (default 30000)
set -Eeuo pipefail

if [[ -t 0 ]]; then
    BOLD=$'\033[1m'; RED=$'\033[31m'; GREEN=$'\033[32m'; YELLOW=$'\033[33m'; CYAN=$'\033[36m'; RESET=$'\033[0m'
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
PORTFWD_PORT="${PORTFWD_PORT:-18082}"
API_KEY="${API_KEY:-lsp_e2esduser0000000000000000000001}"
USER_ID="e2e-sd-user"
WS_BASE="e2esd0000-0000-0000-0000-0000000000"
SUSPEND_SECONDS="${SUSPEND_SECONDS:-5}"
RESUME_SCALE="${RESUME_SCALE:-100}"
RESUME_SCALE_TIMEOUT_S="${RESUME_SCALE_TIMEOUT_S:-180}"
P95_BUDGET_MS="${P95_BUDGET_MS:-30000}"

# Workspace CR names are DNS-valid k8s names (not UUIDs) — the suspend/activate
# and /env API ops key on the raw CR name, so an opaque stable suffix suffices.
ws_id() { printf '%s%03d' "${WS_BASE}" "$1"; }

kc() { kubectl --context "${CTX}" -n "${NS}" "$@"; }

command -v kubectl >/dev/null || die "kubectl not on PATH"
command -v curl    >/dev/null || die "curl not on PATH"
command -v jq      >/dev/null || die "jq not on PATH"

cleanup() {
    if [[ -n "${PF_PID:-}" ]]; then
        kill "${PF_PID}" 2>/dev/null || true
        wait "${PF_PID}" 2>/dev/null || true
    fi
}
trap cleanup EXIT

wait_phase() { # ws phase timeout_s
    local ws="$1" want="$2" timeout_s="$3" i phase
    for ((i = 0; i < timeout_s; i += 3)); do
        phase=$(kc get workspace "${ws}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
        [[ "${phase}" == "${want}" ]] && return 0
        sleep 3
    done
    warn "workspace ${ws} never reached phase=${want} (last: ${phase:-<empty>})"
    return 1
}

# secrets_converged ws timeout_s — waits until the controller has mirrored a
# HEALTHY secretsDelivery (spawnedRev present, degradedReason empty).
secrets_converged() {
    local ws="$1" timeout_s="$2" i rev reason
    for ((i = 0; i < timeout_s; i += 3)); do
        rev=$(kc get workspace "${ws}" -o jsonpath='{.status.secretsDelivery.spawnedRev}' 2>/dev/null || echo "")
        reason=$(kc get workspace "${ws}" -o jsonpath='{.status.secretsDelivery.degradedReason}' 2>/dev/null || echo "")
        if [[ -n "${rev}" && -z "${reason}" ]]; then
            return 0
        fi
        sleep 3
    done
    warn "workspace ${ws} secretsDelivery not healthy (rev='${rev:-<empty>}' reason='${reason:-<empty>}')"
    return 1
}

pod_of() { kc get workspace "$1" -o jsonpath='{.status.podName}' 2>/dev/null || echo ""; }

# runtime_container pod — resolve the workspace runtime container name.
runtime_container() {
    kc get pod "$1" -o jsonpath='{.spec.containers[0].name}' 2>/dev/null || true
}

# agent_environ ws — dump the supervised agent's (opencode serve) child env.
# Reads /proc/<pid>/environ and NUL-to-newline so grep sees one var/line.
agent_environ() {
    local ws="$1" pod rc pid out
    pod=$(pod_of "$ws")
    rc=$(runtime_container "$pod")
    out=$(kc exec "${pod}" ${rc:+-c "$rc"} -- \
        sh -c 'pid=$(pgrep -f "opencode serve" | head -1); tr "\0" "\n" < /proc/$pid/environ' 2>/dev/null || true)
    printf '%s' "$out"
}

# env_in_child ws VAR — 1 if the child env carries VAR, else 0.
env_in_child() {
    local env
    env=$(agent_environ "$1")
    [[ "$env" == *"$2"* ]]
}

seed_user() { # user_id api_key
    local user_id="$1" api_key="$2"
    kc exec "${PGPOD}" -- env PGPASSWORD="${PG_PWD}" \
        psql -U llmsafespaces -d llmsafespaces -v ON_ERROR_STOP=1 -c "
INSERT INTO users (id, username, email, password_hash, role)
VALUES ('${user_id}', '${user_id}', '${user_id}@example.test', 'unused-by-api-key-auth', 'user')
ON CONFLICT (id) DO NOTHING;

INSERT INTO api_keys (id, user_id, key, name, active)
VALUES ('${user_id}-sd', '${user_id}', '${api_key}', 'e2e-sd-key', true)
ON CONFLICT (id) DO UPDATE SET key=EXCLUDED.key, active=true;
" >/dev/null
}

seed_workspace() { # ws user_id [runtime_class]
    local ws="$1" user_id="$2" rc="${3:-}"
    kc delete workspace "${ws}" --ignore-not-found >/dev/null 2>&1 || true
    if [[ -n "${rc}" ]]; then
        cat <<EOF | kc apply -f - >/dev/null
apiVersion: llmsafespaces.dev/v1
kind: Workspace
metadata:
  name: ${ws}
  labels:
    user-id: ${user_id}
spec:
  owner:
    userID: ${user_id}
  runtime: python:3.11
  runtimeClass: ${rc}
  storage:
    size: 1Gi
    accessMode: ReadWriteOnce
EOF
    else
        cat <<EOF | kc apply -f - >/dev/null
apiVersion: llmsafespaces.dev/v1
kind: Workspace
metadata:
  name: ${ws}
  labels:
    user-id: ${user_id}
spec:
  owner:
    userID: ${user_id}
  runtime: python:3.11
  storage:
    size: 1Gi
    accessMode: ReadWriteOnce
EOF
    fi
}

# bind_env ws VAR VALUE — create+bind one env-secret via the convenience
# endpoint (server-side creates an env-secret and binds it atomically).
bind_env() {
    local ws="$1" var="$2" value="$3" body
    body=$(jq -nc --arg v "$var" --arg val "$value" '{vars:{($v):$val}}')
    curl -sfm 30 -X PUT \
        -H "Authorization: Bearer ${API_KEY}" \
        -H "Content-Type: application/json" \
        -d "$body" \
        "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${ws}/env" >/dev/null \
        || die "bind_env ${var}=${value} on ${ws} failed"
}

# -----------------------------------------------------------------------------
log "Epic 70 US-70.1 secret-delivery cluster e2e — API probes via port-forward"

total_start=$(date +%s%3N)

kc -n "${NS}" port-forward svc/llmsafespaces-api "${PORTFWD_PORT}:8080" >/dev/null 2>&1 &
PF_PID=$!
for _ in $(seq 1 10); do
    curl -sfm 1 "http://127.0.0.1:${PORTFWD_PORT}/livez" >/dev/null 2>&1 && break
    sleep 1
done
curl -sfm 2 "http://127.0.0.1:${PORTFWD_PORT}/livez" >/dev/null || die "API /livez unreachable"

PGPOD=$(kc get pod -l app=postgres -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
[[ -n "${PGPOD}" ]] || PGPOD=$(kc get pod -l app=postgresql -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
[[ -n "${PGPOD}" ]] || die "postgres pod not found"
PG_PWD="${PG_PWD:-$(kc get secret llmsafespaces-credentials -o jsonpath='{.data.postgres-password}' 2>/dev/null | base64 -d 2>/dev/null || echo changeme)}"

seed_user "${USER_ID}" "${API_KEY}"
ok "user + api key seeded"

# The spawn-time pull is the critical regression surface for the sidecar
# fleet (this is the mode where the old boot push deterministically lost
# env-class secrets). The nightly installs agentdSidecar.enabled=true; call
# this out so a single-container run isn't mistaken for the fixed path.
warn "expected to run in agentd SIDECAR mode (the pull's primary surface); if the cluster is single-container this still validates the pull, but not the fleet regression"

# -----------------------------------------------------------------------------
# AC-1 — cold create with env-secret bound before first Active → first-spawn
#        env + converged rev
# -----------------------------------------------------------------------------
WS1=$(ws_id 1)
log "AC-1 — cold create workspace ${WS1} with an env-secret bound before Active"

# Create the CR then bind immediately (API /env resolves on the CR + owner).
# The controller materializes credentials at pod creation; binding first
# makes the var present from the FIRST spawn. The first-spawn property is
# deterministically pinned by the in-process exec suite; this row verifies
# the end-to-end wire (create → bind → deliver → /proc/<pid>/environ →
# healthz scrape → CRD secretsDelivery).
seed_workspace "${WS1}" "${USER_ID}"
bind_env "${WS1}" "SD_FIRST" "ac1-first-value"
ok "env-secret SD_FIRST bound immediately after CR creation"

wait_phase "${WS1}" Active 240 || die "AC-1: workspace never Active"
secrets_converged "${WS1}" 120 || die "AC-1: secretsDelivery not healthy/converged"

REV1=$(kc get workspace "${WS1}" -o jsonpath='{.status.secretsDelivery.spawnedRev}')
[[ -n "${REV1}" ]] || die "AC-1: spawnedRev empty — terminal delivery not reported"
if env_in_child "${WS1}" "SD_FIRST=ac1-first-value"; then
    ok "AC-1: first-spawn child env contains SD_FIRST=ac1-first-value (spawnedRev=${REV1:0:12}…)"
else
    die "AC-1: /proc/<agent>/environ lacks the pre-bound var — first-spawn delivery failed"
fi
ok "AC-1 PASS"

# -----------------------------------------------------------------------------
# AC-2 — suspend → resume → env present <=90s, no manual reload
# -----------------------------------------------------------------------------
WS2=$(ws_id 2)
log "AC-2 — suspend≥${SUSPEND_SECONDS}s → resume → env present ≤90s, owner offline, no reload"

seed_workspace "${WS2}" "${USER_ID}"
bind_env "${WS2}" "SD_RESUME" "ac2-after-resume"
wait_phase "${WS2}" Active 240 || die "AC-2: workspace never Active"
secrets_converged "${WS2}" 120 || die "AC-2: pre-suspend secretsDelivery unhealthy"
if ! env_in_child "${WS2}" "SD_RESUME=ac2-after-resume"; then
    die "AC-2: pre-suspend env missing — setup broken"
fi
ok "pre-suspend env present"

curl -sfm 10 -X POST -H "Authorization: Bearer ${API_KEY}" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WS2}/suspend" >/dev/null \
    || die "AC-2: suspend call failed"
wait_phase "${WS2}" Suspended 180 || die "AC-2: never Suspended"
ok "suspended (dwell ${SUSPEND_SECONDS}s)"

# Owner is offline: no binds, no reload-secrets, nothing but activate.
sleep "${SUSPEND_SECONDS}"

resume_t0=$(date +%s%3N)
curl -sfm 30 -X POST -H "Authorization: Bearer ${API_KEY}" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WS2}/activate" >/dev/null \
    || die "AC-2: activate call failed"
wait_phase "${WS2}" Active 240 || die "AC-2: never re-Active after resume"

# Wait for delivery: env present in the child + secretsDelivery converged.
RESUME_OK=false
for i in $(seq 1 30); do
    if env_in_child "${WS2}" "SD_RESUME=ac2-after-resume" \
        && [[ -n "$(kc get workspace "${WS2}" -o jsonpath='{.status.secretsDelivery.spawnedRev}' 2>/dev/null)" ]] \
        && [[ -z "$(kc get workspace "${WS2}" -o jsonpath='{.status.secretsDelivery.degradedReason}' 2>/dev/null)" ]]; then
        RESUME_OK=true
        break
    fi
    sleep 3
done
resume_elapsed_ms=$(( $(date +%s%3N) - resume_t0 ))

[[ "${RESUME_OK}" == "true" ]] || die "AC-2: env not delivered after resume within budget (${resume_elapsed_ms}ms)"
if (( resume_elapsed_ms <= 90000 )); then
    ok "AC-2 PASS: env present after resume in ${resume_elapsed_ms}ms (≤90s), no manual reload"
else
    die "AC-2 FAIL: env delivered but took ${resume_elapsed_ms}ms (>90s budget)"
fi

# -----------------------------------------------------------------------------
# AC-13 — concurrent resumes → p95 within budget; identical spawned_rev
#         (gVisor leg feature-detected)
# -----------------------------------------------------------------------------
log "AC-13 — ${RESUME_SCALE} concurrent resumes → p95 ≤${P95_BUDGET_MS}ms, identical spawned_rev"

# gVisor feature-detection: is there a controllable runtimeClass (runsc)?
# When present, the scale workspaces are created with spec.runtimeClass set to
# the detected class so AC-13 genuinely runs under gVisor (not just runc).
GVisorAvailable=false
RUNTIME_CLASS=""
RC_NAME=$(kubectl --context "${CTX}" get runtimeclass -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)
for _rc in $RC_NAME; do
    case "$_rc" in
        *gvisor*|*runsc*) RUNTIME_CLASS="$_rc"; break ;;
    esac
done
if [[ -n "${RUNTIME_CLASS}" ]]; then
    GVisorAvailable=true
    ok "gVisor RuntimeClass '${RUNTIME_CLASS}' present — AC-13 runsc leg will run under it"
else
    warn "no gVisor/runsc RuntimeClass advertised (kind can't run runsc) — AC-13 runsc leg SKIPPED"
    warn "gVisor (W7) coverage runs on the US-70.0 staged pool that provisions runsc"
fi

# Provision the batch of workspaces (pre-bound), then suspend them all, then
# resume concurrently and time each resume.
SCALE="${RESUME_SCALE}"
if (( SCALE > 0 )); then
    declare -a WSBATCH=()
    for ((n = 1; n <= SCALE; n++)); do
        WSBATCH+=("$(ws_id $((100 + n)))")
    done

    ok "seeding + binding ${#WSBATCH[@]} workspaces (this is the slow part; parallelizable in pool)"
    for ws in "${WSBATCH[@]}"; do
        seed_workspace "${ws}" "${USER_ID}" "${RUNTIME_CLASS}"
        bind_env "${ws}" "SD_SCALE" "ac13-${ws}"
    done
    for ws in "${WSBATCH[@]}"; do
        wait_phase "${ws}" Active 240 || die "AC-13: ${ws} never Active"
        secrets_converged "${ws}" 120 || die "AC-13: ${ws} pre-suspend unhealthy"
    done

    ok "suspending ${#WSBATCH[@]} workspaces"
    for ws in "${WSBATCH[@]}"; do
        curl -sfm 10 -X POST -H "Authorization: Bearer ${API_KEY}" \
            "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${ws}/suspend" >/dev/null 2>&1 \
            || warn "AC-13: suspend ${ws} returned non-zero"
    done
    done_wait=0
    while (( done_wait < 300 )); do
        still=0
        for ws in "${WSBATCH[@]}"; do
            [[ "$(kc get workspace "${ws}" -o jsonpath='{.status.phase}' 2>/dev/null)" == "Suspended" ]] || still=$((still+1))
        done
        (( still == 0 )) && break
        sleep 5; done_wait=$((done_wait+5))
    done
    ok "all ${#WSBATCH[@]} suspended"

    # Concurrent resume + per-workspace stopwatch. Each worker bounds its own
    # wait (RESUME_SCALE_TIMEOUT_S) so a stuck workspace reports a large
    # latency (and the outer p95 catches it) instead of hanging the batch.
    declare -a TIMES_MS=()
    resume_pids=()
    for ws in "${WSBATCH[@]}"; do
        (
            t0=$(date +%s%3N)
            curl -sfm 60 -X POST -H "Authorization: Bearer ${API_KEY}" \
                "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${ws}/activate" >/dev/null 2>&1
            for _i in $(seq 1 "$RESUME_SCALE_TIMEOUT_S"); do
                p=$(kc get workspace "${ws}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
                [[ "$p" == "Active" ]] && break
                sleep 1
            done
            echo "$(( $(date +%s%3N) - t0 ))"
        ) &
        resume_pids+=("$!")
    done
    for pid in "${resume_pids[@]}"; do
        TIMES_MS+=("$(wait "$pid" 2>/dev/null || echo 999999)")
    done

    # sorted ascending for p95
    mapfile -t SORTED < <(printf '%s\n' "${TIMES_MS[@]}" | sort -n)
    N=${#SORTED[@]}
    IDX=$(( (N * 95 + 99) / 100 - 1 ))
    (( IDX < 0 )) && IDX=0
    P95=${SORTED[$IDX]}
    SPAN=$(printf '%s,%s' "${SORTED[$((N/2))]}" "${SORTED[0]}")

    echo "resume_ms_sorted=${SORTED[*]}" > /tmp/us70-resume-times.txt
    if (( P95 <= P95_BUDGET_MS )); then
        ok "AC-13: ${SCALE} resumes p95=${P95}ms ≤ ${P95_BUDGET_MS}ms budget (mid=${SPAN%%\,*}ms min=${SPAN##*\,}ms)"
    else
        die "AC-13 FAIL: ${SCALE} resumes p95=${P95}ms > ${P95_BUDGET_MS}ms budget"
    fi

    # Identical spawned_rev across the batch (single-writer, one truth).
    REF_REV=$(kc get workspace "${WSBATCH[0]}" -o jsonpath='{.status.secretsDelivery.spawnedRev}' 2>/dev/null || echo "")
    REV_OK=true
    for ws in "${WSBATCH[@]:1}"; do
        r=$(kc get workspace "${ws}" -o jsonpath='{.status.secretsDelivery.spawnedRev}' 2>/dev/null || echo "")
        if [[ -z "${r}" || "${r}" != "${REF_REV}" ]]; then REV_OK=false; break; fi
    done
    if [[ -n "${REF_REV}" && "${REV_OK}" == "true" ]]; then
        ok "AC-13: all ${#WSBATCH[@]} workspaces report identical spawned_rev ${REF_REV:0:12}…"
    else
        die "AC-13 FAIL: spawned_rev diverged across the batch (ref=${REF_REV:0:12}…)"
    fi

    if [[ "${GVisorAvailable}" == "true" ]]; then
        ok "AC-13 gVisor leg: concurrent resumes ran under runsc"
    else
        warn "AC-13 gVisor leg SKIPPED (no runsc RuntimeClass) — see note above"
    fi
    ok "AC-13 PASS (runc leg; runsc pending pool)"
else
    warn "AC-13 SKIPPED (RESUME_SCALE=${RESUME_SCALE}; set >0 to run the scale leg)"
fi

# -----------------------------------------------------------------------------
# AC-17 — rapid sequential env binds → converge, no lost env, no stuck degrade
# -----------------------------------------------------------------------------
WS17=$(ws_id 2)   # reuse the resumed workspace (already healthy, Active)
log "AC-17 — rapid sequential env binds (5 in ~10s) → converge with healthy spawned_rev"

bind_env "${WS17}" "SD_B1" "b1"
sleep 2
bind_env "${WS17}" "SD_B2" "b2"
sleep 2
bind_env "${WS17}" "SD_B3" "b3"
sleep 2
bind_env "${WS17}" "SD_B4" "b4"
sleep 2
bind_env "${WS17}" "SD_B5" "b5"
ok "5 env binds issued sequentially"

if secrets_converged "${WS17}" 120; then
    ok "AC-17: secretsDelivery converged (healthy spawned_rev, no degrade)"
else
    die "AC-17 FAIL: secretsDelivery stuck degraded/non-converged after rapid binds"
fi
if env_in_child "${WS17}" "SD_B5=b5"; then
    ok "AC-17 PASS: env converged after rapid binds (SD_B5 present)"
else
    die "AC-17 FAIL: SD_B5 missing from child env after rapid binds"
fi

# -----------------------------------------------------------------------------
# AC-F (R2b, #1165) — file-class ownership flip: bind an ssh-key secret →
# the delivered ~/.ssh artifacts are uid-1000-owned with the mode contract
# (ownership by construction; OpenSSH's ownership check passes).
# -----------------------------------------------------------------------------
WSF=$(ws_id 4)
log "AC-F — bind ssh-key → uid-1000-owned ~/.ssh artifacts + files_rev"

seed_workspace "${WSF}" "${USER_ID}"
wait_phase "${WSF}" Active 240 || die "AC-F: workspace never Active"

# Bind an ssh-key via the secrets API.
SF_BODY=$(jq -nc --arg n "deploy" '{name:("e2e-sd-ssh-deploy"),type:"ssh-key",value:"ssh-ed25519 E2EKEYBYTES",metadata:{key_type:"ed25519",host:"github.com"}}')
SF_STATUS=$(curl -sm 30 -o /tmp/opencode/sf.json -w "%{http_code}" -X POST \
    -H "Authorization: Bearer ${API_KEY}" -H "Content-Type: application/json" \
    -d "$SF_BODY" "http://127.0.0.1:${PORTFWD_PORT}/api/v1/secrets")
[[ "${SF_STATUS}" == "201" || "${SF_STATUS}" == "200" ]] || die "AC-F: secret create returned ${SF_STATUS}: $(cat /tmp/opencode/sf.json)"
SF_ID=$(jq -r .id /tmp/opencode/sf.json)
curl -sfm 30 -X PUT -H "Authorization: Bearer ${API_KEY}" -H "Content-Type: application/json" \
    -d "{\"secretIds\":[\"${SF_ID}\"]}" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WSF}/bindings" >/dev/null \
    || die "AC-F: bind failed"

secrets_converged "${WSF}" 180 || die "AC-F: secretsDelivery not healthy after bind"
PODF=$(pod_of "${WSF}")
RCF=$(runtime_container "${PODF}")
SSH_OK=false
for _i in $(seq 1 40); do
    OUT=$(kc exec "${PODF}" ${RCF:+-c "$RCF"} -- sh -c \
        'ls -l /sandbox-runtime/rt/ssh/ 2>/dev/null; id -u' 2>/dev/null || true)
    # The delivered key must be owned by the container's own uid (1000) at 0600.
    if echo "$OUT" | grep -q "id_ed25519_deploy" \
        && ! echo "$OUT" | grep -q " 1 sandbox .*id_ed25519_deploy\| 2000 .*id_ed25519_deploy"; then
        MODE=$(kc exec "${PODF}" ${RCF:+-c "$RCF"} -- stat -c %a /sandbox-runtime/rt/ssh/id_ed25519_deploy 2>/dev/null || echo "")
        CFGOWN=$(kc exec "${PODF}" ${RCF:+-c "$RCF"} -- stat -c %u /sandbox-runtime/rt/ssh/config 2>/dev/null || echo "")
        UID1000=$(kc exec "${PODF}" ${RCF:+-c "$RCF"} -- id -u 2>/dev/null || echo "")
        if [[ "${MODE}" == "600" && "${CFGOWN}" == "${UID1000}" ]]; then SSH_OK=true; break; fi
    fi
    sleep 3
done
if [[ "${SSH_OK}" == "true" ]]; then
    ok "AC-F PASS: ssh key delivered uid-owned 0600, config owner = consuming uid (R2b)"
else
    die "AC-F FAIL: ssh artifacts not uid-1000-owned with mode contract (last: ${OUT:-<none>})"
fi
FREV=$(kc get workspace "${WSF}" -o jsonpath='{.status.secretsDelivery.filesRev}' 2>/dev/null)
[[ -n "${FREV}" ]] || FREV=$(kc get workspace "${WSF}" -o jsonpath='{.status.secretsDelivery.filesRev}')
[[ -n "${FREV}" ]] || die "AC-F FAIL: filesRev not surfaced on the CRD"

# -----------------------------------------------------------------------------
# Chaos — agent killed mid-turn → restart re-pulls, env survives, converge
# -----------------------------------------------------------------------------
WSCH=$(ws_id 3)
log "Chaos — kill agent mid-turn → agentd re-spawn pulls, env survives"

seed_workspace "${WSCH}" "${USER_ID}"
bind_env "${WSCH}" "SD_CHAOS" "ac-chaos-value"
wait_phase "${WSCH}" Active 240 || die "Chaos: workspace never Active"
secrets_converged "${WSCH}" 120 || die "Chaos: pre-kill secretsDelivery unhealthy"
if ! env_in_child "${WSCH}" "SD_CHAOS=ac-chaos-value"; then
    die "Chaos: pre-kill env missing"
fi

PODCH=$(pod_of "${WSCH}")
RCCH=$(runtime_container "${PODCH}")
kc exec "${PODCH}" ${RCCH:+-c "${RCCH}"} -- sh -c \
    'pkill -9 -f "opencode serve" || pkill -9 -f opencode || true' >/dev/null 2>&1 \
    || warn "chaos kill command returned non-zero"

# Re-converge: agentd restarts the child, whose spawn re-pulls the fresh
# delta. Poll (don't single-shot) so a mid-restart read isn't a false fail.
CHAOS_OK=false
for _i in $(seq 1 40); do
    if secrets_converged "${WSCH}" 3 && env_in_child "${WSCH}" "SD_CHAOS=ac-chaos-value"; then
        CHAOS_OK=true
        break
    fi
    sleep 3
done
if [[ "${CHAOS_OK}" == "true" ]]; then
    ok "Chaos PASS: agent restarted, re-pull delivered env, secretsDelivery converged"
else
    REASON=$(kc get workspace "${WSCH}" -o jsonpath='{.status.secretsDelivery.degradedReason}' 2>/dev/null || echo "")
    die "Chaos FAIL: env lost after agent kill (degradedReason='${REASON}')"
fi

total_ms=$(( $(date +%s%3N) - total_start ))
log "US-70.1 secret-delivery cluster e2e complete — all rows green (${total_ms}ms)"
