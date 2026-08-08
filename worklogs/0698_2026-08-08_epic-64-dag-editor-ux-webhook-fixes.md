# Worklog: Epic 64 DAG Editor, UX, Webhook Security, and onMissingWorkspace

**Date:** 2026-08-08
**Session:** Build visual DAG editor, trigger UX, run observability, fix webhook security gaps, add onMissingWorkspace policy, and add workspace picker for Epic 64
**Status:** Complete

---

## Objective

The handoff for Epic 64 noted the workflow and triggers UXes were minimal (JSON textarea, no visual editor, no run detail, no delivery log). The user directed: (1) analyze where in-workspace tools should live (proof-backed, not assumed), (2) build a visual DAG editor, (3) make the trigger UX user-friendly with cron translation, (4) add first-class observability/debugging, (5) fix webhook security gaps.

---

## Work Completed

### Analysis: In-workspace tool placement (proof-backed)

Evaluated 5 options for where to expose workflow/trigger tools to the in-workspace opencode agent. Proved via code inspection that:
- The pod already reaches the API at runtime (`helm/templates/workspace-network-policy.yaml:143-155` — relay egress rule).
- The pod already has a projected SA token (`cmd/workspace-agentd/bootstrap.go:59`) audience-scoped to `llmsafespace-api`, validated via TokenReview (`api/internal/handlers/pod_bootstrap.go:80`).
- The Epic 53 MCP injection seam exists (`cmd/workspace-agentd/agent_config_writer.go:472-494`).

**Recommendation: Option A — agentd serves a built-in MCP server.** Eliminates Options B (platform MCP remote — requires net-new token exchange), C (sidecar — strictly worse than A), D (direct PG — breaks security model), E (controller — wrong layer). Not yet implemented; design is settled.

### Frontend: Visual DAG editor (replaces JSON textarea)

- `frontend/src/components/workflows/dagTypes.ts` — spec ↔ flow conversion, typed node interfaces.
- `frontend/src/components/workflows/DAGCanvas.tsx` — @xyflow/react canvas with drag-to-connect, node palette, minimap.
- `frontend/src/components/workflows/WorkflowNode.tsx` — 4 custom node components (script/agent/http/condition) with type-specific colors, icons, and condition-branch handles.
- `frontend/src/components/workflows/NodeEditPanel.tsx` — typed edit panel per node type (script handler, agent prompt/schema/session, http method/headers/secrets, condition branches).
- Updated `WorkflowEditor.tsx` — visual/JSON toggle, canvas replaces textarea.

### Frontend: Run detail page (first-class observability)

- `frontend/src/pages/RunDetailPage.tsx` — per-node timeline (status icon, node type, branch, attempt count, duration), expandable input/output/error per node, final output block, cancel button, auto-poll while running.
- Route: `/workflows/:workflowId/runs/:runId` and `/runs/:runId`.
- WorkflowsPage now shows run history panel + navigates to run detail on manual run.

### Frontend: Trigger UX (user-friendly cron + webhook flow)

- `frontend/src/components/workflows/cronUtils.ts` — friendly ↔ cron conversion, human-readable description, timezone picker.
- Rebuilt `TriggersPage.tsx`:
  - Cron builder (every N min/hours, daily at HH:MM, weekdays) with raw-expression toggle.
  - Webhook create flow: IP allowlist, idempotency mode, one-time secret reveal with copy buttons + signing example.
  - Circuit breaker visualization (progress bar + auto-disabled badge).
  - Delivery log panel (live-polling trigger fires).
  - "Run now" button linking to run detail.

### Backend: Webhook security fixes

- `api/internal/handlers/webhook_receiver.go`:
  - **Rate limiting wired.** Added `RateChecker` interface (matches `interfaces.RateLimiterService.Allow`). Called before HMAC verification. Returns 429 + Retry-After when exceeded. Wired in `app.go:512` via `svc.GetRateLimiter()`.
  - **Hash idempotency implemented.** `computeHashDedupKey(body, tsHeader)` derives dedup key from `sha256(body + floor(ts/5min))`. Same body within same 5-min window = deduped.
- 6 new tests: rate-limited (429), rate-limit-allows (202), hash-dedup same/different body, same/different window.

### Backend: Trigger fires delivery log endpoint

- Added `ListTriggerFires` to `triggerStore` interface + handler methods `UserListFires`/`OrgListFires`.
- Route: `GET /api/v1/me/triggers/:id/fires` (returns recent fires with status, action, timestamps).
- Updated mock trigger store to implement the new interface method.

### Frontend: Delivery log wired to API

- `triggerApi.fires(id)` calls `GET /me/triggers/:id/fires`.
- `DeliveryLog` component shows live-polling fire list with status-colored badges.

---

## Key Decisions

1. **agentd is the right place for in-workspace MCP tools (Option A).** Proven: agentd already has the projected SA token, the network path, the HTTP server, and the Epic 53 injection seam. The auth model mirrors pod-bootstrap (TokenReview → resolve workspace owner → scope calls). Options B-E each have decisive cons. Not yet implemented — design settled.

2. **Don't fake opencode history for non-agent node results.** The run IS the history — surfaced via the run detail page. Faking history would couple us to opencode's internal DB schema and conflate deterministic pipelines with conversational sessions.

3. **Cron UX: friendly by default, raw toggle for power users.** Non-technical users get a frequency/time/timezone builder. The raw mode surfaces the 5-field cron expression and notes the parser's limitations.

4. **Rate limiting on webhooks was a missing security control.** The handler comment claimed it, but the code didn't do it. Now wired via the existing `RateLimiterService.Allow` with per-webhook keys.

---

## Blockers

None. Remaining items (`onMissingWorkspace`, agentd MCP server, workspace picker) are scoped but not started — they require their own sessions.

---

## Tests Run

- `go test -timeout 30s -race -run "TestWebhookReceiver_Rate|TestComputeHash" ./api/internal/handlers/` — 6/6 pass
- `go test -timeout 30s -race -run "TestWebhook|TestTrigger|TestWorkflow" ./api/internal/handlers/` — all pass
- `go build ./api/... ./pkg/... ./cmd/...` — clean
- `npx tsc --noEmit` (frontend) — clean
- `npx vitest run` (workflow + trigger tests) — 18/18 pass

---

## Next Steps

1. **agentd built-in MCP server** — implement Option A: add MCP-over-HTTP to agentd, inject as remote entry in agent-config.json, call new internal platform endpoint `/internal/v1/workspace/:id/workflows/*` with projected SA token.
2. **Delivery log detail** — expand individual fire rows to show envelope + action result.

---

## Session 2 Additions (onMissingWorkspace + Workspace Picker)

### Backend: onMissingWorkspace policy (TDD)

- Migration `000019_on_missing_workspace` — adds `on_missing_workspace text NOT NULL DEFAULT 'abort' CHECK (abort|create)` to `workflows`. Helm mirror synced.
- `pkg/types/workflows.go` — `OnMissingAbort`/`OnMissingCreate` constants + `ValidOnMissingWorkspace()`. Added to `CreateWorkflowRequest`, `UpdateWorkflowRequest`, `WorkflowResponse`.
- `pkg/workflows/store.go` — `WorkflowRow.OnMissingWorkspace`, `WorkflowUpdate.OnMissingWorkspace`. All SQL (Create/Get/List/Update/scan) updated. Added `GetWorkflowPolicy()` and `UpdateRunWorkspace()` store methods.
- `api/internal/handlers/workflows.go` — accepts + validates `onMissingWorkspace` on create (defaults to 'abort') and update.
- `api/internal/workflows/engine.go` — `WorkspaceCreator` interface. `executeRun` now checks `OnMissingWorkspace` when `EnsureActive` fails: if `create` + creator configured, provisions new workspace, pins it as workflow target, retries activation. If `abort` (or no creator), fails run as before.
- `api/internal/app/app.go` — `appWorkspaceCreator` adapter wired into reconciler. Creates workspace via workspace service, pins on workflow.
- Tests (TDD, all green):
  - `TestValidOnMissingWorkspace` (types: abort/create valid, others invalid)
  - `TestWorkflowCreate_OnMissingWorkspace_Create` (handler accepts, stores 'create')
  - `TestWorkflowCreate_OnMissingWorkspace_DefaultsToAbort` (defaults to 'abort')
  - `TestWorkflowCreate_OnMissingWorkspace_InvalidValue` (rejects 'skip')
  - `TestReconciler_OnMissingCreate` (engine creates workspace, updates run, succeeds)
  - `TestReconciler_OnMissingAbort` (engine fails run, no workspace created)

### Frontend: Workspace picker + onMissingWorkspace toggle

- `WorkflowEditor.tsx` — workspace dropdown (fetches workspace list via `useQuery`), "If missing" toggle (abort/create, disabled until workspace selected). Run dialog includes workspace picker when no default target set.
- `WorkflowEditor.test.tsx` — updated all tests with `QueryClientProvider` + mocked `workspacesApi`.
- `WorkflowsPage.tsx` — passes `targetWorkspaceId` + `onMissingWorkspace` through create/update/run flows.
- `api/workflows.ts` — `Workflow` type + `workflowApi.create/update` accept the new fields.

---

## Files Modified

**Frontend (new):**
- `frontend/src/components/workflows/dagTypes.ts`
- `frontend/src/components/workflows/DAGCanvas.tsx`
- `frontend/src/components/workflows/WorkflowNode.tsx`
- `frontend/src/components/workflows/NodeEditPanel.tsx`
- `frontend/src/components/workflows/cronUtils.ts`
- `frontend/src/pages/RunDetailPage.tsx`

**Frontend (modified):**
- `frontend/src/components/workflows/WorkflowEditor.tsx` (workspace picker + onMissingWorkspace toggle)
- `frontend/src/components/workflows/WorkflowEditor.test.tsx` (QueryClientProvider wrapper)
- `frontend/src/pages/WorkflowsPage.tsx`
- `frontend/src/pages/TriggersPage.tsx`
- `frontend/src/pages/TriggersPage.test.tsx`
- `frontend/src/api/workflows.ts`
- `frontend/src/router.tsx`
- `frontend/package.json` (added `@xyflow/react@^12.11.2`)

**Backend (new):**
- `api/migrations/000019_on_missing_workspace.up.sql` + `.down.sql`
- `helm/migrations/000019_on_missing_workspace.up.sql` + `.down.sql`

**Backend (modified):**
- `api/internal/handlers/webhook_receiver.go` (rate limiting + hash idempotency)
- `api/internal/handlers/webhook_receiver_test.go` (6 new tests)
- `api/internal/handlers/triggers.go` (ListTriggerFires + delivery log endpoint)
- `api/internal/handlers/triggers_test.go` (mock update)
- `api/internal/handlers/workflows.go` (onMissingWorkspace acceptance + validation + response)
- `api/internal/handlers/workflows_test.go` (3 new tests + mock update)
- `api/internal/server/router.go` (trigger fires route)
- `api/internal/app/app.go` (rate limiter wiring + appWorkspaceCreator + reconciler wiring)
- `api/internal/workflows/engine.go` (WorkspaceCreator interface + executeRun on-missing logic)
- `api/internal/workflows/engine_test.go` (2 new engine tests + mock updates)
- `pkg/types/workflows.go` (constants + validation + transfer objects)
- `pkg/types/workflows_test.go` (1 new test)
- `pkg/workflows/store.go` (WorkflowRow/WorkflowUpdate fields + all SQL + 2 new methods)
