# LLMSafeSpaces SDKs

This directory contains the canonical OpenAPI specification and generated SDK clients for the LLMSafeSpaces API.

## Structure

```
sdks/
├── openapi.yaml          # Canonical OpenAPI 3.0.3 specification (hand-written)
├── Makefile              # Validation and generation targets
├── validate/             # Go-based spec validator
│   ├── main.go
│   └── main_test.go
├── typescript/           # Generated TypeScript SDK (US-14.3)
├── python/              # Generated Python SDK (US-14.4)
├── go/                  # Generated Go SDK (US-14.5)
└── java/                # Generated Java SDK (US-14.6)
```

## OpenAPI Specification

The spec at `openapi.yaml` is the **single source of truth** for the LLMSafeSpaces REST API contract. It is hand-written from:

- `api/internal/server/router.go` — route definitions
- `pkg/types/types.go` — request/response types
- `api/internal/handlers/` — handler implementations

### Updating the spec

When API routes or types change:

1. Update `openapi.yaml` to match the new behavior
2. Run `make validate` to ensure structural correctness
3. Regenerate SDKs with `make generate-all`

### Validation

```bash
make validate
```

This runs a Go-based structural validator that checks:
- Valid OpenAPI 3.0.3 structure
- All `$ref` targets resolve
- Security schemes defined
- At least one path defined

### Proxy endpoints

Endpoints marked with `x-opencode-proxy: true` return responses from the upstream opencode agent. Their response schemas are version-coupled to the opencode version running in sandboxes. These schemas may drift if opencode updates its API format.

### SSE endpoint

`GET /workspaces/{id}/events` is a Server-Sent Events stream. OpenAPI cannot fully model SSE. This endpoint is documented for reference but is **not usable by generated SDK clients**. Use language-specific SSE libraries instead:

- **Browser**: `EventSource` API
- **Python**: `httpx-sse`
- **Go**: manual HTTP streaming
- **TypeScript/Node**: `eventsource` package

## SDK Generation (Future — US-14.3 through US-14.6)

```bash
# Generate all SDKs
make generate-all

# Generate individual SDKs
make generate-ts
make generate-python
make generate-go
make generate-java
```

## Design Decisions

1. **OpenAPI 3.0.3** (not 3.1) — chosen for maximum cross-generator compatibility
2. **Hand-written spec** — no swag annotations exist in the codebase; `swag init` produces empty output
3. **REST-only in v1** — SSE/WebSocket streaming not modeled in SDK types (use native libraries)
4. **Proxy responses loosely typed** — opencode response format may change; marked with `x-opencode-proxy`

## Session Queue (Epic 63)

The session message queue has two modes:

- **Legacy (V1)**: external Redis-backed queue. `listQueue`, `dismissQueued`,
  and `QueuedMessage.RetryCount` are fully functional.
- **V2 (inboard)**: the queue lives inside opencode's durable SQLite.
  `enqueue` works identically. `listQueue` returns a best-effort shadow
  derived from SSE events (may be incomplete). `dismissQueued` removes from
  the shadow only — it does NOT revoke the durable input (abort is
  non-destructive). `RetryCount` is vestigial.

**For SDK consumers who need reliable queue visibility under V2:**
subscribe to the workspace SSE stream (`GET /workspaces/{id}/session-events`) and
track `queue.update` events (`enqueued` = message admitted, `sent` =
message promoted/running). This is the authoritative source.

`listQueue` and `dismissQueued` are deprecated and will be removed in the
next major SDK version.
