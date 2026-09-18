# syntax=docker/dockerfile:1
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Standalone workspace-agentd image (#863).
#
# This is the image-volume delivery artifact: the controller pins it by
# digest into every workspace pod, and the supervisor self-verifies the
# binary sha256 against the per-arch pins before exec (design 0053 —
# the deleted entrypoint's check moved into the binary). Contents: the agentd binary
# plus ONE data file — the public CA bundle (#1416, /etc/ssl/certs/
# ca-certificates.crt) for the sidecar's TLS egress. No distro, no
# shell, no opencode: those stay in the workspace image ("the image is
# the workspace"); this image is platform runtime, delivered on
# platform cadence.
#
# FROM scratch keeps it minimal (~25MB/binary + ~220KB CA roots, #1416)
# so the kubelet image-volume pull is cheap on the resume path. The
# binary is static (CGO_ENABLED=0).
#
# Binary path inside the image MUST match the controller's
# agentdBinaryRelPath (/usr/local/bin/workspace-agentd) — the pod env pin
# LLMSAFESPACES_AGENTD_BINARY is /agentd + that path once mounted.
#
# NOTE: do not add anything EXECUTABLE to this image (the CA bundle is
# 0644 data). The whole point of the sha256 pin is that one file's
# integrity — the binary — is the contract.

FROM --platform=$BUILDPLATFORM golang:1.26-bookworm@sha256:9fdc884aacc3bec89b20ffc69f4bb369c78210e3e4f600387b5128b12c199f81 AS builder

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

# #1416: public TLS roots for the sidecar's workflow http nodes (and any
# https egress) — a scratch image ships none, so every TLS fetch failed
# with "certificate signed by unknown authority". Copied from the builder
# (bookworm) at Go's default system-roots path; purely additive (in-cluster
# API traffic is plain HTTP; nothing sets SSL_CERT_FILE).
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

COPY --from=builder --chmod=755 /out/workspace-agentd /usr/local/bin/workspace-agentd

# Not runnable by itself (no entrypoint). Consumed two ways: as the
# image volume mounted read-only at /agentd in workspace pods (the
# binary), and as the agentd SIDECAR image (binary + CA bundle for TLS
# egress — http nodes, #1416).
