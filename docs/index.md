# LLMSafeSpaces

A Kubernetes-first platform for running AI agents in isolated, persistent workspaces.

LLMSafeSpaces gives every user a Kubernetes pod running [`opencode serve`](https://opencode.ai/docs/server/) — a headless HTTP server driving an LLM agent — backed by a PVC-mounted filesystem at `/workspace`. The platform handles authentication, workspace lifecycle (activate/suspend/resume), secret injection, multi-tenant isolation, and reverse-proxies client requests to the agent.

## Why use it

- **Persistent agent workspaces.** Each workspace is a PVC-backed filesystem that survives pod restarts. Agent session history, project files, and credentials persist across suspend/resume cycles.
- **Multi-tenant isolation.** Per-workspace pods with hardened security contexts, optional gVisor kernel isolation, per-tenant resource quotas, and NetworkPolicies that block egress to RFC1918 / CGNAT / cloud-metadata ranges.
- **Encrypted secret store.** User secrets (LLM keys, SSH keys, env vars) encrypted with per-user DEKs (AES-256-GCM), keys derived via Argon2id. Platform secrets (SSO client credentials, admin API keys) are encrypted at rest under the master KEK. The platform never stores plaintext credentials.
- **Provider-agnostic.** Bring your own LLM gateway (LiteLLM, OpenAI, Anthropic, Bedrock) — workspaces bind to whatever credentials you supply.
- **Kubernetes-native.** Custom Resource Definitions for `Workspace`, `RuntimeEnvironment`, `InferenceRelay`. Deploy via Helm; reconcile via controller-runtime.
- **Supply chain hardened.** Release images are cosign-signed (keyless OIDC + Rekor transparency log), Trivy-scanned for CVEs, and SBOM is published with every release. Verify before deploying: `cosign verify ghcr.io/lenaxia/llmsafespaces/api:<version>`.
- **MCP-compatible.** The platform is an MCP server — any MCP-compatible client can drive workspaces programmatically.

## Quick links

- **[Quickstart](getting-started/quickstart.md)** — install on a local `kind` cluster in 10 minutes
- **[Installation](operator/installation.md)** — production Helm deployment
- **[Security Hardening](operator/security.md)** — threat model and configuration
- **[Helm Values Reference](reference/helm-values.md)** — every chart value documented
- **[REST API](api/rest.md)** — ~90 endpoints, JWT + API-key auth

## Architecture at a glance

```
Clients (REST / SSE / MCP)
    │
    ▼
LLMSafeSpaces API (Gin, stateless)  ──► PostgreSQL + Redis
    │ K8s API
    ▼
Controller (controller-runtime)
    │
    ▼
Workspace Pods (opencode serve :4096)
    ├─ init: credential-setup
    ├─ main: opencode serve
    └─ sidecar: workspace-agentd
```

Three CRDs in the `llmsafespaces.dev/v1` API group: `Workspace` (namespaced), `RuntimeEnvironment` (cluster), `InferenceRelay` (cluster, opt-in).

## Status

**v0.3.0** — Active development. Suitable for homelab and small-team deployments with the threat model understood. Not recommended for public multi-tenant SaaS without reviewing the [threat model](architecture/threat-model.md) and the remaining open security gaps.

## License

GNU Affero General Public License v3.0 or later ([AGPL-3.0-or-later](https://github.com/lenaxia/LLMSafeSpaces/blob/main/LICENSE)). A commercial license is available for organizations that cannot comply with the AGPL — contact `safespace@47north.lat`.
