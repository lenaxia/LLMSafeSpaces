# CLI Tools Reference

The binaries under [`cmd/`](https://github.com/lenaxia/LLMSafeSpaces/blob/main/cmd/) in the repo. Some are platform services deployed by the Helm chart; others are operator utilities you run manually. This page covers each: what it does, its flags, and when you'd run it by hand.

| Binary | Role | Run by |
|---|---|---|
| [`api`](#api) | API server (Gin) | Helm chart (Deployment) |
| [`controller`](#controller) | Kubernetes operator | Helm chart (Deployment) |
| [`workspace-agentd`](#workspace-agentd) | Pod sidecar | Controller (pod spec) |
| [`mcp`](#mcp) | MCP server | Helm chart (Deployment) |
| [`redact`](#redact) | Secret redaction pipeline | Runtime image (PATH wrapper / direct) |
| [`repolint`](#repolint) | Repository layout linter | `.githooks/pre-commit`, CI |
| [`seal-key`](#seal-key) | Key sealing utility | Operator (manual, production setup) |
| [`rotate-kek`](#rotate-kek) | KEK rotation | Operator (manual, rotation events) |
| [`relay-router`](#relay-router) | In-cluster traffic distributor | Helm chart (opt-in, Epic 42) |
| [`relay-proxy`](#relay-proxy) | Per-VM reverse proxy | Controller (cloud-init, Epic 42) |

Build all of them with `go build ./cmd/...` or the per-binary Dockerfiles. The relay-proxy is cross-compiled via `make relay-bin` (produces `deploy/relay-proxy-{arm64,amd64}`).

---

## api

The LLMSafeSpaces API server. A Gin HTTP service: auth, ownership checks, reverse proxy to workspace pods, secrets/settings/credentials management, session tracking, SSE events, MCP server. Stateless and horizontally scalable. See [components](../architecture/components.md).

**Run by:** Helm chart (`api` Deployment, 2 replicas default).

**Manual run (dev):**

```bash
go run ./api/cmd/api
```

**Key flags / env vars** (full set in `api/config/config.yaml`):

| Flag / Env | Default | Purpose |
|---|---|---|
| `LLMSAFESPACES_MASTER_SECRET_FILE` | `/var/run/secrets/llmsafespaces/master-secret` | Master KEK file mount (US-50.1 default). Colon-separated for the rotation window; active = last ≥32-byte file. |
| `LLMSAFESPACES_MASTER_SECRET` | *unset* | Legacy master KEK env var. Deprecated opt-in (`masterSecret.deliveryMethod=env`); logs a startup warning. |
| `LLMSAFESPACES_SECURITY_ROOTKEYPROVIDER` | `""` (static) | Root key provider: `""`/`"static"`, `"sealed"`, `"aws-kms"`. |
| `LLMSAFESPACES_SECURITY_SKIPMASTERKEYWARNING` | `false` | Suppress the static-provider startup warning. Dev/CI only. |
| `LLMSAFESPACES_INTERNAL_TOKEN` | *unset* | Shared secret for controller↔API org-status calls (D20). Endpoint fails closed (403) when unset. |
| `LLMSAFESPACES_METRICS_TOKEN` | *unset* | When set, `/metrics` requires `Authorization: Bearer <token>`. |
| `LLMSAFESPACES_EMAIL_*` | *unset* | Email provider overrides (higher precedence than `api.config`). |
| `LLMSAFESPACES_OIDC_*` | *unset* | OIDC plumbing overrides. |

**Health endpoints:** `/livez` (process alive), `/readyz` (pings Postgres + Redis), `/health` (legacy alias for `/livez`).

**When you'd run it manually:** local development against a `kind` cluster. In production, the Helm chart handles everything.

---

## controller

The Kubernetes operator (controller-runtime). Reconciles `Workspace`/`RuntimeEnvironment`/`InferenceRelay` CRDs; registers validating webhooks; leader-elected.

**Run by:** Helm chart (`controller` Deployment, 1 replica + leader election).

**Manual run (dev):**

```bash
go run ./controller
```

**Key flags** (wired from Helm values):

| Flag | Helm value | Purpose |
|---|---|---|
| `--watch-namespaces` | `controller.watchNamespaces` | Comma-separated namespaces to watch. `""`/`"*"` = cluster-wide. |
| `--metrics-addr` | `controller.metricsAddr` | Metrics bind (loopback default — run `kube-rbac-proxy` for Prometheus). |
| `--health-probe-bind-address` | `controller.probeAddr` | Health probe port. |
| `--enable-inference-relay` | `controller.inferenceRelay.enabled` | Enable the InferenceRelay reconciler. Requires `rbac.scope=cluster`. |
| `--relay-router-url` | `controller.inferenceRelay.routerURL` | Router `/metrics` scrape URL. |
| `--relay-artifact-url` | `controller.inferenceRelay.artifact.urls[0]` | Relay-proxy binary mirror URL. |
| `--relay-artifact-sha256-arm64` / `-amd64` | `...artifact.sha256*` | SHA-256 for cloud-init verification. |
| `--default-runtime-class` | `gvisor.defaultRuntimeClass` (when `gvisor.enabled`) | Default RuntimeClass for workspace pods (typically `gvisor`). |
| `--api-service-url` | `controller.apiServiceURL` | In-cluster API URL for org-status polling (D20) and pod bootstrap (Epic 35). |
| `--inference-relay-secret` | `inferenceRelaySecret` | CF Worker path-segment auth secret. |

!!! note "Webhook port not configurable"
    The webhook server port is hardcoded to `9443` (`controller/main.go:174` — `webhook.NewServer(webhook.Options{Port: 9443})`). There is no `--webhook-port` flag. If you need to change it, edit `controller/main.go` and rebuild.

**Health endpoints:** `/healthz`, `/readyz` on the probe port (`:8081`).

**When you'd run it manually:** local development. Production = Helm chart.

---

## workspace-agentd

The sidecar binary that supervises `opencode` inside every workspace pod. Owns the single `AgentConfigWriter` that builds `/sandbox-runtime/agent-config.json` atomically; runs the one-shot relay injector; serves health/status/metrics and the live credential reload endpoint.

**Run by:** the controller injects it into every workspace pod spec.

**Subcommands:**

| Subcommand | Purpose |
|---|---|
| `materialize` | Reads `/sandbox-cfg/secrets.json` (server-KEK creds) + replays `/sandbox-runtime/last-reload-secrets.json` (cached user-DEK creds), then applies via `pkg/agentd/secrets`. Runs before agentd in the init container. Replaces the legacy bash secret-loop (Epic 17 G2/G20). |
| `bootstrap` | Epic 35 US-35.2: fetches decrypted secrets from the API using a projected SA token. Runs before `materialize` in the init container. Never blocks pod boot — degrades to empty on failure. |
| `--supervise` | The default long-running mode: supervise `opencode`, serve HTTP, run the relay injector. |

**Ports:**

| Port | Purpose | Auth |
|---|---|---|
| `:4096` | opencode HTTP API (the main container, not agentd) | HTTP Basic Auth (workspace password) |
| `:4097` | User port — `/v1/reload-secrets` (live credential reload) | **None (G40 open)** — should be behind `requireBearerToken` |
| `:4098` | Admin port — `/v1/healthz`, `/v1/statusz`, `/v1/readyz`, `/v1/metrics` | Token-gated |

**Key endpoints:**

- `/v1/healthz` — agentd liveness + opencode reachability. Returns build version.
- `/v1/readyz` — readiness including `RelayInjected` signal (used by the API to annotate the model catalog).
- `/v1/statusz` — sessions, disk/memory/CPU/context metrics (scraped by the controller for CRD status enrichment).
- `/v1/metrics` — Prometheus metrics (`workspace_restarts_total`, `workspace_memory_bytes`, `workspace_active_sessions`, `workspace_context_tokens`, `workspace_oom_kills_total`).
- `/v1/reload-secrets` — live credential reload without pod restart (user port, G40).

**When you'd run it manually:** never in production. For development debugging of the materialize/bootstrap subcommands against a test pod.

---

## mcp

The MCP server (Model Context Protocol). Exposes tools (`sandbox_create`, `session_create`, `session_message`, `session_history`, `sandbox_terminate`) and delegates authentication to the API. Supports stdio and SSE transports.

**Run by:** Helm chart (`mcp` Deployment) when `mcp.enabled: true`.

**Transport:** `sse` (HTTP SSE server) or `stdio` (sidecar/subprocess launched by an MCP client like Claude, VS Code).

**Auth:** the MCP server performs no authentication — it forwards the configured API key as `Authorization: Bearer` to the API.

**MCP uses `prompt_async`:** `session_message` calls `POST /session/{id}/prompt_async` (returns 204 immediately; results arrive via the `GET /event` SSE channel). The MCP server subscribes, collects the complete response, and returns it as a single tool result. This is the correct endpoint for programmatic/async callers (Persona 2).

**Flags** (from `cmd/mcp/main.go:32-36`):

| Flag | Default | Purpose |
|---|---|---|
| `--base-url` | `LLMSAFESPACES_URL` env or `http://localhost:8080` | LLMSafeSpaces API base URL. |
| `--api-key` | `LLMSAFESPACES_API_KEY` env | API key for authentication. |
| `--sse` | `false` | Use SSE transport instead of stdio. |
| `--addr` | `MCP_ADDR` env or `:3001` | SSE listen address. |
| `--timeout` | `300s` | Default timeout for `session_message`. |

**When you'd run it manually:** as a stdio subprocess launched by an MCP client, or for local testing of tool definitions.

---

## redact

A stdin → stdout secret redaction pipeline. Reads from stdin, applies the 16 built-in regex rules (plus any from a config file), writes redacted output to stdout. Built from `pkg/redact` and installed at `/usr/local/bin/redact` in every runtime image.

**Run by:** the runtime image's PATH-shadowing wrappers (high-security mode) and directly when needed. Designed to be piped: `some-command | redact`.

**Flags:**

| Flag | Default | Purpose |
|---|---|---|
| `-config` | `/sandbox-cfg/redact-patterns.json` | Path to extra patterns JSON file. |

**Built-in rules (16):** URL credentials, bearer tokens, GitHub tokens, JSON passwords, `password=`/`token=`/`secret=`/`api_key=`/`x-api-key=` assignments, PEM private keys, age keys, OpenAI/Anthropic keys (`sk-…`), AWS IAM keys (`AKIA…`), JWTs, authorization headers, long base64.

**Fail mode:** the wrappers fail-closed (exit 1) if `redact` is missing or crashes — they never emit unredacted output. The binary itself exits 1 on regex compilation failure or a read error.

**Example:**

```bash
echo 'Authorization: Bearer sk-abc123...' | redact
# → Authorization: Bearer [REDACTED]
```

**When you'd run it manually:** testing custom patterns, debugging why a specific secret did/didn't get redacted, or piping manual tool output through it during incident response.

---

## repolint

The repository-layout linter. Validates worklog numbering, CRD drift (chart YAML vs deployed), and migration drift (`api/migrations/` vs `helm/migrations/`). Run by `.githooks/pre-commit` and the CI Lint job.

**Exit codes:** `0` = pass, `1` = check failures, `2` = internal error.

**Flags:**

| Flag | Purpose |
|---|---|
| `-repo` | Repository root (default: auto-detect from CWD). |
| `-fix-drift` | Copy `api/migrations/*.sql` into `helm/migrations/` to resolve drift. |
| `-fix-worklogs` | Auto-renumber duplicate worklog files, then run all checks. |
| `-fix-worklogs-only` | Only auto-renumber; skip checks. For `.githooks/post-rewrite` where the tree may be mid-rebase. |
| `-cluster-drift` | Compare deployed CRDs on the current kubeconfig context against the chart YAMLs. **Off by default** — requires a reachable cluster, so unsuitable for pre-commit/CI without one. Run after `make helm-deploy`. |

**When you'd run it manually:** before committing (the pre-commit hook does this), after a `helm deploy` to verify CRDs landed (`-cluster-drift`), or to auto-fix migration/worklog drift (`-fix-drift`, `-fix-worklogs`).

---

## seal-key

Generates a sealed root-key file for the sealed-key provider (production self-hosted). The root key is wrapped by an Argon2id KEK derived from an operator passphrase; the on-disk sealed file is useless without the passphrase.

**Run by:** the operator, manually, during production setup.

**Flags:**

| Flag | Required | Purpose |
|---|---|---|
| `-out` | yes | Output path for the sealed key file. |
| `-passphrase` | yes (or `-passphrase-file`) | Passphrase to seal the key. |
| `-passphrase-file` | alt to above | Read passphrase from this file. |
| `-key` | no | Hex-encoded 32-byte root key. Random if omitted. |
| `-print-key` | no | **Dangerous:** print the root key to stdout. Never emitted by default — a freshly generated key lives only inside the sealed output file. |

**Sealed file format:**

- **V1 (current, US-50.11):** `magic "LSKP-S"` ‖ `salt(32)` ‖ `nonce(12)` ‖ `ciphertext`. KEK = `Argon2id(passphrase, HKDF(salt, info="llmsafespaces-sealed-root"))`.
- **V0 (legacy):** `salt(32)` ‖ `nonce(12)` ‖ `ciphertext`, plain Argon2id. Still readable for in-place upgrades.

**Example:**

```bash
# Generate a sealed key (root key never printed)
seal-key -out master.sealed -passphrase-file passphrase.txt

# Re-seal an existing key under a new passphrase (manual rotation)
seal-key -out master.new.sealed -passphrase-file new-passphrase.txt \
         -key $(cat master.key.hex)
```

**When you'd run it manually:** initial production setup (switching from static to sealed provider), and during manual KEK rotation (generate new sealed file, then run `rotate-kek`).

---

## rotate-kek

The operational KEK rotation tool (US-50.5). Re-wraps every KEK-protected row in Postgres under a new master key, then flushes the Redis DEK cache. Built on the foundation of multi-key `StaticKeyProvider` decrypt (US-50.4), `key_version` columns (US-50.3), and the rotation-aware write path (US-50.6).

**Run by:** the operator, manually, during a KEK rotation event (suspected compromise, scheduled rotation).

**Flags:**

| Flag | Required | Default | Purpose |
|---|---|---|---|
| `-old-master-file` | yes | | Path to the OLD master KEK file. |
| `-new-master-file` | yes | | Path to the NEW master KEK file. |
| `-database-url` | yes | | PostgreSQL connection string. |
| `-redis-url` | no | | Redis connection string. If empty, the DEK cache flush is skipped (the rotation still succeeds; the cache will age out naturally within ~5 minutes). |
| `-table` | no | `all` | `all`, `provider_credentials`, `api_keys`, or `org_sso_configs`. |
| `-resume-from` | no | | Resume from this row ID (per table; for interrupted runs). |
| `-target-version` | no | `2` | Target key version. |
| `-dry-run` | no | `false` | Report counts without writing. |

**What it does:**

1. Loads old + new master keys, derives old + new providers for every purpose string (`provider-credentials`, `org-credentials`, `master-kek`, `dek-cache`).
2. Connects to Postgres + Redis.
3. For each row in the target table(s): decrypt with the old provider for that row's purpose, re-encrypt with the new provider, bump `key_version`.
4. Flushes the Redis DEK cache.

**Example:**

```bash
# Dry run first
rotate-kek \
  -old-master-file /etc/llmsafespaces/old-master \
  -new-master-file /etc/llmsafespaces/new-master \
  -database-url "postgres://..." \
  -redis-url "redis://..." \
  -dry-run

# Real run
rotate-kek \
  -old-master-file /etc/llmsafespaces/old-master \
  -new-master-file /etc/llmsafespaces/new-master \
  -database-url "postgres://..." \
  -redis-url "redis://..."

# Resume an interrupted run
rotate-kek ... -resume-from row-1234 -table api_keys
```

**When you'd run it manually:** scheduled rotation, suspected KEK compromise, or after generating a new sealed-key file. See [secrets](../architecture/secrets.md) for the full rotation flow.

---

## relay-router

The in-cluster HTTP router that distributes workspace inference traffic across healthy relay VMs (Epic 42). Reads the `relay-router-peers` ConfigMap written by the controller. Weighted: AWS (1000) > OCI (100) > GCP (1); falls back to direct upstream (rate-limited) when all VMs are down.

**Run by:** Helm chart (`relay-router` Deployment, 1 replica) when `controller.inferenceRelay.enabled: true`.

**Environment variables** (flag > env > default):

| Env | Default | Purpose |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | Listen address. |
| `UPSTREAM_URL` | `https://opencode.ai/zen/v1` | Upstream the fleet proxies to. |
| `UPSTREAM_AUTH_KEY` | *unset* | Real upstream key (paid gateway). Forwarded unchanged if unset. |
| `UPSTREAM_AUTH_HEADER` | *unset* (→ `Authorization`) | Header name for the upstream key. |
| `PEER_CONFIG_PATH` | `/etc/relay-router/peers.json` | Peers ConfigMap mount. |
| `PEER_POLL_INTERVAL` | `5s` | How often to re-read peers. |
| `HEALTH_INTERVAL` / `HEALTH_TIMEOUT` / `HEALTH_THRESHOLD` | `15s` / `5s` / `3` | Per-VM health checking. |
| `MAX_429_RATE` / `DETECTION_WINDOW` / `DETECTOR_INTERVAL` | `0.5` / `5m` / `30s` | 429-storm detection. |
| `FALLBACK_RATE` / `FALLBACK_MAX_CONCURRENT` | `0.5` / `1` | Fallback mode rate limiting. |

**Auth model:** per-VM shared-secret tokens (`X-Relay-Token`, `crypto/subtle.ConstantTimeCompare`). WireGuard was removed in worklog 0447 — per-VM tokens preserve the tight blast radius. `/healthz` and `/metrics` are token-exempt.

**When you'd run it manually:** never in production (the Helm chart deploys it). For local relay-router development.

---

## relay-proxy

The reverse proxy binary run *on each relay VM*. Distributed to VMs via cloud-init with SHA-256 verification. Token-gated (`X-Relay-Token` header).

**Run by:** the controller provisions relay VMs via cloud-init, which downloads + SHA-256-verifies + starts this binary. Not containerized — it's a cross-compiled binary (`make relay-bin` → `deploy/relay-proxy-{arm64,amd64}`).

**Flags** (flag > env > default):

| Flag | Env | Default | Purpose |
|---|---|---|---|
| `-upstream` | `UPSTREAM_URL` | `https://opencode.ai/zen/v1` | LLM provider endpoint. **Load-bearing** — the controller renders `--upstream=<spec.upstreamURL>` into each VM's systemd ExecStart. |
| `-listen` | `LISTEN_ADDR` | `0.0.0.0:8080` | Listen address. |
| `-token` | `RELAY_TOKEN` | *unset* | Shared-secret token the relay-router presents. **Empty disables auth** (local dev only) — production relays must set this; without it the proxy is an open forwarder. |
| `-keepalive-interval` | `KEEPALIVE_INTERVAL` | `30s` | Interval between upstream keepalive probes. |

**`/healthz` and `/metrics` are token-exempt** — the router probes health without the per-VM token.

**When you'd run it manually:** never in production (cloud-init handles it). For local relay-VM testing or debugging cloud-init templates.

---

## Build commands

```bash
# All Go binaries
go build ./cmd/...

# Container images (api, controller, base, frontend, relay-router)
docker build -f api/Dockerfile -t llmsafespaces/api:dev .
docker build -f controller/Dockerfile -t llmsafespaces/controller:dev .
docker build -f runtimes/base/Dockerfile -t llmsafespaces/runtime-base:dev runtimes/base
docker build -f frontend/Dockerfile -t llmsafespaces/frontend:dev frontend
docker build -f cmd/relay-router/Dockerfile -t llmsafespaces/relay-router:dev cmd/relay-router

# relay-proxy cross-compilation (NOT containerized — for cloud-init distribution)
make relay-bin   # → deploy/relay-proxy-{arm64,amd64}
sha256sum deploy/relay-proxy-arm64 deploy/relay-proxy-amd64
```

CI builds and pushes to `ghcr.io/lenaxia/llmsafespaces/{api,controller,base,frontend}:dev` on every push to `main`.
