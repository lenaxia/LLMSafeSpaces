# Multi-arch: see api/Dockerfile for the cross-compile pattern. The Go
# tarball URL is arch-specific (golang.org/dl/goX.linux-${TARGETARCH}.tar.gz)
# so TARGETARCH must be the runtime arch, not the builder arch. Removing
# the =amd64 default lets buildx inject the correct per-platform value
# rather than silently downloading amd64 even on arm64 hosts (#462).

FROM ghcr.io/lenaxia/llmsafespaces/base:latest

USER root

# Install Go. TARGETARCH is injected by buildx per-platform
# (linux/amd64, linux/arm64). No hardcoded default — see #462.
# Toolchain floor aligned with the platform Go line (#854): 1.20.5 was
# EOL with years of stdlib CVEs; this image COMPILES tenant code
# (USER sandbox + go on PATH), so the stdlib is the tenant-facing
# surface. Renovate's dockerfile manager keeps GO_VERSION current.
ARG TARGETARCH
ARG GO_VERSION=1.26.6
ENV GO_VERSION=${GO_VERSION}
RUN curl -sSL https://golang.org/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz | tar -C /usr/local -xz
ENV PATH="/usr/local/go/bin:${PATH}"

# GOPATH/GOPROXY are NOT pinned here: the base image redirects GOPATH to
# the PVC (/workspace/.local/share/go, base ENV ordering wins at runtime)
# so tenants keep module caches across suspend/resume, and the default
# GOPROXY/GOSUMDB give them proxy + checksum verification — the old
# GOPROXY=direct/GOSUMDB=off pre-dated the PVC migration and only served
# the (now removed) build-time package pre-installs below.

# Create the scratch GOPATH for the build-time steps only.
RUN mkdir -p /go/bin

# Tenant convenience: a couple of starter tools at current versions.
# The 2023-era pre-installed module set (gorilla/mux 1.8.0, gin 1.9.1,
# cobra 1.7.0, testify 1.8.4, gonum 0.13.0) is removed: `go install`'d
# binaries are GOOS/GOARCH/toolchain-specific artifacts living outside
# module resolution, they duplicated what tenants install themselves via
# the PVC-backed GOPATH, and their pinned versions had aged into CVE
# advisories the same way the toolchain had.
RUN for i in {1..3}; do \
        (go install golang.org/x/tools/cmd/goimports@latest && \
         go install honnef.co/go/tools/cmd/staticcheck@latest) && break || \
        if [ $i -eq 3 ]; then exit 1; fi; \
        echo "Retry $i: Waiting before retrying package installation..." && \
        sleep 5; \
    done

# Copy Go-specific security configurations
COPY --chown=root:root security/go/restricted_packages.json /etc/llmsafespaces/go/

# Copy Go security wrapper
COPY --chown=root:root tools/go-security-wrapper.go /opt/llmsafespaces/bin/go-security-wrapper.go
RUN go build -o /opt/llmsafespaces/bin/go-security-wrapper /opt/llmsafespaces/bin/go-security-wrapper.go && \
    chmod +x /opt/llmsafespaces/bin/go-security-wrapper

USER sandbox
CMD ["/opt/llmsafespaces/bin/go-security-wrapper"]
