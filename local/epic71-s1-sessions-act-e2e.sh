#!/usr/bin/env bash
# Epic 71 / s1-sessions (#1372) — the sessions-cluster write routes
# through agentd Act, cluster-bound rows.
#
# The pool arms AGENTD_STATE_AUTHORITY (us-70-delivery-pool.yml api.env),
# so every platform route below IS the Act path in the authority regime:
#
#   S1a — happy sync send: POST /sessions/{sid}/message → 200 with a
#         contract assistant message (the Act send round trip).
#   S1b — abort PREEMPTS an in-flight turn: a slow mock turn starts, the
#         abort returns 204 while the turn still runs (the r2-f2 pin at
#         cluster level — queueing behind the turn would take the turn's
#         full duration), and the session settles back to idle.
#   S1c — rename: PUT /sessions/{sid}/title → 204 and the AGENT-side
#         title changes (the PATCH rides the actor; the periodic title
#         fetch must not resurrect the old name).
#   S1d — delete: DELETE /sessions/{sid} → 204 and the harness session is
#         gone (agent-side GET 404s).
#   S1e — unhappy: send/abort/delete against a nonexistent session keep
#         the pinned 502 bodies (the response contract does not move).
#
# Environment: same conventions as local/us-70-secret-delivery-e2e.sh
# (see local/lib/us70-common.sh). SEAM-INERT — the pool runs this BEFORE
# the fault seam arms (pin: epic71_s1_sessions_script_test).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/us70-common.sh
source "${SCRIPT_DIR}/lib/us70-common.sh"

# Distinct workspace base (uuid column): sibling suites share the pool
# cluster; a shared base would collide on ws_id suffixes.
WS_BASE="${WS_BASE:-e2e71100-0000-4000-8000-000000000000}"

S1B_ABORT_BUDGET_S="${S1B_ABORT_BUDGET_S:-10}"
# The idle settle is bounded by the slow turn's OWN end, not the abort:
# the abort stops the TURN (the preempt row), but the in-flight provider
# HTTP call runs to its completion before the harness reaps the turn —
# the first pool run pinned this (idle unobserved inside 30s of a 45s
# turn). Budget = turn duration + reaping margin.
S1B_IDLE_BUDGET_S="${S1B_IDLE_BUDGET_S:-$(( S1B_SLOW_TURN_S + 60 ))}"
S1B_SLOW_TURN_S="${S1B_SLOW_TURN_S:-45}"

failures=0
note_fail() { failures=$((failures + 1)); warn "FAIL: $*"; }

# http_code_of <method> <path> <body-or-""> [out_body_file] — one
# authenticated API call; echoes the http code.
http_code_of() {
    local method="$1" path="$2" body="${3:-}" out="${4:-}"
    local args=(-s -m 180 -o "${out:-/dev/null}" -w '%{http_code}'
        -X "${method}" -H "Authorization: Bearer ${AUTH_TOKEN:?}")
    [[ -n "${body}" ]] && args+=(-H 'Content-Type: application/json' -d "${body}")
    curl "${args[@]}" "http://127.0.0.1:${PORTFWD_PORT}${path}" 2>/dev/null || echo 000
}

# agent_get_session_code <pod> <pw> <sid> — the harness-side session GET
# status code (the delete row's ground truth — the API's own read path
# would make the assert circular).
agent_get_session_code() {
    kc exec "$1" -c workspace -- curl -sm 10 -u "opencode:$2" \
        -o /dev/null -w '%{http_code}' "http://127.0.0.1:4096/session/$3" 2>/dev/null || echo 000
}

# agent_field <pod> <pw> <sid> <jq-expr> — one field off the harness
# session object (empty on any failure).
agent_field() {
    kc exec "$1" -c workspace -- curl -sm 10 -u "opencode:$2" \
        "http://127.0.0.1:4096/session/$3" 2>/dev/null \
        | jq -r "$4 // empty" 2>/dev/null || true
}

# ----------------------------------------------------------------------------

log "epic71/s1-sessions rows: harness + mock upstream + workspace setup"
harness_start
seed_session "${USER_ID}"

# Mock upstream (the AC-1d recipe, plus a SLOW mode for the abort row:
# a prompt containing S1-SLOW-TURN sleeps S1B_SLOW_TURN_S before
# replying — a turn long enough that a queued abort cannot beat it).
kubectl --context "${CTX}" apply -f - >/dev/null <<'MOCK'
apiVersion: v1
kind: ConfigMap
metadata:
  name: mock-llm-s1-config
  namespace: llmsafespaces
data:
  serve.py: |
    import json, time
    from http.server import BaseHTTPRequestHandler, HTTPServer
    class H(BaseHTTPRequestHandler):
        def do_POST(self):
            n = int(self.headers.get("content-length", 0))
            body = self.rfile.read(n)
            marker = "S1-TURN-OK"
            if b"S1-SLOW-TURN" in body:
                time.sleep(int(__import__("os").environ.get("S1B_SLOW_TURN_S", "45")))
            if b'"stream":true' in body or b'"stream": true' in body:
                def chunk(delta, finish=None):
                    return json.dumps({
                        "id": "chatcmpl-mock", "object": "chat.completion.chunk",
                        "created": 0, "model": "mock-model-s1",
                        "choices": [{"index": 0, "delta": delta, "finish_reason": finish}],
                    })
                frames = "".join([
                    "data: " + chunk({"role": "assistant", "content": ""}) + "\n\n",
                    "data: " + chunk({"content": marker}) + "\n\n",
                    "data: " + chunk({}, finish="stop") + "\n\n",
                    "data: [DONE]\n\n",
                ])
                resp = frames.encode(); ctype = "text/event-stream"
            else:
                resp = json.dumps({
                    "id": "chatcmpl-mock", "object": "chat.completion",
                    "created": 0, "model": "mock-model-s1",
                    "choices": [{"index": 0, "message": {"role": "assistant", "content": marker}, "finish_reason": "stop"}],
                }).encode(); ctype = "application/json"
            self.send_response(200)
            self.send_header("content-type", ctype)
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
  name: mock-llm-s1
  namespace: llmsafespaces
spec:
  replicas: 1
  selector: {matchLabels: {app: mock-llm-s1}}
  template:
    metadata:
      labels:
        app: mock-llm-s1
        app.kubernetes.io/name: llmsafespaces
        app.kubernetes.io/instance: llmsafespaces
        app.kubernetes.io/component: relay-router
    spec:
      containers:
        - name: serve
          image: python:3.12-alpine
          env: [{name: S1B_SLOW_TURN_S, value: "${S1B_SLOW_TURN_S}"}]
          command: ["python", "/srv/serve.py"]
          volumeMounts: [{name: cfg, mountPath: /srv}]
      volumes:
        - name: cfg
          configMap: {name: mock-llm-s1-config}
---
apiVersion: v1
kind: Service
metadata:
  name: mock-llm-s1
  namespace: llmsafespaces
spec:
  clusterIP: 10.217.200.201
  selector: {app: mock-llm-s1}
  ports: [{port: 80, targetPort: 8080}]
MOCK
kubectl --context "${CTX}" -n "${NS}" rollout status deployment/mock-llm-s1 --timeout=180s >/dev/null \
    || die "s1: mock upstream failed to deploy"

S1_WS=$(ws_id 73)
S1_CRED=$(create_stub_credential "s1-stub" "mock-model-s1" "http://mock-llm-s1.${NS}.svc/v1")
seed_workspace "${S1_WS}"
S1_BIND=$(curl -sm 30 -o /dev/null -w '%{http_code}' -X POST \
    -H "Authorization: Bearer ${AUTH_TOKEN}" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/provider-credentials/${S1_CRED}/bind/${S1_WS}")
[[ "${S1_BIND}" == 2* ]] || die "s1: credential bind failed: HTTP ${S1_BIND}"
wait_phase "${S1_WS}" Active 240 || die "s1: workspace never Active"
secrets_converged "${S1_WS}" 120 || die "s1: secretsDelivery not converged"
registry_admits "${S1_WS}" "s1-stub" "mock-model-s1" 120 \
    || die "s1: mock provider never admitted to the registry"

S1_POD=$(pod_of "${S1_WS}")
S1_PW=$(kc get secret "workspace-pw-${S1_WS}" -o jsonpath='{.data.password}' | base64 -d)

# The platform session (sessions/new is the UNMIGRATED service route —
# honest staging; #1372's five sites are the routes asserted below).
S1_SID=$(curl -sfm 60 -X POST -H "Authorization: Bearer ${AUTH_TOKEN}" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${S1_WS}/sessions/new" | jq -r '.sessionId // empty')
[[ -n "${S1_SID}" ]] || die "s1: platform session create failed"
ok "s1: staged ws=${S1_WS} session=${S1_SID}"

# --- S1a: happy synchronous send through Act ------------------------------
log "S1a: sync send through Act returns the contract assistant message"
S1A_BODY=$(mktemp)
S1A_CODE=$(http_code_of POST "/api/v1/workspaces/${S1_WS}/sessions/${S1_SID}/message" \
    '{"parts":[{"type":"text","text":"reply with the canned marker"}],"model":{"modelID":"mock-model-s1","providerID":"s1-stub"}}' "${S1A_BODY}")
S1A_ID=$(jq -r '.id // empty' "${S1A_BODY}" 2>/dev/null || true)
S1A_TEXT=$(jq -r '.parts[0].text // empty' "${S1A_BODY}" 2>/dev/null || true)
if [[ "${S1A_CODE}" == "200" && -n "${S1A_ID}" && "${S1A_TEXT}" == *S1-TURN-OK* ]]; then
    ok "S1a Act send → 200, message id=${S1A_ID}, marker present"
else
    note_fail "S1a Act send: code=${S1A_CODE} id='${S1A_ID}' text='${S1A_TEXT:0:80}' body=$(head -c 200 "${S1A_BODY}")"
fi
rm -f "${S1A_BODY}"

# --- S1b: abort PREEMPTS the in-flight turn --------------------------------
log "S1b: abort returns while the slow turn still runs (budget ${S1B_ABORT_BUDGET_S}s < turn ${S1B_SLOW_TURN_S}s)"
S1B_SEND_LOG=/tmp/e71s1_slow_send.out
(S1B_CODE=$(http_code_of POST "/api/v1/workspaces/${S1_WS}/sessions/${S1_SID}/message" \
    '{"parts":[{"type":"text","text":"S1-SLOW-TURN take your time"}],"model":{"modelID":"mock-model-s1","providerID":"s1-stub"}}' /dev/null) \
    ; echo "${S1B_CODE}" >"${S1B_SEND_LOG}") &
S1B_SEND_PID=$!
sleep 5 # let the turn start (registry/pod round trips done, mock sleeping)

S1B_T0=$(date +%s)
S1B_ABORT=$(http_code_of POST "/api/v1/workspaces/${S1_WS}/sessions/${S1_SID}/abort" '' )
S1B_ELAPSED=$(( $(date +%s) - S1B_T0 ))
if [[ "${S1B_ABORT}" == "204" && ${S1B_ELAPSED} -le ${S1B_ABORT_BUDGET_S} ]]; then
    ok "S1b abort preempted the in-flight turn: 204 in ${S1B_ELAPSED}s (budget ${S1B_ABORT_BUDGET_S}s)"
else
    note_fail "S1b abort: code=${S1B_ABORT} elapsed=${S1B_ELAPSED}s (queued behind the turn? budget ${S1B_ABORT_BUDGET_S}s)"
fi

# The interrupted session must settle back to idle (harness-side truth).
S1B_IDLE=0
for _ in $(seq 1 $(( S1B_IDLE_BUDGET_S / 3 ))); do
    if [[ "$(agent_field "${S1_POD}" "${S1_PW}" "${S1_SID}" '.status.type')" == "idle" ]]; then
        S1B_IDLE=1; break
    fi
    sleep 3
done
if [[ ${S1B_IDLE} == 1 ]]; then
    ok "S1b session settled idle after the abort"
else
    note_fail "S1b session never returned to idle within ${S1B_IDLE_BUDGET_S}s"
fi

# --- S1c: rename through Act -----------------------------------------------
log "S1c: title rename lands agent-side (the PATCH rides the actor)"
S1C_TITLE="s1-act-renamed-$(date +%s)"
S1C_CODE=$(http_code_of PUT "/api/v1/workspaces/${S1_WS}/sessions/${S1_SID}/title" "{\"title\":\"${S1C_TITLE}\"}")
S1C_AGENT_TITLE=""
for _ in $(seq 1 10); do
    S1C_AGENT_TITLE=$(agent_field "${S1_POD}" "${S1_PW}" "${S1_SID}" '.title')
    [[ "${S1C_AGENT_TITLE}" == "${S1C_TITLE}" ]] && break
    sleep 2
done
if [[ "${S1C_CODE}" == "204" && "${S1C_AGENT_TITLE}" == "${S1C_TITLE}" ]]; then
    ok "S1c rename → 204 and agent-side title='${S1C_AGENT_TITLE}'"
else
    note_fail "S1c rename: code=${S1C_CODE} agent-title='${S1C_AGENT_TITLE}' want='${S1C_TITLE}'"
fi

# --- S1d: delete through Act -----------------------------------------------
log "S1d: delete removes the harness session"
S1D_CODE=$(http_code_of DELETE "/api/v1/workspaces/${S1_WS}/sessions/${S1_SID}")
S1D_AGENT=""
for _ in $(seq 1 10); do
    S1D_AGENT=$(agent_get_session_code "${S1_POD}" "${S1_PW}" "${S1_SID}")
    [[ "${S1D_AGENT}" == "404" ]] && break
    sleep 2
done
if [[ "${S1D_CODE}" == "204" && "${S1D_AGENT}" == "404" ]]; then
    ok "S1d delete → 204 and the harness session is gone (agent GET ${S1D_AGENT})"
else
    note_fail "S1d delete: code=${S1D_CODE} agent-get='${S1D_AGENT}' (want 204/404)"
fi

# --- S1e: the unhappy rows keep the pinned 502 bodies ----------------------
log "S1e: nonexistent-session writes keep the pinned bodies"
S1E_SID="ses_e71s1deadbeefdeadbeefdeadbeef"

S1E_SEND_BODY=$(mktemp)
S1E_SEND=$(http_code_of POST "/api/v1/workspaces/${S1_WS}/sessions/${S1E_SID}/message" \
    '{"parts":[{"type":"text","text":"hello"}]}' "${S1E_SEND_BODY}")
S1E_ABORT_BODY=$(mktemp)
S1E_ABORT=$(http_code_of POST "/api/v1/workspaces/${S1_WS}/sessions/${S1E_SID}/abort" '' "${S1E_ABORT_BODY}")
S1E_DEL_BODY=$(mktemp)
S1E_DEL=$(http_code_of DELETE "/api/v1/workspaces/${S1_WS}/sessions/${S1E_SID}" "${S1E_DEL_BODY}")

if [[ "${S1E_SEND}" == "502" && "$(jq -r .error "${S1E_SEND_BODY}" 2>/dev/null)" == "failed to send message" ]] \
    && [[ "${S1E_ABORT}" == "502" && "$(jq -r .error "${S1E_ABORT_BODY}" 2>/dev/null)" == "failed to abort session" ]] \
    && [[ "${S1E_DEL}" == "502" && "$(jq -r .error "${S1E_DEL_BODY}" 2>/dev/null)" == "failed to delete session" ]]; then
    ok "S1e send/abort/delete on the dead session: 502s with the pinned bodies"
else
    note_fail "S1e: send=${S1E_SEND}'$(jq -r .error "${S1E_SEND_BODY}" 2>/dev/null)' abort=${S1E_ABORT}'$(jq -r .error "${S1E_ABORT_BODY}" 2>/dev/null)' delete=${S1E_DEL}'$(jq -r .error "${S1E_DEL_BODY}" 2>/dev/null)'"
fi
rm -f "${S1E_SEND_BODY}" "${S1E_ABORT_BODY}" "${S1E_DEL_BODY}"

# Reap the slow-turn sender (its outcome is the abort's business, not a row).
wait "${S1B_SEND_PID}" 2>/dev/null || true

# --- verdict ----------------------------------------------------------------
if [[ ${failures} -gt 0 ]]; then
    die "epic71/s1-sessions rows: ${failures} FAIL(S)"
fi
ok "epic71/s1-sessions rows: all green"
