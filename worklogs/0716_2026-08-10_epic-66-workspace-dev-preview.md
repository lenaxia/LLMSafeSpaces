# 0716 — Epic 66: Workspace Dev Preview

**Date:** 2026-08-10
**Epic:** [66](../design/stories/epic-66-workspace-dev-preview/README.md) — Workspace Dev Preview
**Status:** Complete (US-66.1 through US-66.8 + audit fixes)
**Branch:** `feat/epic-66-workspace-dev-preview`

## Objective

Implement an authenticated HTTP/WS tunnel from the browser to dev servers (Vite, Next, etc.) running inside a workspace pod. Closes the "I can't see my React app" gap that sent users to `kubectl port-forward` (worklogs 0069, 0705) and caused the worklog 0678 liveness incident.

## Scope shipped

- **US-66.1** (spike) — closed A1 (HMR through ReverseProxy proven), A4 (Host rewrite mandatory — CVE-2025-30208), A6 (both Content-Length and chunked response shapes normal). Artifact: `PREVIEW-CONTRACT.md`.
- **US-66.2** — `DevPreview bool` on `WorkspaceNetworkAccess` CRD type; deepcopy regenerated; CRD manifest updated.
- **US-66.3** — agentd in-pod forwarder on user mux (4097): Basic auth, port denylist (4096/4097/4098 + <1024), `httputil.ReverseProxy` with Host rewrite, zero custom Hijack code.
- **US-66.4** — API `DevPreviewHandler` on idGroup: inherits auth + ownership, opt-in check, kill-switch, connection cap, response size cap (both shapes), separate transport, Basic auth injection, 502 on unreachable. Response size cap enforced via `cappedReader` wrapper for chunked streams + Content-Length pre-reject.
- **US-66.5** — instance settings registered (`devPreview.enabled`, `maxResponseBytes`, `maxConnsPerWorkspace`).
- **US-66.6** — frontend toggle + open-preview affordance in WorkspaceSettingsDrawer.
- **US-66.7** — router integration tests (route registered, behind auth, optional).
- **US-66.8** — user guide (`docs/user/dev-preview.md`).
- **Audit fixes** — API DTO (`DevPreviewEnabled` on Workspace + WorkspaceListItem), `SetDevPreview` service method, `PUT /workspaces/:id/dev-preview` endpoint, OpenAPI spec, SDK helpers (TS/Py/Go/Java), MCP `dev_preview_url` tool with enable instructions, MCP injection port fix (4098→4097).

## Architecture

```
Browser → GET /api/v1/workspaces/:id/dev-preview/:port/*
  → AuthMiddleware (JWT cookie or Bearer header)
  → WorkspaceAccessMiddleware (ownership)
  → DevPreviewHandler
    → opt-in check (spec.networkAccess.devPreview)
    → kill-switch (devPreview.enabled instance setting)
    → port denylist (4096/4097/4098 + <1024)
    → connection cap (default 50)
    → httputil.ReverseProxy → podIP:4097/v1/dev-preview/:port/*
      → Basic auth injected (workspace password)
      → Host rewritten to localhost:<port> (CVE-2025-30208)
      → Response size cap (cappedReader for chunked, Content-Length pre-reject)
  → agentd devPreviewHandler (port 4097)
    → Basic auth validated
    → port denylist re-checked
    → httputil.ReverseProxy → localhost:<port>
```

## Design decisions (validated against spike + codebase)

1. **agentd-mediated (Option B)**, not API-direct or per-workspace Ingress — reuses existing API→pod:4097 NP allowance, zero NP changes.
2. **Authenticated owner-only** — route on idGroup inherits AuthMiddleware + WorkspaceAccessMiddleware. No shareable URL in v1.
3. **Opt-in via CRD** — `spec.networkAccess.devPreview`, enforced by API handler alone (agentd has no K8s client; consistent with terminal proxy precedent).
4. **Path-based port discovery** — `/dev-preview/:port/*`; no CRD port allowlist.
5. **HTTP+WS only** — no raw-TCP port forwarding.
6. **Public/shareable URLs out of scope** (D7) — categorically different product requiring trust-and-safety infra.

## Bugs found and fixed during implementation

1. **MCP injection port mismatch (pre-existing)** — `injectAgentdMCPServer` injected URL as `http://127.0.0.1:4098/v1/mcp` (admin port), but `mcpHandler` is registered on the user mux (4097). Fixed to `AgentdPort` (4097). Affected `session_list`/`session_read` too.
2. **API DTO missing field** — frontend toggle wrote to a field the backend didn't serialize. Added `DevPreviewEnabled` to `Workspace`/`WorkspaceListItem`, wired CRD→DTO mapping, added dedicated `PUT /workspaces/:id/dev-preview` endpoint (rather than a general PATCH).

## Test coverage

- 10 agentd dev_preview handler tests (auth, port validation, HTTP round-trip, WS upgrade, recursion, dev-server-not-listening)
- 12 API DevPreviewHandler tests (workspace states, opt-in, kill-switch, denied ports, HTTP round-trip, size caps for both shapes, connection limit)
- 7 workspace service tests (SetDevPreview enable/disable/wrong-owner/not-found/CRD-get-fails; GetWorkspace returns DevPreviewEnabled)
- 3 router integration tests (route registered, behind auth, optional)
- 6 MCP `dev_preview_url` tests (happy path, with path, missing port, out of range, tools-list inclusion)
- 2 CRD deepcopy tests (round-trip, default false)
- 2 frontend tests (toggle saved, open-preview link)
- 1 MCP injection port test (locks the 4097 fix)

Total: 43 new tests, all passing.

## Files changed (33)

**New (10):** `api/internal/handlers/dev_preview.go` + test, `api/internal/server/router_devpreview_test.go`, `api/internal/services/workspace/dev_preview_test.go`, `cmd/workspace-agentd/dev_preview.go` + test, `design/stories/epic-66-workspace-dev-preview/README.md` + `PREVIEW-CONTRACT.md`, `docs/user/dev-preview.md`

**Modified (23):** CRD type + test, deepcopy, CRD manifest, container/workspace types, settings registry, workspace service, router, app.go, interfaces, mocks, agentd server + MCP server + MCP test, frontend types + drawer + drawer test, OpenAPI, all four SDKs, Java client.

## Open items (deferred with rationale)

- **Operator guide** (US-66.8 only delivered user guide) — operators discover kill-switch/caps via instance settings registry; dedicated doc is a follow-up.
- **Stale-IP retry not extracted** (Rule 4 — two consumers with different retry shapes don't validate a shared abstraction).
- **Workspace audit log** — no workspace-level audit log exists in the platform; toggle audit is a platform-wide concern, not Epic 66-specific.
- **Agent can't query opt-in state** — `dev_preview_url` MCP tool always includes enable instructions; acceptable for v1.

## Assumptions validated (per Rule 7)

| ID | Assumption | Validation |
|---|---|---|
| A1 | ReverseProxy forwards WS upgrade | ✅ Spike — Vite HMR round-trip proven |
| A4 | Host rewrite required | ✅ Spike — Vite ≥5.4.13 returns 403 for foreign Host (CVE-2025-30208) |
| A6 | Cap must handle both response shapes | ✅ Spike — Vite's small responses are sized, large are chunked |
| Auth pattern | agentd user mux uses Basic auth | ✅ Codebase-verified — `workflow_execute.go:130`, every user-mux handler takes deps.password |
| MCP port | injection URL must match handler port | ✅ Found + fixed — was 4098, now 4097 |

## Build/test/vet/lint status

- `go build ./...` — clean
- `go vet` on all changed packages — clean
- `gofmt` on all changed files — clean
- All 43 new tests pass; existing tests in changed packages pass
- Frontend `tsc --noEmit` — clean; `vitest` — 17/17 pass
