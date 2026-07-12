# Multi-arch: see api/Dockerfile for the cross-compile pattern. The Go
# tarball URL is arch-specific (golang.org/dl/goX.linux-${TARGETARCH}.tar.gz)
# so TARGETARCH must be the runtime arch, not the builder arch. Removing
# the =amd64 default lets buildx inject the correct per-platform value
# rather than silently downloading amd64 even on arm64 hosts (#462).

FROM ghcr.io/lenaxia/llmsafespaces/base:latest

USER root

# Install Go. TARGETARCH is injected by buildx per-platform
# (linux/amd64, linux/arm64). No hardcoded default — see #462.
ARG TARGETARCH
ENV GO_VERSION=1.20.5
RUN curl -sSL https://golang.org/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz | tar -C /usr/local -xz
ENV PATH="/usr/local/go/bin:${PATH}"
ENV GOPATH="/go"
ENV PATH="${GOPATH}/bin:${PATH}"
ENV GOPROXY=direct
ENV GOSUMDB=off

# Create Go directories
RUN mkdir -p ${GOPATH}/src ${GOPATH}/bin && \
    chmod -R 777 ${GOPATH}

# Install common Go packages with retries and error handling
RUN for i in {1..3}; do \
        (go install github.com/gorilla/mux@v1.8.0 && \
         go install github.com/gin-gonic/gin@v1.9.1 && \
         go install github.com/spf13/cobra@v1.7.0 && \
         go install github.com/stretchr/testify@v1.8.4 && \
         go install gonum.org/v1/gonum@v0.13.0) && break || \
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
