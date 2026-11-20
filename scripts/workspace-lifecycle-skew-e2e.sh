#!/usr/bin/env bash
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Workspace lifecycle integration on a CRD-SKEWED kind cluster (owner
# direction 2026-11-17 — the PR #2088 bug class at the kubelet tier).
#
# THE SHAPE: helm never upgrades crds/ on `helm upgrade` (helm/README.md),
# so a normally-upgraded .15/.16 cluster runs the new controller against
# the PRE-.15 Workspace CRD — which has no spec.harness field. A
# structural apiserver silently PRUNES the unknown field on write. The
# .15 incidents: refresh-compute dead, starting workspaces never
# completing, suspend/resume silently gone — the phase machine starved
# by the legacy-harness backfill loop (PR #2088 fixed it; the envtest
# twin of this suite is controller/internal/workspace/
# lifecycle_envtest_test.go).
#
# What ONLY this tier proves: the full stack (real kubelet, real pods,
# real image pulls, the real chart) driving the OWNER-VISIBLE user path
# on a skewed cluster:
#   LC-K1 START:    a legacy workspace (no spec.harness, no marker)
#                   applied straight to the CRD (the pre-.15 fleet
#                   shape) reaches phase=Active — the widened .15
#                   symptom ("starting workspaces never complete").
#   LC-K2 REFRESH:  the RefreshWorkspaceCompute CRD write (restart-
#                   Generation bump + the #1505 force marker) recycles
#                   the pod and returns to Active with the generation
#                   observed — "refreshing compute via the UX".
#   LC-K3 SUSPEND/RESUME: spec.suspend=true → Suspended, pod gone,
#                   PVC RETAINED; resume → Active again.
#   LC-K4 CONVERGE: the operator applies the current CRD (kubectl apply
#                   -f helm/crds/workspace.yaml); the backfill lands the
#                   explicit opencode stamp without disrupting the
#                   running workspace.
#
# The skewed CRD fixture is the SAME pinned artifact the envtest
# lifecycle suite serves: controller/internal/workspace/testdata/
# legacycrd/workspaces.llmsafespaces.dev.yaml (captured from tag
# v0.44.14 — the last harness-less CRD). One artifact, two tiers.
#
# Topology: the us2-kind-integration.sh pattern — controller-lean chart
# (api/mcp/migrations off; webhooks on; cert-manager first), local
# registry for the digest-pinned delivery artifacts (agentd + opencode +
# pi — the design-0053 §4.5/§5.4 mandatory trio the chart refuses to
# render without), --skip-crds so the pre-applied legacy CRD stays
# served (the skew under test).
#
# NOT runnable on the authoring sandbox (no docker): executed by
# .github/workflows/workspace-lifecycle-integration.yml (PR-path-gated
# on the phase-machine surface + weekly + workflow_dispatch) or any
# docker-capable host.
#
# Usage:
#   scripts/workspace-lifecycle-skew-e2e.sh [--keep]   # --keep: no teardown
# Exit codes: 0 all PASS · 1 setup failure · 2 one or more checks FAILED.

set -euo pipefail

TMPDIR="${TMPDIR:-/tmp}"
KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

CLUSTER_NAME="${CLUSTER_NAME:-ws-lifecycle-int}"
REG_NAME="${REG_NAME:-ws-lifecycle-registry}"
REG_PORT="${REG_PORT:-5001}"
NS="${NS:-llmsafespaces}"
RELEASE="${RELEASE:-lss}"
WORKSPACE_NAME="ws-lifecycle-int"
# Node image pinned to match local/kind-cluster.yaml (chart floor 1.35).
NODE_IMAGE="kindest/node:v1.35.5@sha256:ce977ae6d65918d0b58a5f8b5e940429c2ce42fa3a5619ec2bbc60b949c0ac95"
REG="localhost:${REG_PORT}"
# The pinned pre-.15 CRD (v0.44.14) — the skewed environment. Same
# fixture the envtest lifecycle suite serves; see the header comment.
LEGACY_CRD="controller/internal/workspace/testdata/legacycrd/workspaces.llmsafespaces.dev.yaml"
CURRENT_CRD="helm/crds/workspace.yaml"
declare -a RESULTS=()

log()  { printf '[ws-lifecycle] %s\n' "$*"; }
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
  # Teardown ONLY on success (or --keep). On failure the cluster stays
  # up for the workflow's diagnostics step (us2 precedent).
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

kc_ws_field() { # $1: jsonpath expression against the workspace object
  kubectl -n "$NS" get workspace "$WORKSPACE_NAME" -o jsonpath="$1" 2>/dev/null || true
}

wait_phase() { # $1: expected phase; $2: budget in seconds; $3: check label
  local expected="$1" budget="$2" label="$3" phase="" i
  for ((i = 0; i < budget / 3; i++)); do
    phase=$(kc_ws_field '{.status.phase}')
    if [ "$phase" = "$expected" ]; then
      log "  phase=$expected after ~$((i * 3))s ($label)"
      return 0
    fi
    sleep 3
  done
  log "  phase never reached $expected (last: ${phase:-<empty>}) within ${budget}s ($label)"
  return 1
}

# --- Preflight ---------------------------------------------------------------
for bin in docker kind helm kubectl go curl sha256sum; do
  command -v "$bin" >/dev/null 2>&1 || { log "missing required binary: $bin"; exit 1; }
done
[ -f "$LEGACY_CRD" ] || { log "missing skewed CRD fixture: $LEGACY_CRD"; exit 1; }
[ -f "$CURRENT_CRD" ] || { log "missing current CRD: $CURRENT_CRD"; exit 1; }
if grep -q "harness" "$LEGACY_CRD"; then
  log "fixture drift: $LEGACY_CRD must be the PRE-.15 (harness-less) CRD"
  exit 1
fi

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

for node in $(kind get nodes --name "$CLUSTER_NAME"); do
  docker exec "$node" mkdir -p "/etc/containerd/certs.d/localhost:${REG_PORT}"
  printf '[host."http://%s:5000"]\n' "$REG_NAME" \
    | docker exec -i "$node" cp /dev/stdin "/etc/containerd/certs.d/localhost:${REG_PORT}/hosts.toml"
done
if [ "$(docker inspect -f='{{json .NetworkSettings.Networks.kind}}' "$REG_NAME")" = "null" ]; then
  docker network connect kind "$REG_NAME"
fi

# --- Images ------------------------------------------------------------------

log "building and pushing images to $REG (agentd, controller, runtime-base, opencode, pi)"
docker build --network host --build-arg VERSION=ci --build-arg COMMIT_SHA=wslcint \
  -f cmd/workspace-agentd/Dockerfile -t "$REG/llmsafespaces/agentd:ci" .
docker build --network host --build-arg VERSION=ci --build-arg COMMIT_SHA=wslcint \
  -f controller/Dockerfile -t "$REG/llmsafespaces/controller:ci" .
docker build --network host --build-arg VERSION=ci --build-arg COMMIT_SHA=wslcint \
  -f runtimes/base/Dockerfile -t "$REG/llmsafespaces/runtime-base:ci" .
# The chart-MANDATORY delivery artifacts (design 0053 §4.5/§5.4 — no baked
# fallback; the controller-deployment template fails render without them:
# posture-gate.yml / e2e-attachments-single-container.yml / local/
# bootstrap.sh all pin the same three). The opencode Dockerfile pins
# OPENCODE_VERSION itself; the pi build is the ~10min release-source
# compile (r1 review finding: the original script died in SETUP at
# 'controller.opencodeDelivery.image is mandatory').
docker build --network host \
  -f runtimes/opencode/Dockerfile -t "$REG/llmsafespaces/opencode:ci" .
docker build --network host \
  -f runtimes/pi/Dockerfile -t "$REG/llmsafespaces/pi:ci" runtimes/pi
docker push "$REG/llmsafespaces/agentd:ci" >/dev/null
docker push "$REG/llmsafespaces/controller:ci" >/dev/null
docker push "$REG/llmsafespaces/runtime-base:ci" >/dev/null
docker push "$REG/llmsafespaces/opencode:ci" >/dev/null
docker push "$REG/llmsafespaces/pi:ci" >/dev/null

AGENTD_REF=$(docker inspect --format '{{index .RepoDigests 0}}' "$REG/llmsafespaces/agentd:ci")
case "$AGENTD_REF" in
  *@sha256:*) AGENTD_REF="$REG/llmsafespaces/agentd:ci@${AGENTD_REF##*@}" ;;
  *) log "could not determine agentd manifest digest (RepoDigests empty)"; exit 1 ;;
esac
log "agentd reference: $AGENTD_REF"

# >>> extract-binary-sha (sentinels: executed verbatim by
# scripts/workspace_lifecycle_script_test.go under a stubbed docker — keep
# the block self-contained: only CID/BINARY_SHA, $REG, $TMPDIR, log, and
# external commands may be referenced).
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

# opencode + pi delivery refs and binary pins (design 0053 — the chart
# renders fail without all three delivery artifacts; the r1 finding).
OPENCODE_REF=$(docker inspect --format '{{index .RepoDigests 0}}' "$REG/llmsafespaces/opencode:ci")
case "$OPENCODE_REF" in
  *@sha256:*) OPENCODE_REF="$REG/llmsafespaces/opencode:ci@${OPENCODE_REF##*@}" ;;
  *) log "could not determine opencode manifest digest (RepoDigests empty)"; exit 1 ;;
esac
log "opencode reference: $OPENCODE_REF"
OC_CID=$(docker create "$REG/llmsafespaces/opencode:ci" no-start 2>/dev/null) \
  || { log "docker create failed for opencode binary extraction"; exit 1; }
docker cp "$OC_CID:/usr/local/bin/opencode" "$TMPDIR/opencode-bin" >/dev/null 2>&1 \
  || { docker rm -f "$OC_CID" >/dev/null 2>&1; log "docker cp failed to extract opencode binary"; exit 1; }
docker rm -f "$OC_CID" >/dev/null 2>&1
OPENCODE_BINARY_SHA=$(sha256sum "$TMPDIR/opencode-bin" 2>/dev/null | cut -d' ' -f1) \
  || { log "sha256sum failed on extracted opencode binary"; exit 1; }
[ -n "$OPENCODE_BINARY_SHA" ] || { log "could not hash extracted opencode binary"; exit 1; }
log "opencode binary sha256 (amd64): $OPENCODE_BINARY_SHA"

PI_REF=$(docker inspect --format '{{index .RepoDigests 0}}' "$REG/llmsafespaces/pi:ci")
case "$PI_REF" in
  *@sha256:*) PI_REF="$REG/llmsafespaces/pi:ci@${PI_REF##*@}" ;;
  *) log "could not determine pi manifest digest (RepoDigests empty)"; exit 1 ;;
esac
log "pi reference: $PI_REF"
PI_CID=$(docker create "$REG/llmsafespaces/pi:ci" no-start 2>/dev/null) \
  || { log "docker create failed for pi binary extraction"; exit 1; }
docker cp "$PI_CID:/pi" "$TMPDIR/pi-bin" >/dev/null 2>&1 \
  || { docker rm -f "$PI_CID" >/dev/null 2>&1; log "docker cp failed to extract pi binary"; exit 1; }
docker rm -f "$PI_CID" >/dev/null 2>&1
PI_BINARY_SHA=$(sha256sum "$TMPDIR/pi-bin" 2>/dev/null | cut -d' ' -f1) \
  || { log "sha256sum failed on extracted pi binary"; exit 1; }
[ -n "$PI_BINARY_SHA" ] || { log "could not hash extracted pi binary"; exit 1; }
log "pi binary sha256: $PI_BINARY_SHA"

# --- THE SKEW: pre-.15 Workspace CRD, then controller-lean chart over it --------
#
# The incident shape is "the Workspace CRD is stale, the rest of the
# cluster is healthy": the pre-.15 Workspace CRD (no spec.harness —
# the field the backfill cannot land) is applied FIRST, then the
# CURRENT non-Workspace platform CRDs (runtimeenvironment,
# inferencerelay) — the chart ships a RuntimeEnvironment "base"
# resource and helm builds the release manifest against the cluster's
# discovery, so those kinds must exist. The current workspace.yaml is
# NEVER applied here (that would destroy the skew under test) — pinned
# by TestWorkspaceLifecycleScript_SkewWiring.

log "applying the PRE-.15 Workspace CRD (the skew: no spec.harness served)"
kubectl apply -f "$LEGACY_CRD"
log "applying the CURRENT non-Workspace platform CRDs (the healthy-cluster half)"
for crd in runtimeenvironment.yaml inferencerelay.yaml; do
  kubectl apply -f "helm/crds/$crd"
done

# cert-manager first: the controller binary ALWAYS registers its admission
# webhooks (same manifest/version as e2e-nightly / us2).
log "installing cert-manager (webhook certs are mandatory for the controller)"
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.0/cert-manager.yaml
kubectl -n cert-manager rollout status deployment/cert-manager --timeout=180s
kubectl -n cert-manager rollout status deployment/cert-manager-webhook --timeout=180s

# Controller-lean chart with --skip-crds: helm would install the CURRENT
# CRDs on a fresh install and destroy the skew under test.
log "installing controller-lean chart (api/mcp/migrations off; webhooks on; --skip-crds)"
helm upgrade --install "$RELEASE" helm \
  -n "$NS" --create-namespace \
  --skip-crds \
  --set api.enabled=false \
  --set mcp.enabled=false \
  --set migrations.enabled=false \
  --set rbac.scope=cluster \
  --set relayOnlyKeyDelivery.enabled=false \
  --set "webhooks.allowedImageRegistries[0]=$REG/llmsafespaces/" \
  --set externalSecret.create=true \
  --set "externalSecret.postgresPassword=wslcint-pg" \
  --set "externalSecret.redisPassword=wslcint-redis" \
  --set controller.image.repository="$REG/llmsafespaces/controller" \
  --set controller.image.tag=ci \
  --set controller.image.pullPolicy=IfNotPresent \
  --set "controller.agentdDelivery.image=${AGENTD_REF}" \
  --set "controller.agentdDelivery.binarySHA256Amd64=${BINARY_SHA}" \
  --set "controller.agentdDelivery.binarySHA256Arm64=${BINARY_SHA}" \
  --set "controller.opencodeDelivery.image=${OPENCODE_REF}" \
  --set "controller.opencodeDelivery.binarySHA256Amd64=${OPENCODE_BINARY_SHA}" \
  --set "controller.opencodeDelivery.binarySHA256Arm64=${OPENCODE_BINARY_SHA}" \
  --set "controller.piDelivery.image=${PI_REF}" \
  --set "controller.piDelivery.binarySHA256Amd64=${PI_BINARY_SHA}" \
  --set "controller.piDelivery.binarySHA256Arm64=${PI_BINARY_SHA}" \
  --set controller.agentdSidecar.enabled=true \
  --wait --timeout 420s || { setup_diagnostics; exit 1; }

# Webhook bootstrap race (us2 precedent): wait for the webhook Service
# endpoints, then retry the apply through the window.
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

log "creating LEGACY workspace $WORKSPACE_NAME (no spec.harness — the pre-.15 fleet shape)"
WS_MANIFEST=$(mktemp)
cat >"$WS_MANIFEST" <<EOF
apiVersion: llmsafespaces.dev/v1
kind: Workspace
metadata:
  name: $WORKSPACE_NAME
spec:
  owner:
    userID: ws-lifecycle-int
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

# --- LC-K1: START completes under skew ---------------------------------------
# The widened .15 symptom: pre-#2088 the backfill loop eats every
# reconcile and the workspace parks in Creating forever.
log "LC-K1: waiting for phase=Active on the skewed CRD (start completes; budget 5m)"
if wait_phase Active 300 "LC-K1 start"; then
  pass LC-K1 "legacy workspace reached Active through the skewed phase machine"
else
  fail LC-K1 "start never completed on the skewed CRD (the .15 symptom)"
  kubectl -n "$NS" get workspace "$WORKSPACE_NAME" -o yaml | tail -30 || true
fi

POD_BEFORE=$(kc_ws_field '{.status.podName}')
RUNTIME_GEN_BEFORE=$(kc_ws_field '{.status.observedRestartGeneration}')
RUNTIME_GEN_NEXT=$(( ${RUNTIME_GEN_BEFORE:-0} + 1 ))

# --- LC-K2: REFRESH works under skew ------------------------------------------
# The RefreshWorkspaceCompute write shape: restartGeneration++ plus the
# #1505 user-consent force marker. Pre-#2088 the bump is never observed.
log "LC-K2: refresh-compute write (restartGeneration=$RUNTIME_GEN_NEXT + force marker)"
kubectl -n "$NS" annotate workspace "$WORKSPACE_NAME" \
  "llmsafespaces.dev/force-recycle=$RUNTIME_GEN_NEXT" --overwrite >/dev/null
kubectl -n "$NS" patch workspace "$WORKSPACE_NAME" --type=merge \
  -p "{\"spec\":{\"restartGeneration\":$RUNTIME_GEN_NEXT}}" >/dev/null

K2_OK=1
wait_phase Creating 90 "LC-K2 recycle" || K2_OK=0
if wait_phase Active 300 "LC-K2 re-Active"; then
  POD_AFTER=$(kc_ws_field '{.status.podName}')
  GEN_OBSERVED=$(kc_ws_field '{.status.observedRestartGeneration}')
  if [ -n "$POD_BEFORE" ] && [ -n "$POD_AFTER" ] && [ "$POD_BEFORE" != "$POD_AFTER" ]; then
    log "  pod recycled: $POD_BEFORE -> $POD_AFTER"
  else
    log "  pod names: before=$POD_BEFORE after=$POD_AFTER (same-name recreation is legal; phase governs)"
  fi
  [ "$GEN_OBSERVED" = "$RUNTIME_GEN_NEXT" ] \
    && pass LC-K2 "refresh observed (generation $GEN_OBSERVED) and returned to Active" \
    || { K2_OK=0; fail LC-K2 "generation not observed (got ${GEN_OBSERVED:-<empty>}, want $RUNTIME_GEN_NEXT)"; }
else
  K2_OK=0
  fail LC-K2 "refresh never returned to Active on the skewed CRD"
fi
[ "$K2_OK" = "1" ] || kubectl -n "$NS" logs "deploy/${RELEASE}-llmsafespaces-controller" --tail=40 2>&1 | sed 's/^/  /' || true

# --- LC-K3: SUSPEND/RESUME preserves state under skew --------------------------
log "LC-K3: suspend (spec.suspend=true)"
K3_OK=1
kubectl -n "$NS" patch workspace "$WORKSPACE_NAME" --type=merge \
  -p '{"spec":{"suspend":true}}' >/dev/null
if wait_phase Suspended 120 "LC-K3 suspend"; then
  POD_GONE=$(kubectl -n "$NS" get pods -l "llmsafespaces.dev/workspace=$WORKSPACE_NAME" \
    -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)
  [ -z "$POD_GONE" ] && log "  pod deleted on suspend" || { K3_OK=0; fail LC-K3 "pod survived suspend: $POD_GONE"; }
  if kubectl -n "$NS" get pvc "workspace-$WORKSPACE_NAME" >/dev/null 2>&1; then
    log "  PVC retained on suspend"
  else
    K3_OK=0
    fail LC-K3 "PVC missing after suspend (data stranded)"
  fi
  [ "$K3_OK" = "1" ] && pass LC-K3 "suspend: phase=Suspended, pod gone, PVC retained"
else
  K3_OK=0
  fail LC-K3 "workspace never reached Suspended on the skewed CRD"
fi

if [ "$K3_OK" = "1" ]; then
  log "LC-K3: resume (spec.suspend=false)"
  kubectl -n "$NS" patch workspace "$WORKSPACE_NAME" --type=merge \
    -p '{"spec":{"suspend":false}}' >/dev/null
  if wait_phase Active 300 "LC-K3 resume"; then
    kubectl -n "$NS" get pvc "workspace-$WORKSPACE_NAME" >/dev/null 2>&1 \
      && pass LC-K3 "resume: back to Active, PVC preserved through the round trip" \
      || fail LC-K3 "resume reached Active but the PVC was lost"
  else
    fail LC-K3 "resume never returned to Active on the skewed CRD"
  fi
fi

# --- LC-K4: CONVERGENCE when the operator applies the current CRD --------------
log "LC-K4: operator applies the current CRD ($CURRENT_CRD)"
kubectl apply -f "$CURRENT_CRD"
# The backfill re-attempts ride a 10-minute per-workspace window in the
# running controller; a controller restart resets the crash-safe attempt
# map so the next reconcile re-attempts immediately (converges within
# seconds instead of waiting out the window).
kubectl -n "$NS" rollout restart deployment "${RELEASE}-llmsafespaces-controller"
kubectl -n "$NS" rollout status deployment "${RELEASE}-llmsafespaces-controller" --timeout=180s

HARNESS=""
for i in $(seq 1 40); do
  HARNESS=$(kc_ws_field '{.spec.harness}')
  [ "$HARNESS" = "opencode" ] && break
  sleep 3
done
PHASE_AT_CONVERGE=$(kc_ws_field '{.status.phase}')
if [ "$HARNESS" = "opencode" ]; then
  if [ "$PHASE_AT_CONVERGE" = "Active" ]; then
    pass LC-K4 "CRD apply converged: spec.harness=opencode landed, workspace stayed Active"
  else
    fail LC-K4 "stamp landed but phase disrupted (now: ${PHASE_AT_CONVERGE:-<empty>})"
  fi
else
  fail LC-K4 "backfill never landed the harness stamp after the CRD apply (got: '${HARNESS:-<empty>}')"
  kubectl -n "$NS" logs "deploy/${RELEASE}-llmsafespaces-controller" --tail=40 2>&1 | sed 's/^/  /' || true
fi
