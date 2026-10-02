#!/usr/bin/env bash
# #1332/#1333 — dev-preview tunnel e2e (kind).
#
# Complements local/test.sh (same kind-cluster + port-forwarded API +
# postgres-seeded users conventions, lib/us70-common.sh helpers). Closes
# the e2e acceptance legs both issues name:
#
#   #1333-A  bare /dev-preview/:port (no trailing slash) → 308 to the
#            slash form, query preserved (the incident's white-page shape:
#            relative assets resolved off the port segment).
#   #1333-B  follow the redirect → HTML served; the app's RELATIVE
#            stylesheet resolves under the port prefix → 200 text/css
#            with the expected body (the "not passing CSS" regression).
#   #1333-C  validation outranks normalization: /dev-preview/style.css
#            (the incident's mis-resolved asset path) and bare /4096
#            stay 400 — never redirected into a mask.
#   #1332-A  default install (no public origin configured, pods carry
#            the in-cluster svc coordinate): the dev_preview_url tool
#            FAILS LOUD naming LLMSAFESPACE_API_PUBLIC_URL — never
#            emits an .svc URL for the browser.
#   #1332-B  full wiring chain: patch the controller with
#            --api-public-url, recreate the pod, and the tool emits the
#            configured public FQDN (no .svc anywhere) — chart flag →
#            reconciler → pod env → tool output, end to end.
#
# Runs against any bootstrap.sh-shaped cluster (nightly / pool), same as
# the us-70 suites. The dev server is python3 -m http.server (present in
# the runtime base), serving an index.html with RELATIVE asset refs —
# exactly the mini4wd-track-editor incident topology.
set -Eeuo pipefail

cd "$(dirname "$0")"
# shellcheck source=lib/us70-common.sh
source lib/us70-common.sh

WS_DEVPORT="${WS_DEVPORT:-3000}"
TOOL_PF_PORT="${TOOL_PF_PORT:-18097}"
PUBLIC_ORIGIN_E2E="${PUBLIC_ORIGIN_E2E:-https://api.e2e.example}"
PREVIEW_BASE_E2E="${PREVIEW_BASE_E2E:-e2e.example}"
WS="$(ws_id 90)"

# ---------------------------------------------------------------------------
# Setup: harness session first (OWNER_ID must exist before the CR —
# spec.owner.userID is a required field), then the devPreview workspace
# + a relative-asset dev server.

harness_start

log "seeding workspace ${WS} (devPreview enabled)"
kc delete workspace "${WS}" --ignore-not-found >/dev/null 2>&1 || true
cat <<EOF | kc_apply_retry
apiVersion: llmsafespaces.dev/v1
kind: Workspace
metadata:
  name: ${WS}
  labels:
    user-id: ${OWNER_ID}
spec:
  owner:
    userID: ${OWNER_ID}
  runtime: ${RUNTIME_REF}
  networkAccess:
    devPreview: true
  storage:
    size: 1Gi
    accessMode: ReadWriteOnce
EOF
seed_workspace_metadata "${WS}" "${OWNER_ID}"
wait_phase "${WS}" Active 300 || { diagnose_workspace "${WS}"; die "workspace never reached Active"; }
POD="$(pod_of "${WS}")"
[[ -n "${POD}" ]] || die "no pod for ${WS}"
RCTNR="$(runtime_container "${POD}")"
[[ -n "${RCTNR}" ]] || die "could not resolve the runtime container of ${POD}"

log "starting the relative-asset dev server (python3 http.server :${WS_DEVPORT}) in ${POD}"
kc exec "${POD}" -c "${RCTNR}" -- mkdir -p /tmp/dpv 2>/dev/null || kc exec "${POD}" -- mkdir -p /tmp/dpv
kc exec "${POD}" -c "${RCTNR}" -- sh -c 'printf "%s\n" \
  "<!DOCTYPE html><html><head><link rel=\"stylesheet\" href=\"style.css?v=3\"></head>" \
  "<body>dpv-e2e-marker</body></html>" > /tmp/dpv/index.html' 2>/dev/null \
  || kc exec "${POD}" -- sh -c 'printf "%s\n" "<!DOCTYPE html><html><head></head><body>dpv-e2e-marker</body></html>" > /tmp/dpv/index.html'
kc exec "${POD}" -c "${RCTNR}" -- sh -c 'printf "%s\n" "body{background:#0f0}" > /tmp/dpv/style.css' 2>/dev/null \
  || kc exec "${POD}" -- sh -c 'printf "%s\n" "body{background:#0f0}" > /tmp/dpv/style.css'
kc exec "${POD}" -c "${RCTNR}" -- sh -c 'cd /tmp/dpv && nohup python3 -m http.server '"${WS_DEVPORT}"' >/tmp/dpv/server.log 2>&1 & sleep 1; curl -sfm 3 http://127.0.0.1:'"${WS_DEVPORT}"'/ >/dev/null' 2>/dev/null \
  || kc exec "${POD}" -- sh -c 'cd /tmp/dpv && nohup python3 -m http.server '"${WS_DEVPORT}"' >/tmp/dpv/server.log 2>&1 & sleep 1; curl -sfm 3 http://127.0.0.1:'"${WS_DEVPORT}"'/ >/dev/null'
ok "dev server up in-pod on :${WS_DEVPORT}"

BASE="http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WS}/dev-preview"
AUTH=(-H "Authorization: Bearer ${AUTH_TOKEN}")

# ---------------------------------------------------------------------------
# #1333 — the redirect legs.

log "#1333-A: bare port 308s to the slash form"
hdr=$(mktemp)
code=$(curl -sm 10 -o /dev/null -D "${hdr}" -w '%{http_code}' "${AUTH[@]}" "${BASE}/${WS_DEVPORT}")
[[ "${code}" == "308" ]] || die "expected 308 on bare /${WS_DEVPORT}, got ${code}"
loc=$(awk 'tolower($1)=="location:"{print $2}' "${hdr}" | tr -d '\r')
# Location may be path-relative (the API's form) or absolute — both are
# RFC 7231-valid; assert on the path + query.
loc_path="${loc#*://127.0.0.1:${PORTFWD_PORT}}"
[[ "${loc_path}" == "/api/v1/workspaces/${WS}/dev-preview/${WS_DEVPORT}/" ]] || die "unexpected Location: ${loc}"
ok "308 → ${loc}"

log "#1333-A2: query preserved on the redirect"
code=$(curl -sm 10 -o /dev/null -D "${hdr}" -w '%{http_code}' "${AUTH[@]}" "${BASE}/${WS_DEVPORT}?v=3&x=1")
loc=$(awk 'tolower($1)=="location:"{print $2}' "${hdr}" | tr -d '\r')
loc_path="${loc#*://127.0.0.1:${PORTFWD_PORT}}"
[[ "${code}" == "308" && "${loc_path}" == "/api/v1/workspaces/${WS}/dev-preview/${WS_DEVPORT}/?v=3&x=1" ]] \
  || die "expected 308 with preserved query, got ${code} ${loc}"
ok "308 preserves ?v=3&x=1"

log "#1333-B: redirect followed → HTML; relative CSS resolves under the port prefix"
body=$(curl -sLm 15 "${AUTH[@]}" "${BASE}/${WS_DEVPORT}")
[[ "${body}" == *dpv-e2e-marker* ]] || die "HTML marker missing after redirect follow: ${body:0:200}"
code=$(curl -sm 10 -o /dev/null -w '%{http_code}%{content_type}' "${AUTH[@]}" "${BASE}/${WS_DEVPORT}/style.css?v=3" | tr -d ' ')
[[ "${code}" == *"200text/css"* || "${code}" == 200* ]] || die "relative style.css via tunnel failed: ${code}"
css=$(curl -sm 10 "${AUTH[@]}" "${BASE}/${WS_DEVPORT}/style.css?v=3")
[[ "${css}" == *"#0f0"* ]] || die "style.css body wrong: ${css:0:120}"
ok "CSS passes through the tunnel (the incident regression)"

log "#1333-C: validation outranks normalization"
code=$(curl -sm 10 -o /dev/null -w '%{http_code}' "${AUTH[@]}" "${BASE}/style.css")
[[ "${code}" == "400" ]] || die "mis-resolved asset path must stay 400, got ${code}"
code=$(curl -sm 10 -o /dev/null -w '%{http_code}' "${AUTH[@]}" "${BASE}/4096")
[[ "${code}" == "400" ]] || die "bare denied port must stay 400, got ${code}"
ok "incident asset path + denied port stay 400"

# ---------------------------------------------------------------------------
# #1332 — the tool legs. The MCP tool is served by agentd's user mux
# (:4097, §D1 opencode:<workspace password>).

WS_PW=$(kc get secret "workspace-pw-${WS}" -o jsonpath='{.data.password}' | base64 -d)
[[ -n "${WS_PW}" ]] || die "workspace password secret unreadable"

MCP_CALL_SEQ=0
mcp_call() { # port → prints the text of content[0]
    # One port per call: consecutive calls (and calls across pod
    # recreates) otherwise race the previous kubectl port-forward's
    # shutdown for the bind — the new PF dies silently to /dev/null and
    # every curl hits the dead forward (observed as empty output on the
    # #1332-B leg while the endpoint answered by hand).
    MCP_CALL_SEQ=$((MCP_CALL_SEQ + 1))
    local pfport=$((TOOL_PF_PORT + MCP_CALL_SEQ)) pf=$! _i _out=""
    kc -n "${NS}" port-forward "pod/${POD}" "${pfport}:4097" >/dev/null 2>&1 &
    pf=$!
    for _i in $(seq 1 15); do
        _out=$(curl -sfm 15 -X POST "http://127.0.0.1:${pfport}/v1/mcp" \
            -u "opencode:${WS_PW}" \
            -H "Content-Type: application/json" \
            -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dev_preview_url","arguments":{"port":'"$1"'}}}' 2>/dev/null || true)
        # Retry until the mux serves: after a pod recreate agentd's boot
        # (opencode spawn + secrets materialize) can outlast the
        # port-forward's readiness — a 404-probe would never succeed on
        # this mux, so the authenticated POST itself is the probe.
        [[ -n "${_out}" ]] && break
        sleep 4
    done
    kill "${pf}" 2>/dev/null || true
    wait "${pf}" 2>/dev/null || true
    printf '%s' "${_out}" | jq -r '.result.content[0].text // empty'
}

# tool_url <text> — extract the URL from the tool output's markdown link
# (line 2: "[Open dev preview :N](URL)"). Line 1 is ALWAYS the
# LSP_DEV_PREVIEW_V1 marker — asserting against the whole payload can
# never match an origin prefix (round-2 review finding).
tool_url() {
    printf '%s\n' "$1" | sed -n 's/^.*(\(https\?:\/\/[^)]*\)).*$/\1/p' | head -1
}

# restore_controller — removes BOTH e2e controller flags. Idempotent and
# wired to EXIT: a die between patch and restore must not leave the
# cluster carrying --api-public-url/--preview-origin-base-domain (the
# round-2 review's cluster-poisoning finding).
CONTROLLER_PATCHED=0
restore_controller() {
    if [[ "${CONTROLLER_PATCHED}" == "1" ]]; then
        log "restoring the controller (removing the e2e flags)"
        kc get deploy llmsafespaces-controller -o jsonpath='{.spec.template.spec.containers[0].args}' \
            | jq -c 'map(select((startswith("--api-public-url=") or startswith("--preview-origin-base-domain=")) | not))' > /tmp/dpv-args.json
        kubectl --context "${CTX}" -n "${NS}" patch deployment llmsafespaces-controller --type json \
            -p "$(jq -nc --slurpfile a /tmp/dpv-args.json '[{"op":"replace","path":"/spec/template/spec/containers/0/args","value":$a[0]}]')" >/dev/null
        kc rollout status deployment/llmsafespaces-controller --timeout=180s >/dev/null || true
        CONTROLLER_PATCHED=0
    fi
    kc delete workspace "${WS}" --ignore-not-found >/dev/null 2>&1 || true
}
# Chain the lib's port-forward cleanup (replacing its trap wholesale would
# leave the API port-forward alive on manual runs).
trap 'restore_controller; cleanup' EXIT

# ---------------------------------------------------------------------------
# 0062 — dev_preview_headers e2e (design 0062 §6): a fixture service
# that REJECTS requests without X-Service-Key; the agent sets the
# header through the tool → the preview renders; clear → rejected
# again. Plus the pod-tier lifecycle arms: an agentd container restart
# KEEPS the state (the emptyDir outlives the container), and a pod
# recreate WIPES it (memory-backed class — a resumed workspace starts
# header-clean).

FIXPORT=3001
FIXKEY="svc-key-0062-e2e"
FIXPY='from http.server import BaseHTTPRequestHandler, HTTPServer
class H(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.headers.get("X-Service-Key") != "'"${FIXKEY}"'":
            self.send_response(401)
            self.end_headers()
            self.wfile.write(b"0062-e2e-reject")
            return
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"0062-e2e-ok")
    def log_message(self, *a):
        pass
HTTPServer(("127.0.0.1", '"${FIXPORT}"'), H).serve_forever()'

# mcp_headers_call <arguments-json> — tools/call dev_preview_headers
# through the pod's user mux (same port-forward dance + retry as
# mcp_call; the shared MCP_CALL_SEQ keeps every call on its own PF
# port).
mcp_headers_call() {
    MCP_CALL_SEQ=$((MCP_CALL_SEQ + 1))
    local pfport=$((TOOL_PF_PORT + MCP_CALL_SEQ)) pf _i _out=""
    kc -n "${NS}" port-forward "pod/${POD}" "${pfport}:4097" >/dev/null 2>&1 &
    pf=$!
    for _i in $(seq 1 15); do
        _out=$(curl -sfm 15 -X POST "http://127.0.0.1:${pfport}/v1/mcp" \
            -u "opencode:${WS_PW}" \
            -H "Content-Type: application/json" \
            -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"dev_preview_headers","arguments":'"$1"'}}' 2>/dev/null || true)
        [[ -n "${_out}" ]] && break
        sleep 4
    done
    kill "${pf}" 2>/dev/null || true
    wait "${pf}" 2>/dev/null || true
    printf '%s' "${_out}" | jq -r '.result.content[0].text // empty'
}

# start_fixture — (re)starts the rejecting fixture in-pod. The python
# source rides base64 (no quoting through kubectl exec); the files
# live under /tmp (PVC subPath tmp — they survive pod recreates, the
# PROCESS does not).
start_fixture() {
    local b64 code
    b64="$(printf '%s' "${FIXPY}" | base64 -w0)"
    kc exec "${POD}" -c "${RCTNR}" -- mkdir -p /tmp/dpv0062
    kc exec "${POD}" -c "${RCTNR}" -- sh -c "echo ${b64} | base64 -d > /tmp/dpv0062/fix.py"
    kc exec "${POD}" -c "${RCTNR}" -- sh -c 'pkill -f "python3 fix.py" 2>/dev/null || true; cd /tmp/dpv0062 && nohup python3 fix.py > server.log 2>&1 & sleep 1'
    code=$(kc exec "${POD}" -c "${RCTNR}" -- sh -c 'curl -sm 3 -o /dev/null -w "%{http_code}" http://127.0.0.1:'"${FIXPORT}"'/')
    [[ "${code}" == "401" ]] || die "fixture must reject pre-header requests (got ${code})"
}

preview_code() { # → HTTP status of one preview GET on the fixture port
    curl -sm 10 -o /dev/null -w '%{http_code}' "${AUTH[@]}" "${BASE}/${FIXPORT}/"
}

log "0062-A: fixture rejects the preview without the configured header"
start_fixture
code="$(preview_code)"
[[ "${code}" == "401" ]] || die "preview must hit the fixture's 401 pre-header, got ${code}"
ok "unhappy arm: fixture rejects the headerless preview (401)"

log "0062-B: set X-Service-Key via the tool → the preview renders"
out="$(mcp_headers_call '{"action":"set","name":"X-Service-Key","value":"'"${FIXKEY}"'"}')"
[[ "${out}" == *'"X-Service-Key"'* ]] || die "set must acknowledge the entry, got: ${out:0:200}"
code="$(preview_code)"
[[ "${code}" == "200" ]] || die "preview must render with the configured header, got ${code}"
body="$(curl -sm 10 "${AUTH[@]}" "${BASE}/${FIXPORT}/")"
[[ "${body}" == *"0062-e2e-ok"* ]] || die "fixture body must confirm the accepted request: ${body:0:200}"
ok "happy arm: tool set → preview renders (200, fixture marker)"

log "0062-C: clear → the fixture rejects again"
out="$(mcp_headers_call '{"action":"clear","name":"X-Service-Key"}')"
[[ "${out}" == *'"entries":0'* ]] || die "clear must report zero entries, got: ${out:0:200}"
code="$(preview_code)"
[[ "${code}" == "401" ]] || die "cleared header must stop reaching the fixture, got ${code}"
ok "clear arm: rejection restored (401)"

log "0062-D: agentd container restart KEEPS the state (emptyDir outlives the container)"
mcp_headers_call '{"action":"set","name":"X-Service-Key","value":"'"${FIXKEY}"'"}' >/dev/null
if kc get pod "${POD}" -o jsonpath='{.spec.containers[*].name}' | grep -qw agentd; then
    kc exec "${POD}" -c agentd -- kill 1 || die "could not signal the agentd container"
    sleep 5
    out="$(mcp_headers_call '{"action":"list"}')"
    [[ "${out}" == *'"X-Service-Key"'* ]] || die "state must survive an agentd container restart, got: ${out:0:200}"
    code="$(preview_code)"
    [[ "${code}" == "200" ]] || die "injection must resume after the agentd restart, got ${code}"
    ok "restart-keeps: list shows the entry and the preview renders (memory-backed emptyDir survives the container)"
else
    warn "no agentd sidecar container on this pod — the restart arm needs the sidecar topology (the nightly installs it)"
fi

log "0062-E: pod recreate WIPES the state (a resumed workspace starts header-clean)"
OLD_UID="$(pod_uid)"
kc delete pod "${POD}" --wait=false >/dev/null 2>&1 || true
sleep 5
wait_new_pod "${OLD_UID}" 300
sleep 10 # agentd boot (opencode spawn + secrets materialize)
WS_PW=$(kc get secret "workspace-pw-${WS}" -o jsonpath='{.data.password}' | base64 -d)
start_fixture
out="$(mcp_headers_call '{"action":"list"}')"
[[ "${out}" == "[]" ]] || die "pod death must wipe the memory-backed state, got: ${out:0:200}"
code="$(preview_code)"
[[ "${code}" == "401" ]] || die "a fresh pod must start header-clean (no injection), got ${code}"
ok "suspend/pod-death arm: state wiped, list empty, fixture rejects again"

# --- #1332 — the tool legs (unchanged below) -------------------------

log "#1332-A: no public origin configured → tool fails loud (never an .svc link)"
out="$(mcp_call "${WS_DEVPORT}")" || true
[[ "${out}" == Error:*LLMSAFESPACE_API_PUBLIC_URL* ]] \
  || die "expected the fail-loud error naming LLMSAFESPACE_API_PUBLIC_URL, got: ${out:0:300}"
[[ "${out}" != *".svc"* ]] || die "refusal leaked an svc URL: ${out:0:300}"
ok "tool refuses to emit a cluster-internal origin"

# wait_new_pod <old_pod_uid> <timeout_s> — after a pod delete, wait_phase
# is a NO-OP (the workspace stays Active through the recreate), and the
# controller recreates pods with a DETERMINISTIC name (spec-hash suffix):
# same name, NEW object. Wait on the pod UID changing and the new pod
# Running — a name-based wait never terminates, and port-forwarding the
# dying object yields a dead forward (observed as empty MCP output on
# the #1332-B leg while the endpoint answered by hand).
wait_new_pod() { # old_pod_uid timeout_s
    local old="$1" t="$2" i uid
    for i in $(seq 1 "$((t / 5))"); do
        uid="$(kc get pod "${POD}" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
        if [[ -n "${uid}" && "${uid}" != "${old}" ]]; then
            if kc get pod "${POD}" -o jsonpath='{.status.containerStatuses[0].state.running.startedAt}' 2>/dev/null | grep -q .; then
                return 0
            fi
        fi
        sleep 5
    done
    die "no recreated pod for ${WS} (waited ${t}s)"
}

pod_uid() { kc get pod "${POD}" -o jsonpath='{.metadata.uid}' 2>/dev/null || true; }

log "#1332-B: full wiring chain — patch controller --api-public-url, recreate pod"
kc patch deployment llmsafespaces-controller --type json \
  -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--api-public-url='"${PUBLIC_ORIGIN_E2E}"'"}]' >/dev/null
CONTROLLER_PATCHED=1
kc rollout status deployment/llmsafespaces-controller --timeout=180s >/dev/null
OLD_UID="$(pod_uid)"
kc delete pod "${POD}" --wait=false >/dev/null 2>&1 || true
sleep 5
wait_new_pod "${OLD_UID}" 300
sleep 10 # agentd boot (opencode spawn + secrets materialize)
WS_PW=$(kc get secret "workspace-pw-${WS}" -o jsonpath='{.data.password}' | base64 -d)
out="$(mcp_call "${WS_DEVPORT}")" || true
url="$(tool_url "${out}")"
[[ "${url}" == "${PUBLIC_ORIGIN_E2E}"/* ]] \
  || die "tool's link URL must start with the configured public origin, got: ${out:0:300}"
[[ "${out}" != *".svc"* ]] || die "svc origin leaked into tool output: ${out:0:300}"
[[ "${out}" == LSP_DEV_PREVIEW_V1* ]] || die "tool output lost its marker line: ${out:0:300}"
ok "tool emits ${url} end-to-end (flag → controller → pod env → tool)"

log "#1332-C: origin mode — patch --preview-origin-base-domain, recreate pod"
kc patch deployment llmsafespaces-controller --type json \
  -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--preview-origin-base-domain='"${PREVIEW_BASE_E2E}"'"}]' >/dev/null
kc rollout status deployment/llmsafespaces-controller --timeout=180s >/dev/null
OLD_UID="$(pod_uid)"
kc delete pod "${POD}" --wait=false >/dev/null 2>&1 || true
sleep 5
wait_new_pod "${OLD_UID}" 300
sleep 10
WS_PW=$(kc get secret "workspace-pw-${WS}" -o jsonpath='{.data.password}' | base64 -d)
out="$(mcp_call "${WS_DEVPORT}")" || true
url="$(tool_url "${out}")"
[[ "${out}" == LSP_DEV_PREVIEW_V1\ port=*origin="${PREVIEW_BASE_E2E}"* ]] \
  || die "origin mode must engage (marker must carry origin=${PREVIEW_BASE_E2E}), got: ${out:0:300}"
[[ "${url}" == "${PUBLIC_ORIGIN_E2E}/api/v1/workspaces/${WS}/dev-preview-bootstrap/${WS_DEVPORT}" ]] \
  || die "origin-mode bootstrap URL wrong, got: ${url}"
[[ "${out}" != *".svc"* ]] || die "svc origin leaked into tool output: ${out:0:300}"
ok "origin mode emits the bootstrap URL on the public origin"

# --- #1580: the DISABLED arm (r1's missing unhappy path) — the space
# turns dev preview OFF; the projected flag flips via a pod recreate;
# the tool must refuse LOUD (no marker, no URL) instead of minting.
log "#1580: disabled arm — patch devPreview false, recreate, expect the loud refusal"
kc patch workspace "${WS}" --type merge \
  -p '{"spec":{"networkAccess":{"devPreview":false}}}' >/dev/null
OLD_UID="$(pod_uid)"
kc delete pod "${POD}" --wait=false >/dev/null 2>&1 || true
sleep 5
wait_new_pod "${OLD_UID}" 300
sleep 10 # agentd boot
WS_PW=$(kc get secret "workspace-pw-${WS}" -o jsonpath='{.data.password}' | base64 -d)
out="$(mcp_call "${WS_DEVPORT}")" || true
[[ "${out}" == *"DISABLED"* ]] \
  || die "a disabled space must fail LOUD with the DISABLED reason, got: ${out:0:300}"
[[ "${out}" == *"Workspace Settings"* ]] \
  || die "the refusal must carry the recovery hint, got: ${out:0:300}"
[[ "${out}" != *LSP_DEV_PREVIEW_V1* ]] \
  || die "no URL marker may be minted for a disabled space: ${out:0:300}"
ok "disabled space: the tool fails loud (reason + hint, no marker minted)"

log "dev-preview tunnel e2e: ALL LEGS GREEN"
