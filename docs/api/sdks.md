# Client SDKs

LLMSafeSpaces ships typed client SDKs in four languages, plus a VS Code extension. They live under [`sdks/`](https://github.com/lenaxia/LLMSafeSpaces/tree/main/sdks) and are thin, ergonomic wrappers over the [REST API](rest.md).

## Available SDKs

| SDK | Location | Package | Runtime | Auth |
|-----|----------|---------|---------|------|
| **Go** | `sdks/go/` | `github.com/lenaxia/llmsafespaces` (local module) | Go 1.21+ | API key or email/password |
| **TypeScript** | `sdks/typescript/` | `@llmsafespaces/sdk` | Node.js 18+ / browser | API key or email/password |
| **Python** | `sdks/python/` | `llmsafespaces` | Python 3.10+ | API key or email/password |
| **Java** | `sdks/java/` | `com.llmsafespaces:sdk` | Java 17+ | API key |
| **VS Code** | `sdks/vscode-llmsafespaces/` | extension | VS Code 1.95+ | API key (stored in OS keychain) |

## How they're generated

All four language SDKs are kept in sync against a single hand-written **OpenAPI 3.0.3 specification** at [`sdks/openapi.yaml`](https://github.com/lenaxia/LLMSafeSpaces/tree/main/sdks/openapi.yaml). The spec is the source of truth for the REST contract, derived from:

- `api/internal/server/router.go` — route definitions
- `pkg/types/types.go` — request/response types
- `api/internal/handlers/` — handler implementations

```bash
cd sdks

# Validate the spec structurally (Go-based validator, no npm required)
make validate

# Regenerate all SDKs
make generate-all

# Or one at a time
make generate-ts
make generate-python
make generate-go
make generate-java
```

The validator checks OpenAPI 3.0.3 structure, resolves all `$ref` targets, and confirms security schemes and at least one path are defined.

### What's not modeled

- **SSE** (`/events`, `/session-events`) — OpenAPI cannot fully model Server-Sent Events. Use language-native SSE libraries instead (`EventSource`, `httpx-sse`, manual HTTP streaming in Go).
- **WebSocket** (terminal) — likewise; use a WebSocket client.
- **Proxy responses** — endpoints marked `x-opencode-proxy: true` return responses shaped by the upstream `opencode` agent. Their schemas are loosely typed because the agent's response format may drift between versions.

## When to use an SDK vs raw HTTP

Use an SDK when:

- You want typed request/response structs and language-idiomatic error handling.
- You're building automation, a CI integration, or a longer-lived tool.
- You want JWT auto-refresh on 401 (TypeScript and Python SDKs do this).

Use raw HTTP when:

- You're prototyping with `curl`/`jq`.
- You need an endpoint the SDK doesn't cover (SSE, terminal, org management).
- You're writing a one-off script and don't want a dependency.

!!! tip "API keys are the right auth for SDKs"
    All SDKs accept an API key directly. JWT (email/password) auth is supported by the Go, TypeScript, and Python SDKs but requires a login round-trip and token refresh logic. For unattended use, generate an API key once and pass it in.

---

## Go

The Go SDK uses the standard library only — no external HTTP dependency. It follows the `client.Service.Method` pattern familiar from other Go API clients.

### Install

The SDK is a local module under `sdks/go/`. Use a `replace` directive in your `go.mod` to point at it, or vendor it:

```go
// go.mod
require github.com/lenaxia/llmsafespaces v0.0.0

replace github.com/lenaxia/llmsafespaces => ./path/to/sdks/go
```

### Usage

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/lenaxia/llmsafespaces"
)

func main() {
    ctx := context.Background()

    client := llmsafespaces.New("https://llmsafespaces.example.com",
        llmsafespaces.WithAPIKey("lsp_your_api_key"),
    )

    // Create + activate a workspace
    ws, err := client.Workspaces.Create(ctx, llmsafespaces.CreateWorkspaceRequest{
        Name:        "my-project",
        Runtime:     "base",
        StorageSize: "10Gi",
    })
    if err != nil {
        log.Fatal(err)
    }

    if _, err := client.Workspaces.Activate(ctx, ws.ID); err != nil {
        log.Fatal(err)
    }

    // Start a session and send a message
    sess, err := client.Sessions.Ensure(ctx, ws.ID)
    if err != nil {
        log.Fatal(err)
    }

    resp, err := client.Sessions.SendMessage(ctx, ws.ID, sess.SessionID,
        "Reply with exactly the word: PONG")
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println(resp)
}
```

### Service surface

The client exposes typed services: `Workspaces`, `Sessions`, `Auth`, `Secrets`, `Terminal`, `UserSettings`, `Account`, `ProviderCredentials`, `AdminProviderCredentials`, `Prompts`, `AgentRoles`. Each method takes a `context.Context` and returns `(result, error)`; errors are `*APIError` with `Status` and `Message` fields.

Auth options: `WithAPIKey("lsp_…")` for API keys, `WithCredentials(email, password)` for JWT (auto-login). `WithHTTPClient` and `WithTimeout` are available for tuning.

---

## TypeScript

The TypeScript SDK has **zero runtime dependencies** — it uses the native `fetch` available in Node.js 18+ and modern browsers.

### Install

```bash
npm install @llmsafespaces/sdk
```

### Usage

```typescript
import { LLMSafeSpaces, TimeoutError } from '@llmsafespaces/sdk';

const client = new LLMSafeSpaces({
  baseUrl: 'https://llmsafespaces.example.com',
  apiKey: 'lsp_your_api_key',
});

// Create a workspace and send a message
const workspace = await client.workspaces.create({
  name: 'my-project',
  runtime: 'python:3.11',
  storageSize: '10Gi',
});
const session = await client.sessions.ensure(workspace.id);
const response = await client.sessions.sendMessage(
  workspace.id,
  session.sessionId,
  'Write hello world in Python',
);
console.log(response.content);
```

### Error handling

The SDK throws typed errors:

```typescript
import {
  NotFoundError,
  AuthError,
  ConflictError,
  TimeoutError,
  LLMSafeSpacesError,
} from '@llmsafespaces/sdk';

try {
  await client.workspaces.get('nonexistent');
} catch (e) {
  if (e instanceof NotFoundError) { /* 404 */ }
  if (e instanceof AuthError) { /* 401/403 */ }
  if (e instanceof ConflictError) { /* 409 — e.g. workspace not active */ }
  if (e instanceof TimeoutError) {
    // sendMessage timed out — the prompt may still be processing.
    // Poll getHistory to check.
  }
  if (e instanceof LLMSafeSpacesError) { /* any API error: e.status, e.message */ }
}
```

### `sendMessage` timeout

`sendMessage` proxies to the LLM agent and blocks until it responds — 30–120+ seconds is normal. The default timeout is 120s. On timeout, a `TimeoutError` is thrown, but the prompt may still be processing. Poll `client.sessions.getHistory(...)` to check.

---

## Python

The Python SDK is built on [`httpx`](https://www.python-httpx.org/) and ships both **synchronous** and **asynchronous** clients.

### Install

```bash
pip install llmsafespaces
```

### Usage (sync)

```python
from llmsafespaces import LLMSafeSpaces, NotFoundError

with LLMSafeSpaces(
    base_url="https://llmsafespaces.example.com",
    api_key="lsp_your_api_key",
) as client:
    workspace = client.workspaces.create(
        name="my-project",
        runtime="python:3.11",
        storage_size="10Gi",
    )
    session = client.sessions.ensure(workspace["id"])
    response = client.sessions.send_message(
        workspace["id"],
        session["sessionId"],
        "Write hello world in Python",
    )
    print(response)
```

### Usage (async)

```python
import asyncio
from llmsafespaces import AsyncLLMSafeSpaces

async def main():
    async with AsyncLLMSafeSpaces(
        base_url="https://llmsafespaces.example.com",
        api_key="lsp_your_api_key",
    ) as client:
        workspace = await client.workspaces.create(
            name="my-project", runtime="python:3.11", storage_size="10Gi",
        )
        session = await client.sessions.ensure(workspace["id"])
        response = await client.sessions.send_message(
            workspace["id"], session["sessionId"], "Hello",
        )
        print(response)

asyncio.run(main())
```

The sync client is a context manager (`with LLMSafeSpaces(...) as client:`) that closes the underlying `httpx.Client` on exit. Error classes mirror the TypeScript SDK: `LLMSafeSpacesError`, `AuthError`, `NotFoundError`, `ConflictError`, `TimeoutError`, `RateLimitError`.

---

## Java

The Java SDK targets Java 17+ and uses the built-in `java.net.http.HttpClient` with [Gson](https://github.com/google/gson) for JSON. It follows a builder pattern.

### Install

Maven:

```xml
<dependency>
  <groupId>com.llmsafespaces</groupId>
  <artifactId>sdk</artifactId>
  <version>1.0.0</version>
</dependency>
```

Gradle:

```groovy
implementation 'com.llmsafespaces:sdk:1.0.0'
```

### Usage

```java
import com.llmsafespaces.sdk.LLMSafeSpacesClient;
import com.llmsafespaces.sdk.LLMSafeSpacesException;
import com.google.gson.JsonObject;

LLMSafeSpacesClient client = LLMSafeSpacesClient.builder("https://llmsafespaces.example.com")
    .apiKey("lsp_your_api_key")
    .timeout(Duration.ofSeconds(120))
    .build();

// Create a workspace
JsonObject body = new JsonObject();
body.addProperty("name", "my-project");
body.addProperty("runtime", "base");
JsonObject workspace = client.post("/workspaces", body, JsonObject.class);

String workspaceId = workspace.get("id").getAsString();

// Activate (no response body)
client.post("/workspaces/" + workspaceId + "/activate", null);
```

The Java SDK exposes generic `get`, `post`, and `delete` methods plus a static `extractTextContent(JsonObject)` helper for pulling text out of opencode response parts. Errors throw `LLMSafeSpacesException` with a status code and message.

---

## VS Code extension

The VS Code extension (`sdks/vscode-llmsafespaces/`) wraps the API into the editor: a workspace sidebar, a WebSocket terminal, and a Copilot Chat participant.

### Features

- **Workspace sidebar** — browse and manage sandboxes from the activity bar, with color-coded status (active / suspended / terminated).
- **Terminal access** — open a secure terminal connected to any active workspace. Traffic flows through the API's WebSocket proxy — no SSH keys or port-forwarding needed.
- **Chat participant** — type `@llmsafespaces` in Copilot Chat to route prompts directly to your sandbox agent. The agent has full access to the workspace filesystem and tools.
- **Commands** — Create Workspace, Suspend/Resume, Activate, Open Terminal, Configure.

### Getting started

1. Install the extension.
2. Open the Command Palette and run `LLMSafeSpaces: Configure`.
3. Enter your API URL and API key (`lsp_...`).
4. Your workspaces appear in the sidebar.

API keys are stored in VS Code's `SecretStorage` (the OS keychain). The extension communicates only with the LLMSafeSpaces instance you configure — no telemetry, no third-party data.

---

## Contract testing

The SDKs are validated against the spec and the live API with [Hurl](https://hurl.dev/) contract tests under `sdks/tests/contract/`.

```bash
# Against a running API instance
make contract-test BASE_URL=http://localhost:8080 TOKEN=lsp_xxx

# Against a Prism mock spun up from the spec (no live API needed)
make contract-test-mock
```

`contract-test-mock` uses [`@stoplight/prism-cli`](https://github.com/stoplightio/prism) to serve a mock from `openapi.yaml` on `:4010`, then runs every Hurl file against it. This validates that the SDKs' expected requests match the spec without requiring a live deployment.

## Next

- [REST API](rest.md) — the full endpoint reference
- [Authentication](authentication.md) — JWT and API key mechanics
- [MCP server](mcp.md) — drive workspaces from any MCP-compatible client
