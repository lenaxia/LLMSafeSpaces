ARG TARGETARCH

FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS redact-builder

ARG TARGETARCH

WORKDIR /src

ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}

COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY pkg/ ./pkg/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -o /out/redact ./cmd/redact

FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS agentd-builder

ARG TARGETARCH

WORKDIR /src

ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}

COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY pkg/ ./pkg/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -o /out/workspace-agentd ./cmd/workspace-agentd

FROM debian:bookworm-slim

ARG TARGETARCH

# Override to use a different release: --build-arg OPENCODE_VERSION=<version>
# Pinned to 1.15.12 (May 2026) — validated against the proxy code in
# api/internal/handlers/proxy.go and session_tracker.go. See worklog 0070
# for the full v1.2.27→v1.15.12 impact analysis (15 items, all non-breaking).
# Key changes: Effect HttpApi replaces Hono, events gain "id" field, responses
# may be gzip-compressed (proxy streams transparently).
ARG OPENCODE_VERSION=1.15.12

ARG MISE_VERSION=2026.5.15

# GitHub CLI (gh) — pinned to a specific release.
# Source: https://github.com/cli/cli/releases
# Asset naming: gh_{version}_linux_{arch}.tar.gz
ARG GH_VERSION=2.74.1

# AWS CLI v2 — pinned to a specific patch release.
# Source: https://awscli.amazonaws.com/awscli-exe-linux-{x86_64,aarch64}-${VERSION}.zip
# Verification: the corresponding .sig file is PGP-signed with the AWS CLI
# Team key (fingerprint FB5D B77F D5C1 18B8 0511  ADA8 A631 0ACC 4672 475C).
# We import the key into a temporary keyring and verify before extraction.
ARG AWSCLI_VERSION=2.34.57

ENV DEBIAN_FRONTEND=noninteractive

# System packages.
#
# Why these are in the base image (not via mise or on-demand):
# - bash, ca-certificates, curl, git, jq, unzip, xz-utils — required by the
#   opencode/mise install steps below; chicken-and-egg if deferred.
# - openssh-client — `ssh`, `ssh-keygen`, `ssh-agent`. Required for `git push`
#   over SSH from inside the sandbox.
# - make, build-essential — Makefiles are ubiquitous in real projects;
#   build-essential supplies gcc/g++/libc-dev for native extensions
#   (cgo, npm node-gyp, pip wheels with C deps). ~250MB but unavoidable
#   for a credible dev environment.
# - less — git's default pager. Without it, `git log/diff/show` produce
#   broken output. Tiny.
# - vim-tiny — `git commit` (no -m), `git rebase -i`, conflict resolution
#   need an editor. vim-tiny is ~1MB; full vim is not needed.
# - procps — `ps`, `top`, `kill`. debian:slim ships without these.
# - file — file-type detection used by many build/test scripts.
# - rsync — common in deploy/sync scripts; works with ssh transport above.
# - gnupg — used immediately below to verify the AWS CLI v2 signature, and
#   generally needed for `git commit -S`, signed-tag verification, and
#   third-party apt repos that agents may want to add at runtime.
# - openssl — CLI for cert/key inspection and ad-hoc crypto. The `openssl`
#   binary is separate from `ca-certificates` (which only ships CA bundles).
# - sqlite3 — file-based DB; common as application local store and test fixture.
# - postgresql-client — `psql`, `pg_dump`. Connect to managed Postgres
#   from sandbox without installing a Python/Node driver first.
# - default-mysql-client — `mysql` client (MariaDB-based, smaller than
#   Oracle's). Same rationale as postgres.
# - zstd — Zstandard compression/decompression. Required by modern package
#   managers (apt cache, conda, npm tarballs), git's optional zstd pack
#   compression, and container image layers. Tiny (~150KB).
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash \
    build-essential \
    ca-certificates \
    curl \
    default-mysql-client \
    file \
    git \
    gnupg \
    jq \
    less \
    make \
    openssh-client \
    openssl \
    postgresql-client \
    procps \
    rsync \
    sqlite3 \
    unzip \
    vim-tiny \
    xz-utils \
    zstd \
    && apt-get upgrade -y zlib1g \
    && apt-get upgrade -y curl libcurl4 \
    && apt-get upgrade -y sqlite3 libsqlite3-0 \
    && rm -rf /var/lib/apt/lists/*

# Install opencode from upstream releases.
#
# Source: https://github.com/anomalyco/opencode/releases
# Asset naming: opencode-linux-{x64,arm64}.tar.gz
#   (Note: x64, NOT amd64, despite Docker's TARGETARCH using amd64.)
#
# SECURITY NOTE: Upstream does not publish .sha256 or signature files for
# release assets. Transport integrity relies on TLS to github.com plus curl's
# --fail flag rejecting non-2xx responses. For SLSA-grade verification, the
# correct path is `cosign verify-blob` against the Sigstore OIDC signature,
# but anomalyco/opencode does not currently publish Sigstore attestations.
# Track upstream: https://github.com/anomalyco/opencode/issues/?q=cosign
#
# When upstream adds .sha256 or Sigstore signatures, restore verification here.
RUN set -eux; \
    TARGETARCH="${TARGETARCH:-$(dpkg --print-architecture)}"; \
    case "${TARGETARCH}" in \
        amd64) OC_ARCH=x64 ;; \
        arm64) OC_ARCH=arm64 ;; \
        *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl --fail --show-error --location \
        "https://github.com/anomalyco/opencode/releases/download/v${OPENCODE_VERSION}/opencode-linux-${OC_ARCH}.tar.gz" \
        -o /tmp/opencode.tar.gz; \
    tar -xzf /tmp/opencode.tar.gz -C /usr/local/bin/ opencode; \
    chmod +x /usr/local/bin/opencode; \
    rm /tmp/opencode.tar.gz; \
    \
# Install GitHub CLI (gh) from upstream releases.
# Source: https://github.com/cli/cli/releases
# Asset naming: gh_{version}_linux_{arch}.tar.gz  (amd64/arm64 — matches Docker TARGETARCH).
# Pinned to 2.74.1 (June 2026) — validated against API surface used by sandbox agents.
    GH_VERSION="${GH_VERSION:-2.74.1}"; \
    case "${TARGETARCH}" in \
        amd64) GH_ARCH=amd64 ;; \
        arm64) GH_ARCH=arm64 ;; \
        *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1; \
    esac; \
    GH_TARBALL="gh_${GH_VERSION}_linux_${GH_ARCH}.tar.gz"; \
    curl --fail --show-error --location \
        "https://github.com/cli/cli/releases/download/v${GH_VERSION}/${GH_TARBALL}" \
        -o /tmp/gh.tar.gz; \
# G9: Verify the gh CLI checksum. GitHub publishes a consolidated
# checksums.txt per release (not per-asset .sha256). We download it,
# extract the hash for our specific tarball, and compare.
    curl --fail --show-error --location \
        "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_checksums.txt" \
        -o /tmp/gh_checksums.txt; \
    EXPECTED_HASH="$(grep "  ${GH_TARBALL}$" /tmp/gh_checksums.txt | cut -d' ' -f1)"; \
    if [ -z "${EXPECTED_HASH}" ]; then \
        echo "FATAL: gh CLI checksum not found for ${GH_TARBALL} in checksums.txt" >&2; \
        exit 1; \
    fi; \
    ACTUAL_HASH="$(sha256sum /tmp/gh.tar.gz | cut -d' ' -f1)"; \
    if [ "${EXPECTED_HASH}" != "${ACTUAL_HASH}" ]; then \
        echo "FATAL: gh CLI checksum mismatch (expected ${EXPECTED_HASH}, got ${ACTUAL_HASH})" >&2; \
        exit 1; \
    fi; \
    tar -xzf /tmp/gh.tar.gz -C /tmp/; \
    cp "/tmp/gh_${GH_VERSION}_linux_${GH_ARCH}/bin/gh" /usr/local/bin/gh; \
    chmod +x /usr/local/bin/gh; \
    rm -rf "/tmp/gh_${GH_VERSION}_linux_${GH_ARCH}" /tmp/gh.tar.gz /tmp/gh_checksums.txt

# Install mise (polyglot runtime manager) so sandbox users can install
# language runtimes (Python, Node.js, Go, etc.) without root access.
#
# Source: https://github.com/jdx/mise/releases
# Asset naming: mise-v{version}-linux-{x64,arm64}.tar.gz
#   (x64, NOT amd64 — same convention as the opencode assets above.)
RUN set -eux; \
    TARGETARCH="${TARGETARCH:-$(dpkg --print-architecture)}"; \
    case "${TARGETARCH}" in \
        amd64) MISE_ARCH=x64 ;; \
        arm64) MISE_ARCH=arm64 ;; \
        *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl --fail --show-error --location \
        "https://github.com/jdx/mise/releases/download/v${MISE_VERSION}/mise-v${MISE_VERSION}-linux-${MISE_ARCH}.tar.gz" \
        -o /tmp/mise.tar.gz; \
    tar -xzf /tmp/mise.tar.gz -C /tmp/; \
    cp /tmp/mise/bin/mise /usr/local/bin/mise; \
    chmod +x /usr/local/bin/mise; \
    rm -rf /tmp/mise /tmp/mise.tar.gz

# Install AWS CLI v2 from the official AWS-signed tarball.
#
# Source: https://awscli.amazonaws.com/awscli-exe-linux-{x86_64,aarch64}-${AWSCLI_VERSION}.zip
# Asset arch naming: x86_64 / aarch64 (NOT amd64/arm64).
#
# Integrity: the .sig file alongside the .zip is a PGP signature from the
# AWS CLI Team key (fingerprint FB5D B77F D5C1 18B8 0511 ADA8 A631 0ACC 4672 475C,
# expires 2026-07-07). The public key is shipped in the repo at
# runtimes/base/security/aws-cli-team.pgp so the build does NOT depend on a
# public keyserver — this avoids supply-chain exposure to keyserver compromise
# or build-time network reachability issues.
#
# When the AWS CLI Team rotates this key (currently expires 2026-07-07),
# replace runtimes/base/security/aws-cli-team.pgp with the new key from
# https://docs.aws.amazon.com/cli/latest/userguide/getting-started-version.html,
# update EXPECTED_FPR below, and bump AWSCLI_VERSION.
COPY runtimes/base/security/aws-cli-team.pgp /tmp/aws-cli-team.pgp
RUN set -eux; \
    TARGETARCH="${TARGETARCH:-$(dpkg --print-architecture)}"; \
    case "${TARGETARCH}" in \
        amd64) AWS_ARCH=x86_64 ;; \
        arm64) AWS_ARCH=aarch64 ;; \
        *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    GNUPGHOME="$(mktemp -d)"; export GNUPGHOME; \
    gpg --batch --import /tmp/aws-cli-team.pgp; \
    EXPECTED_FPR="FB5DB77FD5C118B80511ADA8A6310ACC4672475C"; \
    ACTUAL_FPR="$(gpg --batch --with-colons --fingerprint aws-cli@amazon.com | awk -F: '/^fpr:/ {print $10; exit}')"; \
    [ "${ACTUAL_FPR}" = "${EXPECTED_FPR}" ] || { echo "FATAL: AWS CLI key fingerprint mismatch (got ${ACTUAL_FPR})" >&2; exit 1; }; \
    curl --fail --show-error --location \
        "https://awscli.amazonaws.com/awscli-exe-linux-${AWS_ARCH}-${AWSCLI_VERSION}.zip" \
        -o /tmp/awscliv2.zip; \
    curl --fail --show-error --location \
        "https://awscli.amazonaws.com/awscli-exe-linux-${AWS_ARCH}-${AWSCLI_VERSION}.zip.sig" \
        -o /tmp/awscliv2.zip.sig; \
    gpg --batch --verify /tmp/awscliv2.zip.sig /tmp/awscliv2.zip; \
    unzip -q /tmp/awscliv2.zip -d /tmp; \
    /tmp/aws/install --bin-dir /usr/local/bin --install-dir /usr/local/aws-cli; \
    rm -rf /tmp/aws /tmp/awscliv2.zip /tmp/awscliv2.zip.sig /tmp/aws-cli-team.pgp "${GNUPGHOME}"; \
    aws --version

COPY --from=redact-builder --chmod=755 /out/redact /usr/local/bin/redact
COPY --from=agentd-builder --chmod=755 /out/workspace-agentd /usr/local/bin/workspace-agentd

COPY --chmod=755 runtimes/base/tools/entrypoints/entrypoint-common.sh  /usr/local/bin/entrypoint-common.sh
COPY --chmod=755 runtimes/base/tools/entrypoints/entrypoint-opencode.sh /usr/local/bin/entrypoint-opencode.sh

COPY --chmod=755 runtimes/base/tools/smoke-test.sh /usr/local/bin/smoke-test.sh

# Pre-install runtimes into the system mise data dir so they are baked into
# the image layer and always available regardless of PVC state.
#
# mise --system installs into /usr/local/share/mise (MISE_SYSTEM_DATA_DIR).
# At runtime, MISE_DATA_DIR points at the PVC so agent installs persist there.
# mise resolves tools by checking MISE_DATA_DIR first, then the system dir,
# so image-layer tools are the fallback and PVC tools always win.
#
# MISE_YES=1                  — non-interactive (skip trust prompts)
# MISE_GITHUB_ATTESTATIONS=1  — enforce Sigstore-backed GitHub
#                               attestation checks on every download
#                               (G19, Epic 17). Pre-fix this was 0
#                               which let mise download a malicious
#                               binary from a compromised upstream
#                               release without notice. Note: this
#                               requires the build environment to
#                               reach Sigstore + GitHub OIDC; CI
#                               runners with restricted egress need
#                               to allow-list those endpoints.
RUN mkdir -p /etc/mise \
    && ARCH=$(uname -m) \
    && echo "Building for architecture: $ARCH" \
    && for tool in python@3.12 node@lts rust@stable go@latest; do \
        echo "Installing $tool..." \
        && for attempt in 1 2 3; do \
            MISE_YES=1 MISE_GITHUB_ATTESTATIONS=1 \
            mise install --system "$tool" && break; \
            echo "Retry $attempt for $tool..." && sleep 5; \
            [ "$attempt" = "3" ] && { echo "FATAL: failed to install $tool after 3 attempts on $ARCH"; exit 1; }; \
        done; \
    done \
    && for tool in python@3.13 java@lts maven@latest gradle@latest; do \
        echo "Installing $tool..." \
        && MISE_YES=1 MISE_GITHUB_ATTESTATIONS=1 \
        mise install --system "$tool" \
        || echo "WARN: $tool failed on $ARCH, skipping (will be available via mise at runtime)"; \
    done \
    && printf '[tools]\npython = "3.12"\nnode = "lts"\nrust = "stable"\ngo = "latest"\njava = "lts"\nmaven = "latest"\ngradle = "latest"\n' > /etc/mise/config.toml \
    && mise reshim

RUN /usr/local/bin/smoke-test.sh

# --- Runtime environment variables ---
#
# MISE_DATA_DIR points at the PVC so everything the agent installs persists
# across suspend/resume cycles without reinstallation.
#
# Package manager homes are redirected to the PVC for the same reason.
# The PATH additions expose binaries from those homes immediately.
ENV MISE_DATA_DIR=/workspace/.local/share/mise \
    CARGO_HOME=/workspace/.local/share/cargo \
    GEM_HOME=/workspace/.local/share/gem \
    GOPATH=/workspace/.local/share/go \
    NPM_CONFIG_PREFIX=/workspace/.local \
    PYTHONUSERBASE=/workspace/.local \
    PATH=/workspace/.local/bin:/workspace/.local/share/go/bin:/workspace/.local/share/cargo/bin:/usr/local/share/mise/shims:$PATH

RUN useradd -u 1000 -m -s /bin/bash sandbox

# /tmp and /home/sandbox require emptyDir volumes in the pod spec (readOnlyRootFilesystem: true)
USER sandbox
WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/entrypoint-opencode.sh"]
