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

The server advertises 15 tools, all workspace-centric — the sandbox layer is hidden from callers. Each tool maps to one or more REST API calls.

### Workspace lifecycle

| Tool | Required args | Description |
|------|---------------|-------------|
| `workspace_create` | `runtime`, `name`? | Create a new workspace with a persistent development environment |
| `workspace_activate` | `workspace_id` | Activate a workspace (starts the agent). Required before creating sessions. |
| `workspace_stop` | `workspace_id` | Stop a workspace (suspends the agent, preserves all files) |
| `workspace_refresh_compute` | `workspace_id` | Re-sync resource defaults + latest image version and rebuild the pod |

`runtime` is a string like `python:3.10`, `nodejs:18`, or `go:1.21`.

### Sessions

| Tool | Required args | Description |
|------|---------------|-------------|
| `session_create` | `workspace_id` | Create a conversation session in an active workspace |
| `session_message` | `workspace_id`, `session_id`, `message` | Send a message and get the response (blocks until the agent replies) |
| `session_history` | `workspace_id`, `session_id` | Get the message history of a session |

!!! warning "`session_message` blocks"
    Like the REST `POST .../message`, this tool waits for the full assistant response and can take 30–120+ seconds. The default tool timeout is 300s. Messages are capped at 1 MiB.

### Agent input: questions & permissions

The agent can pause and ask the caller a question or request permission for an action. These tools reply to those requests.

| Tool | Required args | Description |
|------|---------------|-------------|
| `session_question_reply` | `workspace_id`, `request_id`, `answers` | Reply to a question. `answers` is a JSON array of string arrays, e.g. `[["answer1"],["answer2"]]`. |
| `session_question_reject` | `workspace_id`, `request_id` | Reject a question (aborts the current operation) |
| `session_permission_reply` | `workspace_id`, `request_id`, `reply` | Reply to a permission request. `reply` is `once`, `always`, or `reject`. Optional `message`. |

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
