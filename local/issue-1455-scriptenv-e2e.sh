#!/usr/bin/env bash
# Issue #1455 — the script-node ENVIRONMENT CONTRACT and the http-node
# secrets coordinate, exercised against real workspace pods through the
# platform API. agentd serves /v1/workflow/node/execute from the
# container it lives in.
#
#   R1 — script-node mode contract (adaptive, never silently skipped):
#        a one-node python script workflow must terminate as EITHER
#        (a) succeeded with the handler's marker output (single-
#        container clusters: scripts still execute post-EnvCheck,
#        guarding EnvCheck against false positives in the real
#        toolchain env), OR (b) failed with script_env_unavailable +
#        the sidecar-naming detail. The NIGHTLY installs
#        controller.agentdSidecar.enabled=true (e2e-nightly.yml) — R1b
#        is the expected arm there; R1a covers single-container
#        pool/dev clusters. Any other terminal shape — the pre-#1455
#        incidental "create temp dir: stat /tmp" script_failed,
#        timeouts, fork/exec errors — fails the row.
#
#   R2 — http-node {{secrets.*}} coordinate (live join): bind an
#        env-secret via the harness convenience endpoint, wait for
#        materialization (in sidecar mode secrets-env is the US-4b
#        relocated /agentd-secrets coordinate — exactly the path the
#        fix taught loadSecretsEnv to read), then run an http node
#        against an in-cluster header-echo upstream (1417 pattern):
#        the echoed Authorization header must carry the RESOLVED value,
#        and an unbound ref must stay literal (documented pass-through
#        semantics).
#
# Environment: same conventions as local/issue-1417-templating-e2e.sh
# (local/lib/us70-common.sh). Runs on the nightly kind cluster.
# No LLM_MODEL required — neither node type calls a model.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/us70-common.sh
source "${SCRIPT_DIR}/lib/us70-common.sh"

harness_start

# UNCONDITIONAL (the us-70-revisions r21 pattern): the lib sets its own
# WS_BASE default at source time, so the :- form here is dead code — the
# pool's shared e2e5d000 prefix would silently apply and per-script
# isolation never engages.
WS_BASE="e2e14550-0000-4000-8000-000000000000"
WS="$(ws_id 1)"
RUN_WAIT_S="${RUN_WAIT_S:-300}"
failures=0
note_fail() { failures=$((failures + 1)); warn "FAIL: $*"; }
created_workflows=()

cleanup() {
    local id
    for id in ${created_workflows[*]:-}; do
        curl -sfm 10 -X DELETE -H "Authorization: Bearer ${API_KEY}" \
            "http://127.0.0.1:${PORTFWD_PORT}/api/v1/me/workflows/${id}" >/dev/null 2>&1 || true
    done
    kc delete workspace "${WS}" --ignore-not-found >/dev/null 2>&1 || true
    kubectl --context "${CTX}" -n "${NS}" delete deployment/echo-1455 service/echo-1455 configmap/echo-1455-config >/dev/null 2>&1 || true
    # This trap REPLACES us70-common's own EXIT trap (one trap per
    # signal; our function also shadows its name) — reap its port-forward
    # or the forward leaks past script exit.
    if [[ -n "${PF_PID:-}" ]]; then
        kill "${PF_PID}" 2>/dev/null || true
        wait "${PF_PID}" 2>/dev/null || true
    fi
}
trap cleanup EXIT

api() { # method path [body] -> response body; status in ${api_status}
    local method="$1" path="$2" body="${3:-}"
    local args=(-s -m 20 -X "${method}" -H "Authorization: Bearer ${API_KEY}" \
        -H "Content-Type: application/json" -w '\n%{http_code}' \
        "http://127.0.0.1:${PORTFWD_PORT}${path}")
    [[ -n "${body}" ]] && args+=(-d "${body}")
    local out; out=$(curl "${args[@]}")
    api_status="${out##*$'\n'}"
    printf '%s' "${out%$'\n'*}"
}

# wait_run <run-id> <timeout_s> — poll to a terminal state; sets run_row.
wait_run() {
    local rid="$1" timeout_s="$2" i run_json
    run_row=""
    for ((i = 0; i < timeout_s; i += 6)); do
        run_json=$(api GET "/api/v1/me/runs/${rid}")
        run_row="${run_json}"
        run_status=$(printf '%s' "${run_json}" | jq -r '.status // empty')
        [[ "${run_status}" == "succeeded" || "${run_status}" == "failed" ]] && return 0
        sleep 6
    done
    return 1
}

# wait_env_present ws VAR=VALUE timeout_s — poll the spawned child env
# (same helper shape as us-70-faults-e2e.sh; the child env observation is
# the harness's proxy for "the batch carrying the var materialized" —
# the same batch file the http-node reader consumes).
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

log "R0 — workspace ${WS} up"
seed_workspace "${WS}"
wait_phase "${WS}" Active 360 || die "R0: workspace never Active"
ok "workspace Active"

# -----------------------------------------------------------------------------
log "R1 — script-node mode contract (execute OR fail loud, nothing between)"

WF_BODY=$(jq -nc --arg w "${WS}" '{name:"e2e-1455-scriptenv",targetWorkspaceId:$w,
    specYaml:"{\"nodes\":[{\"id\":\"s1\",\"type\":\"script\",\"data\":{\"language\":\"python\",\"handler\":\"def handler(input):\\n    return {\\\"marker\\\": \\\"e2e-1455-scriptenv-ran\\\"}\\n\"}}],\"edges\":[]}"}')
WF_RESP=$(api POST /api/v1/me/workflows "${WF_BODY}")
if [[ "${api_status}" != "201" ]]; then
    die "R1 setup: workflow create failed: ${api_status} ${WF_RESP}"
fi
WF_ID=$(printf '%s' "${WF_RESP}" | jq -r '.id')
created_workflows+=("${WF_ID}")

RUN_RESP=$(api POST "/api/v1/me/workflows/${WF_ID}/runs" '{"input":{}}')
[[ "${api_status}" == "201" || "${api_status}" == "202" ]] \
    || die "R1 setup: run create failed: ${api_status} ${RUN_RESP}"
RUN_ID=$(printf '%s' "${RUN_RESP}" | jq -r '.id // empty')

if ! wait_run "${RUN_ID}" "${RUN_WAIT_S}"; then
    note_fail "R1: run never reached a terminal state within ${RUN_WAIT_S}s (last: ${run_status:-none})"
else
    # Assert the marker on the extracted .output — the raw run row carries
    # the specSnapshot (handler source embedded), so matching the marker
    # there would be a tautology (r4 finding; the R2b class). The script
    # node's output IS the handler's return dict, so the literal can only
    # come from a real execution.
    r1_output=$(printf '%s' "${run_row}" | jq -r '.output | if type == "string" then . else tostring end')
    if [[ "${run_status}" == "succeeded" && "${r1_output}" == *"e2e-1455-scriptenv-ran"* ]]; then
        ok "R1a: single-container mode — script node executed post-EnvCheck (marker present in the node output)"
    elif [[ "${run_status}" == "failed" && "${run_row}" == *"script_env_unavailable"* ]]; then
        if [[ "${run_row}" == *"sidecar"* ]]; then
            ok "R1b: sidecar mode — script node failed LOUD (script_env_unavailable naming the sidecar cause)"
        else
            note_fail "R1b: failed with script_env_unavailable but the detail does not name the sidecar cause: ${run_row:0:300}"
        fi
    elif [[ "${run_status}" == "succeeded" ]]; then
        note_fail "R1a: run succeeded but the handler marker is missing from the node output: ${r1_output:0:300}"
    else
        note_fail "R1: terminal shape outside the mode contract (status=${run_status}): ${run_row:0:300}"
    fi
fi

# -----------------------------------------------------------------------------
log "R2 — http-node {{secrets.*}} resolves from the materialized coordinate"

# In-cluster header-echo upstream (the 1417 in-cluster mock pattern).
kubectl --context "${CTX}" apply -f - >/dev/null <<'ECHO'
apiVersion: v1
kind: ConfigMap
metadata:
  name: echo-1455-config
  namespace: llmsafespaces
data:
  serve.py: |
    import json
    from http.server import BaseHTTPRequestHandler, HTTPServer
    class H(BaseHTTPRequestHandler):
        def do_GET(self):
            resp = json.dumps({
                "auth": self.headers.get("Authorization", ""),
                "probe": self.headers.get("X-Probe", ""),
            }).encode()
            self.send_response(200)
            self.send_header("content-type", "application/json")
            self.send_header("content-length", str(len(resp)))
            self.end_headers()
            self.wfile.write(resp)
        def log_message(self, *a):
            pass
    HTTPServer(("0.0.0.0", 8080), H).serve_forever()
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: echo-1455
  namespace: llmsafespaces
spec:
  replicas: 1
  selector: {matchLabels: {app: echo-1455}}
  template:
    metadata:
      labels:
        app: echo-1455
        app.kubernetes.io/name: llmsafespaces
    spec:
      containers:
        - name: serve
          image: python:3.12-alpine
          command: ["python", "/srv/serve.py"]
          ports: [{containerPort: 8080}]
          volumeMounts: [{name: cfg, mountPath: /srv}]
      volumes:
        - name: cfg
          configMap: {name: echo-1455-config}
---
apiVersion: v1
kind: Service
metadata:
  name: echo-1455
  namespace: llmsafespaces
spec:
  selector: {app: echo-1455}
  ports: [{port: 80, targetPort: 8080}]
ECHO
kubectl --context "${CTX}" -n "${NS}" rollout status deployment/echo-1455 --timeout=180s >/dev/null \
    || die "R2 setup: echo upstream never rolled out"

# Bind + wait for materialization: wait_env_present observes the value in
# the spawned child env — the same materialized batch secrets-env the
# workflow http-node reader consumes.
bind_env "${WS}" "WT1455_PROBE_TOKEN" "sekret-1455-e2e"
secrets_converged "${WS}" 300 || die "R2 setup: secretsDelivery never healthy"
wait_env_present "${WS}" "WT1455_PROBE_TOKEN=sekret-1455-e2e" 300 \
    || die "R2 setup: probe secret never materialized"

R2_BODY=$(jq -nc --arg w "${WS}" --arg url "http://echo-1455.llmsafespaces.svc/echo" '{name:"e2e-1455-http-secrets",targetWorkspaceId:$w,
    specYaml:("{\"nodes\":[{\"id\":\"h1\",\"type\":\"http\",\"data\":{\"method\":\"GET\",\"url\":\"" + $url + "\",\"headers\":{\"Authorization\":\"Bearer {{secrets.WT1455_PROBE_TOKEN}}\",\"X-Probe\":\"{{secrets.WT1455_ABSENT_VAR}}\"}}}],\"edges\":[]}")}')
R2_RESP=$(api POST /api/v1/me/workflows "${R2_BODY}")
if [[ "${api_status}" != "201" ]]; then
    die "R2 setup: workflow create failed: ${api_status} ${R2_RESP}"
fi
R2_WF=$(printf '%s' "${R2_RESP}" | jq -r '.id')
created_workflows+=("${R2_WF}")

R2_RUN=$(api POST "/api/v1/me/workflows/${R2_WF}/runs" '{"input":{}}')
[[ "${api_status}" == "201" || "${api_status}" == "202" ]] \
    || die "R2 setup: run create failed: ${api_status} ${R2_RUN}"
R2_RUN_ID=$(printf '%s' "${R2_RUN}" | jq -r '.id // empty')

if ! wait_run "${R2_RUN_ID}" "${RUN_WAIT_S}"; then
    note_fail "R2: run never reached a terminal state within ${RUN_WAIT_S}s (last: ${run_status:-none})"
elif [[ "${run_status}" != "succeeded" ]]; then
    note_fail "R2a: http node run failed (wanted succeeded): ${run_row:0:300}"
else
    # Assert on the ECHOED BODY (.output.body — what the upstream actually
    # received), never the raw run row: the row embeds the workflow's own
    # specSnapshot, so asserting the literal there would be a tautology
    # (r3 finding). The resolved value exists nowhere in the spec, so its
    # presence in the echoed body can only come from a resolved header.
    r2_body=$(printf '%s' "${run_row}" | jq -r '.output | if type == "string" then . else (.body // tostring) end')
    if [[ "${r2_body}" != *"sekret-1455-e2e"* ]]; then
        note_fail "R2a: {{secrets.*}} did NOT resolve — echoed body lacks the bound value: ${r2_body:0:300}"
    else
        ok "R2a: http-node {{secrets.*}} resolved from the materialized secrets-env coordinate"
        if [[ "${r2_body}" == *"{{secrets.WT1455_ABSENT_VAR}}"* ]]; then
            ok "R2b: unbound ref stayed literal in the echoed request (documented pass-through semantics)"
        else
            note_fail "R2b: unbound ref vanished or expanded upstream — echoed body: ${r2_body:0:300}"
        fi
    fi
fi

if [[ "${failures}" -gt 0 ]]; then
    die "${failures} scriptenv e2e row(s) failed"
fi
log "issue-1455 scriptenv e2e: all rows green"
