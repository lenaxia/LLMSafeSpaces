# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Standalone pi image — the second-harness overlay (design 0053 §5.4:
# "a second agent is a different image at the same mount point, not a
# redesign"; scoping report §2.3, story S1).
#
# The third image-volume delivery artifact: the controller pins it by
# digest into every workspace pod (RO mount at /pi on the workspace
# container), and the supervisor verifies the pi binary's sha256 against
# per-arch pins at boot, BEFORE any harness spawn (exit 85/86 — disjoint
# from opencode's 83/84 and agentd's 81/82 so a tri-overlay pod's failure
# attribution never crosses artifacts).
#
# TRUST RULING — hash-the-binary + digest-the-tree (the #1416 CA-bundle /
# #1469 plugin precedent, extended): pi's release artifact is a DIRECTORY
# TREE (the bun-compiled executable plus runtime assets it resolves
# relative to itself), not one file. The "one executable, one hash"
# contract (design 0053 §4.1) extends as:
#   - the BINARY at /pi/pi keeps the per-arch sha256 pin contract exactly
#     as agentd/opencode do (CI stamps it as an index annotation, the
#     supervisor verifies it before spawn — exit 85 on mismatch);
#   - every OTHER file in the tree (package.json, theme/, assets/,
#     export-html/, docs/, examples/, photon_rs_bg.wasm, native/ prebuilds)
#     is NON-executable data covered by the image digest itself — the
#     digest-pinned image volume is the transport-integrity boundary for
#     the tree, exactly as the #1416 CA bundle and #1469 plugins are data
#     inside their digest-pinned images.
#   - never add a second EXECUTABLE to this tree; assets are data
#     consumed by the pinned binary itself.
#
# TREE LAYOUT RULING: the image preserves the UPSTREAM release layout
# byte-for-byte (binary at the tree root /pi/pi, package.json adjacent)
# rather than the sketched /pi/bin/pi. bun's --compile-autoload-package-
# json resolves a package.json NEXT TO the executable (upstream's build
# enables it so extension dependencies resolve), and the native prebuilds
# live at their fixed relative path — relocating the binary inside the
# tree would silently break asset/dependency resolution. The scoping
# report's "/pi/bin/pi" was an "e.g."; the controller pins
# LLMSAFESPACES_PI_BINARY=/pi/pi (opencode_overlay.go's rel-path seam).
#
# BUILD PROVENANCE: built from the RELEASE SOURCE ARCHIVE with upstream's
# own script (./scripts/build-binaries.sh --offline-model-data ...) — the
# exact pipeline upstream's release workflow uses for the official
# standalone binaries (their "Build binaries from source archive" step),
# so a pinned source archive + pinned toolchain reproduces the delivered
# artifact with NO network model-catalog traffic (--offline-model-data
# builds against the model data HYDRATED INTO the archive at release
# time; the npm registry is still needed for npm ci — "offline" scopes to
# model data, matching upstream's usage). The source archive's sha256 is
# verified against the release's SHA256SUMS entry before the build runs
# (pi publishes checksums; opencode's overlay could not do better than
# TLS — see runtimes/opencode/Dockerfile).
#
# The final stage is FROM scratch: no distro, no shell — the workspace
# base provides the OS. pi's bun-compiled binary is dynamically linked
# (glibc); it executes against the workspace container's userland, like
# every other overlay artifact. Not a runnable image by itself (no
# entrypoint) — consumed only as an image volume mounted read-only at
# /pi in workspace pods.

# Override to use a different release: --build-arg PI_VERSION=<version>
# (and the matching PI_SOURCE_SHA256 from that release's SHA256SUMS).
# Pinned to 1.5.2 (2026-11-10) — the version the scoping report
# validated (RPC surface, session format, offline-build path). Bump only
# with the S6 drift-gate fixture refresh (REFRESH.md regime).
ARG PI_VERSION=1.5.2
ARG PI_SOURCE_SHA256=da14b72e5d674959b64fc541e4a9241ac9fcdfa243431479119d3edb598b2231

FROM node:22-bookworm-slim@sha256:e9f565d32859f0cbfb52125eafe8be39390f24dac302dc3cdf566e8ba3ab21ee AS build

ARG TARGETARCH
ARG PI_VERSION
ARG PI_SOURCE_SHA256

# Upstream's release toolchain (their build-binaries.yml): node 22 (the
# base image) + bun 1.4.4 on PATH. bookworm-slim ships neither curl nor
# unzip — both are needed for the fetches (same apt block shape as the
# opencode fetch stage). git: npm ci fallback resolution only (the
# lockfile carries no git deps — validated; kept for bump safety).
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl unzip git \
    && rm -rf /var/lib/apt/lists/*

# Fetch the release SOURCE ARCHIVE and verify its sha256 against the
# release's SHA256SUMS entry BEFORE building (stronger than the opencode
# overlay's TLS-only posture: pi publishes checksums).
RUN set -eux; \
    echo "${PI_SOURCE_SHA256}  /tmp/pi-source.tar.gz" | sha256sum -c - ; \
    curl --fail --show-error --location --retry 5 --retry-delay 3 --retry-all-errors \
        "https://github.com/earendil-works/pi/releases/download/v${PI_VERSION}/pi-${PI_VERSION}-source.tar.gz" \
        -o /tmp/pi-source.tar.gz; \
    echo "${PI_SOURCE_SHA256}  /tmp/pi-source.tar.gz" | sha256sum -c - ; \
    mkdir -p /src; \
    tar -xzf /tmp/pi-source.tar.gz -C /src

# bun 1.4.4 — the version upstream's release workflow pins. TLS to
# github releases + the versioned asset name (upstream publishes no
# per-asset signatures for setup-bun either).
RUN set -eux; \
    case "${TARGETARCH}" in \
        amd64) BUN_ARCH=x64 ;; \
        arm64) BUN_ARCH=arm64 ;; \
        *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl --fail --show-error --location --retry 5 --retry-delay 3 --retry-all-errors \
        "https://github.com/oven-sh/bun/releases/download/bun-v1.4.4/bun-linux-${BUN_ARCH}.zip" \
        -o /tmp/bun.zip; \
    mkdir -p /opt/bun /usr/local/bin; \
    unzip -q /tmp/bun.zip -d /opt/bun; \
    ln -s /opt/bun/bun-linux-${BUN_ARCH}/bun /usr/local/bin/bun; \
    bun --version

WORKDIR /src/pi-${PI_VERSION}

# Upstream's own build, verbatim: npm ci --ignore-scripts, then
# build-binaries.sh --offline-model-data --platform linux-<arch>. The
# script leaves the EXTRACTED platform tree at <out>/<platform>/ (pi
# binary at its root + runtime assets) alongside the release archive;
# /tree normalizes the platform dir away so the final COPY is at a fixed
# path (COPY arg substitution is stage-local; ARG indirection here would
# be invisible to reviewers and to the mode-pin CI check).
RUN set -eux; \
    case "${TARGETARCH}" in \
        amd64) PI_PLATFORM=linux-x64 ;; \
        arm64) PI_PLATFORM=linux-arm64 ;; \
    esac; \
    npm ci --ignore-scripts; \
    ./scripts/build-binaries.sh \
        --offline-model-data \
        --platform "${PI_PLATFORM}" \
        --out /out; \
    mkdir -p /tree; \
    cp -a "/out/${PI_PLATFORM}/." /tree/; \
    test -x /tree/pi; \
    test -f /tree/package.json; \
    test -d /tree/native; \
    sha256sum /tree/pi

FROM scratch

# The upstream release tree, unmodified: binary at /pi/pi with
# package.json, assets, and native prebuilds at their fixed relative
# paths (see the TREE LAYOUT RULING above). Plain COPY preserves the
# build stage's modes — bun emits the binary 0755 and the script's
# mkdir/cp emit 0755 dirs / 0644 data files; the uid-1000 runtime uid
# needs the other-execute traversal bit on /pi and other-read+execute on
# the binary (the 2026-09-19 /plugins incident class). The CI mode-pin
# check (build-pi) asserts exactly those bits on every build.
COPY --from=build /tree/ /pi/

# Not a runnable image by itself (no entrypoint) — it is consumed only as
# an image volume mounted read-only at /pi in workspace pods.
