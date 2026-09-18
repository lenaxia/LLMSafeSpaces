#!/usr/bin/env bash
# Issue #1417 — agent-prompt dotted-path templating e2e. The unit and
# handler-integration tiers prove renderTemplateRefs; this row proves it
# end to end on the cluster: a workflow run whose agent node addresses
# {{.body.topic}} (nested path, the webhook-envelope shape) and an
# unresolvable {{.missing.path}} executes against a mock OpenAI upstream
# that ECHOES the prompt — the run output must carry the RENDERED nested
# value and the LITERAL unresolvable ref.
#
#   T1 — happy:  {{.body.topic}} renders from nested run input
#   T2 — unhappy: {{.missing.path}} stays literal (inspectable, never
#        silently empty) — both assertions ride the SAME executed turn.
#
# Requires a workspace with an agent turn: a mock LLM upstream is
# deployed in-cluster (us-70 pattern) and a stub credential admits its
# model through the registry.
#
# Environment: same conventions as local/issue-1410-1412-automation-e2e.sh
# (see local/lib/us70-common.sh). Runs on the pool/nightly kind cluster.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/us70-common.sh
source "${SCRIPT_DIR}/lib/us70-common.sh"

harness_start

WS="$(ws_id 17)"
created_triggers=()
RUN_WAIT_S="${RUN_WAIT_S:-300}"
failures=0
note_fail() { failures=$((failures + 1)); warn "FAIL: $*"; }

cleanup() {
    local id
    for id in ${created_triggers[*]:-}; do
        curl -sfm 10 -X DELETE -H "Authorization: Bearer ${API_KEY}" \
            "http://127.0.0.1:${PORTFWD_PORT}/api/v1/me/triggers/${id}" >/dev/null 2>&1 || true
    done
    for id in ${CREATED_WF:-}; do
        curl -sfm 10 -X DELETE -H "Authorization: Bearer ${API_KEY}" \
            "http://127.0.0.1:${PORTFWD_PORT}/api/v1/me/workflows/${id}" >/dev/null 2>&1 || true
    done
    kc delete workspace "${WS}" --ignore-not-found >/dev/null 2>&1 || true
    kubectl --context "${CTX}" -n "${NS}" delete deployment/mock-llm-1417 configmap/mock-llm-1417-config service/mock-llm-1417 --ignore-not-found >/dev/null 2>&1 || true
}
trap cleanup EXIT

api() { # method path [body] -> response body; status in ${api_status}
    local method="$1" path="$2" body="${3:-}"
    local args=(-s -m 30 -X "${method}" -H "Authorization: Bearer ${API_KEY}" \
        -H "Content-Type: application/json" -w '\n%{http_code}' \
        "http://127.0.0.1:${PORTFWD_PORT}${path}")
    [[ -n "${body}" ]] && args+=(-d "${body}")
    local out; out=$(curl "${args[@]}")
    api_status="${out##*$'\n'}"
    printf '%s' "${out%$'\n'*}"
}

# --- mock LLM upstream: echoes the last user message verbatim -----------
kubectl --context "${CTX}" apply -f - >/dev/null <<'MOCK'
apiVersion: v1
kind: ConfigMap
metadata:
  name: mock-llm-1417-config
  namespace: llmsafespaces
data:
  serve.py: |
    import json
    from http.server import BaseHTTPRequestHandler, HTTPServer
    class H(BaseHTTPRequestHandler):
        def do_POST(self):
            n = int(self.headers.get("content-length", 0))
            req = json.loads(self.rfile.read(n) or b"{}")
            msgs = req.get("messages", [])
            content = ""
            for m in reversed(msgs):
                if m.get("role") == "user":
                    parts = m.get("content")
                    if isinstance(parts, str):
                        content = parts
                    elif isinstance(parts, list):
                        content = "".join(p.get("text", "") for p in parts if isinstance(p, dict))
                    break
            resp = json.dumps({
                "id": "chatcmpl-mock", "object": "chat.completion",
                "created": 0, "model": req.get("model", "mock-model-1"),
                "choices": [{"index": 0, "message": {"role": "assistant", "content": content}, "finish_reason": "stop"}],
                "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
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
  name: mock-llm-1417
  namespace: llmsafespaces
spec:
  replicas: 1
  selector: {matchLabels: {app: mock-llm-1417}}
  template:
    metadata:
      labels:
        app: mock-llm-1417
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
          configMap: {name: mock-llm-1417-config}
---
apiVersion: v1
kind: Service
metadata:
  name: mock-llm-1417
  namespace: llmsafespaces
spec:
  selector: {app: mock-llm-1417}
  ports: [{port: 80, targetPort: 8080}]
MOCK

kc rollout status deployment/mock-llm-1417 --timeout=120s >/dev/null

# --- workspace + admitted stub model -------------------------------------
seed_workspace "${WS}"
wait_phase "${WS}" Ready 600 || die "workspace ${WS} never Ready"

SLUG="mock1417"
MODEL="mock-1417-model"
create_stub_credential "${SLUG}" "${MODEL}" "http://mock-llm-1417.llmsafespaces.svc/v1" >/dev/null
if ! registry_admits "${WS}" "${SLUG}" "${MODEL}" 180; then
    die "registry never admitted ${SLUG}/${MODEL} on ${WS}"
fi

# --- workflow with a nested-ref + unresolvable-ref prompt ----------------
SPEC=$(jq -nc --arg model "${SLUG}/${MODEL}" '{
  nodes: [{id: "say", type: "agent", data: {
    prompt: "PROMPT-WAS: {{.body.topic}} / {{.missing.path}}",
    agent: $model
  }}],
  edges: []
}')
WF_BODY=$(jq -nc --arg spec "${SPEC}" --arg ws "${WS}" \
  '{name:"e2e-1417-templating", specYaml: $spec, targetWorkspaceId: $ws}')
wf_resp=$(api POST /api/v1/me/workflows "${WF_BODY}")
[[ "${api_status}" == "201" ]] || die "workflow create failed: ${api_status} ${wf_resp}"
WF=$(printf '%s' "${wf_resp}" | jq -r '.id')
CREATED_WF="${WF}"

run_resp=$(api POST "/api/v1/me/workflows/${WF}/runs" \
    '{"input":{"body":{"topic":"e2e-nested-topic"}}}')
[[ "${api_status}" == "202" ]] || die "run start failed: ${api_status} ${run_resp}"
RUN=$(printf '%s' "${run_resp}" | jq -r '.id')

status=""
for ((i = 0; i < RUN_WAIT_S; i += 6)); do
    run_json=$(api GET "/api/v1/me/runs/${RUN}")
    status=$(printf '%s' "${run_json}" | jq -r '.status // empty')
    [[ "${status}" == "succeeded" || "${status}" == "failed" ]] && break
    sleep 6
done

out=$(printf '%s' "${run_json}" | jq -r '.output.response // .output // "" | if type == "string" then . else tostring end')
if [[ "${status}" != "succeeded" ]]; then
    note_fail "run finished '${status}' (wanted succeeded); output: ${out:0:300}"
elif ! grep -q "PROMPT-WAS: e2e-nested-topic" <<<"${out}"; then
    note_fail "T1 (happy): nested ref did NOT render — output: ${out:0:300}"
else
    ok "T1 (happy): {{.body.topic}} rendered from nested run input"
    if grep -q "{{.missing.path}}" <<<"${out}"; then
        ok "T2 (unhappy): unresolvable ref stayed literal (inspectable)"
    else
        note_fail "T2 (unhappy): unresolvable ref vanished or expanded — output: ${out:0:300}"
    fi
fi

# -----------------------------------------------------------------------------
# #1446: fire results are observable (and owner-bound) — routine fire
# with captureMode full must expose its captured output via the fires
# list; a foreign trigger UUID must 404 (ownership guard).
# -----------------------------------------------------------------------------
log "#1446 — fire result observability rows"

FTR_RESP=$(api POST /api/v1/me/triggers '{"name":"e2e-1417-fires","sourceType":"cron","sourceConfig":{"expr":"0 3 1 * *","tz":"UTC"},"prompt":"ACK","captureMode":"full","autoDisableAfter":10}')
if [[ "${api_status}" == "201" ]]; then
    FTR_ID=$(printf '%s' "${FTR_RESP}" | jq -r '.id')
    created_triggers+=("${FTR_ID}")
    ok "T3 setup: fires-result trigger created"
else
    note_fail "T3 setup: trigger create failed: ${api_status} ${FTR_RESP:0:120}"
fi

# Seed a fire row with a result directly through the workflow target —
# simplest deterministic route: point the trigger at the (deleted-by-
# cleanup) workflow is racy; instead assert on R4-style failed fires is
# the 1410 script's lane. Here: assert the RESULT field round-trips on
# the fires endpoint for the created trigger (empty list = field absent
# is fine; the shape assertion runs on whatever rows exist) + guard.
fires_json=$(api GET "/api/v1/me/triggers/${FTR_ID:-none}/fires")
if [[ "${api_status}" == "200" ]] && printf '%s' "${fires_json}" | jq -e '.fires' >/dev/null 2>&1; then
    ok "T3: fires endpoint answers for the owner (rows: $(printf '%s' "${fires_json}" | jq '.fires | length'))"
else
    note_fail "T3: fires endpoint failed for the owner: ${api_status}"
fi

# Unhappy: a foreign/nonexistent trigger UUID must 404 — the ownership
# guard (GetTrigger owner-scoped) blocks cross-tenant fire reads.
foreign=$(api GET "/api/v1/me/triggers/00000000-0000-4000-8000-000000000099/fires")
if [[ "${api_status}" == "404" ]]; then
    ok "T4: foreign trigger UUID 404s (ownership guard)"
else
    note_fail "T4: foreign UUID returned ${api_status}, expected 404"
fi

# T5 (happy, #1446): a captureMode-full ROUTINE fire's captured output
# reaches the caller via .result — real content, not liveness.
CAP_TR_RESP=$(api POST /api/v1/me/triggers "$(jq -nc --arg ws "${WS}" \
    '{name:"e2e-1417-capfull",sourceType:"webhook",sourceConfig:{method:"POST"},workspaceId:$ws,prompt:"E2E-CAPTURED-MARKER reply ACK",captureMode:"full",autoDisableAfter:10}')")
[[ "${api_status}" == "201" ]] || note_fail "T5 setup: capture trigger create ${api_status} ${CAP_TR_RESP:0:120}"
CAP_ID=$(printf '%s' "${CAP_TR_RESP}" | jq -r '.id // empty')
[[ -n "${CAP_ID}" ]] && created_triggers+=("${CAP_ID}")
CAP_ROT=$(api POST "/api/v1/me/triggers/${CAP_ID}/rotate-secret")
CAP_SECRET=$(printf '%s' "${CAP_ROT}" | jq -r '.webhookSecret // empty')
CAP_URL=$(printf '%s' "${CAP_ROT}" | jq -r '.webhookUrl // empty')
cap_payload='{"marker":"cap-full-e2e"}'
cap_sig="sha256=$(printf '%s' "${cap_payload}" | openssl dgst -sha256 -hmac "${CAP_SECRET}" -hex | awk '{print $NF}')"
if [[ -n "${CAP_URL}" && -n "${CAP_SECRET}" ]]; then
    cap_code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:${PORTFWD_PORT}${CAP_URL#*api.safespaces.dev}" \
        -H "Content-Type: application/json" -H "X-Hub-Signature-256: ${cap_sig}" \
        -d "${cap_payload}" --max-time 20) || cap_code="curl-failed"
    # NOTE: delivered through the port-forward (in-cluster path, no CF).
    cap_result=""
    for ((i = 0; i < 60; i += 6)); do
        cap_fires=$(api GET "/api/v1/me/triggers/${CAP_ID}/fires")
        cap_status_one=$(printf '%s' "${cap_fires}" | jq -r '.fires[0].status // empty')
        [[ "${cap_status_one}" == "delivered" || "${cap_status_one}" == "failed" ]] && break
        sleep 6
    done
    cap_result=$(printf '%s' "${cap_fires}" | jq -r '.fires[0].result // empty' | head -c 400)
    if [[ "${cap_status_one}" == "delivered" && "${cap_result}" != "" && "${cap_result}" != "null" ]]; then
        ok "T5: captureMode-full fire exposes .result (delivered, content present)"
    else
        note_fail "T5: status=${cap_status_one:-none} result=${cap_result:-<empty>}"
    fi
else
    note_fail "T5 setup: rotate failed (${api_status})"
fi

# T6 (unhappy, #1446): a FAILED routine exposes its error cause via
# .result — delete the target workspace first, then deliver.
WS2="$(ws_id 18)"
seed_workspace "${WS2}" >/dev/null 2>&1 || true
FAIL_TR_RESP=$(api POST /api/v1/me/triggers "$(jq -nc --arg ws "${WS2}" \
    '{name:"e2e-1417-failcause",sourceType:"webhook",sourceConfig:{method:"POST"},workspaceId:$ws,prompt:"ACK",captureMode:"full",autoDisableAfter:50}')")
FAIL_ID=$(printf '%s' "${FAIL_TR_RESP}" | jq -r '.id // empty')
[[ -n "${FAIL_ID}" ]] && created_triggers+=("${FAIL_ID}")
kc delete workspace "${WS2}" --ignore-not-found >/dev/null 2>&1 || true
# give the controller a beat to tear the pod down
sleep 10
FAIL_ROT=$(api POST "/api/v1/me/triggers/${FAIL_ID}/rotate-secret")
FAIL_SECRET=$(printf '%s' "${FAIL_ROT}" | jq -r '.webhookSecret // empty')
FAIL_URL=$(printf '%s' "${FAIL_ROT}" | jq -r '.webhookUrl // empty')
fail_payload='{"x":1}'
fail_sig="sha256=$(printf '%s' "${fail_payload}" | openssl dgst -sha256 -hmac "${FAIL_SECRET}" -hex | awk '{print $NF}')"
if [[ -n "${FAIL_URL}" && -n "${FAIL_SECRET}" ]]; then
    curl -s -o /dev/null -X POST "http://127.0.0.1:${PORTFWD_PORT}${FAIL_URL#*api.safespaces.dev}" \
        -H "Content-Type: application/json" -H "X-Hub-Signature-256: ${fail_sig}" \
        -d "${fail_payload}" --max-time 20 || true
    fail_result=""
    for ((i = 0; i < 60; i += 6)); do
        fail_fires=$(api GET "/api/v1/me/triggers/${FAIL_ID}/fires")
        fail_status_one=$(printf '%s' "${fail_fires}" | jq -r '.fires[0].status // empty')
        [[ "${fail_status_one}" == "failed" ]] && break
        sleep 6
    done
    fail_result=$(printf '%s' "${fail_fires}" | jq -r '.fires[0].result // empty' | head -c 300)
    if [[ "${fail_status_one}" == "failed" && "${fail_result}" == *'"error"'* ]]; then
        ok "T6: failed routine exposes its cause via .result"
    else
        note_fail "T6: status=${fail_status_one:-none} result=${fail_result:-<empty>}"
    fi
else
    note_fail "T6 setup: rotate failed (${api_status})"
fi

# T7 (#1451): routine agent calls retry transient upstream 5xx — the
# provider blip class the engine now masks (bounded, timeouts excluded).
# Structural pin only: the behavioral matrix lives in the engine tests
# (TestExecuteWithRetry_*); asserting a live provider blip from e2e
# would be flake-shaped by definition. The row pins the WIRING: the
# scheduler-level test that fails if the retry call site is reverted.
log "T7: retry wiring pinned at scheduler level (TestScheduler_RoutineFireRetriesTransient5xx in CI)"

if [[ "${failures}" -gt 0 ]]; then
    die "${failures} templating e2e row(s) failed"
fi
log "issue-1417 templating e2e: all rows green"
