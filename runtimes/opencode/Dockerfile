# syntax=docker/dockerfile:1
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
# NOTE: do not add anything executable to this image. One file, one
# sha256 — the integrity contract (design 0053 §3, agentd precedent).

# Override to use a different release: --build-arg OPENCODE_VERSION=<version>
# Pinned to 1.18.15 (Aug 2026) — validated against the proxy code in
# api/internal/handlers/proxy.go and session_tracker.go. The 1.18.10→1.18.15
# upgrade preserves every legacy/v1 HTTP route, auth scheme, SSE envelope, and
# config-schema shape LLMSafeSpaces depends on (verified by upstream source
# diff at both tags: packages/schema and packages/opencode/src/config have
# ZERO commits in the range; V1 routes untouched).
# Why 1.18.15: it is the first release containing the upstream V2 queue-drain
# fixes (#40990 order legacy message loop by time / #40991 chronological
# message boundaries) — the root cause of our #755 (V2 queue admits but never
# drains correctly; on 1.18.10 the loop re-answers a stale prior message).
# Empirically re-verified 2026-08-27 against both binaries with a local mock
# provider: 1.18.10 mis-binds queued turns, 1.18.15 drains FIFO with correct
# texts. V1-only wiring is byte-identical in behavior.
# DO NOT bump into v1.18.16–v1.18.18: that window contains a projector/DB
# change that breaks v1-format session databases (fixed only in v1.18.19 by
# upstream #42444) — existing pod session volumes would traverse the broken
# migration. Jump .15 → .19+ in one hop if a later bump is ever needed.
ARG OPENCODE_VERSION=1.18.15

FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS fetch

ARG TARGETARCH
ARG OPENCODE_VERSION

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
    curl --fail --show-error --location \
        "https://github.com/anomalyco/opencode/releases/download/v${OPENCODE_VERSION}/opencode-linux-${OC_ARCH}.tar.gz" \
        -o /tmp/opencode.tar.gz; \
    mkdir -p /out; \
    tar -xzf /tmp/opencode.tar.gz -C /out opencode; \
    chmod 0755 /out/opencode; \
    sha256sum /out/opencode

FROM scratch

COPY --from=fetch --chmod=755 /out/opencode /usr/local/bin/opencode

# Not a runnable image by itself (no entrypoint) — it is consumed only as
# an image volume mounted read-only at /opencode in workspace pods.
