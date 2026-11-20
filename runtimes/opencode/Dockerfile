# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Standalone opencode image — design 0053 §4.2 (Artifact 2, S1).
#
# The second image-volume delivery artifact: the controller pins it by
# digest into every workspace pod, and the supervisor verifies the binary
# sha256 against per-arch pins before spawn (exit 83/84). It contains
# ONLY the opencode binary — no distro, no shell. The workspace base is
# the OS; this image is the validated, independently-rolled-back agent
# coordinate (design 0053 §5: independent rollback, cadence, auditable
# validation).
#
# Binary path inside the image MUST match the controller's
# opencodeBinaryRelPath (/usr/local/bin/opencode) — the pod env pin
# LLMSAFESPACES_OPENCODE_BINARY is /opencode + that path once mounted.
#
# Per-arch binary sha256s are stamped onto the image index as OCI
# annotations (dev.llmsafespaces/opencode.sha256-{amd64,arm64}) by the
# same CI job family that stamps agentd's; the controller resolves them
# at startup (controller/internal/workspace/opencode_pins.go).
#
# NOTE (amended #1469; design 0053 §4.2): the artifact's BINARY
# integrity contract is unchanged — ONE executable, one per-arch
# sha256, verified by the supervisor before spawn. Alongside the
# binary the artifact carries NON-executable platform files, each
# digest-pinned by the image digest itself (the #1416 CA-bundle
# precedent): the first-party opencode plugins under /plugins. Plugins
# are version-coupled to the binary by necessity — their hook contract
# (tool.execute.before) is a harness-internal surface, so shipping a
# plugin facing a different harness risks silent no-op instead of loud
# failure. The CI origin-plugin-pin freeze job gates that coupling on
# every bump. Never add a second EXECUTABLE; plugins are data consumed
# by the pinned binary itself.

# Override to use a different release: --build-arg OPENCODE_VERSION=<version>
# Pinned to 1.18.35 (Nov 2026) — the 1.18.x line's latest release, past the
# v1.18.16–.18 bad window. DO NOT cross into 1.19+: v1.19.5 (shipped in
# v0.44.17/.18, PR #2083) broke the legacy GET /provider + GET /config/providers
# routes (instant HTTP 499, empty body — merged upstream into /api/provider +
# /api/model), blanking the frontend model picker and zeroing agentd's
# ProviderReady/catalog consumers; production was emergency-reverted to the
# 1.18.15 overlay on 2026-11-20. Re-pin to 1.19+ ONLY after the pkg/agent/
# opencode + agentd consumers migrate to the /api/* routes and the bump
# checklist covers the model-catalog endpoints.
# 1.18-line lineage: 1.18.15 (Aug 2026) was validated against the proxy code
# in api/internal/handlers/proxy.go and session_tracker.go; 1.18.35 preserves
# every legacy/v1 HTTP route, auth scheme, SSE envelope, and config-schema
# shape LLMSafeSpaces depends on (verified empirically on the release binary:
# local/opencode-binary-contract.sh green, origin-plugin freeze legs green —
# the 2026-11-20 v0.44.19 bump validation). Contains the upstream V2
# queue-drain fixes (#40990/#40991, first landed .15) that fix our #755.
# DO NOT bump into v1.18.16–v1.18.18: that window contains a projector/DB
# change that breaks v1-format session databases (fixed only in v1.18.19 by
# upstream #42444) — existing pod session volumes would traverse the broken
# migration.
ARG OPENCODE_VERSION=1.18.35

FROM --platform=$BUILDPLATFORM debian:bookworm-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171 AS fetch

ARG TARGETARCH
ARG OPENCODE_VERSION

# bookworm-slim ships neither curl nor CA bundles — both are needed for
# the TLS fetch (the pre-S3 base installed them in its apt block).
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*

# SECURITY NOTE (unchanged from the pre-S3 base install): upstream does
# not publish .sha256 or signature files for release assets. Transport
# integrity relies on TLS to github.com plus curl's --fail flag
# rejecting non-2xx responses. For SLSA-grade verification, the correct
# path is `cosign verify-blob`, but anomalyco/opencode does not
# currently publish Sigstore attestations.
# Track upstream: https://github.com/anomalyco/opencode/issues/?q=cosign
RUN set -eux; \
    case "${TARGETARCH}" in \
        amd64) OC_ARCH=x64 ;; \
        arm64) OC_ARCH=arm64 ;; \
        *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl --fail --show-error --location --retry 5 --retry-delay 3 --retry-all-errors \
        "https://github.com/anomalyco/opencode/releases/download/v${OPENCODE_VERSION}/opencode-linux-${OC_ARCH}.tar.gz" \
        -o /tmp/opencode.tar.gz; \
    mkdir -p /out; \
    tar -xzf /tmp/opencode.tar.gz -C /out opencode; \
    chmod 0755 /out/opencode; \
    sha256sum /out/opencode

FROM scratch

COPY --from=fetch --chmod=755 /out/opencode /usr/local/bin/opencode

# Platform first-party plugins (design 0053 §4.2, #1465): the
# origin-injection plugin hooks tool.execute.before and stamps the
# calling session's ID onto llmsafespaces send_message calls. Mounted
# read-only with the image volume at /opencode/plugins; referenced by
# file:// from agent-config.json (injectPlatformAgentConfig). Data,
# not executables — see the NOTE above.
# 755, NOT 644: BuildKit applies --chmod to implicitly created parent
# directories as well as the copied entry — a 644 here strips the
# traversal bit from the auto-created /plugins and uid 1000 (opencode's
# runtime uid) cannot traverse it, so the plugin's file:// import fails
# SILENTLY (live incident 2026-09-19: every send_message degraded to
# self-declared origin). The plugin is data; the x bit on it is
# harmless. Pinned by pkg/repolint/opencode_plugins_mode_test.go.
COPY --chmod=755 runtimes/opencode/plugins/llmsafespaces-origin.js /plugins/llmsafespaces-origin.js

# Not a runnable image by itself (no entrypoint) — it is consumed only as
# an image volume mounted read-only at /opencode in workspace pods.
