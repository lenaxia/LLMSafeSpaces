# Epic 61: Credential-Proxy Injection

**Status:** Proposed — Pending U1/U4 Spike (see [Spikes Required Before Story Detail](#spikes-required-before-story-detail))
**Depends On:** Epic 30 (Unified Credential Model — `provider_credentials`), Epic 50 (Master KEK file delivery, US-50.1 shipped)
**Composes With:** Epic 35 (Secretless Credential Injection) — see [Relationship to Epic 35](#relationship-to-epic-35)
**Cross-Project Reference:** Inspired by sandboxd `internal/authproxy` (reviewed 2026-07-19). sandboxd ships the same property for one-shot `opencode run` tasks; this epic adapts it to persistent `opencode serve`.
**Estimated Effort:** ~10–14 stories, 4–6 weeks (refine after spike)

---

## Problem Being Solved

LLMSafeSpaces writes decrypted LLM-provider credentials into the workspace pod's tmpfs (`/sandbox-runtime/agent-config.json`, `secrets-env`, `auth.json`) on every boot and every credential reload. This is the root cause of the entire "Relay Config Subsystem" complexity documented in `README-LLM.md`:

- The single-writer `AgentConfigWriter` (`cmd/workspace-agentd/agent_config_writer.go`, US-46.10) exists because providers, model, and relay all write `agent-config.json` from independent code paths
- The reload-replay cache `last-reload-secrets.json` (#443) exists because `Materializer.reset()` would otherwise wipe user-DEK provider credentials on container restart
- The 20s stale `relayInjected` readyz window exists because provider state is read back from disk through layered caches
- US-35.7 tmpfs-credential-paths exists because plaintext credentials on the PVC at rest were unacceptable

**Epic 35's own README (line 479)** declares the residual risk fundamental:

> "an attacker with RCE in the running workspace can exfiltrate keys from process memory. This is inherent to any system where the key must be usable by the code — it cannot be solved by file-path redirection or a same-pod proxy."

This claim is **incorrect**. The sandboxd codebase ships a counterexample: a control-plane-side reverse proxy (`internal/authproxy`) where the workspace agent holds only a dummy key and a base URL, and the proxy swaps the real `Authorization` / `x-api-key` header on the wire. The real credential is never on the sandbox filesystem, never in the agent process, never in pod environment. An attacker with RCE in the workspace can **abuse** the credential (spend the user's money through the proxy — bounded by Epic 12 metering) but cannot **exfiltrate** it (take it elsewhere, sell it, reuse outside the platform).

The threat-model upgrade is meaningful: exfiltration → abuse. Abuse is bounded by spend limits and egress controls (which Epic 35 correctly identifies as the right lever for the abuse threat). Exfiltration is unbounded — a stolen Anthropic key works anywhere.

---

## Relationship to Epic 35

**Compose, do not supersede.** The two epics eliminate different attack vectors:

| Vector | Epic 35 (Secretless) | Epic 61 (Proxy) |
|---|---|---|
| K8s Secret in etcd during boot window | ✅ Eliminated (projected SA token + TokenReview) | Unchanged |
| Plaintext on PVC at rest | ✅ Eliminated (US-35.7 tmpfs) | Unchanged |
| Plaintext in `agent-config.json` on tmpfs | ❌ Present | ✅ Eliminated for LLM-provider creds |
| Plaintext in agent process memory | ❌ "fundamental constraint" (Epic 35 line 479) | ✅ Eliminated for LLM-provider creds |
| Reload-replay cache after restart | ❌ Required (#443) | ✅ Eliminated for LLM-provider creds |

Epic 35 remains valuable for SSH keys, git credentials, and env-secrets — none of which fit the HTTP-proxy model. **Epic 61 is scoped to LLM-provider credentials only** (Anthropic, OpenAI, Zen, custom OpenAI-compatible endpoints). The Materializer's non-HTTP secret types continue through Epic 35's pipeline unchanged.

**Ordering:** Epic 61 can proceed independently of Epic 35. If both ship, the workspace pod ends up with: no K8s Secret during boot (35), tmpfs-only non-HTTP creds (35.7), and zero LLM-provider creds anywhere reachable by the agent process (61).

---

## Validated Assumptions

Each claim is tied to a concrete file:line in the current codebase. Re-verify at story-implementation time.

| # | Assumption | Verified At | Result |
|---|---|---|---|
| A1 | opencode supports custom OpenAI-compatible providers via `agent-config.json` with arbitrary `baseURL` + `apiKey` | sandboxd `control-plane/cmd/runtimed/opencode.go:276-288`; LLMSafeSpaces `cmd/workspace-agentd/agent_config_writer.go:377-414` already builds this shape for `opencode-relay` | Confirmed |
| A2 | opencode's Bun runtime rejects bare single-label hostnames in `baseURL` (`fetch() URL is invalid`) — must pass a resolved IP literal | sandboxd `control-plane/cmd/runtimed/opencode.go:230-248` (`ipBaseURL`) | Confirmed — applies to any in-cluster Service DNS; we must resolve to an IP at config-write time |
| A3 | Provider credentials are stored in PostgreSQL `provider_credentials` (Epic 30, owner_type user/org/admin) and resolved by `CredentialProvisioner` at workspace boot | `api/internal/services/workspace/workspace_service.go:271-276` (per Epic 30 stories README); `pg_credential_store.go:81-138` (`SeedWorkspaceCredentials`) | Confirmed |
| A4 | Master KEK is delivered as a read-only file mount at `/var/run/secrets/llmsafespaces/master-secret` (US-50.1 shipped), read via `LLMSAFESPACE_MASTER_SECRET_FILE` | `README-LLM.md` §"Master KEK (server root key)" | Confirmed — same delivery mechanism reusable by the proxy |
| A5 | Workspace → API service NetworkPolicy egress rule already exists (added by Epic 26 for the CF Worker relay, retained after Epic 60) | `charts/.../workspace-network-policy.yaml:109-118` (per Epic 35 A7) | Confirmed — proxy Service reuses the same pattern |
| A6 | The `opencode-relay` provider entry already uses `apiKey: "public"` — a non-secret placeholder — proving opencode accepts dummy keys in provider config | `cmd/workspace-agentd/agent_config_writer.go:410` | Confirmed |
| A7 | `opencode serve` does NOT hot-reload `agent-config.json` — credential changes today require `proc.restart()` via `makeSessionAwareRestartDecision` | `README-LLM.md` §"opencode config loading order"; `cmd/workspace-agentd/secrets.go:837` | Confirmed — proxy eliminates this restart for LLM-credential changes |
| A8 | The `CredentialProvisioner` interface is on `workspace.Service` and the SecretInjector pattern is well-established | Epic 35 A22 (`workspace_service.go:128-135`); Epic 30 README | Confirmed — same injection seam reusable |
| A9 | Per-workspace ServiceAccount creation pattern is established (Epic 35 US-35.1 — `workspace-<name>` SA with OwnerRef) | Epic 35 US-35.1 spec | Confirmed — pattern reusable for the proxy audience |

---

## Assumptions Requiring Spikes

These four assumptions are mechanically load-bearing for the design but have not been validated against `opencode serve` in persistent mode. They are the gate for story-level commitment.

| # | Assumption | Why It Matters | Spike |
|---|---|---|---|
| U1 | opencode correctly routes **streaming SSE responses** when the upstream is an openai-compatible custom provider pointing at our proxy | sandboxd uses one-shot `opencode run`; LLMSafeSpaces uses persistent `opencode serve` with interactive streaming. If streaming breaks, the design is dead. | Stand up minimal proxy + opencode serve + Anthropic-compatible mock upstream; verify streamed tokens reach the client |
| U2 | `pkg/agent/opencode/FormatOpenCodeConfig` can emit a single "proxy" provider entry with the dummy key + per-workspace baseURL without further changes | If format changes are required, scope grows | Read the formatter; attempt the render in a test harness |
| U3 | Custom-endpoint enrichment (`cmd/workspace-agentd/model_enricher.go`) currently fetches `/models` directly from the provider using decrypted credentials. Under the proxy, `/models` requests also need cred injection | If enrichment cannot route through the proxy, two cred paths coexist | Verify whether the proxy can serve `/models` for upstreams that support it; otherwise enrichment must run proxy-side |
| U4 | Provider-specific auth schemes (Anthropic `x-api-key`, OpenAI `Authorization: Bearer`, Zen `Bearer`, custom passthrough) are bounded and enumerable | If arbitrary auth schemes exist, the proxy's header-injection logic becomes a research problem | Enumerate `provider_credentials` rows in a representative install; confirm the auth-scheme set is small |

---

## Solution Overview

A new control-plane service — **`llm-credential-proxy`** — sits between workspace pods and upstream LLM providers. Workspaces receive a dummy key and a per-workspace proxy baseURL in `agent-config.json`. The proxy validates workspace identity via TokenReview, resolves the workspace's bound credentials from PostgreSQL (decrypted via the master KEK), and injects the real auth header on the wire.

```
┌─────────────────────────────────────────────────────────────────────────┐
│ Control Plane                                                           │
│                                                                         │
│   ┌────────────┐         ┌──────────────────────┐         ┌──────────┐ │
│   │ API server │         │ llm-credential-proxy │         │postgres  │ │
│   │ (existing) │         │  (NEW — this epic)   │◄──KEK───┤provider_ │ │
│   └────────────┘         │  stateless, ≥3 repl  │  decrypt │credentials│ │
│                          │                      │         └──────────┘ │
│                          │  TokenReview cache   │                       │
│                          │  (60s TTL)           │                       │
│                          └──────────┬───────────┘                       │
└─────────────────────────────────────┼───────────────────────────────────┘
                                      │ inject Authorization / x-api-key
                                      ▼
                           ┌─────────────────────┐
                           │ Upstream providers  │
                           │ (Anthropic, OpenAI, │
                           │  Zen, custom)       │
                           └─────────────────────┘
                                      ▲
                                      │ dummy key + workspace-scoped baseURL
┌─────────────────────────────────────┼───────────────────────────────────┐
│ Workspace Pod                                                           │
│   ┌──────────────────────────────────────────────────────────────────┐  │
│   │ opencode serve :4096                                             │  │
│   │  agent-config.json:                                              │  │
│   │    provider.proxy.options.baseURL = http://<ip>/v1/workspaces/   │  │
│   │                                      <id>/upstreams/openai       │  │
│   │    provider.proxy.options.apiKey   = "llmsafespace-dummy"        │  │
│   │    model = "proxy/<real-model-id>"                               │  │
│   │                                                                  │  │
│   │  PROCESS NEVER HOLDS REAL LLM CREDENTIAL                         │  │
│   └──────────────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────────────┘
```

### Auth model: per-workspace projected SA token

Each workspace gets a short-lived projected SA token (audience `llmsafespace-llm-proxy`) mounted into the **main** container at `/var/run/llm-proxy/token`. The proxy validates via TokenReview on each request (cached, 60s TTL) and extracts `workspace-<id>` from the SA username (`system:serviceaccount:<ns>:workspace-<id>`).

- Compromised workspace A's token cannot authenticate as workspace B — K8s SA token semantics (same boundary Epic 35 US-35.3 relies on for pod-bootstrap).
- Token rotates automatically (projected volume; kubelet refreshes at ~80% of TTL).
- No static per-workspace secret to manage — token issuance is implicit via SA binding.

**G17 preservation:** this is a projected volume mount on the main container — distinct from Epic 35's init-only mount. Required because `opencode serve` makes upstream calls throughout the pod's lifetime, not just at boot. Security review must accept this; the projected token is workspace-scoped, audience-bound, short-lived, and reveals nothing useful beyond the ability to call the proxy as that workspace (which the workspace can already do).

### Provider routing

```
POST http://llm-credential-proxy/v1/workspaces/<id>/upstreams/anthropic/v1/messages
POST http://llm-credential-proxy/v1/workspaces/<id>/upstreams/openai/v1/chat/completions
POST http://llmafespaces-proxy/v1/workspaces/<id>/upstreams/zen/v1/chat/completions
POST http://llm-credential-proxy/v1/workspaces/<id>/upstreams/custom/<provider-slug>/v1/chat/completions
```

The proxy resolves `<id>` → workspace → bound credentials (Epic 30 priority order: user → org → admin), picks the credential for `<upstream>`, injects the appropriate header, and proxies with streaming passthrough. The `<id>` in the path is informational — the TokenReview-extracted SA identity is authoritative; the path value MUST match or the request is rejected (defense-in-depth against path manipulation).

### What `agent-config.json` becomes

After Epic 61, `agent-config.json` for an LLM-configured workspace contains:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "proxy": {
      "name": "LLMSafeSpaces Credential Proxy",
      "npm": "@ai-sdk/openai-compatible",
      "options": {
        "baseURL": "http://<resolved-ip>/v1/workspaces/<id>/upstreams/openai",
        "apiKey": "llmsafespace-dummy"
      },
      "models": { "<model-id>": { "name": "<model-id>" } }
    }
  },
  "model": "proxy/<model-id>"
}
```

No real credentials. The `AgentConfigWriter`'s providers source becomes a **constant** for any workspace — derived from workspace ID + token mount path, not from decrypted credentials. The entire `setProviders(formatted)` + `FormatOpenCodeConfig` path is replaced by a single static-render function.

---

## Stories (Scoping Level)

Story-level detail (Files / Logic / Acceptance criteria / Tests) is deferred until the U1/U4 spike completes. Per `README-LLM.md` [§7 Assumptions: State, Then Validate](#) and [§4 Not over-engineered](#), committing story-level specs against unvalidated assumptions violates engineering principles.

| Story | Goal | Notes |
|---|---|---|
| US-61.1 | Proxy service skeleton: new `cmd/llm-credential-proxy` binary, Helm Deployment (≥3 replicas), Service, basic HTTP server | Stateless; horizontally scalable (matches API server principle) |
| US-61.2 | Master-KEK bootstrap in proxy: read KEK from file mount (Epic 50 delivery), decrypt `provider_credentials` from PostgreSQL on demand | Reuses Epic 50 root-of-trust; no new key material |
| US-61.3 | Workspace auth: TokenReview + SA name → workspaceID extraction; 60s validation cache | Same pattern as Epic 35 US-35.3 pod-bootstrap endpoint |
| US-61.4 | Provider routing & header injection: per-upstream auth-scheme map (Anthropic `x-api-key`, OpenAI `Bearer`, Zen `Bearer`, custom passthrough) | Resolves [U4](#assumptions-requiring-spikes) |
| US-61.5 | SSE / streaming passthrough: flushed `ResponseWriter`, no buffering, correct `content-type`, supports Anthropic + OpenAI event shapes | Validates [U1](#assumptions-requiring-spikes); spike first |
| US-61.6 | Controller: project SA token into main container at `/var/run/llm-proxy/token`; RBAC for new audience; new audience-bound SA per workspace | Mirrors Epic 35 US-35.4 projected volume pattern, but on main container (required for opencode's lifetime) |
| US-61.7 | agentd: `AgentConfigWriter` gains a `setProxySource(workspaceID, proxyBaseURL)` method that replaces `setProviders`. New workspaces write only the proxy provider. | The "providers source" becomes a static render; deletes `FormatOpenCodeConfig` from agentd's hot path |
| US-61.8 | Live credential reload: `POST /v1/reload-secrets` becomes a no-op for `llm-provider` type (creds are resolved at the proxy on next request). SSH/git/env still materialized. | Eliminates the reload-replay cache (#443) for LLM providers; eliminates session-aware restart on LLM cred change (US-44 wiring no longer fires) |
| US-61.9 | Feature flag: `credentialProxy.enabled` (default off). Per-workspace migration via controller annotation. Old path retained until cut-over. | Helm value, controller flag, API setting |
| US-61.10 | NetworkPolicy updates: workspace egress to proxy Service (port 8080 or similar); proxy egress to upstream providers | Extends `workspace-network-policy.yaml`; new `proxy-network-policy.yaml` |
| US-61.11 | Deletions: remove `Materializer.FormatProviders` write path for `llm-provider` secrets, remove reload-replay cache for `llm-provider` secrets, simplify `AgentConfigWriter` provider source to a static-render function | The payoff. Each deletion needs careful audit of test coverage. **Runs AFTER US-61.9 cut-over completes** |
| US-61.12 | Observability: per-workspace request counter, per-upstream latency histogram, credential-resolution-error counter. **Never log headers or request bodies.** | Follow Epic 24/33 metrics patterns; automated secret-detection CI check on proxy code |
| US-61.13 | Security regression tests: adversarial probe that runs `find / -name '*key*'`-style enumeration against a migrated workspace and asserts zero plaintext LLM credentials reachable | Red-team-style probe. Locks the property |
| US-61.14 | Documentation: rewrite `README-LLM.md` §"Relay Config Subsystem", new top-level design doc `design/0046_credential_proxy.md`, update `ARCHITECTURE.md` diagram | The complexity story inverts — most of "Known design fragilities" disappears |

---

## What Gets Deleted (the payoff)

Run during US-61.11, after migration completes. Each item is complexity this epic **eliminates**, not complexity it adds.

| Deleted component | Reason it existed | Why it's now dead |
|---|---|---|
| `Materializer.FormatProviders()` write path for `llm-provider` secrets | Wrote real creds to `agent-config.json` | Proxy makes this unnecessary; provider entry is a static render |
| Reload-replay cache (`last-reload-secrets.json`) for `llm-provider` secrets (#443) | Restored user-DEK LLM creds after container restart | No LLM creds on sandbox to restore |
| `setProviders` source in `AgentConfigWriter` | Held decrypted provider map as a writer source | Replaced by static proxy-source render |
| Boot-time `FlushProviders` call in `materialize` subcommand (LLM-provider path only) | Wrote initial providers to config at boot | Replaced by static proxy-source render |
| Session-aware restart on `llm-provider` credential change (US-44 wiring) | opencode needed restart to read new creds from disk | Proxy resolves new creds on next request; no restart |
| Stale `relayInjected` readyz window (cosmetic; partial) | Model cache + `providerCache` TTLs compound | Partially dissolves — relay config is now a static render too |
| Most of `README-LLM.md` §"Relay Config Subsystem" Known Fragilities list | Documented the fragility of multi-writer credential files | Most items evaporate (the `AgentConfigWriter` shrinks from 415 lines to a small struct with a model + relay source and a constant proxy-source renderer) |

**Net effect:** this epic deletes more complexity than it adds. The new proxy service (~1000–1500 LoC estimated) replaces a sprawling subsystem whose documented fragilities have generated at least four dedicated worklogs (US-46.10, #443, #483, the four-writer consolidation).

---

## Implementation Order

```
1. Spike U1 (streaming) + U4 (auth schemes)
   └─ Gate: do not proceed to story detail until both pass

2. Write all tests first — must fail before implementation (README-LLM.md §0)

3. US-61.1  — Proxy service skeleton (cmd/llm-credential-proxy, Helm Deployment)
4. US-61.2  — Master-KEK bootstrap + PostgreSQL credential decrypt
5. US-61.3  — Workspace auth (TokenReview + SA name extraction)
6. US-61.4  — Provider routing & header injection
7. US-61.5  — SSE / streaming passthrough
   └─ End of proxy-side stories. Proxy is feature-complete behind a flag.

8. US-61.6  — Controller: project SA token into main container
9. US-61.10 — NetworkPolicy updates
10. US-61.7 — agentd: AgentConfigWriter setProxySource
11. US-61.8 — Live credential reload becomes no-op for llm-provider
12. US-61.9 — Feature flag + per-workspace migration annotation
    └─ Cut-over: first workspace migrated to proxy path

13. US-61.11 — Deletions (FormatProviders write path, reload-replay cache, etc.)
14. US-61.12 — Observability (metrics + secret-detection CI)
15. US-61.13 — Security regression tests (adversarial probe)
16. US-61.14 — Documentation rewrite

17. Run all tests:
    cd cmd/llm-credential-proxy && go test ./... -timeout 120s -race
    cd cmd/workspace-agentd     && go test ./... -timeout 120s -race
    cd api                      && go test ./... -timeout 120s -race
    cd controller               && go test ./... -timeout 120s -race

18. Adversarial self-review (README-LLM.md §11)
19. Security review (Epic 17 methodology)
```

Implementation worklogs will be numbered sequentially under `worklogs/` at story start.

---

## Non-Requirements (Explicitly Out of Scope)

| Item | Rationale |
|---|---|
| SSH keys, git credentials, env-secrets via the proxy | Not HTTP — don't fit the proxy model. Continue through Epic 35. |
| The inference relay fleet (Epic 42) | Different problem (free-tier IP rotation, not credential hiding). Could compose later: proxy → relay-router → relay VM → Zen. Out of scope here. |
| Claude OAuth subscription tokens | Already behind a control-plane proxy in spirit (sandboxd `docs/agent-auth.md` parallel). Decide in U4 spike: fold into this proxy or keep separate. Default: fold in. |
| Per-workspace spend limits / abuse prevention | Separate concern. Epic 61 changes exfiltration → abuse; abuse still needs bounded by metering (Epic 12). |
| Replacing the relay-router | Architecturally similar but solves a different problem (IP rotation across cloud providers). |
| Custom model rerouting / model allowlisting | Belongs in policy engine (Epic 43). Proxy is credential-injection only. |
| Hot-reload of `agent-config.json` in opencode | Out of our control (opencode upstream behaviour). Proxy makes this irrelevant for credential changes. |
| The `workspace-pw-<id>` admin-token Secret | Different risk profile (Epic 35 §"Non-Requirements"). Unchanged. |

---

## Risks & Open Questions

1. **Latency** — extra intra-cluster hop. Probably negligible (~1–2ms p99 in a healthy cluster) but must be measured before rollout. Add a benchmark story if spike results are ambiguous.

2. **Failure mode: proxy down** — workspaces lose LLM access entirely. Mitigation: ≥3 replicas, PDB, healthcheck, fast kubelet respawn. The proxy MUST be more available than the API server (today's failure mode for credential resolution is the API server being down, which is already fatal — no regression).

3. **Streaming correctness under all opencode providers** — Anthropic and OpenAI have different SSE event shapes; some providers send binary frames. Spike [U1](#assumptions-requiring-spikes) before committing to story detail.

4. **Custom provider URLs (SSRF surface)** — users configure arbitrary endpoints (e.g. `ai.thekao.cloud/v1`). The proxy must support arbitrary upstreams, which expands the attack surface: a workspace could attempt to configure the proxy to fetch internal cluster URLs (metadata service, etcd, etc.). Mitigation: upstream allowlist per workspace, configured via Epic 30's `provider_credentials`; egress NetworkPolicy on the proxy restricting destinations to non-RFC1918 space (with an explicit allowlist override for verified custom endpoints).

5. **Token volume on main container** — projected SA token must persist for the lifetime of the pod (`opencode serve` makes requests at any time). Different from Epic 35's init-only mount. Security review must accept this; see [Auth model](#auth-model-per-workspace-projected-sa-token) for the threat-model argument.

6. **The proxy becomes a new secrets-processing surface** — it holds the KEK and decrypts provider creds on demand. Security review (Epic 17 methodology) is mandatory. The proxy MUST never log headers, never persist decrypted creds, expose zero introspection endpoints. Automated secret-detection CI check on proxy code (US-61.12).

7. **Migration cut-over** — annotation-based per-workspace migration means two code paths coexist during rollout. Test matrix doubles. Define a deprecation date for the old path the day cut-over starts (US-61.9); track in `design/0046_credential_proxy.md` (created during US-61.14).

8. **Provider credential rotation** — if a user rotates their Anthropic key today, the live-reload path pushes the new key. Under Epic 61, rotation is a DB update; the proxy picks up the new value on its next read (configurable TTL, default 60s). No restart required. Validate this is fast enough for the "I just updated my key in the UI" UX.

---

## Adversarial Assessment

### Does the proxy actually prevent exfiltration?

Yes, with one operational caveat. The agent process makes HTTP requests to the proxy with a dummy key. An attacker with RCE in the workspace can:

- ✅ Make arbitrary LLM calls (bounded by Epic 12 spend limits — same abuse surface as today)
- ✅ Read the dummy key (useless outside the proxy)
- ✅ Read the per-workspace proxy token (useful only for impersonating that workspace to the proxy, which the attacker can already do)
- ❌ Read the real Anthropic/OpenAI key (it is in the proxy's process memory in the control-plane namespace, not the workspace's)

The caveat: if the proxy logs request bodies or headers, the secret escapes via logs. Mitigation is operational (logging discipline, automated secret-detection in CI on proxy code — US-61.12) not architectural. This is the same constraint as the API server today.

### Could a workspace impersonate another workspace to the proxy?

Only if it can present another workspace's SA token. SA tokens are cryptographically signed by the K8s API server and mounted only into pods bound to that SA. A workspace pod cannot forge another workspace's SA token. This is the same trust boundary Epic 35 US-35.3 relies on for pod-bootstrap — well-established K8s semantics.

The path-encoded `<workspaceID>` is defense-in-depth: the proxy MUST reject any request where the path ID does not match the TokenReview-extracted identity. This catches misconfiguration (wrong baseURL written to agent-config.json) without relying on the auth layer.

### What about a proxy compromise?

A proxy compromise exposes all credentials the proxy has decrypted. Mitigations:

- The proxy holds the KEK (same as the API server today) — no new key material
- The proxy decrypts on demand per request (no in-memory cache of all workspaces' creds; bounded TTL cache optional, default off)
- The proxy runs in the control-plane namespace, behind the same NetworkPolicy boundary as the API server
- The proxy has a strictly smaller attack surface than the API server (no DB writes, no user-facing API, no SSO, no billing webhooks)

This is not worse than the status quo (API server compromise exposes all creds today). It does increase the **blast radius** of a proxy compromise slightly because the proxy is a new service. Counter: the proxy is much smaller than the API server — smaller attack surface, smaller review surface.

### Is this premature abstraction? (README-LLM.md §12 — Containment Before Abstraction)

No. The current `Materializer.FormatProviders` path is a single-consumer abstraction (opencode) that has bled opencode's config-merge semantics across the codebase (last-writer-wins, `OPENCODE_CONFIG` always wins, no hot reload, `agent-config.json` write architecture — none of these are *our* requirements; all are documented in `README-LLM.md` as opencode's behaviour leaking into our design).

The proxy is **containment**, not abstraction — it puts the opencode config-file dance behind a single seam (the proxy's request path). When the second agent arrives (Claude Code as a `serve`-mode peer, per §12 trigger 1: "When a second consumer is funded"), it points at the same proxy with the same dummy-key pattern. No new abstraction; same boundary. The trigger for paying the bigger cost (a real "agent provider" interface) remains a second consumer, not this epic.

### What if Epic 35 is also implemented — do they conflict?

No. They operate on disjoint credential types:

- Epic 35 eliminates the K8s Secret + PVC-at-rest vectors for **all** secret types (HTTP and non-HTTP)
- Epic 61 eliminates the in-process-memory vector for **LLM-provider** secrets only

The two compose. If both ship, the workspace pod's credential surface becomes:

- SSH keys, git creds, env-secrets: tmpfs only (Epic 35), wiped on pod death
- LLM-provider creds: nowhere on the pod (Epic 61)

### Does this break the live-reload UX?

No — it improves it. Today, a credential update triggers `makeSessionAwareRestartDecision` (deferred restart with 15-minute force-restart ceiling, polling interval, busy-session tracking). Under Epic 61, a credential update is a DB row update; the proxy reads it on the next request (configurable TTL, default 60s). No restart, no session-aware deferral, no restart-reason marker. The user's next message uses the new credential; an in-flight session is unaffected.

The session-aware restart machinery (US-44) is not deleted — it remains for SSH/git/env-secret changes (those still hit `agent-config.json` via the legacy path). Only the `llm-provider` trigger is removed from `shouldRestart` (`cmd/workspace-agentd/secrets.go:871`).

---

## Spikes Required Before Story Detail

Two spikes are mandatory before any story in this epic gets Files / Logic / Acceptance / Tests elaboration. Both are small (≤1 day each).

### Spike U1: Streaming Passthrough

**Goal:** Verify that an openai-compatible custom provider in `agent-config.json` pointing at a local proxy preserves SSE streaming end-to-end through `opencode serve`.

**Setup:**

1. Write a 100-line Go HTTP server that:
   - Accepts `POST /v1/chat/completions`
   - Validates a dummy `Authorization: Bearer llmsafespace-dummy` header
   - Proxies the request to a real upstream (Anthropic or OpenAI sandbox key) with the real header injected
   - Streams the response back with `Flush()` per chunk
2. Configure `opencode serve` to use this proxy as its only provider
3. Run an interactive session and verify streamed tokens reach the client without buffering

**Pass criteria:**

- SSE events arrive at the opencode client with sub-100ms additional latency vs. direct-to-upstream
- No dropped events, no buffering, no `content-type` corruption
- Works for both Anthropic-style (`message_stream` events) and OpenAI-style (`chat.completion.chunk` events) upstreams

**Fail criteria:**

- If streaming is broken, the design needs a different proxy transport (WebSocket? direct TCP? HTTP/2?) before story work begins.

### Spike U4: Provider Auth Scheme Enumeration

**Goal:** Confirm the set of upstream auth schemes is bounded and enumerable, so the proxy's header-injection logic is a small map, not a research problem.

**Method:**

1. Read `pkg/agent/opencode/` formatter to extract every auth shape it emits
2. Enumerate `provider_credentials` rows in a representative install (dev + staging)
3. Cross-reference with Anthropic, OpenAI, OpenRouter, Zen, and the top 3 custom endpoints users have configured
4. Document the auth scheme per upstream as a table

**Pass criteria:**

- The auth scheme set is ≤5 distinct shapes
- Each shape is implementable in <50 lines of Go

**Fail criteria:**

- If arbitrary auth schemes exist (e.g. some custom endpoint requires Digest auth, mTLS, or a multi-step token exchange), the proxy's header-injection logic needs a plugin system. This is a major scope expansion and triggers a redesign.

---

## Cross-References

- **Epic 35** (`design/stories/epic-35-secretless-credential-injection/README.md`) — composes; eliminates disjoint vectors
- **Epic 30** (stories README) — provides `provider_credentials` and `CredentialProvisioner`, the credential source the proxy reads
- **Epic 50** (`design/stories/epic-50-master-kek-hardening/README.md`) — provides master KEK file delivery (US-50.1, shipped); the proxy reuses this verbatim
- **Epic 26** (superseded by Epic 60) — established the workspace → API service NetworkPolicy pattern this epic extends
- **Epic 12** (stories README) — metering; bounds the abuse surface that remains after exfiltration is eliminated
- **`README-LLM.md` §"Relay Config Subsystem"** — the complexity being deleted; will be rewritten in US-61.14
- **sandboxd `internal/authproxy`** (`/workspace/sandboxd/control-plane/internal/authproxy/proxy.go`) — external reference for the design pattern
