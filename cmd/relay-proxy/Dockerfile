# syntax=docker/dockerfile:1.7
# Build the relay-proxy binary (runs on relay VMs, NOT in-cluster).
# This image is pushed to the registry so cloud-init can pull it.
#
#   docker build -f cmd/relay-proxy/Dockerfile -t llmsafespaces/relay-proxy:dev .
#
# Multi-arch: see api/Dockerfile for the cross-compile pattern. Do not pin
# ARG TARGETARCH to a default — buildx injects the value per-platform; a
# hardcoded default can produce a binary for the wrong arch (issue #462).

FROM --platform=$BUILDPLATFORM golang:1.26@sha256:3c3e25a4da13fd0478eed2df1eb35a0e667094a7124d3993a6a1d30f71c17e79 AS builder

WORKDIR /workspace

ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY pkg/ pkg/

ARG VERSION=unknown
ARG COMMIT_SHA=unknown
ARG BUILD_TIME=unknown

# TARGETARCH is injected by buildx per-platform. No hardcoded default — see #462.
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build \
        -trimpath \
        -ldflags "-X github.com/lenaxia/llmsafespaces/pkg/version.Version=${VERSION} \
                  -X github.com/lenaxia/llmsafespaces/pkg/version.CommitSHA=${COMMIT_SHA} \
                  -X github.com/lenaxia/llmsafespaces/pkg/version.BuildTime=${BUILD_TIME}" \
        -o /out/relay-proxy \
        ./cmd/relay-proxy

FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3

WORKDIR /

COPY --from=builder /out/relay-proxy /usr/local/bin/relay-proxy

USER 65532:65532

ENTRYPOINT ["/usr/local/bin/relay-proxy"]
