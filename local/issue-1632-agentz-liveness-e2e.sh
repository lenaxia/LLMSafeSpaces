#!/usr/bin/env bash
# issue-1632-agentz-liveness-e2e.sh — #1632 fix #3 happy-path e2e:
# the agentz liveness wiring on a LIVE sidecar-mode workspace.
#
# Rows covered (the feasible-today set the review asked for):
#   1. The workspace container's kubelet livenessProbe is HTTPGet
#      /v1/agentz on the admin port WITH the bearer header, and the
#      kernel-accept-blind tcpSocket probe is gone (spec-level).
#   2. /v1/agentz on the live pod answers 401 WITHOUT the bearer and
#      200 + ok:true WITH it (the F1.4.2 failure class — a mis-wired
#      probe 401s/404s forever and restart-loops the container).
#   3. A healthy workspace carries ZERO workspace-container restarts
#      over the observation window (the probe is not firing blind).
#
# NOT covered here (needs an env-tunable sustain bound or a nightly
# home — 10m sustained-unhealthy episodes are not kind-lane material):
#   - the full wedge→episode→container-restart loop. Unit/integration
#     pins for that path live in cmd/workspace-agentd/agentz_test.go.
set -euo pipefail

CTX="${CTX:-kind-llmsafespaces}"
NS="${NS:-llmsafespaces}"
WS="${WS:-agentz-e2e-$$}"
API="${API:-http://localhost:8080}"
API_KEY="${API_KEY:?API_KEY must be set (harness lane contract)}"
POLL="${POLL:-5}"
TIMEOUT="${TIMEOUT:-300}"

kc() { kubectl --context "${CTX}" -n "${NS}" "$@"; }

cleanup() {
  curl -sfm 10 -X POST -H "Authorization: Bearer ${API_KEY}" \
    "${API}/workspaces/${WS}/terminate" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "== R0: create sidecar-mode workspace ${WS}"
curl -sfm 240 -X POST -H "Authorization: Bearer ${API_KEY}" -H 'Content-Type: application/json' \
  -d '{"name":"'"${WS}"'","runtime":"standard"}' "${API}/workspaces" >/dev/null

deadline=$((SECONDS + TIMEOUT))
POD=""
while [ $((SECONDS < deadline)) ]; do
  POD="$(kc get pods -l "llmsafespaces.io/workspace=${WS}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
  [ -n "${POD}" ] && kc get pod "${POD}" -o jsonpath='{.status.containerStatuses[0].ready}' 2>/dev/null | grep -q true && break
  sleep "${POLL}"
done
[ -n "${POD}" ] || { echo "FAIL: pod never became ready"; exit 1; }
echo "pod: ${POD}"

echo "== R1: workspace-container livenessProbe shape (spec)"
probe_path="$(kc get pod "${POD}" -o jsonpath='{.spec.containers[0].livenessProbe.httpGet.path}')"
probe_port="$(kc get pod "${POD}" -o jsonpath='{.spec.containers[0].livenessProbe.httpGet.port}')"
probe_tcp="$(kc get pod "${POD}" -o jsonpath='{.spec.containers[0].livenessProbe.tcpSocket}')"
[ "${probe_path}" = "/v1/agentz" ] || { echo "FAIL: liveness path is '${probe_path}', want /v1/agentz"; exit 1; }
[ "${probe_port}" = "4098" ] || { echo "FAIL: liveness port is '${probe_port}', want 4098 (admin)"; exit 1; }
[ -z "${probe_tcp}" ] || { echo "FAIL: tcpSocket liveness must be gone (kernel-accept blind)"; exit 1; }
hdr_count="$(kc get pod "${POD}" -o jsonpath='{range .spec.containers[0].livenessProbe.httpGet.httpHeaders[*]}{.name}{"\n"}{end}' | grep -c '^Authorization$' || true)"
[ "${hdr_count}" -ge 1 ] || { echo "FAIL: liveness probe must carry the Authorization bearer header"; exit 1; }
echo "R1 ok: HTTPGet /v1/agentz:4098 + bearer, tcpSocket absent"

echo "== R2: agentz auth on the live pod (shared netns via workspace container)"
TOKEN="$(kc get secret "workspace-${WS}" -o jsonpath='{.data.admin-token}' | base64 -d)"
noauth="$(kc exec "${POD}" -c workspace -- curl -s -o /dev/null -w '%{http_code}' -m 5 http://127.0.0.1:4098/v1/agentz)"
[ "${noauth}" = "401" ] || { echo "FAIL: agentz without bearer returned ${noauth}, want 401"; exit 1; }
body="$(kc exec "${POD}" -c workspace -- curl -s -m 5 -H "Authorization: Bearer ${TOKEN}" http://127.0.0.1:4098/v1/agentz)"
echo "${body}" | grep -q '"ok":true' || { echo "FAIL: authorized agentz not ok: ${body}"; exit 1; }
echo "R2 ok: 401 without bearer, ok:true with bearer (${body})"

echo "== R3: zero workspace-container restarts (healthy pod, probe silent)"
restarts="$(kc get pod "${POD}" -o jsonpath='{.status.containerStatuses[0].restartCount}')"
[ "${restarts}" = "0" ] || { echo "FAIL: workspace container restarted ${restarts}x on a healthy pod"; exit 1; }
echo "R3 ok: restartCount=0"

echo "PASS: agentz liveness wiring verified end to end"
