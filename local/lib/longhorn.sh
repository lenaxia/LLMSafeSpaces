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
    docker exec -u 0 "$node" bash -c '
      set -e
      export DEBIAN_FRONTEND=noninteractive
      apt-get update -qq >/dev/null
      apt-get install -y -qq open-iscsi nfs-common util-linux >/dev/null
      modprobe iscsi_tcp 2>/dev/null || true
      # iscsid MUST actually run AND its IPC must be reachable — Longhorn
      # v1 engines drive the iSCSI initiator by nsenter-ing into the
      # namespaces OF ISCSID ITSELF (/host/proc/<pid>/ns/*) and running
      # iscsiadm there; iscsiadm reaches iscsid over the system bus and
      # needs /run/lock/iscsi. In a live kind node the open-iscsi deb's
      # tmpfiles never ran (/run/lock/iscsi missing) and the unit did
      # not start it (run 39097290981: "Could not open /run/lock/iscsi"
      # + "Failed to connect to system scope bus"). Finish the
      # packaging's job, start iscsid both ways, then SMOKE-TEST the
      # exact command shape the engine uses — abort loud if iscsiadm
      # still cannot reach the daemon.
      systemd-tmpfiles --create 2>/dev/null || true
      mkdir -p /run/lock/iscsi /etc/iscsi
      # open-iscsi can report installed-from-cache without its conffile
      # tree present (run 39103144189 died writing initiatorname into a
      # missing /etc/iscsi) — verify the binary, reinstall if absent.
      if ! command -v iscsid >/dev/null 2>&1; then
        echo "longhorn.sh: iscsid binary missing after install — reinstalling"
        apt-get install --reinstall -y -qq open-iscsi >/dev/null 2>&1 || apt-get install -y -qq open-iscsi >/dev/null
        command -v iscsid >/dev/null 2>&1 || { echo "longhorn.sh: open-iscsi will not install"; exit 1; }
      fi
      # initiatorname.iscsi is normally written by the deb postinst; in a
      # live container node the postinst skips it and iscsid exits at
      # once ("could not read initiatorname") — create it if missing.
      if [ ! -s /etc/iscsi/initiatorname.iscsi ]; then
        echo "InitiatorName=iqn.2026-10.org.kind.$(hostname)-spike" > /etc/iscsi/initiatorname.iscsi
        echo "longhorn.sh: wrote missing /etc/iscsi/initiatorname.iscsi"
      fi
      systemctl daemon-reload 2>/dev/null || true
      systemctl enable --now iscsid 2>/dev/null || true
      systemctl start iscsid 2>/dev/null || true
      systemctl start open-iscsi 2>/dev/null || true
      systemctl restart dbus 2>/dev/null || true
      if ! pgrep -x iscsid >/dev/null 2>&1; then
        echo "longhorn.sh: iscsid unit did not start it — launching directly"
        iscsid >/tmp/iscsid-direct.log 2>&1 || true
        sleep 1
      fi
      if ! pgrep -x iscsid >/dev/null 2>&1; then
        echo "longhorn.sh: iscsid FAILED to start — diagnostic output follows"
        systemctl status iscsid --no-pager -l 2>&1 | tail -15 || true
        cat /tmp/iscsid-direct.log 2>/dev/null || true
        echo "--- foreground attempt (5s debug run)"
        timeout 5 iscsid -f -d 4 2>&1 | tail -15 || true
        echo "longhorn.sh: Longhorn v1 engines cannot attach in this node"
        exit 1
      fi
      echo "longhorn.sh: iscsid running ($(pgrep -x iscsid | head -1))"
      # Smoke test: "can not connect to iSCSI daemon" = IPC broken;
      # anything else (e.g. no portals found / connection refused to the
      # probe target) means the daemon channel works.
      DISC=$(iscsiadm -m discovery -t sendtargets -p 127.0.0.1 2>&1 || true)
      if echo "$DISC" | grep -q "can not connect to iSCSI daemon"; then
        echo "longhorn.sh: iscsiadm cannot reach iscsid even with it running:"
        echo "$DISC"
        exit 1
      fi
      echo "longhorn.sh: iscsiadm→iscsid IPC verified"
      mkdir -p /var/lib/longhorn
      # O_DIRECT probe — Longhorn v1 engines need it, and nested
      # overlayfs (kind-in-dind is overlay-on-overlay) may not support
      # it: engines would hang in "starting" with replicas running.
      # Longhorn'"'"'s own kind CI mounts a real ext4 loopback at
      # /var/lib/longhorn for exactly this; we do the same when the
      # probe fails.
      if dd if=/dev/zero of=/var/lib/longhorn/.odirect-probe bs=1M count=1 oflag=direct 2>/dev/null; then
        echo "longhorn.sh: O_DIRECT ok on /var/lib/longhorn (no loopback needed)"
        rm -f /var/lib/longhorn/.odirect-probe
        exit 0
      fi
      rm -f /var/lib/longhorn/.odirect-probe
      echo "longhorn.sh: O_DIRECT UNSUPPORTED on overlay — building ext4 loopback at /var/lib/longhorn"
      truncate -s 100G /var/lib/longhorn.img
      mkfs.ext4 -q -F /var/lib/longhorn.img
      mount -o loop /var/lib/longhorn.img /var/lib/longhorn
      dd if=/dev/zero of=/var/lib/longhorn/.odirect-probe bs=1M count=1 oflag=direct
      rm -f /var/lib/longhorn/.odirect-probe
      echo "longhorn.sh: ext4 loopback mounted; O_DIRECT verified"
    '
}

longhorn_default_node() {
    kubectl --context "${CTX}" get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | head -1
}

longhorn_install() {
    helm repo add longhorn https://charts.longhorn.io >/dev/null 2>&1 || true
    helm repo update >/dev/null
    # The chart tarball is hosted on github release assets — run
    # 38889584710 died on a transient 500 fetching it. Retry the whole
    # upgrade with backoff. Scope note: a FAILED `--wait` leaves the
    # release pending-install and Helm 3 refuses to re-upgrade it — the
    # retry genuinely covers only the pre-install FETCH failure class,
    # which is the observed mode; anything past the fetch aborts loud.
    local i
    for i in 1 2 3; do
        if longhorn_install_once; then return 0; fi
        if [ "$i" -lt 3 ]; then
            echo "longhorn install attempt $i failed — retrying in 30s" >&2
            sleep 30
        fi
    done
    echo "longhorn install failed after 3 attempts" >&2
    return 1
}

longhorn_install_once() {
    # defaultClass=false: kind's local-path stays default; the spike
    # provisions exclusively through the user-shared class below.
    # NOTE: createDefaultDiskLabeled is NOT set (run 38866952477: with it
    # true and no node.longhorn.io/create-default-disk label, Longhorn
    # creates NO disks — replicas never schedule, RWX volumes sit at
    # "Waiting for volume share to be available" for their whole life).
    # The default (false) auto-creates a disk on every schedulable node.
    # replicaSoftAntiAffinity=true (run 38873829660): the chart default
    # DISABLES soft anti-affinity — 2 replicas of one volume hard-affine
    # to distinct nodes, which a single-node kind cluster can never
    # satisfy: replica #2 stays unscheduled, the RWX engine stays
    # degraded, ganesha crash-loops on its readiness probe
    # (/var/run/ganesha.pid never appears), and the volume goes faulted.
    # Allowing same-node co-location keeps the 2-replica write path (the
    # honest throughput shape) on one node.
    # preUpgradeChecker.jobEnabled=false (review R2: the chart's real key
    # — `preUpgradeChecker.enabled` is silently accepted by helm --set
    # and does nothing).
    helm upgrade --install longhorn longhorn/longhorn \
        --version "$LONGHORN_CHART_VERSION" \
        -n "$NS_LONGHORN" --create-namespace \
        --set defaultSettings.defaultReplicaCount=2 \
        --set defaultSettings.replicaSoftAntiAffinity=true \
        --set defaultSettings.storageMinimalAvailablePercentage=15 \
        --set persistence.defaultClass=false \
        --set preUpgradeChecker.jobEnabled=false \
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
    [ "$count" -ge 1 ] || { echo "longhorn.sh: instance-manager pods never appeared (600s)" >&2; return 1; }
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
  unmapMarkSnapChainRemoved: "ignored"
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
