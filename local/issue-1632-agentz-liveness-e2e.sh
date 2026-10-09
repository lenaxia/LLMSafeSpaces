#!/usr/bin/env bash
# issue-1632-agentz-liveness-e2e.sh — #1632 fix #3 e2e: the agentz
# liveness wiring on a LIVE sidecar-mode workspace.
#
# STATUS: pending first harness-lane execution (authored r4; every path,
# name, and label below verified against source — router mounts
# /api/v1/workspaces (api/internal/server/router.go:484), lifecycle
# delete is DELETE /:id (:1345), the controller Secret is workspace-pw-*
# (constants.go passwordSecretName), the pod label domain is
# llmsafespaces.dev/workspace (constants.go LabelWorkspace), and the
# harness runtime contract is a seeded RuntimeEnvironment named
# python-3.11 (local/test.sh Test 3)).
#
# Rows:
#   R0  workspace reaches Active (sidecar-mode pod).
#   R1  the workspace container's kubelet livenessProbe is HTTPGet
#       /v1/agentz on the admin port WITH the bearer header, and the
#       kernel-accept-blind tcpSocket probe is gone (spec-level).
#   R2  /v1/agentz on the live pod answers 401 WITHOUT the bearer and
#       200 + ok:true WITH it (the F1.4.2 failure class: a mis-wired
#       probe 401s/404s forever and restart-loops the container).
#   R3  a healthy workspace carries ZERO workspace-container restarts
#       (the probe is not firing blind).
#   R4  (unhappy loop, opt-in via AGENTZ_AGENTZ_SUSTAIN_SECONDS + WEDGE=1):
#       with the env knob clamped low by the agentd floor, wedge the
#       agent's event loop (a busy synchronous child), let the episode
#       age past the sustain, and observe a workspace-container restart
#       followed by recovery. Skipped unless WEDGE=1 — the happy rows
#       are the always-on contract.
set -euo pipefail

CTX="${CTX:-kind-llmsafespaces}"
NS="${NS:-llmsafespaces}"
WS="${WS:-agentz-e2e-$$}"
API="${API:-http://localhost:8080}"
API_KEY="${API_KEY:?API_KEY must be set (harness lane contract)}"
RUNTIME="${RUNTIME:-python-3.11}"
RUNTIME_IMAGE_REF="${RUNTIME_IMAGE_REF:-llmsafespaces/runtime-base:dev}"
POLL="${POLL:-5}"
TIMEOUT="${TIMEOUT:-300}"
WEDGE="${WEDGE:-0}"

kc() { kubectl --context "${CTX}" -n "${NS}" "$@"; }

cleanup() {
  curl -sfm 10 -X DELETE -H "Authorization: Bearer ${API_KEY}" \
    "${API}/api/v1/workspaces/${WS}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "== R0: create sidecar-mode workspace ${WS} (runtime ${RUNTIME})"
cat <<EOF | kc apply -f - >/dev/null
apiVersion: llmsafespaces.dev/v1
kind: RuntimeEnvironment
metadata:
  name: ${RUNTIME}
spec:
  image: ${RUNTIME_IMAGE_REF}
  language: python
  version: "3.11"
EOF
curl -sfm 240 -X POST -H "Authorization: Bearer ${API_KEY}" -H 'Content-Type: application/json' \
  -d '{"name":"'"${WS}"'","runtime":"'"${RUNTIME}"'"}' \
  "${API}/api/v1/workspaces" >/dev/null

deadline=$((SECONDS + TIMEOUT)); POD=""
while [ ${SECONDS} -lt ${deadline} ]; do
  POD="$(kc get pods -l "llmsafespaces.dev/workspace=${WS}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
  if [ -n "${POD}" ]; then
    ready="$(kc get pod "${POD}" -o jsonpath='{.status.containerStatuses[0].ready}' 2>/dev/null || true)"
    [ "${ready}" = "true" ] && break
  fi
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
TOKEN="$(kc get secret "workspace-pw-${WS}" -o jsonpath='{.data.admin-token}' | base64 -d)"
noauth="$(kc exec "${POD}" -c workspace -- curl -s -o /dev/null -w '%{http_code}' -m 5 http://127.0.0.1:4098/v1/agentz)"
[ "${noauth}" = "401" ] || { echo "FAIL: agentz without bearer returned ${noauth}, want 401"; exit 1; }
body="$(kc exec "${POD}" -c workspace -- curl -s -m 5 -H "Authorization: Bearer ${TOKEN}" http://127.0.0.1:4098/v1/agentz)"
echo "${body}" | grep -q '"ok":true' || { echo "FAIL: authorized agentz not ok: ${body}"; exit 1; }
echo "R2 ok: 401 without bearer, ok:true with bearer (${body})"

echo "== R3: zero workspace-container restarts (healthy pod, probe silent)"
restarts="$(kc get pod "${POD}" -o jsonpath='{.status.containerStatuses[0].restartCount}')"
[ "${restarts}" = "0" ] || { echo "FAIL: workspace container restarted ${restarts}x on a healthy pod"; exit 1; }
echo "R3 ok: restartCount=0"

if [ "${WEDGE}" != "1" ]; then
  echo "PASS: agentz liveness wiring verified (happy rows; set WEDGE=1 for the sustain loop)"
  exit 0
fi

echo "== R4 (WEDGE=1): sustain-episode → workspace-container restart loop"
echo "(requires AGENTZ_AGENTZ_SUSTAIN_SECONDS set low — the agentd-side floor clamp"
echo " bounds how low; the pod spec must carry the env to the sidecar)"
# Deterministic agent wedge: SIGSTOP the opencode process — alive, TCP
# listener still kernel-answered, /global/health times out (the incident's
# wedged-but-listening shape, without depending on timing). The episode
# ages past the clamped sustain, agentz 503s, kubelet restarts the
# workspace container (which clears the stopped process — recovery).
before="$(kc get pod "${POD}" -o jsonpath='{.status.containerStatuses[0].restartCount}')"
kc exec "${POD}" -c workspace -- pkill -STOP -x opencode \
  || kc exec "${POD}" -c workspace -- pkill -STOP -f 'opencode serve'
deadline=$((SECONDS + TIMEOUT)); after=""
while [ ${SECONDS} -lt ${deadline} ]; do
  after="$(kc get pod "${POD}" -o jsonpath='{.status.containerStatuses[0].restartCount}')"
  [ -n "${after}" ] && [ "${after}" -gt "${before}" ] && break
  sleep "${POLL}"
done
[ -n "${after}" ] && [ "${after}" -gt "${before}" ] || { echo "FAIL: no workspace-container restart observed within ${TIMEOUT}s"; exit 1; }
echo "R4 ok: restartCount ${before} → ${after} (wedge → episode → restart)"
echo "PASS: agentz liveness wiring verified end to end (happy + sustain loop)"
