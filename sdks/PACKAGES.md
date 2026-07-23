# LLMSafeSpaces SDKs

Multi-language client SDKs for the LLMSafeSpaces API.

## Packages

| Language | Package | Install |
|----------|---------|---------|
| Python | `llmsafespaces` | `pip install llmsafespaces` |
| TypeScript | `@llmsafespaces/sdk` | `npm install @llmsafespaces/sdk` |
| Go | `github.com/lenaxia/llmsafespaces/sdk/go` | `go get github.com/lenaxia/llmsafespaces/sdk/go` |

SDK versions track the platform version. The OpenAPI spec at `sdks/openapi.yaml`
is the canonical API contract.

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
├── openapi.yaml          # Canonical OpenAPI 3.0.3 spec (84 paths)
├── go/                   # Go SDK (reference implementation)
├── python/               # Python SDK (sync + async)
├── typescript/           # TypeScript SDK
├── java/                 # Java SDK (typed facade)
├── tests/contract/       # Hurl contract tests
└── validate/             # Spec validator + route-coverage CI check
```

## Versioning

SDK versions match the platform version. When a platform tag (e.g., `v0.4.5`)
is pushed, the release workflow publishes Python and TypeScript SDKs at the
same version. Go modules resolve directly from VCS tags.
