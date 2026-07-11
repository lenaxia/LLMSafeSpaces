# syntax=docker/dockerfile:1.7
# Build the API server binary.
# Build context MUST be the repository root (single Go module at root).
#
#   docker build -f api/Dockerfile -t llmsafespaces/api:dev .

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

COPY api/ api/
COPY pkg/ pkg/
COPY mocks/ mocks/
COPY cmd/ cmd/
COPY controller/ controller/

ARG VERSION=dev
ARG BUILD_TIME=unknown

ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build \
        -ldflags "-X github.com/lenaxia/llmsafespaces/pkg/version.Version=${VERSION}" \
        -o /out/api \
        ./api/cmd/api

FROM gcr.io/distroless/static:nonroot

WORKDIR /

COPY --from=builder /out/api /usr/local/bin/api
COPY api/config/config.yaml /etc/llmsafespaces/config.yaml
COPY api/migrations /etc/llmsafespaces/migrations

ENV CONFIG_PATH=/etc/llmsafespaces/config.yaml

USER 65532:65532

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/api"]
