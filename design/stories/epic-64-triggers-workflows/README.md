# Epic 64: Triggers & Workflows

**Status:** Definition (not yet in implementation)
**Created:** 2026-08-05
**Priority:** Medium — expands the platform from "interactive agent sessions" to "automated, deterministic pipelines." Unblocks cron-driven processing, webhook-driven automation, and repeatable multi-step tasks.
**Depends On:** Epic 30 (Unified Credential Model — injection pipeline), Epic 29 (AgentClient abstraction), Epic 23 (controller single-writer for `Status.Phase`). Soft-depends on Epic 51 (tenant isolation — quota patterns).
**Authoritative for:** How the platform lets users and orgs define, schedule, trigger, and execute deterministic DAG-structured pipelines whose nodes run inside workspace pods alongside the existing opencode agent.

---

## Problem Statement

### Current State

Every workspace today runs an interactive AI agent (`opencode serve`). The platform's entire execution model is **human-initiated, conversational, and synchronous**: a user opens a session, types a message, waits for a response. There is no mechanism for:

- **Scheduled work** — "process new meetings every hour," "run the nightly backup," "summarize the week on Monday at 9am."
- **Event-driven work** — "when a GitHub issue is opened, investigate it," "when a sensor reports a temperature over threshold, run thermal analysis."
- **Repeatable multi-step pipelines** — "fetch → normalize → classify → verify → persist," expressed once and run many times with different inputs.

The agent loop (opencode) is excellent at cognition inside a single turn but is the wrong primitive for deterministic orchestration. Encoding "validate input, then load known entities, then ask the agent to classify, then verify aliases via phonetool, then persist" as a single prompt produces incoherent instructions and un-debuggable failures. This is the textbook case for a workflow engine: deterministic steps compose around concentrated cognitive steps.

A code audit (2026-08-05) confirmed:

- **Zero** scheduler, cron, job-queue, or durable-execution concept exists in the codebase. `go.mod` has no `robfig/cron`, `gocron`, `asynq`, `riverqueue`, or `temporal` dependency. Background work is done via ad-hoc `time.Ticker` goroutines (e.g. `pkg/secrets/jwt_session_janitor.go`, `controller/internal/freemodels/refresher.go`).
- **No** generic webhook receiver. The only inbound webhook handler is `api/internal/handlers/webhook.go` — Stripe-specific, relying on `stripe-go`'s built-in HMAC verify. Raw `crypto/hmac` appears once, in `api/internal/services/sso/sso.go` for state-cookie signing.
- **No** workflow, DAG, pipeline, trigger, or job concept. `grep` for `workflow`, `trigger`, `pipeline`, `dag`, `cron_job` across `*.go` returns no production hits.
- The platform MCP server (`pkg/mcp/server.go`) exposes 15 tools — all interactive-session-centric. None relate to automation or scheduling.

### Desired State

Users and orgs can define **workflows** (deterministic DAGs of `script` / `agent` / `http` / `condition` nodes) and **triggers** that fire them on a schedule, on an inbound webhook, or on manual demand. Workflows run **inside a target workspace pod** — reusing its filesystem, git state, materialized credentials, and opencode agent — with the platform orchestrating the DAG from the controller. Runs either complete or fail; there is no resume.

Concretely, a user should be able to:

1. Author a workflow (YAML or visual editor) that validates input, loads workspace data, delegates a cognitive step to a named opencode agent with structured output, and persists results to workspace files.
2. Attach a cron trigger ("every weekday at 9am") or a webhook trigger ("POST `/hooks/:id` with HMAC signature") to that workflow.
3. Watch runs execute node-by-node in the UI, with per-node status, outputs, and errors.
4. Have the in-workspace agent itself create, modify, and fire workflows/triggers via MCP tools (gated by org policy + quota).

---

## Relationship to Existing Subsystems (do not confuse)

| | opencode agent (shipped) | Workflows (this epic) |
|---|---|---|
| What it is | Cognitive loop — reasons, uses tools, branches on judgment | Deterministic DAG — graph walk, no reasoning except inside `agent` nodes |
| When to use | Open-ended task, unknown structure, dynamic tool selection | Known multi-step procedure with deterministic edges |
| State | Session memory across turns | Per-node JSON input/output, persisted after each transition |
| Failure mode | Agent gives up or produces wrong output | Node fails after retries → run fails (no compensation) |

**Scope guardrail (load-bearing):** *Workflows are for deterministic pipelines. If a workflow has more than two sequential `agent` nodes without deterministic work between them, it should be a single prompt to the agent.* The workflow engine does not compete with opencode's agent loop — it composes around it. This rule is enforced by review, not by code.

---

## Scope

### In scope

- **Node types (4):** `script` (inline handler executed in the workspace runtime via `mise`), `agent` (delegate a sub-task to a named opencode agent with prompt template + output schema + session lifecycle + retries), `http` (outbound HTTP through the workspace egress NetworkPolicy), `condition` (multi-branch expr-lang routing).
- **Trigger sources (2):** `cron` (cron expression + timezone), `webhook` (HMAC-SHA256 verified inbound HTTP). Manual runs go through `POST /workflows/:id/runs` directly — there is no `manual` source type (a manual trigger is just run-create with extra steps). Defer `workflow_done` chaining and `mcp` source.
- **Trigger actions (2):** `run_workflow`, `run_script`. Session actions (`reply`/`prompt`/`new_session`) are deliberately excluded — they are `agent` nodes inside a one-node workflow, not first-class trigger actions.
- **Ownership tiers (2):** `user`, `org`. Defer `admin/platform` tier until a real "workflow that runs against every workspace" requirement surfaces.
- **Execution model:** Workflows run inside a target workspace pod. The controller orchestrates the DAG state machine; agentd (port 4098) dispatches leaf execution. No server-side script execution.
- **Run semantics:** Run to completion or fail. **No resume.** If the workspace dies, is suspended, or the controller restarts mid-run, the run is marked `failed` with a machine-readable `error_code`. The user re-runs.
- **Authoring by external agents:** External agents (Claude Desktop, Cursor) using the platform MCP server can CRUD workflows/triggers and start runs, authenticated via API key, gated by the user's quota. The `allow_user_workflow_create` org policy governs whether user-scope authoring is permitted at all.
- **In-workspace agent authoring — DEFERRED to v2:** The agent running *inside* a workspace has no platform API credentials and cannot reach the CRUD endpoints today. Enabling it requires the agentd built-in MCP server (D10) or a dedicated workspace-scoped token mechanism — both are net-new security surfaces and are explicitly out of scope for v1. The platform MCP server (US-64.11) serves external agents only.
- **MCP tool surface:** Platform MCP server (`pkg/mcp/server.go`) gains workflow/trigger tools for **external** agents. The agentd built-in MCP server is **deferred to v2** (see D10).
- **Webhooks:** Generic HMAC-SHA256 signature verification + idempotency/dedup table + optional IP allowlist + per-trigger rate limit. Defer Slack-signing-secret and Discord-Ed25519 schemes.

### Out of scope (with rationale — see "Out of Scope" section)

`transform` / `parallel` / `delay` / `mcp_call` node types; `manual` trigger source type; `workflow_done` and `mcp` trigger sources; session trigger actions; platform/admin ownership tier; child workflows; dynamic DAGs; long-lived waits (human approval); cross-workspace orchestration; sagas/compensation; run resumability; **in-workspace agent authoring** (requires agentd built-in MCP or workspace-scoped tokens — net-new security surface); the agentd built-in MCP server; Slack/Discord webhook signature schemes; workflow versioning / `version: pinned` triggers; oversize-output spill to file (hard-fail instead).

---

## Alternatives Considered (build vs. adopt)

This is a conscious strategic decision, not a default. Durable execution is a solved problem with mature engines.

| Engine | Fit assessment | Verdict |
|---|---|---|
| **Temporal** | Needs its own server cluster + dedicated DB. Massive operational addition the codebase has deliberately avoided. Shimming it to call into workspace pods via agentd still requires all the agentd work. | **Reject** — wrong operational shape for a self-hosted K8s product. |
| **Argo Workflows** | Task-per-container model. Our model is task-inside-existing-pod (reuse workspace fs + creds + opencode). Fundamentally different. | **Reject** — doesn't fit the in-workspace execution model. |
| **Inngest / Hatchet** | SaaS or bring-your-own-infra. Wrong shape for self-hosted. | **Reject.** |
| **Build (this epic)** | Execution model is specific enough (in-workspace, opencode-aware, reuses Epic 30 secrets + workspace fs) that a general engine fights us. | **Adopt** — but scope tightly. The moment the engine grows generic features (child workflows, signals, queries, search APIs) we are reinventing Temporal badly and should stop. |

**Risk to manage:** building a poor man's Temporal. Mitigation: only build the in-workspace + opencode integration that's genuinely ours; crib the small, proven primitives (retry policy, timeouts, run states, concurrency caps) from the mature engines; refuse the speculative ones.

### Features cribbed from mature engines (in v1)

| Feature | Source | Why it earns its complexity |
|---|---|---|
| Explicit run state enum (`queued`/`running`/`succeeded`/`failed`/`canceled`/`timed_out`) | Temporal | Distinguishes user-cancel from node-failure (different UX); 6 states, no library |
| Per-node retry policy (`maxAttempts`, `initialInterval`, `backoff`, `maxInterval`) | Temporal + Argo | The reference workflow uses `maxRetries: 2`; retries are a top-3 need |
| Per-node + global `timeout` | Argo | Reference workflow uses `timeoutMinutes: 10`; trivial via context deadline |
| `defaults` block (nodeTemplate) | Argo | Avoids repeating `maxAttempts` on every node; validation-only feature |
| ~~`tags` jsonb + GIN index on runs~~ | Temporal (search attributes) | **Cut from v1** — speculative; `(workflow_id, status, created_at)` filtering is sufficient |
| Single-in-flight-per-workflow (hardcoded, partial unique index) | Inngest | Eliminates cron overlap; eliminates TOCTOU on concurrent webhook deliveries; eliminates the need for hand-written idempotency guards |
| Per-trigger rate limit (token bucket in Redis) | Inngest | Prevents webhook floods from spawning 1000 queued runs |
| `error_code` enum column on runs | Hatchet (visibility) | Machine-readable failure categorization for UI grouping |

### Features deliberately NOT cribbed

| Feature | Source | Why rejected |
|---|---|---|
| Activities vs. workflows distinction | Temporal | We run inside one workspace; one node category suffices |
| Signals / Queries | Temporal | Assume long-lived resumable workflows; we don't have those |
| Child workflows | Temporal | Without resume + run-to-completion, a child is just a node |
| Event-sourcing replay history | Temporal | We persist transitions for audit/UI, not replay |
| Workflow-as-code SDK | Temporal | YAML/JSON spec with inline handlers is the right level |
| DAG vs. Steps dual templates | Argo | One DAG model; linear flows use linear edges |
| Artifact repositories (S3 I/O) | Argo | PVC-backed workspace is the artifact store; `input`/`output` are JSON in PG |
| Resource templates (K8s manifest nodes) | Argo | We execute inside the workspace, not against the cluster |
| Exit handlers / hooks | Argo | Adds a second mini-DAG concept; v2 |
| Fan-out functions / pause-unpause | Inngest | No queue, no pause — run-to-completion or fail |
| Throttle / priority queues | Inngest | Without a queue (reject on overflow), N/A |
| Worker pools / assignment | Hatchet | Controller executes directly, no queue |
| Cron with manual ack | Hatchet | Scheduler tick fires + logs; missed-fire policy is v2 |
| Scheduling affinity | Hatchet | Each run is bound to one workspace |

---

## Personas & Primary Paths (v1)

**Authoring persona: human, via UI/API.** v1's primary authoring path is a human writing a workflow spec (YAML in a textarea with server-side validation) and attaching triggers via the UI or REST. The frontend (US-64.12) is built for this. We do NOT optimize v1 for agent-authored specs — the in-workspace opencode agent has no platform API credentials (D10), and enabling them is a net-new security surface deferred to v2. External agents (Claude Desktop, Cursor) using the platform MCP server (US-64.11) can author workflows on behalf of an authenticated user, but this is a power-user path, not the primary one.

**Triggering persona: programmatic and deterministic.** Triggers fire from cron schedules ("every weekday at 9am") or inbound webhooks from external systems ("when a new job posting is detected," "when a GitHub issue is opened"). These are deterministic, automated, and may originate from non-LLM systems. An LLM may be *inside* a workflow (`agent` node) and may *cause* an external event that later fires a webhook trigger, but the LLM is not the trigger source in v1. This keeps the trigger layer simple (two sources: cron + webhook) and the authoring surface honest (humans define what runs; systems signal when).

**Run-consumption persona: human, via UI.** Run history, per-node status/output/error codes, circuit-breaker state, and active-run indicators are consumed by humans in the frontend. Programmatic consumption (polling `GET /runs/:id`) is supported but secondary.

**What this means for scope:** the frontend (US-64.12) is a v1 highlight, not a stopgap — it's how the primary persona interacts with the feature. The YAML editor is a textarea + server-side validation in v1 (a rich schema-driven form editor and visual DAG canvas are v2). US-64.11 (platform MCP tools) serves the external-agent power-user path. In-workspace agent authoring is explicitly v2.

---

## Actors & Roles

| Actor | Auth guard | Can do |
|---|---|---|
| User (owner) | authenticated | CRUD own workflows/triggers/webhooks; fire own triggers; start/cancel own runs; read own run history |
| Org admin (`org_memberships.role='admin'`) | `OrgAdminGuard` | CRUD org-scoped workflows/triggers/webhooks; fire org triggers; manage org runs |
| Org member | `OrgMemberGuard` | Read-only list of org workflows/triggers; **cannot** author (mirror Epic 53 D11) |
| External agent (Claude Desktop, Cursor, etc.) | authenticated via API key against the platform MCP server (`pkg/mcp/server.go`) | CRUD workflows/triggers the key's owner owns, **iff** `allow_user_workflow_create` policy is enabled and per-user quota not exceeded; all changes audit-logged. The in-workspace agent has **no** such path in v1 (deferred — see D10) |
| End user (any) | authenticated | Benefit only: sees run results, SSE events |

---

## Use Cases

**UC-1 — Cron-driven batch processing.** A user wants every new meeting processed on the hour: validate → load entities → agent classification → verify → persist. They define a 6-node workflow, attach a cron trigger (`0 * * * *`, UTC), and the platform fires it hourly against their workspace. Single-in-flight-per-workflow means an overlap is rejected, not piled up.

**UC-2 — Webhook-driven investigation.** A GitHub webhook fires on issue-opened. The trigger's HMAC secret is registered; the platform verifies the signature, deduplicates on `X-Request-ID`, and starts a workflow that passes `body.issue` into an `agent` node for investigation.

**UC-3 — Manual run from the UI.** A user clicks "Run" on a workflow, supplies JSON input matching the workflow's `inputSchema` via `POST /workflows/:id/runs`, and watches node-by-node execution in the run history view. There is no `manual` trigger source type — manual runs are run-create directly, with `trigger_id = null`.

**UC-4 — External agent authors automation (MCP).** From Claude Desktop (or another external MCP client), an agent is asked "set up a nightly backup of the database to S3." It calls `workflow_create` with a one-node `script` spec, `trigger_create` with a cron source, and confirms. Authenticated via the user's API key against the platform MCP server; gated by `allow_user_workflow_create` and per-user quota; audit-logged. *(In-workspace agent authoring is deferred to v2 — see D10 — because the in-pod agent has no platform API credentials in v1.)*

**UC-5 — Workspace dies mid-run.** A `script` node is executing when the pod OOMs. The controller's agentd call fails or times out; the run is marked `failed` with `error_code: workspace_unavailable`. The user re-runs once the workspace is healthy.

**UC-6 — Trigger flood.** A misconfigured sender delivers 1000 webhook POSTs/sec. The per-trigger rate limit (token bucket) drops the excess; idempotency dedup collapses retried duplicates. The workflow is not spawned 1000 times.

**UC-7 — Runaway cron auto-disables.** A workflow's cron trigger fires hourly but every run fails (the workspace lost a dependency). After `autoDisableAfterConsecutiveFailures` (default 10) failures, the scheduler sets `triggers.enabled=false`, emits an SSE event, and writes an audit entry. The user is notified instead of burning tokens indefinitely.

---

## Architecture

```
┌── Definitions (PostgreSQL) ───────────────────────────────────────┐
│  workflows          (owner_type user|org, spec_yaml, input_schema) │
│  triggers           (source cron|webhook, action run_*, circuit breaker) │
│  webhooks           (HMAC secret envelope, IP allowlist)           │
│  webhook_deliveries (idempotency dedup — copy stripe_events shape) │
│  workflow_runs      (state machine, pinned spec snapshot)        │
│  workflow_node_runs (per-node status/input/output/error_code)      │
│  trigger_fires      (fire audit + action_result)                   │
└────────────────────────────────────────────────────────────────────┘
          │                                              ▲
          │ API writes definitions/runs                  │ SSE events
          ▼                                              │
┌── API server (stateless) ──────────────┐   ┌── eventbroker ────┐
│  • CRUD handlers (user + org scope)    │   │ workflow.run_*    │
│  • Webhook receiver (HMAC verify)      │──►│ trigger.fired     │
│  • Run initiation (write row, return)  │   │ (existing broker) │
│  • Cancel                              │   └───────────────────┘
└────────────────────────────────────────┘
          │ queued runs visible to controller in PG
          ▼
┌── Controller (stateful, leader-elected) ──────────────────────────┐
│  • Workflow reconciler: claim queued runs (FOR UPDATE SKIP LOCKED)│
│    → ensure target workspace Active (wake if suspended)          │
│    → drive nodes in DAG order (linear + condition branching)     │
│    → persist workflow_node_runs after each node                  │
│    → apply per-node retry policy + timeout                       │
│    → on workspace death/suspend/timeout/cancel → run failed      │
│  • Scheduler goroutine (manager.Runnable, NeedLeaderElection):   │
│    → poll due cron triggers, fire them, log to trigger_fires     │
│    → enforce per-trigger rate limit (Redis token bucket)         │
│  • Global concurrency semaphore (bounded worker pool)            │
└────────────────────────────────────────────────────────────────────┘
          │ POST /v1/workflow/node/execute per node
          ▼
┌── Workspace pod — agentd (port 4098) ─────────────────────────────┐
│  • script  → exec handler via mise runtime; stdin←JSON, stdout→JSON│
│  • agent   → POST opencode /sessions/:id/prompt; validate schema   │
│  • http    → net/http through existing egress NetworkPolicy        │
│  • condition → expr-lang eval, return matched branch handle        │
│  • /v1/workflow/node/cancel → kill in-flight node                  │
└────────────────────────────────────────────────────────────────────┘
```

**Why the orchestrator lives in the controller, not the API.** The API is stateless and horizontally scalable by design ("Stateless API server — no sticky sessions"). Putting a run-driving goroutine in the API makes it stateful, breaks horizontal scaling, and kills runs on API pod recycle. The controller is already the stateful control plane — it runs leader-elected reconcilers (`freemodels/refresher.go:156-185` is the precedent) and is designed for exactly this. The API only writes rows and returns.

---

## Data Model

Migration **`000016_triggers_workflows`** (+ helm mirror; run `make chart-sync-migrations`). Follows the `000012_mcp_servers` idiom (`owner_type` CHECK, crypto envelope for secrets, `trg_*_updated_at` triggers, `BEGIN...COMMIT` wrapping).

### `workflows`
```
id uuid PK
owner_type text NOT NULL CHECK (owner_type IN ('user','org'))
owner_id text NOT NULL                       -- user_id or org_id
name text NOT NULL                           -- UNIQUE per (owner_type, owner_id)
slug text NOT NULL                           -- UNIQUE per (owner_type, owner_id)
description text
spec_yaml text NOT NULL                      -- canonical definition (validated on write)
spec_json jsonb NOT NULL                     -- parsed/validated DAG (denormalized for execution)
input_schema jsonb                           -- JSON Schema for workflow input
target_workspace_id uuid                     -- nullable; null = caller picks at run time
status text NOT NULL DEFAULT 'draft'         -- draft|active|archived
defaults jsonb                               -- nodeTemplate: {maxAttempts, timeout, ...}
created_at, updated_at timestamptz
UNIQUE (owner_type, owner_id, name)
UNIQUE (owner_type, owner_id, slug)
```

### `triggers`
```
id uuid PK
owner_type text NOT NULL CHECK (owner_type IN ('user','org'))
owner_id text NOT NULL
name text NOT NULL                           -- UNIQUE per (owner_type, owner_id)
description text
enabled bool NOT NULL DEFAULT true
source_type text NOT NULL CHECK (source_type IN ('cron','webhook'))  -- NO 'manual'; manual runs use POST /workflows/:id/runs directly
source_config jsonb NOT NULL                 -- cron:{expr,tz}
                                             -- webhook:{webhook_id}
target_type text NOT NULL CHECK (target_type IN ('run_workflow','run_script'))
target_config jsonb NOT NULL                 -- run_workflow:{workflow_id, input_template}  (NO version: pinned in v1 — see D6)
                                             -- run_script:{workspace_id, path, args, env}
consecutive_failures int NOT NULL DEFAULT 0  -- circuit breaker (UC-7); reset on first success
auto_disable_after int NOT NULL DEFAULT 10   -- when consecutive_failures >= this, set enabled=false + emit event + audit
last_fired_at, next_fire_at timestamptz
created_at, updated_at timestamptz
UNIQUE (owner_type, owner_id, name)
```

### `webhooks`
```
id uuid PK
trigger_id uuid NOT NULL FK triggers ON DELETE CASCADE
secret_cipher bytea NOT NULL                 -- crypto envelope (KEK-encrypted HMAC secret)
key_version int NOT NULL
allowed_ips cidr[]                           -- optional IP allowlist
idempotency_mode text NOT NULL DEFAULT 'header'  -- header|hash|disabled
idempotency_header text NOT NULL DEFAULT 'X-Request-ID'
created_at timestamptz
```

### `webhook_deliveries`  (idempotency — copy `stripe_events` pattern from `webhook.go:81-101`)
```
id uuid PK
webhook_id uuid NOT NULL FK webhooks ON DELETE CASCADE
dedup_key text NOT NULL                      -- header value OR sha256(body+window)
delivered_at timestamptz NOT NULL DEFAULT now()
UNIQUE (webhook_id, dedup_key)
```

### `workflow_runs`
```
id uuid PK
workflow_id uuid NOT NULL FK workflows
spec_snapshot jsonb NOT NULL                 -- IMMUTABLE pinned copy of spec_json at run start (D6)
input jsonb
output jsonb
status text NOT NULL DEFAULT 'queued'        -- queued|running|succeeded|failed|canceled|timed_out
error_code text                              -- node_failed|workspace_unavailable|canceled|timed_out|validation_error|schema_mismatch|output_oversize
error jsonb                                  -- human-readable detail
trigger_id uuid                              -- nullable (null = manual run via POST /workflows/:id/runs)
trigger_fire_id uuid                         -- nullable; links to trigger_fires
workspace_id uuid NOT NULL
started_at, finished_at timestamptz
created_at, updated_at timestamptz
-- PARTIAL UNIQUE INDEX enforces single-in-flight-per-workflow (D8). v1 hardcodes this rule;
-- there is no per-workflow maxConcurrentRuns setting, no advisory-lock path, no count-check.
-- Two concurrent webhook deliveries cannot both pass a check-then-insert race (the TOCTOU class
-- Epic 23 hardened against) — the insert itself is the atomic gate. Configurable N>1 + queueing
-- is v2 (it reintroduces count-check TOCTOU and needs careful design).
--   CREATE UNIQUE INDEX uq_workflow_run_single_inflight
--     ON workflow_runs (workflow_id) WHERE status IN ('queued','running');
```

### `workflow_node_runs`
```
id uuid PK
workflow_run_id uuid NOT NULL FK workflow_runs ON DELETE CASCADE
node_id text NOT NULL                        -- matches spec_snapshot.nodes[].id
node_type text NOT NULL                      -- script|agent|http|condition
status text NOT NULL DEFAULT 'pending'       -- pending|running|succeeded|failed|skipped
attempt int NOT NULL DEFAULT 0
input jsonb                                  -- predecessor output (or trigger envelope for first node)
output jsonb                                 -- capped at maxNodeOutputBytes; oversize FAILS the node (error_code=output_oversize), not spill — see Edge Case 4
branch text                                  -- condition nodes: matched handle
error_code text
error jsonb
started_at, finished_at timestamptz
-- index on (workflow_run_id, node_id)
```

### `trigger_fires`
```
id uuid PK
trigger_id uuid NOT NULL FK triggers
source_type text NOT NULL
input_envelope jsonb                         -- {source, received_at, headers, query, body}
action_type text NOT NULL
action_result jsonb                          -- {workflow_run_id} or {exit_code}
status text NOT NULL                         -- fired|delivered|failed|validation_error|rate_limited|skipped|auto_disabled
                                             -- skipped: missed-fire policy dropped a late cron tick (logged, not silent — see Edge Case 11)
                                             -- auto_disabled: circuit breaker tripped (UC-7)
fired_at, completed_at timestamptz
-- index on (trigger_id, fired_at)
```

### New `instance_settings` keys (registered via `pkg/settings/registry.go`)
| Key | Default | Purpose |
|---|---|---|
| `workflows.maxPerUser` | 50 | Per-user workflow count cap |
| `workflows.maxPerOrg` | 200 | Per-org cap |
| `workflows.maxRunDurationSec` | 3600 | Hard timeout per run |
| `workflows.workspaceActivationTimeoutSec` | 120 | Deadline for waking a suspended workspace before failing the run fast (separate from run timeout — prevents global semaphore exhaustion on dead workspaces) |
| `workflows.maxNodeOutputBytes` | 1048576 | Per-node output cap (1 MiB) |
| `triggers.maxPerUser` | 20 | Per-user trigger cap |
| `triggers.cronMinIntervalSec` | 60 | Floor on cron interval (anti-abuse) |
| `triggers.webhookRateLimitPerSec` | 10 | Default per-webhook token-bucket rate |

**Note on concurrency:** v1 hardcodes single-in-flight-per-workflow (D8). There is no `maxConcurrentRuns` setting. Adding configurable N>1 in v2 brings back the count-check TOCTOU problem and is deferred alongside queueing.

### New `org_policies` key
| Key | Default | Purpose |
|---|---|---|
| `allow_user_workflow_create` | false | Kill-switch for agent authoring (mirror of `allow_user_mcp_servers`) |

---

## Node Type Specifications

Every node: JSON in, JSON out. **Default pass-through:** a node's successor receives that node's `output` object as its `input`. **`condition` is the exception:** it routes (chooses the next edge) but passes its own *input* through to the successor unchanged — it never transforms data. Multi-predecessor merge semantics are deferred with `parallel`.

### `script`
```yaml
type: script
data:
  language: python            # python|node|go (via mise)
  handler: |                  # inline source; defines a `handler(input)` function (NOT a stdin/stdout process)
    def handler(input):
        # input is a dict; return a dict
        return {"meetingId": input["meetingId"], "processed": True}
  env: {}                     # optional env overrides
maxAttempts: 1                # override of defaults
timeout: 10m
```
**Execution model — function-call, not stdin/stdout process.** agentd writes the handler to a temp file, generates a thin per-language wrapper that imports the handler, feeds it the input dict, and serializes the return value to JSON. Wrapper contract per language:
- **Python:** `import json, sys, handler; print(json.dumps(handler.handler(json.loads(sys.stdin.read()))))`
- **Node:** `const h = require('./handler'); process.stdout.write(JSON.stringify(h.handler(JSON.parse(require('fs').readFileSync(0)))))`
- **Go:** compiled handler package exposing `Handler(input map[string]any) (map[string]any, error)`; wrapper `main` reads stdin, calls, writes stdout.

This matches the reference workflow's `def handler(input): return {...}` model (not a stdin-parsing process). On non-dict return or JSON-marshal failure: node fails with `error_code: script_output_invalid`. On unhandled exception/non-zero exit: node fails with `error_code: script_failed`, `error.stderr` captured. Workspace fs, git, mise-installed libs, materialized secrets all available. Runs as the workspace user — the workspace IS the sandbox (`runAsNonRoot`, dropped caps, `readOnlyRootFilesystem` on most paths, NetworkPolicy egress, gVisor opt-in).

### `agent`
```yaml
type: agent
data:
  agent: sdm-assistant        # named opencode agent profile from workspace config; default = workspace default
  prompt: |                   # Go text/template interpolation over input (NOT expr-lang — see D9)
    Process meeting {{.input.meetingId}}:
    {{.input.rawSummary}}
  system: ""                  # optional override
  outputSchema: { ... }       # JSON Schema; enforced when enforceStructuredOutput
  enforceStructuredOutput: true
  session: ephemeral          # ephemeral|new|existing
  session_id: ""              # required when session: existing
  releaseSessionOnComplete: true
maxAttempts: 2
timeout: 10m
```
agentd posts the rendered prompt to opencode's `/sessions/:id/prompt` (it already proxies this), waits for the response, validates against `outputSchema` if enforced. On schema mismatch: retry within `maxAttempts` with a repair hint appended to the prompt.

**Session lifecycle (concrete defaults — not advisory):**
- **Default `session` value: `ephemeral`.** Every run of an `agent` node without an explicit `session` field creates a fresh session and tears it down on completion (the opencode session DB row is deleted, not abandoned — otherwise the workspace's opencode.db grows unboundedly across runs).
- **`session: existing` + `session_id`** — reuses an existing session. If the referenced session has been deleted (e.g. workspace recycled), the node **fails** with `error_code: session_not_found` (no implicit fallback to `new` — silent fallback masks config errors). Using a shared `session_id` across multiple runs is a documented footgun: state from run N leaks into run N+1's context. Intended only for the narrow case of a workflow that runs sequentially against one long-lived investigative thread.
- **`session: new`** — creates a fresh session but does **not** tear it down on completion (the session persists for later inspection). Use when the run's output is meant to seed a human-reviewable conversation. The session is not auto-cleaned; workspace session-retention policy applies.

The `agent` node does **not** compete with the workflow engine — it delegates a bounded cognitive sub-task to opencode and captures structured output. The agent can natively use any Epic 53-injected MCP tool during its turn.

### `http`
```yaml
type: http
data:
  method: POST
  url: https://api.example.com/widgets
  headers:
    Authorization: "Bearer {{secrets.GITHUB_TOKEN}}"   # {{secrets.NAME}} resolves against materialized env-secrets
  body: "{{.input.payload}}"
  timeout: 30s
```
agentd makes the request via Go `net/http`. Goes through the workspace's existing egress NetworkPolicy naturally. Output: `{status, headers, body, duration_ms}`.

**Secret resolution contract (Fix 5):** `{{secrets.NAME}}` resolves against the already-materialized env-secret entries in `/sandbox-runtime/secrets-env` — the same tmpfs file agentd reads for the existing credential reload path (see Relay Config Subsystem in README-LLM.md). `NAME` is the env-var name as it appears in `secrets-env` (e.g. `GITHUB_TOKEN`, `OPENAI_API_KEY`). agentd reads the file once per node execution, builds a `map[string]string`, and substitutes before the request. Secrets injected via Epic 53 MCP servers are **not** reachable here (they live in `agent-config.json`, not `secrets-env`); only `env-secret` type credentials are available to `http` nodes. If `NAME` is not present in `secrets-env`: the node **fails** with `error_code: secret_not_found` (no silent empty-string substitution).

**Prompt templates use `text/template` (D9); `{{secrets.X}}` is a custom function registered into the template environment, NOT a field access.** This keeps secret substitution in the same trust-safe interpolation layer while making the syntax familiar.

**Secrets must be referenced, never inlined** — spec validation rejects raw tokens (any string matching common secret patterns: `Bearer `, `ghp_`, `sk-`, `xoxb-`, long hex/base64) in `headers`/`body`/`url` at workflow-create time.

### `condition`
```yaml
type: condition
data:
  conditions:
    - id: skip
      expression: "input.skipped == true"
    - id: retry
      expression: "input.error_code == 'transient'"
    # implicit: otherwise
# edges bind to sourceHandle: skip|retry|otherwise
```
agentd evaluates each expr-lang expression against `input` in order; first match wins. **A condition node routes; it does not transform.** Its successor receives the condition's *input* verbatim (the data that was evaluated), NOT a `{branch}` object. The matched branch id is orchestrator metadata carried by the edge's `sourceHandle` — it determines *which* successor runs, not *what data that successor sees*. This matches the reference workflow: `load-known-entities` runs after `skip-choice` and reads `input.meetingId`, `input.rawSummary` — the same fields `skip-choice` evaluated. Pure compute, ~microseconds. expr-lang is type-checked at workflow-validate time.

---

## Trigger Model

A trigger is `source + input contract + action + binding`. The **source** determines when raw data arrives. The **input contract** is the workflow's `inputSchema`. The **action** is the side effect. The **binding** (a template map) shapes raw input into the form the action expects.

### Sources

| Source | Envelope population | Validation |
|---|---|---|
| `webhook` | Raw HTTP body parsed (JSON if `application/json`, else `{raw, content_type}` under `body`); headers + query preserved | **Advisory** — author doesn't control sender; mismatch sets `validation_error`, workflow can branch |
| `cron` | Static `input_template` rendered at fire time with `{{.now}}`, `{{.nowISO}}`, `{{.lastFireAt}}`, `{{.triggerID}}` (text/template) | N/A — author controls |

Manual runs (no source type) go through `POST /workflows/:id/runs` directly; the caller provides `body` matching the workflow's `inputSchema` and validation is **strict** (reject on mismatch). `trigger_fires` records automated fires only; manual runs have `trigger_id = null`.

The **coercion layer** for webhooks is usually a `condition` + `script` node right after the trigger — extract `.body.issue.number` from the GitHub envelope into typed inputs.

### Circuit breaker (UC-7)

Cron-driven automation runs unattended — a failing workflow with no human watching burns tokens and fills the audit log indefinitely. Every trigger carries `consecutive_failures` (incremented on each run that reaches a terminal `failed`/`timed_out` state, reset on first `succeeded`). When `consecutive_failures >= auto_disable_after` (default 10), the scheduler sets `enabled=false`, writes a `trigger_fires` row with `status=auto_disabled`, emits an SSE event (`trigger.fired` with the auto-disabled payload), and audit-logs the trip. The user re-enables manually after fixing the workflow. This is a v1 safety primitive, not a nicety — unattended cron without it is an operational hazard.

### Actions

| Action | Target | Semantics |
|---|---|---|
| `run_workflow` | Workflow | Start a DAG run with rendered input. The composition primitive. |
| `run_script` | Workspace | Execute a single script. Degenerate workflow, but first-class for the "cron runs backup.sh" case — better UX than a one-node DAG. |

**Triggers initiate; they don't wait for cognition.** For session actions, the fire is recorded as "delivered", not "agent replied". If you need the agent's response as data, that's a workflow with an `agent` node.

### Binding (for session/script actions)

```yaml
# Webhook → run_workflow: each GitHub issue opens an investigation
trigger:
  source: { type: webhook, config: { webhook_id: hk_abc } }
  action:
    type: run_workflow
    config:
      workflow_id: wf_investigate   # always runs the current spec; in-flight runs are pinned by spec_snapshot (D6)
      input:                          # template map rendered against envelope
        repo: "{{ .body.repository.full_name }}"
        issue_number: "{{ .body.issue.number }}"
        issue_title: "{{ .body.issue.title }}"
        issue_body: "{{ .body.issue.body }}"
```
Template engine: **Go `text/template`** (pure interpolation, dot-syntax `{{.field}}`) — consistent with prompt templates and `http` node bodies. NOT expr-lang (D9): trigger inputs may carry attacker-controlled webhook payloads; interpolation is safe by construction where an expression engine would create an injection surface.

---

## Webhook Security

- **URL:** `POST /api/v1/hooks/{webhook_id}` — public route, no JWT. **The signature IS the credential** (copy `webhook.go:36` comment).
- **HMAC verify:** `crypto/hmac` + `crypto/sha256` + `subtle.ConstantTimeCompare` (recipe from `sso.go:740-757`). Header `X-Hub-Signature-256` (covers GitHub, GitLab, most modern sources). Timestamp skew check (±5min) when a timestamp header is present.
- **Optional IP allowlist** (`allowed_ips cidr[]`) — early reject before HMAC.
- **Idempotency:** caller-supplied dedup key (header value or `sha256(body+timestamp-window)`); `webhook_deliveries(webhook_id, dedup_key)` UNIQUE; duplicate → `200 "duplicate"` (copy Stripe pattern at `webhook.go:81-101`).
- **Secret storage:** exact `mcp_servers` crypto envelope pattern. Admin/org → server KEK; user → session DEK (zero-knowledge for user-owned hooks).
- **Atomicity of fire-row + run-create (v1 correctness):** the `trigger_fires` row and the `workflow_runs` row must be created in a single Postgres transaction, with the partial unique index violation (single-in-flight) causing the whole transaction to roll back — so a rejected delivery leaves no orphan `trigger_fires` row claiming "fired" with no run. On unique-violation, the handler commits a *separate* `trigger_fires` row with `status=skipped` and `action_result: {reason: "already_running"}`, then returns `409 + Retry-After`. This keeps the fire log honest: every row's status reflects what actually happened.

---

## Orchestrator (Controller) — Run Lifecycle

```
claim queued runs (SELECT ... FOR UPDATE SKIP LOCKED LIMIT N)
acquire global concurrency semaphore
for each claimed run:
  set status=running, started_at=now
  ensure target workspace Active (deadline = workspaceActivationTimeout, separate from run timeout):
    if suspended → ActivateWorkspace (reuse existing lifecycle)
    if not Active within workspaceActivationTimeout (default 120s): fail run fast (error_code=workspace_unavailable)
      — do NOT hold the global semaphore waiting on a dead workspace for the full run timeout
    if activation fails or workspace dies → fail run (error_code=workspace_unavailable)
  compute node order (topological from spec_snapshot edges)
  for each node in order:
    check run timeout (global) and cancel flag
    for attempt in 1..maxAttempts:
      persist node_run(status=running, attempt)
      POST /v1/workflow/node/execute to agentd with node spec + input
        (context deadline = node.timeout; honor /cancel)
      on success:
        if output > maxNodeOutputBytes: persist node_run(status=failed, error_code=output_oversize); fail run (no spill in v1 — Edge Case 4)
        validate against node.outputSchema if present
        persist node_run(status=succeeded, output)
        break
      on failure:
        if attempt < maxAttempts: backoff, retry
        else: persist node_run(status=failed, error_code); fail run (error_code=node_failed)
    compute next node(s) from edges + branch (condition passes its INPUT to the successor, not its output)
  set status=succeeded, output=last node output, finished_at=now
  publish SSE events via existing eventbroker
on controller restart: any status=running runs are marked failed (error_code=api_restart)
```

**Run states (6, no library):** `queued`, `running`, `succeeded`, `failed`, `canceled`, `timed_out`. All transitions persisted to PG after each node. The reconciler is stateless between ticks.

**Concurrency:** single-in-flight-per-workflow (D8) enforced at run create via the partial unique index `uq_workflow_run_single_inflight` — the insert is the atomic gate; a unique-violation maps to `409 "already running"` (webhooks) or `409` (manual/cron, both rare). Global controller semaphore bounds total in-flight runs across all tenants.

**Cancellation (immediate, not next-node-boundary):** user POSTs cancel → run row flagged → the reconciler's per-run goroutine observes the flag via a `context.WithCancel` propagated into the in-flight agentd HTTP call. Cancelling the context tears down the HTTP connection; agentd's `/v1/workflow/node/cancel` endpoint (or its disconnect handler, per the spike) kills the child process / interrupts opencode. The run is then marked `canceled` (distinct from `failed`). **Latency is seconds, not `node.timeout`** — the goroutine is not blocked on the agentd call oblivious to the cancel flag; the context cancellation interrupts the blocking call. Validated during US-64.1 (cancel mechanism deliverable).

---

## Validated Assumptions

These must be confirmed with evidence during US-64.1 (the spike) before dependent stories merge. Recording them per README-LLM.md Rule 7.

| ID | Assumption | Validation plan |
|---|---|---|
| A1 | opencode's `/sessions/:id/prompt` returns a synchronous structured response suitable for programmatic capture (not just SSE stream chunks) | US-64.1: probe live workspace; capture the actual response shape |
| A2 | opencode enforces JSON Schema-conformant output when a schema is supplied to a named agent (or whether we must validate/retry on our side) | US-64.1: run a named agent with a schema; check conformance |
| A3 | agentd can exec a handler via `mise`-installed runtime and capture the wrapper's serialized return within a context deadline | US-64.1: round-trip a `def handler(input): return {...}` Python script via the wrapper contract |
| A4 | The existing session proxy path (`proxy.go`) can be reused by agentd for `agent` node dispatch without a new auth surface | US-64.1: confirm agentd can call opencode locally on port 4096 |
| A5 | The `freemodels/refresher.go` `manager.Runnable` + `NeedLeaderElection` pattern supports a long-running reconciler that claims PG rows | US-64.8: follow the precedent exactly |
| A6 | expr-lang (`github.com/expr-lang/expr`) is CGO-free, embeddable, and type-checks against an `outputSchema`-derived environment at workflow-validate time | US-64.1: add dep, compile a condition expression against a typed env, confirm error on field mismatch |
| A7 | Activating a suspended workspace from the controller takes ~22s (per README-LLM.md) and that is acceptable latency for cron/webhook triggers | US-64.8: measure on a real cluster |
| A8 | opencode returns a machine-distinguishable error when a named agent profile does not exist (so we can map to `agent_not_found`) | US-64.1: POST a prompt with a non-existent agent name; capture the error |
| A9 | `/sandbox-runtime/secrets-env` is readable by agentd and contains the env-secret entries the `http` node needs to resolve `{{secrets.NAME}}` | US-64.1: read the file from a running workspace, confirm format (KEY=VALUE lines) and agentd's read access |

---

## Design Decisions

### D1 — Orchestrator in the controller, NOT the API
The API is stateless by design. Run-driving goroutines make it stateful, break horizontal scaling, and die on pod recycle. The controller is the stateful control plane with leader election. The API only writes rows and returns.

### D2 — In-workspace execution only
All node execution happens inside the target workspace pod. Reuses existing isolation (the workspace IS the sandbox), filesystem, git, and materialized credentials. A server-side runner would need a second sandbox + a second secret injection path — a different product. Cron-fired runs resume the workspace first (~22s, acceptable for background automation).

### D3 — Run to completion or fail. NO resume.
If the workspace dies, is suspended, or the controller restarts mid-run, the run is marked `failed` with a machine-readable `error_code`. This drops the hardest 30% of reconciler logic (frontier reconstruction, mid-flight node recovery). v2 adds resumability only if real long-running workflows prove to need it.

### D4 — `agent` not `llm`
An `agent` node delegates a sub-task to a **named opencode agent** (with prompt template, output schema, session lifecycle, retries), not a raw LLM call. The agent can use any injected MCP tool during its turn. This matches the reference workflow's `agent: "sdm-assistant"`, `enforceStructuredOutput`, `releaseSessionOnComplete` model.

### D5 — Two trigger actions, not five
`run_workflow` and `run_script`. Session actions (`reply`/`prompt`/`new_session`) are `agent` nodes inside a workflow — the trigger layer does not re-implement composition. Triggers initiate; they don't wait for cognition.

### D6 — Spec snapshot pinned per run; no version column in v1
`workflow_runs.spec_snapshot` is an immutable copy of the parsed DAG at run start. Editing a workflow does not affect in-flight runs. v1 does not support `version: pinned` triggers (there is no `version_count` column on `workflows`) — all triggers run the *current* spec at fire time, and the snapshot isolates in-flight work. Versioning/pinning is deferred until a real "lock this trigger to workflow v3" requirement surfaces; adding it later is additive (new column + a pin field on triggers) and does not break the snapshot model.

### D7 — Two ownership tiers (user, org)
Three tiers (platform/admin/org/user) is consistency-for-its-own-sake for an unproven feature. Start with two; add admin/platform tier when a real "workflow that runs against every workspace" requirement surfaces.

### D8 — Single-in-flight-per-workflow, hardcoded (no configurability in v1)
Eliminates cron overlap AND closes the TOCTOU race two concurrent webhook deliveries would otherwise hit. Enforced via `CREATE UNIQUE INDEX uq_workflow_run_single_inflight ON workflow_runs(workflow_id) WHERE status IN ('queued','running')` — the insert itself is the atomic gate; the loser gets a unique-violation. This is the same concurrency-hardening class Epic 23 applied to the controller's status writes.

**v1 deliberately hardcodes the limit at 1.** There is no `maxConcurrentRuns` setting, no per-workflow configurability, no count-check path. Configurable N>1 reintroduces the TOCTOU problem (a count check between two inserts is racy) and requires either a per-row index toggle (impossible — partial indexes are table-wide) or a separate advisory-lock mechanism that fights the index. Both are over-engineering for an unproven feature. N>1 with queueing ships together in v2, designed as one mechanism rather than two competing ones.

**Implication for webhooks (Edge Case 3):** when a second webhook delivery arrives while a run is in flight, the unique-index violation surfaces to the sender. For event-driven webhooks (distinct real-world events), this is documented data loss unless the sender retries — see Edge Case 15.

### D9 — Two distinct templating mechanisms (NOT one engine)
The feature uses **two deliberately separate** mechanisms with different trust boundaries:

1. **Prompt templates** (`agent` node `prompt`, `http` node `body`/`headers`/`url`, trigger `input_template`) use **Go `text/template`** with dot-syntax (`{{.input.meetingId}}`). Pure interpolation: no expression evaluation, no method calls, no function calls. Inputs include attacker-controlled webhook payloads — interpolation is safe by construction (a malicious `{{` in `rawSummary` is treated as literal text, not evaluated). Using a full expression engine here would create an injection surface for no benefit.

2. **Condition expressions** (`condition` node) use **expr-lang** (`github.com/expr-lang/expr`). Full expressions (`input.skipped == true`, `input.error_code == 'transient'`). The expression is author-controlled; the input is data only. expr-lang is type-checked at workflow-validate time against the predecessor's `outputSchema` (assumption A6 — closed by the spike).

Both are CGO-free. expr-lang is a hard dependency only because `condition` is a v1 node (proven necessary by the reference workflow's guard clause). `text/template` is stdlib — no new dependency. **Do not unify these.** The trust boundary difference is the point: interpolation handles untrusted data, expressions handle author-trusted logic.

### D10 — agentd built-in MCP server AND in-workspace agent authoring deferred to v2
The agentd built-in MCP server (new MCP-over-HTTP endpoint inside the pod) is expensive security-critical net-new code. v1 exposes workflow/trigger tools via the **platform MCP server** (`pkg/mcp/server.go`) only — useful for external agents (Claude Desktop, Cursor) authenticated via API key. The in-workspace opencode agent has **no platform API credentials** in v1 and therefore cannot author workflows/triggers — enabling it requires either the agentd built-in MCP server or a workspace-scoped token mechanism, both net-new security surfaces. v2.

### D11 — External agent authoring gated by org policy + quota
External agents using the platform MCP server can CRUD workflows/triggers iff `allow_user_workflow_create` policy is enabled and per-user quota is not exceeded. All changes audit-logged. Mirrors Epic 53's `allow_user_mcp_servers` gate. (In-workspace agent authoring is blocked by D10, not by this policy.)

### D12 — Webhook validation is advisory; manual/cron is strict
Webhook authors don't control what GitHub sends. A bad payload still fires (HMAC is valid); the run records `validation_error`; the workflow can branch on it. Manual runs (via `POST /workflows/:id/runs`) and cron inputs are author-controlled and rejected on schema mismatch.

---

## Edge Cases (each named, not hand-waved)

1. **Workspace dies mid-node** → agentd call fails or times out → run fails (`error_code=workspace_unavailable`). The workspace being in a bad state is reported distinctly from a workflow bug.
2. **Workspace suspended mid-run by user** → orchestrator's agentd call fails or it observes phase change → run fails (`workspace_unavailable`), never hangs.
3. **Concurrent runs from overlapping triggers** → single-in-flight-per-workflow (D8) enforced via a **partial unique index** `uq_workflow_run_single_inflight ON workflow_runs(workflow_id) WHERE status IN ('queued','running')`, NOT a check-then-insert. Two concurrent webhook deliveries that both pass a count check would both create runs (TOCTOU — the class Epic 23 hardened the controller against); the unique index makes the insert itself the atomic gate.
4. **Node output > `maxNodeOutputBytes` (default 1 MiB)** → node **fails** with `error_code=output_oversize`. No spill to file in v1 — a spill on the PVC is unreachable when the workspace is suspended/deleted and has no clean retrieval path, making it a second-class artifact. v1 forces authors to return summaries, not dumps; spill + object storage is v2.
5. **Cancellation propagation** → user cancels → orchestrator calls `POST /v1/workflow/node/cancel` on agentd → in-flight node killed → run marked `canceled` (distinct from `failed`).
6. **LLM schema mismatch** → on `enforceStructuredOutput`, retry within `maxAttempts` with a repair hint appended; exhausted → node fails (`schema_mismatch`).
7. **DAG validation at create/update** → topological sort + reachability: reject cycles, dangling refs, unreachable nodes, multiple starts, missing end. **Additionally:** `condition` nodes must have edges covering every declared branch id + an `otherwise` edge; expr-lang expressions referencing fields absent from the predecessor's `outputSchema` fail type-inference at validate time. Pure function, fully tested (US-64.4).
8. **Secret leakage in specs** → spec validation forbids raw secrets in `http` headers/body/url; only `{{secrets.NAME}}` refs allowed. Lint at write time (pattern-detection: `Bearer `, `ghp_`, `sk-`, `xoxb-`, long hex/base64).
9. **Webhook flood** → per-webhook + per-IP + global rate limits (Redis token buckets); idempotency dedup collapses retried duplicates.
10. **Per-trigger input validation for webhooks** → advisory (D12); UI surfaces "fired 47×, 45 failed validation" clearly.
11. **Controller restart mid-run** → on startup, any `running` runs marked `failed` (`error_code=api_restart`). No resume (D3).
12. **Missed cron fires (controller downtime)** → late cron ticks where `next_fire_at < now - tickInterval` are **skipped but logged**: the scheduler writes a `trigger_fires` row with `status=skipped` so the user can distinguish "trigger broken" from "controller was down." v1 policy is always skip; bounded catch-up is v2.
13. **Webhook latency on suspended workspace** → a webhook targeting a suspended workspace pays the ~22s wake-up cost before the first node runs. This is an acknowledged v1 limitation of in-workspace-only execution. v2 answers: server-side execution for headless flows, or warm/keepalive workspaces for webhook-bound targets. The UI should surface "waiting for workspace" state during wake-up.
14. **Workflow runs vs. workspace auto-suspend lifecycle** → a workflow firing hourly against a workspace that auto-suspends after 15min idle (Epic 47) will wake-sleep-wake-sleep each cycle. This is correct behavior (the PVC persists), but operators should be aware of the interaction. No v1 mitigation; document in operator guide.
15. **Webhook concurrency-reject = documented data loss for distinct events** → single-in-flight (D8) means a second webhook delivery arriving while a run is in flight is rejected with `409 "already running"` + `Retry-After: 30`. For cron this is fine (next hour's run is equivalent). For event-driven webhooks (GitHub issue #451 arrives while #450 is processing), if the sender does not retry, #451 is **lost**. This is an accepted v1 trade of in-workspace single-in-flight execution. Webhook consumers who cannot tolerate drops must either (a) rely on sender retries, or (b) wait for v2 queueing. Returning `200` would be silent data loss; `409` makes the loss observable. Documented in operator guide.
16. **Concurrent workflow run + interactive session on same workspace** → a user actively chatting at 9am while a cron-fired run executes at 9am have **no coordination**. A `script` node may modify `opencode.db` or workspace files the agent is reading; a `script` node may delete a file the agent has open. The workspace is a single-user shared-mutable-state environment; concurrent access is the user's responsibility. Building file locking or DB coordination is over-engineering. The UI surfaces "a workflow run is active on this workspace" so the user knows to expect interference. Documented footgun, not a bug.
17. **Named agent not found** → the `agent` node references `agent: sdm-assistant`. If that profile does not exist in the workspace's opencode config, the API server cannot validate at workflow-create time (it doesn't know the workspace's agents). The node **fails at first execution** with `error_code=agent_not_found` and a clear message naming the missing profile. Validated during the spike (US-64.1) by confirming opencode's error behaviour for a missing agent name.
18. **Workspace never activates** → a cron-fired run targets a workspace that fails to wake (bad image, quota exceeded, node pressure). Without a separate activation deadline, the reconciler goroutine blocks on the activation wait until the *run* timeout (default 3600s); 50 dead workspaces pin the global controller semaphore for an hour each. The orchestrator uses a **separate `workspaceActivationTimeoutSec` (default 120s)** — after which the run fails fast with `workspace_unavailable`, releasing the semaphore. The activation wait does NOT consume the run timeout budget; a 10-minute workflow on a 2-minute wake-up gets the full 10 minutes once Active.

---

## Non-Functional Requirements

### Security
- Webhook secrets in the existing crypto envelope (KEK for admin/org, session DEK for user — zero-knowledge).
- HMAC verify with `subtle.ConstantTimeCompare`; timestamp skew check.
- IP allowlist on webhooks (early reject).
- No raw secrets in workflow specs (validation-enforced).
- `script`/`http`/`agent` run as the workspace user inside the existing sandbox — no new isolation primitive.
- All trigger/workflow CRUD audit-logged.
- Per-tenant rate limits and quotas.

### Scalability & Performance
- Scheduler tick interval tunable (default 30s); fine up to ~10k triggers; minute-level precision is the floor.
- Controller bounded worker pool (global semaphore); queue overflow policy = reject (v1).
- Per-node output capped (`maxNodeOutputBytes`, default 1 MiB); oversize **fails the node** (`output_oversize`) — no spill in v1.
- PG `FOR UPDATE SKIP LOCKED` for run claiming — multi-replica-safe (though controller is leader-elected).
- Existing eventbroker reused for SSE fan-out (goroutine-safe).

### Robustness
- All state transitions persisted after each node.
- No goroutine-per-run held across controller restarts.
- Timeouts on every node and every run.
- Retries with bounded backoff per node.
- Fail-loud on partial failure (no silent corruption).

---

## Observability

Prometheus metrics (added per-component, not a separate epic):
- `workflow_runs_total{status, error_code, owner_type}` counter
- `workflow_run_duration_seconds{workflow_id}` histogram
- `workflow_node_duration_seconds{node_type}` histogram
- `workflow_concurrent_runs` gauge
- `trigger_fires_total{source, status}` counter
- `webhook_deliveries_total{webhook_id, status}` counter
- `workflow_scheduler_tick_duration_seconds` histogram

SSE events (via existing `userBroker.PublishToUser`):
- `workflow.run_started`, `workflow.node_finished`, `workflow.run_finished`
- `trigger.fired`

---

## Stories

| Story | Title | Effort | Depends On |
|---|---|---|---|
| US-64.1 | Validation spike: agentd node-execution + opencode structured-output contract (BLOCKING) | 1.5d | None — closes A1/A2/A3/A4 |
| US-64.2 | Data model: migration `000016` + Go domain/transfer types | 0.5d | US-64.1 |
| US-64.3 | Storage layer: workflow/trigger/webhook/run/fire stores + concurrency partial unique index | 2d | US-64.2 |
| US-64.4 | Workflow CRUD handlers + routes (user + org) + DAG validation (topo + condition-edge coverage + expr-lang type inference) | 2d | US-64.3 |
| US-64.5 | Trigger CRUD handlers + routes (cron/webhook sources; run_workflow/run_script actions; circuit-breaker columns) | 1.5d | US-64.3 |
| US-64.6 | Webhook receiver: HMAC-SHA256 verify, idempotency table, IP allowlist, rate limit | 1.5d | US-64.5 |
| US-64.7 | agentd node-execution endpoint (script/agent/http/condition dispatch + cancel + session lifecycle) | 2d | US-64.1 |
| US-64.8 | Controller workflow reconciler: claim, wake workspace, drive nodes, persist, retries, timeouts, concurrency (partial-index enforcement), cancel, fail-on-restart, oversize-output hard-fail | 3d | US-64.3, US-64.7 |
| US-64.9 | Scheduler: leader-elected ticker, cron firing, per-trigger rate limit, trigger_fires logging, missed-fire skip-logging, circuit-breaker auto-disable | 2d | US-64.5, US-64.8 |
| US-64.10 | SSE events + Prometheus metrics + error_code plumbing | 1d | US-64.8 |
| US-64.11 | Platform MCP server tools for external agents (workflow/trigger CRUD + run + cancel) | 1d | US-64.4, US-64.5 |
| US-64.12 | Frontend: workflow YAML editor + validator, trigger list, run history with per-node status/output, webhook management | 3d | US-64.4, US-64.5, US-64.8 |
| US-64.13 | E2E integration: full wired path (cron + webhook + manual run-create → workflow → in-workspace execution → SSE → UI) | 2d | US-64.6, US-64.8, US-64.9, US-64.12 |

Total estimated effort: ~23 days.

---

## Dependency Graph

```
US-64.1 (spike: agentd + opencode contract)   ─── BLOCKING, starts immediately
   │
   ├──> US-64.2 (data model)
   │       │
   │       └──> US-64.3 (stores)
   │               ├──> US-64.4 (workflow CRUD + DAG validation)
   │               ├──> US-64.5 (trigger CRUD)
   │               │       │
   │               │       └──> US-64.6 (webhook receiver)
   │               │
   │               └──> US-64.8 (controller reconciler)  ─── also depends on US-64.7
   │                       │
   │                       ├──> US-64.9 (scheduler)
   │                       └──> US-64.10 (SSE + metrics)
   │
   ├──> US-64.7 (agentd node-execution endpoint)  ─── also depends on US-64.1
   │
   ├──> US-64.11 (platform MCP tools)  ─── depends on US-64.4, US-64.5
   │
   └──> US-64.12 (frontend)  ─── depends on US-64.4, US-64.5, US-64.8
           │
           └──> US-64.13 (e2e)  ─── depends on US-64.6, US-64.8, US-64.9, US-64.12
```

Two stories start on day 1: US-64.1 (the blocking spike) and US-64.2 (data model, which can proceed in parallel since it's pure schema). Nothing else merges before US-64.1 is accepted.

---

## Execution Strategy

**Phase 0 — De-risk (day 1–2):** US-64.1 spike closes A1–A4. Produce a contract artifact (the agentd node-execute request/response shapes, the opencode structured-output capture method). No production code merges before this.

**Phase 1 — Foundation (days 2–5):** US-64.2 (migration) → US-64.3 (stores). Backend-only, fully tested, no UI.

**Phase 2 — Surfaces (days 5–9):** US-64.4 (workflow CRUD + DAG validation), US-64.5 (trigger CRUD), US-64.7 (agentd endpoint) in parallel after US-64.3. End of Phase 2: definitions are manageable via API; agentd can execute a node.

**Phase 3 — The engine (days 9–13):** US-64.8 (controller reconciler — the load-bearing piece), then US-64.9 (scheduler) and US-64.6 (webhook receiver). End of Phase 3: a queued run actually executes end-to-end inside a workspace.

**Phase 4 — Polish + UX (days 13–18):** US-64.10 (SSE + metrics), US-64.11 (platform MCP tools), US-64.12 (frontend) in parallel.

**Phase 5 — Closure (days 18–20):** US-64.13 (e2e across all three trigger sources + the full execution path). End of Phase 5: the full human-workable, end-to-end-verified feature.

Each phase ends with `make test && make build && make lint` green and a worklog entry. No phase skips the validator loop (README-LLM.md Multi-Agent Workflow).

---

## Per-Story Detail

### US-64.1: Validation spike — agentd + opencode contract (BLOCKING)

**Goal:** Convert A1–A9 from "believed" to "validated with evidence." Produce the exact node-execution contract that US-64.7 implements, the structured-output capture method US-64.8 relies on, the script wrapper contract, the secret resolution path, and the named-agent error behaviour. No production code merges before this.

**Deliverables (written, in this epic's folder):**
1. `NODE-EXECUTE-CONTRACT.md` — the request/response JSON shapes for `POST /v1/workflow/node/execute` per node type (`script`, `agent`, `http`, `condition`), with captured examples from real workspace runs.
2. Confirmation of opencode's structured-output behaviour (A2): does a named agent with a JSON Schema produce conformant output, or must we validate + retry on our side?
3. The cancel mechanism: how agentd kills an in-flight `script` (process kill) vs `agent` (opencode interrupt) vs `http` (context cancel).
4. **The script wrapper contract** (A3): the exact wrapper source per language (Python/Node/Go) that turns `def handler(input) -> dict` into a stdin/stdout process. Round-trip a real handler in a real workspace.
5. **Named-agent error behaviour** (A8): POST a prompt with a non-existent agent name; capture the error shape that maps to `agent_not_found`.
6. **Secret resolution path** (A9): read `/sandbox-runtime/secrets-env` from a running workspace; confirm format (KEY=VALUE) and agentd's read access; document the `{{secrets.NAME}}` → env-var-key mapping.
7. **expr-lang type-check** (A6): add the dep, compile a condition expression against a typed environment derived from a sample `outputSchema`, confirm the error message on field mismatch.

**Validation evidence required:** captured request/response from a live workspace for each of the above. "It should work" is not acceptable (Rule 7).

**Tests (TDD):** N/A (spike). The live execution demonstrations ARE the tests.

### US-64.2: Data model — migration `000016` + Go types

**Goal:** Land the schema and types. No behaviour yet.

**Deliverables:**
- `api/migrations/000016_triggers_workflows.up.sql` + `.down.sql` (and helm mirror via `make chart-sync-migrations`). Follow `000012_mcp_servers` idioms.
- Domain types in `pkg/types/workflows.go` (transfer objects only — request/response shapes).
- Register new `instance_settings` and `org_policies` keys in `pkg/settings/registry.go`.

**Acceptance:** migration applies cleanly up/down; types compile; `make deepcopy` green if any CRD-adjacent types are added (none expected — workflows are API-owned relational data, no CRD).

### US-64.3: Storage layer

**Goal:** Postgres-backed stores for all six tables, with the crypto envelope for webhook secrets (reuse `mcp_servers` pattern).

**Deliverables:** `pkg/workflows/store.go` (or extend `pkg/secrets/` for the webhook-secret path). CRUD + the crypto paths (KEK for org, session DEK for user) + the run-claim query (`FOR UPDATE SKIP LOCKED`).

**Tests:** table-driven unit tests against `go-sqlmock` + integration tests against a real PG (per testing requirements).

### US-64.4: Workflow CRUD + DAG validation

**Goal:** User + org handlers, routes (copy `registerMCPRoutes` at `router.go:1577`), and the DAG validator.

**Deliverables:**
- `api/internal/handlers/workflows.go` — single handler struct, two constructors (user, org), mirroring `mcp_servers.go`.
- DAG validation pure function. Must catch:
  - Cycles (topological sort fails)
  - Dangling refs (edge source/target points at non-existent node)
  - Unreachable nodes (not reachable from start)
  - Multiple starts, missing end
  - **`condition` nodes whose edges don't cover every declared branch id + `otherwise`** (else runtime → no edge to follow → ambiguous failure). The validator must enumerate the condition's declared `conditions[].id`, assert an edge exists for each plus the implicit `otherwise`.
  - **expr-lang type errors**: expressions referencing fields absent from the predecessor's `outputSchema` must fail type-inference at validate time (expr-lang compiles against a typed environment built from the upstream node's schema — contract validated by US-64.1 A6). **Prompt templates use Go `text/template` (D9) — no expression evaluation, no injection surface; validate only that referenced fields exist in the input schema.**
- `defaults` block merging into per-node config (precedence: node-level > `defaults` block > system default; see Defaults Precedence below).
- Quota enforcement (`workflows.maxPerUser/maxPerOrg`).
- **Audit logging** of all create/update/delete operations (actor, action, target id, before/after spec) — reuse the existing audit-log pattern (see `org_sso.go` for the `sso.update` / `sso.delete` precedent). Every mutation emits an `audit_log` row.

**Tests:** happy + unhappy + validation-failure paths; table-driven DAG-validator tests covering every failure mode above (cycles, dangling, multi-start, **missing-otherwise-edge**, **unreachable-condition-branch**, **expr-type-mismatch**); **defaults-precedence** (node-level wins over `defaults`, `defaults` wins over system default).

**Defaults Precedence (Issue 6):** when resolving a node's effective config, the order is **node-level > workflow `defaults` block > system default**. Only these fields may be set in `defaults` (others are ignored with a validation warning): `maxAttempts`, `timeout`. Behavioural fields (`session`, `enforceStructuredOutput`, `language`, `handler`, `agent`, `url`, `method`, `conditions`) MUST be specified per-node — defaulting them would mask configuration errors. The `defaults` block exists solely to avoid repeating `maxAttempts: 2, timeout: 10m` on every node of a 6-node workflow.

### US-64.5: Trigger CRUD

**Goal:** User + org handlers for triggers (cron + webhook sources only); `source_config`/`target_config` validation per source/action type.

**Deliverables:** `api/internal/handlers/triggers.go`. Validates cron expressions (via a small cron parser — no full library; v1 supports basic expr + interval), webhook bindings (referenced webhook must exist + caller owns it), target workflow/script existence. `next_fire_at` computed on insert for cron sources. Manages `consecutive_failures` / `auto_disable_after` columns (the circuit breaker is *enforced* by US-64.9, but the CRUD handler must reject `auto_disable_after < 1` and surface the current breaker state on read). **Audit logging** of all create/update/delete operations (same pattern as US-64.4).

### US-64.6: Webhook receiver

**Goal:** Public `POST /api/v1/hooks/:webhook_id` endpoint with HMAC verify, idempotency, IP allowlist, rate limit.

**Deliverables:** `api/internal/handlers/webhook_triggers.go`. Raw body capture (not gin's binding — needed for HMAC), HMAC verify (`crypto/hmac` + `subtle.ConstantTimeCompare`), timestamp skew, dedup insert into `webhook_deliveries`, rate-limit check (Redis token bucket), then **single-transaction** write of `trigger_fires` + `workflow_runs` (the partial unique index may reject with a unique-violation → whole tx rolls back; on rollback, commit a separate `trigger_fires` row with `status=skipped, action_result:{reason:"already_running"}`, return `409 "already running"` **with `Retry-After: 30`** — see Edge Case 15 + the Webhook Security atomicity bullet).

**Tests:** valid/invalid signature, missing signature, replay (duplicate dedup key), IP-rejected, rate-limited, idempotent re-delivery, **concurrency-reject (409 + Retry-After + skipped-fire-row committed)**, **fire-row-orphan-prevention (no fired-status row when run was rejected)**.

### US-64.7: agentd node-execution endpoint

**Goal:** `POST /v1/workflow/node/execute` on agentd (port 4098) dispatching the four node types + `POST /v1/workflow/node/cancel`.

**Deliverables:** `cmd/workspace-agentd/workflow_execute.go`. Dispatch table (one function per node type). `script` → write handler to temp file, generate per-language wrapper per the US-64.1 contract, exec via `mise`, capture wrapper stdout/stderr within context deadline; non-dict return or marshal failure → `script_output_invalid`; non-zero exit → `script_failed`. `agent` → POST to opencode `/sessions/:id/prompt` (local port 4096), capture response, validate schema; **session lifecycle per the `agent` node spec** (`ephemeral` create-and-destroy default, `existing` fails on missing session with `session_not_found`, `new` persists); **missing named agent → `agent_not_found`** (per A8). `http` → `net/http` with context; **resolve `{{secrets.NAME}}` against `/sandbox-runtime/secrets-env`** (per A9); missing NAME → `secret_not_found`. `condition` → expr-lang eval against typed environment. Cancel → kill process / interrupt opencode / cancel context. **Output size check**: if response > `maxNodeOutputBytes`, return an error result with `error_code=output_oversize` (no truncation, no spill — the node fails).

**Tests:** round-trip per node type against a real workspace (per US-64.1 contract); cancel mid-execution; timeout enforcement; **session-lifecycle per mode**; **named-agent-not-found**; **secret-not-found**; **oversize-output hard-fail**; **script-output-invalid on bad return**.

### US-64.8: Controller workflow reconciler (load-bearing)

**Goal:** The state machine. Claim queued runs, ensure workspace active, drive nodes, persist transitions, enforce retries/timeouts/concurrency, handle cancel, fail-on-restart.

**Deliverables:** `controller/internal/workflows/reconciler.go` implementing `manager.Runnable` + `NeedLeaderElection` (follow `freemodels/refresher.go:156-185`). Bounded worker pool. Run-claim query (`FOR UPDATE SKIP LOCKED`). Per-node retry loop with backoff. Per-node + global timeout via context. Cancel-flag polling + agentd cancel call. On startup: mark `running` runs as `failed (api_restart)`. **Concurrency is enforced by the partial unique index** (D8) — the reconciler does not need its own per-workflow lock; the insert at run-create is the gate, and the reconciler simply claims whatever is `queued`.

**Tests:** TDD every state transition. Happy path (linear DAG), condition branching, node failure → run failure, retry-then-success, retry-exhausted, timeout, cancel, workspace-dies-mid-node, controller-restart-marks-failed, concurrency-reject (second insert hits unique violation), **oversize-output-node-failure**. Integration test exercising the real wiring (controller → agentd → opencode).

### US-64.9: Scheduler

**Goal:** Leader-elected ticker that polls due cron triggers and fires them, with circuit-breaker and missed-fire logging.

**Deliverables:** `controller/internal/workflows/scheduler.go`. 30s default tick. Selects `triggers WHERE source_type='cron' AND enabled AND next_fire_at <= now()`. Per-trigger rate limit (Redis token bucket). On fire: compute input from `input_template`, write `trigger_fires` row, create `workflow_runs` row (subject to single-in-flight partial index — D8), advance `next_fire_at`. **Circuit breaker**: after each run reaches terminal state, increment or reset `consecutive_failures`; when it crosses `auto_disable_after`, set `enabled=false`, write a `trigger_fires` row with `status=auto_disabled`, emit SSE, **audit-log the auto-disable event** (actor = system, action = `trigger.auto_disabled`, target = trigger id, reason = `consecutive_failures >= N`). **Missed fires**: if `next_fire_at < now() - tickInterval` (controller was down), write a `trigger_fires` row with `status=skipped` (do NOT fire) — never silent. v1 policy is always skip; bounded catch-up is v2.

**Tests:** due-trigger selection, rate-limit drop, next-fire computation, **missed-fire-skip-logged**, **circuit-breaker-trips-at-threshold**, **circuit-breaker-resets-on-success**, leader-election-only-one-replica.

### US-64.10: SSE events + metrics

**Goal:** Wire the existing eventbroker + Prometheus metrics.

**Deliverables:** publish `workflow.run_started/node_finished/run_finished` and `trigger.fired` via `userBroker.PublishToUser`. Register the 7 metrics listed in Observability. Plumb `error_code` end-to-end.

### US-64.11: Platform MCP server tools (external agents)

**Goal:** Add workflow/trigger tools to `pkg/mcp/server.go` for **external** agents (Claude Desktop, Cursor, etc.) authenticated via API key. These tools are NOT reachable by the in-workspace opencode agent in v1 (see D10).

**Deliverables:** `workflow_list`, `workflow_get`, `workflow_create`, `workflow_update`, `workflow_run`, `workflow_status`, `workflow_cancel`, `trigger_list`, `trigger_create`, `trigger_update`, `trigger_delete`. (No `trigger_fire` — there is no manual source type; `workflow_run` starts a run directly.) Follow the existing tool-handler signature exactly (`func(ctx, req) (*CallToolResult, error)`, never non-nil error for logical failures). Add corresponding methods to the `APIClient` interface + `HTTPClient`. Authoring tools gated by `allow_user_workflow_create` policy + per-user quota (D11).

### US-64.12: Frontend

**Goal:** Workflow YAML editor with live validation, trigger management, run history with per-node status/output, webhook management UI.

**Deliverables:** React components under `frontend/src/components/workflows/`. YAML editor with schema validation (reuse existing patterns). Run history view polling or SSE-subscribed. Webhook secret reveal (one-time, like API keys). **Circuit-breaker state shown** on trigger rows (`auto_disabled` badge + re-enable action). **Webhook wake-up latency** shown in run detail when the run waits for a suspended workspace. **Active-run indicator** on workspace views when a workflow run is in flight (Edge Case 16 — concurrent run + interactive session has no coordination; the user must be informed). Follow existing frontend conventions (TanStack Query where applicable).

### US-64.13: E2E integration

**Goal:** Prove the full wired path end-to-end across cron, webhook, and manual run-create.

**Deliverables:** integration tests under `tests/` (or `api/internal/tests/integration/`). Scenarios: (1) cron trigger fires workflow → workspace wakes → nodes execute → run succeeds → SSE event received; (2) webhook trigger with valid signature → run starts; invalid signature → 401; replay → dedup; (3) manual run via `POST /workflows/:id/runs`; (4) workspace dies mid-run → run fails with `workspace_unavailable`; (5) cancel mid-run → run `canceled`; (6) single-in-flight reject (concurrent inserts, second hits unique violation, webhook sender gets `409 + Retry-After`); (7) circuit breaker: 10 consecutive failures → trigger auto-disabled + audit entry; (8) missed-fire: simulate controller downtime → `trigger_fires.status=skipped` row written; (9) oversize output → node fails `output_oversize`; (10) named-agent-not-found → node fails `agent_not_found`; (11) secret-not-found in `http` node → node fails `secret_not_found`. No unwired code (per PR Review Guide E2E Wiring Verification).

---

## Out of Scope (deferred with rationale)

| Item | Reason |
|---|---|
| `transform` node | Reference workflow folds transforms into `script`; not load-bearing |
| `parallel` / `delay` nodes | Not in reference workflow; speculative until a real need |
| `mcp_call` node | Redundant — `agent` uses MCP tools natively; `script` does raw JSON-RPC |
| `manual` trigger source type | A manual trigger is just `POST /workflows/:id/runs` with extra steps; no entity needed. `trigger_fires` records automated fires only |
| `workflow_done` / `mcp` trigger sources | Chaining is speculative until a real "when X finishes, start Y" surfaces |
| Session trigger actions | They are `agent` nodes; triggers initiate, don't wait for cognition |
| Platform/admin ownership tier | Three tiers is consistency-for-its-own-sake on an unproven feature |
| Child workflows | Without resume + run-to-completion, a child is just a node |
| Dynamic DAGs | If structure is dynamic, use the agent directly |
| Long-lived waits / human approval | Different model; not v1 |
| Cross-workspace orchestration | A run lives in one workspace; use `http` for cross-workspace data |
| Sagas / compensation | v1 doesn't undo; cleanup is a separate workflow |
| Run resumability | D3 — ship fail-on-restart, add resume in v2 if needed |
| In-workspace agent authoring | D10 — the in-pod agent has no platform API credentials in v1; requires agentd built-in MCP or workspace-scoped tokens (net-new security surface) |
| agentd built-in MCP server | D10 — expensive net-new security-critical code; defer to v2 |
| Workflow versioning / `version: pinned` triggers | D6 — snapshot-per-run isolates in-flight work; explicit versions deferred until a real pin-to-vN need surfaces |
| Slack / Discord webhook signature schemes | Generic HMAC-SHA256 covers the common case |
| Queueing on concurrency overflow | D8 — reject (via partial unique index) is simpler; queueing is v2 (designed together with N>1 concurrency, not bolted on) |
| Configurable `maxConcurrentRuns > 1` | D8 — v1 hardcodes single-in-flight; N>1 reintroduces count-check TOCTOU and conflicts with the table-wide partial index; ship with queueing in v2 as one mechanism |
| `tags` on runs | Cut from v1 — speculative; `(workflow_id, status, created_at)` filtering is sufficient. Trivial to add later (one column, one GIN index, no migration pain) |
| Missed-fire catch-up policy | v1 skips (but logs it); catch-up is v2 |
| Oversize-output spill to file | Hard-fail (`output_oversize`) is cleaner v1 — spill on PVC is unreachable when suspended; spill + object storage is v2 |
| Run output retention / TTL cleanup | Ship without; add a janitor (jwt_session_janitor pattern) post-v1 |
| Visual DAG canvas editor | v1 is YAML; canvas is v2 |

---

## Open Questions

1. **Workspace selection for runs with no pinned `target_workspace_id`** — caller picks at run time (simplest). Org-admin "run on behalf of a user's workspace" is a permission question worth its own story post-v1.
2. **Global controller concurrency default** — per-workflow is single-in-flight (D8). The controller's global semaphore needs a sensible default (e.g. 50 in-flight runs total); tune after load testing.
3. **Missed-fire policy** — v1 skips. If catch-up becomes needed, the policy is per-trigger (`skip` | `catchup_bounded` | `fire_once`).
4. **Run output retention** — `workflow_node_runs.output` grows unboundedly without a TTL. Ship without; add a janitor post-v1.

---

## Reference

A real-world workflow (6 nodes: `validate-input` → `skip-choice` (condition) → `load-known-entities` → `process-meeting` (agent) → `phonetool-verify` (agent) → `persist-and-finalize`) informed this design. It validates: linear-with-guard is the norm; `script` + `agent` + `condition` are the workhorses; per-node `outputSchema` is mandatory; inline handlers as strings are the primary authoring path; idempotency lives in scripts (which single-in-flight makes belt-and-suspenders).

**Honest fit assessment:** This workflow fits the v1 *execution model* — its 6-node DAG, condition branching, named-agent delegation, and structured-output pattern map directly onto our node types. Two caveats, both outside the engine itself: (1) the `phonetool-verify` agent node uses `ReadInternalWebsites`, an MCP tool — for that to work, the `sdm-assistant` agent must have that tool bound via Epic 53, so the reference workflow's full functionality *depends on Epic 53 being deployed*; (2) `persist-and-finalize` imports from `~/shared/meeting-intelligence`, coupling the workflow to workspace preconfiguration that is the user's responsibility, not the platform's. The engine runs the DAG; the workspace provides the dependencies.
