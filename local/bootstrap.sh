#!/usr/bin/env bash
# Bootstrap a kind cluster with a fully-installed LLMSafeSpaces control plane.
#
# Phases:
#   1. Verify prerequisites (kind, kubectl, helm, docker)
#   2. Create kind cluster (idempotent)
#   3. Build api, controller, runtime-base images and load into kind
#   4. Install cert-manager and wait for it
#   5. Install Postgres + Redis (local/postgres-redis.yaml) and wait
#   6. helm install LLMSafeSpaces and wait for rollout
#   7. Print smoke-test commands
#
# Idempotent: re-running re-uses the existing cluster and images.
# Use ./teardown.sh to remove everything.
#
# Environment overrides (sane defaults):
#   CLUSTER_NAME      - kind cluster name (default: llmsafespaces)
#   IMAGE_TAG         - tag used for built images (default: dev)
#   OPENCODE_VERSION  - opencode version pinned into the delivery artifact (default: 1.18.25)
#   GOPROXY           - Go proxy for module downloads in builds (default: $(go env GOPROXY))
#   SKIP_BUILD        - set to 1 to skip docker build / load (re-use existing)
#   SKIP_CERT_MANAGER - set to 1 if cert-manager is already installed
#   SKIP_DEPS         - set to 1 if Postgres/Redis are already installed
set -Eeuo pipefail

# -----------------------------------------------------------------------------
# Pretty logging
# -----------------------------------------------------------------------------
if [[ -t 1 ]]; then
    BOLD=$'\033[1m'; DIM=$'\033[2m'; RED=$'\033[31m'; GREEN=$'\033[32m'
    YELLOW=$'\033[33m'; CYAN=$'\033[36m'; RESET=$'\033[0m'
else
    BOLD=''; DIM=''; RED=''; GREEN=''; YELLOW=''; CYAN=''; RESET=''
fi
log()  { printf '%s==>%s %s\n' "${CYAN}${BOLD}" "${RESET}" "$*"; }
ok()   { printf '%s ✓%s %s\n' "${GREEN}" "${RESET}" "$*"; }
warn() { printf '%s !%s %s\n' "${YELLOW}" "${RESET}" "$*" >&2; }
die()  { printf '%s ✗%s %s\n' "${RED}${BOLD}" "${RESET}" "$*" >&2; exit 1; }

# -----------------------------------------------------------------------------
# Configuration
# -----------------------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

CLUSTER_NAME="${CLUSTER_NAME:-llmsafespaces}"
IMAGE_TAG="${IMAGE_TAG:-dev}"
OPENCODE_VERSION="${OPENCODE_VERSION:-1.18.25}"
GOPROXY_BUILD="${GOPROXY:-$(go env GOPROXY 2>/dev/null || echo direct)}"
NS="llmsafespaces"
RELEASE_NAME="llmsafespaces"

API_IMAGE="llmsafespaces/api:${IMAGE_TAG}"
CONTROLLER_IMAGE="llmsafespaces/controller:${IMAGE_TAG}"
RUNTIME_IMAGE="llmsafespaces/runtime-base:${IMAGE_TAG}"

# -----------------------------------------------------------------------------
# Phase 1: prerequisites
# -----------------------------------------------------------------------------
log "Phase 1/6 — prerequisite checks"
for cmd in kind kubectl helm docker go; do
    if ! command -v "$cmd" >/dev/null 2>&1; then
        die "$cmd not found in PATH. Install it and re-run."
    fi
done
docker info >/dev/null 2>&1 || die "Docker daemon not reachable. Start Docker and re-run."
ok "kind=$(kind version 2>&1 | head -1)"
ok "kubectl=$(kubectl version --client 2>&1 | grep -oE 'v[0-9.]+' | head -1)"
ok "helm=$(helm version --short 2>&1 | head -1)"
ok "go=$(go env GOVERSION)"

# -----------------------------------------------------------------------------
# Phase 2: kind cluster
# -----------------------------------------------------------------------------
log "Phase 2/6 — kind cluster"
if kind get clusters 2>&1 | grep -qx "${CLUSTER_NAME}"; then
    ok "cluster '${CLUSTER_NAME}' already exists"
else
    kind create cluster --config "${SCRIPT_DIR}/kind-cluster.yaml"
    ok "cluster created"
fi
kubectl --context "kind-${CLUSTER_NAME}" cluster-info >/dev/null
ok "kubectl context kind-${CLUSTER_NAME} reachable"

# Local registry for the digest-pinned delivery artifacts (design 0053
# §4.5 — both pins are mandatory; digest refs must resolve in-node).
# Same pattern as the e2e-nightly workflow.
if docker ps --format '{{.Names}}' | grep -qx lss-dev-registry; then
    ok "local registry already running"
else
    log "  starting local registry (:5001)"
    docker run -d --restart=always -p "127.0.0.1:5001:5000" \
        --network bridge --name lss-dev-registry registry:3 >/dev/null
    for _ in $(seq 1 30); do
        curl -fs http://127.0.0.1:5001/v2/ >/dev/null 2>&1 && break
        sleep 1
    done
    ok "local registry ready"
fi
for node in $(kind get nodes --name "${CLUSTER_NAME}"); do
    docker exec "$node" mkdir -p /etc/containerd/certs.d/localhost:5001
    printf '[host."http://lss-dev-registry:5000"]\n' \
        | docker exec -i "$node" cp /dev/stdin /etc/containerd/certs.d/localhost:5001/hosts.toml
done
if [ "$(docker inspect -f='{{json .NetworkSettings.Networks.kind}}' lss-dev-registry)" = "null" ]; then
    docker network connect kind lss-dev-registry
fi
ok "registry wired into kind node"

# -----------------------------------------------------------------------------
# Disk hygiene: prune buildx + image cache before building, and trim the
# kind node's containerd image store of our own previous tags. Without this,
# repeated bootstrap runs progressively fill the disk and `kind load` fails
# with "no space left on device" once the buildx activity dir or /tmp
# overflow.
#
# Set DISABLE_DISK_PRUNE=1 to skip.
# -----------------------------------------------------------------------------
if [[ "${DISABLE_DISK_PRUNE:-0}" == "1" ]]; then
    warn "DISABLE_DISK_PRUNE=1 — skipping cleanup"
else
    log "  pruning docker buildx + dangling images (DISABLE_DISK_PRUNE=1 to skip)"
    docker buildx prune -af >/dev/null 2>&1 || true
    docker image prune -af >/dev/null 2>&1 || true
    # Drop our own prior image tags from the kind node so kind load can
    # re-import without first fighting deduplication on a full disk.
    if docker ps --format '{{.Names}}' | grep -qx "${CLUSTER_NAME}-control-plane"; then
        for img in "${API_IMAGE}" "${CONTROLLER_IMAGE}" "${RUNTIME_IMAGE}"; do
            docker exec "${CLUSTER_NAME}-control-plane" \
                crictl rmi "docker.io/${img}" >/dev/null 2>&1 || true
        done
    fi
    AVAIL_GB=$(df -BG / 2>/dev/null | awk 'NR==2 { gsub("G","",$4); print $4 }')
    if [[ -n "${AVAIL_GB}" ]] && (( AVAIL_GB < 5 )); then
        warn "only ${AVAIL_GB}G free on /; build may fail. Free up space and retry."
    else
        ok "disk hygiene done (${AVAIL_GB:-?}G free on /)"
    fi
fi

# -----------------------------------------------------------------------------
# Phase 3: build + load images
# -----------------------------------------------------------------------------
if [[ "${SKIP_BUILD:-0}" == "1" ]]; then
    warn "SKIP_BUILD=1 — re-using existing images"
else
    log "Phase 3/6 — build images (api, controller, runtime-base)"
    cd "${REPO_ROOT}"

    # GOPROXY override: some environments (e.g. WSL2 with corp DNS) cannot
    # resolve proxy.golang.org from inside the build container.
    BUILD_ARGS=(--network host --build-arg "GOPROXY=${GOPROXY_BUILD}")

    log "  building ${API_IMAGE}"
    docker build "${BUILD_ARGS[@]}" -f api/Dockerfile -t "${API_IMAGE}" .

    log "  building ${CONTROLLER_IMAGE}"
    docker build "${BUILD_ARGS[@]}" -f controller/Dockerfile -t "${CONTROLLER_IMAGE}" .

    if [[ "${SKIP_RUNTIME_BUILD:-0}" == "1" ]] && \
       docker image inspect "${RUNTIME_IMAGE}" >/dev/null 2>&1; then
        warn "  SKIP_RUNTIME_BUILD=1 — re-using ${RUNTIME_IMAGE}"
    else
        log "  building ${RUNTIME_IMAGE} (dev-OS content)"
        docker build "${BUILD_ARGS[@]}" \
            -f runtimes/base/Dockerfile -t "${RUNTIME_IMAGE}" .
    fi

    # Design 0053 S3: both delivery artifacts, digest-pinned via the
    # local registry (the base carries no platform binaries).
    log "  building + pushing delivery artifacts (agentd, opencode)"
    docker build "${BUILD_ARGS[@]}" \
        -f cmd/workspace-agentd/Dockerfile -t localhost:5001/llmsafespaces/agentd:${IMAGE_TAG} .
    docker push localhost:5001/llmsafespaces/agentd:${IMAGE_TAG} >/dev/null
    AGENTD_REF=$(docker inspect --format '{{index .RepoDigests 0}}' localhost:5001/llmsafespaces/agentd:${IMAGE_TAG})
    case "${AGENTD_REF}" in
        *@sha256:*) AGENTD_REF="localhost:5001/llmsafespaces/agentd:${IMAGE_TAG}@${AGENTD_REF##*@}" ;;
        *) die "could not determine agentd digest" ;;
    esac
    A_CID=$(docker create localhost:5001/llmsafespaces/agentd:${IMAGE_TAG} no-start)
    docker cp "${A_CID}:/usr/local/bin/workspace-agentd" /tmp/lss-agentd >/dev/null 2>&1 || { docker rm -f "${A_CID}"; die "agentd binary extract failed"; }
    docker rm -f "${A_CID}" >/dev/null 2>&1
    AGENTD_BINARY_SHA=$(sha256sum /tmp/lss-agentd | cut -d' ' -f1)
    [ -n "${AGENTD_BINARY_SHA}" ] || die "agentd binary hash failed"

    docker build --network=host \
        --build-arg "OPENCODE_VERSION=${OPENCODE_VERSION}" \
        -f runtimes/opencode/Dockerfile -t localhost:5001/llmsafespaces/opencode:${IMAGE_TAG} .
    docker push localhost:5001/llmsafespaces/opencode:${IMAGE_TAG} >/dev/null
    OPENCODE_REF=$(docker inspect --format '{{index .RepoDigests 0}}' localhost:5001/llmsafespaces/opencode:${IMAGE_TAG})
    case "${OPENCODE_REF}" in
        *@sha256:*) OPENCODE_REF="localhost:5001/llmsafespaces/opencode:${IMAGE_TAG}@${OPENCODE_REF##*@}" ;;
        *) die "could not determine opencode digest" ;;
    esac
    O_CID=$(docker create localhost:5001/llmsafespaces/opencode:${IMAGE_TAG} no-start)
    docker cp "${O_CID}:/usr/local/bin/opencode" /tmp/lss-opencode >/dev/null 2>&1 || { docker rm -f "${O_CID}"; die "opencode binary extract failed"; }
    docker rm -f "${O_CID}" >/dev/null 2>&1
    OPENCODE_BINARY_SHA=$(sha256sum /tmp/lss-opencode | cut -d' ' -f1)
    [ -n "${OPENCODE_BINARY_SHA}" ] || die "opencode binary hash failed"
    ok "delivery artifacts pinned"

    log "  loading images into kind (one at a time to keep /tmp use small)"
    for img in "${API_IMAGE}" "${CONTROLLER_IMAGE}" "${RUNTIME_IMAGE}"; do
        kind load docker-image "${img}" --name "${CLUSTER_NAME}"
    done
    ok "images loaded"
fi

# -----------------------------------------------------------------------------
# Phase 4: cert-manager
# -----------------------------------------------------------------------------
if [[ "${SKIP_CERT_MANAGER:-0}" == "1" ]]; then
    warn "SKIP_CERT_MANAGER=1 — assuming cert-manager already installed"
else
    log "Phase 4/6 — cert-manager"
    if kubectl --context "kind-${CLUSTER_NAME}" get ns cert-manager >/dev/null 2>&1; then
        ok "cert-manager namespace exists"
    else
        kubectl --context "kind-${CLUSTER_NAME}" apply \
            -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.0/cert-manager.yaml
    fi
    log "  waiting for cert-manager rollout (up to 3m)"
    for d in cert-manager cert-manager-cainjector cert-manager-webhook; do
        kubectl --context "kind-${CLUSTER_NAME}" -n cert-manager rollout status \
            "deployment/${d}" --timeout=180s
    done
    ok "cert-manager ready"
fi

# -----------------------------------------------------------------------------
# Phase 5: Postgres + Redis
# -----------------------------------------------------------------------------
if [[ "${SKIP_DEPS:-0}" == "1" ]]; then
    warn "SKIP_DEPS=1 — assuming Postgres/Redis already installed"
else
    log "Phase 5/6 — Postgres + Redis"

    # The Postgres/Valkey manifests reference the llmsafespaces-credentials
    # Secret for their passwords. The chart creates this Secret as a
    # pre-install hook — but Postgres must start BEFORE the chart. Create
    # the namespace + Secret first with known non-insecure passwords, then
    # pass the same passwords to helm install so the chart does not rotate
    # them.
    kubectl --context "kind-${CLUSTER_NAME}" create namespace "${NS}" \
        --dry-run=client -o yaml 2>/dev/null | \
        kubectl --context "kind-${CLUSTER_NAME}" apply -f -
    kubectl --context "kind-${CLUSTER_NAME}" -n "${NS}" create secret \
        generic llmsafespaces-credentials \
        --from-literal=postgres-password=dev-pg-pw-2026 \
        --from-literal=redis-password=dev-redis-pw-2026 \
        --dry-run=client -o yaml 2>/dev/null | \
        kubectl --context "kind-${CLUSTER_NAME}" apply -f -

    kubectl --context "kind-${CLUSTER_NAME}" apply -f "${SCRIPT_DIR}/postgres-redis.yaml"
    log "  waiting for Postgres rollout (up to 3m)"
    kubectl --context "kind-${CLUSTER_NAME}" -n "${NS}" rollout status deployment/postgres --timeout=180s
    log "  waiting for Redis rollout (up to 3m)"
    kubectl --context "kind-${CLUSTER_NAME}" -n "${NS}" rollout status deployment/valkey --timeout=180s
    ok "data services ready"
fi

# -----------------------------------------------------------------------------
# Phase 6: helm install LLMSafeSpaces
# -----------------------------------------------------------------------------
log "Phase 6/6 — helm install LLMSafeSpaces"
helm --kube-context "kind-${CLUSTER_NAME}" upgrade --install "${RELEASE_NAME}" \
    "${REPO_ROOT}/helm" \
    -n "${NS}" --create-namespace \
    --set "api.image.repository=llmsafespaces/api" \
    --set "api.image.tag=${IMAGE_TAG}" \
    --set "api.image.pullPolicy=IfNotPresent" \
    --set "controller.image.repository=llmsafespaces/controller" \
    --set "controller.image.tag=${IMAGE_TAG}" \
    --set "controller.image.pullPolicy=IfNotPresent" \
    --set "postgresql.host=postgres" \
    --set "postgresql.port=5432" \
    --set "postgresql.user=llmsafespaces" \
    --set "postgresql.database=llmsafespaces" \
    --set "redis.host=redis-master" \
    --set "redis.port=6379" \
    --set "externalSecret.create=true" \
    --set "externalSecret.postgresPassword=dev-pg-pw-2026" \
    --set "externalSecret.redisPassword=dev-redis-pw-2026" \
    --set "rbac.scope=cluster" \
    --set "api.config.logging.development=true" \
    --set "controller.agentdDelivery.image=${AGENTD_REF}" \
    --set "controller.agentdDelivery.binarySHA256Amd64=${AGENTD_BINARY_SHA}" \
    --set "controller.agentdDelivery.binarySHA256Arm64=${AGENTD_BINARY_SHA}" \
    --set "controller.opencodeDelivery.image=${OPENCODE_REF}" \
    --set "controller.opencodeDelivery.binarySHA256Amd64=${OPENCODE_BINARY_SHA}" \
    --set "controller.opencodeDelivery.binarySHA256Arm64=${OPENCODE_BINARY_SHA}" \
    --wait --timeout 5m

log "  waiting for API rollout"
kubectl --context "kind-${CLUSTER_NAME}" -n "${NS}" rollout status \
    deployment/llmsafespaces-api --timeout=180s

log "  waiting for controller rollout"
kubectl --context "kind-${CLUSTER_NAME}" -n "${NS}" rollout status \
    deployment/llmsafespaces-controller --timeout=180s

ok "LLMSafeSpaces installed"

# -----------------------------------------------------------------------------
# Done — print next steps
# -----------------------------------------------------------------------------
cat <<EOF

${BOLD}${GREEN}Bootstrap complete.${RESET}

Cluster:    kind-${CLUSTER_NAME}
Namespace:  ${NS}
Release:    ${RELEASE_NAME}

${BOLD}Next steps:${RESET}

  # Smoke-test the API (port-forward to a random local port to avoid
  # conflicts with anything on host port 8080):
  kubectl --context kind-${CLUSTER_NAME} -n ${NS} port-forward \\
      svc/llmsafespaces-api 18080:8080 &
  curl http://localhost:18080/livez
  curl http://localhost:18080/readyz

  # Run the end-to-end test (creates a Workspace + Sandbox via kubectl,
  # verifies the controller reconciles them):
  ${SCRIPT_DIR}/test.sh

  # Tear everything down:
  ${SCRIPT_DIR}/teardown.sh

EOF
