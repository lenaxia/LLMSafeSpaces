#!/usr/bin/env bash
# entrypoint-common.sh — Boot-time secret materialization shim.
#
# As of Epic 17 G2 remediation (worklog 0078), all secret materialization
# is performed by the `workspace-agentd materialize` Go subcommand. This
# script is a thin shim that:
#
#   1. Verifies the agentd binary exists in the runtime image (a missing
#      binary is a build error, not a runtime degradation).
#   2. Invokes the materialize subcommand. The subcommand:
#       - reads /sandbox-cfg/secrets.json (no-op if absent),
#       - validates each secret entry against the threat-model invariants
#         in pkg/agentd/secrets,
#       - writes credential files with mode 0600 atomic-on-create,
#       - skips invalid entries (T5: never blocks pod boot for one bad
#         entry), reporting the rejection reason to stderr.
#
# Threat-model invariants this shim preserves:
#
#   T1 No interpretation of secret values by the shell. The materializer
#      is a Go binary; no plaintext ever passes through bash word-splitting.
#   T2 No file ever exists with mode > 0600 for credential material.
#   T5 An invalid secret skips that secret only; the rest still apply.
#
# See pkg/agentd/secrets/secrets.go for the implementation and
# pkg/agentd/secrets/secrets_test.go for the bash-subprocess regression
# corpus that locks these invariants in place.
#
# -----------------------------------------------------------------------------
# #863 agentd overlay delivery (image volume).
#
# When the controller pins an agentd image volume (AGENTD_IMAGE_VOLUME=1,
# env: LLMSAFESPACES_AGENTD_BINARY + LLMSAFESPACES_AGENTD_SHA256_<ARCH>),
# the binary at the overlay path REPLACES the baked-in one. Before exec,
# its sha256 is verified against the pod-spec pin:
#
#   - Match    → exec overlay binary.
#   - Mismatch → exit 81. NO fallback to the baked binary: the main
#                container runs arbitrary code by design, so a tampered
#                overlay must never execute and must never silently
#                downgrade. Page-worthy; controller emits
#                AgentdVerificationFailed.
#   - Missing  → exit 82 (config/rollout error: pin says overlay, volume
#                content absent).
#
# The digest pins live in the pod spec (immutable post-create,
# workspace-unwritable) — the only integrity anchor the container itself
# cannot touch.
#
# Exit codes 81/82 are consumed by the controller's
# detectAgentdVerificationFailure; do not reuse them for other failures.
# -----------------------------------------------------------------------------
set -euo pipefail

agentd_exit_verify_failed=81
agentd_exit_overlay_missing=82

verify_and_select_agentd() {
    if [[ "${AGENTD_IMAGE_VOLUME:-}" != "1" ]]; then
        if ! command -v workspace-agentd >/dev/null 2>&1; then
            echo "entrypoint-common: workspace-agentd binary missing from runtime image" >&2
            exit 1
        fi
        AGENTD_BIN="$(command -v workspace-agentd)"
        return
    fi

    local bin="${LLMSAFESPACES_AGENTD_BINARY:-/agentd/usr/local/bin/workspace-agentd}"
    local arch expected
    arch="$(uname -m)"
    case "${arch}" in
        x86_64)  expected="${LLMSAFESPACES_AGENTD_SHA256_AMD64:-}" ;;
        aarch64) expected="${LLMSAFESPACES_AGENTD_SHA256_ARM64:-}" ;;
        *)       expected="" ;;
    esac

    if [[ ! -f "${bin}" ]]; then
        echo "agentd-overlay: pinned binary missing at ${bin} (exit ${agentd_exit_overlay_missing})" | tee /dev/termination-log >&2
        exit "${agentd_exit_overlay_missing}"
    fi
    if [[ -z "${expected}" ]]; then
        echo "agentd-overlay: no sha256 pin for arch ${arch} (exit ${agentd_exit_verify_failed})" | tee /dev/termination-log >&2
        exit "${agentd_exit_verify_failed}"
    fi

    local actual
    if ! actual="$(sha256sum "${bin}" 2>/dev/null | awk '{print $1}')"; then
        echo "agentd-overlay: sha256sum failed for ${bin} (exit ${agentd_exit_verify_failed})" | tee /dev/termination-log >&2
        exit "${agentd_exit_verify_failed}"
    fi
    if [[ "${actual}" != "${expected}" ]]; then
        # The line format is parsed for the event message (expected=/got=).
        echo "AgentdVerificationFailed: expected=${expected} got=${actual} binary=${bin} node_arch=${arch}" \
            | tee /dev/termination-log >&2
        exit "${agentd_exit_verify_failed}"
    fi
    echo "agentd-overlay: verified ${bin} (sha256 ok, arch ${arch})"
    AGENTD_BIN="${bin}"
}

verify_and_select_agentd
export AGENTD_BIN

"${AGENTD_BIN}" materialize
