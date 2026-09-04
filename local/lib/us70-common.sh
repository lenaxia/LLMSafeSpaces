#!/usr/bin/env bash
# us70-common.sh — shared harness helpers for the Epic 70 delivery suites
# (local/us-70-secret-delivery-e2e.sh, local/us-70-faults-e2e.sh).
# Extracted verbatim from local/us-70-secret-delivery-e2e.sh (US-70.0);
# the row logic lives in the sourcing scripts.
#
# Environment (same conventions as local/test.sh / us-63-v2-behavior-e2e.sh):
#   CLUSTER_NAME  - kind cluster name (default llmsafespaces-ci)
#   CTX           - kubectl context (default kind-$CLUSTER_NAME)
#   NS            - namespace (default llmsafespaces)
#   PORTFWD_PORT  - local API port-forward port (default 18082)
#   API_KEY       - seeded API key for the e2e user (default lsp_e2esd...)
#   USER_ID       - seeded e2e user (default e2e-sd-user)
#   WS_BASE       - workspace CR name prefix (default e2esd0000-…; the
#                   fault suite overrides with a distinct prefix)
#   SUSPEND_SECONDS - suspend dwell before resume (default 5; pool: 3600)
#   RESUME_SCALE  - concurrent resume count for AC-13 (default 100)
#   RESUME_SCALE_TIMEOUT_S - per-workspace resume wait (default 180)
#   P95_BUDGET_MS - unused knob kept for nightly compat (AC-13 no longer gates on it)
#   RECONCILE_INTERVAL_S - API secrets-reconcile loop period in seconds;
#                   MUST match the helm set
#                   api.extraEnv[LLMSAFESPACES_SECRETS_RECONCILE_INTERVAL]
#                   in e2e-nightly.yml / us-70-delivery-pool.yml (5s) —
#                   the AC-8/AC-10 convergence budgets are derived from it.
#   RESYNC_PORT   - local port for the pod resync-endpoint port-forward
#                   (default 18099)

[[ -n ${__US70_COMMON:-} ]] && return 0
__US70_COMMON=1

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
USER_ID="${USER_ID:-e2e-sd-user}"
# WS_BASE must be a valid UUID: production mints workspaceID = uuid.New() and
# the CR name IS that UUID (workspace_service.go CreateWorkspace), so every
# API workspace op resolves WHERE workspaces.id = $1 (uuid column) against the
# CR name. ws_id keeps the base's first 32 chars and appends a 4-digit suffix.
WS_BASE="${WS_BASE:-e2e5d000-0000-4000-8000-000000000000}"
SUSPEND_SECONDS="${SUSPEND_SECONDS:-5}"
RESUME_SCALE="${RESUME_SCALE:-100}"
RESUME_SCALE_TIMEOUT_S="${RESUME_SCALE_TIMEOUT_S:-180}"
P95_BUDGET_MS="${P95_BUDGET_MS:-30000}" # unused by the pool (see us-70-secret-delivery-e2e.sh AC-13)
RECONCILE_INTERVAL_S="${RECONCILE_INTERVAL_S:-5}"
RESYNC_PORT="${RESYNC_PORT:-18099}"

ws_id() { printf '%s%04d\n' "${WS_BASE:0:32}" "$1"; }

kc() { kubectl --context "${CTX}" -n "${NS}" "$@"; }

# kc_apply_retry — reads a manifest on stdin, applies it with bounded
# retries on transient failures. The dind runner's etcd blips under
# controller churn ("etcdserver: request timed out", run 33820116556
# died on the first one during AC-13 seeding); apply is idempotent so
# retrying is safe. Stdin is buffered — a retry must not see empty input.
kc_apply_retry() {
    local manifest attempt
    manifest=$(cat)
    for attempt in 1 2 3 4 5; do
        if printf '%s' "$manifest" | kc apply -f - >/dev/null 2>&1; then return 0; fi
        warn "kc apply failed (attempt ${attempt}/5) — transient apiserver/etcd error, retrying in 3s"
        sleep 3
    done
    die "kc apply failed after 5 attempts: ${manifest:0:120}…"
}

command -v kubectl >/dev/null || die "kubectl not on PATH"
command -v curl    >/dev/null || die "curl not on PATH"
command -v jq      >/dev/null || die "jq not on PATH"

cleanup() {
    if [[ -n "${PF_PID:-}" ]]; then
        kill "${PF_PID}" 2>/dev/null || true
        wait "${PF_PID}" 2>/dev/null || true
    fi
    if [[ -n "${RESC_PF_PID:-}" ]]; then
        kill "${RESC_PF_PID}" 2>/dev/null || true
        wait "${RESC_PF_PID}" 2>/dev/null || true
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
    diagnose_workspace "${ws}"
    return 1
}

# diagnose_workspace dumps why a workspace is stuck: CR status/conditions,
# pod state, container logs (agentd first — the sidecar gates the main
# container), controller tail, and recent events. Diverging blind
# retry-fix cycles (pool runs 4-5) each cost ~40 minutes; this makes the
# next failure legible in the log itself.
diagnose_workspace() { # ws
    local ws="$1" pod
    pod=$(pod_of "${ws}")
    {
        echo "===== DIAGNOSE ${ws} ====="
        echo "--- CR status"
        kc get workspace "${ws}" -o yaml 2>/dev/null | sed -n '/^status:/,$p'
        echo "--- pod ${pod:-<none>}"
        [[ -n "${pod}" ]] && kc describe pod "${pod}" 2>/dev/null | tail -45
        echo "--- container logs"
        if [[ -n "${pod}" ]]; then
            for c in agentd workspace opencode platform-bootstrap platform-init platform-materialize; do
                if kc logs "${pod}" -c "${c}" --tail=60 2>/dev/null; then
                    echo "--- (container ${c} above)"
                    break
                fi
            done
        fi
        echo "--- controller tail"
        kc logs deployment/llmsafespaces-controller --tail=50 2>/dev/null
        echo "--- recent events"
        kc get events --sort-by=.lastTimestamp 2>/dev/null | tail -25
        echo "===== END DIAGNOSE ====="
    } >&2
}

# secrets_converged ws timeout_s — waits until the controller has mirrored a
# HEALTHY secretsDelivery (spawnedRev present, degradedReason empty).
# Empty rev + empty reason means the controller has not successfully scraped
# the pod's healthz yet (or cleared it as unreachable) — under pool-scale
# load the serial scrape cycle can lag well behind wait_phase Active.
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
    diagnose_workspace "${ws}"
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

# harness_start — establish the API port-forward, wait for /livez, find the
# postgres pod, read PG_PWD, and seed the e2e user + API key. Safe to call
# once from either sourcing script (after their row preamble).
harness_start() {
    kc -n "${NS}" port-forward svc/llmsafespaces-api "${PORTFWD_PORT}:8080" >/dev/null 2>&1 &
    PF_PID=$!
    local _i
    for _i in $(seq 1 10); do
        curl -sfm 1 "http://127.0.0.1:${PORTFWD_PORT}/livez" >/dev/null 2>&1 && break
        sleep 1
    done
    curl -sfm 2 "http://127.0.0.1:${PORTFWD_PORT}/livez" >/dev/null || die "API /livez unreachable"

    PGPOD=$(kc get pod -l app=postgres -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
    [[ -n "${PGPOD}" ]] || PGPOD=$(kc get pod -l app=postgresql -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
    [[ -n "${PGPOD}" ]] || die "postgres pod not found"
    PG_PWD="${PG_PWD:-$(kc get secret llmsafespaces-credentials -o jsonpath='{.data.postgres-password}' 2>/dev/null | base64 -d 2>/dev/null || echo changeme)}"

    seed_session "${USER_ID}"
    ok "harness session seeded (OWNER_ID=${OWNER_ID}; API key for resolution ops, JWT for DEK-gated secret authoring)"

    # The spawn-time pull is the critical regression surface for the sidecar
    # fleet (this is the mode where the old boot push deterministically lost
    # env-class secrets). The nightly installs agentdSidecar.enabled=true; call
    # this out so a single-container run isn't mistaken for the fixed path.
    warn "expected to run in agentd SIDECAR mode (the pull's primary surface); if the cluster is single-container this still validates the pull, but not the fleet regression"
}


# api_key_db_hash <plaintext> — the value the API stores/expects in
# api_keys.key: sha256(plaintext) hex, matching validateAPIKey's
# sha256.Sum256 -> hex.EncodeToString (auth.go). Pinned by
# local/us70_common_test.go against an independent sha256.
# >>> api-key-db-hash
api_key_db_hash() {
    local plaintext="$1" escaped
    # SQL string doubling for embedded single quotes (correctness; the
    # harness mints [A-Za-z0-9_] keys, but the helper must not corrupt
    # or break on anything else).
    escaped=${plaintext//\'/\'\'}
    printf "encode(sha256(convert_to('%s', 'UTF8')), 'hex')" "${escaped}"
}
# <<< api-key-db-hash

# seed_session registers the harness user through the API when needed and
# logs in. Secret AUTHORING (CreateSecret) resolves the user's DEK via the
# session cache — an API-key request carries no session, so binds would 500
# with ErrDEKUnavailable. Registration also provisions user_keys under the
# server KEK (the epic-58 default), and with no email verifier wired the
# user is email_verified immediately. OWNER_ID is the users.id UUID the API
# mints — ownership (api_keys, workspaces, CR spec.owner) must carry it,
# not the username. The API-key row stores the sha256 hash
# (api_key_db_hash) — the plaintext never authenticates.
HARNESS_PASSWORD="${HARNESS_PASSWORD:-us70-pool-pw-2026}"
# Direct image ref the controller can resolve on its own (loaded onto the
# kind node by the pool workflow). NOT a catalog name like python:3.11 —
# those resolve only through the API's create path (pool run 5 lesson).
RUNTIME_REF="${RUNTIME_REF:-ghcr.io/lenaxia/llmsafespaces/runtime-base:${IMAGE_TAG:-ci}}"
AUTH_TOKEN=""
OWNER_ID=""

login_harness_user() {
    AUTH_TOKEN=$(curl -sfm 15 -X POST "http://127.0.0.1:${PORTFWD_PORT}/api/v1/auth/login" \
        -H "Content-Type: application/json" \
        -d "{\"email\":\"${USER_ID}@example.test\",\"password\":\"${HARNESS_PASSWORD}\"}" \
        | jq -r '.token // empty' 2>/dev/null) || AUTH_TOKEN=""
}

seed_session() { # user_id
    local user_id="$1"
    login_harness_user
    if [[ -z "${AUTH_TOKEN}" ]]; then
        local reg
        reg=$(curl -sm 15 -X POST "http://127.0.0.1:${PORTFWD_PORT}/api/v1/auth/register" \
            -H "Content-Type: application/json" \
            -d "{\"username\":\"${user_id}\",\"email\":\"${user_id}@example.test\",\"password\":\"${HARNESS_PASSWORD}\"}" || true)
        login_harness_user
        [[ -n "${AUTH_TOKEN}" ]] || die "harness register/login failed: ${reg:-no body}"
    fi
    OWNER_ID=$(curl -sfm 15 "http://127.0.0.1:${PORTFWD_PORT}/api/v1/auth/me" \
        -H "Authorization: Bearer ${AUTH_TOKEN}" | jq -r '.user.id // .id // empty' 2>/dev/null) || OWNER_ID=""
    [[ -n "${OWNER_ID}" ]] || die "could not resolve the harness user's id"

    kc exec "${PGPOD}" -- env PGPASSWORD="${PG_PWD}" \
        psql -U llmsafespaces -d llmsafespaces -v ON_ERROR_STOP=1 -c "
-- api_keys.id is varchar(36) (uuid-shaped): '${OWNER_ID}-sd' is 36+3 =
-- 39 chars -> 'value too long' (pool run 4). A short literal id is
-- unique in the pool's dedicated DB and keeps ON CONFLICT idempotent.
INSERT INTO api_keys (id, user_id, key, name, active)
VALUES ('us70-harness-apikey', '${OWNER_ID}', $(api_key_db_hash "${API_KEY}"), 'e2e-sd-key', true)
ON CONFLICT (id) DO UPDATE SET key=EXCLUDED.key, active=true;
" >/dev/null
}

seed_workspace() { # ws [runtime_class] [resources_yaml] — owned by the harness session user
    local ws="$1" user_id="${OWNER_ID:?harness_start must run first}" rc="${2:-}" extra="${3:-}"
    kc delete workspace "${ws}" --ignore-not-found >/dev/null 2>&1 || true
    if [[ -n "${rc}" ]]; then
        # spec.runtimeClass is admin-gated: the Workspace validating
        # webhook denies any non-nil value unless the object carries the
        # override annotation (controller/internal/webhooks/
        # workspace_webhook.go:69-85; s5 precedent at
        # local/s5-overlay-validation.sh:492-520). Harness mints it here
        # because the gVisor batch is admin-operated, not tenant-facing.
        cat <<EOF | kc_apply_retry
apiVersion: llmsafespaces.dev/v1
kind: Workspace
metadata:
  name: ${ws}
  labels:
    user-id: ${user_id}
  annotations:
    llmsafespaces.dev/allow-runtime-class-override: "true"
spec:
  owner:
    userID: ${user_id}
  runtime: ${RUNTIME_REF}
  runtimeClass: ${rc}
${extra:+  resources:
${extra}}
  storage:
    size: 1Gi
    accessMode: ReadWriteOnce
EOF
    else
        cat <<EOF | kc_apply_retry
apiVersion: llmsafespaces.dev/v1
kind: Workspace
metadata:
  name: ${ws}
  labels:
    user-id: ${user_id}
spec:
  owner:
    userID: ${user_id}
  runtime: ${RUNTIME_REF}
${extra:+  resources:
${extra}}
  storage:
    size: 1Gi
    accessMode: ReadWriteOnce
EOF
    fi
    # The API's workspace routes resolve through PostgreSQL
    # (WorkspaceAccessMiddleware -> workspaces.id = $1), and ownership is
    # established at API create time — nothing back-fills a kubectl-applied
    # CR into the DB. The harness therefore seeds the metadata row itself
    # (test.sh precedent): without it every bind/suspend/activate 4xx/5xx's.
    seed_workspace_metadata "${ws}" "${user_id}"
}

seed_workspace_metadata() { # ws user_id
    local ws="$1" user_id="$2"
    [[ -n "${PGPOD:-}" && -n "${PG_PWD:-}" ]] || die "seed_workspace_metadata: harness_start must run first (PGPOD/PG_PWD unset)"
    kc exec "${PGPOD}" -- env PGPASSWORD="${PG_PWD}" psql -U llmsafespaces -d llmsafespaces -v ON_ERROR_STOP=1 \
        -c "INSERT INTO workspaces (id, name, user_id, namespace, runtime, storage_size) VALUES ('${ws}', 'us70-${ws}', '${user_id}', '${NS}', '${RUNTIME_REF}', '1Gi') ON CONFLICT (id) DO UPDATE SET user_id = EXCLUDED.user_id, deleted_at = NULL" >/dev/null
}

# bind_env ws VAR VALUE — create+bind one env-secret via the convenience
# endpoint (server-side creates an env-secret and binds it atomically).
bind_env() {
    local ws="$1" var="$2" value="$3" body
    body=$(jq -nc --arg v "$var" --arg val "$value" '{vars:{($v):$val}}')
    local code body_out attempt
    body_out=$(mktemp)
    for attempt in 1 2; do
        code=$(curl -sm 30 -o "${body_out}" -w '%{http_code}' -X PUT \
            -H "Authorization: Bearer ${AUTH_TOKEN:?}" \
            -H "Content-Type: application/json" \
            -d "$body" \
            "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${ws}/env")
        [[ "${code}" == "401" && "${attempt}" == "1" ]] || break
        # JWT expiry mid-suite: re-login once and retry — the pool's
        # multi-hour dwells can outlive a short token TTL.
        login_harness_user
    done
    if [[ "${code}" != 2* ]]; then
        die "bind_env ${var}=${value} on ${ws} failed: HTTP ${code}: $(head -c 400 "${body_out}")"
    fi
    rm -f "${body_out}"
}

# detect_runtime_class — gVisor feature-detection: is there a controllable
# runtimeClass (runsc)? Sets RUNTIME_CLASS + GVisorAvailable globals. When
# present, the scale workspaces are created with spec.runtimeClass set to
# the detected class so AC-13 genuinely runs under gVisor (not just runc).
detect_runtime_class() {
    GVisorAvailable=false
    RUNTIME_CLASS=""
    local _rc
    local rc_name
    rc_name=$(kubectl --context "${CTX}" get runtimeclass -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)
    for _rc in $rc_name; do
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
}

# ---------------------------------------------------------------------------
# US-70.3 (notify → re-pull + reconcile) helpers.

# spawned_seq ws — the seq prefix of status.secretsDelivery.spawnedRev
# ("seq:manifestHash:contentHash"; the reconcile loop's convergence key).
# Empty when unset or legacy-format (pre-US-70.2 bare hash) — callers must
# treat empty as "not comparable", never as 0.
spawned_seq() {
    local rev
    rev=$(kc get workspace "$1" -o jsonpath='{.status.secretsDelivery.spawnedRev}' 2>/dev/null || true)
    if [[ "${rev}" == *:* ]]; then
        printf '%s\n' "${rev%%:*}"
    fi
}

# env_absent_from_child ws VAR= — 0 iff a LIVE child's environ is readable,
# non-empty, and does not carry VAR. Distinct from ! env_in_child: while the
# child is mid-restart /proc/<pid>/environ is unreadable and agent_environ
# returns EMPTY, which must not read as "the var is gone" (the revocation
# rows would pass on a crashed child). Pass the var WITH '=' so a prefix
# collision (SD_X vs SD_X2) cannot blur the match.
env_absent_from_child() {
    local env
    env=$(agent_environ "$1")
    [[ -n "${env}" && "${env}" != *"$2"* ]]
}

# pg_scalar query — one scalar from the API's postgres via the pg pod.
pg_scalar() {
    kc exec "${PGPOD}" -- env PGPASSWORD="${PG_PWD}" \
        psql -U llmsafespaces -d llmsafespaces -tA -c "$1" 2>/dev/null || true
}

# secret_id_by_name ws name — resolve a bound secret's id via the API
# (GET /workspaces/:id/bindings). bind_env names its secrets
# "<workspaceID>-env-<lowercased var>", so callers can derive the name
# without a separate create call.
secret_id_by_name() {
    local ws="$1" name="$2" body
    body=$(curl -sfm 15 -H "Authorization: Bearer ${AUTH_TOKEN:?}" \
        "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${ws}/bindings" 2>/dev/null) || body=""
    printf '%s' "${body}" | jq -r --arg n "${name}" \
        '.bindings[]? | select(.name == $n) | .secretId' | head -1
}

# resync_forward_start ws — port-forward the POD's agentd user mux
# (4097 — POST /v1/resync-secrets, the notify target and the
# secrets_resync MCP tool's backend) to ${RESYNC_PORT} on localhost, and
# read the workspace password from the controller-owned Secret
# workspace-pw-<ws> (same channel as local/test.sh's opencode health
# probe). The forward rides the kube-apiserver, so it works even while
# the llmsafespaces-api deployment itself is scaled to zero (AC-8).
resync_forward_start() {
    local ws="$1" pod logf _i
    pod=$(pod_of "${ws}")
    [[ -n "${pod}" ]] || die "resync: workspace ${ws} has no pod"
    RESC_PW=$(kc get secret "workspace-pw-${ws}" -o jsonpath='{.data.password}' 2>/dev/null \
        | base64 -d 2>/dev/null || true)
    [[ -n "${RESC_PW}" ]] || die "resync: secret workspace-pw-${ws} missing or empty (controller did not mint the workspace password)"
    if [[ -n "${RESC_PF_PID:-}" ]]; then
        kill "${RESC_PF_PID}" 2>/dev/null || true
        wait "${RESC_PF_PID}" 2>/dev/null || true
    fi
    logf=$(mktemp)
    # Three bounded retries, each on a DIFFERENT local port: a killed
    # forward's listener can sit in TIME_WAIT (kubectl binds without
    # SO_REUSEADDR), so re-binding the same port fails until the kernel
    # releases it (observed: AC-11's forward leaked into AC-8's establish,
    # 'bind: address already in use' x3, run 33865691207). The local port
    # is scratch — rotate it.
    local attempt
    for attempt in 0 1 2; do
        RESYNC_PORT=$(( RESYNC_PORT + attempt ))
        kc port-forward "pod/${pod}" "${RESYNC_PORT}:4097" >"${logf}" 2>&1 &
        RESC_PF_PID=$!
        for _i in $(seq 1 20); do
            if grep -q "Forwarding from" "${logf}" 2>/dev/null; then
                rm -f "${logf}"
                return 0
            fi
            kill -0 "${RESC_PF_PID}" 2>/dev/null || break
            sleep 0.5
        done
        kill "${RESC_PF_PID}" 2>/dev/null || true
        wait "${RESC_PF_PID}" 2>/dev/null || true
        sleep 1
    done
    cat "${logf}" >&2 || true
    rm -f "${logf}"
    die "resync: port-forward pod/${pod} ${RESYNC_PORT}:4097 failed to establish"
}

# resync_call — one authenticated POST /v1/resync-secrets against the
# established forward; sets RESC_CODE (HTTP status) + RESC_BODY. The
# endpoint NEVER reads a request body (0050 finding-3) — curl sends none.
resync_call() {
    local out
    out=$(mktemp)
    # -w '%{http_code}' emits 000 by itself on transport failure; the `||
    # true` only keeps set -e from killing the caller (the substitution
    # would otherwise carry curl's non-zero status).
    RESC_CODE=$(curl -sm 20 -o "${out}" -w '%{http_code}' -X POST \
        -u "opencode:${RESC_PW}" \
        "http://127.0.0.1:${RESYNC_PORT}/v1/resync-secrets" 2>/dev/null || true)
    RESC_BODY=$(cat "${out}" 2>/dev/null || true)
    rm -f "${out}"
}

resync_forward_stop() {
    if [[ -n "${RESC_PF_PID:-}" ]]; then
        kill "${RESC_PF_PID}" 2>/dev/null || true
        wait "${RESC_PF_PID}" 2>/dev/null || true
        RESC_PF_PID=""
    fi
}

# resync_pod ws — one-shot resync: forward, one POST, stop; leaves
# RESC_CODE/RESC_BODY set for the caller's asserts. The port-forward
# teardown between calls is fine for the pod-side rate limiter: the
# min-interval (2s default) state lives on the POD, not the connection.
resync_pod() {
    resync_forward_start "$1"
    resync_call
    resync_forward_stop
}

# api_portforward_restart — re-establish the API port-forward after the
# api deployment's pod set changed (scale-to-0 kills the svc-backed
# forward: kubectl bound it to a pod that no longer exists).
api_portforward_restart() {
    if [[ -n "${PF_PID:-}" ]]; then
        kill "${PF_PID}" 2>/dev/null || true
        wait "${PF_PID}" 2>/dev/null || true
    fi
    kc port-forward svc/llmsafespaces-api "${PORTFWD_PORT}:8080" >/dev/null 2>&1 &
    PF_PID=$!
    local _i
    for _i in $(seq 1 30); do
        curl -sfm 2 "http://127.0.0.1:${PORTFWD_PORT}/livez" >/dev/null 2>&1 && return 0
        sleep 1
    done
    die "API port-forward could not be re-established (livez unreachable on ${PORTFWD_PORT})"
}

# api_down — scale the api deployment to zero and wait until every api
# pod is REALLY gone (SIGTERM drains in-flight work; the api exits
# promptly when idle but the 660s grace means the wait must be bounded
# and loud). No notify can leave the API and no pod pull can reach it
# while down — the network-layer block the AC-8/AC-10 row uses instead
# of the (pool-only) fault seam.
api_down() {
    kc scale deployment llmsafespaces-api --replicas=0 >/dev/null \
        || die "api_down: scale --replicas=0 failed"
    local _i names
    for _i in $(seq 1 120); do
        names=$(kc get pods -l app.kubernetes.io/component=api \
            -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)
        [[ -z "${names}" ]] && return 0
        sleep 2
    done
    die "api_down: llmsafespaces-api pods still present after 240s (${names})"
}

# api_up — scale the api back to one replica, wait for readiness, and
# re-establish the harness port-forward.
api_up() {
    kc scale deployment llmsafespaces-api --replicas=1 >/dev/null \
        || die "api_up: scale --replicas=1 failed"
    kc rollout status deployment llmsafespaces-api --timeout=240s >/dev/null 2>&1 \
        || die "api_up: llmsafespaces-api never became ready"
    api_portforward_restart
    ok "api deployment recovered (replicas=1, port-forward re-established)"
}

# >>> resume-p95
# us70_resume_p95 TDIR EXPECTED — p95 (ms) over EXPECTED per-workspace
# latency files (TDIR/<ws>.ms, one integer ms per file). A missing or
# empty file means that workspace's stopwatch never reported (the
# 999999 sentinel) so an incomplete batch can never read as fast.
# Prints "P95 MID MIN COUNT". Sourced-marker style matches
# api-key-db-hash so us70_common_test.go can exercise the math.
us70_resume_p95() {
    local tdir="$1" expected="$2"
    local -a sorted=()
    local f v n idx p95 mid min count=0
    for f in "${tdir}"/*.ms; do
        [[ -e "$f" ]] || continue
        v=$(cat "$f" 2>/dev/null || echo "")
        [[ "$v" =~ ^[0-9]+$ ]] || v=999999
        sorted+=("$v")
    done
    n=${#sorted[@]}
    # Sentinel-fill up to EXPECTED: a worker that died before writing its
    # file must count against the p95, not vanish from the sample set.
    while (( n < expected )); do sorted+=("999999"); n=$((n+1)); done
    mapfile -t sorted < <(printf '%s\n' "${sorted[@]}" | sort -n)
    count=${#sorted[@]}
    idx=$(( (count * 95 + 99) / 100 - 1 ))
    (( idx < 0 )) && idx=0
    p95=${sorted[$idx]}
    mid=${sorted[$((count / 2))]}
    min=${sorted[0]}
    echo "${p95} ${mid} ${min} ${count}"
}
# <<< resume-p95
