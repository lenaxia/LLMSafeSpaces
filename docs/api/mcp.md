# MCP Server

LLMSafeSpaces **is** an [Model Context Protocol](https://modelcontextprotocol.io/) (MCP) server. Any MCP-compatible client — Claude Desktop, a custom agent harness, an IDE extension — can connect to it and drive workspaces programmatically: create workspaces, activate them, send messages, manage credentials, and reply to agent questions and permission requests.

The server is implemented with [`mark3labs/mcp-go`](https://github.com/mark3labs/mcp-go) and lives in [`pkg/mcp/`](https://github.com/lenaxia/LLMSafeSpaces/tree/main/pkg/mcp). The binary entrypoint is [`cmd/mcp/main.go`](https://github.com/lenaxia/LLMSafeSpaces/tree/main/cmd/mcp).

## Transports

Two transports are supported:

| Transport | Flag | Use when |
|-----------|------|----------|
| **stdio** (default) | _(none)_ | The client launches the server as a subprocess (Claude Desktop, local CLI tools). |
| **SSE** | `--sse` | The server runs as a long-lived HTTP service the client connects to over the network. |

```bash
# stdio (default) — launched by the client as a subprocess
mcp \
  --base-url https://llmsafespaces.example.com \
  --api-key lsp_...

# SSE — long-lived HTTP server on :3001
mcp \
  --sse \
  --addr :3001 \
  --base-url https://llmsafespaces.example.com \
  --api-key lsp_...
```

## Configuration

The MCP server is a thin client over the REST API. It needs a base URL and an API key.

| Flag | Env var | Default | Purpose |
|------|---------|---------|---------|
| `--base-url` | `LLMSAFESPACES_URL` | `http://localhost:8080` | LLMSafeSpaces API base URL |
| `--api-key` | `LLMSAFESPACES_API_KEY` | _(empty)_ | API key for authentication |
| `--sse` | — | `false` | Use SSE transport instead of stdio |
| `--addr` | `MCP_ADDR` | `:3001` | SSE listen address |
| `--timeout` | — | `300s` | Default timeout for `session_message` |

If no API key is configured, the server still starts but every API call will fail with `401`. This is intentional — it lets you wire the transport first, then inject credentials.

## Connecting a client

### Claude Desktop (stdio)

Add the server to your `claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "llmsafespaces": {
      "command": "mcp",
      "args": [
        "--base-url", "https://llmsafespaces.example.com",
        "--api-key", "lsp_your_api_key_here"
      ]
    }
  }
}
```

Restart Claude Desktop. The tools appear as `mcp__llmsafespaces__workspace_create`, etc.

### SSE (remote)

Point any MCP-compatible client at the SSE endpoint:

```
http://your-host:3001/sse
```

### The VS Code extension

The [VS Code extension](sdks.md#vs-code-extension) embeds the MCP tool surface as a Copilot Chat participant (`@llmsafespaces`), so you get the same capability without configuring an MCP client directly.

## Tools

The server advertises 44 tools, all workspace-centric — the sandbox layer is hidden from callers. Each tool maps to one or more REST API calls.

### Workspace lifecycle

| Tool | Required args | Description |
|------|---------------|-------------|
| `workspace_create` | `runtime`, `name` | Create a new workspace with a persistent development environment |
| `workspace_activate` | `workspace_id` | Activate a workspace (starts the agent). Required before creating sessions. |
| `workspace_stop` | `workspace_id` | Stop a workspace (suspends the agent, preserves all files) |
| `workspace_status` | `workspace_id` | Get a workspace's status: phase, conditions, agent health, and the supervisor restart history (restart count, last restart reason/verdict/time, suppressed restart count) |
| `workspace_refresh_compute` | `workspace_id` | Re-sync resource defaults + latest image version and rebuild the pod |

`runtime` is a string like `python:3.10`, `nodejs:18`, or `go:1.21`.

### Sessions

| Tool | Required args | Description |
|------|---------------|-------------|
| `session_create` | `workspace_id` | Create a conversation session in an active workspace. Returns the `sessionId` from `POST /workspaces/:id/sessions/new`. |
| `session_message` | `workspace_id`, `session_id`, `message` | Send a message and get the response (blocks until the agent replies). Optional `files` (array of upload paths) appends the v1 attachment manifest to the prompt — same semantics as the REST `/prompt` route. |
| `session_history` | `workspace_id`, `session_id` | Get the message history of a session (contract shape: `id`/`type`/`parts`) |

`session_message` sends the prompt via `POST .../sessions/:id/prompt`, then subscribes to the workspace event stream (`GET .../session-events`) until the session goes idle. If the agent asks a question during the turn, the tool returns a structured `{"type":"question","request":{...}}` result instead of blocking; permission requests are auto-approved (`always`) in headless mode.

### File uploads

| Tool | Required args | Description |
|------|---------------|-------------|
| `workspace_file_upload` | `workspace_id`, `filename`, `content_b64` | Upload a file to `/workspace/uploads/` on the workspace PVC. `content_b64` is base64 (wrapped/whitespace tolerated). Returns `{"path","name","size"}`; pass `path` values to `session_message`'s `files` param. |

`workspace_file_upload` decodes to at most **5 MiB** (larger inputs are rejected before decoding) — use the REST `POST /workspaces/:id/uploads` multipart route or an SDK for bigger files. Paths follow the manifest contract: `[llmsafespaces:attachment path="<path>" name="<name>"]` lines appended to prompts; agents read the files with their own tools.

!!! warning "`session_message` blocks"
    Like the REST `POST .../message`, this tool waits for the full assistant response and can take 30–120+ seconds. The default tool timeout is 300s. Messages are capped at 1 MiB.

### Agent input: `run_resolve`

The agent can pause and ask the caller a question or request permission for an action. `run_resolve` (US-65.7) is the unified reply tool for both — it replaced the former `session_question_reply`, `session_question_reject`, and `session_permission_reply` tools.

| Tool | Required args | Description |
|------|---------------|-------------|
| `run_resolve` | `workspace_id`, `request_id`, `reply` | Resolve a pending input request. For `que_*` IDs, `reply` is a JSON array of answers, e.g. `[["answer1"]]`. For `per_*` IDs, `reply` is `once`, `always`, or `reject`. Optional `message`. |

Question request IDs start with `que_`; permission request IDs start with `per_`.

### Credentials & models

| Tool | Required args | Description |
|------|---------------|-------------|
| `credential_create` | `kind`, `slug`, `api_key` | Create an LLM provider credential. Optional: `name`, `base_url`, `default_model`, `workspace_id` (auto-binds). |
| `credential_list` | _(none)_ | List configured credentials (names and IDs, never values) |
| `credential_delete` | `credential_id` | Delete a credential |
| `model_list` | `workspace_id` | List available models (requires active workspace) |
| `model_set` | `workspace_id`, `model` | Set the default model (e.g. `anthropic/claude-sonnet-4-5`) |

`kind` selects the adapter the agent loads. Valid kinds:

```
openai, anthropic, google, opencode, bedrock, azure_openai, vertex,
cohere, mistral, perplexity, groq, xai, openrouter, together,
openai_compatible
```

`slug` is the per-owner unique identity: lowercase alphanumeric and hyphens, 1–64 chars, no leading/trailing hyphen. It becomes the `agent-config.json` provider key.

### Workflows (Epic 64)

| Tool | Required args | Description |
|------|---------------|-------------|
| `workflow_list` | _(none)_ | List workflows owned by the authenticated user |
| `workflow_get` | `workflow_id` | Get a workflow definition by ID |
| `workflow_create` | `name`, `spec_yaml` | Create a workflow definition. Optional: `status` (`draft` default, `active`, `archived`). |
| `workflow_update` | `workflow_id` | Partial update — only provided args are sent. Optional: `name`, `status`, `spec_yaml`. |
| `workflow_run` | `workflow_id` | Start a workflow run. Optional: `input` (JSON), `workspace_id` (override target). |
| `workflow_status` | `run_id` | Get the status of a workflow run |
| `workflow_cancel` | `run_id` | Cancel a running workflow |

`spec_yaml` is the workflow DAG spec: a JSON document (`{"nodes":[...],"edges":[...]}`) or YAML text (single document) — the server converts YAML to the canonical JSON spec.

### Triggers (Epic 64)

| Tool | Required args | Description |
|------|---------------|-------------|
| `trigger_list` | _(none)_ | List triggers owned by the authenticated user |
| `trigger_create` | `name`, `source_type`, `source_config` | Create a trigger. Routine mode (no `workflow_id`): `workspace_id` + `prompt`. Workflow mode: `workflow_id`. Optional: `memory_mode`, `capture_mode`, `preserve_session`, `input_from` (`envelope` default \| `body` — webhook only, the posted payload becomes the run input \| `mapped` — the static `input` document), `input` (static run input, workflow mode; a JSON object ≤ 64 KiB; in envelope/body modes a shallow top-level overlay, static wins; in `mapped` mode validated against the workflow's `inputSchema`). Wiring an envelope-mode trigger with no `input` to a workflow whose `inputSchema` requires non-envelope fields is rejected with a 400 — set `input`, use `input_from: "body"`, or relax the schema. |
| `trigger_update` | `trigger_id` | Partial update. Optional: `enabled` (**boolean**), `input_from`, `input` (a JSON `null` clears the static input; omitting the key keeps it). |
| `trigger_delete` | `trigger_id` | Delete a trigger |

`source_type` is `cron` (with `source_config` JSON `{"expr","tz"}`) or `webhook` (with `{}`).

### Task Board (design 0067)

The owner's manager surface (ruling R7 — user-level only): every tool
rides the authenticated `/api/v1/me` board routes, so guard outcomes,
idempotent replay (`request_id` — reuse ONE id per logical operation
across retries), and the read perimeter match the REST/SDK contract
exactly. Worker-only actions (heartbeat, throttle report, claiming) are
deliberately absent — they live on the in-workspace agent registry, and
external actors hold no claim tokens.

| Tool | Required args | Description |
|------|---------------|-------------|
| `board_list_projects` | _(none)_ | List the authenticated user's board projects |
| `board_get` | `item_id` | Get one item by short id — the `prj-`/`epc-`/`tsk-` prefix selects the kind (project, epic with derived progress, task with blocked-by derivation) |
| `board_read` | _(none)_ | Read a board. Default (with `project_id`): the project's tasks; Optional: `stage` (one of `backlog`, `in_progress`, `in_review`, `done`, `pending_input`, `cancelled`). `kind=epics` (with `project_id`): the project's epics with derived progress. `q`: board-wide search (short id exact or title substring; Optional: `project_id`, `epic_id`, `stage`, `tags` — zero-hit near-misses carry did-you-mean suggestions). `health=true`: the health snapshot |
| `board_create_project` | `name`, `target_workspace_id` | Create a board project (one target workspace per project — ruling R8). Optional: `worker_slots` (default 2), `tags`, `request_id` |
| `board_create_epic` | `project_id`, `title` | Create a DRAFT epic. Optional: `outcome` (the steering wheel), `priority` (`P0`–`P3`, default `P2`), `tags`, `request_id` |
| `board_create_task` | `epic_id`, `title` | Create a backlog task; `description` (markdown) seeds the fresh worker's charter. Optional: `priority`, `tags` (may carry one `pr:N` anchor — the only PR-drift form the reconciler consumes; malformed anchors are inert), `acceptance_criteria` (`[{text, met}]` — Done = ALL met + evidence), `request_id` |
| `board_update_item` | `item_id` | Patch an item; only fields applying to the addressed kind are accepted (a field for another kind is an error, not a silent drop). Omitted scalar fields keep existing values. **Tags differ by kind** (engine rules, REST-identical): on a task, omitted `tags` keep the set and an explicit `[]` clears it; on a project/epic, tags are written unconditionally — omitting them clears (send the full set to keep). Project fields: `name`, `target_workspace_id`, `worker_slots`, `tags`. Epic: `title`, `outcome`, `priority`, `tags`. Task: `title`, `description`, `priority`, `tags`, `acceptance_criteria`, `set_criteria_met` (0-based index check-off). Optional: `request_id` |
| `board_move_stage` | `task_id`, `to_stage` | Server-arbitrated stage move. Legal manager targets: `in_progress`, `in_review`, `done`, `pending_input`, and `backlog` (the pending_input human exits); entering from backlog is claim-only (`board_start_task`) and `cancelled` is `board_cancel`'s terminal — neither is a stage move. `done` requires all acceptance criteria met + `evidence_url`. Optional: `evidence_url`, `request_id` |
| `board_start_task` | `task_id` | Manual one-off dispatch (claim + fresh-charter worker spawn); allowed in draft epics, refused for paused/completed/archived epics and archived projects. Optional: `request_id` |
| `board_epic_activate` | `epic_id` | Activate an epic (start feeding work); re-opens a completed epic. Optional: `request_id` |
| `board_epic_pause` | `epic_id` | Pause an epic (stop new claims; in-flight leases run on). Optional: `request_id` |
| `board_epic_archive` | `epic_id` | Archive an epic (reversible storage). Optional: `request_id` |
| `board_epic_unarchive` | `epic_id` | Unarchive an epic (always restores active). Optional: `request_id` |
| `board_project_archive` | `project_id` | Archive a project (stops all claims into it). Optional: `request_id` |
| `board_project_unarchive` | `project_id` | Unarchive a project (restores active). Optional: `request_id` |
| `board_cancel` | `item_id` | Terminal cancel of an epic or task (`epc-`/`tsk-`): revokes lease + token, aborts the worker, cascades per design §4.2. Optional: `reason`, `request_id` |
| `board_add_dependency` | `task_id`, `depends_on` | Add a structural dependency edge (within a project; cycles rejected with did-you-mean suggestions). Optional: `request_id` |
| `board_remove_dependency` | `task_id`, `depends_on` | Remove a dependency edge. Optional: `request_id` |
| `board_health` | _(none)_ | The board health snapshot (design 0067 §4.8): active leases with expiry, recent reclaims, task and project-slot cooldowns, pending_input counts by origin, per-project worker slots in use — the same projection `board_read?health=true` renders |

All lifecycle tools (`board_epic_activate`/`_pause`/`_archive`/`_unarchive`,
`board_project_archive`/`_unarchive`) accept an Optional `request_id`
idempotency key — one id per logical operation, reused on retry (a
replay renders 200-with-original, never an error).

## How MCP maps to workspace sessions

The MCP server is a stateless adapter over the REST API. There is no MCP-side session state — every tool call translates to one or more HTTP requests against the API:

```
MCP client ──► MCP server (stdio/SSE) ──► LLMSafeSpaces API ──► workspace pod (opencode serve :4096)
```

- A **workspace** is the persistent unit (PVC + pod). Create it once with `workspace_create`, activate it with `workspace_activate`.
- A **session** is a conversation inside a workspace. Create one with `session_create`; it returns a `session_id` you pass to `session_message`.
- The agent's session history lives in the PVC at `/workspace/.local/opencode`, so it survives suspend/resume cycles. `session_history` reads it back.

This means an MCP-driven workflow looks identical to the REST workflow in the [Quickstart](../getting-started/quickstart.md):

1. `workspace_create` → `workspace_activate` → wait for Active
2. `credential_create` (with `workspace_id` to auto-bind) or bind via REST
3. `session_create` → `session_message`

The MCP server deliberately does not expose the full REST surface. For operations it doesn't cover (secrets CRUD, org management, settings, terminal), use the [SDKs](sdks.md) or call the [REST API](rest.md) directly.

## Resources and prompts

The MCP server advertises **tool capabilities** only (`server.WithToolCapabilities(true)`). It does not currently expose MCP `resources` or `prompts` primitives — workspace data is accessed by calling tools, and the system prompt is managed via the [prompt endpoints](rest.md#platform-admin) on the REST API.

## End-to-end example

A typical MCP-driven session, expressed as a sequence of tool calls:

```
workspace_create(runtime="python:3.10", name="demo")
  → { "id": "ws_abc123", ... }

workspace_activate(workspace_id="ws_abc123")
  → { "phase": "Resuming", ... }
  (poll status until Active — see REST GET /workspaces/:id/status)

credential_create(
  kind="anthropic",
  slug="my-anthropic",
  api_key="sk-ant-...",
  workspace_id="ws_abc123"
)
  → { "id": "cred_...", ... }

session_create(workspace_id="ws_abc123")
  → { "sessionId": "ses_xyz", ... }

session_message(
  workspace_id="ws_abc123",
  session_id="ses_xyz",
  message="Write a Python function that returns the nth Fibonacci number."
)
  → "Here's a Python function ..."

workspace_stop(workspace_id="ws_abc123")
  → "Workspace ws_abc123 stopped (files preserved)"
```

## Next

- [REST API](rest.md) — the full endpoint surface the MCP server adapts
- [SDKs](sdks.md) — typed clients for Go, TypeScript, Python, Java
- [Quickstart](../getting-started/quickstart.md) — the underlying REST workflow
