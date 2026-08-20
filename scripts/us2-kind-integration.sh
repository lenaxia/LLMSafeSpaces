#!/usr/bin/env bash
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# US-2 integration level L3 (design 0051) — kind-cluster execution of the
# K1–K8 checks specified in docs/testing/0051-us2-integration-test-plan.md.
#
# What this level uniquely proves (everything below needs a real kubelet):
#   K1 native-sidecar start ordering (credential-setup → agentd → main)
#   K2 #857 stamp-before-opencode-reads (sidecar startup gate)
#   K3 cross-uid file bridge (boot trio 0640 via shared gid 1000)
#   K4 in-pod control-socket round trip (Appendix A over real TCP)
#   K5 supervisor crash-recovery + restart-reason marker
#   K6 a stopped SIDECAR restarts only the sidecar (probe isolation)
#   K7 a stopped WORKSPACE container restarts without touching the sidecar
#   K8 pod termination drains both containers within the grace budget
#
# Topology: controller-only chart install (api/mcp/webhooks off — no
# postgres, no cert-manager). The reconciler needs none of those to build
# pods; the sidecar split is what is under test. All images come from a
# throwaway local registry wired into the kind nodes via certs.d (the
# canonical kind local-registry pattern), because the agentd sidecar
# REQUIRES a digest-pinned reference (controller startup validation) and
# digest pulls must resolve.
#
# NOT runnable on the authoring sandbox (no docker): executed by
# .github/workflows/us2-kind-integration.yml (workflow_dispatch + weekly)
# or any docker-capable host.
#
# Usage:
#   scripts/us2-kind-integration.sh [--keep]   # --keep: no teardown
# Exit codes: 0 all PASS · 1 setup failure · 2 one or more checks FAILED.

set -euo pipefail

TMPDIR="${TMPDIR:-/tmp}"
KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

CLUSTER_NAME="${CLUSTER_NAME:-us2-int}"
REG_NAME="${REG_NAME:-us2-int-registry}"
REG_PORT="${REG_PORT:-5001}"
NS="${NS:-llmsafespaces}"
RELEASE="${RELEASE:-lss}"
WORKSPACE_NAME="ws-us2-int"
# Node image pinned to match local/kind-cluster.yaml (chart floor 1.35).
NODE_IMAGE="kindest/node:v1.35.5@sha256:ce977ae6d65918d0b58a5f8b5e940429c2ce42fa3a5619ec2bbc60b949c0ac95"
REG="localhost:${REG_PORT}"
declare -a RESULTS=()

log()  { printf '[us2-int] %s\n' "$*"; }
pass() { RESULTS+=("PASS $1 — $2"); log "PASS $1 — $2"; }
fail() { RESULTS+=("FAIL $1 — $2"); log "FAIL $1 — $2"; }

finish() {
  local code=$?
  log "===== SUMMARY ====="
  local r fails=0
  for r in "${RESULTS[@]:-}"; do
    [ -z "$r" ] && continue
    printf '  %s\n' "$r"
    case "$r" in FAIL*) fails=$((fails+1));; esac
  done
  if [ "$code" -ne 0 ] && [ "$fails" -eq 0 ]; then
    log "SETUP FAILURE before checks ran (exit $code)"
  fi
  # Teardown ONLY on success (or --keep). On failure the cluster stays up
  # so the workflow's diagnostics step can inspect it — GitHub-hosted
  # runners are ephemeral, so a leaked cluster costs nothing.
  if [ "$KEEP" -ne 1 ] && [ "$fails" -eq 0 ] && [ "$code" -eq 0 ]; then
    kind delete cluster --name "$CLUSTER_NAME" >/dev/null 2>&1 || true
    docker rm -f "$REG_NAME" >/dev/null 2>&1 || true
  else
    log "cluster $CLUSTER_NAME + registry $REG_NAME left running for diagnostics (--keep to force on success)"
  fi
  [ "$fails" -eq 0 ] || exit 2
}
trap finish EXIT

# In-script diagnostics on setup failure (runs while the cluster is still
# up, unlike the workflow's post-hoc step): controller logs + pod describe.
# The controller Deployment's name is the chart fullname
# (${RELEASE}-llmsafespaces-controller) — the run-32417391466 iteration
# logged the WRONG name (lss-controller) and captured nothing.
setup_diagnostics() {
  log "setup failed — dumping diagnostics (cluster left running)"
  kubectl -n "$NS" get pods -o wide 2>&1 | sed 's/^/  /' || true
  kubectl -n "$NS" describe deploy -l app.kubernetes.io/name=llmsafespaces 2>&1 | tail -40 || true
  kubectl -n "$NS" logs "deploy/${RELEASE}-llmsafespaces-controller" --tail=60 2>&1 | sed 's/^/  /' || true
  kubectl -n "$NS" get events --sort-by=.lastTimestamp 2>&1 | tail -25 || true
}

# --- Preflight ---------------------------------------------------------------
for bin in docker kind helm kubectl go jq curl; do
  command -v "$bin" >/dev/null 2>&1 || { log "missing required binary: $bin"; exit 1; }
done

# --- Registry + cluster ------------------------------------------------------

log "starting local registry $REG_NAME (host port $REG_PORT)"
if [ "$(docker inspect -f '{{.State.Running}}' "$REG_NAME" 2>/dev/null || true)" != "true" ]; then
  docker run -d --restart=always -p "127.0.0.1:${REG_PORT}:5000" \
    --network bridge --name "$REG_NAME" registry:3 >/dev/null
fi
for i in $(seq 1 30); do
  curl -fs "http://127.0.0.1:${REG_PORT}/v2/" >/dev/null 2>&1 && break
  sleep 1
done
curl -fs "http://127.0.0.1:${REG_PORT}/v2/" >/dev/null || { log "registry did not become ready"; exit 1; }

log "creating kind cluster $CLUSTER_NAME (inline config; certsd path enabled)"
cat <<EOF | kind create cluster --config=-
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: ${CLUSTER_NAME}
nodes:
  - role: control-plane
    image: ${NODE_IMAGE}
containerdConfigPatches:
- |-
  [plugins."io.containerd.grpc.v1.cri".registry]
    config_path = "/etc/containerd/certs.d"
EOF

# Alias localhost:$REG_PORT inside the nodes to the registry container
# (localhost is network-namespace local — the canonical kind workaround).
for node in $(kind get nodes --name "$CLUSTER_NAME"); do
  docker exec "$node" mkdir -p "/etc/containerd/certs.d/localhost:${REG_PORT}"
  printf '[host."http://%s:5000"]\n' "$REG_NAME" \
    | docker exec -i "$node" cp /dev/stdin "/etc/containerd/certs.d/localhost:${REG_PORT}/hosts.toml"
done
if [ "$(docker inspect -f='{{json .NetworkSettings.Networks.kind}}' "$REG_NAME")" = "null" ]; then
  docker network connect kind "$REG_NAME"
fi

# --- Images ------------------------------------------------------------------

log "building and pushing images to $REG (agentd, controller, runtime-base)"
docker build --network host --build-arg VERSION=ci --build-arg COMMIT_SHA=us2int \
  -f cmd/workspace-agentd/Dockerfile -t "$REG/llmsafespaces/agentd:ci" .
docker build --network host --build-arg VERSION=ci --build-arg COMMIT_SHA=us2int \
  -f controller/Dockerfile -t "$REG/llmsafespaces/controller:ci" .
docker build --network host --build-arg VERSION=ci --build-arg COMMIT_SHA=us2int \
  -f runtimes/base/Dockerfile -t "$REG/llmsafespaces/runtime-base:ci" .
docker push "$REG/llmsafespaces/agentd:ci" >/dev/null
docker push "$REG/llmsafespaces/controller:ci" >/dev/null
docker push "$REG/llmsafespaces/runtime-base:ci" >/dev/null

# The sidecar requires a digest-pinned reference (controller validation);
# RepoDigests carries the manifest digest the push just registered.
AGENTD_REF=$(docker inspect --format '{{index .RepoDigests 0}}' "$REG/llmsafespaces/agentd:ci")
case "$AGENTD_REF" in
  *@sha256:*) AGENTD_REF="$REG/llmsafespaces/agentd:ci@${AGENTD_REF##*@}" ;;
  *) log "could not determine agentd manifest digest (RepoDigests empty)"; exit 1 ;;
esac
log "agentd reference: $AGENTD_REF"

# Binary sha256 pins (the break-glass path). REQUIRED here: with only the
# image set, the controller resolves pins at startup by querying the
# registry AT THE REFERENCE'S HOSTNAME from INSIDE ITS POD — where
# localhost:5001 does not resolve (the certs.d alias is node-level
# containerd-only) — and exits fatal. Explicit pins skip that lookup
# (controller/main.go: both-or-neither). The kind node is amd64, so the
# amd64 hash is what the entrypoint actually verifies; the arm64 field
# must be non-empty for validation and is inert on this cluster.
# >>> extract-binary-sha (sentinels: executed verbatim by
# scripts/us2_kind_script_test.go under a stubbed docker — keep the
# block self-contained: only CID/BINARY_SHA, $REG, $TMPDIR, log, and
# external commands may be referenced).
# Extract the binary for hashing. The agentd image is FROM scratch with
# NO entrypoint/cmd by design, so `docker create` needs an explicit
# command arg (never started — filesystem access only). Every step is
# explicitly guarded: under set -e an unguarded assignment would exit
# silently before its error message (run 32412056256's silent death).
CID=$(docker create "$REG/llmsafespaces/agentd:ci" no-start 2>/dev/null) \
  || { log "docker create failed for binary extraction"; exit 1; }
docker cp "$CID:/usr/local/bin/workspace-agentd" "$TMPDIR/workspace-agentd" >/dev/null 2>&1 \
  || { docker rm -f "$CID" >/dev/null 2>&1; log "docker cp failed to extract agentd binary"; exit 1; }
docker rm -f "$CID" >/dev/null 2>&1
BINARY_SHA=$(sha256sum "$TMPDIR/workspace-agentd" 2>/dev/null | cut -d' ' -f1) \
  || { log "sha256sum failed on extracted binary"; exit 1; }
[ -n "$BINARY_SHA" ] || { log "could not hash extracted binary"; exit 1; }
log "agentd binary sha256 (amd64): $BINARY_SHA"
# <<< extract-binary-sha

# --- Chart + workspace -------------------------------------------------------

# cert-manager first: the controller binary ALWAYS registers its admission
# webhooks (no flag gate — controller/main.go), so webhooks.enabled=false
# merely removes the cert volume and crash-loops the manager on cert load.
# Same manifest/version as e2e-nightly.
log "installing cert-manager (webhook certs are mandatory for the controller)"
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.0/cert-manager.yaml
kubectl -n cert-manager rollout status deployment/cert-manager --timeout=180s
kubectl -n cert-manager rollout status deployment/cert-manager-webhook --timeout=180s

# Controller-lean: api/mcp off, and NO migrations job — the migrate hook
# runs the API image against postgres, and the reconciler needs neither to
# build pods (the sidecar split is what is under test). Webhooks stay ON
# (see above). externalSecret stays ENABLED (create=true, dummy values):
# the controller Deployment resolves LLMSAFESPACES_INTERNAL_TOKEN from that
# Secret regardless of api.enabled.
log "installing controller-lean chart (api/mcp/migrations off; webhooks on)"
helm upgrade --install "$RELEASE" helm \
  -n "$NS" --create-namespace \
  --set api.enabled=false \
  --set mcp.enabled=false \
  --set migrations.enabled=false \
  --set rbac.scope=cluster \
  --set "webhooks.allowedImageRegistries[0]=$REG/llmsafespaces/" \
  --set externalSecret.create=true \
  --set "externalSecret.postgresPassword=us2int-pg" \
  --set "externalSecret.redisPassword=us2int-redis" \
  --set controller.image.repository="$REG/llmsafespaces/controller" \
  --set controller.image.tag=ci \
  --set controller.image.pullPolicy=IfNotPresent \
  --set "controller.agentdDelivery.image=${AGENTD_REF}" \
  --set "controller.agentdDelivery.binarySHA256Amd64=${BINARY_SHA}" \
  --set "controller.agentdDelivery.binarySHA256Arm64=${BINARY_SHA}" \
  --set controller.agentdSidecar.enabled=true \
  --wait --timeout 420s || { setup_diagnostics; exit 1; }

# Webhook bootstrap race: helm --wait covers the Deployment, NOT the
# ValidatingWebhookConfiguration's Service endpoints (and cert-manager's
# cert landing in the pod). Creating the workspace the instant helm
# returns gets "connection refused" from the webhook. Wait for the
# Service to have endpoints, then retry the apply through the window.
WEBHOOK_SVC="${RELEASE}-llmsafespaces-controller-webhook"
log "waiting for webhook endpoints ($WEBHOOK_SVC)"
EPS=""
for i in $(seq 1 60); do
  EPS=$(kubectl -n "$NS" get endpoints "$WEBHOOK_SVC" \
    -o jsonpath='{.subsets[0].addresses[0].ip}' 2>/dev/null || true)
  [ -n "$EPS" ] && break
  sleep 2
done
[ -n "$EPS" ] || { log "webhook service never got endpoints"; setup_diagnostics; exit 1; }

log "creating workspace $WORKSPACE_NAME (retrying through the webhook cert window)"
WS_MANIFEST=$(mktemp)
cat >"$WS_MANIFEST" <<EOF
apiVersion: llmsafespaces.dev/v1
kind: Workspace
metadata:
  name: $WORKSPACE_NAME
spec:
  owner:
    userID: us2-int
  runtime: $REG/llmsafespaces/runtime-base:ci
  storage:
    size: 1Gi
EOF
WS_APPLIED=0
for i in $(seq 1 15); do
  if kubectl -n "$NS" apply -f "$WS_MANIFEST"; then WS_APPLIED=1; break; fi
  log "workspace apply failed (attempt $i) — retrying"
  sleep 4
done
rm -f "$WS_MANIFEST"
[ "$WS_APPLIED" = "1" ] || { log "workspace apply kept failing (webhook unreachable?)"; setup_diagnostics; exit 1; }

POD=""
for i in $(seq 1 60); do
  POD=$(kubectl -n "$NS" get pods -l llmsafespaces.dev/workspace="$WORKSPACE_NAME" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  [ -n "$POD" ] && break
  sleep 2
done
[ -n "$POD" ] || { log "workspace pod never appeared"; kubectl -n "$NS" get events --sort-by=.lastTimestamp | tail -20; exit 1; }
log "pod: $POD — waiting Ready (opencode cold boot; startup budget 3min)"
kubectl -n "$NS" wait --for=condition=Ready "pod/$POD" --timeout=300s \
  || { kubectl -n "$NS" describe pod "$POD"; exit 1; }

jq_field() { kubectl -n "$NS" get pod "$POD" -o json | jq -r "$1"; }
socket_json() { # $1: id, $2: method — one Appendix-A round trip from inside the pod
  kubectl -n "$NS" exec "$POD" -c workspace -- bash -c \
    "exec 3<>/dev/tcp/127.0.0.1/4099; printf '{\"v\":1,\"id\":$1,\"method\":\"$2\",\"params\":{}}' >&3; head -c 400 <&3" 2>/dev/null \
    || echo ""
}

# --- K1: native-sidecar start ordering ---------------------------------------
TERM_CRED=$(jq_field '.status.initContainerStatuses[]? | select(.name=="credential-setup") | .state.terminated.finishedAt // .lastState.terminated.finishedAt // empty')
START_SIDECAR=$(jq_field '.status.initContainerStatuses[]? | select(.name=="agentd") | .state.running.startedAt // .state.terminated.startedAt // empty')
START_MAIN=$(jq_field '.status.containerStatuses[0].state.running.startedAt // empty')
if [ -n "$TERM_CRED" ] && [ -n "$START_SIDECAR" ] && [ -n "$START_MAIN" ] \
  && [ "$TERM_CRED" \< "$START_SIDECAR" ] && [ "$START_SIDECAR" \< "$START_MAIN" ]; then
  pass K1 "credential-setup@$TERM_CRED < sidecar@$START_SIDECAR < main@$START_MAIN"
else
  fail K1 "ordering: cred=$TERM_CRED sidecar=$START_SIDECAR main=$START_MAIN"
fi

# --- K2: #857 stamp present in the config opencode first read ----------------
MCP_ENTRY=$(kubectl -n "$NS" exec "$POD" -c workspace -- \
  grep -c 'llmsafespaces' /sandbox-runtime/agent-config.json 2>/dev/null || echo 0)
if [ "$MCP_ENTRY" -ge 1 ]; then
  pass K2 "agent-config.json carries the llmsafespaces MCP entry at first boot"
else
  fail K2 "no llmsafespaces entry in agent-config.json"
fi

# --- K3: cross-uid file modes -------------------------------------------------
MODE_CFG=$(kubectl -n "$NS" exec "$POD" -c workspace -- stat -c %a /sandbox-runtime/agent-config.json 2>/dev/null || echo "?")
MODE_PROMPT=$(kubectl -n "$NS" exec "$POD" -c workspace -- stat -c %a /sandbox-runtime/admin-prompt.md 2>/dev/null || echo "absent")
MODE_PW=$(kubectl -n "$NS" exec "$POD" -c workspace -- stat -c %a /sandbox-cfg/password 2>/dev/null || echo "?")
if [ "$MODE_CFG" = "640" ] && { [ "$MODE_PROMPT" = "640" ] || [ "$MODE_PROMPT" = "absent" ]; } && [ "$MODE_PW" = "600" ]; then
  pass K3 "agent-config=$MODE_CFG admin-prompt=$MODE_PROMPT password=$MODE_PW"
else
  fail K3 "agent-config=$MODE_CFG admin-prompt=$MODE_PROMPT password=$MODE_PW (want 640/{640,absent}/600)"
fi

# --- K4: in-pod control-socket round trip ------------------------------------
HELLO=$(socket_json 7 hello)
if printf '%s' "$HELLO" | grep -q '"supervisor":"supervise-opencode"'; then
  pass K4 "hello over 127.0.0.1:4099 answered: $(printf '%s' "$HELLO" | head -c 80)"
else
  fail K4 "socket hello got: $HELLO"
fi

# --- K5: child crash → respawn + marker --------------------------------------
OLD_CHILD=$(printf '%s' "$(socket_json 8 status)" | jq -r '.result.child_pid // 0' 2>/dev/null || echo 0)
if [ "${OLD_CHILD:-0}" -gt 0 ] 2>/dev/null; then
  kubectl -n "$NS" exec "$POD" -c workspace -- sh -c "kill -9 $OLD_CHILD" >/dev/null 2>&1 || true
  NEW_CHILD=0
  for i in $(seq 1 45); do
    NEW_CHILD=$(printf '%s' "$(socket_json 9 status)" | jq -r '.result.child_pid // 0' 2>/dev/null || echo 0)
    [ "$NEW_CHILD" -gt 0 ] 2>/dev/null && [ "$NEW_CHILD" != "$OLD_CHILD" ] && break
    sleep 2
  done
  MARKER=$(kubectl -n "$NS" exec "$POD" -c workspace -- \
    cat /sandbox-runtime/last-restart-reason.json 2>/dev/null || echo "")
  # The supervisor classifies ANY SIGKILL as potential-OOM (isOOMExit: SIGKILL
  # is what the OOM killer sends — externally-killed and OOM-killed are
  # indistinguishable BY DESIGN). Our kill -9 therefore yields "oom", not
  # "crash"; either marker proves the crash path recorded the restart.
  MARKER_OK=$(printf '%s' "$MARKER" | jq -r '.reason // empty' 2>/dev/null)
  if [ "$NEW_CHILD" -gt 0 ] 2>/dev/null && [ "$NEW_CHILD" != "$OLD_CHILD" ] \
    && { [ "$MARKER_OK" = "crash" ] || [ "$MARKER_OK" = "oom" ]; }; then
    pass K5 "child $OLD_CHILD → $NEW_CHILD respawned; crash marker recorded"
  else
    fail K5 "child $OLD_CHILD → ${NEW_CHILD:-?}; marker: $(printf '%s' "$MARKER" | head -c 80)"
  fi
else
  fail K5 "could not read child pid over the socket: $(socket_json 8 status)"
fi

# --- K6/K7: container-level restart isolation (crictl via the kind node) -----
NODE_CONTAINER=$(docker ps --filter "name=${CLUSTER_NAME}-control-plane" --format '{{.ID}}' | head -1)
[ -n "$NODE_CONTAINER" ] || log "K6/K7: no docker container matched ${CLUSTER_NAME}-control-plane"
CRICTL_BIN=""
if [ -n "$NODE_CONTAINER" ]; then
  CRICTL_BIN=$(docker exec "$NODE_CONTAINER" sh -c \
    'command -v crictl 2>/dev/null || { [ -x /usr/local/bin/crictl ] && echo /usr/local/bin/crictl; }' \
    2>/dev/null | head -1)
  [ -n "$CRICTL_BIN" ] || log "K6/K7: crictl not found on node ${NODE_CONTAINER}"
fi
crictl_cid() { # $1: pod uid, $2: container name
  docker exec "$NODE_CONTAINER" "$CRICTL_BIN" ps -o json 2>/dev/null \
    | jq -r --arg pod "$1" --arg name "$2" \
      '.containers[]? | select(.podSandboxId == $pod and .metadata.name == $name) | .id' \
    | head -1
}
if [ -n "$NODE_CONTAINER" ] && [ -n "$CRICTL_BIN" ]; then
  POD_UID=$(jq_field '.metadata.uid')

  # K6: stop the SIDECAR → only it restarts; opencode keeps serving.
  SC_BEFORE=$(jq_field '.status.initContainerStatuses[]? | select(.name=="agentd") | .restartCount // 0')
  MAIN_BEFORE=$(jq_field '.status.containerStatuses[0].restartCount')
  SC_CID=$(crictl_cid "$POD_UID" agentd)
  docker exec "$NODE_CONTAINER" crictl stop --timeout 5 "$SC_CID" >/dev/null 2>&1 || true
  sleep 30
  SC_AFTER=$(jq_field '.status.initContainerStatuses[]? | select(.name=="agentd") | .restartCount // 0')
  MAIN_AFTER=$(jq_field '.status.containerStatuses[0].restartCount')
  OC_UP=$(kubectl -n "$NS" exec "$POD" -c workspace -- bash -c \
    'exec 3<>/dev/tcp/127.0.0.1/4096 && echo up' 2>/dev/null || echo down)
  if [ "$SC_AFTER" -gt "$SC_BEFORE" ] && [ "$MAIN_AFTER" = "$MAIN_BEFORE" ] && [ "$OC_UP" = "up" ]; then
    pass K6 "sidecar restarts ${SC_BEFORE}→${SC_AFTER}; main ${MAIN_BEFORE}→${MAIN_AFTER}; :4096 $OC_UP"
  else
    fail K6 "sidecar ${SC_BEFORE}→${SC_AFTER}; main ${MAIN_BEFORE}→${MAIN_AFTER}; :4096 $OC_UP"
  fi

  # K7: stop the WORKSPACE container → it restarts; the sidecar does NOT.
  SC_BEFORE=$(jq_field '.status.initContainerStatuses[]? | select(.name=="agentd") | .restartCount // 0')
  MAIN_BEFORE=$(jq_field '.status.containerStatuses[0].restartCount')
  MAIN_CID=$(crictl_cid "$POD_UID" workspace)
  docker exec "$NODE_CONTAINER" "$CRICTL_BIN" stop --timeout 5 "$MAIN_CID" >/dev/null 2>&1 || true
  MAIN_AFTER=$MAIN_BEFORE
  for i in $(seq 1 60); do
    MAIN_AFTER=$(jq_field '.status.containerStatuses[0].restartCount')
    [ "$MAIN_AFTER" -gt "$MAIN_BEFORE" ] && break
    sleep 2
  done
  kubectl -n "$NS" wait --for=condition=Ready "pod/$POD" --timeout=300s >/dev/null 2>&1 || true
  SC_AFTER=$(jq_field '.status.initContainerStatuses[]? | select(.name=="agentd") | .restartCount // 0')
  if [ "$MAIN_AFTER" -gt "$MAIN_BEFORE" ] && [ "$SC_AFTER" = "$SC_BEFORE" ]; then
    pass K7 "main restarts ${MAIN_BEFORE}→${MAIN_AFTER}; sidecar ${SC_BEFORE}→${SC_AFTER} (survives)"
  else
    fail K7 "main ${MAIN_BEFORE}→${MAIN_AFTER}; sidecar ${SC_BEFORE}→${SC_AFTER}"
  fi
else
  log "SKIP K6/K7 (no docker exec into kind node / crictl unavailable)"
  RESULTS+=("SKIP K6 — crictl unavailable on node" "SKIP K7 — crictl unavailable on node")
fi

# --- K8: termination drains within the grace budget ---------------------------
START=$(date +%s)
kubectl -n "$NS" delete "pod/$POD" --wait=true >/dev/null 2>&1
ELAPSED=$(( $(date +%s) - START ))
if [ "$ELAPSED" -le 30 ]; then
  pass K8 "pod delete completed in ${ELAPSED}s (terminationGracePeriod 5s + API overhead)"
else
  fail K8 "pod delete took ${ELAPSED}s"
fi

log "all checks executed"
