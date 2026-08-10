# Worklog: US-65.4 proxy handler migration (batch 1 - internal paths)

**Date:** 2026-08-10
**Session:** US-65.4 incremental migration of proxy handlers to the Adapter seam. First batch: internal-facing paths only.
**Status:** Complete (batch 1 of multi-batch effort)

---

## Objective

Migrate proxy handlers from the legacy dialect + proxyToWorkspace path to the Adapter (US-65.3). This batch targets **internal-facing** paths only: background goroutines, SSE event enrichment, and session-index persistence. Client-facing proxy handlers (CreateSession, SendMessage, GetHistory, etc.) are deferred to a later batch because they change the response wire shape, requiring frontend coordination.

---

## Work Completed

### Infrastructure (PR #716, merged)

- `ProxyHandler.adapter agent.Adapter` field + `SetAdapter` with post-Start guard.
- `AdapterPasswordResolver` + `WorkspacePodIPResolver` bridges in `proxy_connections.go`.
- `app.go` constructs the Adapter via `agentoc.NewAdapter` and wires via `SetAdapter`.
- 8 tests covering resolver paths, SetAdapter guards, error propagation.

### Handler migrations (PR #717)

Five internal-facing paths now check `if h.adapter != nil` and take the Adapter path:

1. **`session_parents.go` `fetchSessionParent`** - `adapter.GetSession` replaces raw HTTP + inline JSON parse. Returns typed `session.Session.ParentID`.
2. **`proxy_session_index.go` `fetchAndPersistTitle`** - `adapter.GetSession` for title + parentID backfill. Extracted `persistSessionMeta` helper shared between Adapter and legacy paths.
3. **`proxy_session_index.go` `runParentBackfill`** - `adapter.ListSessions` replaces raw HTTP + inline parse of session array.
4. **`proxy_permissions.go` `autoApprovePermission`** - `adapter.Resolve(ctx, "", workspaceID, requestID, "always")` replaces raw POST with hardcoded body.
5. **`proxy_input.go` `emitPendingInputRequests`** - `adapter.ListPending` returns unified `[]InputRequest` in a single call, replacing two `fetchFromPod` + dialect-parse cycles. `emitPendingViaAdapter` converts to legacy SSE shapes (`agent.QuestionRequest` / `agent.PermissionRequest`).

### Cleanup (commit on US-65.4 branch)

- 8 sites with hardcoded `SetBasicAuth("opencode", password)` replaced with `agentd.AuthUsername`.
- 1 site with hardcoded `"4096"` port replaced with `opencodePort` constant.
- 3 files gained `pkg/agentd` import for the auth username constant.

---

## Key Decisions

1. **Internal paths first.** Client-facing proxy handlers (CreateSession, SendMessage, etc.) proxy raw opencode bytes to the client. Switching to Adapter-typed responses changes the wire shape, requiring frontend changes. Internal paths (background goroutines, SSE enrichment, session index writes) consume the data themselves and are safe to migrate independently.

2. **Dual-path with nil guard.** Every migrated handler checks `if h.adapter != nil` and falls back to the legacy path. This allows incremental rollout: if the Adapter path has a bug, disabling the adapter (nil) restores the legacy behavior. Once all paths are verified, the legacy paths become dead code and are removed (US-65.5).

3. **`emitPendingViaAdapter` converts to legacy SSE shapes.** The Adapter returns `session.InputRequest` (unified question+permission), but the SSE consumers expect `agent.QuestionRequest` / `agent.PermissionRequest` (separate types). The conversion preserves every field including `RootSessionID` resolution and `Tool` refs. Once the SSE consumers migrate to `InputRequest` (frontend/SDK), this conversion is deleted.

4. **`userID` passed as empty string.** The Adapter's methods require `userID` for ownership verification, but internal paths (SSE event processing, background goroutines) don't have a user context. The `proxyPodIPResolver` ignores `userID` (ownership is enforced upstream in the auth/middleware chain), so empty string is safe. Documented in the resolver's doc comment.

---

## Tests

- `go build ./...` - clean
- `gofmt` / `goimports` - clean
- `golangci-lint --new-from-rev=origin/main` - 0 issues
- Pre-commit hooks (repolint, gofmt, goimports, golangci-lint) - all pass
- Existing tests pass unchanged (Adapter not set in tests; legacy path runs)

---

## Next Steps

**Batch 2 (client-facing handlers):** requires frontend coordination.
- `proxy_handlers.go` CreateSession/ListSessions/GetSession/RenameSession/DeleteSession
- `proxy_handlers.go` SendMessage/SendPromptAsync/AbortSession
- `proxy_handlers.go` GetHistory (the largest single migration - inline opencode history parsing + pagination)
- `proxy_events.go` SSE event bridge
- `proxy_v2.go` V2 session queue

**Batch 3 (US-65.5):** delete legacy paths once all handlers verified on Adapter.

---

## Files Modified

- `api/internal/handlers/proxy.go` (SetAdapter + adapter field + auth constant)
- `api/internal/handlers/proxy_connections.go` (resolver bridges)
- `api/internal/handlers/proxy_input.go` (emitPendingViaAdapter + auth/port constants)
- `api/internal/handlers/proxy_permissions.go` (autoApprovePermission via Adapter)
- `api/internal/handlers/proxy_session_index.go` (fetchAndPersistTitle + runParentBackfill via Adapter)
- `api/internal/handlers/session_parents.go` (fetchSessionParent via Adapter)
- `api/internal/handlers/proxy_events.go` (auth constant)
- `api/internal/handlers/proxy_handlers.go` (auth constant)
- `api/internal/app/app.go` (Adapter construction + wiring)
- `api/internal/handlers/proxy_adapter_infra_test.go` (new - resolver + SetAdapter tests)
