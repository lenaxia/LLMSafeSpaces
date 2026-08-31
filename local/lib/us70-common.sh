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
#   P95_BUDGET_MS - AC-13/AC-1 resume p95 budget (default 30000)

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
WS_BASE="${WS_BASE:-e2esd0000-0000-0000-0000-0000000000}"
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

    seed_user "${USER_ID}" "${API_KEY}"
    ok "user + api key seeded"

    # The spawn-time pull is the critical regression surface for the sidecar
    # fleet (this is the mode where the old boot push deterministically lost
    # env-class secrets). The nightly installs agentdSidecar.enabled=true; call
    # this out so a single-container run isn't mistaken for the fixed path.
    warn "expected to run in agentd SIDECAR mode (the pull's primary surface); if the cluster is single-container this still validates the pull, but not the fleet regression"
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
        # spec.runtimeClass is admin-gated: the Workspace validating
        # webhook denies any non-nil value unless the object carries the
        # override annotation (controller/internal/webhooks/
        # workspace_webhook.go:69-85; s5 precedent at
        # local/s5-overlay-validation.sh:492-520). Harness mints it here
        # because the gVisor batch is admin-operated, not tenant-facing.
        cat <<EOF | kc apply -f - >/dev/null
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
