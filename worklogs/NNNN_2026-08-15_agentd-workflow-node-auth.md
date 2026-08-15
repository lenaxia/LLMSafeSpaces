# Worklog: agentd workflow-node auth enforcement + executor caller wiring (#762)

**Date:** 2026-08-15
**Session:** Fix issue #762 — /v1/workflow/node/execute and /v1/workflow/node/cancel on the agentd user mux (4097) had no authentication. Add Basic-auth gates on agentd, wire the legitimate API-server callers, and fix the latent PreserveOnFailure delete-session 401 bug found during review.
**Status:** Complete

---

## Objective

Close #762: any in-pod process could POST arbitrary script/http/condition/agent nodes to agentd (RCE + SSRF bypass) and cancel in-flight nodes, with no credential check. Auth pattern already existed on /v1/dev-preview and /v1/workflow/session/delete — this session extends it to the remaining workflow endpoints and wires every legitimate caller.

---

## Work Completed

### agentd (cmd/workspace-agentd)

- `auth.go` (new): shared `checkBasicAuth` / `rejectUnauthorized` / `basicAuth` helpers. `crypto/subtle.ConstantTimeCompare` on the full Authorization header (same rationale as relay-proxy `X-Relay-Token`, worklog 0447); unified 401 + `WWW-Authenticate: Basic realm="agentd"`.
- `workflow_execute.go`: `workflowExecuteHandler` and `workflowCancelHandler(password)` now gate at entry. `workflowCancelHandler` signature changed from `()` to `(password string)`; `server.go` registration updated.
- `workflowDeleteSessionHandler`: refactored from plain `!=` string compare onto the constant-time helper (behavior-compatible: 401 on bad creds, now with challenge header).
- `dev_preview.go`: refactored onto the same helpers (same behavior, removes the 4th copy of the pattern).

### API server (api/internal)

- `workflows/engine.go`:
  - `AgentdExecutor.Execute` gains a `workspaceID` parameter (needed to resolve the per-workspace password; podIP alone is insufficient).
  - `HTTPAgentExecutor` gains required `PasswordProvider` (`api/internal/interfaces.WorkspacePasswordProvider`, US-46.11 — satisfied structurally by `*handlers.ProxyHandler`); `Execute` resolves the password and sets `Authorization: Basic opencode:<pw>` on every dispatch. Nil provider fails fast with an explicit error. Non-200 responses now return an explicit error (a 401 previously would have surfaced as a confusing JSON-parse failure).
  - Extracted `deleteRoutineSession` (pure) + `Scheduler.deleteRoutineSessionAuthorized`. **Bug fixed:** the routine PreserveOnFailure path called `/v1/workflow/session/delete` without auth — agentd has enforced auth there since Epic 64, so every call 401'd and the `< 400` check silently swallowed it; ephemeral sessions were never deleted. Now sends Basic auth and logs non-2xx.
  - `Scheduler` gains `PasswordProvider` field.
- `app.go`: both `HTTPAgentExecutor` constructions (reconciler + scheduler) and the `Scheduler` get `PasswordProvider: proxyHandler`.

### Tests (TDD — written first, verified red, then green)

- agentd: `TestCheckBasicAuth` (7 cases incl. malformed base64, wrong scheme), `TestRejectUnauthorized`, `TestWorkflowExecute_RequiresAuth/WrongPassword/ValidAuth`, `TestWorkflowCancel_RequiresAuth/WrongPassword`, `TestWorkflowDeleteSession_RequiresAuth/WrongPassword`. Existing workflow tests updated to send credentials.
- API: `TestHTTPAgentExecutor_SetsBasicAuthHeader` (asserts exact header + workspaceID passed to provider), `_Non200ReturnsError`, `_MissingPasswordProvider`, `_PasswordProviderError`, `TestDeleteRoutineSession_SendsAuth`, `_401IsNotDeleted` (asserts logging). Mocks updated for the new interface signature.

---

## Key Decisions

1. **Reuse `interfaces.WorkspacePasswordProvider`** rather than inventing a workflows-local interface — it already exists (US-46.11), `ProxyHandler` implements it with a Redis/in-memory cache in front of the K8s Secret read, so per-dispatch resolution is cheap after first call.
2. **Constant-time compare** on the full header string via `subtle.ConstantTimeCompare` — Go's standard idiom; length-mismatch early-return is accepted (same as relay-proxy precedent).
3. **`workspaceID` in the executor interface** rather than resolving the password at construction time — passwords are per-workspace and rotatable via pod recreation; resolving per dispatch follows the existing `getPassword` cache pattern.
4. **Deploy-ordering note**: old agentd + new API is harmless (extra header ignored); new agentd + old API would 401 every dispatch. The chart ships API + agentd together (digest-pinned image volume, #872), so a coordinated rollout is the normal path. Flagged in the PR description for the reviewer.
5. **Scope**: `/v1/mcp` stays ungated in this PR — it is #847's scope (needs the injected opencode MCP entry to carry headers). `/v1/reload-secrets` and `/v1/agent/reload` are #848's scope.

---

## Blockers

None.

---

## Tests Run

- `go test -timeout 120s -count=1 ./cmd/workspace-agentd/` — ok (full suite incl. script/e2e, 328s)
- `go test -timeout 300s -count=1 ./api/internal/workflows/` — ok (1.1s)
- `go test -timeout 900s -count=1 -short ./api/internal/app/...` — ok (0.8s)
- `go build ./...` — ok
- `golangci-lint run --new-from-merge-base=origin/main` (changed packages) — 0 issues
- TDD red phase verified: new auth tests failed compile/run before `auth.go` + handler changes.

---

## Next Steps

- PR 2 (#847): gate `/v1/mcp`, add `headers.Authorization` to the injected `llmsafespaces` remote MCP entry, verify the pinned opencode build sends headers on `initialize` (spike; fallback = drop session tools from default surface).
- PR 3 (#848): gate `/v1/reload-secrets` + `/v1/agent/reload`; wire passwords into `agentpush.Service.Push` and `AgentReloadHandler`'s two dispatch sites (`SetPasswordGetter` already exists).
- Follow-up issue (out of scope): Basic auth does not defend against same-uid in-pod code (password readable at /sandbox-cfg/password). True in-pod mitigation = run agentd under a separate uid. NetworkPolicy remains the primary control; this PR is defense-in-depth, consistent with the owner's triage note on #762.

---

## Files Modified

- `cmd/workspace-agentd/auth.go` (new)
- `cmd/workspace-agentd/auth_test.go` (new)
- `cmd/workspace-agentd/workflow_execute.go`
- `cmd/workspace-agentd/workflow_execute_test.go`
- `cmd/workspace-agentd/server.go`
- `cmd/workspace-agentd/dev_preview.go`
- `api/internal/workflows/engine.go`
- `api/internal/workflows/engine_test.go`
- `api/internal/app/app.go`
- `worklogs/NNNN_2026-08-15_agentd-workflow-node-auth.md` (this file)
