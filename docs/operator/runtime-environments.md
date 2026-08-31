# Runtime Environments

This page covers the `RuntimeEnvironment` CRD and the container images it maps to: what the base runtime image contains, the language-specific runtimes (Python, Node.js, Go), how to build and register custom runtime images, and the registry allow-list that governs which images workspace creators may reference. A runtime environment is the mapping from a human-friendly name (like `python-3.11`) to a container image providing the dev-OS plus a language toolchain. The agent processes themselves (opencode, workspace-agentd) are NOT in the image — they arrive as two digest-pinned image volumes (design 0053) that the controller mounts into every pod.

## On this page

- [What a RuntimeEnvironment is](#what-a-runtimeenvironment-is)
- [The base runtime image](#the-base-runtime-image)
- [Language runtimes](#language-runtimes)
- [Building custom runtime images](#building-custom-runtime-images)
- [Registering a RuntimeEnvironment](#registering-a-runtimeenvironment)
- [The registry allow-list](#the-registry-allow-list)
- [Runtime images and security](#runtime-images-and-security)
- [Air-gapped and private registry deployments](#air-gapped-and-private-registry-deployments)

---

## What a RuntimeEnvironment is

A `RuntimeEnvironment` is a cluster-scoped CRD (`llmsafespaces.dev/v1`, short name `rte`) that maps a runtime name to a container image. When a user creates a workspace with `"runtime": "python-3.11"`, the controller looks up the `RuntimeEnvironment` named `python-3.11`, resolves its image, and uses it for the workspace pod.

```yaml
apiVersion: llmsafespaces.dev/v1
kind: RuntimeEnvironment
metadata:
  name: python-3.11
spec:
  image: ghcr.io/lenaxia/llmsafespaces/base:<pinned-tag>
  description: "Base image; Python 3.11 via mise"
```

Why cluster-scoped? Runtime environments are shared infrastructure — every tenant in the cluster references the same set. Namespacing them would force duplication and drift.

### The `base` runtime

The chart seeds one `RuntimeEnvironment` by default:

```yaml
runtimeEnvironments:
  base:
    image:
      repository: ghcr.io/lenaxia/llmsafespaces/base
      tag: ""   # falls back to Chart.AppVersion
```

This resolves to `ghcr.io/lenaxia/llmsafespaces/base:<appVersion>`. The `base` runtime is what the smoke-test workspace uses (`"runtime": "base"`).

---

## The base runtime image

The base image ([`runtimes/base/Dockerfile`](https://github.com/lenaxia/LLMSafeSpaces/blob/main/runtimes/base/Dockerfile)) is built on **Debian bookworm-slim** (tag-pinned; Renovate configured to open digest-pinning PRs) and contains everything an agent needs to be useful out of the box, without any language toolchain installed yet (those come via `mise` at runtime).

### System packages

| Package | Why it's in the base image |
|---|---|
| `bash`, `ca-certificates`, `curl`, `git`, `jq`, `unzip`, `xz-utils` | Required by the opencode/mise install steps; chicken-and-egg if deferred. |
| `openssh-client` | `ssh`, `ssh-keygen`, `ssh-agent` — for `git push` over SSH from inside the sandbox. |
| `make`, `build-essential` | gcc/g++/libc-dev for native extensions (cgo, npm node-gyp, pip wheels with C deps). ~250MB but unavoidable for a credible dev environment. |
| `less` | git's default pager. Without it, `git log/diff/show` produce broken output. |
| `vim-tiny` | `git commit` (no `-m`), `git rebase -i`, conflict resolution need an editor. |
| `procps` | `ps`, `top`, `kill` — debian:slim ships without these. |
| `file`, `rsync` | File-type detection and sync scripts. |

### Bundled binaries

| Binary | Version | Verification |
|---|---|---|
| **opencode** | NOT in the image (design 0053 S3) | Delivered as the `opencodeDelivery` digest-pinned image volume (`runtimes/opencode/Dockerfile`, pinned 1.18.15); the supervisor verifies its sha256 before spawn (exit 83/84). |
| **gh** (GitHub CLI) | 2.74.1 (pinned) | Downloaded over TLS; **checksum-verified** via `checksums.txt` (G9 partial fix). |
| **AWS CLI v2** | 2.34.57 (pinned) | Full PGP verification (AWS CLI Team key). |
| **mise** | 2026.5.15 (pinned) | `MISE_GITHUB_ATTESTATIONS=1` — verifies Sigstore-backed GitHub attestations on every tool install. |
| **redact** | `workspace-agentd` subcommand (design 0053 S2) | On PATH via wrapper scripts (supervisor-written `/sandbox-runtime/bin/redact`; baked `/usr/local/bin/redact` until the S3 strip) — no standalone binary. |
| **workspace-agentd** | NOT in the image (design 0053 S3) | Delivered as the `agentdDelivery` digest-pinned image volume; self-verifies before any work (exit 81/82). |

### No entrypoint (design 0053 S3)

The entrypoint scripts are deleted. The pod spec execs the pinned agentd
binary directly (`--supervise`, or `supervise-opencode` in sidecar mode);
the supervisor self-verifies (exit 81), rebuilds mise shims, installs the
redact PATH wrapper, and owns the opencode child (verify-before-spawn,
exit 83/84). The image's ENV block is controller-injected pod env —
nothing platform-contract-shaped lives in the image.

### Security posture

The base image is built to comply with the `restricted` Pod Security profile:

- Runs as non-root (UID 1000 in the image; pod overrides to 65532).
- `readOnlyRootFilesystem` compatible (all writable paths are mounted volumes).
- No setuid binaries.

---

## Language runtimes

The repo ships ONE runtime image: `runtimes/base/`.

The legacy per-language Dockerfiles (`runtimes/{go,nodejs,python}/`) were deleted (Epic 7 / US-7.8; resurrected accidentally by a stale-branch merge in `c9c68684` and deleted again by #854 — a repolint `ForbiddenPathsCheck` now fails any future resurrection). They were never built by any pipeline: the tenant toolchains are **mise-managed in the base image**, which is also why a `RuntimeEnvironment` only ever needs to point at a `base` image tag — language stacks are installed per-workspace into the PVC-backed mise home.

### mise as the runtime manager

The base image uses [mise](https://mise.jdx.dev/) (jdx/mise) as the polyglot runtime manager. This means:

- Agents can install Python/Node/Go/etc. versions at runtime **without root** (`mise install python@3.12`).
- Every mise install verifies Sigstore-backed GitHub attestations (`MISE_GITHUB_ATTESTATIONS=1` — G19, fixed).
- The language-runtime Dockerfiles use mise at build time to pre-install a version, but users can install others at runtime.

This design means the "language runtime" is somewhat fluid — the `python-3.11` image ships Python 3.11 pre-installed for fast cold starts, but a user can `mise install python@3.13` inside any workspace if they need a different version.

---

## Building custom runtime images

To add a runtime the base/language images don't cover (Rust, Ruby, Java, a custom internal toolchain):

### 1. Write a Dockerfile extending the base

```dockerfile
# runtimes/rust/Dockerfile
FROM ghcr.io/lenaxia/llmsafespaces/base:0.5.1

# Install Rust via mise (attestation-verified)
RUN mise install rust@1.78.0 && mise global rust@1.78.0

# Pre-warm cargo registry / common crates if desired
# ...

# No ENTRYPOINT — the pod spec's overlay supervisor command owns the process
```

### 2. Build and push

```bash
docker build -f runtimes/rust/Dockerfile \
    -t ghcr.io/yourorg/llmsafespaces-rust:1.78.0 .
docker push ghcr.io/yourorg/llmsafespaces-rust:1.78.0
```

!!! important "Pin to immutable tags"
    Pin your custom runtime images to immutable tags (`sha-<commit>`, semver, or explicit version). Avoid `latest` — kubelet image-pull caching of moving tags is unreliable.

### 3. Register the RuntimeEnvironment

```yaml
apiVersion: llmsafespaces.dev/v1
kind: RuntimeEnvironment
metadata:
  name: rust-1.78
spec:
  image: ghcr.io/yourorg/llmsafespaces-rust:1.78.0
  description: "Rust 1.78 with opencode, build-essential, mise"
```

```bash
kubectl apply -f rust-runtime.yaml
```

Now workspaces can reference `"runtime": "rust-1.78"`.

---

## Registering a RuntimeEnvironment

`RuntimeEnvironment` is cluster-scoped, so it applies to all namespaces. Apply via `kubectl`:

```bash
cat <<EOF | kubectl apply -f -
apiVersion: llmsafespaces.dev/v1
kind: RuntimeEnvironment
metadata:
  name: python-3.11
spec:
  image: ghcr.io/lenaxia/llmsafespaces/base:<pinned-tag>
  description: "Base image; Python 3.11 via mise"
EOF
```

List them:

```bash
kubectl get rte
```

The validating webhook (`RuntimeEnvironment` webhook, registered when `webhooks.enabled=true`) validates the object at admission — image references must be well-formed.

### Seeding via Helm

The chart seeds the `base` runtime from the `runtimeEnvironments.base` value. To seed additional runtimes at deploy time, you currently apply them out-of-band via `kubectl`. (Bulk seeding of multiple runtimes via Helm values is a future enhancement.)

---

## The registry allow-list

The workspace validating webhook gates which container image references a workspace may use. This is defense-in-depth: even if a user has direct `kubectl` access, they cannot create a workspace pointing at an arbitrary image.

### How it works

When a workspace's `spec.runtime` contains `/` (i.e. is shaped like an explicit image reference, e.g. `ghcr.io/yourorg/custom:1.0`), the webhook checks it against `webhooks.allowedImageRegistries`:

```yaml
webhooks:
  allowedImageRegistries:
    - "ghcr.io/lenaxia/"
```

The runtime must match at least one prefix. An empty list means **only `RuntimeEnvironment`-name references are allowed** (e.g. `runtime: python-3.11`) — explicit image references are rejected entirely.

### Configuring the allow-list

```yaml
webhooks:
  allowedImageRegistries:
    - "ghcr.io/lenaxia/"        # upstream images
    - "ghcr.io/yourorg/"        # your custom runtimes
    - "registry.internal:5000/sandboxes/"  # air-gapped mirror
```

### RuntimeEnvironment-name vs image reference

There are two ways to specify a workspace's runtime:

| Form | Example | Webhook check |
|---|---|---|
| `RuntimeEnvironment` name | `runtime: python-3.11` | Must resolve to an existing `RuntimeEnvironment` CR |
| Explicit image reference | `runtime: ghcr.io/yourorg/custom:1.0` | Must match `allowedImageRegistries` prefix |

The first form is preferred — it decouples the workspace from the image tag and lets operators update the image by editing the `RuntimeEnvironment`. The second form is an escape hatch for ad-hoc images.

---

## Runtime images and security

### What the runtime image must provide

For a workspace pod to function, the runtime image must:

1. **No process contract** — never install an ENTRYPOINT or bake platform binaries; the controller execs the pinned overlay supervisor into every container. Extending the base inherits exactly this shape.
2. **Include `workspace-agentd`** — the sidecar binary. The base image includes it; custom images extending base inherit it.
3. **Include `redact`** — the secret-redaction entry point (`workspace-agentd redact`, or the wrapper the base provides). Same inheritance.
4. **Be `readOnlyRootFilesystem`-compatible** — all writable paths must be mounted volumes (`/workspace`, `/home/sandbox`, `/tmp`, `/sandbox-cfg`, `/sandbox-runtime`).

If you build a runtime image **from scratch** (not extending base), you must provide all four. This is why extending the base image is strongly recommended.

### Supply-chain integrity

| Binary in base | Verification | Gap |
|---|---|---|
| opencode artifact | TLS download, version-pinned, digest-pinned delivery | G9 (accepted) at BUILD (no upstream signatures); delivery integrity is the digest pin + supervisor sha256 verify |
| gh | TLS download, checksum-verified | G9 (partial fix) — checksums.txt verified at build |
| AWS CLI | Full PGP verification | None |
| mise | Sigstore GitHub attestations | None (G19 fixed) |
| agentd artifact (redact inside) | Built from source, digest-pinned delivery, supervisor self-verify | None |

For custom runtime images, mirror the base image's verification practices. If you install additional binaries, pin versions and verify checksums where the upstream publishes them. Release images are cosign-signed (see [Security Hardening](security.md#supply-chain-security)); admission-time verification is the remaining gap.

---

## Air-gapped and private registry deployments

For air-gapped clusters or private registries, you need to mirror the images and point the chart at them.

### Mirror the images

```bash
# Pull the upstream images
docker pull ghcr.io/lenaxia/llmsafespaces/base:0.5.1
docker pull ghcr.io/lenaxia/llmsafespaces/api:0.5.1
docker pull ghcr.io/lenaxia/llmsafespaces/controller:0.5.1

# Retag and push to your private registry
docker tag ghcr.io/lenaxia/llmsafespaces/base:0.5.1 \
    registry.internal:5000/sandboxes/base:0.5.1
docker push registry.internal:5000/sandboxes/base:0.5.1
# ... repeat for api, controller, frontend
```

### Point the chart at the private registry

```yaml
runtimeEnvironments:
  base:
    image:
      repository: registry.internal:5000/sandboxes/base
      tag: "0.5.1"

api:
  image:
    repository: registry.internal:5000/sandboxes/api
    tag: "0.5.1"
  imagePullSecrets:
    - name: registry-creds   # Secret with private registry credentials

controller:
  image:
    repository: registry.internal:5000/sandboxes/controller
    tag: "0.5.1"

webhooks:
  allowedImageRegistries:
    - "registry.internal:5000/sandboxes/"
```

### mise in air-gapped environments

The base image uses mise to install language runtimes. In an air-gapped cluster, mise cannot reach GitHub releases. Options:

- **Pre-bake runtimes into custom images** — install the language versions you need at build time (the language-runtime Dockerfiles do this).
- **Mirror the mise registry** — configure `MISE_DATA_DIR` and a local mirror of the mise tool definitions.
- **Disable on-demand installs** — set `MISE_DISABLE_TOOLS=...` to prevent runtime installs; users get only what's pre-baked.

### The free-models refresher in air-gapped

The controller's free-models refresher fetches from `models.dev`. In air-gapped deployments, point it at an internal mirror:

```yaml
controller:
  freeModelsRefresher:
    apiURL: "https://mirror.internal/models-dev/api.json"
```

When unreachable, workspace pods fall back to the in-pod relay injector (legacy behavior, ~6–8s slower cold start).

---

## Related

- [Multi-tenant Isolation](multi-tenant.md) — runtimes are cluster-scoped, shared across tenants.
- [Security Hardening](security.md) — the registry allow-list and webhook validation.
- [CRDs Reference](../reference/crds.md) — the `RuntimeEnvironment` CRD schema.
- [Helm Values Reference](../reference/helm-values.md) — `runtimeEnvironments.*`, `webhooks.allowedImageRegistries`.
