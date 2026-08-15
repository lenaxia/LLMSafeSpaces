# syntax=docker/dockerfile:1
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Standalone workspace-agentd image (#863).
#
# This is the image-volume delivery artifact: the controller pins it by
# digest into every workspace pod, and the entrypoint verifies the binary
# sha256 against per-arch pins before exec. It contains ONLY the agentd
# binary — no distro, no shell, no opencode. Those stay in the workspace
# image ("the image is the workspace"); this image is platform runtime,
# delivered on platform cadence.
#
# FROM scratch keeps it minimal (~25MB/binary) so the kubelet image-volume
# pull is cheap on the resume path. The binary is static (CGO_ENABLED=0).
#
# Binary path inside the image MUST match the controller's
# agentdBinaryRelPath (/usr/local/bin/workspace-agentd) — the pod env pin
# LLMSAFESPACES_AGENTD_BINARY is /agentd + that path once mounted.
#
# NOTE: do not add anything executable to this image. The whole point of
# the sha256 pin is that one file's integrity is the contract.

FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS builder

ARG TARGETARCH
WORKDIR /src

ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}

ARG VERSION=unknown
ARG COMMIT_SHA=unknown
ARG BUILD_TIME=unknown

COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY pkg/ ./pkg/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath \
        -ldflags "-X github.com/lenaxia/llmsafespaces/pkg/version.Version=${VERSION} \
                  -X github.com/lenaxia/llmsafespaces/pkg/version.CommitSHA=${COMMIT_SHA} \
                  -X github.com/lenaxia/llmsafespaces/pkg/version.BuildTime=${BUILD_TIME}" \
        -o /out/workspace-agentd ./cmd/workspace-agentd

FROM scratch

COPY --from=builder --chmod=755 /out/workspace-agentd /usr/local/bin/workspace-agentd

# Not a runnable image by itself (no entrypoint) — it is consumed only as
# an image volume mounted read-only at /agentd in workspace pods.
