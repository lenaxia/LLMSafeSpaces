# LLMSafeSpaces — LLM Implementation Guide

> **Repository:** `github.com/lenaxia/llmsafespaces`

**Version:** 1.22
**Last Updated:** 2026-06-29
**Project Status:** Active Development

---

## Table of Contents

1. [Project Overview](#project-overview)
2. [Critical Guidelines & Hard Rules](#critical-guidelines--hard-rules)
3. [Repository Structure](#repository-structure)
4. [Architecture Overview](#architecture-overview)
5. [Relay Config Subsystem](#relay-config-subsystem)
6. [Inference Relay Fleet](#inference-relay-fleet)
7. [Storage Settings](#storage-settings)
8. [Technology Stack](#technology-stack)
9. [Worklog Requirements](#worklog-requirements)
10. [Development Workflow](#development-workflow)
11. [Multi-Agent Workflow](#multi-agent-workflow)
12. [PR Review Guide](#pr-review-guide)
13. [Common Commands](#common-commands)
14. [Testing Requirements](#testing-requirements)
15. [Multi-Tenant OIDC SSO](#multi-tenant-oidc-sso)
16. [Cloudflare Turnstile CAPTCHA](#cloudflare-turnstile-captcha)

---

## Project Overview

**LLMSafeSpaces** is a Kubernetes-first platform for running AI agents securely in isolated sandboxes. Every sandbox runs `opencode serve` as a persistent HTTP server with a PVC-backed persistent workspace. The API acts as a reverse proxy to the agent, supporting both interactive chat and programmatic (MCP/REST) access.

**Core principles:**

- Every sandbox runs an AI agent (`opencode serve`) — no bare code execution
- Every sandbox is workspace-backed — PVC-mounted persistent filesystem at `/workspace`
- Workspaces can be suspended (pod deleted, PVC retained) and resumed (~22s measured post-optimization; design target is faster — see worklog 0541 + the post-optimization benchmark)
- Credentials stored exclusively in K8s Secrets — never in PostgreSQL, Redis, or logs
- LLMSafeSpaces is an MCP server — any MCP-compatible client can connect
- Stateless API server — horizontally scalable, no sticky sessions required

**Three deliverables:**

1. `api` — Go API service (Gin) + MCP server — reverse proxy to workspace agents, workspace/credential/secret management, session tracking, event streaming
2. `controller` — Kubernetes operator (controller-runtime) — manages Workspace CRD (pod lifecycle, PVC, credentials, health monitoring via agentd sidecar), validating webhooks for Workspace and RuntimeEnvironment, optional InferenceRelay reconciler (multi-cloud relay fleet, Epic 42)
3. `runtimes` — Container images (Python, Node.js, Go) — hardened environments with `opencode serve`, `redact` binary, credential injection

**Optional deployable binaries** (feature-gated, only when the self-hosted relay fleet is enabled): `cmd/relay-router` (in-cluster traffic distributor) and `cmd/relay-proxy` (token-gated reverse proxy run on each relay VM). See [Inference Relay Fleet](#inference-relay-fleet).

**Authoritative design document:**

- [`design/0021_2026-05-21_evolution-v2.md`](design/0021_2026-05-21_evolution-v2.md) — System architecture. This is the single source of truth for architecture decisions.

**Historical design docs (reference only — archived under `design/archive/v1/`):**

- `design/archive/v1/0001_2025-03-05_architecture.md` — System overview, deployment topology, security model
- `design/archive/v1/0003_2025-03-05_controller.md` — Controller specification (CRD types, reconciliation loops)
- `design/archive/v1/0005_2025-03-05_security.md` — Defense-in-depth security model
- `design/archive/v1/0020_2025-03-05_network.md` — Network policy design and egress filtering
- `design/archive/v1/0007_2025-03-05_runtimeenv.md` — Runtime environments

---

## Critical Guidelines & Hard Rules

### 0. Test Driven Development (TDD)

**MANDATORY:** Write tests BEFORE writing functional code. Always.

```
Correct workflow:
1. Write test
2. Run test (must fail)
3. Write minimal code to pass
4. Run test (must pass)
5. Refactor if needed
```

**Test requirements (all are mandatory — none are optional):**

- Multiple happy path tests
- Multiple unhappy path tests (errors, invalid inputs, boundary failures, dependency failures)
- Edge case coverage
- End-to-end integration tests that exercise the real wiring (router → service → K8s/DB/Redis or fakes thereof) — unit tests alone are not sufficient
- Always use `-timeout` when running tests
- Tests must pass before marking work complete

**Definition of done:**

A task is **not** done until it has been demonstrated to be integrated properly via passing e2e/integration tests. "It compiles", "unit tests pass", or "it works in isolation" do not satisfy this requirement. Code that is built but never wired into the live request path is incomplete work.

### 1. Type Safety First

**Always:**

- Define strongly-typed structs for all data structures
- Create domain types for related fields (see `pkg/types/types.go`)
- Use Go types for all CRD specs and statuses

**Never:**

- Use `map[string]interface{}` for structured data
- Use `interface{}` when the type is known
- Pass untyped data between functions

Maps are acceptable only when parsing external JSON/YAML with unknown structure — and even then, convert to a typed struct immediately.

### 2. Idiomatic Go

- Follow Go conventions throughout
- Use `(value, error)` multiple return pattern
- Avoid global state
- Create custom error types for domain-specific errors (see `api/internal/errors/errors.go`)
- Prefer minimal concurrency; add it only when there is clear, measurable benefit

### 3. Explicit Over Implicit

- Explicit error handling — no swallowed errors
- Explicit type declarations
- No magic or hidden behaviour

### 4. Code Quality

**Engineering principles — every change must be:**

- **SOLID** — single responsibility, open/closed, Liskov-substitutable, interface-segregated, dependency-inverted
- **Robust** — handles failures, partial states, and adversarial inputs without corruption
- **Reliable** — deterministic, repeatable, no flaky behaviour
- **Maintainable** — clear naming, small functions, obvious data flow; the next reader should not need a map
- **Scalable** — no hidden O(n²) loops, no per-request allocations of expensive resources, no global locks on hot paths
- **Performant** — measure before optimising; do not pessimise (e.g. unnecessary copies, N+1 queries, synchronous I/O on hot paths)
- **Secure** — input validated, outputs sanitised, secrets never logged, least-privilege by default
- **Not over-engineered** — no speculative abstractions, no premature generalisation, no frameworks-for-the-sake-of-frameworks
- **Not overly complex** — prefer the simplest design that satisfies the requirement; if a junior engineer cannot read it, simplify
- **Idiomatic** — follow the conventions of the language and the surrounding codebase (Go idioms here; see Rule 2)
- **Faithful to the ask** — meet the spirit AND the letter of the requirement; do not solve a different problem because it is easier

**Comments and self-documentation:**

- No comments unless strictly necessary and timeless
- Incorrect or outdated comments must be removed or corrected
- Code is self-documenting through clear naming

### 5. Zero Technical Debt

- Do not create adapters for backwards compatibility
- Remove legacy code
- Implement the full final solution
- Never hack tests to pass — fix the root cause
- **No pre-existing errors are acceptable.** "Pre-existing" is not an excuse. If you encounter errors, warnings, or broken behaviour in the codebase — even if you did not introduce them — fix them. We are the only ones working on this codebase; every error is our responsibility. Leave the codebase in a zero-error state after every session.

### 6. Uncertainty Protocol

If uncertain about correct behaviour: **ask the user**. Do not guess, assume, or implement workarounds.

### 7. Assumptions: State, Then Validate

Every non-trivial change rests on assumptions about the system (data shape, caller behaviour, library semantics, deployment environment, ordering, concurrency, error modes, etc.). These assumptions cause most production bugs when they go unstated and unchecked.

**Mandatory protocol:**

1. **State assumptions up front.** Before writing code, list every assumption the change relies on. Write them in the worklog, the PR description, or a comment block at the top of the design discussion. "It is obvious" is not an excuse — write it down.
2. **Validate every assumption.** For each one, identify how you will prove it true:
   - Read the relevant source/spec/doc
   - Run a query, probe the running cluster, or write a quick test
   - Check git history or existing tests
   - Ask the user if it cannot be validated mechanically
3. **If you cannot validate it, do not rely on it.** Either find a way to validate it, redesign so the assumption is unnecessary, or ask the user. Never proceed on an unvalidated assumption.
4. **Record the validation result.** In the worklog, next to each assumption, record what proved it (e.g. "verified via `pkg/kubernetes/client_test.go:142`" or "confirmed by `kubectl get sandbox -o yaml` on cluster X").
5. **Treat failed validations as findings.** A disproved assumption is a bug or design flaw. Surface it; do not work around it silently.

This rule is non-negotiable. The most common failure mode in this codebase has been silent assumption drift — code that "should work" because someone assumed a behaviour that was never true (see worklogs 0030 and 0033 for examples).

### 8. Understand the Architecture First

Before making any change, read the relevant design document(s). Understand how the change fits the overall data flow. Never modify code without knowing why.

Key documents by area:

| Area | Document |
|------|----------|
| **Architecture** | `design/0021_2026-05-21_evolution-v2.md` (authoritative) |
| Implementation stories | `design/stories/` |
| Security model | `design/0027_2026-05-24_security-policy-v21.md`, `design/0021 §9` |
| Inference relay fleet | `design/stories/epic-42-multi-cloud-inference-relay/README.md` |
| Master KEK hardening | `design/stories/epic-50-master-kek-hardening/README.md` |
| Tenant isolation (gVisor + quotas) | `design/stories/epic-51-tenant-isolation/README.md` |
| System overview (V1) | `design/archive/v1/0001_2025-03-05_architecture.md` |
| Controller + CRDs (V1) | `design/archive/v1/0003_2025-03-05_controller.md` |
| Runtime environments (V1) | `design/archive/v1/0007_2025-03-05_runtimeenv.md` |
| Network policies (V1) | `design/archive/v1/0020_2025-03-05_network.md` |

### 9. Communication Tone

- Neutral, factual, objective
- Not sensational or sycophantic
- Provide honest and critical feedback
- Validate claims with evidence before stating them

### 10. Never Force Push Without Explicit Permission

**NEVER use `git push --force` or `git push --force-with-lease` unless the user has explicitly told you it is okay to force push.**

Force pushing rewrites shared history and can destroy a collaborator's work. The only acceptable scenarios are:

1. The user directly instructs you: "force push" or "push --force"
2. You are fixing a CI-rejected commit (e.g. repolint worklog numbering) and no other collaborator has pulled the broken commit
3. You are working on a private branch that no one else has ever pushed to

**Always prefer `git pull --rebase` + normal `git push` over force pushing.** If you pushed a broken commit, first ask the user if force push is acceptable, describe why it's needed, and wait for confirmation.

### 11. Adversarial Self-Review

After implementing any non-trivial change, **before marking it complete**, conduct a structured adversarial review in three phases.

#### Phase 1: Identify Weaknesses, Gaps, and Failure Modes

Explicitly ask:

1. **Where are the gaps?** What did the design not cover? What edge cases are unhandled? What requirements were omitted?
2. **Where is it weak?** Which parts are fragile, tightly coupled, or depend on implicit ordering?
3. **Where will it fail?** Under what conditions (concurrency, partial failure, invalid state, resource exhaustion, adversarial input) will the implementation behave unexpectedly?
4. **What did I assume without verifying?** Re-read the assumptions list. For each one, ask: "Did I actually validate this, or did I just believe it?"
5. **What would a skeptical reviewer reject?** If someone with no context read this diff, what would they flag?
6. **Why might this code be wrong?** Take the adversarial view — assume the implementation is incorrect or misses the mark, and prove otherwise.

#### Phase 2: Validate Each Finding

For every criticism generated in Phase 1:

1. **Is the finding real?** Re-read the code, re-run the test, reproduce the scenario. Do not take findings at face value.
2. **Is it a bug, a design flaw, or a false alarm?**
   - **Real bug:** Fix it before proceeding. Do not defer.
   - **Design flaw:** Surface with proposed remediation. Do not proceed without addressing.
   - **False alarm:** Document why it is not a real issue (one sentence with evidence). Do not silently dismiss.
3. **If uncertain:** Escalate to the user rather than dismissing or guessing.
4. **Only validated findings make it into the record.** Unvalidated claims, guesses, and assumed-but-unverified assertions are discarded. They have no place in a worklog, PR description, or review report.

#### Phase 3: Remediate or Document

- Real findings must be fixed with regression tests before the change is complete.
- False alarms must be documented with rationale (one sentence is sufficient).
- The change is not ready until Phase 2 returns zero real findings.

This is not optional introspection — it is a mandatory validation gate. Code that has not survived its own adversarial review is not ready for commit.

See also the [Adversarial Assessment](#adversarial-assessment) section in the PR Review Guide for expanded criteria used during pull request review.

### 12. Containment Before Abstraction (External-Dependency Coupling)

This codebase depends on external components it does not own — most notably the AI agent that runs inside every workspace (`opencode serve`). The platform's value is the orchestration, isolation, and multi-tenant control layer, *not* the agent loop. The risk is that knowledge of an external component's implementation details bleeds into platform code (API, controller, services), making every future change a jury-rig and every eventual swap a rewrite.

The rule is about *when* to pay for an abstraction — and it is the opposite of "abstract everything early."

**Containment now (cheap, mandatory):**

- **Keep external-dependency knowledge behind a single seam.** A package, folder, or adapter — not scattered across the codebase. The opencode config-merge semantics, provider-ID model, relay injection, and readyz contract should live in one place. Platform code talks to that seam; it does not know what is behind it.
- **Stop adding new external-component specifics to platform code.** When you write a new feature, ask: "does this line need to know the agent is opencode?" If yes, it belongs behind the seam, not in a service or handler.
- **Containment is not abstraction.** No interface design, no generics, no provider registry. Just a boundary — a folder whose contents are allowed to know the external component, and a small surface the rest of the codebase calls. This is cheap and reversible.

**Do NOT abstract prematurely (the trap):**

- A single consumer tells you nothing about what the interface should look like. Any abstraction designed against one implementation will encode that implementation's shape as if it were universal — and you will refactor the abstraction itself when the second consumer arrives. You pay twice. This is the speculative-abstraction tax Rule 4 prohibits.
- The relay-config subsystem is the canonical example: every accommodation of opencode's config-merge semantics (last-writer-wins, `OPENCODE_CONFIG` always wins, no hot reload, the `agent-config.json` write architecture, the one-shot injector, the 20s stale window) is opencode's behaviour leaking into our design. None of those are *our* requirements. Designing an "agent provider" interface today — against opencode only — would freeze that leakage into a contract we'd then have to break.

**Trigger the real abstraction here (pay the big cost):**

1. **When a second consumer is funded.** The moment a second agent (e.g., Claude Code, a homegrown harness) is scheduled as real work, abstract first, adapter second. A second consumer is the only thing that validates interface shape — writing the second adapter straight onto single-consumer-shaped platform code produces two jury-rigged systems instead of one. This is the highest-leverage moment to spend the money.
2. **When the external component forces a rewrite anyway.** If an opencode breaking change forces a relay-config rewrite, absorb the abstraction cost then — the rewrite cost is already sunk.
3. **When pain recurs in the same seam.** A repeated pattern of jury-rigging the *same* area (e.g., multiple worklogs touching `agent-config.json` handling) is evidence that containment has failed. That is a valid abstraction signal even before a second consumer — but the bar is *recurrence*, not a single inconvenience.

**The test for "is this premature?"**

Before introducing an interface, adapter, or provider abstraction for an external component, answer: *"Do I have at least two concrete consumers, or a forcing rewrite, or demonstrated recurring pain in this seam?"* If none, contain behind a boundary and stop. The abstraction will be cheaper and more correct when one of those is true.

**Scope:** this rule is general — it applies to any external dependency whose internals we accommodate (agents, cloud drivers, relay VM binaries, MCP SDKs). The agent provider is the current primary case because the coupling is deepest there.

---

## Repository Structure

```
llmsafespaces/
├── cmd/           # Top-level binaries (api, mcp, redact, repolint, seal-key, workspace-agentd, relay-router, relay-proxy)
├── api/           # Go API service (Gin) + MCP server — reverse proxy, workspace/credential/secret management
├── controller/    # Kubernetes operator (controller-runtime) — Workspace reconciler, InferenceRelay reconciler, validating webhooks
├── runtimes/      # Container images (Python, Node.js, Go) with opencode serve, redact binary
├── pkg/           # Shared packages imported by api/ and controller/ (see CRD type ownership below)
├── mocks/         # Shared test mocks
├── sdks/          # Client SDKs (Go, TypeScript, Python, Java, VS Code extension) from OpenAPI spec
├── frontend/      # React 19 + TypeScript + Vite SPA
├── charts/        # Helm chart (API, controller, frontend, CRDs, RBAC, webhooks, optional relay-router)
├── design/        # Design documents — 0021_evolution-v2.md is authoritative
├── hack/          # Build and code generation scripts
├── local/         # kind bootstrap/test/teardown scripts
├── tests/         # End-to-end integration tests
└── .github/       # CI/CD workflows + AI prompt templates
```

**Before editing:** Read each folder's `README.md` for rules and conventions. Folders missing a `README.md` should have one added.

**CRD type ownership:** `pkg/apis/llmsafespaces/v1/` holds authoritative kubebuilder-annotated CRD types (Workspace, RuntimeEnvironment). `pkg/types/` holds API transfer objects only (request/response DTOs) — not CRD schemas. These must not be merged.

---

## Architecture Overview

```
┌──────────────────────────────────────────────────────────────────────────────┐
│                                                                              │
│   MCP Clients / Browser / REST / SDK                                        │
│         │                                                                    │
│         ▼                                                                    │
│   ┌─────────────────────────────────────────────────────────────────────┐   │
│   │  LLMSafeSpaces API (stateless, horizontally scalable)               │   │
│   │  ┌──────────┐  ┌──────────┐  ┌───────────┐  ┌──────────────────┐  │   │
│   │  │ REST API │  │  SSE     │  │   Auth    │  │  Rate Limiting   │  │   │
│   │  │ (Gin)    │  │ Stream   │  │ JWT+APIKey│  │  + Validation    │  │   │
│   │  └──────────┘  └──────────┘  └───────────┘  └──────────────────┘  │   │
│   │  ┌──────────┐  ┌──────────┐  ┌───────────┐  ┌──────────────────┐  │   │
│   │  │ Workspace│  │  Proxy   │  │  Secrets  │  │  MCP Server      │  │   │
│   │  │ Service  │  │ Handler  │  │  Service  │  │  (stdio/SSE)     │  │   │
│   │  └──────────┘  └──────────┘  └───────────┘  └──────────────────┘  │   │
│   │  ┌──────────┐  ┌──────────┐  ┌───────────┐                         │   │
│   │  │ Database │  │  Cache   │  │  Metrics  │                         │   │
│   │  │ (pgx)    │  │ (Redis)  │  │ (Prom)    │                         │   │
│   │  └──────────┘  └──────────┘  └───────────┘                         │   │
│   └───────────────────────────┬─────────────────────────────────────────┘   │
│                               │ CRD + Secret operations via K8s API         │
│                               ▼                                              │
│   ┌─────────────────────────────────────────────────────────────────────┐   │
│   │  Kubernetes Cluster                                                 │   │
│   │                                                                     │   │
│   │  ┌───────────────────────────────────────────────────────────────┐ │   │
│   │  │  Controller (controller-runtime)                               │ │   │
│   │  │  ┌──────────────────┐  ┌────────────────────────────────────┐ │ │   │
│   │  │  │  Workspace       │  │  Validating Webhooks               │ │ │   │
│   │  │  │  Reconciler      │  │  (Workspace + RuntimeEnvironment)  │ │ │   │
│   │  │  └──────────────────┘  └────────────────────────────────────┘ │ │   │
│   │  └───────────────────────────────────────────────────────────────┘ │   │
│   │                                                                     │   │
│   │  ┌───────────────────────────────────────────────────────────────┐ │   │
│   │  │  Workspace Pods (each runs opencode serve :4096)             │ │   │
│   │  │  ┌──────────────────┐  ┌──────────────────┐                  │ │   │
│   │  │  │ init: workspace- │  │ init: credential- │                  │ │   │
│   │  │  │ setup (packages, │  │ setup (creds →    │                  │ │   │
│   │  │  │ initScript)      │  │ /sandbox-cfg)     │                  │ │   │
│   │  │  ├──────────────────┤  └──────────────────┘                  │ │   │
│   │  │  │ main: opencode serve --hostname 0.0.0.0 --port 4096       │ │   │
│   │  │  │ security: readOnlyRoot, runAsNonRoot, drop ALL caps        │ │   │
│   │  │  └──────────────────────────────────────────────────────────┘  │ │   │
│   │  │  Volumes: PVC at /workspace (subPath:workspace) + /home/sandbox (subPath:home) + /tmp (subPath:tmp) + emptyDir /sandbox-cfg  │ │   │
│   │  └───────────────────────────────────────────────────────────────┘ │   │
│   └─────────────────────────────────────────────────────────────────────┘   │
│                                                                              │
│   ┌─────────────────────┐  ┌─────────────────┐                              │
│   │ PostgreSQL           │  │ Redis            │                              │
│   │ (user metadata,      │  │ (caching, rate   │                              │
│   │  workspace names,    │  │  limiting, DEK    │                              │
│   │  secrets, settings)  │  │  cache)           │                              │
│   └─────────────────────┘  └─────────────────┘                              │
└──────────────────────────────────────────────────────────────────────────────┘
```

### Custom Resource Definitions

The controller manages 3 CRDs in the `llmsafespaces.dev/v1` API group:

| CRD | Kind | Scope | Short | Purpose |
|-----|------|-------|-------|---------|
| `workspace.yaml` | `Workspace` | Namespaced | `ws` | PVC-backed persistent environment + pod running `opencode serve` |
| `runtimeenvironment.yaml` | `RuntimeEnvironment` | Cluster | `rte` | Defines a runtime image (Python, Node.js, Go) |
| `inferencerelay.yaml` | `InferenceRelay` | Cluster | `irelay` | Managed fleet of relay VMs (AWS/OCI/GCP) proxying free-tier inference — feature-gated (`controller.inferenceRelay.enabled`), requires `rbac.scope=cluster` |

Legacy CRDs (Sandbox, SandboxProfile, WarmPool, WarmPod) have been removed. The Workspace CRD absorbs all sandbox and profile functionality. `InferenceRelay` is the only CRD beyond the core Workspace/RuntimeEnvironment pair and is opt-in.

### CRD type ownership

CRD types exist in two locations with strictly separate roles:

| Location | Purpose |
|----------|---------|
| `pkg/apis/llmsafespaces/v1/` | **Authoritative** — kubebuilder-annotated CRD types (Workspace, RuntimeEnvironment), used by both controller and API service |
| `pkg/types/` | **API transfer objects only** — REST request/response shapes (`CreateWorkspaceRequest`, etc.). Not CRD schemas. |

These are intentionally different types. The API types are transfer objects; the CRD types are Kubernetes schemas. They must not be merged.

### Workspace lifecycle

```
Pending → Creating → Active → Suspending → Suspended → Resuming → Active
             │                   ↘           ↘           ↘
             └──→ Failed           Terminating            Terminating
                                      ↘                       ↘
                                    Terminated              Terminated
```

Nine phases: `Pending`, `Creating`, `Active`, `Suspending`, `Suspended`, `Resuming`, `Terminating`, `Terminated`, `Failed`.

Suspend deletes the pod but retains the PVC. Activating a suspended workspace re-creates the pod (~22s measured post-optimization; PVC re-attach + opencode boot dominate the remaining cost — the original ~3s figure was an unvalidated design target). Session history in the PVC survives.

### State management: K8s CRD vs PostgreSQL

| Data | Owner | Source of Truth |
|------|-------|-----------------|
| Workspace phase | Controller | K8s CRD status |
| PVC name, pod IP | Controller | K8s CRD status |
| Conditions | Controller | K8s CRD status |
| `status.lastActivityAt` (workspace) | API server (batched, ≤60s flush) | K8s CRD status |
| Workspace display name | API | PostgreSQL |
| User ID ownership | Both | K8s CRD (`spec.owner`) authoritative; PostgreSQL mirrors for query perf |
| Creation/update timestamps | Both | K8s CRD authoritative; PostgreSQL mirrors |
| Credentials | Controller | K8s Secrets (never PostgreSQL) |
| User auth data (passwords, API keys, DEKs) | API | PostgreSQL |
| Encrypted secrets | API | PostgreSQL (encrypted at rest) |
| Settings | API | PostgreSQL |

### Service initialization order

The API service starts dependencies in a specific order with rollback on failure:

```
Metrics → Database → Cache → Auth → Workspace → SessionIndex → Secrets → Settings → ProviderCredentials
```

Shutdown reverses this order.

### Master KEK (server root key)

The master KEK is the root of trust for at-rest encryption: it wraps API-key DEKs, org SSO client secrets, and (via the `AdminKeyDeriver` callback) every admin/org LLM provider credential. Two crypto layers consume it today (Layer 1 `RootKeyProvider` for `api_keys` + `org_sso_configs`; Layer 2 `AdminKeyDeriver` for `provider_credentials`); Epic 50 US-50.2 will unify them under `RootKeyProvider`.

**Delivery (US-50.1, shipped):** projected as a read-only file mount at `/var/run/secrets/llmsafespaces/master-secret` (Helm `masterSecret.deliveryMethod=file`, the default), read via `LLMSAFESPACES_MASTER_SECRET_FILE`. This eliminates `/proc/1/environ` exposure — the previous env-var delivery (`LLMSAFESPACES_MASTER_SECRET`) remains as a deprecated opt-in (`deliveryMethod=env`) for non-Helm deploys. Multi-file colon-separated paths support the future US-50.4 rotation window (active = last ≥32-byte file). See `design/stories/epic-50-master-kek-hardening/README.md`.

### Tenant isolation

Multi-tenant isolation rests on layered controls in a **shared namespace** (no per-tenant namespaces — they don't stop container escape and don't scale to 1,000+ tenants):

| Control | Status | Mechanism |
|---------|--------|-----------|
| Network isolation | Shipped | Chart-level default-deny ingress + RFC1918/CGNAT-filtered egress NetworkPolicies |
| Secret scoping | Shipped | `rbac.scope=namespace` default; namespace-scoped Secrets Role |
| Tenant identity | Shipped | `WorkspaceOwner{UserID, OrgID}` on the CRD; `llmsafespaces.dev/tenant` pod label |
| Container-runtime isolation | Opt-in (Epic 51 S51.1) | gVisor (`runsc`) RuntimeClass — the primary control against kernel-exploitation container escape; `--default-runtime-class=gvisor` + `gvisor.defaultRuntimeClass`; per-workspace opt-out via `spec.runtimeClass: "runc"` |
| Per-tenant resource quotas | Opt-in (Epic 51 S51.2) | `PodTenantQuotaValidator` admission webhook keyed on the tenant label — caps max-workspaces / max-cpu-millis / max-memory-mi per tenant; disabled when all limits are 0; fails open on transient errors |

Org-specific quota overrides and billing-tier→quota mapping are deferred to Epic 43. See `design/stories/epic-51-tenant-isolation/README.md`.

---

## Relay Config Subsystem

### Overview

The relay config subsystem manages how `agent-config.json` — the file opencode reads for provider credentials — is built and kept correct across the pod lifetime. Multiple processes write to this file, which has been the source of several confirmed production bugs.

**Volume layout on every workspace pod:**

| Mount | Type | Persists across pod restart? | Owner |
|---|---|---|---|
| `/workspace` | Longhorn PVC (`subPath: workspace`) | Yes | User workspace data, opencode.db, auth.json |
| `/home/sandbox` | Longhorn PVC (`subPath: home`) | Yes | SSH keys, secrets base dir, enricher cache, tool caches |
| `/tmp` | Longhorn PVC (`subPath: tmp`) | Yes — init scripts, package caches; NOT credentials (US-35.7 moved them to tmpfs) | init-script.sh |
| `/sandbox-cfg` | emptyDir (memory, 32Mi) | No — ephemeral per pod, read-only on main container | secrets.json, workspace-config.json, password (from bootstrap) |
| `/sandbox-runtime` | emptyDir (memory, 96Mi, RW) | No — ephemeral per pod, wiped on death | agent-config.json, secrets-env, `admin-prompt.md` (merged platform/org/role/user system prompt, #483), `last-reload-secrets.json` (reload-replay cache, #443), symlink targets for SSH/git/secrets/auth.json |

**Key path constants** (`pkg/agentd/types.go`):

```
AgentConfigPath  = "/sandbox-runtime/agent-config.json"
AdminPromptPath  = "/sandbox-runtime/admin-prompt.md"  ← bootstrap writes merged platform→org→role→user system prompt here; #483
SecretsBasePath  = "/sandbox-runtime/rt/secrets"   ← deleted by reset() on every reload; tmpfs
SecretsEnvPath   = "/sandbox-runtime/secrets-env"
ReloadSecretsCachePath = "/sandbox-runtime/last-reload-secrets.json"  ← persisted by reloadSecretsHandler; replayed by boot-time materialize to restore user-DEK creds after a container restart (#443); tmpfs, wiped on pod death
```


Note: `/tmp` is a PVC subPath (`subPath: tmp`). US-35.7 moved `agent-config.json` and `secrets-env` off `/tmp` to `/sandbox-runtime` (tmpfs/RAM). `admin-prompt.md` (PR #416 / fix #483) followed — both for at-rest data isolation and because the `credential-setup` init container's `/tmp` is read-only (ReadOnlyRootFilesystem with no writable emptyDir mounted). `$HOME`-relative credential paths (`.ssh`, `.secrets`, `.git-credentials`, `auth.json`) are symlinks created by the init container pointing into `/sandbox-runtime/rt/*`. On pod death, tmpfs is wiped — the PVC retains only dangling symlinks, no plaintext bytes.

**opencode config loading order** (validated from opencode 1.15.12 binary):

opencode merges config files via recursive deep-merge, last writer wins:
1. Global XDG config: `~/.config/opencode/opencode.jsonc`
2. Project config: `findUp(["opencode.json","opencode.jsonc"], cwd, {rootFirst:true})`
3. `OPENCODE_CONFIG` env var path — **always appended last, always wins**

`OPENCODE_CONFIG=/sandbox-runtime/agent-config.json` is set by `entrypoint-opencode.sh`. Therefore `agent-config.json` overrides all other config for any key it sets. opencode does **not** hot-reload this file — it is only read at process startup.

**auth.json location** (validated): `XDG_DATA_HOME=/workspace/.local` is set before `exec workspace-agentd`, so agentd inherits it. `authJSONPath = /workspace/.local/opencode/auth.json` — US-35.7: this path is a symlink to `/sandbox-runtime/rt/auth.json` (tmpfs), created by the init container. Wiped on pod death; no plaintext on PVC at rest.

---

### Writers of agent-config.json (as of 2026-06-19, post-US-46.10)

Within the agentd process, there is **one** write path to `agent-config.json`:

| Writer | File | When | Produces |
|---|---|---|---|
| `AgentConfigWriter.Rebuild()` | `cmd/workspace-agentd/agent_config_writer.go` | Every credential reload + relay injection | Complete merged config: providers + model + relay (temp-file + `os.Rename`) |

The **materialize subcommand** (separate process, runs before agentd) writes directly via `FlushProviders` + `applyWorkspaceConfig`. Once agentd starts, it reads this initial file via `newAgentConfigWriter()` and owns all subsequent writes.

The writer holds three sources, each updated independently:
- **Providers** — `setProviders()` called after `Materializer.FormatProviders()` on credential reload
- **Model** — captured from the existing file at boot (set by `applyWorkspaceConfig`)
- **Relay** — `setRelay()` called by `startRelayInjector` after successful free-model discovery

`Rebuild()` merges all three and writes atomically. The `sync.Mutex` serialises concurrent calls.

---

### Known design fragilities (documented, not bugs)

1. **~~Multiple writers of agent-config.json~~ — RESOLVED (US-46.10).** The four-writer design has been replaced by a single `AgentConfigWriter` that owns all writes to `agent-config.json`. The writer holds three sources (providers, model, relay) and `Rebuild()` merges them into a complete config written atomically via temp-file + `os.Rename`. The relay injector and reload handler update their source then call `Rebuild()`. The `atomic.Pointer[[]relayModel]` coordination and the reload handler's manual relay re-merge have been removed — the writer always reflects current state.

2. **One-shot relay injector.** The injector goroutine runs once per pod lifetime. If the opencode credential changes after the injector has run (personal key → public key), the relay is not re-evaluated. The user must restart the pod. A re-triggerable injector (channel-based state machine) would handle this automatically.

3. **In-memory model cache is per-API-replica.** `SetModel` evicts on the replica that handled the request; other replicas serve stale data for up to 5 seconds. Future: Redis-backed cache for cross-replica consistency (US-30.11).

4. **`resolveModelWithProvider` non-determinism on collision.** When two providers in `agent-config.json` share a model ID, Go map iteration is non-deterministic — `resolveModelWithProvider` returns whichever provider the runtime visits first. In practice, provider model IDs are namespaced and do not collide, but this is not enforced.

---

### How the relay config subsystem works (as-built)

The relay config subsystem uses a single `AgentConfigWriter` (`cmd/workspace-agentd/agent_config_writer.go`) that owns all writes to `agent-config.json` within the agentd process. The writer holds three sources:
1. **Providers** — from `Materializer.FormatProviders()` (llm-provider credentials)
2. **Model** — from `applyWorkspaceConfig()` (workspace-config.json default model)
3. **Relay** — from `startRelayInjector()` (opencode-relay provider + disabled_providers)

`Rebuild()` merges all three sources and writes atomically (temp-file + `os.Rename`). Coordination is via the writer's `sync.Mutex`, which serialises concurrent `Rebuild()` calls. opencode reads `agent-config.json` once at startup — not hot-reloaded.

#### Agent-config.json write sequence (boot)

1. **Materialize subcommand** (separate process, before agentd): loads base `/sandbox-cfg/secrets.json` (server-KEK creds) and replays `/sandbox-runtime/last-reload-secrets.json` (the last reload-secrets batch, #443) merged on top — cache wins on duplicate Type+Name. `Materializer.reset()` wipes tmpfs credential files → `Materialize(merged)` re-applies both base + cached user-DEK creds → `FlushProviders()` writes provider credentials → `applyWorkspaceConfig()` adds model key with providerID/modelID. Absent cache = first boot (base only). Corrupt cache = warn + base only.
2. **agentd starts**: `newAgentConfigWriter()` reads the existing file, captures providers + model as initial sources
3. **~T+7s**: `startRelayInjector()` fetches free models → `writer.SetRelay(url, models)` + `writer.Rebuild()` writes merged config → updates auth.json → restarts opencode

#### Agent-config.json write sequence (credential reload)

1. `reloadMu.Lock()` → `Materializer.reset()` → `Materialize(batch)` → `Materializer.FormatProviders()` formats credentials → `writer.SetProviders(formatted)` + `writer.Rebuild()` merges with existing model + relay sources → **`writeReloadSecretsCache()`** persists the batch to `/sandbox-runtime/last-reload-secrets.json` (tmpfs; survives container restart, wiped on pod death) → `reloadMu.Unlock()`
2. `proc.restart()` reboots opencode with updated config

The cache write (#443) is what lets user-DEK credentials (env-secrets like `GH_TOKEN`, SSH keys, user LLM providers) survive a main-container restart (OOM, panic, kubelet restart): without it, the next boot's `reset()` would wipe them and the base `secrets.json` (bootstrap, sessionless) never contained them. The cache is written after `Materialize` succeeds, is never written on a hard failure (500), and degrades to base-only on a corrupt read.

#### RelayInjected signal flow

The API server needs to know whether the relay injector ran for a specific pod
so it can correctly annotate the model catalog. The signal flows:

```
relay_injector.go:
  writer.SetRelay(url, models) → AgentConfigWriter.relay (non-nil after success)

agentd /v1/readyz:
  writer.HasRelay() → ReadyzResponse.RelayInjected = true
  readyz uses: healthCache.Snapshot() (atomic, no I/O)
             + cachedState() (providerCache, 15s TTL; live calls on miss, bounded by 5s)

API server (ListModels cache miss):
  fetchRelayInjected() → GET /v1/readyz (Bearer token, port 4098, 5s total timeout)
                       → ReadyzResponse.RelayInjected
  → cached in modelCachePayload with 5s TTL alongside model list
```

**Stale window:** `relayInjected` can take up to **5s + 15s = 20s** to reflect a
relay injection that has just completed:
- The model cache TTL is 5s — a cache hit may serve the previous `relayInjected=false` value
  for up to 5s after the cache was written.
- The `providerCache` inside readyz has a 15s TTL — a readyz call may return stale
  `connected[]` data for up to 15s after relay injection.
- In the worst case, a `ListModels` request at T=1s caches `relayInjected=false` until
  T=6s; relay injection completes at T=7s; the cache expires at T=6s but the next readyz
  call may read stale `providerCache` for another 15s — making the first correct response
  appear at approximately T=21s.

This is acceptable: the Phase 1 window is ~7s, and users are unlikely to interact with
the workspace within the first 20s of pod boot. The stale window is purely cosmetic
(models show `providerID="opencode"` instead of `"opencode-relay"`) and self-corrects.

#### annotateModels remap — intentional defense-in-depth

The remap guard `relayGloballyEnabled && relayInjected && avail == ModelFreeTier && p.ID=="opencode"` is unreachable in Phase 2 (because `disabled_providers` removes `opencode` from `connected[]`) and correctly suppressed in Phase 1. **It is intentionally retained as defense-in-depth**, not removed as tech debt.

The guard protects against a failure mode we have already lived through: if an opencode version ever keeps the built-in `opencode` provider in `connected[]` despite `disabled_providers`, this code correctly remaps free-tier models to `opencode-relay` rather than silently routing users to a disabled provider. `disabled_providers` is an upstream mechanism we do not control; single mechanisms fail, which is why the guard layers on top of it.

History supports keeping it: the guard was specifically *narrowed* (not added) in worklog 0178 to fix a real `ProviderModelNotFoundError` bug, and the comment block was re-reasoned in worklog 0189 after a follow-up audit. The ~20 LoC cost (4-line conditional + 2 tests + the comment block at `models.go:439-453`) is justified by the silent-failure mode it prevents. See worklog 0341 for the full rationale.

---

## Inference Relay Fleet

### Overview

The **inference relay fleet** (Epic 42) is the self-hosted relay option for free-tier opencode Zen model inference. It exists for operators who need IP rotation on free-tier access: the fleet runs relay VMs across multiple clouds (AWS primary, OCI secondary, GCP optional) for IP diversity and rotation on 429.

The default mode is **direct-to-Zen**: workspace pods call `https://opencode.ai/zen/v1` directly using opencode's built-in `public` anonymous key. No relay configuration is required for this mode.

The Cloudflare Worker relay (Epic 26) was removed in Epic 60 (2026-07-12): Zen now blocks all Cloudflare Worker egress IPs, making the Worker unreachable. See `design/stories/epic-60-remove-cf-worker-relay/README.md`.

> **Not to be confused with the [Relay Config Subsystem](#relay-config-subsystem).** That section describes how `agent-config.json` is built *inside* the workspace pod. This section describes the *external* fleet of VMs the pod's relay injector may point at.

### Components

| Component | Location | Role |
|-----------|----------|------|
| `InferenceRelay` CRD | `pkg/apis/llmsafespaces/v1/inferencerelay_types.go` | Desired fleet state: providers (AWS/OCI/GCP), health-check, 429-rotation, fallback config. Cluster-scoped (`irelay`). |
| Relay reconciler | `controller/internal/relay/` | Provisions VMs via cloud-init, health-checks them, destroys + reprovisions on 429 storms or sustained unhealthiness. AWS (`aws_driver.go`), OCI (`oci_driver.go`) drivers; GCP via the provider enum. |
| relay-router | `cmd/relay-router/` | In-cluster Deployment (1 replica). Distributes workspace traffic across healthy relay VMs (weighted: AWS primary, OCI secondary, GCP tertiary), detects 429 storms, and falls back to direct upstream when all VMs are down. Reads the `relay-router-peers` ConfigMap written by the controller. |
| relay-proxy | `cmd/relay-proxy/` | Reverse proxy run *on each relay VM*. Distributed via cloud-init (SHA-256 verified) from `controller.inferenceRelay.artifact.urls`. Token-gated (`X-Relay-Token` header). |
| Admin API | `api/internal/handlers/relay_admin.go` | Setup wizard + status dashboard (`/api/v1/admin/relay/*`); creates the `InferenceRelay` CR and stores provider credentials as Secrets. |

### Auth model (WireGuard removed)

The router↔relay path was originally a WireGuard mesh. **Removed in worklog 0447** and replaced with plaintext HTTP + **per-VM shared-secret tokens** (`X-Relay-Token`, `crypto/subtle.ConstantTimeCompare`):

- Per-VM (not fleet-wide) tokens preserve WG's tight blast radius — a compromised VM's token cannot be used against sibling relays. Stored in the `relay-vm-tokens` Secret keyed by provider slot; rotation = destroy + reprovision.
- `/healthz` and `/metrics` on relay-proxy are token-exempt (the router probes health without the per-VM token).
- Plaintext HTTP (not TLS) is an accepted trade-off: the exposure is free-tier Zen access only (no paid credentials, no user data). See the design doc `design/stories/epic-42-multi-cloud-inference-relay/README.md`.

### Feature gate and configuration

Disabled by default. Enable via Helm:

| Value | Purpose |
|-------|---------|
| `controller.inferenceRelay.enabled` | Feature gate. Requires `rbac.scope=cluster` (cluster-scoped CRD). |
| `controller.inferenceRelay.routerURL` | Router `/metrics` scrape URL (controller → router, in-cluster). |
| `controller.inferenceRelay.workspaceRouterURL` | URL workspace pods use to reach the router. Empty → derived cross-namespace FQDN. |
| `controller.inferenceRelay.artifact.{urls,sha256Arm64,sha256Amd64}` | relay-proxy binary distribution (cloud-init downloads + verifies). |
| `controller.inferenceRelay.upstreamURL` | Upstream the fleet proxies to (default `https://opencode.ai/zen/v1`). |
| `controller.inferenceRelay.upstreamAuth.keySecret` | Optional real upstream key for router-side injection (paid gateway). |

Controller flags mirror these: `--enable-inference-relay`, `--relay-router-url`, `--relay-artifact-url`, `--relay-artifact-sha256-{arm64,amd64}`. Build the VM binaries with `make relay-bin` (cross-compiles `deploy/relay-proxy-{arm64,amd64}`).

### Operational notes

- The router's `SelectRelay` is weighted: **AWS receives 100% of traffic when healthy (weight 1000)**; OCI (weight 100) receives traffic only when AWS is unavailable or draining; GCP (weight 1, optional) only when both AWS and OCI are unavailable. Fallback mode rate-limits direct upstream access (default 0.5 req/s, max 1 concurrent) to avoid worsening IP throttling. See `relayWeight` in `cmd/relay-router/fleet.go`.
- Fleet validation is gated on a real cloud deploy (in-cluster testing requirements in worklog 0462; full validation run in worklogs 0464–0471). Unit + integration tests cover the state machine, peer polling, health, cloud-init rendering, and token round-trip.

---

## Storage Settings

### Settings involved

| Setting key | Schema default | Admin UX label | Where enforced |
|---|---|---|---|
| `workspace.defaultStorageSize` | `15Gi` | Default Storage | API service at workspace create time |
| `workspace.defaultStorageClass` | `""` | Storage Class | API service at workspace create time |

Both are Tier 2 (admin-mutable) `instance_settings` entries stored in PostgreSQL and served by the settings service (`pkg/settings/instance_service.go`). The admin UX reads them via `GET /admin/settings` and writes via `PUT /admin/settings/{key}` (`api/internal/handlers/settings.go`).

**Helm-managed override for `workspace.defaultStorageClass`:** operators can pin the value in `values.yaml` under `workspace.defaultStorageClass`. When non-empty at API boot, `app.go` calls `instanceSettings.SetHelmOverrides` which promotes it to Tier 1 (read-only in the admin UI, PUTs return 409). When empty (the chart default), the setting stays Tier 2 and admin-editable. This exists so operators running on clusters with dedicated low-durability StorageClasses (e.g. Longhorn 2-replica) can declare that choice in the chart instead of relying on post-install UI configuration.

**Removed settings:**
- `workspace.maxStorageSize` — removed. PVC size is set once at creation and never changed; the admission webhook (`webhooks.maxWorkspaceStorageGi: 1024 Gi` in `values.yaml`) is the correct infrastructure-level ceiling. A dynamic DB-backed cap that only applied to the API path added complexity without meaningful safety.
- `workspace.defaultResources.ephemeralStorage` — removed alongside the entire ephemeral-storage concept (see "Ephemeral storage — not set on the pod" below).

### `workspace.defaultStorageSize` — full trace

1. **Frontend** (`frontend/src/api/workspaces.ts`): `storageSize` is intentionally omitted from the create workspace payload — the API resolves the default.
2. **API service** (`api/internal/services/workspace/workspace_service.go`): on `CreateWorkspace`, if `req.StorageSize` is empty, `instanceSettings.GetString(ctx, "workspace.defaultStorageSize")` supplies it.
3. The resolved size is written into `WorkspaceSpec.Storage.Size` in the CRD, persisted to the `workspace_metadata` PostgreSQL table, and returned in API responses as `storageSize`.

**Side effects of changing `defaultStorageSize`:**
- Affects only **new** workspaces. Existing PVCs are never resized.
- Takes effect immediately on the next workspace creation — no redeploy needed.
- The hard ceiling is `webhooks.maxWorkspaceStorageGi` (default `1024 Gi`, Helm value) enforced at the Kubernetes admission layer for all paths including direct `kubectl apply`.

### Ephemeral storage — not set on the pod

The pod builder does NOT set `ephemeral-storage` requests or limits on workspace containers (`controller/internal/workspace/pod_builder.go` `resourceRequirementsFor`). The `Workspace` CRD has no `spec.resources.ephemeralStorage` field, the webhook has no corresponding cap, and Helm has no `maxWorkspaceEphemeralStorageGi` flag. All of these were removed because they protected against a threat (uncontrolled writes to node-local ephemeral storage) that the architecture already mitigates.

**Why nothing meaningful writes to ephemeral storage on a workspace pod:**

| Source | Counts toward ephemeral storage? | Notes |
|---|---|---|
| Container writable layer (overlay FS) | No | `readOnlyRootFilesystem: true` — EROFS for all unmounted paths |
| Container log files (stdout/stderr) | **Yes** | Kubelet writes to `/var/log/pods/` on node disk; kubelet rotation caps at ~50 Mi (10 Mi × 5 files) regardless of pod limits |
| `/tmp` (PVC `subPath: tmp`) | No | PVC-backed |
| `/workspace` (PVC `subPath: workspace`) | No | PVC-backed |
| `/home/sandbox` (PVC `subPath: home`) | No | PVC-backed |
| `/sandbox-cfg` (emptyDir, `Medium: Memory`) | No | Counts toward memory, not ephemeral storage |

Container logs are the only consumer, and kubelet's own log rotation already bounds them. A per-pod ephemeral-storage limit added no protection beyond that. If a future feature introduces a node-disk-backed `emptyDir` (`Medium: ""`), per-pod ephemeral limits would need to come back, scoped to the actual concern.

---

## Technology Stack

| Component | Technology | Reason |
|-----------|-----------|--------|
| API language | Go 1.25 | Type-safe, strong concurrency, idiomatic for K8s ecosystem |
| API framework | Gin | High-performance HTTP framework with middleware support |
| Controller framework | controller-runtime | Standard Kubernetes controller pattern |
| Database | PostgreSQL (pgx/v5) | Relational data for users, API keys, workspace metadata |
| Cache | Redis (go-redis/v8) | Caching, rate limiting |
| Auth | JWT (golang-jwt/v5) + API keys | Stateless auth with `lsp_` prefixed API keys |
| MCP server | mark3labs/mcp-go | MCP server SDK (stdio + SSE transports) |
| Config | Viper | YAML config + env var overrides |
| Logging | go.uber.org/zap | Structured logging with sensitive data filtering |
| Metrics | Prometheus (client_golang) | Standard K8s observability |
| Validation | go-playground/validator | Request and CRD validation |
| API docs | swaggo/swag | Auto-generated Swagger/OpenAPI |
| Security | unrolled/secure | HTTP security headers |
| Code generation | k8s.io/code-generator | DeepCopy for controller CRD types |
| Testing | testify, go-sqlmock, miniredis | Unit and integration testing |
| Runtime images | Debian bookworm-slim (digest-pinned) | Small attack surface; SHA256-verified binaries |
| Runtime manager | mise (jdx/mise) | Polyglot runtime manager — agents install Python/Node/Go/etc. without root |
| Secret redaction | pkg/redact (internal) | 16-rule regex pipeline; prevents credential leaks in agent output |

---

## Worklog Requirements

Worklogs are **mandatory**. They are the institutional memory of this project. Every meaningful session must produce a worklog entry. This is not optional.

### When to write a worklog

Write a worklog entry after **any** of the following:

- Completing a user story or part of one
- Making an architectural decision
- Discovering a bug or unexpected behaviour
- Completing a design document
- Running into a blocker
- Starting or finishing a feature branch
- Any session longer than 30 minutes of work

If in doubt: **write the worklog**.

### Worklog file naming

```
NNNN_YYYY-MM-DD_short-description.md
```

- `NNNN` is a literal sentinel placeholder — **do not pick a number yourself**. The post-merge bot assigns the real sequential number when your PR merges and comments the assigned number on the merged PR.
- Date is the actual date the work was done
- Description is lowercase, hyphen-separated, 3–6 words
- The pre-commit hook blocks new worklogs that don't match the `NNNN_` prefix

Examples:

```
NNNN_2026-05-01_initial-project-setup.md
NNNN_2026-05-02_api-service-foundation.md
NNNN_2026-05-03_controller-tdd-sandbox.md
```

After merge, the bot renames them:

```
0545_2026-05-01_initial-project-setup.md
0546_2026-05-02_api-service-foundation.md
0547_2026-05-03_controller-tdd-sandbox.md
```

**Why sentinels:** picking numbers manually races under concurrent PRs (two branches both observe `max=543`, both pick `544`, merge collision). The sentinel scheme eliminates the race — the bot assigns numbers atomically at merge time, serialized by GitHub's sequential merge-commit ordering.

### Worklog format

Every worklog entry must follow this exact structure:

```markdown
# Worklog: <Short Title>

**Date:** YYYY-MM-DD
**Session:** <brief description of what this session was about>
**Status:** Complete | In Progress | Blocked

---

## Objective

What was the goal of this session?

---

## Work Completed

<Per-session entries — one ### subsection per logical unit of work>

---

## Key Decisions

List any decisions made and the rationale behind them. If a decision was
made without enough information, note that and flag it for follow-up.

---

## Blockers

List anything that is blocking progress. Include what information or action
is needed to unblock. If none, write "None."

---

## Tests Run

List test commands run and their outcomes. If no tests were run, explain why.

---

## Next Steps

What should the next session start with? Be specific enough that a fresh
context can pick up immediately without re-reading everything.

---

## Files Modified

List every file created or modified in this session.
```

### Worklog discipline rules

1. **Write it before ending the session** — not the next day. Memory degrades fast.
2. **Be specific** — vague entries like "worked on controller" are useless. Name the functions, the decisions, the line numbers if relevant.
3. **Document decisions with rationale** — not just what was decided, but why. Future sessions will need to understand the reasoning, not just the outcome.
4. **Record blockers immediately** — if you are blocked, write it down. Do not silently skip the entry.
5. **List every file touched** — this makes it trivial to audit what changed in a session.
6. **Next steps must be actionable** — "continue implementation" is not actionable. "Implement `CreateSandbox()` in `pkg/secrets/secret_service.go` and write tests first per TDD" is actionable.
7. **Never retroactively rewrite a worklog** — worklogs are append-only history. If something was wrong, note the correction in the next entry.

---

## Development Workflow

### Before starting work

1. **Install pre-commit hooks** — run `make install-hooks` immediately after cloning. This is not optional. Every commit runs repolint, gofmt, goimports, golangci-lint, and helm-render checks. Without hooks installed, broken commits reach CI and waste time.
2. Read `README-LLM.md` (this file)
3. Read the relevant design document(s) from `design/` — see the table in [Rule 8](#8-understand-the-architecture-first)
4. Read `pkg/README.md` for shared package conventions
5. Check recent git history to understand current state of the area you're modifying

### Branch and PR workflow (MANDATORY)

**Never push directly to main.** Every change — no matter how small — follows this cycle:

1. **Create a feature branch** from main: `feat/`, `fix/`, `test/`, `chore/`, or `security/` prefix.
2. **Do the work** — TDD, write code, run tests locally.
3. **Push the branch and open a PR.**
4. **Wait for the automated review** — the AI reviewer triggers on every PR open and push.
5. **Read every finding.** Fix all real issues. Push to the same branch (triggers re-review).
6. **Iterate** — repeat steps 4–5 until the automated reviewer posts **APPROVE**.
7. **Merge** — only after approval. Use squash merge.
8. **Write a worklog entry** if the session was substantive.

This applies to humans and AI agents equally. No exceptions. The review-iterate-approve-merge cycle is the quality gate — skipping it defeats the purpose of having it.

### During work

1. Write tests first — TDD, always
2. Use strongly-typed structs (see `pkg/types/types.go` for existing domain types)
3. Commit at each logical unit of work with a descriptive message

### After completing work

1. Run all tests: `make test` or `go test -timeout 30s -race ./...`
2. Run linter: `make lint`
3. Verify tests pass
4. **Write a worklog entry** (see [Worklog Requirements](#worklog-requirements))
5. Commit everything

### Go module downloads in restricted environments

If `proxy.golang.org` is unreachable (common in sandboxed/air-gapped dev environments), use `GOPROXY=direct` to download modules directly from source repositories (GitHub, etc.):

```bash
# Download all modules (bypassing proxy.golang.org and sum.golang.org)
GOPROXY=direct GONOSUMCHECK=* GONOSUMDB=* go mod download

# Run tests with direct proxy
GOPROXY=direct GONOSUMCHECK=* GONOSUMDB=* go test -timeout 120s -short ./...

# Build with direct proxy
GOPROXY=direct GONOSUMCHECK=* GONOSUMDB=* go build ./...
```

This works whenever the source repos (e.g. github.com) are reachable even if the Go module proxy is not.

---

## Multi-Agent Workflow

This section defines two agent roles and their workflows for collaborative or multi-step development.

**IMPORTANT:** These workflows are MANDATORY when working on epics, user stories, or complex multi-step tasks.

---

### Agent Role 1: Orchestrator Agent

**Purpose:** Coordinate multiple delegations to complete epics, stories, or complex multi-step tasks.

**When to use:**

- Working on epic-level features (e.g., new runtime environment, new CRD)
- User story implementation requiring multiple sub-tasks
- Complex refactoring or architectural changes
- Coordinating work across `api/`, `controller/`, `pkg/`, and `runtimes/`

#### Orchestrator responsibilities

1. **Context distribution** — Ensure all delegations have access to critical documentation
2. **Scope definition** — Define clear boundaries, ownership, and integration points
3. **Quality enforcement** — Validate work meets standards through code review and testing
4. **Gap detection** — Identify and resolve integration gaps between sub-tasks
5. **Integration validation** — Ensure all components work together end-to-end
6. **Testing coordination** — Run comprehensive builds and tests across the entire repository
7. **Worklog management** — Create completion worklogs documenting the entire epic/story

#### Orchestrator workflow (11-step process)

Follow this workflow for all epic/story implementation tasks. Steps 2–5 form the **Validator Loop** — they are MANDATORY and must run until the validator returns zero findings. There is no "good enough" exit.

```
1. Context Setup
   └─> Delegate: "Read README-LLM.md, relevant design docs"
   └─> Include: Design constraints, architectural patterns, integration points
   └─> Define: Clear scope, ownership boundaries, expected deliverables
   └─> Require: Assumptions stated and validated (per Rule 7)

2. Implementation Delegation
   └─> Delegate: User story implementation (per Rule 0 — TDD)
   └─> Prompt detail level: "Fresh developer seeing codebase for first time"
   └─> Include: Specific file references, pattern examples
   └─> Require: Happy + unhappy + e2e integration tests (per Rule 0)
   └─> Require: Stated assumptions list with validation evidence (per Rule 7)

3. Skeptical Validator Delegation (MANDATORY)
   └─> Delegate to a SEPARATE sub-agent acting as a skeptical validator
   └─> Validator's job: assume nothing works; prove every claim
   └─> Validator must check (per Rule 11):
       - Stated assumptions — actually true? (re-validate independently)
       - Integration points — wired into the live request path?
       - Test coverage — happy + unhappy + e2e/integration all present and meaningful?
       - Engineering principles (per Rule 4)
       - Spirit AND letter of the ask
       - Tech debt — any TODOs, hacks, workarounds, dead code?
   └─> Output: Detailed findings report with code references and severity
   └─> Validator MUST NOT also be the implementer (independence is the point)

4. Findings Triage and Remediation Delegation
   └─> Validate each finding is REAL (per Rule 11 Phase 2)
   └─> Document false alarms with rationale; do NOT silently dismiss
   └─> Delegate fixes for ALL real findings (per Rule 5 — zero tech debt)
   └─> Each fix must include a regression test

5. Re-Validate (LOOP)
   └─> Send remediated code BACK to a skeptical validator
   └─> If new findings: return to Step 4
   └─> If zero findings: exit the loop
   └─> NO compromises: loop continues until validator returns zero real findings

6. Build and Test Validation
   └─> Run: `make build && make test && make lint`
   └─> Fix ALL failures regardless of relevance to current work (per Rule 5)

7. Commit and Push
   └─> git add/commit/push with descriptive message referencing story/epic

8. Worklog Creation
   └─> Create worklog per Worklog Requirements section

9. Move to Next Story
   └─> Validate no integration gaps between previous and current story
   └─> Repeat from Step 1

10. Integration Gap Check
    └─> Validate integration between stories (imports, service registration, CRD schema)

11. Final Validation
    └─> Run full repository test suite one final time
```

#### Orchestrator delegation guidelines

**Prompt quality standards:**

- Detail level: "Instructions for a developer seeing the codebase for the first time"
- Specificity: Include exact file paths, function names, pattern references
- Context: Provide architectural context, design decisions, trade-offs
- Boundaries: Clear scope limits, what is in/out of scope, integration points
- Examples: Reference similar implementations and established patterns

**Delegation prompt template:**

```
CONTEXT:
- Primary doc: README-LLM.md (your bible)
- Design docs: [List relevant design/ documents]
- CRD types: pkg/types/types.go
- Design constraints: [TDD, type safety, etc.]

SCOPE:
- Objective: [Clear, specific goal]
- Boundaries: [What is included, what is excluded]
- Integration points: [How this connects to existing code]
- Ownership: [Which files/packages this delegation owns]

REQUIREMENTS:
- MUST read README-LLM.md
- MUST read relevant design documents
- MUST follow TDD (tests first)
- MUST use established patterns
- MUST validate integration points
- MUST create worklog

DELIVERABLES:
1. [Specific deliverable 1 with acceptance criteria]
2. [Specific deliverable 2 with acceptance criteria]

SUCCESS CRITERIA:
- All tests passing (make test)
- All builds successful (make build)
- Integration points validated
- Code follows established patterns
- Worklog created
```

#### Orchestrator principles

**Respect other agents:**

- Multiple agents may work simultaneously in the same repository
- NEVER perform indiscriminate destructive git operations (`git checkout .`, `git clean -fd`)
- Define clear ownership boundaries to avoid conflicts between `api/`, `controller/`, `pkg/`

**Thoroughness:**

- Proof of work = code + tests, NOT status updates
- Integration points MUST be identified and updated
- Sufficient end-to-end and integration tests for happy/unhappy paths
- NO gaps acceptable, no matter how minor

**Quality gates:**

- Code review before merge
- ALL tests passing before next story
- ALL builds successful before next story
- Worklog created before task closure

**Proper fixes only:**

- ALWAYS use the proper fix
- NEVER use workarounds, hacks, or shortcuts

---

### Agent Role 2: Delegation Agent

**Purpose:** Execute specific, well-scoped tasks as part of a larger epic or story.

**When to use:**

- Implementing a specific service or reconciler
- Writing tests for a component
- Code review of another agent's work
- Fixing a specific bug or gap
- Integrating a component into the main codebase

#### Delegation agent responsibilities

1. **Context acquisition** — Read ALL assigned documentation (per Rule 8)
2. **Scope adherence** — Stay within defined boundaries; ask orchestrator if unclear
3. **Pattern following** — Use established patterns; check similar implementations
4. **TDD compliance** — Per Rule 0
5. **Integration awareness** — Identify and document integration points
6. **Quality standards** — Per Rules 1–5 (type safety, error handling, zero tech debt)
7. **Worklog creation** — Document work performed if completing a task

#### Delegation agent workflow

**Standard implementation task:**

```
1. Read Required Documentation (per Rule 8)
2. Understand Context — review delegation prompt, scope boundaries, integration points
3. Plan Implementation — break into sub-tasks, identify test scenarios and patterns
4. Write Tests FIRST (per Rule 0)
5. Implement — follow established patterns (per Rules 1–4)
6. Validate — `make test && make build`, verify integration points
7. Create Worklog (per Worklog Requirements section)
8. Report Back to Orchestrator — completion status, gaps, integration validation
```

**Code review task (per Rule 11):**

```
1. Read Code with Skeptical Mindset — assume nothing works until proven
2. Validate Against Standards — rules followed? TDD? type safety? patterns?
3. Integration Point Analysis — all identified, tested, end-to-end flows work?
4. Gap Identification — document every gap with code references and fix recommendations
5. Report Generation — clear descriptions, severity, NO APPROVAL until all gaps fixed
```

#### Delegation agent principles

- **Read first, ask later:** Always read README-LLM.md and relevant docs before work (per Rule 8). Check `pkg/types/types.go` for existing types before creating new ones.
- **Follow patterns:** Check similar implementations; use established patterns. Do not invent new patterns without approval.
- **TDD:** Tests before code, always (per Rule 0).
- **Quality:** Type safety (per Rule 1), explicit error handling (per Rule 3), no TODOs or placeholders (per Rule 5).
- **Communication:** Report completion clearly, document gaps/uncertainties, ask when scope is unclear.

---

### Common failure modes

| Role | Failure Mode | Consequence |
|------|-------------|-------------|
| Orchestrator | Insufficient detail in delegation prompts | Delegation confusion, pattern violations |
| Orchestrator | Skipping integration validation | Code works in isolation but fails together |
| Orchestrator | Not aligning api/ and controller/ types | CRD schema drift, runtime failures |
| Delegation | Not reading README-LLM.md | Pattern violations, rule violations |
| Delegation | Scope creep | Conflicts with other agents, boundary violations |
| Delegation | Creating new types instead of using pkg/types/ | Duplicate types, conversion errors |
| Both | No worklog | Lost context, incomplete task tracking |

---

## PR Review Guide

Every PR must be reviewed against the rubric below before merging. Score each dimension 1–10; a score of **9 or higher** is required on every dimension. For each dimension, list specific remediation items needed to reach ≥9.

### Quality Rubric & Scoring

#### Robustness

**Definition:** Handles failures, partial states, and adversarial inputs without corruption or data loss.

| Score | Criteria |
|-------|----------|
| 1–3 | No error handling; panics on unexpected input; no recovery from partial failure |
| 4–6 | Basic error returns but some paths silently ignored; no retry/backoff; crashes on dependency failure |
| 7–8 | All errors handled explicitly; retry with backoff on transient failures; graceful degradation |
| 9–10 | Every failure mode enumerated and tested; circuit breakers; defensive coding against all inputs; provably correct under partial failure. **Verify:** every function handles documented error returns; integration tests for each dependency failure; no silent error swallowing; external inputs validated at boundary; recovery from partial state |

**Definition:** Performance characteristics hold as load, data volume, and concurrency increase.

| Score | Criteria |
|-------|----------|
| 1–3 | O(n²) or worse on hot paths; no pagination; global locks on every request |
| 4–6 | Linear scans where indexed lookups exist; per-request expensive allocations; no connection pooling |
| 7–8 | Bounded loops; pagination on list endpoints; connection pooling; no per-request resource exhaustion |
| 9–10 | Verified O(1) or O(log n) on all hot paths; horizontal scalability demonstrated; no hidden N+1 queries; resource limits enforced. **Verify:** no N+1 query patterns; list endpoints use pagination; no unbounded goroutines/slice growth; connection pools sized and reused; no per-request lock on shared resources |

#### Maintainability

**Definition:** Code is readable, well-structured, and follows established patterns; a new contributor can modify it confidently.

| Score | Criteria |
|-------|----------|
| 1–3 | No tests; no doc comments; monolithic functions; inconsistent naming |
| 4–6 | Some tests but low coverage; mixed patterns; unclear data flow; magic numbers |
| 7–8 | Good test coverage; clear naming; small focused functions; follows project conventions |
| 9–10 | Self-documenting code; no unnecessary comments; consistent patterns throughout; a junior engineer can read and modify safely. **Verify:** functions ≤50 lines; naming follows Go conventions; no duplicate/near-duplicate code; every struct has single responsibility; no TODOs/FIXMEs/commented-out code |

#### Reliability

**Definition:** Deterministic, repeatable behaviour; no flaky tests; consistent results across environments.

| Score | Criteria |
|-------|----------|
| 1–3 | Non-deterministic behaviour; race conditions; flaky tests ignored |
| 4–6 | Some races handled; tests occasionally flaky; no timeout on external calls |
| 7–8 | Race-free in normal operation; stable tests; timeouts on all external calls |
| 9–10 | Race-free at high concurrency; all tests pass consistently with `-race`; timeout and deadline propagation everywhere. **Verify:** tests pass with `-race`; all external calls have timeouts; no flaky tests; no shared mutable state without synchronisation; all mutation endpoints idempotent |

#### Performance

**Definition:** Efficient use of CPU, memory, and I/O; no unnecessary pessimisation.

| Score | Criteria |
|-------|----------|
| 1–3 | Unbounded memory allocations; synchronous I/O on hot paths; no caching |
| 4–6 | Some caching but misses common patterns; unnecessary copies of large objects |
| 7–8 | Proper use of pointers, reuse, and pooling; async I/O where beneficial; cache headers |
| 9–10 | Benchmark-driven optimisation; zero-copy paths where possible; measured and documented trade-offs. **Verify:** no unnecessary heap allocations in hot loops; JSON marshal/unmarshal not on every response; no synchronous I/O in hot handler without justification; profiled with realistic load |

#### Security

**Definition:** Input validated, outputs sanitised, secrets never logged, least-privilege by default.

| Score | Criteria |
|-------|----------|
| 1–3 | No input validation; secrets logged; no auth on endpoints |
| 4–6 | Basic validation but bypassable; secrets may leak in error messages; broad permissions |
| 7–8 | All inputs validated at boundary; secrets filtered from logs; least-privilege RBAC |
| 9–10 | Defence in depth; no user data in error messages; injection-proof by construction; security tests for every control. **Verify:** no secrets in logs/errors/responses; user input validated (length/type/range/chars); permission checks in service layer; parameterised queries only; security tests for every control; rate limiting and body size limits applied |

#### Test Coverage & Quality

**Definition:** Tests exist at the right levels, cover happy+unhappy paths, and are reliable.

| Score | Criteria |
|-------|----------|
| 1–3 | No tests, or tests don't actually assert anything |
| 4–6 | Some unit tests but no unhappy paths; no integration tests |
| 7–8 | Good unit coverage + unhappy paths + integration/e2e tests; table-driven |
| 9–10 | Comprehensive coverage at all levels; TDD followed; tests run with `-race`; no flaky tests. **Verify:** table-driven tests cover happy and unhappy paths; e2e/integration tests exercise real wiring; tests pass with `-race -count=1`; test utilities reduce boilerplate; no tests depend on external services without mock/fake |

#### SOLID Compliance

**Definition:** Follows Single Responsibility, Open/Closed, Liskov Substitution, Interface Segregation, and Dependency Inversion principles. Every type has one clear reason to change; abstractions are stable; dependencies flow inward.

| Score | Criteria |
|-------|----------|
| 1–3 | Violates multiple SOLID principles; god objects; concrete coupling everywhere; impossible to test in isolation |
| 4–6 | Some SRP violations; mixed abstraction levels; some coupling to concrete types; partial testability |
| 7–8 | Mostly SOLID; clear interfaces; dependency injection; small focused types; testable in isolation |
| 9–10 | Fully SOLID by construction; every type has one reason to change; abstractions are caller-shaped not implementation-shaped; high-level modules never import low-level details. **Verify:** every type has single responsibility; interfaces are small (1–3 methods) and caller-shaped; no concrete dependency where interface would serve; new variants don't require modifying existing types; high-level modules don't import low-level details |

#### Right-Sized Complexity

**Definition:** The code is exactly as complex as it needs to be — no more (over-engineered), no less (under-engineered). Abstractions earn their keep. 10 is perfect; scores decrease in either direction.

| Score | Criteria |
|-------|----------|
| 10 | Perfectly sized — abstraction level matches the problem; every interface has ≥2 implementations or a clear imminent need; no speculative generality; a junior engineer can follow the flow. **Verify:** every interface has ≥2 implementations or imminent need; functions >30 lines justifiable; no single-implementation abstractions; new features add code not modify abstraction layer; simplest correct solution chosen |
| 7–9 | Slightly off — one unnecessary abstraction layer OR one missing abstraction that would simplify callers. Functions and type boundaries are mostly right |
| 4–6 | Noticeably off — speculative abstractions with no current consumer, or monoliths that should be split. Multiple indirection layers without value |
| 1–3 | Severely wrong — framework-in-disguise (unnecessary factories/visitors/strategies for a simple CRUD path), or giant monolithic functions with no decomposition. Actively reduces productivity |

### E2E Wiring Verification

Beyond scoring individual dimensions, every PR must verify that all expected user workflows and system pathways are fully wired end-to-end. "Wired" means the code is connected through the full request path — entry point, middleware, service/controller logic, data store interaction, response propagation, and error handling at every step.

#### Process

1. **List every expected workflow** affected by this PR:
   - User-facing operations (create sandbox, send message, suspend workspace, etc.)
   - System operations (reconciliation loop, webhook validation, credential injection, etc.)
   - Background operations (cache eviction, metrics collection, health checks)
   - Error/recovery paths (dependency failure, invalid state, timeout)

2. **For each workflow, trace the full path:**
   - Entry point (REST endpoint, CRD event, CLI command, timer)
   - Middleware/authorisation layer
   - Service/controller logic
   - Data store interaction (DB, Redis, K8s API)
   - Response or propagation back to caller
   - Error handling and rollback at every step

3. **Confirm wiring with evidence:**
   - Integration test that exercises the real path (router → service → store)
   - Or, for paths that cannot be integration-tested, a documented manual verification with output
   - **"It compiles" or "unit tests pass" is NOT sufficient** — the actual wiring must be demonstrated

4. **Identify and flag unwired code:**
   - Any handler, service, or function that was built but never called from a live request path
   - Any code path guarded by a dead conditional (env var never set, feature flag never enabled)
   - These are not acceptable — either wire them or remove them

5. **Common wiring failures to check:**
   - New handler not registered in the router
   - New service not initialised in the service bootstrap (`services.go`)
   - New CRD type not registered in the scheme
   - New reconciler not added to the controller setup
   - New migration not included in the startup sequence
   - New middleware not added to the chain
   - New error type not handled in the error handler middleware
   - New permission not checked in the authorisation layer
   - New mock missing a method (silent no-op in tests)

This verification must be documented in the final PR review report. Unwired code is dead code and is not acceptable.

### Adversarial Assessment

In addition to the rubric scoring, every PR must undergo a structured adversarial review per [Rule 11 — Adversarial Self-Review](#11-adversarial-self-review). Apply Rule 11 Phases 1–2 as written, with these PR-specific additions:

**PR-specific omissions checklist (add to Phase 1):**

- Missing input validation
- Missing authentication/authorisation checks
- Missing logging for debugging
- Missing metrics for monitoring
- Missing timeout/deadline propagation

#### Phase 3: Final Report

The final PR review report must contain:

- Scores for each quality dimension (1–10) with specific remediation items
- E2E wiring verification results — which workflows were traced, evidence for each, and any unwired code identified
- List of validated adversarial findings (real bugs and design flaws)
- List of false alarms with rationale for each
- A pass/fail recommendation — fail unless all real findings are fixed, no unwired code exists, and all dimensions score ≥9

---

## Common Commands

```bash
# --- Root module ---

# Tidy dependencies
go mod tidy

# Run all tests
make test

# Run tests with verbose output and timeout
go test -timeout 30s -race -v ./...

# Run tests with coverage
make cover

# Format code
make fmt

# Static analysis
make vet

# Lint
make lint

# Build API binary
make build

# Cross-compile for Linux amd64
make build-linux

# Docker build
make docker-build

# --- API service (from api/) ---

# Build API service
cd api && make build

# Run API service locally
cd api && make run

# Run database migrations up
cd api && make migrate-up

# Rollback database migrations
cd api && make migrate-down

# --- Controller ---

# Build controller binary
cd controller && go build -o bin/manager .

# Run controller locally (against current kubeconfig)
cd controller && go run ./main.go --enable-leader-election=false

# Install CRDs into cluster
cd controller && bash scripts/install-crds.sh

# --- Code generation ---

# Regenerate DeepCopy methods (after modifying pkg/types/types.go)
make deepcopy
# Or manually:
# hack/update-deepcopy.sh

# --- Docker (local development) ---

# Build API image
make docker-build

# Run API image
make docker-run
```

---

## Testing Requirements

### TDD and coverage requirements

See [Rule 0 — Test Driven Development](#0-test-driven-development-tdd) for the mandatory TDD workflow, test requirements, and definition of done.

### Table-driven tests

Use table-driven tests with `t.Run()` for any function with multiple input cases:

```go
func TestCreateWorkspace(t *testing.T) {
    tests := []struct {
        name    string
        req     types.CreateWorkspaceRequest
        wantErr bool
    }{
        {"valid workspace", types.CreateWorkspaceRequest{Runtime: "base", Name: "test"}, false},
        {"empty name", types.CreateWorkspaceRequest{Runtime: "base", Name: ""}, true},
        {"invalid storage size", types.CreateWorkspaceRequest{Runtime: "base", Name: "test", StorageSize: "-1"}, true},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            _, err := svc.CreateWorkspace(ctx, tt.req)
            if (err != nil) != tt.wantErr {
                t.Errorf("CreateWorkspace() error = %v, wantErr %v", err, tt.wantErr)
            }
        })
    }
}
```

### Always use timeout

```bash
# Good
go test -timeout 30s -race ./...

# Bad — can hang forever
go test ./...
```

### Mock conventions

- Service mocks live in `api/internal/mocks/` and `mocks/` (root)
- Kubernetes mocks use the interface from `pkg/interfaces/kubernetes.go`
- Use `testify/mock` for mock generation
- Database tests use `go-sqlmock`
- Redis tests use `miniredis` (in-memory Redis)

### Code generation

When modifying CRD types in `pkg/apis/llmsafespaces/v1/*_types.go`, you must regenerate the DeepCopy implementations:

```bash
# From project root
make deepcopy

# Verify and commit generated changes
git add pkg/apis/llmsafespaces/v1/zz_generated.deepcopy.go
git commit -m "Update generated DeepCopy code"
```

`pkg/types/types.go` contains API transfer objects only — no generated deepcopy. Manual `DeepCopy` methods are implemented only where needed (types passed by pointer across goroutine boundaries).

---

## Authentication & Authorization

### Endpoints

| Endpoint | Method | Auth | Purpose |
|----------|--------|------|---------|
| `/api/v1/auth/register` | POST | Public | Create user, return JWT |
| `/api/v1/auth/login` | POST | Public | Email+password login, return JWT |
| `/api/v1/auth/api-keys` | POST | JWT/API Key | Generate `lsp_`-prefixed API key |
| `/api/v1/auth/api-keys` | GET | JWT/API Key | List user's API keys (secrets stripped) |
| `/api/v1/auth/api-keys/:id` | DELETE | JWT/API Key | Revoke an API key |

### Security Controls

| Control | Implementation | Validated By |
|---------|---------------|-------------|
| Password hashing | bcrypt cost 12 | `auth_test.go:TestRegister_Success` |
| Email enumeration prevention | Identical generic errors for duplicate email, wrong password, nonexistent user, inactive user | `router_auth_security_test.go:TestRegister_DuplicateEmail_GenericError`, `TestLogin_WrongPassword_GenericError`, `TestLogin_InactiveUser_GenericError` |
| Password never in response | `json:"-"` on `User.PasswordHash`; verified in e2e tests | `TestRegister_PasswordNotInResponse`, `TestLogin_PasswordNotInResponse` |
| API key secrets stripped on list | `ListAPIKeys` zeroes `Key` field before return | `TestListAPIKeys_SecretsStripped` |
| API key secret returned only on creation | `CreateAPIKey` returns full key; `ListAPIKeys` strips it | `TestCreateAPIKey_SecretOnlyOnCreation` |
| Body size limits | `http.MaxBytesReader` (1 MiB) on all auth endpoints | `TestRegister_BodyTooLarge_Rejected` |
| Sanitized binding errors | Binding failures return generic "invalid request body" | `TestRegister_InvalidJSON_SanitizedError` |
| No internal error leakage | Service errors return generic messages; details logged server-side only | `TestRegister_DuplicateEmail_GenericError` |
| JWT includes `jti` claim | Enables per-token revocation (not per-user) | `auth_test.go:TestGenerateToken` |
| API keys use `crypto/rand` | 32-byte random keys with `lsp_` prefix | `auth_test.go:TestCreateAPIKey_Success` |
| JWT cache keys hashed before Redis storage | `hashToken()` uses MD5 to prevent raw JWT exposure in Redis | `auth.go:hashToken` |
| Token extraction: header-only by default | Query param and cookie extraction disabled | `token_extractor_test.go:Query parameter disabled by default` |
| Rate limiter wired into global middleware stack | `ratelimit.Service` backed by Redis + in-memory token bucket | `ratelimit_test.go` |
| Rate limiter IP fallback | Falls back to `c.ClientIP()` when no API key in context | `rate_limit.go:54-58` |
| Protected endpoints require auth | API key CRUD behind `AuthMiddleware()` | `TestAPIKeyEndpoints_RequireAuth` |
| Wrong HTTP method rejection | Only POST on register/login, returns 404 | `TestRegister_RejectsGet`, `TestLogin_RejectsGet` |
| Turnstile CAPTCHA on `/register` | Optional Cloudflare Turnstile widget gates registration behind bot verification. Feature-flagged via `chart.turnstile.enabled`; disabled by default, enabled on prod. See [Cloudflare Turnstile CAPTCHA](#cloudflare-turnstile-captcha). | `middleware/turnstile_test.go`, `router_auth_turnstile_test.go`, `register-turnstile.spec.ts` |

### E2E Testing

Go tests: `go test -race ./api/internal/server/... -run "TestRegister|TestLogin|TestCreateAPIKey|TestListAPIKeys|TestDeleteAPIKey|TestAPIKeyEndpoints"`

Shell script against running server: `./local/test-auth.sh http://localhost:8080`

---

## Multi-Tenant OIDC SSO

**Status:** Shipped — Epic 43, US-43.10, decisions D17 (see `design/stories/epic-43-organization-management/README.md`). Org owners (org admins) configure their own OIDC identity provider per organization. Login is Authorization Code + PKCE (`coreos/go-oidc/v3`). Each org's IdP config is isolated in its own row; there is no instance-level/global IdP.

### Model

One row per org in `org_sso_configs` (`api/migrations/000038_org_sso_configs.up.sql`), keyed by `org_id`. The org admin supplies the IdP wiring; the platform owns the client secret at rest.

| Column | Type | Notes |
|--------|------|-------|
| `org_id` | `UUID` PK | FK → `organizations(id)` `ON DELETE CASCADE` |
| `oidc_discovery_url` | `TEXT` | IdP `.well-known/openid-configuration` URL |
| `oidc_client_id` | `TEXT` | Public client identifier |
| `oidc_client_secret` | `BYTEA` | **Encrypted at rest** with the server KEK (D17-S4) |
| `claimed_domains` | `TEXT[]` | Email domains that route to this org on the login page; GIN-indexed for domain→org lookup |
| `auto_provision` | `BOOLEAN` | Create a new user on first SSO login if none exists for the email |
| `group_role_mapping` | `JSONB` | `{groupId: "admin"|"member"}`; applied on every login |

Go types in `pkg/types/orgs.go:203-242`:
- `OrgSSOConfig` — DB shape (`ClientSecret []byte`, `json:"-"`)
- `OrgSSOConfigResponse` — API shape (`HasSecret bool` replaces the secret)
- `UpsertSSOConfigRequest` — `PUT` body (empty `ClientSecret` = "leave existing unchanged")
- `SSODomain` — one entry in domain discovery (`domain`, `orgSlug`, `orgName`)

### Endpoints

| Method | Path | Auth | Purpose |
|--------|------|------|---------|
| `GET` | `/api/v1/orgs/:id/sso` | Org admin | Read this org's SSO config (secret omitted) |
| `PUT` | `/api/v1/orgs/:id/sso` | Org admin | Upsert SSO config; encrypts client secret, audits `sso.update` |
| `DELETE` | `/api/v1/orgs/:id/sso` | Org admin | Remove SSO config; audits `sso.delete` |
| `POST` | `/api/v1/orgs/:id/sso/domains/:domain/verify` | Org admin | On-demand DNS verification of a claimed domain (D17 Q-S2) |
| `POST` | `/api/v1/orgs/:id/sso/verification-token/rotate` | Org admin | Generate or rotate the DNS verification token |
| `GET` | `/api/v1/auth/sso/domains` | Public | List all orgs' **verified** domains (for login-page routing) |
| `GET` | `/api/v1/auth/sso/:orgSlug/start` | Public | Begin PKCE flow; 302 to IdP, sets signed state cookie |
| `GET` | `/api/v1/auth/sso/:orgSlug/callback` | Public | Complete flow; sets `lsp_session` JWT cookie, 302 to frontend |

The CRUD routes are registered in `registerOrgRoutes` behind `OrgAdminGuard` (`api/internal/server/router.go:1234-1238`). The public login routes sit under the auth group (`router.go:633-635`). `/auth/config` advertises `oidcEnabled = (CountSSOConfigs > 0)` so the frontend can hide SSO UI when no org has configured it.

### Login flow (PKCE)

```
Browser                  API (stateless)              IdP
  │                         │                          │
  │ 1. GET /auth/sso/<slug>/start                      │
  │ ───────────────────────►│                          │
  │                         │ load org_sso_configs[org] │
  │                         │ oidc.NewProvider(discoveryURL)
  │                         │ generate verifier + state │
  │                         │ sign cookie {state, verifier, orgID, exp} HMAC-SHA256
  │ 2. Set-Cookie: lsp_sso_state=<signed>; SameSite=Lax
  │ 3. 302 Location: <IdP authorize URL>?code_challenge=S256&state=...
  │ ◄───────────────────────│                          │
  │                                                    │
  │ 4. User authenticates at IdP                       │
  │ ──────────────────────────────────────────────────►│
  │ 5. 302 /auth/sso/<slug>/callback?code=...&state=...│
  │ ◄──────────────────────────────────────────────────│
  │ 6. GET /auth/sso/<slug>/callback                   │
  │ ───────────────────────►│                          │
  │                         │ verify state cookie (HMAC, exp, orgID bound to slug)
  │                         │ token exchange (code + verifier) ─►│
  │                         │ verify id_token (provider.Verifier, clientID aud)
  │                         │ enforce email_verified == true (F8)
  │                         │ resolveUser: lookup by email OR auto-provision
  │                         │ resolveRole: highest-priv match in group_role_mapping
  │                         │ ensureMembership: create/update role (last-admin guard)
  │                         │ auth.Service.GenerateToken(userID) → JWT
  │ 7. Set-Cookie: lsp_session=<jwt>; HttpOnly; Secure │
  │ 8. 302 <frontend>/?sso=success                     │
  │ ◄───────────────────────│                          │
```

The state cookie carries `{state, verifier, orgID, exp}` because the API is stateless — there is no server-side PKCE session store. The callback is bound to the org that started the flow (`org.ID == payload.OrgID`), so an attacker cannot start SSO for org A and replay the callback against org B.

### Org admin config flow

`PUT /api/v1/orgs/:id/sso` (`api/internal/handlers/org_sso.go:111`):
1. Handler loads any existing config to capture the current encrypted secret (for the partial-update path).
2. `sso.Service.ApplyConfigMutation` (`services/sso/sso.go:246`):
   - Validates role values (`admin`/`member` only).
   - If `ClientSecret` present → `EncryptClientSecret` with server KEK; if empty → reuse existing blob; if empty and no existing → `400 client secret is required`.
   - `NormalizeDomains` lowercases, strips leading `@`, dedups.
   - `UpsertSSOConfig` (`ON CONFLICT (org_id) DO UPDATE`).
3. Emits `sso.update` to the org audit log.

### Auto-provisioning and role mapping

- **`resolveUser`** (`services/sso/sso.go:606`): lookup by lowercased email; if not found and `auto_provision=true`, create a user with a random unusable bcrypt hash (`$2a$12$<random>`) so password login is permanently blocked — the user has no password to derive a DEK from. Personal credential operations stay unavailable until they set a password; org workspaces still work via server-side injection.
- **`resolveRole`** (`services/sso/sso.go:777`): walk IdP groups (OIDC `groups` ∪ Azure AD `memberOf`); the highest-privilege match wins; `admin` outranks `member`; unmapped/empty → `member` (safe default).
- **`ensureMembership`** (`services/sso/sso.go:645`): create or update the membership row so IdP-driven role changes propagate on every re-login. A demotion `admin→member` is skipped when the user is the sole admin (last-admin protection; logged at WARN).

### Security controls

| Control | Implementation | Reference |
|---------|----------------|-----------|
| Client secret encryption at rest | Server KEK (`RootKeyProvider.Encrypt`), `BYTEA` column | D17-S4, `sso.go:212` |
| PKCE S256 | `code_challenge` derived from random verifier, verifier carried in signed cookie | `sso.go:475,792` |
| State cookie integrity | HMAC-SHA256 over `{state, verifier, orgID, exp}`; constant-time compare | `sso.go:713,730` |
| State cookie expiry | 10-minute TTL (`DefaultStateTTL`) | `sso.go:150` |
| Callback bound to start org | `org.ID == payload.OrgID` check on callback | `sso.go:516` |
| `email_verified` enforcement (F8) | Absent/false → `ErrEmailUnverified` (403) | `sso.go:571` |
| Email-claim trust | `email` only used for account binding when IdP-verified | `sso.go:562-571` |
| Suspended-user block | `user.Status == suspended` → `ErrUserSuspended` | `sso.go:579` |
| Last-admin protection | IdP demotion refused if user is sole org admin | `sso.go:671` |
| Secret never in responses | `OrgSSOConfigResponse.HasSecret` replaces the blob | `orgs.go:222`, `org_sso.go:65` |
| SameSite=Lax state cookie | Survives top-level IdP→callback redirect, blocked on cross-site POST | `org_sso.go:346` |
| IdP-registered redirect URI | Defense-in-depth: the IdP only redirects to registered URIs. `redirectBaseUrl` is now **required** for SSO (fail-loud, F11) — header derivation removed | `org_sso.go:319` |
| Auto-provision off → 403 | `ErrAutoProvisionOff` mapped to `provisioning_disabled` | `sso.go:46`, `org_sso.go:378` |

### Configuration

**Instance plumbing** (cross-cutting, NOT per-org IdP config) in `api/internal/config/config.go:128-138`. Two configuration paths of equal validity:

| Source | When to use |
|--------|-------------|
| Helm chart (`oidc:` block in `values.yaml`) | **Default for chart-managed deploys.** Rendered into the configmap by `helm/templates/configmap-api.yaml`. |
| Env vars (`LLMSAFESPACES_OIDC_*`) | Higher precedence (Viper `AutomaticEnv`); useful for non-Helm deploys or per-pod overrides. |

| Helm key | Env var | Default | Purpose |
|----------|---------|---------|---------|
| `oidc.redirectBaseUrl` | `LLMSAFESPACES_OIDC_REDIRECTBASEURL` | `""` | Absolute base for SSO callback URLs. **Required for SSO** — Start/Callback return a config error if unset, rather than trusting `X-Forwarded-*` headers (F11). Full callback = `{redirectBaseUrl}/api/v1/auth/sso/:orgSlug/callback`. |
| `oidc.frontendRedirectUrl` | `LLMSAFESPACES_OIDC_FRONTENDREDIRECTURL` | `""` | Browser landing URL after SSO callback (e.g. `https://app.example.com`). Empty → `/`. |
| `oidc.stateCookieName` | `LLMSAFESPACES_OIDC_STATECOOKIENAME` | `""` (→ `lsp_sso_state` in Go) | PKCE/state cookie name. Override only on collision. |

The state-cookie signing key is `deriveServerKey("oidc-state-cookie")` (`api/internal/app/app.go:445`), derived from the same master secret as the KEK. When unset, the SSO service constructs but rejects config mutation and login at runtime (`sso.go:429,499`).

**Per-org IdP config** is not in `config.yaml`, `values.yaml`, or the settings system — it is entered by the org admin through the API and stored in `org_sso_configs`.

### Frontend

- **`frontend/src/components/org-admin/OrgSSOTab.tsx`** — org admin SSO config form (discovery URL, client ID, write-only client secret, claimed domains, auto-provision toggle, group→role textarea). Registered as the `sso` tab (admin-only) in `OrgAdminLayout.tsx:58`; routed at `router.tsx` `path: "sso"`.
- **`frontend/src/pages/LoginPage.tsx`** — if `oidcEnabled`, fetches `/auth/sso/domains` and matches the typed email domain against claimed domains, surfacing a "Sign in with {orgName}" button. Surfaces the SSO outcome from the `?sso=` query param (`success|provisioning_disabled|suspended|state_invalid|email_unverified|error`).
- **`frontend/src/api/sso.ts`** — `ssoApi` (`getConfig`, `upsert`, `remove`, `domains`, `ssoRedirectURL(orgSlug)`).

### Known gaps and non-goals

- **DNS verification of `claimed_domains`** — shipped (D17 Q-S2). On-demand verification via `POST /orgs/:id/sso/domains/:domain/verify`; the org admin adds a `TXT _llmsafespaces-verify.<domain> = <token>` DNS record and clicks Verify. Only verified domains appear in the login-page discovery endpoint (`ListSSODomains` filters on `verified_domains`). Existing claimed domains at migration time were grandfathered as verified (operator decision).
- **No instance-level / platform-global OIDC** — every SSO login is org-scoped (`/auth/sso/:orgSlug/...`). A single-IdP-for-the-whole-deployment mode does not exist; `cfg.OIDC` carries only plumbing.
- **No SAML or SCIM** — explicitly deferred per Epic 43 decision D3.
- **No generic org-level settings tier** — org config lives in dedicated normalized tables (`org_policies`, `org_sso_configs`, `org_credentials`), not a key-value `org_settings` table.

### File reference

| Concern | File |
|---------|------|
| OIDC engine (PKCE, auto-provision, role mapping, encryption) | `api/internal/services/sso/sso.go` |
| OIDC unit tests (fake IdP with JWKS) | `api/internal/services/sso/sso_test.go` |
| HTTP handler (CRUD + login + discovery) | `api/internal/handlers/org_sso.go` |
| Handler integration tests + fake IdP helpers | `api/internal/handlers/org_sso_test.go`, `org_sso_idp_helpers_test.go` |
| Store interface + Postgres impl | `api/internal/services/database/pg_org_store.go` (interface `OrgStore:32`, SSO impl `1660-1830`) |
| Store tests | `api/internal/services/database/pg_org_store_sso_test.go` |
| Schema migration | `api/migrations/000038_org_sso_configs.up.sql` |
| API DTOs | `pkg/types/orgs.go:203-242` |
| Router registration | `api/internal/server/router.go:633-635, 1234-1238` |
| Service wiring (KEK, state key) | `api/internal/app/app.go:445-459` |
| Frontend admin UI | `frontend/src/components/org-admin/OrgSSOTab.tsx` |
| Frontend login integration | `frontend/src/pages/LoginPage.tsx`, `frontend/src/api/sso.ts` |
| Design doc (D17 decisions) | `design/stories/epic-43-organization-management/README.md` (Q-S1..Q-S4) |
| Hardening history | worklogs `0372` (F8–F13), `0380` (F8/F9/F10/F11), `0386` (callback URL e2e) |

---

## Cloudflare Turnstile CAPTCHA

**Status:** Shipped — PR #501 (feature), worklog `0595`, deployed to prod 2026-07-04. Feature-flagged via chart values; **disabled by default**, currently **enabled on `safespaces.thekao.cloud`**.

Gates `POST /api/v1/auth/register` behind Cloudflare's Turnstile CAPTCHA. When enabled, the frontend renders the Turnstile widget on the register page and the API middleware validates the client-supplied token against Cloudflare's siteverify endpoint before invoking `auth.Register`. Blocks automated account creation without imposing friction on legitimate users (Turnstile "managed" mode resolves invisibly for well-behaved browsers).

### Feature gate

Everything hangs off `chart.turnstile.enabled`. When `false` (chart default), the entire feature is a no-op:
- API: middleware is not installed on `/register` (the `if turnstile.Enabled` branch in `registerAuthRoutes` skips it)
- Frontend: `env.turnstileSiteKey` is empty, widget component returns `null`, submit button is not gated on any token
- CSP: no relaxation applied (chart's `regexReplaceAll` + API's `addTurnstileToCSP()` both no-op)

When `true`, the wire-up is deliberately fail-closed at every layer.

### Wire-up (chart → deployment → runtime)

| Concern | Location |
|---------|----------|
| Feature toggle | `helm/values.yaml:turnstile.enabled` (default `false`) |
| Public site key (rendered into HTML) | `helm/values.yaml:turnstile.siteKey` → substituted by Flux from cluster-config `TURNSTILE_SITE_KEY` |
| Secret key (server-side verification) | K8s Secret `llmsafespaces-credentials` key `turnstile-secret`, populated by ExternalSecret from AWS Secrets Manager |
| Verify URL (default: Cloudflare production) | `helm/values.yaml:turnstile.verifyURL` |
| Frontend deployment env | `TURNSTILE_SITE_KEY` (public value) — `helm/templates/frontend-deployment.yaml` |
| API deployment env | `LLMSAFESPACES_TURNSTILE_{ENABLED,SECRETKEY,VERIFYURL}` — `helm/templates/api-deployment.yaml`; `SECRETKEY` via `secretKeyRef` so it never lands in a rendered ConfigMap |
| CDK context values | `llmsafespaces:turnstileSecretArn`, `llmsafespaces:turnstileSiteKey` in `cdk.context.json` |
| cluster-config keys emitted by CDK | `TURNSTILE_SECRET_ARN`, `TURNSTILE_SITE_KEY` (PlatformStack) |
| ExternalSecret pulls the secret ARN → K8s Secret | `kubernetes/apps/llmsafespaces/llmsafespaces/externalsecret/externalsecret.yaml` (ops-prod repo) |

### API middleware

`api/internal/middleware/turnstile.go` — gin middleware installed conditionally on `/auth/register` when `turnstile.Enabled=true`. Fails closed on every failure mode with `HTTP 401 {"error":"turnstile_failed","reason":<code>,"detail":<human>}`:

| Fail mode | `reason` |
|-----------|----------|
| Missing token (no header + no form field) | `missing_token` |
| siteverify HTTP error / timeout | `verify_unavailable` |
| siteverify returns non-200 | `verify_unavailable` |
| siteverify returns `success:false` | `rejected` (Cloudflare's error-code list is passed through in `detail`) |

Additionally, `verifyTurnstileToken` returns a `no_secret_configured` internal marker when `SecretKey==""`, but this state is normally unreachable: the config startup guard (`applyTurnstileEnv`, see "Config startup guard" below) refuses to start the API in that state. If somehow reached at request time, the middleware surfaces it via `respondTurnstileFail(c, "rejected", "no_secret_configured")` — i.e. `reason: "rejected"`, `detail: "no_secret_configured"`. The internal marker is a `detail` string, not a client-facing `reason` code.

Token extraction order (first match wins):
1. Header `cf-turnstile-response` (production path — the frontend uses JSON, so this is the only real path)
2. Form field `cfTurnstileResponse` (form-encoded body only; the frontend never uses this, kept for callers that submit `application/x-www-form-urlencoded`)

Client IP is forwarded to Cloudflare's siteverify as the `remoteip` fraud-scoring hint (leftmost of `X-Forwarded-For`, else `CF-Connecting-IP`, else gin's `c.ClientIP()`). This is **not** used for any access-control decision, so bypassing gin's `TrustedProxies` model is intentional — spoofing here at worst degrades Cloudflare's own scoring.

### Config startup guard (fail-closed)

`api/internal/config/config.go:applyTurnstileEnv()` refuses to start the API with `Turnstile.Enabled=true` and `Turnstile.SecretKey==""`. This surfaces a config bug at boot (obvious log + non-zero exit) rather than silently accepting every registration.

### Content Security Policy relaxation

The default frontend + API CSP is `script-src 'self'`. Turnstile loads its client script from `https://challenges.cloudflare.com/turnstile/v0/api.js` and renders the challenge in an iframe served from the same origin — both operations are blocked by the default CSP. When `turnstile.enabled=true`, the CSP is conditionally extended in two surfaces:

- **Chart (nginx-ingress)**: `templates/frontend-ingress.yaml` uses `regexReplaceAll` on the `nginx.ingress.kubernetes.io/configuration-snippet` annotation to append `https://challenges.cloudflare.com` to `script-src` and synthesize a `frame-src` directive.
- **API (SecurityConfig middleware)**: `api/internal/app/app.go:addTurnstileToCSP()` performs the equivalent transform on `securityCfg.ContentSecurityPolicy` at server startup. Idempotent + guards against duplicating the origin.

Both surfaces have dedicated tests that verify (a) the transform is correct when enabled, (b) the default CSP is unchanged when disabled — no accidental broadening. The CSP exception itself is documented in `design/0027_2026-05-24_security-policy-v21.md` Appendix D.

### 401 redirect exclusions (frontend)

The frontend's global fetch wrapper (`frontend/src/api/client.ts:handleUnauthorized`) normally redirects to `/login` on any 401. Turnstile's 401 needs to be excluded so users stay on `/register` to re-challenge instead of being bounced away:

- Path-based: `/auth/register` joins `/auth/me` in the `noRedirectPaths` set
- Body-based: any 401 with `body.error==='turnstile_failed'` is exempt regardless of path (guards against a regression where a future endpoint adds Turnstile but forgets to update `noRedirectPaths`)

### Frontend widget

`frontend/src/components/auth/TurnstileWidget.tsx` — React wrapper around Cloudflare's Turnstile client script. Loads once (cached across mounts via a module-level singleton `loadPromise`), renders in flexible/auto-theme mode, cleans up on unmount via `window.turnstile.remove(widgetId)`. When `siteKey` is empty (feature disabled), the component returns `null` and never loads the script.

`RegisterForm.tsx` renders the widget when `env.turnstileSiteKey` is non-empty and gates submit until the widget issues a token. On backend `turnstile_failed` error, the token is cleared so the user re-challenges before retrying.

### Endpoints affected

| Endpoint | Turnstile behavior |
|----------|-------------------|
| `POST /api/v1/auth/register` | Middleware installed when enabled; token required |
| `POST /api/v1/auth/login` | Not gated — different threat model (credential-stuffing already covered by CF rate-limit + auth lockout) |
| `POST /api/v1/auth/lookup`, `/sso/domains` | Not gated — enumeration-safe by design; CAPTCHA friction would degrade legitimate login-discovery UX |

### Tests

| Layer | File | Coverage |
|-------|------|----------|
| Middleware unit (9 tests) | `api/internal/middleware/turnstile_test.go` | valid, missing token, cloudflare rejects, unreachable, 5xx, form-field, header-precedence, XFF-remoteip, no-secret |
| Config unit (4 tests) | `api/internal/config/config_test.go` | default-disabled, enabled+secret, fail-closed guard, verify-URL override |
| CSP transform unit (4 tests) | `api/internal/app/csp_turnstile_test.go` | extend both directives, idempotency, synthesize when absent |
| Router integration (5 tests) | `api/internal/server/router_auth_turnstile_test.go` | wires middleware onto `/register` when enabled; naked when disabled; `authSvc.Register` gated on token validity |
| Chart CSP (2 tests) | `helm/chart_test.go` | ingress CSP extended when enabled; unchanged when disabled |
| Frontend widget (7 tests) | `frontend/src/components/auth/TurnstileWidget.test.tsx` | render lifecycle, callbacks, cleanup, siteKey-empty short-circuit |
| Frontend form enabled-path (4 tests) | `frontend/src/components/auth/RegisterForm.test.tsx` | widget renders, submit gated on token, token forwarded, turnstile_failed re-challenge |
| Frontend client 401 exclusion (2 tests) | `frontend/src/api/client.test.ts` | `/auth/register` and `turnstile_failed` body both bypass the redirect |
| E2E Playwright (5 tests) | `frontend/tests/e2e/register-turnstile.spec.ts` | happy path, disabled path, 3 unhappy paths (missing token, 401 rejected, verify unavailable) with stubbed Cloudflare siteverify |

### Provisioning (once, per deployment)

1. **Cloudflare dashboard** → Turnstile → Add site. Mode: `managed`. Domain: the app hostname. Copy site key + secret key.
2. **AWS Secrets Manager**: `aws secretsmanager create-secret --name llmsafespaces/turnstile-secret --secret-string <SECRET_KEY> --tags Key=llmsafespaces:role,Value=app-secret`. The `llmsafespaces:role=app-secret` tag is required — the ExternalSecrets IRSA role gates access on it.
3. **CDK**: set `llmsafespaces:turnstileSecretArn` and `llmsafespaces:turnstileSiteKey` in `cdk.context.json`, then `cdk deploy 'LlmSafeSpaces/Platform'`. Emits both keys into the cluster-config ConfigMap.
4. **ops-prod**: `turnstile.enabled: true` in the llmsafespaces HR values, ExternalSecret pulls the secret. Flux reconciles, pods restart with env vars set, widget starts appearing on `/register`.

### Enable / disable

Enable: `turnstile.enabled: true` in ops-prod HR values → git push → Flux reconciles → ~90s.

Disable: same, set `false`. Turnstile-related env vars disappear from pods, middleware not installed, widget renders nothing, CSP unchanged. Same ~90s cycle. No image redeploy needed.

### Live verification

```bash
# 1. env.json includes the public site key
curl -sk https://safespaces.thekao.cloud/env.json
# Expected: {"apiBaseUrl":"/api/v1","turnstileSiteKey":"0x4AAAAAAD..."}

# 2. Registration without token is blocked
curl -sw '\n%{http_code}\n' -X POST https://safespaces.thekao.cloud/api/v1/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"username":"x","email":"x@example.com","password":"password123"}'
# Expected: 401 {"error":"turnstile_failed","reason":"missing_token", ...}

# 3. Registration with invalid token is blocked (Cloudflare rejects)
curl -sw '\n%{http_code}\n' -X POST https://safespaces.thekao.cloud/api/v1/auth/register \
  -H 'Content-Type: application/json' \
  -H 'cf-turnstile-response: FAKE_TOKEN' \
  -d '{"username":"y","email":"y@example.com","password":"password123"}'
# Expected: 401 {"error":"turnstile_failed","reason":"rejected","detail":"invalid-input-response"}
```

### Known limitations

- **`loadPromise` singleton never resets on script-load error**. If Cloudflare's api.js `<script>` `onerror` fires once (transient network failure at first mount), every subsequent mount resolves instantly with `window.turnstile === undefined` and the widget silently doesn't render. Users hit `verify_unavailable` on submit. Non-blocking; the backend fails-closed. Would self-heal with a `loadPromise = null` in the `onerror` handler if this becomes a support issue.
- **`extractTurnstileToken` form-field fallback** only works for `application/x-www-form-urlencoded` bodies. The frontend uses JSON, so this path is unreachable in practice; kept for form-encoded callers that don't exist yet.
- **`clientIP` bypasses `TrustedProxies`** by design — the extracted IP is only used as Cloudflare's `remoteip` fraud-scoring hint, not for access control. If this function is ever repurposed for access control, switch to `c.ClientIP()`.

### File reference

| Concern | File |
|---------|------|
| Middleware | `api/internal/middleware/turnstile.go` |
| Middleware tests | `api/internal/middleware/turnstile_test.go` |
| Config struct + fail-closed guard | `api/internal/config/config.go` (`Turnstile` block, `applyTurnstileEnv()`) |
| Router conditional install | `api/internal/server/router.go` (`registerAuthRoutes`, `TurnstileRouterConfig`) |
| Router integration tests | `api/internal/server/router_auth_turnstile_test.go` |
| CSP transform (API) | `api/internal/app/app.go` (`addTurnstileToCSP()`, applied to `securityCfg`) |
| CSP transform tests | `api/internal/app/csp_turnstile_test.go` |
| Chart values | `helm/values.yaml` (`turnstile` block) |
| Chart api deployment env wiring | `helm/templates/api-deployment.yaml` |
| Chart frontend deployment env wiring | `helm/templates/frontend-deployment.yaml` |
| Chart ingress CSP transform | `helm/templates/frontend-ingress.yaml` |
| Chart CSP tests | `helm/chart_test.go` (`TestTurnstile_CSP*`) |
| Frontend widget component | `frontend/src/components/auth/TurnstileWidget.tsx` |
| Frontend widget tests | `frontend/src/components/auth/TurnstileWidget.test.tsx` |
| Frontend form integration | `frontend/src/components/auth/RegisterForm.tsx` |
| Frontend form tests | `frontend/src/components/auth/RegisterForm.test.tsx` |
| Frontend runtime env injection | `frontend/docker-entrypoint.sh`, `frontend/src/env.ts` |
| Frontend 401 exclusion + tests | `frontend/src/api/client.ts`, `frontend/src/api/client.test.ts` |
| Frontend E2E | `frontend/tests/e2e/register-turnstile.spec.ts` |
| CDK context + emission | `~/llmsafespaces-cdk/lib/config.ts`, `lib/platform-stack.ts`, `bin/app.ts`, `cdk.context.json` |
| CDK example context | `~/llmsafespaces-cdk/cdk.context.example.json` |
| ops-prod ExternalSecret | `kubernetes/apps/llmsafespaces/llmsafespaces/externalsecret/externalsecret.yaml` |
| ops-prod HR values | `kubernetes/apps/llmsafespaces/llmsafespaces/app/helm-release.yaml` (`turnstile` block) |
| Security policy exception | `design/0027_2026-05-24_security-policy-v21.md` Appendix D |
| Feature worklog | `worklogs/0595_2026-07-04_turnstile-captcha-register.md` |
| PR | [#501](https://github.com/lenaxia/LLMSafeSpaces/pull/501) |

---

## API Reference

The complete REST API is documented in `README.md` under "REST API". The API has ~90 routes covering:

- **Auth** (8 routes): register, login, logout, me, API key CRUD
- **Workspaces** (10 routes): CRUD + suspend, activate, restart, refresh-compute, status, agent reload
- **Session management** (5 routes): list, ensure, rename, mark-seen, active
- **Session proxy** (7 routes): message, prompt, history, get, abort, delete, SSE events — reverse-proxied to the workspace pod's `opencode serve` on port 4096
- **Questions & Permissions** (5 routes): list/reply/reject agent questions and permission requests
- **Events** (2 routes): user-scoped SSE stream, bulk agent reload
- **Secrets** (8 routes): CRUD + audit + reveal + bindings — encrypted at rest store
- **Workspace bindings** (3 routes): set/get bindings, reload-secrets
- **Workspace env** (3 routes): set/get/delete environment variables
- **Models** (2 routes): list available models, set default model
- **Terminal** (2 routes): ticket + WebSocket proxy
- **Admin provider credentials** (8 routes): CRUD + auto-apply rules
- **User provider credentials** (7 routes): CRUD + bindings
- **Settings** (6 routes): admin instance + user preferences + schemas
- **Account** (3 routes): key rotation, password change, recovery
- **Relay fleet** (9 routes, conditional): setup wizard + status + provider creds + deploy/rotate/pause/resume — registered only when the relay admin handler is wired (Epic 42/48)
- **Infrastructure** (4 routes): livez, health, readyz, metrics

### `?verbose=true` flag

By default, the proxy strips parts of `type=="patch"` from message and history responses. opencode emits a `patch` part for every assistant turn, listing every workspace file it touched (~2 KB per response of internal snapshot paths). For most clients this is noise.

Pass `?verbose=true` on any message or history request to receive the unfiltered response.

---


## Configuration Reference

The API service is configured via `api/config/config.yaml` with environment variable overrides via Viper.

| Section | Key | Default | Env Var | Description |
|---------|-----|---------|---------|-------------|
| `server` | `host` | `0.0.0.0` | `LLMSAFESPACES_SERVER_HOST` | Listen address |
| `server` | `port` | `8080` | `LLMSAFESPACES_SERVER_PORT` | Listen port |
| `server` | `shutdownTimeout` | `30s` | — | Graceful shutdown timeout |
| `kubernetes` | `inCluster` | `true` | — | Use in-cluster config |
| `kubernetes` | `namespace` | `llmsafespaces` | — | Default namespace |
| `database` | `host` | `postgres` | — | PostgreSQL host |
| `database` | `port` | `5432` | — | PostgreSQL port |
| `database` | `password` | (empty) | `LLMSAFESPACES_DATABASE_PASSWORD` | PostgreSQL password |
| `database` | `maxOpenConns` | `25` | — | Max open connections |
| `redis` | `host` | `redis` | — | Redis host |
| `redis` | `port` | `6379` | — | Redis port |
| `redis` | `password` | (empty) | `LLMSAFESPACES_REDIS_PASSWORD` | Redis password |
| `redis` | `poolSize` | `20` | — | Connection pool size |
| `auth` | `jwtSecret` | (empty) | `LLMSAFESPACES_AUTH_JWTSECRET` | JWT signing secret (required) |
| `auth` | `tokenDuration` | `24h` | — | Token expiry |
| `auth` | `apiKeyPrefix` | `lsp_` | — | API key prefix |
| `auth` | `lockoutEnabled` | `false` | `LLMSAFESPACES_AUTH_LOCKOUTENABLED` | Enable account lockout after failed logins |
| `auth` | `lockoutAttempts` | `0` | `LLMSAFESPACES_AUTH_LOCKOUTATTEMPTS` | Failed attempts before lockout (e.g. `5`) |
| `auth` | `lockoutDuration` | `0` | `LLMSAFESPACES_AUTH_LOCKOUTDURATION` | Lockout duration (e.g. `15m`) |
| `security` | `allowedOrigins` | (empty) | `LLMSAFESPACES_SECURITY_ALLOWEDORIGINS` | Comma-separated CORS origins (e.g. `https://app.example.com,https://admin.example.com`) |
| `security` | `allowCredentials` | `false` | `LLMSAFESPACES_SECURITY_ALLOWCREDENTIALS` | Allow credentials in CORS |
| `rateLimiting` | `enabled` | `false` | `LLMSAFESPACES_RATELIMITING_ENABLED` | Enable rate limiting |
| `rateLimiting` | `defaultLimit` | `100` | `LLMSAFESPACES_RATELIMITING_DEFAULTLIMIT` | Requests per window |
| `rateLimiting` | `defaultWindow` | `1m` | `LLMSAFESPACES_RATELIMITING_DEFAULTWINDOW` | Window duration |
| `rateLimiting` | `burstSize` | `20` | `LLMSAFESPACES_RATELIMITING_BURSTSIZE` | Burst allowance |
| `logging` | `level` | `info` | — | Log level |
| `logging` | `encoding` | `json` | — | Log format (json/console) |

---

## Version History

| Version | Date | Changes |
|---------|------|---------|
| 1.22 | 2026-06-29 | Secrets UX: added `global_default` boolean to `user_secrets` (migration 000004); propagated through `SecretStore`, `PgSecretStore`, `SecretService`, `AsyncAuditLogger`; `SecretService.SeedGlobalDefaultSecrets` added; `workspace.Service` gains `SecretAutoProvisioner` interface + `SetSecretAutoProvisioner` setter, called best-effort on `CreateWorkspace` after `credProvisioner`; `UpdateSecretRequest.GlobalDefault` is a `*bool` (nil = leave unchanged); frontend: `globalDefault` field on `SecretResponse`/`CreateSecretRequest`/`UpdateSecretRequest`; `SecretsTab` adds "Include in all workspaces" checkbox on create form, "Update" inline form per secret row (carries globalDefault toggle + new value), softened post-creation warning from "will never be shown again" to "you can reveal this value later using your password". |
| 1.21 | 2026-06-29 | Closed the F11 header-trust gap: `resolveCallbackURL` (`org_sso.go`) no longer derives the SSO callback URL from `X-Forwarded-Proto`/`Host` when `oidc.redirectBaseUrl` is unset. Start returns HTTP 500 with a config hint; Callback redirects to the frontend with `?sso=config_error`. New sentinel `sso.ErrRedirectBaseURLNotSet`. The default (empty) is now safe-by-construction — SSO fails closed instead of trusting attacker-influenceable headers. |
| 1.19 | 2026-06-22 | Documented the self-hosted multi-cloud inference relay fleet (Epic 42): new `InferenceRelay` cluster-scoped CRD (3rd CRD), `cmd/relay-router` + `cmd/relay-proxy` binaries, `controller/internal/relay` reconciler with AWS/OCI/GCP drivers, and the `/api/v1/admin/relay/*` admin API; noted the WireGuard→HTTPS+per-VM-token transition (worklog 0447). Added Master KEK delivery subsection (Epic 50 US-50.1: file mount, not env var) and Tenant isolation subsection (Epic 51: gVisor RuntimeClass + per-tenant quota webhook). Updated CRD count (2→3), repository structure, deliverables framing, Rule 8 design-doc table, and API route inventory. |
| 1.18 | 2026-06-20 | Shipped DNS verification of claimed SSO domains (D17 Q-S2): new `verified_domains` + `verification_token` columns (migration 000041); on-demand DNS verification via `POST /orgs/:id/sso/domains/:domain/verify`; token rotation endpoint; login-page discovery (`ListSSODomains`) now filters on verified only; existing domains grandfathered as verified; updated §14 endpoints + known gaps |
| 1.17 | 2026-06-20 | Surfaced per-org OIDC SSO instance-plumbing config (`oidc.redirectBaseUrl`, `oidc.frontendRedirectUrl`, `oidc.stateCookieName`) in the Helm chart (`values.yaml` + `configmap-api.yaml`), exposing the F11 header-trust mitigation (operators set `oidc.redirectBaseUrl` to close it; ships default-empty so the gap remains open in unconfigured deploys); updated §14 Configuration to document both chart and env-var paths |
| 1.16 | 2026-06-20 | Added "Multi-Tenant OIDC SSO" section documenting the as-built per-org OIDC system (Epic 43 / US-43.10 / D17): data model, endpoints, PKCE login flow, org-admin config flow, auto-provisioning + role mapping, security controls, instance plumbing config, frontend, known gaps, and file reference |
| 1.15 | 2026-06-18 | US-46.14/US-46.15: archived V1 design docs (`0001`–`0020`) to `design/archive/v1/`; repointed all V1 references in README-LLM.md to the archive path; fixed stale filenames (network doc was listed as `0007` but is `0020`; runtimeenv doc was listed as `0006` but is `0007`) |
| 1.14 | 2026-06-18 | Reclassified annotateModels remap guard from "dead code (tech debt to remove)" to "intentional defense-in-depth" — aligns the doc with the code author's documented reasoning at `models.go:450-456` and the hardening history from worklogs 0178/0189 (see worklog 0341) |
| 1.13 | 2026-06-12 | Removed redundant Bug Status, Confirmed Bugs, Implementation Status, Branch Management sections; simplified repo structure, worklog template, multi-agent workflow, PR adversarial assessment; folded scoring bullets into tables; compressed relay write sequences and version history; removed backwards compat; updated annotateModels remap note |
| 1.12 | 2026-06-11 | Fixed repo structure, CRD count, architecture diagram, API reference, tech stack, SSE paths, route docs |
| 1.11 | 2026-06-08 | Added relay config subsystem: bugs, volume layout, config merge order, design, gap fixes |
| 1.10 | 2026-06-04 | Added PR Review Guide (1–10 rubric, E2E wiring verification, adversarial assessment); expanded Rule 11 |
| 1.9 | 2026-05-27 | Frontend streaming UX fixes (user echo, thinking blocks, bubble overflow) |
| 1.8 | 2026-05-23 | Engineering principles in Rule 4; Rule 7 assumptions; TDD definition of done; validator loop |
| 1.5 | 2026-05-23 | Sandbox CRUD API, `?verbose=true` flag, README.md rewritten for V2 |
| 1.4 | 2026-05-23 | Rate limiting, CORS hardening, account lockout |
| 1.3 | 2026-05-23 | Auth endpoints with security hardening and e2e tests |
| 1.2 | 2026-05-22 | Repo structure, architecture, CRD ownership, tech stack aligned with EVOLUTION-V2 |
| 1.1 | 2026-05-22 | Updated for V2 architecture |
| 1.0 | 2026-05-21 | Initial creation |
