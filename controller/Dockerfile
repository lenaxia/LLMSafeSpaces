# syntax=docker/dockerfile:1.7
# Build the controller manager binary.
# Build context MUST be the repository root (single Go module at root).
#
#   docker build -f controller/Dockerfile -t llmsafespaces/controller:dev .

ARG TARGETARCH=amd64

FROM golang:1.25 AS builder

WORKDIR /workspace

# Allow operators to override the Go module proxy at build time. Useful in
# environments where proxy.golang.org is unreachable (e.g. some corporate
# VPNs); set --build-arg GOPROXY=direct to fetch modules directly.
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}

COPY go.mod go.sum ./
RUN go mod download

COPY controller/ controller/
COPY pkg/ pkg/
COPY mocks/ mocks/
COPY api/ api/
COPY cmd/ cmd/

ARG VERSION=dev

ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build \
        -trimpath \
        -ldflags "-X github.com/lenaxia/llmsafespaces/pkg/version.Version=${VERSION}" \
        -o /out/manager \
        ./controller

FROM gcr.io/distroless/static:nonroot

WORKDIR /

COPY --from=builder /out/manager /usr/local/bin/manager

USER 65532:65532

ENTRYPOINT ["/usr/local/bin/manager"]
