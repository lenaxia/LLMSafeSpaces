# Overview

LLMSafeSpaces is built around one core idea: **every AI agent runs in its own isolated, persistent Kubernetes pod**, and the platform manages the lifecycle, security, and connectivity around that pod.

## What a workspace is

A *workspace* is:

- A **Kubernetes pod** running `opencode serve` on port 4096
- A **PVC-mounted filesystem** at `/workspace` (and `/home/sandbox`, `/tmp`) that persists across pod restarts
- A **set of bound secrets** (LLM provider credentials, SSH keys, environment variables) injected via tmpfs at `/sandbox-cfg` and `/sandbox-runtime`
- An **owner** (a user, optionally in an organization) enforced by the `WorkspaceAccessMiddleware`

A workspace is *not* a generic sandbox. It specifically runs an AI agent that exposes an HTTP API. The platform is the orchestration layer around that agent.

## The lifecycle

```
Pending → Creating → Active → Suspending → Suspended → Resuming → Active
                     │                      ↘           ↘
                     └──→ Failed             Terminating  Terminating
```

- **Active** — the pod is running; the API reverse-proxies client requests to `opencode serve`.
- **Suspended** — the pod is deleted but the PVC is retained. The workspace consumes no compute.
- **Terminated** — the pod and PVC are both deleted.

Suspending and re-activating takes ~22 seconds (measured); the cost is PVC re-attach + opencode boot. Session history in the PVC survives.

## Who uses it

| Role | What they do |
|---|---|
| **Operator** | Deploys LLMSafeSpaces via Helm, configures storage/networking/security, manages the cluster |
| **User** | Creates workspaces via the REST API or frontend, binds credentials, drives agent sessions |
| **Agent** | Runs inside the workspace pod, executes user requests, writes to `/workspace` |
| **Integrator** | Connects an MCP client or SDK to drive workspaces programmatically |

## What it is not

- **Not a code execution sandbox.** The agent inside the pod is the workload; the platform doesn't run arbitrary user code (beyond what the agent itself executes).
- **Not a multi-cloud control plane.** It runs in one Kubernetes cluster. The optional InferenceRelay fleet is the only multi-cloud component.
- **Not stateless.** PVCs are real persistent volumes; workspace data is real data. Backups are your responsibility.
- **Not a substitute for cluster security.** The platform hardens workspace pods, but a compromised control plane compromises everything. Run on a cluster you trust.

## Next

Read the [Quickstart](quickstart.md) for a 10-minute local install, or jump to [Concepts](concepts.md) for the data model.
