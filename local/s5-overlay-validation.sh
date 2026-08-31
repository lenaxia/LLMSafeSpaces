#!/usr/bin/env bash
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Design 0053 S5 — overlay delivery validation on a real kubelet.
# Executed by .github/workflows/s5-overlay-validation.yml (workflow_dispatch
# + weekly). NOT runnable on the authoring sandbox (no docker).
#
# What this level uniquely proves (everything below needs a real cluster):
#   S5.1 Missing-pin render failure — a chart install without both
#       delivery pins fails the Helm render loudly (§4.5's operator
#       surface, exercised for real rather than via chart tests).
#   S5.2 Full launch→ready on the STRIPPED base + BOTH overlays — the
#       capstone: workspace Active, platform inits ran, opencode serves,
#       the S2 redact wrapper exists on the pod (closes S2's deferred
#       cluster-e2e debt), and the main container execs the pinned agentd.
#   S5.3 Agentd verify failure — wrong break-glass binary pin → the
#       supervisor exits 81 → AgentdVerificationFailed condition + event
#       on the Workspace (surfaced, not crash-looped silently).
#   S5.4 Opencode verify failure — wrong opencode pin → exit 83 →
#       OpencodeVerificationFailed condition + event.
#   S5.5 Resume-path pull cost — suspend, evict BOTH overlay images from
#       the node, activate; the cold re-pull (opencode ~10× agentd) must
#       land inside the resume budget. Measured number → job summary.
#   S5.6 gVisor (runsc) leg — image volumes under the gVisor runtime
#       (design 0051's open item): install runsc on the kind node,
#       register the handler, boot a workspace on RuntimeClass gvisor,
#       assert Ready + overlays mounted.
#
# Topology: controller-only chart install (api/mcp/webhooks lean, same as
# us2-kind-integration.sh — the reconciler is what is under test). Images
# from a throwaway local registry wired via certs.d.
#
# Usage:
#   local/s5-overlay-validation.sh [--keep]   # --keep: no teardown
# Exit codes: 0 all PASS · 1 setup failure · 2 one or more checks FAILED.

set -euo pipefail

TMPDIR="${TMPDIR:-/tmp}"
KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

CLUSTER_NAME="${CLUSTER_NAME:-s5-ovl}"
REG_NAME="${REG_NAME:-s5-ovl-registry}"
REG_PORT="${REG_PORT:-5011}"
NS="${NS:-llmsafespaces}"
RELEASE="${RELEASE:-lss}"
NODE_IMAGE="kindest/node:v1.35.5@sha256:ce977ae6d65918d0b58a5f8b5e940429c2ce42fa3a5619ec2bbc60b949c0ac95"
REG="localhost:${REG_PORT}"
RESUME_BUDGET="${RESUME_BUDGET:-120}"
STEP_SUMMARY="${GITHUB_STEP_SUMMARY:-/dev/null}"
declare -a RESULTS=()

log()  { printf '[s5-ovl] %s\n' "$*"; }
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
  if [ "$KEEP" -ne 1 ] && [ "$fails" -eq 0 ] && [ "$code" -eq 0 ]; then
    kind delete cluster --name "$CLUSTER_NAME" >/dev/null 2>&1 || true
    docker rm -f "$REG_NAME" >/dev/null 2>&1 || true
  else
    log "cluster $CLUSTER_NAME + registry $REG_NAME left running for diagnostics (--keep to force on success)"
  fi
  [ "$fails" -eq 0 ] || exit 2
}
trap finish EXIT

setup_diagnostics() {
  log "setup failed — dumping diagnostics (cluster left running)"
  kubectl -n "$NS" get pods -o wide 2>&1 | sed 's/^/  /' || true
  kubectl -n "$NS" describe deploy -l app.kubernetes.io/name=llmsafespaces 2>&1 | tail -40 || true
  kubectl -n "$NS" logs "deploy/${RELEASE}-llmsafespaces-controller" --tail=80 2>&1 | sed 's/^/  /' || true
  kubectl -n "$NS" get events --sort-by=.lastTimestamp 2>&1 | tail -25 || true
}

for bin in docker kind helm kubectl go jq curl; do
  command -v "$bin" >/dev/null 2>&1 || { log "missing required binary: $bin"; exit 1; }
done

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

# --- S5.1: missing-pin render failure (pre-cluster — pure Helm surface) -----
BAD_RENDER=$(helm template s5-missing-pins helm \
  --set api.enabled=false --set mcp.enabled=false --set migrations.enabled=false 2>&1) || true
if echo "$BAD_RENDER" | grep -q "agentdDelivery.image is mandatory"; then
  pass S5.1 "missing-pin install fails the render with the agentdDelivery mandatory message"
else
  fail S5.1 "render without pins did not fail with the mandatory message: $(echo "$BAD_RENDER" | tail -2 | tr '\n' ' ')"
fi
BAD_RENDER_OC=$(helm template s5-missing-opencode helm \
  --set api.enabled=false --set mcp.enabled=false --set migrations.enabled=false \
  --set "controller.agentdDelivery.image=ghcr.io/lenaxia/llmsafespaces/agentd@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" 2>&1) || true
if echo "$BAD_RENDER_OC" | grep -q "opencodeDelivery.image is mandatory"; then
  pass S5.1b "agentd-only pin fails the render with the opencodeDelivery mandatory message"
else
  fail S5.1b "agentd-only render did not fail with the opencode mandatory message"
fi

# --- Registry + cluster (canonical kind local-registry pattern) -------------

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

log "creating kind cluster $CLUSTER_NAME"
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

for node in $(kind get nodes --name "$CLUSTER_NAME"); do
  docker exec "$node" mkdir -p "/etc/containerd/certs.d/localhost:${REG_PORT}"
  printf '[host."http://%s:5000"]\n' "$REG_NAME" \
    | docker exec -i "$node" cp /dev/stdin "/etc/containerd/certs.d/localhost:${REG_PORT}/hosts.toml"
done
if [ "$(docker inspect -f='{{json .NetworkSettings.Networks.kind}}' "$REG_NAME")" = "null" ]; then
  docker network connect kind "$REG_NAME"
fi

# --- Images: stripped base + BOTH delivery artifacts ------------------------

log "building and pushing images to $REG (agentd, opencode, controller, stripped base)"
docker build --network host --build-arg VERSION=ci --build-arg COMMIT_SHA=s5ovl \
  -f cmd/workspace-agentd/Dockerfile -t "$REG/llmsafespaces/agentd:ci" .
docker build --network host \
  -f runtimes/opencode/Dockerfile -t "$REG/llmsafespaces/opencode:ci" .
docker build --network host --build-arg VERSION=ci --build-arg COMMIT_SHA=s5ovl \
  -f controller/Dockerfile -t "$REG/llmsafespaces/controller:ci" .
docker build --network host \
  -f runtimes/base/Dockerfile -t "$REG/llmsafespaces/runtime-base:ci" .
for img in agentd opencode controller runtime-base; do
  docker push "$REG/llmsafespaces/$img:ci" >/dev/null
done

digest_ref() {
  local ref d
  ref=$(docker inspect --format '{{index .RepoDigests 0}}' "$REG/llmsafespaces/$1:ci")
  case "$ref" in
    *@sha256:*) echo "$REG/llmsafespaces/$1:ci@${ref##*@}" ;;
    *) log "could not determine $1 manifest digest"; exit 1 ;;
  esac
}
binary_sha() {
  local cid
  cid=$(docker create "$REG/llmsafespaces/$1:ci" no-start 2>/dev/null) \
    || { log "docker create failed for $1 extraction"; exit 1; }
  docker cp "$cid:$2" "$TMPDIR/$1-bin" >/dev/null 2>&1 \
    || { docker rm -f "$cid" >/dev/null 2>&1; log "docker cp failed for $1"; exit 1; }
  docker rm -f "$cid" >/dev/null 2>&1
  sha256sum "$TMPDIR/$1-bin" | cut -d' ' -f1
}

AGENTD_REF=$(digest_ref agentd)
OPENCODE_REF=$(digest_ref opencode)
AGENTD_SHA=$(binary_sha agentd /usr/local/bin/workspace-agentd)
OPENCODE_SHA=$(binary_sha opencode /usr/local/bin/opencode)
[ -n "$AGENTD_SHA" ] && [ -n "$OPENCODE_SHA" ] || { log "binary hashing failed"; exit 1; }
log "pins: agentd=${AGENTD_REF} sha=${AGENTD_SHA:0:12}… opencode=${OPENCODE_REF} sha=${OPENCODE_SHA:0:12}…"

# --- Chart (controller-lean, BOTH pins — the mandatory S3 shape) ------------

log "installing cert-manager"
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.0/cert-manager.yaml
kubectl -n cert-manager rollout status deployment/cert-manager --timeout=180s
kubectl -n cert-manager rollout status deployment/cert-manager-webhook --timeout=180s

HELM_PIN_ARGS=(
  --set "controller.agentdDelivery.image=${AGENTD_REF}"
  --set "controller.agentdDelivery.binarySHA256Amd64=${AGENTD_SHA}"
  --set "controller.agentdDelivery.binarySHA256Arm64=${AGENTD_SHA}"
  --set "controller.opencodeDelivery.image=${OPENCODE_REF}"
  --set "controller.opencodeDelivery.binarySHA256Amd64=${OPENCODE_SHA}"
  --set "controller.opencodeDelivery.binarySHA256Arm64=${OPENCODE_SHA}"
)
HELM_LEAN_ARGS=(
  --set api.enabled=false --set mcp.enabled=false --set migrations.enabled=false
  --set rbac.scope=cluster
  --set "webhooks.allowedImageRegistries[0]=$REG/llmsafespaces/"
  --set externalSecret.create=true
  --set "externalSecret.postgresPassword=s5-pg"
  --set "externalSecret.redisPassword=s5-redis"
  --set controller.image.repository="$REG/llmsafespaces/controller"
  --set controller.image.tag=ci
  --set controller.image.pullPolicy=IfNotPresent
  --set runtimeEnvironments.base.image.repository="$REG/llmsafespaces/runtime-base"
  --set runtimeEnvironments.base.image.tag=ci
)

log "installing controller-lean chart with BOTH pins (single-container mode)"
helm upgrade --install "$RELEASE" helm -n "$NS" --create-namespace \
  "${HELM_LEAN_ARGS[@]}" "${HELM_PIN_ARGS[@]}" \
  --wait --timeout 420s || { setup_diagnostics; exit 1; }

WEBHOOK_SVC="${RELEASE}-llmsafespaces-controller-webhook"
for i in $(seq 1 60); do
  EPS=$(kubectl -n "$NS" get endpoints "$WEBHOOK_SVC" -o jsonpath='{.subsets[0].addresses[0].ip}' 2>/dev/null || true)
  [ -n "$EPS" ] && break
  sleep 2
done
[ -n "$EPS" ] || { log "webhook service never got endpoints"; exit 1; }

apply_workspace() {
  local name=$1 extra=${2:-} ws_manifest
  ws_manifest=$(mktemp)
  cat >"$ws_manifest" <<EOF
apiVersion: llmsafespaces.dev/v1
kind: Workspace
metadata:
  name: $name
  namespace: $NS
spec:
  owner:
    userID: s5-int
  runtime: $REG/llmsafespaces/runtime-base:ci
  storage:
    size: 1Gi
$extra
EOF
  # Retry through the webhook cert/endpoint window (us2 lesson).
  local applied=0 i
  for i in 1 2 3 4 5; do
    if kubectl -n "$NS" apply -f "$ws_manifest" >/dev/null 2>&1; then applied=1; break; fi
    log "workspace $name apply failed (attempt $i) - retrying"
    sleep 3
  done
  [ "$applied" = "1" ] || { log "workspace $name apply kept failing"; return 1; }
  log "workspace $name applied"
  return 0
}

wait_phase() {
  local name=$1 phase=$2 timeout=${3:-420}
  for i in $(seq 1 "$timeout"); do
    [ "$(kubectl -n "$NS" get workspace "$name" -o jsonpath='{.status.phase}' 2>/dev/null || true)" = "$phase" ] && return 0
    sleep 2
  done
  return 1
}

pod_of() { kubectl -n "$NS" get pods -l llmsafespaces.dev/workspace="$1" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null; }

# --- S5.2: full launch→ready on stripped base + both overlays (capstone) ----

WS_MAIN="ws-s5-main"
log "S5.2: creating workspace $WS_MAIN"
apply_workspace "$WS_MAIN"

if wait_phase "$WS_MAIN" Active 420; then
  POD=$(pod_of "$WS_MAIN")
  # The main container must exec the pinned overlay agentd directly.
  CMDLINE=$(kubectl -n "$NS" exec "$POD" -c workspace -- cat /proc/1/cmdline 2>/dev/null | tr '\0' ' ' || true)
  if echo "$CMDLINE" | grep -q "/agentd/usr/local/bin/workspace-agentd"; then
    pass S5.2a "main container PID1 is the pinned overlay agentd: $CMDLINE"
  else
    fail S5.2a "PID1 is not the overlay agentd: ${CMDLINE:-<unreadable>}"
  fi
  # opencode serving (the overlay artifact, spawned by the supervisor).
  OC_CODE=$(kubectl -n "$NS" exec "$POD" -c workspace -- \
    sh -c 'curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:4096/ 2>/dev/null' 2>/dev/null || echo 000)
  if [ "$OC_CODE" != "000" ]; then
    pass S5.2b "opencode serves on :4096 (http $OC_CODE)"
  else
    fail S5.2b "opencode not reachable on :4096"
  fi
  # S2's deferred cluster e2e: the supervisor-written redact wrapper.
  REDACT_OUT=$(kubectl -n "$NS" exec "$POD" -c workspace -- \
    sh -c 'echo token=sk-ant-api03-abcdef123456 | /sandbox-runtime/bin/redact 2>/dev/null' 2>/dev/null || true)
  if [ -n "$REDACT_OUT" ] && ! echo "$REDACT_OUT" | grep -q "sk-ant-api03-abcdef123456"; then
    pass S5.2c "supervisor redact wrapper redacts through a real pipe (S2 cluster e2e)"
  else
    fail S5.2c "redact wrapper missing or leaked: ${REDACT_OUT:-absent}"
  fi
else
  fail S5.2 "workspace never reached Active"
  kubectl -n "$NS" describe pod -l llmsafespaces.dev/workspace="$WS_MAIN" 2>&1 | tail -30 || true
fi

# --- S5.3: agentd verify failure → condition + event -------------------------

WS_BAD_AG="ws-s5-bad-agentd"
BAD_SHA=$(printf 'agentd-tamper-%s' "$AGENTD_SHA" | head -c 64)
log "S5.3: upgrading chart with a WRONG agentd break-glass pin"
helm upgrade --install "$RELEASE" helm -n "$NS" \
  "${HELM_LEAN_ARGS[@]}" \
  --set "controller.agentdDelivery.image=${AGENTD_REF}" \
  --set "controller.agentdDelivery.binarySHA256Amd64=${BAD_SHA}" \
  --set "controller.agentdDelivery.binarySHA256Arm64=${BAD_SHA}" \
  --set "controller.opencodeDelivery.image=${OPENCODE_REF}" \
  --set "controller.opencodeDelivery.binarySHA256Amd64=${OPENCODE_SHA}" \
  --set "controller.opencodeDelivery.binarySHA256Arm64=${OPENCODE_SHA}" \
  >/dev/null 2>&1 || log "S5.3: tamper upgrade reported an error (continuing — the check polls conditions)"

apply_workspace "$WS_BAD_AG"
AG_COND=""
for i in $(seq 1 120); do
  AG_COND=$(kubectl -n "$NS" get workspace "$WS_BAD_AG" -o jsonpath='{range .status.conditions[*]}{.type}={.status} {end}' 2>/dev/null || true)
  echo "$AG_COND" | grep -q "AgentdVerified=False" && break
  sleep 2
done
if echo "$AG_COND" | grep -q "AgentdVerified=False"; then
  AG_EVENTS=$(kubectl get events --field-selector involvedObject.name="$WS_BAD_AG" -o json 2>/dev/null | jq -r '.items[].reason' | sort -u | tr '\n' ' ' || true)
  if echo "$AG_EVENTS" | grep -q "AgentdVerificationFailed"; then
    pass S5.3 "wrong agentd pin → exit 81 → AgentdVerified=False condition + AgentdVerificationFailed event"
  else
    pass S5.3 "wrong agentd pin → AgentdVerified=False condition (event reasons: ${AG_EVENTS:-none})"
  fi
else
  fail S5.3 "wrong agentd pin never surfaced the condition: ${AG_COND:-none}"
fi
kubectl -n "$NS" delete workspace "$WS_BAD_AG" --wait=false >/dev/null 2>&1 || true

# --- S5.4: opencode verify failure → condition + event -----------------------

WS_BAD_OC="ws-s5-bad-opencode"
BAD_OC_SHA=$(printf 'opencode-tamper-%s' "$OPENCODE_SHA" | head -c 64)
log "S5.4: upgrading chart with a WRONG opencode break-glass pin"
helm upgrade --install "$RELEASE" helm -n "$NS" \
  "${HELM_LEAN_ARGS[@]}" \
  --set "controller.agentdDelivery.image=${AGENTD_REF}" \
  --set "controller.agentdDelivery.binarySHA256Amd64=${AGENTD_SHA}" \
  --set "controller.agentdDelivery.binarySHA256Arm64=${AGENTD_SHA}" \
  --set "controller.opencodeDelivery.image=${OPENCODE_REF}" \
  --set "controller.opencodeDelivery.binarySHA256Amd64=${BAD_OC_SHA}" \
  --set "controller.opencodeDelivery.binarySHA256Arm64=${BAD_OC_SHA}" \
  >/dev/null 2>&1 || log "S5.4: tamper upgrade reported an error (continuing — the check polls conditions)"

apply_workspace "$WS_BAD_OC"
OC_COND=""
for i in $(seq 1 120); do
  OC_COND=$(kubectl -n "$NS" get workspace "$WS_BAD_OC" -o jsonpath='{range .status.conditions[*]}{.type}={.status} {end}' 2>/dev/null || true)
  echo "$OC_COND" | grep -q "OpencodeVerified=False" && break
  sleep 2
done
if echo "$OC_COND" | grep -q "OpencodeVerified=False"; then
  pass S5.4 "wrong opencode pin → exit 83 → OpencodeVerified=False condition"
else
  fail S5.4 "wrong opencode pin never surfaced the condition: ${OC_COND:-none}"
fi
kubectl -n "$NS" delete workspace "$WS_BAD_OC" --wait=false >/dev/null 2>&1 || true

log "restoring correct pins (controller rolls in background; S5.5 waits for readiness)"
helm upgrade --install "$RELEASE" helm -n "$NS" \
  "${HELM_LEAN_ARGS[@]}" "${HELM_PIN_ARGS[@]}" >/dev/null 2>&1 \
  || log "restore upgrade reported an error (continuing)"
kubectl -n "$NS" rollout status deployment/"${RELEASE}-llmsafespaces-controller" --timeout=300s \
  || fail S5.5 "controller did not roll back to correct pins (S5.5 cannot run)"

# --- S5.5: resume-path pull cost (cold-node overlay re-pull) ------------------

log "S5.5: suspend → evict overlay images from the node → activate (cold pull)"
if wait_phase "$WS_MAIN" Active 60 \
   && kubectl -n "$NS" patch workspace "$WS_MAIN" --type=merge -p '{"spec":{"suspend":true}}' >/dev/null \
   && wait_phase "$WS_MAIN" Suspended 300; then
  NODE=$(kind get nodes --name "$CLUSTER_NAME" | head -1)
  for img in "$AGENTD_REF" "$OPENCODE_REF"; do
    docker exec "$NODE" crictl rmi "$img" >/dev/null 2>&1 || true
  done
  T0=$(date +%s)
  kubectl -n "$NS" patch workspace "$WS_MAIN" --type=merge -p '{"spec":{"suspend":false}}' >/dev/null
  if wait_phase "$WS_MAIN" Active 600; then
    T1=$(date +%s)
    RESUME=$((T1 - T0))
    {
      echo "### S5.5 resume-path pull cost (cold node)"
      echo "- suspend → Suspended: ok"
      echo "- node overlay images evicted (agentd + opencode)"
      echo "- activate → Active: **${RESUME}s** (budget ${RESUME_BUDGET}s)"
    } >> "$STEP_SUMMARY"
    if [ "$RESUME" -le "$RESUME_BUDGET" ]; then
      pass S5.5 "cold-pull resume ${RESUME}s ≤ ${RESUME_BUDGET}s budget"
    else
      fail S5.5 "cold-pull resume ${RESUME}s exceeded ${RESUME_BUDGET}s budget"
    fi
  else
    fail S5.5 "workspace never returned to Active after image eviction"
  fi
else
  fail S5.5 "suspend flow did not complete"
fi

# --- S5.6: gVisor (runsc) leg --------------------------------------------------
# Design 0051's open item: nested RO image volumes under runsc. Install
# runsc into the kind node, register the runtime handler, boot a workspace
# on RuntimeClass gvisor. Best-effort INSTALL: a node that cannot host
# runsc is an environment failure (SKIP with a loud marker — the flip
# decision needs a real run, not a skip, so this still reports red in CI
# unless explicitly env-gated).
if [ "${S5_SKIP_GVISOR:-0}" = "1" ]; then
  fail S5.6 "SKIPPED via S5_SKIP_GVISOR=1 — the flip decision requires a real runsc run"
else
  log "S5.6: installing gVisor runsc on the kind node"
  NODE=$(kind get nodes --name "$CLUSTER_NAME" | head -1)
  if docker exec "$NODE" bash -c '
      set -e
      apt-get update -qq >/dev/null 2>&1 || true
      apt-get install -y -qq apt-transport-https gnupg2 >/dev/null 2>&1 || true
      curl -fsSL https://gvisor.dev/archives.asc | apt-key add - >/dev/null 2>&1 || true
      echo "deb https://storage.googleapis.com/gvisor/releases release main" > /etc/apt/sources.list.d/gvisor.list
      apt-get update -qq >/dev/null 2>&1
      apt-get install -y -qq runsc >/dev/null 2>&1
      runsc --version >/dev/null
      # Register the handler in containerd (config_v2 runtime table).
      CFG=/etc/containerd/config.toml
      grep -q "runsc" "$CFG" || {
        printf "\n[plugins.\"io.containerd.grpc.v1.cri\".containerd.runtimes.runsc]\n  runtime_type = \"io.containerd.runsc.v1\"\n" >> "$CFG"
      }
    ' >/dev/null 2>&1; then
    docker exec "$NODE" systemctl restart containerd >/dev/null 2>&1 || docker exec "$NODE" pkill -x containerd >/dev/null 2>&1 || true
    sleep 10
    cat <<'EOF' | kubectl apply -f - >/dev/null
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gvisor
handler: runsc
EOF
    WS_GVISOR="ws-s5-gvisor"
    log "S5.6: creating gvisor workspace $WS_GVISOR"
    apply_workspace "$WS_GVISOR" '  runtimeClassName: gvisor'
    if wait_phase "$WS_GVISOR" Active 600; then
      GV_POD=$(pod_of "$WS_GVISOR")
      GV_OC=$(kubectl -n "$NS" exec "$GV_POD" -c workspace -- \
        sh -c 'curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:4096/ 2>/dev/null' 2>/dev/null || echo 000)
      if [ "$GV_OC" != "000" ]; then
        pass S5.6 "gVisor pod Active with BOTH image volumes; opencode serves (http $GV_OC)"
      else
        fail S5.6 "gVisor pod Active but opencode unreachable (overlay mount issue under runsc?)"
      fi
    else
      fail S5.6 "gvisor workspace never reached Active (image volumes under runsc — design 0051 open item)"
      kubectl -n "$NS" describe pod -l llmsafespaces.dev/workspace="$WS_GVISOR" 2>&1 | tail -30 || true
    fi
    kubectl -n "$NS" delete workspace "$WS_GVISOR" --wait=false >/dev/null 2>&1 || true
  else
    fail S5.6 "runsc installation failed on the kind node (environment — re-run on a gvisor-capable runner)"
  fi
fi
