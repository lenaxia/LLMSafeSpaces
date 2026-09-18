#!/usr/bin/env bash
# gvisor.sh — runsc provisioning for the US-70.0 delivery pool and the
# s5-overlay-validation S5.6 leg (both call this one flow — the s5
# script's former inline copy was the same permanently-dead GCS fetch
# and was removed 2026-09-15). Installs runsc + its containerd shim
# from the gVisor GitHub release bundle with sha512 verification,
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
      resolve_tag() {
        # No -L here: the tag rides the FIRST redirect — following it
        # lands on the release page and %{redirect_url} comes back
        # empty. Retried: releases/latest is itself a moving alias (the
        # flip window this whole block defends against).
        local i url tag=""
        for i in 1 2 3; do
          url=$(curl -fsS --connect-timeout 15 --max-time 60 -o /dev/null -w "%{redirect_url}" https://github.com/google/gvisor/releases/latest) && break
          echo "tag resolve: attempt $i failed — retrying in 10s" >&2
          sleep 10
        done
        tag=${url#*/tag/}
        case "$tag" in
          release-*) printf "%s\n" "$tag" ;;
          *) echo "gvisor: could not resolve latest release tag (got: ${url:-empty})" >&2; return 1 ;;
        esac
      }
      # verify_bundle <bundle-path> <sums-path> <bundle-name>: verify the
      # downloaded SHA512SUMS line against the bundle hash — the install
      # only happens after this passes.
      verify_bundle() {
        local bundle=$1 sums=$2 name=$3 EXPECTED ACTUAL
        EXPECTED=$(grep " $name\$" "$sums" | cut -d" " -f1)
        # sha512 hex is exactly 128 chars — a regex, not a glob (a
        # miscounted ? glob false-positived on a good checksum in run 8).
        [[ "$EXPECTED" =~ ^[0-9a-f]{128}$ ]] \
          || { echo "SHA512SUMS format changed upstream (got: $(cat "$sums"))"; return 1; }
        ACTUAL=$(sha512sum "$bundle" | cut -d" " -f1)
        [ "$EXPECTED" = "$ACTUAL" ] || { echo "gvisor bundle sha512 mismatch"; return 1; }
      }
      TAG=$(resolve_tag) || exit 1
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
      # bundle against its line BEFORE anything is installed.
      verify_bundle /tmp/gvisor.tar.zstd /tmp/gvisor.SHA512SUMS "$BUNDLE" || exit 1
      # The bundle carries runsc + the shim at its root, plus — since the
      # 2026-09 release shape (verified against release-20260914.0) — a
      # gvisor-bin/ directory holding the sentry sidecar runsc now
      # REQUIRES under --sidecar-usage-policy=STRICT (run 35306741294:
      # "sidecar gvisor_sentry not usable (stat /usr/local/bin/gvisor-bin/
      # gvisor_sentry: no such file or directory)"). Install all three:
      # the binaries to /usr/local/bin, the sidecar dir alongside them.
      tar --zstd -xf /tmp/gvisor.tar.zstd -C /tmp runsc containerd-shim-runsc-v1 gvisor-bin
      install -m 0755 /tmp/runsc /usr/local/bin/runsc
      install -m 0755 /tmp/containerd-shim-runsc-v1 /usr/local/bin/containerd-shim-runsc-v1
      mkdir -p /usr/local/bin/gvisor-bin
      install -m 0755 /tmp/gvisor-bin/* /usr/local/bin/gvisor-bin/
      rm -f /tmp/gvisor.tar.zstd /tmp/gvisor.SHA512SUMS /tmp/runsc /tmp/containerd-shim-runsc-v1
      rm -rf /tmp/gvisor-bin
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
