# LLMSafeSpaces SDKs

Multi-language client SDKs for the LLMSafeSpaces API.

## Packages

| Language | Package | Install |
|----------|---------|---------|
| Python | `llmsafespaces` | `pip install llmsafespaces` |
| TypeScript | `@llmsafespaces/sdk` | `npm install @llmsafespaces/sdk` |
| Go | `github.com/lenaxia/llmsafespaces/sdk/go` | `go get github.com/lenaxia/llmsafespaces/sdk/go` |

The OpenAPI spec at `sdks/openapi.yaml` is the canonical API contract. Route
parity between the spec and the production router is enforced by
`TestOpenAPIRouterContract` (api/internal/server) — spec and router are
diffed in both directions with every handler wired.

## Quick start (Python)

```python
from llmsafespaces import LLMSafeSpaces

client = LLMSafeSpaces("https://llmsafespaces.example.com", api_key="lsp_...")
ws = client.workspaces.create(name="my-project", runtime="base", storage_size="5Gi")
session = client.sessions.ensure(ws.id)
response = client.sessions.send_message(ws.id, session.session_id, "Hello!")
print(response.content)
```

## Quick start (TypeScript)

```typescript
import { LLMSafeSpaces } from "@llmsafespaces/sdk";

const client = new LLMSafeSpaces({ baseUrl: "https://llmsafespaces.example.com", apiKey: "lsp_..." });
const ws = await client.workspaces.create({ name: "my-project", runtime: "base", storageSize: "5Gi" });
const session = await client.sessions.ensure(ws.id);
const response = await client.sessions.sendMessage(ws.id, session.sessionId, "Hello!");
console.log(response.content);
```

## Structure

```
sdks/
├── openapi.yaml          # Canonical OpenAPI 3.0.3 spec
├── go/                   # Go SDK (reference implementation)
├── python/               # Python SDK (sync + async)
├── typescript/           # TypeScript SDK
├── java/                 # Java SDK (typed facade)
├── tests/contract/       # Hurl contract tests
└── validate/             # Spec structural validator (route parity lives in api/internal/server)
```

## Versioning

The spec and SDKs version INDEPENDENTLY of the platform (semver over the API
surface: additive changes bump the minor, breaking changes the major). The
platform release version (e.g. `v0.21.3`) tracks the deployable artifacts,
not the SDK surface. When the spec minor bumps, Python and TypeScript SDKs
are republished from the same change; Go modules resolve from VCS tags.
