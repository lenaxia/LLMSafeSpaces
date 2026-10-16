#!/usr/bin/env bash
# longhorn.sh — Longhorn provisioning for #1709 /shared parked-PVC spike
# legs (and any future local leg that needs RWX volumes): installs the
# node-side prerequisites Longhorn needs inside a kind node (open-iscsi,
# nfs-common — the share-manager RWX export is mounted by the node kernel,
# so EVERY schedulable node needs the NFS client), deploys Longhorn via
# the pinned chart, waits for engine/instance-manager readiness, and
# applies the user-shared StorageClass + VolumeSnapshotClass the design
# doc names.
#
# Mirrors local/lib/gvisor.sh conventions (CLUSTER_NAME/CTX env, docker
# exec into kind nodes, loud provisioning failures).
#
# Chart pin: 1.12.1 — the version the production cluster runs
# (talos-ops-prod kubernetes/apps/storage/longhorn/app/helm-release.yaml);
# the spike must measure the posture operators actually deploy.
#
# Usage:
#   bash local/lib/longhorn.sh node-prereqs [node-name]  # apt pkgs in node
#   bash local/lib/longhorn.sh install                   # helm install + wait
#   bash local/lib/longhorn.sh classes                   # SC + VolumeSnapshotClass
set -Eeuo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-llmsafespaces-ci}"
CTX="${CTX:-kind-${CLUSTER_NAME}}"
NS_LONGHORN="${NS_LONGHORN:-longhorn-system}"
NS_SPIKE="${NS_SPIKE:-spike1709}"
LONGHORN_CHART_VERSION="${LONGHORN_CHART_VERSION:-1.12.1}"

longhorn_node_prereqs() { # node
    local node="$1"
    docker exec "$node" bash -c '
      set -e
      export DEBIAN_FRONTEND=noninteractive
      apt-get update -qq >/dev/null
      apt-get install -y -qq open-iscsi nfs-common util-linux >/dev/null
      systemctl enable --now iscsid >/dev/null 2>&1 || true
    '
}

longhorn_default_node() {
    kubectl --context "${CTX}" get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | head -1
}

longhorn_install() {
    helm repo add longhorn https://charts.longhorn.io >/dev/null 2>&1 || true
    helm repo update >/dev/null
    # defaultClass=false: kind's local-path stays default; the spike
    # provisions exclusively through the user-shared class below.
    # NOTE: createDefaultDiskLabeled is NOT set (run 38866952477: with it
    # true and no node.longhorn.io/create-default-disk label, Longhorn
    # creates NO disks — replicas never schedule, RWX volumes sit at
    # "Waiting for volume share to be available" for their whole life).
    # The default (false) auto-creates a disk on every schedulable node.
    helm upgrade --install longhorn longhorn/longhorn \
        --version "$LONGHORN_CHART_VERSION" \
        -n "$NS_LONGHORN" --create-namespace \
        --set defaultSettings.defaultReplicaCount=2 \
        --set defaultSettings.storageMinimalAvailablePercentage=15 \
        --set persistence.defaultClass=false \
        --set preUpgradeChecker.enabled=false \
        --wait --timeout 15m
    # Wait for the instance-manager pods (the CSI data plane) — helm's
    # --wait only covers deployments, and Longhorn creates instance-
    # managers ASYNCHRONOUSLY after install (run 38865495450: "no
    # matching resources found" because a bare kubectl wait races their
    # creation). Poll for existence first, then readiness; RWX also
    # needs the share-manager image pulled before the first attach, but
    # that starts on demand with the first RWX volume.
    local i count=0
    for i in $(seq 1 60); do
        count=$(kubectl --context "${CTX}" -n "$NS_LONGHORN" get pods \
            -l longhorn.io/component=instance-manager -o name 2>/dev/null | wc -l | tr -d ' ')
        [ "$count" -ge 1 ] && break
        sleep 10
    done
    [ "$count" -ge 1 ] || { echo "longhorn.sh: instance-manager pods never appeared (600s)" >&2; exit 1; }
    kubectl --context "${CTX}" -n "$NS_LONGHORN" wait \
        --for=condition=ready pod -l longhorn.io/component=instance-manager \
        --timeout=900s
}

longhorn_classes() {
    kubectl --context "${CTX}" apply -f - <<EOF
---
# user-shared StorageClass — the #1709 design §storage posture:
# 2 replicas default (chart-overridable to 3), prod conventions carried
# verbatim from talos-ops-prod storageclass-2r.yaml.
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: user-shared
provisioner: driver.longhorn.io
allowVolumeExpansion: true
reclaimPolicy: Delete
volumeBindingMode: Immediate
parameters:
  numberOfReplicas: "2"
  staleReplicaTimeout: "30"
  fsType: "ext4"
  dataLocality: "best-effort"
  disableRevisionCounter: "true"
  dataEngine: "v1"
---
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshotClass
metadata:
  name: user-shared-snap
driver: driver.longhorn.io
deletionPolicy: Delete
EOF
}

case "${1:-}" in
    node-prereqs)
        NODE="${2:-$(longhorn_default_node)}"
        [[ -n "${NODE}" ]] || { echo "longhorn.sh: no kind node resolved" >&2; exit 1; }
        longhorn_node_prereqs "${NODE}"
        ;;
    install)
        longhorn_install
        ;;
    classes)
        longhorn_classes
        ;;
    *)
        echo "usage: $0 node-prereqs [node-name] | install | classes" >&2
        exit 2
        ;;
esac
