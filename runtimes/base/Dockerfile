ARG TARGETARCH
FROM debian:bookworm-slim

ARG TARGETARCH

# Design 0053 S3: the opencode version pin lives in ONE place —
# runtimes/opencode/Dockerfile (the delivery artifact). Nothing here
# installs or versions opencode.

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

# Install GitHub CLI (gh) from upstream releases.
# Source: https://github.com/cli/cli/releases
# Asset naming: gh_{version}_linux_{arch}.tar.gz  (amd64/arm64 — matches Docker TARGETARCH).
# Pinned to 2.74.1 (June 2026) — validated against API surface used by sandbox agents.
RUN set -eux; \
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
#   (x64, NOT amd64 — upstream mise uses the x64 naming.)
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

# Design 0053 §4.3 (S3): the base is the OS. No platform-contract
# artifacts are baked — agentd, redact (an agentd subcommand), the
# entrypoints, and opencode arrive at pod time via the two digest-pinned
# overlay volumes (controller.agentdDelivery / opencodeDelivery).
# The build-time smoke test below stays: OS-content assertions only
# (design §4.3 — platform-contract assertions moved to the artifacts CI).
COPY --chmod=755 runtimes/base/tools/smoke-test.sh /usr/local/bin/smoke-test.sh

# --- Git credential consumption (#1087) ---
#
# The agentd materializer writes bound git-credential secrets to
# /sandbox-runtime/rt/git-credentials on EVERY pod boot (tmpfs, wiped on
# pod death), with $HOME/.git-credentials as the PVC-side symlink
# (cmd/workspace-agentd/init_fs.go symlink farm). Two layers make git
# CONSUME that file directly, decoupling git auth from `gh` and from
# session-harness env injection (GH_TOKEN) that suspend/resume does not
# replay:
#
#   1. /etc/gitconfig (system level) — the visible, standard layer.
#   2. GIT_CONFIG_COUNT/KEY_0/VALUE_0 (env level) — git applies env
#      config AFTER the user's ~/.gitconfig, so a `gh auth setup-git`
#      block persisted in the PVC (empty `helper =` reset + gh shim for
#      github.com) cannot clear this entry: the failing shim is skipped
#      and the store helper still supplies the materialized token.
#      Validated against git 2.39 (bookworm): a system-level helper
#      alone IS cleared by the setup-git reset; the env layer is what
#      survives it.
#
# Defense in depth — each layer covers the other's failure mode: env can
# be scrubbed or clobbered by user tooling (then /etc/gitconfig serves);
# /etc/gitconfig is defeated by the setup-git reset (then env serves).
# Both point at the absolute PVC-symlink path, immune to $HOME drift.
RUN printf '[credential]\n\thelper = store --file=/home/sandbox/.git-credentials\n' > /etc/gitconfig

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
    && for tool in python@3.12.13 node@lts rust@stable go@1.26.6; do \
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
    && printf '[tools]\npython = "3.12.13"\nnode = "lts"\nrust = "stable"\ngo = "1.26.6"\njava = "lts"\nmaven = "latest"\ngradle = "latest"\n' > /etc/mise/config.toml

# Rebuild shims into the SYSTEM data dir (/usr/local/share/mise/shims):
# baked into the image layer, on the runtime PATH (controller-injected pod
# env post-S3), and — unlike MISE_DATA_DIR under /workspace — NEVER
# shadowed by the PVC mount. The install-time `mise reshim` above ran as
# root with the default data dir (~root/.local/share/mise/shims): dead
# weight, which is why both runtime shims dirs were historically empty
# and toolchains never resolved in non-interactive shells. Runtime user
# installs are re-shimmed to $MISE_DATA_DIR/shims at boot (ensureMiseShims
# in the supervisor) — PVC tools win via PATH precedence, this dir is the
# floor. Smoke test runs with the system shims on PATH so `command -v`
# verifies real resolution, not just `mise which` discoverability.
RUN MISE_DATA_DIR=/usr/local/share/mise mise reshim \
    && PATH="/usr/local/share/mise/shims:$PATH" /usr/local/bin/smoke-test.sh

# --- Runtime environment variables ---
#
# MISE_DATA_DIR points at the PVC so everything the agent installs persists
# across suspend/resume cycles without reinstallation.
#
# Package manager homes are redirected to the PVC for the same reason.
# The PATH additions expose binaries from those homes immediately.
# Design 0053 §4.3 (S3): the runtime ENV block (MISE_DATA_DIR, cargo/
# gem/go/npm/python homes, PATH composition) moved to controller-injected
# pod env (controller/internal/workspace/platform_env.go) — it encodes
# PVC mount topology (platform contract), not OS content.

RUN useradd -u 1000 -m -s /bin/bash sandbox

# /tmp and /home/sandbox require emptyDir volumes in the pod spec (readOnlyRootFilesystem: true)
USER sandbox
WORKDIR /workspace


