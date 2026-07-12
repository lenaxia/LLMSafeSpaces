# syntax=docker/dockerfile:1.7
# Build the relay-router binary.
# Build context MUST be the repository root (single Go module at root).
#
#   docker build -f cmd/relay-router/Dockerfile -t llmsafespaces/relay-router:dev .
#
# Multi-arch: see api/Dockerfile for the cross-compile pattern. Do not pin
# ARG TARGETARCH to a default — buildx injects the value per-platform; a
# hardcoded default can produce a binary for the wrong arch (issue #462).

FROM --platform=$BUILDPLATFORM golang:1.25 AS builder

WORKDIR /workspace

ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY pkg/ pkg/

# TARGETARCH is injected by buildx per-platform. No hardcoded default — see #462.
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build \
        -trimpath \
        -o /out/relay-router \
        ./cmd/relay-router

FROM gcr.io/distroless/static:nonroot

WORKDIR /

COPY --from=builder /out/relay-router /usr/local/bin/relay-router

USER 65532:65532

ENTRYPOINT ["/usr/local/bin/relay-router"]
