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
WS="$(ws_id 90)"

# ---------------------------------------------------------------------------
# Setup: workspace with devPreview enabled + a relative-asset dev server.

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
RCTNR="$(runtime_container "${WS}")"
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

harness_start

BASE="http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WS}/dev-preview"
AUTH=(-H "Authorization: Bearer ${AUTH_TOKEN}")

# ---------------------------------------------------------------------------
# #1333 — the redirect legs.

log "#1333-A: bare port 308s to the slash form"
hdr=$(mktemp)
code=$(curl -sm 10 -o /dev/null -D "${hdr}" -w '%{http_code}' "${AUTH[@]}" "${BASE}/${WS_DEVPORT}")
[[ "${code}" == "308" ]] || die "expected 308 on bare /${WS_DEVPORT}, got ${code}"
loc=$(awk 'tolower($1)=="location:"{print $2}' "${hdr}" | tr -d '\r')
[[ "${loc}" == "${BASE}/${WS_DEVPORT}/" ]] || die "unexpected Location: ${loc}"
ok "308 → ${loc}"

log "#1333-A2: query preserved on the redirect"
code=$(curl -sm 10 -o /dev/null -D "${hdr}" -w '%{http_code}' "${AUTH[@]}" "${BASE}/${WS_DEVPORT}?v=3&x=1")
loc=$(awk 'tolower($1)=="location:"{print $2}' "${hdr}" | tr -d '\r')
[[ "${code}" == "308" && "${loc}" == "${BASE}/${WS_DEVPORT}/?v=3&x=1" ]] \
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

mcp_call() { # port → prints the text of content[0]
    kc -n "${NS}" port-forward "pod/${POD}" "${TOOL_PF_PORT}:4097" >/dev/null 2>&1 &
    local pf=$! _i _out=""
    for _i in $(seq 1 10); do
        curl -sfm 2 "http://127.0.0.1:${TOOL_PF_PORT}/" >/dev/null 2>&1 && break
        sleep 1
    done
    _out=$(curl -sfm 15 -X POST "http://127.0.0.1:${TOOL_PF_PORT}/v1/mcp" \
        -u "opencode:${WS_PW}" \
        -H "Content-Type: application/json" \
        -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dev_preview_url","arguments":{"port":'"$1"'}}}')
    kill "${pf}" 2>/dev/null || true
    printf '%s' "${_out}" | jq -r '.result.content[0].text // empty'
}

log "#1332-A: no public origin configured → tool fails loud (never an .svc link)"
out="$(mcp_call "${WS_DEVPORT}")" || true
[[ "${out}" == Error:*LLMSAFESPACE_API_PUBLIC_URL* ]] \
  || die "expected the fail-loud error naming LLMSAFESPACE_API_PUBLIC_URL, got: ${out:0:300}"
[[ "${out}" != *".svc"* ]] || die "refusal leaked an svc URL: ${out:0:300}"
ok "tool refuses to emit a cluster-internal origin"

log "#1332-B: full wiring chain — patch controller --api-public-url, recreate pod"
kc patch deployment llmsafespaces-controller --type json \
  -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--api-public-url='"${PUBLIC_ORIGIN_E2E}"'"}]' >/dev/null
kc rollout status deployment/llmsafespaces-controller --timeout=180s >/dev/null
kc delete pod "${POD}" --wait=false >/dev/null 2>&1 || true
sleep 5
wait_phase "${WS}" Active 300 || { diagnose_workspace "${WS}"; die "workspace never returned to Active after pod recreate"; }
POD="$(pod_of "${WS}")"
[[ -n "${POD}" ]] || die "no recreated pod for ${WS}"
WS_PW=$(kc get secret "workspace-pw-${WS}" -o jsonpath='{.data.password}' | base64 -d)
out="$(mcp_call "${WS_DEVPORT}")" || true
[[ "${out}" == "${PUBLIC_ORIGIN_E2E}"/* ]] \
  || die "tool output must start with the configured public origin, got: ${out:0:300}"
[[ "${out}" != *".svc"* ]] || die "svc origin leaked into tool output: ${out:0:300}"
ok "tool emits ${PUBLIC_ORIGIN_E2E}/… end-to-end (flag → controller → pod env → tool)"

log "restoring the controller (removing the e2e flag)"
kc patch deployment llmsafespaces-controller --type json \
  -p '[{"op":"remove","path":"/spec/template/spec/containers/0/args/-"}]' >/dev/null
kc rollout status deployment/llmsafespaces-controller --timeout=180s >/dev/null

kc delete workspace "${WS}" --ignore-not-found >/dev/null 2>&1 || true
log "dev-preview tunnel e2e: ALL LEGS GREEN"
