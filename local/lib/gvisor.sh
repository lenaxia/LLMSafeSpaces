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
    # and the pool — verified 2026-08-31), and as of 2026-09-15 the
    # standalone GCS release binaries are gone too (release/latest
    # serves runsc but containerd-shim-runsc-v1 404s permanently —
    # three pool runs died in provisioning on it). The artifacts now
    # ship ONLY inside the GitHub release tarballs: resolve the latest
    # release tag ONCE, fetch the bundle + its SHA512SUMS from that one
    # tag (a straddled release flip must fail the checksum, never
    # install a mixed pair), verify, extract runsc + the shim.
    docker exec "$node" bash -c '
      set -e
      export DEBIAN_FRONTEND=noninteractive
      apt-get update -qq >/dev/null
      apt-get install -y -qq curl ca-certificates zstd >/dev/null
      CURL="curl -fsSL --connect-timeout 15 --max-time 300 --retry 3 --retry-delay 3"
      # No -L here: the tag rides the FIRST redirect — following it
      # lands on the release page and %{redirect_url} comes back empty.
      TAG_URL=$(curl -fsS --connect-timeout 15 --max-time 60 -o /dev/null -w "%{redirect_url}" https://github.com/google/gvisor/releases/latest)
      TAG=${TAG_URL#*/tag/}
      case "$TAG" in
        release-*) : ;;
        *) echo "gvisor: could not resolve latest release tag (got: $TAG_URL)"; exit 1 ;;
      esac
      BASE=https://github.com/google/gvisor/releases/download/$TAG
      BUNDLE=gvisor-x86_64.tar.zstd
      # Release flips briefly 404 the moving alias and plain curl
      # --retry does not cover 404 — retry the whole fetch with backoff
      # so the window rides through.
      fetch_retry() {
        local url=$1 dest=$2 i
        for i in 1 2 3 4 5; do
          if $CURL "$url" -o "$dest"; then return 0; fi
          echo "fetch: $url attempt $i failed — retrying in 15s (release flip?)" >&2
          sleep 15
        done
        echo "fetch: $url failed after 5 attempts" >&2
        return 1
      }
      fetch_retry "$BASE/$BUNDLE" /tmp/gvisor.tar.zstd
      fetch_retry "$BASE/SHA512SUMS" /tmp/gvisor.SHA512SUMS
      # gVisor publishes "<sha512>  <file>" per artifact — verify the
      # bundle against its line. An empty/short EXPECTED means the
      # checksum FORMAT changed upstream (fail with that diagnosis
      # instead of a bare mismatch).
      EXPECTED=$(grep " $BUNDLE\$" /tmp/gvisor.SHA512SUMS | cut -d" " -f1)
      # sha512 hex is exactly 128 chars — a regex, not a glob (a miscounted
      # ? glob pattern false-positived on a perfectly good checksum in run 8).
      [[ "$EXPECTED" =~ ^[0-9a-f]{128}$ ]] \
        || { echo "SHA512SUMS format changed upstream (got: $(cat /tmp/gvisor.SHA512SUMS))"; exit 1; }
      ACTUAL=$(sha512sum /tmp/gvisor.tar.zstd | cut -d" " -f1)
      [ "$EXPECTED" = "$ACTUAL" ] || { echo "gvisor bundle sha512 mismatch ($TAG)"; exit 1; }
      # The bundle carries BOTH binaries at its root (verified against
      # release-20260907.0); runsc also needs the SHIM (run 10: "runtime
      # io.containerd.runsc.v1 binary not installed containerd-shim-runsc-v1").
      tar --zstd -xf /tmp/gvisor.tar.zstd -C /tmp runsc containerd-shim-runsc-v1
      install -m 0755 /tmp/runsc /usr/local/bin/runsc
      install -m 0755 /tmp/containerd-shim-runsc-v1 /usr/local/bin/containerd-shim-runsc-v1
      rm -f /tmp/gvisor.tar.zstd /tmp/gvisor.SHA512SUMS /tmp/runsc /tmp/containerd-shim-runsc-v1
      /usr/local/bin/runsc --version >/dev/null
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
