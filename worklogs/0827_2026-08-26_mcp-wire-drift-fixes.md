# Worklog: MCP wire-drift fixes (#1033–#1038) + router integration gate

**Date:** 2026-08-26
**Session:** Fix the four P0 MCP wire bugs (session_create 404, session_message dead SSE/prompt legs, trigger_update string/bool mismatch, workflow_update partial-update 400s), land the MCP-client↔production-router integration gate (#1037), and refresh docs/api/mcp.md (#1038) with a doc-parity guard.
**Status:** Complete

---

## Objective

Close the P0 block of epic #1032: make `session_create`, `session_message`, `trigger_update`, and `workflow_update` actually work in production, and install the test gate that would have caught all four.

---

## Work Completed

### Verification (all four P0s reproduced at HEAD `128a429b` before fixing)

- #1033: `pkg/mcp/client.go:341` posted `/sessions` (router serves `/sessions/new`, `router.go:1374`); `SessionResp` decoded `id` but `EnsureSessionResponse` serializes `sessionId` (`pkg/types/session.go:83-88`).
- #1034: `client.go:379` subscribed `GET /workspaces/:id/events` — removed by Epic 28 (commit `75996864`: same handler/envelope renamed to `/session-events`). **Plus a fifth bug in the same flow, not in the original filing:** the prompt leg sent `{"message": ...}` but `extractPromptText` (`proxy_handlers.go:403`) only parses `{parts:[{type:"text"}]}` — so `session_message` 400'd ("text must not be empty") on EVERY call before the SSE leg even ran. Both legs fixed.
- #1035: `workflow_tools.go:84` declared `enabled` as string; client sent `map[string]string` vs `UpdateTriggerRequest.Enabled *bool` (`pkg/types/workflows.go:389`). Every call 400'd (`json: cannot unmarshal string ... of type bool`).
- #1036: `client.go:631` always sent name/status/specYaml with empty strings; `ValidWorkflowName("")` fails → every partial update 400'd ("invalid workflow name").
- #1037: `pkg/mcp` unit tests mock HTTP at whatever path the client requests; nothing drove `HTTPClient` against the real router.
- #1038: docs/api/mcp.md documented 15 tools (3 removed ones still listed) vs 24 registered.

### A0 — the gate (#1037)

New `api/internal/server/mcp_router_integration_test.go`: production `NewRouter` + mocked services (WorkspaceService testify mock, in-memory workflow/trigger stores capturing the bound `pkg/types` structs, stub opencode pod with real `opencode.Adapter`) + `httptest.Server`; `mcp.HTTPClient` pointed at it. Tool-level tests drive the full chain (MCP tool schema → handler → client → router → REST handler → store) via the mcp-go in-process client. 12 tests: 6 red-first (the four bugs + omitted-arg semantics), 6 green pins (workspace lifecycle, workflow/trigger CRUD sweep, question/permission route resolution, history route). Scope boundary vs #1043 documented in the file header (secrets/models/credentials handlers need service fakes that don't exist yet).

Supporting seam: `ProxyHandler.SetUserBrokerForTest` (`proxy.go`, mirrors `SetOutboxForTest`) so the fixture can wire the SSE broker without running `Start()`.

### A1–A4 — the fixes (`pkg/mcp/client.go`, `workflow_tools.go`)

- `CreateSession` → `POST /sessions/new`, decodes `sessionId` (#1033).
- `SendMessage` → prompt body `{parts:[{type:"text",text}]}`; SSE → `/session-events` (#1034).
- `trigger_update`: schema `WithBoolean`, handler `*bool` pass-through, client sends the key only when supplied (#1035).
- `workflow_update`: `UpdateWorkflow(ctx, id, name, status, specYAML *string)`; client omits absent keys; handler forwards only provided args (#1036).
- APIClient interface + MockAPIClient signatures updated; `pkg/mcp` unit-test mocks repinned to the production paths (they had frozen the drift in place).

### History decode (same #1034 cluster)

Client `Message` decoded `{role, content}` — never matched the contract-shaped API response (`{id, type, parts}`, design 0049), so `session_history` and the SendMessage fallback always returned empty fields. Fixed to contract shape with `TextContent()` extraction.

### #1038 — docs + parity guard

`docs/api/mcp.md` tool tables regenerated against the 24-tool registry: removed `session_question_reply/reject` + `session_permission_reply`, documented `run_resolve`, the 11 Epic 64 workflow/trigger tools, `session_message`'s SSE behavior, and boolean `enabled`. New `TestDocs_MCPTools_Parity` (`pkg/repolint/canary_mcp_tools_parity_test.go`) diffs the doc's table rows against the live registry in both directions.

---

## Key Decisions

- **Fix the prompt-body shape as part of #1034** rather than only the SSE path: the POST leg runs first, so the SSE fix alone left `session_message` dead. Same cluster, same tool, same test.
- **Contract-shaped history decode**: the `{role, content}` decode was dead code against the live API; fixing it is required for the idle-termination behavior the #1034 test pins.
- **Route-resolution-level assertions for question/permission replies**: they are opaque pod pass-throughs (no API-side binding) on the legacy fixed-port path, unreachable from the fixture; asserting not-404/405/400/401 is the meaningful wiring proof. Full-handler wiring for every route remains #1043.
- **Breaking change to the `enabled` tool schema is safe**: the string form 400'd on every call — no working caller can regress.

## Assumptions (stated + validated)

1. `/session-events` emits identical envelopes to the removed `/events` — validated via Epic 28 commit `75996864` (same handler + broker, path-only rename) and proven behaviorally by the question-event test.
2. `sessionId` is the correct decode field — validated against `pkg/types/session.go:83-88` and the passing e2e decode assertion.
3. The stub-pod adapter port override is fixture-only — validated against `e2e_adapter_test.go`'s identical `WithAdapterPort` pattern.

## Blockers

None.

## Tests Run

- `go test -timeout 300s -race ./pkg/mcp/` — ok
- `go test -timeout 600s -race ./api/internal/server/` — ok (includes new gate + existing router suites)
- `go test -timeout 600s -race ./api/internal/handlers/ ./pkg/repolint/` — ok
- `go build ./...` — ok; `gofmt -l` clean
- Gate red-phase confirmed before fixes: all six target tests failed with the exact production errors (`API error 400: cannot unmarshal string ... enabled of type bool`, `400 invalid workflow name`, 404 on `/sessions`)

## Next Steps

- PR this branch; closes #1033, #1034, #1035, #1036, #1037, #1038.
- File follow-up issue: SSE live-content accumulation in `SendMessage` reads `event.Content`, a field `WorkspaceSSEEvent` never carries (content is inside `data`) — pre-existing; final text arrives via history fallback today; extracting text parts from `session.event` data would stream content.
- #1043 (contract-test fixture wires ALL handlers) extends this fixture pattern to secrets/models/credentials surfaces.

## Files Modified

- `pkg/mcp/client.go` — A1–A4 + history contract decode
- `pkg/mcp/workflow_tools.go` — trigger_update bool schema, workflow_update partial handler
- `pkg/mcp/client_test.go`, `pkg/mcp/integration_test.go`, `pkg/mcp/server_test.go` — repinned mocks/fixtures
- `api/internal/handlers/proxy.go` — SetUserBrokerForTest seam
- `api/internal/server/mcp_router_integration_test.go` — new gate (A0)
- `pkg/repolint/canary_mcp_tools_parity_test.go` — doc-parity guard
- `docs/api/mcp.md` — 24-tool registry sync
