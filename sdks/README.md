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
3. Hand-sync the four SDKs to the spec (see "Keeping the SDKs in
   sync" below — the generation targets are no-ops)

### Validation

```bash
make validate
```

This runs a Go-based structural validator that checks:
- Valid OpenAPI 3.0.3 structure
- All `$ref` targets resolve
- Security schemes defined
- At least one path defined

### Session and input surfaces (platform contract)

The session and input-request surfaces are typed against the
platform-owned session contract (`pkg/session`, design 0049) — they are
NOT raw passthroughs of the upstream agent. Response shapes
(`Session`, `Message`, `Part`, `InputRequest`) are the contract's
camelCase JSON; agent-specific identifiers and shapes never appear.
Adding a field is a spec change against `pkg/session` first, then here.

### SSE endpoint

`GET /workspaces/{id}/session-events` is a Server-Sent Events stream. OpenAPI cannot fully model SSE. This endpoint is documented for reference but is **not usable by generated SDK clients**. Use language-specific SSE libraries instead:

- **Browser**: `EventSource` API
- **Python**: `httpx-sse`
- **Go**: manual HTTP streaming
- **TypeScript/Node**: `eventsource` package

## Keeping the SDKs in sync

The four SDKs are hand-written against `openapi.yaml` (the generation
targets from US-14.3–14.6 never shipped; the placeholder targets remain
only as no-ops). Sync is enforced by tests, not codegen:

```bash
# Spec validity + spec↔router parity contract (Epic 68 E9 core)
make sdk-check

# Validate the spec structurally alone
make validate
```

- Route parity: `TestOpenAPIRouterContract` (api/internal/server) diffs the
  spec and the production router in both directions — CI-blocking.
- Per-language compile + wire-level tests: the `sdk-contract` CI job
  (`.github/workflows/ci.yml`) builds and tests Go/TypeScript/Python/Java
  on every PR.
- Live-API canaries: `sdks/canary/` (`make -C sdks/canary canary-ci`).

## Design Decisions

1. **OpenAPI 3.0.3** (not 3.1) — chosen for maximum cross-generator compatibility
2. **Hand-written spec** — no swag annotations exist in the codebase; `swag init` produces empty output
3. **REST-only in v1** — SSE/WebSocket streaming not modeled in SDK types (use native libraries)
4. **Contract-typed session/input responses** — session, history, and input surfaces are the platform-owned `pkg/session` contract (design 0049); no response schema tracks the upstream agent. Agent-specific shapes are contained behind the adapter seam.

## Versioning and breaking changes

The spec and SDKs version independently of the platform (semver over the
API surface — see [PACKAGES.md](PACKAGES.md): additive changes bump the
minor, breaking changes the major).

### 1.0.0 (from 0.7.0)

Breaking, covering the input-surface retype (epic-71 4b, #1302 cleanup)
and the session-surface truth-up (#1304):

- `getSession` now documents the contract `Session` (pkg/session) — it
  previously documented a raw passthrough object.
- `deleteSession` and `abortSession` are bodyless `204` (previously
  documented as `200`).
- `sendPromptAsync` documents its real bodies: `202` carries the
  accepted-entry receipt (`{messageID, clientMessageID, status}`), and a
  retried `clientMessageID` answers `200` with the original entry
  (`status: "duplicate"`). SDK `sendPromptAsync`/`sendPrompt` methods
  return the receipt instead of nothing (Go/TS/Python/Java signatures
  changed).
- `sendMessage`'s request body extracts text from `parts`; `content` is
  accepted but ignored (previously documented as the text carrier).
- `getHistory` pages are oldest-first (chronological) within a page —
  the previously documented "newest-first ordering within a page" was
  wrong.
- `Part.type` discriminator for file-change parts is `file_change` on
  the wire (pkg/session contract) — the spec enum and the TS/Java SDK
  types previously said `file-change`, which the server never emits
  (caught by the new live-router conformance test,
  `api/internal/server/router_session_contract_test.go`).
- Question/permission surfaces speak the contract `InputRequest`
  vocabulary (#1302): typed lists, contract reply bodies, bodyless 200s
  on live replies/rejects, `202` late-answer bodies
  (`InboxLateAnswerAccepted`), the inbox dismiss exit (204), and
  `input-snapshot` (202). SDK reply signatures changed accordingly.

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
track `queue.update` events. Values: `enqueued` = message admitted to the durable
queue; `delivering` = the delivery worker picked it up (POST in flight — clear
any "queued" UI state here; the send is synchronous turn-to-completion, so `sent`
can lag by minutes); `sent` = delivery confirmed (authoritative cleanup);
`error` = delivery failed (retryable, entry carries the failure); `dismissed` =
entry removed by the user.

`listQueue` and `dismissQueued` are deprecated and will be removed in the
next major SDK version.
