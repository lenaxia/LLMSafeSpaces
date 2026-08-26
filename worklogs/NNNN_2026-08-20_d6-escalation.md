# Worklog: D6 unattended escalation (#998)

**Date:** 2026-08-20
**Session:** Implement D6 — notify-only surfacing of hung-and-alive sessions. Branch `feat/998-d6-escalation`.
**Status:** Complete

## Objective

Post-watchdog-demotion (design 0050 D1), ambiguous hangs are
suppressed-forever by design — correct when someone watches. D6 surfaces
hung-and-alive sessions to the owner: **notify, never execute**.

## Work Completed

### agentd
- `session_tracker.go`: `busySince` timestamps (set on busy transition,
  NOT restarted by re-set; cleared on idle AND by the D2 generation
  reset — an orphaned flag's age is fiction). `busyDurations()`.
- `StatuszResponse`: `oldest_busy_seconds` + per-session `busy_ages`.

### API
- `escalateHungs` on the sseWatchReconciler tick (60s): for each watched
  Active workspace past cooldown, fetch statusz (bearer candidates, 5s
  timeout); `oldest_busy_seconds >= 15min` → publish
  `workspace.alert`/`session_hung` (session_id = oldest, policy
  notify_only, guidance text) to workspace + user streams. Cooldown
  30min per workspace (unattended hangs persist; per-tick alerts would
  be noise). Statusz failures silent-to-log (slow pods are the tracker
  alerts' domain, not this path's).
- Failure-isolated: a panic in the sweep is recovered and logged; the
  reconciler (which arms SSE watches) must never die from D6 — losing it
  re-creates the #902 incident class. Found by the full suite: a mock
  panic in the pre-existing reconciler test killed the goroutine.

### Frontend
- `WorkspaceAlertEvent` typed; ChatPage amber banner ("busy for N min
  without completing — possibly hung; nothing was stopped
  automatically"), auto-cleared on that session's next idle,
  dismissable.
- `SessionActivityProvider`: workspace-keyed `hungWorkspaces` set (set
  on alert, cleared on the workspace's next idle); `useWorkspaceHung`.
- Sidebar: amber hung badge replaces the busy dot while alerted.

## Key Decisions
- 15min threshold / 30min cooldown as package vars (design sketch
  values; workspace-class config deferred until evidence demands it).
- Workspace-keyed cooldown + badge: an unattended hang persists until a
  human acts; per-session tracking would churn.
- Notify-only pinned by test: zero workspace mutations during
  escalation.

## Tests
- agentd: busyDurations lifecycle, generation-reset clears busy ages,
  statusz fields through the real handler (idle → 0; 20min busy →
  oldest ≥ 1199).
- API (5, -race): fires once per cooldown (names session, notify_only),
  below-threshold silent, statusz-down silent, cooldown expiry re-fires,
  notify-only (no workspace Patch).
- Frontend: banner renders + auto-clears on idle; dismissable. 476
  hook/component tests green; tsc clean.
- Full handlers suite green (98s) after the isolation fix;
  agentd suite green.

## Files Modified
- cmd/workspace-agentd/session_tracker.go (+tests), main_test.go
- pkg/agentd/types.go
- api/internal/handlers/proxy_lifecycle.go (+proxy_d6_test.go)
- api/internal/handlers/proxy.go (busyAlerts state)
- frontend/src/api/types.ts, pages/ChatPage.tsx (+queue tests),
  providers/SessionActivityProvider.tsx, components/layout/Sidebar.tsx

## Round 2 — PR #1003 review findings (2026-08-26)

Review verdict: REQUEST CHANGES — one hard blocker (issue #998 finding
4: "event lands in session history for workflow surfaces" — transient
SSE + in-memory only) plus missing test categories and a Lint failure
(imports) + frontend mock breakage (`useWorkspaceHung` missing from 16
test mocks).

### Finding 4 — alert persistence
- migration 000026: `session_alerts` table (ws, session, alert, oldest
  busy seconds, created_at; ws+created DESC index).
- `api/internal/services/sessionalerts`: sessionindex-pattern service —
  non-blocking RecordAlert → bounded queue → drainer → InsertSessionAlert;
  Stop flushes; ListByWorkspace filters 24h retention.
- `ProxyHandler.SetSessionAlerts` + RecordAlert hook in escalateHungs
  (nil = SSE-only dev/test). GET /workspaces/:id/alerts (limit 1-200,
  default 50) for reconnects/workflows.
- Wired in app.go alongside sessionIndex (Start/Stop lifecycle).

### Tests added
- Handler: PersistsAlert (mock service args), ConcurrentCooldownIsolation
  (cooling ws cannot mask a second hung ws), ReconcilerTickIntegration
  (real sseWatchReconciler tick → live statusz fetch asserting Bearer
  auth via workspace-pw admin-token secret), GetWorkspaceAlerts (list,
  limit=0/abc → 400), NotConfigured → 501.
- Service: non-blocking enqueue, drain-to-insert, Stop flushes 3,
  retention filter hides >24h.
- Sidebar: 4 badge tests (hung badge on collapse, replaces busy dot,
  busy-but-healthy keeps dot, idle shows nothing) — sidebar auto-expands
  groups on load; badge renders collapsed-only, tests collapse first.
- 16 existing test-file mocks gained `useWorkspaceHung`; e2e_suspend
  recordingDB gained the two new DatabaseService methods; imports fixed.

### Verification
- handlers 110s green (race), sessionalerts green, app green, mocks
  build; imports-check + fmt-check clean.
- Frontend 1703 pass (7 fails confined to a foreign in-flight
  SessionAuthority.test.tsx — untracked, not in this PR).

## Round 3 — PR #1003 second review (2026-08-26)

Verdict: REQUEST CHANGES — Finding 3b: alerts infrastructure existed but
no surface CONSUMED it; 4 e2e gaps; OpenAPI contract test failed on the
undocumented route.

### Surfaces now consume alerts (#998 finding 3b)
- frontend workspacesApi.getAlerts; SessionActivityProvider seeds
  hungWorkspaces from persisted alerts once per workspace on (re)load —
  reconnect recovery for the banner/badge.
- WorkflowsPage: amber hung badge on runs whose workspace has
  persisted alerts (workflow surfaces see hangs without SSE).
- GET /workspaces/{id}/alerts documented in sdks/openapi.yaml
  (+SessionAlert schema) — fixes TestOpenAPIRouterContract.

### E2E tests (handlers)
- DetectionToHistory: real reconciler tick → SSE alert AND persisted
  insert through the real sessionalerts service.
- PanicIsolation_ReconcilerSurvives: RecordAlert panics; reconciler
  keeps ticking (>=3 ticks).
- PersistFailureStillAlerts: DB insert errors; SSE alert still emitted;
  failure logged, no crash.

### Frontend tests
- SessionActivityProvider.alerts.test.tsx (3): seeds hung from alerts,
  stays healthy when empty, holds state on fetch failure.
- WorkflowsPage badge (2): flags hung-workspace runs, silent when clean.
