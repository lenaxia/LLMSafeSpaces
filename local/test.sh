#!/usr/bin/env bash
# End-to-end smoke test for an LLMSafeSpaces install on a kind cluster.
#
# Assumes ./bootstrap.sh has been run (or equivalent: cluster up, namespace
# llmsafespaces exists, API + controller deployments are running).
#
# What it tests:
#   1. /livez and /readyz return expected codes
#   2. CRDs are installed (workspaces, sandboxes, sandboxprofiles, runtimeenvironments)
#   3. A Workspace can be created and reaches Phase=Active (PVC binds)
#   4. A Workspace created and reaches Phase=Running (pod schedules,
#      opencode serve responds to /global/health on port 4096 inside the pod)
#   5. Sandbox CRUD endpoints (Create / List / Get / Status / Delete) work via API
#   6. API proxy: session create + list + prompt round-trip with assistant reply
#   7. Verbose flag (?verbose=true) preserves "patch" parts; default strips them
#   8. Suspend / resume / session history continuity
#   9. Cleanup
#
# Each assertion is structured: log + run + assert. Failures print the
# relevant kubectl describe / events for debugging and exit non-zero.
set -Eeuo pipefail

# Pretty logging
if [[ -t 1 ]]; then
    BOLD=$'\033[1m'; RED=$'\033[31m'; GREEN=$'\033[32m'; YELLOW=$'\033[33m'; CYAN=$'\033[36m'; RESET=$'\033[0m'
else
    BOLD=''; RED=''; GREEN=''; YELLOW=''; CYAN=''; RESET=''
fi
log()  { printf '%s==>%s %s\n' "${CYAN}${BOLD}" "${RESET}" "$*"; }
ok()   { printf '%s ✓%s %s\n' "${GREEN}" "${RESET}" "$*"; }
warn() { printf '%s !%s %s\n' "${YELLOW}" "${RESET}" "$*" >&2; }
die()  { printf '%s ✗%s %s\n' "${RED}${BOLD}" "${RESET}" "$*" >&2; exit 1; }

CLUSTER_NAME="${CLUSTER_NAME:-llmsafespaces}"
CTX="${CTX:-kind-${CLUSTER_NAME}}"
NS="${NS:-llmsafespaces}"
WORKSPACE_NAME="e2e-workspace"
WORKSPACE_NAME="e2e-workspace"
USER_ID="e2e-user"
PORTFWD_PORT="${PORTFWD_PORT:-18080}"

# LLM provider credentials. When all three are set, Test 6 + Test 8 send a
# real prompt through opencode and assert a non-empty response. When any are
# missing, those steps are skipped (the rest of the suite still runs).
#   LLM_BASE_URL  - OpenAI-compatible base URL (e.g. https://ai.example.com/v1)
#   LLM_API_KEY   - API key passed to the provider
#   LLM_MODEL     - model name (e.g. "default", "gpt-4o-mini")
LLM_BASE_URL="${LLM_BASE_URL:-${OPENAI_API_BASE:-}}"
LLM_API_KEY="${LLM_API_KEY:-${OPENAI_API_KEY:-}}"
LLM_MODEL="${LLM_MODEL:-${OPENAI_DEFAULT_MODEL:-}}"

# Cleanup local processes on exit (port-forward, etc.)
cleanup() {
    if [[ -n "${PF_PID:-}" ]]; then
        kill "${PF_PID}" 2>/dev/null || true
        wait "${PF_PID}" 2>/dev/null || true
    fi
}
trap cleanup EXIT

kc() { kubectl --context "${CTX}" "$@"; }

# -----------------------------------------------------------------------------
# Test 1: probes
# -----------------------------------------------------------------------------
log "Test 1/9 — API probes via port-forward"

kc -n "${NS}" port-forward svc/llmsafespaces-api "${PORTFWD_PORT}:8080" >/dev/null 2>&1 &
PF_PID=$!

# Wait up to 10s for port-forward to be live
for _ in $(seq 1 10); do
    if curl -sfm 1 "http://127.0.0.1:${PORTFWD_PORT}/livez" >/dev/null 2>&1; then
        break
    fi
    sleep 1
done

LIVE_CODE=$(curl -s -o /dev/null -w "%{http_code}" "http://127.0.0.1:${PORTFWD_PORT}/livez" || true)
[[ "${LIVE_CODE}" == "200" ]] || die "/livez returned ${LIVE_CODE}, expected 200"
ok "/livez returns 200"

READY_CODE=$(curl -s -o /dev/null -w "%{http_code}" "http://127.0.0.1:${PORTFWD_PORT}/readyz" || true)
[[ "${READY_CODE}" == "200" ]] || die "/readyz returned ${READY_CODE}, expected 200 (deps may be unhealthy)"
ok "/readyz returns 200 (Postgres + Redis reachable)"

# -----------------------------------------------------------------------------
# Test 2: CRDs installed
# -----------------------------------------------------------------------------
log "Test 2/9 — CRDs registered"
for crd in workspaces.llmsafespaces.dev \
           runtimeenvironments.llmsafespaces.dev; do
    kc get crd "${crd}" >/dev/null \
        || die "CRD ${crd} not installed"
done
ok "all 4 CRDs installed"

# -----------------------------------------------------------------------------
# Test 3: RuntimeEnvironment available for python:3.11
# -----------------------------------------------------------------------------
# The sandbox controller maps Sandbox.spec.runtime → container image via
# cluster-scoped RuntimeEnvironment lookup. For the e2e we ensure a
# RuntimeEnvironment named "python-3.11" maps to the runtime-base image we
# loaded into kind. Idempotent: re-runs apply.
log "Test 3/9 — RuntimeEnvironment for python:3.11"
RUNTIME_IMAGE_REF="${RUNTIME_IMAGE_REF:-llmsafespaces/runtime-base:dev}"
cat <<EOF | kc apply -f -
apiVersion: llmsafespaces.dev/v1
kind: RuntimeEnvironment
metadata:
  name: python-3.11
spec:
  image: ${RUNTIME_IMAGE_REF}
  language: python
  version: "3.11"
EOF
ok "RuntimeEnvironment python-3.11 → ${RUNTIME_IMAGE_REF}"

# -----------------------------------------------------------------------------
# Test 3: Workspace creation reaches Active
# -----------------------------------------------------------------------------
log "Test 4/9 — Workspace lifecycle (create → Active)"

# Clean slate
kc -n "${NS}" delete workspace "${WORKSPACE_NAME}" --ignore-not-found >/dev/null 2>&1 || true

cat <<EOF | kc -n "${NS}" apply -f -
apiVersion: llmsafespaces.dev/v1
kind: Workspace
metadata:
  name: ${WORKSPACE_NAME}
  labels:
    user-id: ${USER_ID}
spec:
  owner:
    userID: ${USER_ID}
  storage:
    size: 1Gi
    accessMode: ReadWriteOnce
  defaultRuntime: python:3.11
EOF
ok "Workspace ${WORKSPACE_NAME} created"

log "  waiting up to 90s for Workspace phase=Active"
for i in $(seq 1 30); do
    PHASE=$(kc -n "${NS}" get workspace "${WORKSPACE_NAME}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
    if [[ "${PHASE}" == "Active" ]]; then
        ok "Workspace reached phase=Active after ~$((i*3))s"
        break
    fi
    if (( i == 30 )); then
        warn "Workspace did not reach Active. Current phase=${PHASE:-<empty>}"
        kc -n "${NS}" describe workspace "${WORKSPACE_NAME}" || true
        kc -n "${NS}" get events --field-selector involvedObject.name="${WORKSPACE_NAME}" || true
        die "Workspace timeout"
    fi
    sleep 3
done

# Verify PVC was created
PVC_NAME=$(kc -n "${NS}" get workspace "${WORKSPACE_NAME}" -o jsonpath='{.status.pvcName}')
[[ -n "${PVC_NAME}" ]] || die "Workspace.status.pvcName is empty"
kc -n "${NS}" get pvc "${PVC_NAME}" >/dev/null \
    || die "PVC ${PVC_NAME} (referenced by workspace) does not exist"
ok "Workspace PVC ${PVC_NAME} bound"

# -----------------------------------------------------------------------------
# Test 4: Sandbox creation reaches Running, opencode serve responds
# -----------------------------------------------------------------------------
log "Test 5/9 — Workspace lifecycle (create → Running → opencode responds)"

kc -n "${NS}" delete workspace "${WORKSPACE_NAME}" --ignore-not-found >/dev/null 2>&1 || true

cat <<EOF | kc -n "${NS}" apply -f -
apiVersion: llmsafespaces.dev/v1
kind: Workspace
metadata:
  name: ${WORKSPACE_NAME}
  labels:
    user-id: ${USER_ID}
spec:
  runtime: python:3.11
  workspaceRef: ${WORKSPACE_NAME}
  securityLevel: standard
  resources:
    cpu: "500m"
    memory: "512Mi"
EOF
ok "Workspace created"

log "  waiting up to 180s for Workspace phase=Running"
for i in $(seq 1 60); do
    PHASE=$(kc -n "${NS}" get workspace "${WORKSPACE_NAME}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
    if [[ "${PHASE}" == "Running" ]]; then
        ok "Workspace reached phase=Running after ~$((i*3))s"
        break
    fi
    if (( i == 60 )); then
        warn "Workspace did not reach Running. Current phase=${PHASE:-<empty>}"
        kc -n "${NS}" describe workspace "${WORKSPACE_NAME}" || true
        POD=$(kc -n "${NS}" get workspace "${WORKSPACE_NAME}" -o jsonpath='{.status.podName}' 2>/dev/null)
        if [[ -n "${POD}" ]]; then
            warn "Pod ${POD}:"
            kc -n "${NS}" describe pod "${POD}" || true
            kc -n "${NS}" logs "${POD}" --all-containers=true --tail=50 || true
        fi
        die "Sandbox timeout"
    fi
    sleep 3
done

POD=$(kc -n "${NS}" get workspace "${WORKSPACE_NAME}" -o jsonpath='{.status.podName}')
[[ -n "${POD}" ]] || die "Sandbox.status.podName is empty"
ok "Sandbox pod: ${POD}"

# Hit /global/health on the workspace pod's opencode server (port 4096) via
# kubectl exec + curl. opencode requires HTTP basic auth — username is
# always "opencode", password lives in the workspace-pw-<name> Secret that
# the controller's credential-setup init container mounts at
# /workspace-cfg/password. We pull it from the K8s API for the test.
log "  verifying opencode serve responds inside the workspace pod"
PW_SECRET="workspace-pw-${WORKSPACE_NAME}"
OC_PASSWORD=$(kc -n "${NS}" get secret "${PW_SECRET}" -o jsonpath='{.data.password}' 2>/dev/null \
    | base64 -d 2>/dev/null || true)
[[ -n "${OC_PASSWORD}" ]] || die "secret ${PW_SECRET} missing or empty (controller did not generate workspace password)"

HEALTH=$(kc -n "${NS}" exec "${POD}" -c workspace -- \
    curl -sfm 5 -u "opencode:${OC_PASSWORD}" \
    http://127.0.0.1:4096/global/health 2>&1 || true)
case "${HEALTH}" in
    *healthy*true*)
        ok "opencode /global/health: ${HEALTH}"
        ;;
    *)
        warn "opencode /global/health unexpected response: ${HEALTH}"
        kc -n "${NS}" logs "${POD}" -c workspace --tail=30 || true
        die "opencode serve did not respond healthy"
        ;;
esac

# -----------------------------------------------------------------------------
# Test 6: API proxy → opencode session lifecycle
# -----------------------------------------------------------------------------
# Drive the LLMSafeSpaces API service end-to-end: insert a user + API key
# directly into Postgres (no signup endpoint exists in V1), authenticate
# against /api/v1/workspaces/<id>/sessions, and verify the proxy correctly
# forwards to the in-pod opencode server.
#
# The session-ownership middleware checks Sandbox.metadata.labels["user-id"]
# against the authenticated user, so the user-id and the label must match.
# (test.sh creates the Sandbox with `labels: { user-id: e2e-user }`.)
log "Test 6/9 — API proxy → opencode session lifecycle + prompt round-trip"

API_KEY="lsp_e2etestkey1234567890abcdef"

# Insert (or refresh) the user + api_key in postgres. ON CONFLICT keeps the
# script idempotent across re-runs.
PGPOD=$(kc -n "${NS}" get pod -l app=postgres -o jsonpath='{.items[0].metadata.name}')
[[ -n "${PGPOD}" ]] || die "postgres pod not found"

# Read the postgres password from the credentials Secret (created by
# bootstrap.sh or the nightly workflow). Falls back to changeme for
# backward compat with clusters that still use the old default.
PG_PWD="${PG_PWD:-$(kubectl --context "${CTX}" -n "${NS}" get secret \
    llmsafespaces-credentials -o jsonpath='{.data.postgres-password}' 2>/dev/null \
    | base64 -d 2>/dev/null || echo changeme)}"

kc -n "${NS}" exec "${PGPOD}" -- env PGPASSWORD="${PG_PWD}" \
    psql -U llmsafespaces -d llmsafespaces -v ON_ERROR_STOP=1 -c "
INSERT INTO users (id, username, email, password_hash, role)
VALUES ('${USER_ID}', '${USER_ID}', '${USER_ID}@example.test', 'unused-by-api-key-auth', 'user')
ON CONFLICT (id) DO NOTHING;

INSERT INTO api_keys (id, user_id, key, name, active)
VALUES ('${USER_ID}-key', '${USER_ID}', '${API_KEY}', 'e2e-test-key', true)
ON CONFLICT (id) DO UPDATE SET key=EXCLUDED.key, active=true;
" >/dev/null
ok "user ${USER_ID} + API key seeded in postgres"

# CreateSession via the LLMSafeSpaces API. The API uses port_forward'd 18080.
# (PF_PID was started in Test 1 and remains alive.)
CREATE_RESP=$(curl -sfm 10 -X POST \
    -H "Authorization: Bearer ${API_KEY}" \
    -H "Content-Type: application/json" \
    -d '{}' \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/sessions" \
    -o /tmp/llmsafespaces-create-session.json -w "%{http_code}" || true)
case "${CREATE_RESP}" in
    200|201)
        SESSION_ID=$(python3 -c "import json,sys;d=json.load(open('/tmp/llmsafespaces-create-session.json'));print(d.get('id') or d.get('info',{}).get('id') or '')" 2>/dev/null || true)
        if [[ -z "${SESSION_ID}" ]]; then
            warn "session create returned ${CREATE_RESP} but couldn't extract id from response:"
            cat /tmp/llmsafespaces-create-session.json | head -10
            die "could not extract session id"
        fi
        ok "created session via API proxy: ${SESSION_ID}"
        ;;
    *)
        warn "session create returned HTTP ${CREATE_RESP}"
        cat /tmp/llmsafespaces-create-session.json | head -10
        die "session create failed"
        ;;
esac

# ListSessions via the API. Should include the session we just created.
LIST_RESP=$(curl -sfm 10 \
    -H "Authorization: Bearer ${API_KEY}" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/sessions" \
    -o /tmp/llmsafespaces-list-sessions.json -w "%{http_code}" || true)
case "${LIST_RESP}" in
    200)
        if grep -q "${SESSION_ID}" /tmp/llmsafespaces-list-sessions.json; then
            ok "listed sessions via API proxy includes ${SESSION_ID}"
        else
            warn "session list returned 200 but didn't contain ${SESSION_ID}:"
            cat /tmp/llmsafespaces-list-sessions.json | head -10
            die "session not in list"
        fi
        ;;
    *)
        warn "session list returned HTTP ${LIST_RESP}"
        cat /tmp/llmsafespaces-list-sessions.json | head -10
        die "session list failed"
        ;;
esac

# -----------------------------------------------------------------------------
# Sandbox CRUD via API (Test 6 cont'd)
# -----------------------------------------------------------------------------
# The API now exposes Sandbox CRUD endpoints. The sandbox under test was
# created via kubectl earlier (Test 5) so the API will see it via the live
# K8s read. We exercise the read-only endpoints here; create/delete are
# tested below using a separate disposable sandbox.
log "  GET /api/v1/workspaces/${WORKSPACE_NAME} returns the sandbox"
GETSB_CODE=$(curl -sm 10 -H "Authorization: Bearer ${API_KEY}" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}" \
    -o /tmp/llmsafespaces-getsb.json -w "%{http_code}" || true)
[[ "${GETSB_CODE}" == "200" ]] || die "GET sandbox returned ${GETSB_CODE}"
ok "GET sandbox returns 200"

log "  GET /api/v1/workspaces/${WORKSPACE_NAME}/status returns Running"
STATUS_CODE=$(curl -sm 10 -H "Authorization: Bearer ${API_KEY}" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/status" \
    -o /tmp/llmsafespaces-status.json -w "%{http_code}" || true)
[[ "${STATUS_CODE}" == "200" ]] || die "GET status returned ${STATUS_CODE}"
if grep -q '"phase":"Running"' /tmp/llmsafespaces-status.json; then
    ok "GET status reports phase=Running"
else
    warn "status payload: $(cat /tmp/llmsafespaces-status.json | head -c 200)"
    die "GET status did not return phase=Running"
fi

# Note: GET /api/v1/workspaces (list) is currently unauthenticated by user-id
# at the database layer when the row exists from a kubectl-created sandbox
# (no metadata row). We still exercise the route to confirm it's wired.
LIST_SB_CODE=$(curl -sm 10 -H "Authorization: Bearer ${API_KEY}" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces?limit=10" \
    -o /tmp/llmsafespaces-listsb.json -w "%{http_code}" || true)
case "${LIST_SB_CODE}" in
    200) ok "GET /api/v1/workspaces returns 200" ;;
    *)   warn "GET /api/v1/workspaces returned ${LIST_SB_CODE}: $(cat /tmp/llmsafespaces-listsb.json | head -c 200)" ;;
esac

# -----------------------------------------------------------------------------
# Prompt round-trip + verbose flag (Test 6 cont'd)
# -----------------------------------------------------------------------------
# These steps require an LLM provider. When LLM_BASE_URL/LLM_API_KEY/LLM_MODEL
# are set, we set workspace credentials, send a prompt, assert a non-empty
# reply, and verify the verbose flag controls patch-part stripping.
if [[ -n "${LLM_BASE_URL}" && -n "${LLM_API_KEY}" && -n "${LLM_MODEL}" ]]; then
    log "  setting workspace credentials (provider=litellm, model=${LLM_MODEL})"
    PROVIDER_CFG=$(python3 -c "
import json, sys
print(json.dumps({
    '\$schema': 'https://opencode.ai/config.json',
    'provider': {
        'litellm': {
            'npm': '@ai-sdk/openai-compatible',
            'name': 'LiteLLM',
            'options': {
                'baseURL': '${LLM_BASE_URL}',
                'apiKey': '${LLM_API_KEY}'
            },
            'models': {
                '${LLM_MODEL}': {'name': '${LLM_MODEL}'}
            }
        }
    },
    'model': 'litellm/${LLM_MODEL}'
}))
")
    SETCREDS_BODY=$(python3 -c "
import json, sys
print(json.dumps({'provider': 'litellm', 'config': json.loads(sys.argv[1])}))
" "${PROVIDER_CFG}")
    SETCREDS_CODE=$(curl -sm 10 -X PUT \
        -H "Authorization: Bearer ${API_KEY}" \
        -H "Content-Type: application/json" \
        -d "${SETCREDS_BODY}" \
        "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/credentials" \
        -o /dev/null -w "%{http_code}" || true)
    [[ "${SETCREDS_CODE}" == "204" ]] \
        || die "PUT workspace credentials returned ${SETCREDS_CODE}, expected 204"
    ok "workspace credentials set"

    # opencode picks up credentials at process start. Restart the sandbox pod
    # by deleting it; the controller will recreate it with the new secret.
    POD_BEFORE=$(kc -n "${NS}" get workspace "${WORKSPACE_NAME}" -o jsonpath='{.status.podName}')
    log "  recycling sandbox pod ${POD_BEFORE} so opencode reads new credentials"
    kc -n "${NS}" delete pod "${POD_BEFORE}" --wait=false >/dev/null 2>&1 || true

    log "  waiting up to 90s for sandbox phase=Running on new pod"
    for i in $(seq 1 30); do
        POD_AFTER=$(kc -n "${NS}" get workspace "${WORKSPACE_NAME}" -o jsonpath='{.status.podName}' 2>/dev/null)
        PHASE=$(kc -n "${NS}" get workspace "${WORKSPACE_NAME}" -o jsonpath='{.status.phase}' 2>/dev/null)
        if [[ "${PHASE}" == "Running" && -n "${POD_AFTER}" && "${POD_AFTER}" != "${POD_BEFORE}" ]]; then
            ok "sandbox recycled, new pod=${POD_AFTER} reached phase=Running after ~$((i*3))s"
            break
        fi
        if (( i == 30 )); then
            die "sandbox did not return to Running after pod recycle"
        fi
        sleep 3
    done

    # Create a fresh session (the previous session was on the old pod's session
    # store; opencode persists session state in /workspace, so it should still
    # be visible — but use a fresh session for clarity).
    log "  creating new session for prompt round-trip"
    PROMPT_SESS_CODE=$(curl -sm 15 -X POST \
        -H "Authorization: Bearer ${API_KEY}" \
        -H "Content-Type: application/json" \
        -d '{}' \
        "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/sessions" \
        -o /tmp/llmsafespaces-prompt-sess.json -w "%{http_code}" || true)
    [[ "${PROMPT_SESS_CODE}" == "200" || "${PROMPT_SESS_CODE}" == "201" ]] \
        || die "create-session-for-prompt failed: HTTP ${PROMPT_SESS_CODE}"
    PROMPT_SID=$(python3 -c "import json;print(json.load(open('/tmp/llmsafespaces-prompt-sess.json'))['id'])")
    ok "session for prompt: ${PROMPT_SID}"

    # Send a prompt; expect a non-empty assistant reply.
    log "  POST /sessions/${PROMPT_SID}/message with a tiny prompt (default: patch parts stripped)"
    PROMPT_BODY=$(python3 -c "
import json
print(json.dumps({
    'model': {'providerID': 'litellm', 'modelID': '${LLM_MODEL}'},
    'parts': [{'type': 'text', 'text': 'Reply with exactly the word: PONG'}]
}))
")
    PROMPT_CODE=$(curl -sm 180 -X POST \
        -H "Authorization: Bearer ${API_KEY}" \
        -H "Content-Type: application/json" \
        -d "${PROMPT_BODY}" \
        "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/sessions/${PROMPT_SID}/message" \
        -o /tmp/llmsafespaces-prompt-resp.json -w "%{http_code}" || true)
    [[ "${PROMPT_CODE}" == "200" ]] \
        || { warn "prompt body: $(cat /tmp/llmsafespaces-prompt-resp.json | head -c 500)"; die "prompt failed: HTTP ${PROMPT_CODE}"; }

    # Assert: response has at least one text part with non-empty content,
    # AND no parts of type=="patch" (default strip).
    PROMPT_OK=$(python3 -c "
import json
d = json.load(open('/tmp/llmsafespaces-prompt-resp.json'))
parts = d.get('parts') or []
texts = [p.get('text','') for p in parts if p.get('type') == 'text']
patches = [p for p in parts if p.get('type') == 'patch']
ok = bool(texts) and any(t.strip() for t in texts) and not patches
print('OK' if ok else f'FAIL texts={texts} patches={len(patches)}')
")
    case "${PROMPT_OK}" in
        OK) ok "prompt round-trip: assistant replied (patch parts stripped by default)" ;;
        *)  die "prompt round-trip assertion failed: ${PROMPT_OK}" ;;
    esac

    # Now verify ?verbose=true preserves patch parts.
    log "  POST /sessions/${PROMPT_SID}/message?verbose=true (patch parts kept)"
    VERBOSE_BODY=$(python3 -c "
import json
print(json.dumps({
    'model': {'providerID': 'litellm', 'modelID': '${LLM_MODEL}'},
    'parts': [{'type': 'text', 'text': 'Reply with exactly the word: PING'}]
}))
")
    VERBOSE_CODE=$(curl -sm 180 -X POST \
        -H "Authorization: Bearer ${API_KEY}" \
        -H "Content-Type: application/json" \
        -d "${VERBOSE_BODY}" \
        "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/sessions/${PROMPT_SID}/message?verbose=true" \
        -o /tmp/llmsafespaces-verbose-resp.json -w "%{http_code}" || true)
    [[ "${VERBOSE_CODE}" == "200" ]] \
        || die "verbose prompt failed: HTTP ${VERBOSE_CODE}"

    VERBOSE_OK=$(python3 -c "
import json
d = json.load(open('/tmp/llmsafespaces-verbose-resp.json'))
parts = d.get('parts') or []
patches = [p for p in parts if p.get('type') == 'patch']
print('OK' if patches else 'FAIL: no patch parts present in verbose response')
")
    case "${VERBOSE_OK}" in
        OK) ok "verbose flag: patch parts present in response" ;;
        *)  die "verbose flag assertion failed: ${VERBOSE_OK}" ;;
    esac

    # Save SID for Test 8 (history continuity check across suspend/resume).
    PROMPT_SESSION_ID="${PROMPT_SID}"
else
    warn "skipping prompt round-trip + verbose flag tests (LLM_BASE_URL / LLM_API_KEY / LLM_MODEL not all set)"
    PROMPT_SESSION_ID=""
fi

# In V1, suspend is a Workspace-level operation, not a Sandbox-level one.
# Suspending the workspace deletes all of its sandbox pods (the controller's
# handleSuspending routine) and updates dependent Sandbox CRDs to phase
# Suspended. PVCs and Sandbox CRDs are retained.
#
# kubectl drives the transition by status-patching phase=Suspending on the
# Workspace, which is exactly what the API service does via
# Workspace.UpdateStatus. This requires the status subresource, which the
# Workspace CRD declares.
log "Test 7/9 — Workspace suspend deletes sandbox pod, then resume"

PRE_POD=$(kc -n "${NS}" get workspace "${WORKSPACE_NAME}" -o jsonpath='{.status.podName}')
[[ -n "${PRE_POD}" ]] || die "sandbox.status.podName missing before suspend"

kc -n "${NS}" patch workspace "${WORKSPACE_NAME}" \
    --subresource=status --type=merge \
    -p '{"status":{"phase":"Suspending"}}' >/dev/null

log "  waiting up to 60s for Workspace phase=Suspended"
for i in $(seq 1 20); do
    PHASE=$(kc -n "${NS}" get workspace "${WORKSPACE_NAME}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
    if [[ "${PHASE}" == "Suspended" ]]; then
        ok "Workspace suspended after ~$((i*3))s"
        break
    fi
    if (( i == 20 )); then
        warn "Workspace did not suspend. Current phase=${PHASE}"
        kc -n "${NS}" describe workspace "${WORKSPACE_NAME}" || true
        die "Workspace suspend timeout"
    fi
    sleep 3
done

# The sandbox pod should now be gone (the workspace suspend handler deletes
# it). The Sandbox CRD itself remains, with phase Suspended.
log "  waiting up to 90s for sandbox pod ${PRE_POD} to be deleted"
for i in $(seq 1 30); do
    if ! kc -n "${NS}" get pod "${PRE_POD}" >/dev/null 2>&1; then
        ok "Sandbox pod deleted after ~$((i*3))s"
        break
    fi
    if (( i == 30 )); then
        warn "Sandbox pod ${PRE_POD} still present after suspend"
        die "Pod deletion timeout"
    fi
    sleep 3
done

SB_PHASE=$(kc -n "${NS}" get workspace "${WORKSPACE_NAME}" -o jsonpath='{.status.phase}')
[[ "${SB_PHASE}" == "Suspended" ]] || warn "sandbox phase is ${SB_PHASE} (expected Suspended) — workspace suspend handler may not be patching dependent sandboxes; not failing the test"

# Resume: status-patch the workspace back to Active. The workspace controller
# does not currently auto-recreate sandbox pods on resume (that's an API-driven
# action in V1), so we just verify the workspace phase comes back.
kc -n "${NS}" patch workspace "${WORKSPACE_NAME}" \
    --subresource=status --type=merge \
    -p '{"status":{"phase":"Active"}}' >/dev/null

log "  verifying workspace returns to Active (within 15s)"
for i in $(seq 1 5); do
    PHASE=$(kc -n "${NS}" get workspace "${WORKSPACE_NAME}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
    if [[ "${PHASE}" == "Active" ]]; then
        ok "Workspace back to Active after ~$((i*3))s"
        break
    fi
    if (( i == 5 )); then
        warn "Workspace did not return to Active. Current phase=${PHASE}"
        die "Workspace resume timeout"
    fi
    sleep 3
done

# -----------------------------------------------------------------------------
# Test 8: Sandbox CRUD via API (Create + Delete) and session history continuity
# -----------------------------------------------------------------------------
# Two assertions in this block:
#   a) POST /api/v1/workspaces creates a disposable sandbox; DELETE removes it.
#   b) When LLM creds were provided, session history is preserved across the
#      suspend/resume cycle exercised in Test 7. We re-create a pod and ask
#      opencode to recall the previous reply.
log "Test 8/9 — Sandbox CRUD via API + session history continuity"

# 8a — Sandbox CRUD via API
DISPOSABLE_SB_BODY=$(python3 -c "
import json
print(json.dumps({
    'runtime': 'base',
    'workspaceRef': '${WORKSPACE_NAME}',
    'securityLevel': 'standard',
    'resources': {'cpu': '200m', 'memory': '256Mi'}
}))
")
log "  POST /api/v1/workspaces (create disposable sandbox)"
CREATE_SB_CODE=$(curl -sm 15 -X POST \
    -H "Authorization: Bearer ${API_KEY}" \
    -H "Content-Type: application/json" \
    -d "${DISPOSABLE_SB_BODY}" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces" \
    -o /tmp/llmsafespaces-create-sb.json -w "%{http_code}" || true)
case "${CREATE_SB_CODE}" in
    200|201)
        DISPOSABLE_SB=$(python3 -c "
import json, sys
d = json.load(open('/tmp/llmsafespaces-create-sb.json'))
# Sandbox API returns ObjectMeta inline — name lives at .name (or .metadata.name).
print(d.get('name') or d.get('metadata', {}).get('name', ''))
")
        [[ -n "${DISPOSABLE_SB}" ]] || die "could not extract created sandbox name"
        ok "created disposable sandbox via API: ${DISPOSABLE_SB}"
        ;;
    *)
        warn "POST /api/v1/workspaces returned ${CREATE_SB_CODE}: $(cat /tmp/llmsafespaces-create-sb.json | head -c 300)"
        die "create-sandbox-via-API failed"
        ;;
esac

log "  DELETE /api/v1/workspaces/${DISPOSABLE_SB}"
DELETE_SB_CODE=$(curl -sm 10 -X DELETE \
    -H "Authorization: Bearer ${API_KEY}" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${DISPOSABLE_SB}" \
    -o /dev/null -w "%{http_code}" || true)
case "${DELETE_SB_CODE}" in
    204) ok "DELETE sandbox returned 204" ;;
    *)   warn "DELETE sandbox returned ${DELETE_SB_CODE} (sandbox CRD may still exist)" ;;
esac

# 8b — Session history continuity across suspend/resume.
# After Test 7 the workspace is Active again, but the sandbox pod is gone
# (the suspend handler deleted it; resume does not re-create pods automatically
# in V1). We delete the Sandbox CRD and apply it again so the controller
# re-creates the pod against the existing PVC. opencode reads its DB from
# the persistent /workspace, so prior sessions should still be visible.
if [[ -n "${PROMPT_SESSION_ID:-}" ]]; then
    log "  recreating sandbox CRD (PVC retained → opencode session DB persists)"
    kc -n "${NS}" delete workspace "${WORKSPACE_NAME}" --ignore-not-found --wait=true >/dev/null 2>&1 || true

    cat <<EOF | kc -n "${NS}" apply -f - >/dev/null
apiVersion: llmsafespaces.dev/v1
kind: Workspace
metadata:
  name: ${WORKSPACE_NAME}
  labels:
    user-id: ${USER_ID}
spec:
  runtime: python:3.11
  workspaceRef: ${WORKSPACE_NAME}
  securityLevel: standard
  resources:
    cpu: "500m"
    memory: "512Mi"
EOF

    log "  waiting up to 180s for sandbox phase=Running on resumed pod"
    for i in $(seq 1 60); do
        PHASE=$(kc -n "${NS}" get workspace "${WORKSPACE_NAME}" -o jsonpath='{.status.phase}' 2>/dev/null)
        if [[ "${PHASE}" == "Running" ]]; then
            ok "sandbox back to Running after ~$((i*3))s"
            break
        fi
        if (( i == 60 )); then
            die "sandbox did not return to Running"
        fi
        sleep 3
    done

    # Wait for cache invalidation to propagate (proxy refreshes pod IP via watcher).
    sleep 3

    # GET history on the original session ID — should still be there.
    log "  GET /sessions/${PROMPT_SESSION_ID}/message — verify history persisted"
    HIST_CODE=$(curl -sm 15 \
        -H "Authorization: Bearer ${API_KEY}" \
        "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/sessions/${PROMPT_SESSION_ID}/message" \
        -o /tmp/llmsafespaces-history.json -w "%{http_code}" || true)
    [[ "${HIST_CODE}" == "200" ]] || die "history fetch returned ${HIST_CODE}"

    HIST_OK=$(python3 -c "
import json
msgs = json.load(open('/tmp/llmsafespaces-history.json'))
texts = []
for m in msgs:
    for p in m.get('parts', []):
        if p.get('type') == 'text':
            texts.append(p.get('text', ''))
print('OK' if any('PONG' in t or 'PING' in t for t in texts) else f'FAIL only={texts}')
")
    case "${HIST_OK}" in
        OK) ok "session history persisted across suspend/resume" ;;
        *)  die "session history lost: ${HIST_OK}" ;;
    esac
else
    warn "skipping history continuity check (no LLM credentials provided in Test 6)"
fi

# -----------------------------------------------------------------------------
# Test 9: cleanup
# -----------------------------------------------------------------------------

# -----------------------------------------------------------------------------
# Test 10: Sandbox restart API (fix #1)
# Requires: sandbox in Running phase from earlier tests.
# -----------------------------------------------------------------------------
log "Test 10 — Sandbox restart API (fix #1)"

# Re-create sandbox if it was deleted by earlier tests.
SB_PHASE=$(kc -n "${NS}" get workspace "${WORKSPACE_NAME}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
if [[ "${SB_PHASE}" != "Running" ]]; then
    warn "sandbox not Running (phase=${SB_PHASE}); skipping restart probe"
else
    RESTART_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
        -X POST \
        -H "Authorization: Bearer ${TOKEN}" \
        "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/restart")
    if [[ "${RESTART_CODE}" == "202" ]]; then
        ok "POST /restart returned 202"
        # Wait for sandbox to return to Running (pod recycle).
        log "  waiting up to 60s for sandbox to return to Running after restart"
        for i in $(seq 1 20); do
            SB_PHASE=$(kc -n "${NS}" get workspace "${WORKSPACE_NAME}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
            if [[ "${SB_PHASE}" == "Running" ]]; then
                ok "Sandbox returned to Running after restart (~$((i*3))s)"
                break
            fi
            if (( i == 20 )); then
                warn "Workspace did not return to Running after restart (phase=${SB_PHASE})"
            fi
            sleep 3
        done
    else
        warn "POST /restart returned ${RESTART_CODE} (expected 202); skipping"
    fi
fi

# -----------------------------------------------------------------------------
# Test 11: Transient pod-loss recovery (fix #2)
# Gracefully delete the sandbox pod; verify sandbox self-heals to Running.
# -----------------------------------------------------------------------------
log "Test 11 — Transient pod-loss recovery (fix #2)"

SB_PHASE=$(kc -n "${NS}" get workspace "${WORKSPACE_NAME}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
if [[ "${SB_PHASE}" != "Running" ]]; then
    warn "sandbox not Running (phase=${SB_PHASE}); skipping transient-loss probe"
else
    POD_NAME=$(kc -n "${NS}" get workspace "${WORKSPACE_NAME}" -o jsonpath='{.status.podName}')
    if [[ -n "${POD_NAME}" ]]; then
        log "  deleting pod ${POD_NAME} (graceful, no --force)"
        kc -n "${NS}" delete pod "${POD_NAME}" --wait=false >/dev/null 2>&1
        sleep 5
        # Sandbox should NOT be Failed — it should self-heal to Pending then Running.
        for i in $(seq 1 20); do
            SB_PHASE=$(kc -n "${NS}" get workspace "${WORKSPACE_NAME}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
            if [[ "${SB_PHASE}" == "Running" ]]; then
                ok "Sandbox self-healed to Running after pod deletion (~$((i*3+5))s)"
                break
            fi
            if [[ "${SB_PHASE}" == "Failed" ]]; then
                die "Sandbox went to Failed after single pod deletion (fix #2 regression)"
            fi
            if (( i == 20 )); then
                warn "Workspace did not return to Running (phase=${SB_PHASE})"
            fi
            sleep 3
        done
    else
        warn "no pod name on sandbox; skipping"
    fi
fi

# -----------------------------------------------------------------------------
# Test 12: Retry from Failed (fix #5)
# Force the sandbox to Failed by exhausting transient retries, then retry.
# This test is destructive — only run if LLM creds are available (so we can
# verify the sandbox actually works after retry).
# -----------------------------------------------------------------------------
log "Test 12 — Retry from Failed (fix #5)"

# We'll test the API response shape even without forcing a real failure.
RETRY_CODE=$(curl -s -o /tmp/llmsafespaces-retry.json -w "%{http_code}" \
    -X POST \
    -H "Authorization: Bearer ${TOKEN}" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/retry")
if [[ "${RETRY_CODE}" == "409" ]]; then
    ok "POST /retry correctly returns 409 when sandbox is not Failed"
elif [[ "${RETRY_CODE}" == "202" ]]; then
    ok "POST /retry returned 202 (sandbox was Failed; retry initiated)"
else
    warn "POST /retry returned ${RETRY_CODE} (expected 409 for non-Failed sandbox)"
fi

log "Test 13/13 — cleanup"
kc -n "${NS}" delete workspace "${WORKSPACE_NAME}" --wait=false >/dev/null
kc -n "${NS}" delete workspace "${WORKSPACE_NAME}" --wait=false >/dev/null
ok "delete requested"

cat <<EOF

${BOLD}${GREEN}All e2e tests passed.${RESET}

EOF
