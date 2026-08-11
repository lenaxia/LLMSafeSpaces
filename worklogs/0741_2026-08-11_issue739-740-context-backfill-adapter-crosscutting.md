# Worklog: #739 + #740 — Context usage backfill + Epic 65 adapter cross-cutting regressions

**Date:** 2026-08-11
**Issues:** [#739](https://github.com/lenaxia/LLMSafeSpaces/issues/739) (context usage), [#740](https://github.com/lenaxia/LLMSafeSpaces/issues/740) (Epic 65 audit)
**Status:** Fixes complete; all tests pass; ready for review.

---

## #739 — Per-session context usage broken after API pod restarts

### Diagnosis (corrected from issue)

**Original issue hypothesis:** opencode 1.18.10 changed the `session.next.step.ended` event's `properties.tokens` wire shape, causing `persistContextFromEvent` to silently no-op.

**Actual root cause (disproven original hypothesis):** The event shape is **identical** across opencode 1.15.12 and 1.18.10. Verified by:
1. Capturing a live `session.next.step.ended` event from an opencode 1.18.10 pod during an active LLM step. The `properties.tokens` shape (`{input, output, reasoning, cache.{read,write}}`) matches the parser exactly.
2. Triggering a test step on a 1.18.10 workspace and confirming `context_used` WAS persisted to the DB (`ses_00e263c0cffefp7OTjuOM7YTkR` → `context_used=3733`).

**Real root cause:** Data freshness gap. `session_index.context_used` is populated exclusively by **live** SSE events (`persistContextFromEvent` on `session.next.step.ended`). When the API pod restarts (deploy, crash), the SSE tracker reconnects but **does not replay missed events**. Any session whose last LLM step completed during the downtime has `context_used = NULL` in the DB.

Proven by version correlation in production:
| Workspace | opencode | DB context_used | CRD status context_used |
|---|---|---|---|
| f94d596f (1.15.12) | values present | ✓ | ✓ |
| 1044f4f2 (1.18.10) | ses_a: NULL, ses_b: NULL | ✗ | ses_a: 75043, ses_b: 325355 ✓ |

The CRD status (populated by agentd every ~60s via the controller) HAS the correct values. The DB does not.

### Fix 1 — Backfill from CRD status (read-time merge)

**File:** `api/internal/services/workspace/workspace_service.go`

Added `backfillContextUsed()` method called by `ListWorkspaceSessions()`. When any session has `context_used = NULL`, the method:
1. Fetches the workspace CRD (only when needed — skipped when all sessions have values)
2. Builds a `sessionID → contextUsed` map from `crd.Status.Sessions`
3. For each NULL session, fills from the CRD map
4. **Persists the backfilled value to the DB** (`UpsertContextUsed`) so subsequent calls skip the CRD fetch (self-healing)
5. Fails open: CRD fetch failure or DB persist failure returns the DB values as-is without error

### Fix 2 — Observability in persistContextFromEvent

**File:** `api/internal/handlers/proxy_events.go`

Replaced the three silent `return` paths in `persistContextFromEvent` with structured warn logs:
- JSON parse failure → `"persistContextFromEvent: failed to parse step.ended event"`
- Empty sessionID → `"persistContextFromEvent: step.ended event missing sessionID"`
- Nil tokens → `"persistContextFromEvent: step.ended event missing tokens — opencode wire shape may have changed"`

These ensure the next opencode wire-shape drift is visible in logs instead of being silent data loss.

### Tests (TDD)

**7 new tests** in `api/internal/services/workspace/context_backfill_test.go`:
- `TestListWorkspaceSessions_BackfillsContextUsedFromCRD` (RED→GREEN)
- `TestListWorkspaceSessions_PreservesDBContextUsed` (RED→GREEN)
- `TestListWorkspaceSessions_AllHaveContext_SkipsCRDFetch` (GREEN→GREEN)
- `TestListWorkspaceSessions_CRDFetchFails_ReturnsDBValuesFailOpen` (GREEN→GREEN)
- `TestListWorkspaceSessions_CRDHasNoSessionData` (GREEN→GREEN)
- `TestListWorkspaceSessions_BackfillPersistsToDB` (RED→GREEN)
- `TestListWorkspaceSessions_BackfillUpsertFails_NoError` (RED→GREEN)

**3 new tests** in `api/internal/handlers/context_observability_test.go`:
- `TestPersistContextFromEvent_MissingTokens_LogsWarn`
- `TestPersistContextFromEvent_UnparseableEvent_LogsWarn`
- `TestPersistContextFromEvent_EmptySessionID_LogsWarn`

**Fixed** 3 existing tests in `opencode_upgrade_test.go` that constructed `ProxyHandler` without a logger (now required for the warn log path).

**Fixed** 1 existing test in `workspace_session_test.go` that needed a CRD mock expectation for the backfill path.

---

## #740 — Epic 65 adapter cross-cutting regressions

### Root cause

Every `if h.adapter != nil` branch in `proxy_handlers.go` was wired as a shortcut that bypassed `proxyToWorkspaceWithErrBody` — the legacy code path that provides workspace-readiness checks, connection limits, session limits, metering, quota enforcement, activity tracking, and more.

### Fix — Cross-cutting helpers extracted and wired

**New file:** `api/internal/handlers/proxy_adapter_crosscutting.go`

Extracted 5 shared helpers from `proxyToWorkspaceWithErrBody`:

| Helper | Cross-cutting concerns restored |
|---|---|
| `resolveWorkspaceForAdapter` | A: workspace readiness (503 + Retry-After), B: connection limit (429) |
| `checkAdapterSessionLimit` | C: MaxActiveSessions enforcement (429) |
| `checkAdapterQuota` | G: quota enforcement (429) |
| `postAdapterSuccess` | F: metering/billing, H: activity tracking, I: session-index message recording |
| `adapterEnsureSSEWatch` | SSE tracker connection for event flow |

**Wired into all 6 adapter handler branches:**

| Handler | Concerns restored |
|---|---|
| `SendMessage` | readiness + conn limit + session limit + quota + SSE watch + metering + activity + session index |
| `SendPromptAsync` | readiness + conn limit + session limit + quota + SSE watch + metering + activity + session index |
| `GetHistory` | readiness + conn limit |
| `GetSession` | readiness + conn limit |
| `ListSessions` | readiness + conn limit |
| `CreateSession` | readiness + conn limit |

**Error path cleanup:** SendMessage and SendPromptAsync now call `removeActiveSession` on failure (matching the legacy path's cleanup).

### Tests

**5 new tests** in `api/internal/handlers/adapter_crosscutting_test.go`:
- `TestAdapterPath_GetHistory_WorkspaceNotReady_Returns503`
- `TestAdapterPath_GetSession_WorkspaceNotReady_Returns503`
- `TestAdapterPath_ListSessions_WorkspaceNotReady_Returns503`
- `TestAdapterPath_CreateSession_WorkspaceNotReady_Returns503`
- `TestAdapterPath_GetHistory_WorkspaceReady_Returns200`

**Fixed** `newProxyHandlerForAdapterTest` to set up K8s mocks for workspace resolution (required by the new cross-cutting code).

---

## Validation

```
$ go test ./api/internal/handlers/ -count=1 -timeout 180s
ok  95.244s

$ go test ./api/internal/services/workspace/ -count=1 -timeout 60s
ok  1.016s

$ go vet ./...     # clean
$ go build ./...   # clean
$ gofmt -l ...     # clean
```

---

## Files Changed

- `api/internal/services/workspace/workspace_service.go` — `backfillContextUsed` + updated `ListWorkspaceSessions`
- `api/internal/handlers/proxy_events.go` — observability in `persistContextFromEvent`
- `api/internal/handlers/proxy_adapter_crosscutting.go` — **new**: 5 extracted helpers
- `api/internal/handlers/proxy_handlers.go` — wired helpers into SendMessage, SendPromptAsync, GetHistory, GetSession, ListSessions, CreateSession
- `api/internal/services/workspace/context_backfill_test.go` — **new**: 7 tests
- `api/internal/handlers/context_observability_test.go` — **new**: 3 tests
- `api/internal/handlers/adapter_crosscutting_test.go` — **new**: 5 tests
- `api/internal/handlers/opencode_upgrade_test.go` — fixed 3 tests for logger requirement
- `api/internal/handlers/adapter_path_test.go` — fixed `newProxyHandlerForAdapterTest` with K8s mocks
- `api/internal/services/workspace/workspace_session_test.go` — fixed test for CRD mock

---

## Follow-up (not in this PR)

- **#740 deferred concerns:** Request buffering (D) and stale-IP retry (E) require adapter-level connection hooks and are deferred to a follow-up. The cross-cutting helpers land the most critical concerns (metering, quota, session limits, workspace readiness).
- **#740 model selection:** SendMessage/SendPromptAsync still pass `session.SendOpts{}` without parsing the model from the request body. Requires a request-body schema change (the current body is opencode's `{parts:[{type:"text",text}]}` shape which doesn't carry a model field). Deferred to a follow-up that designs the contract body schema.
- **#737** (GetHistory 16MB cap) is being worked on separately.
