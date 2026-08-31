#!/usr/bin/env bash
# gvisor.sh — runsc provisioning for the US-70.0 delivery pool.
# Extracted from local/s5-overlay-validation.sh S5.6 (which keeps its own
# inline copy; do not modify the s5 script). Installs runsc + its
# containerd shim from the gVisor latest release with sha512 verification,
# registers the containerd runtime handler, restarts containerd, and
# applies the gvisor RuntimeClass (handler runsc).
#
# Usage:
#   bash local/lib/gvisor.sh install [node-name]   # default: first node
#   bash local/lib/gvisor.sh runtimeclass
#
# Environment: CLUSTER_NAME (default llmsafespaces-ci), CTX (default
# kind-$CLUSTER_NAME) — the kubectl context used for node discovery and
# the RuntimeClass apply.
set -Eeuo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-llmsafespaces-ci}"
CTX="${CTX:-kind-${CLUSTER_NAME}}"

gvisor_install_on_node() { # node
    local node="$1"
    # gVisor's apt repository is gone upstream (404s on the suite, the key,
    # and the pool — verified 2026-08-31). The direct release binaries remain:
    # download runsc + its published sha512, verify, install to /usr/local/bin.
    docker exec "$node" bash -c '
      set -e
      export DEBIAN_FRONTEND=noninteractive
      apt-get update -qq >/dev/null
      apt-get install -y -qq curl ca-certificates >/dev/null
      BASE=https://storage.googleapis.com/gvisor/releases/release/latest/x86_64
      CURL="curl -fsSL --connect-timeout 15 --max-time 180 --retry 3 --retry-delay 3"
      $CURL "$BASE/runsc" -o /tmp/runsc
      $CURL "$BASE/runsc.sha512" -o /tmp/runsc.sha512
      # gVisor publishes "<sha512>  runsc" — verify the download against it.
      # An empty/short EXPECTED means the checksum FORMAT changed upstream
      # (fail with that diagnosis instead of a bare mismatch).
      EXPECTED=$(cut -d" " -f1 /tmp/runsc.sha512)
      # sha512 hex is exactly 128 chars — a regex, not a glob (a miscounted
      # ? glob pattern false-positived on a perfectly good checksum in run 8).
      [[ "$EXPECTED" =~ ^[0-9a-f]{128}$ ]] \
        || { echo "runsc.sha512 format changed upstream (got: $(cat /tmp/runsc.sha512))"; exit 1; }
      ACTUAL=$(sha512sum /tmp/runsc | cut -d" " -f1)
      [ "$EXPECTED" = "$ACTUAL" ] || { echo "runsc sha512 mismatch"; exit 1; }
      install -m 0755 /tmp/runsc /usr/local/bin/runsc
      rm -f /tmp/runsc /tmp/runsc.sha512
      /usr/local/bin/runsc --version >/dev/null
      # containerd also needs the SHIM binary (run 10: "runtime
      # io.containerd.runsc.v1 binary not installed containerd-shim-runsc-v1").
      $CURL "$BASE/containerd-shim-runsc-v1" -o /tmp/shim
      $CURL "$BASE/containerd-shim-runsc-v1.sha512" -o /tmp/shim.sha512
      EXPECTED=$(cut -d" " -f1 /tmp/shim.sha512)
      [[ "$EXPECTED" =~ ^[0-9a-f]{128}$ ]] \
        || { echo "shim.sha512 format changed upstream (got: $(cat /tmp/shim.sha512))"; exit 1; }
      ACTUAL=$(sha512sum /tmp/shim | cut -d" " -f1)
      [ "$EXPECTED" = "$ACTUAL" ] || { echo "shim sha512 mismatch"; exit 1; }
      install -m 0755 /tmp/shim /usr/local/bin/containerd-shim-runsc-v1
      rm -f /tmp/shim /tmp/shim.sha512
      # Register the handler in containerd (config_v2 runtime table);
      # containerd resolves `runsc` from PATH (/usr/local/bin).
      CFG=/etc/containerd/config.toml
      grep -q "runsc" "$CFG" || {
        printf "\n[plugins.\"io.containerd.grpc.v1.cri\".containerd.runtimes.runsc]\n  runtime_type = \"io.containerd.runsc.v1\"\n" >> "$CFG"
      }
    '
    docker exec "$node" systemctl restart containerd >/dev/null 2>&1 || docker exec "$node" pkill -x containerd >/dev/null 2>&1 || true
    sleep 10
}

gvisor_apply_runtimeclass() {
    cat <<'EOF' | kubectl --context "${CTX}" apply -f - >/dev/null
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gvisor
handler: runsc
EOF
}

gvisor_default_node() {
    kubectl --context "${CTX}" get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | head -1
}

case "${1:-}" in
    install)
        NODE="${2:-$(gvisor_default_node)}"
        [[ -n "${NODE}" ]] || { echo "gvisor.sh: no kind node resolved" >&2; exit 1; }
        gvisor_install_on_node "${NODE}"
        ;;
    runtimeclass)
        gvisor_apply_runtimeclass
        ;;
    *)
        echo "usage: $0 install [node-name] | runtimeclass" >&2
        exit 2
        ;;
esac
