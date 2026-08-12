# Worklog: Epic 65 hotfix marathon — chat history, message loss, stuck-busy, wire-shape drift, security

**Date:** 2026-08-11 through 2026-08-12
**Issues fixed:** #737, #739, #740, #743, #744, #745, #746, #747, #749, #750, #751, #753, #754, #755, #757, #763, #764, #765, #792
**PRs merged:** #741, #748, #757, #738, #795
**Status:** All fixes on `main`. Production intentionally held at v0.13.1 until end-to-end verification.

---

## Executive Summary

The Epic 65 adapter migration (v0.14.0) introduced a cascade of regressions that broke the core chat experience: sessions showed "Failed to fetch" on large histories, messages disappeared after sending, sessions stayed stuck "busy" forever, and multiple opencode 1.18.10 wire-shape changes caused silent data loss. This worklog covers the full investigation, root-cause analysis, fix implementation, and what remains.

**The user's four reported symptoms, traced to root cause:**

| Symptom | Root Cause | Fix | Verified |
|---|---|---|---|
| Chat history unavailable (502) | 16MB `readBody` cap truncated large history bodies → JSON parse failure | Streaming `json.Decoder` (#737, PR #738) | Code path verified |
| Messages disappear after send | `SendPromptAsync` used V2 queue (`delivery:"queue"`) which opencode 1.18.10 admits but never drains | Switched to synchronous V1 `adapter.Send` (#755, PR #757) | Live-tested V1 vs V2 on pod |
| Session stuck "busy" forever | Session list read status from in-memory `activeSess` map that goes stale when SSE events are missed | Ground-truth `/v1/statusz` query on every read (#792 Pattern 1, PR #795) | Code path + router test verified |
| Chat history crash on new sessions | `GetHistory` returned `null` for empty sessions → frontend `.filter()` crash | Initialize to `[]session.Message{}` (direct push to main) | Code path verified |

**What is NOT yet verified:**
- End-to-end through the full API stack with adapter wired in production
- Whether long LLM turns (60s+) complete without timeout now that the 10s client timeout was removed
- Whether the statusz endpoint is reachable from the API pod at the expected port
- Frontend behavior with the new response shapes

---

## Timeline of changes on `main`

### PR #738 — GetHistory streaming decoder (#737)
- Replaced capped `readBody(resp, 16<<20)` + batch `json.Unmarshal` with `ParseHistoryStream(resp.Body, ...)` using a streaming `json.Decoder`
- No body-size cap; peak decode memory is O(largest single message)
- Returns partial results on truncated body (graceful degradation)
- Added `SystemMessage` nil-CreatedAt fix (review finding)
- Added abort consistency (`v2Pending.remove` on legacy path)

### PR #748 — Wire-shape drift: providerID, Agent, Status (#743, #744)
- `ocModelRef.Provider` custom `UnmarshalJSON` accepting both `"provider"` (1.15.x) and `"providerID"` (1.18.10)
- Added `Agent` field to `ocSession` + `AgentID` mapping in `translateSession`
- Changed `Status` from required value type to `json.RawMessage` with polymorphic `translateSessionStatus`
- V2 bridge: busy-session guard in `wakeStrandedV2Sessions`, TTL pruning in `v2PendingSessions`, symmetric nil-guard

### PR #757 — Full audit fixes: #745, #746, #747, #749, #750, #751, #753, #754, #755
- **#755 Sev1 (messages disappear):** `SendPromptAsync` switched from `adapter.SendAsync` (V2 queue) to `adapter.Send` (V1 synchronous)
- **#750 (reasoning dropped):** `translate.go` reads `p.Text` as fallback when `p.Reasoning` is empty
- **#746 (10s HTTP timeout):** Removed hard `http.Client.Timeout` — context deadline is the boundary
- **#745 (CapDiff false advertising):** Conditional on `a.differ != nil`
- **#751 (SSE tracker):** Cost parsed as `json.RawMessage` (not float64), provider key fallback, `StopWatching` cleans all 3 billing maps
- **#753 (secrets rotation):** agentd tracker treats `retry`/`error`/`compacting` as busy
- **#749 (MCP OOM):** 4MB/16MB `io.LimitReader` caps + status-code validation
- **#754 (session_index drops):** Warn log + Prometheus counter

### Direct pushes to main
- **Null history crash fix:** `if page == nil { page = []session.Message{} }` before `c.JSON(200, page)`
- **Stuck-busy SSE watch fix:** Added `adapterEnsureSSEWatch` to GetHistory, GetSession, ListSessions, CreateSession read paths
- **opencode.json repair:** Removed corrupted bash deny-list from CI config
- **Line 445 fix:** `/sessions/active` endpoint now uses `GetAuthoritativeActiveSessions`

### PR #795 — Ground-truth session status (#792 Pattern 1)
- `GetAuthoritativeActiveSessions`: queries `/v1/statusz` for authoritative busy/idle
- Replaced `GetActiveSessions` (in-memory map) at router.go:1303 (session list) and router.go:445 (active sessions endpoint)
- Self-healing: reconciles stale in-memory `activeSess` entries as a side effect
- Conservative fallback: falls back to in-memory on workspace-not-ready; returns empty on statusz error
- 5 TDD tests including router-level regression test

---

## Detailed root-cause analysis

### 1. Chat history unavailable (502 Bad Gateway)

**User symptom:** `GET /api/v1/workspaces/:wid/sessions/:sid/message` returns 502. Specific sessions affected, others work.

**Root cause:** `Adapter.GetHistory` called `readBody(resp, 16<<20)` which wraps the response body in `io.LimitReader(resp.Body, 16<<20)`. For the affected session (91 MB of message history), this silently truncated the body at 16 MiB. The truncated bytes are not valid JSON, so `json.Unmarshal` failed → adapter returned error → handler returned 502.

**Live evidence:** Direct pod probe showed opencode returned HTTP 200 with 91 MB body. The platform's 502 was purely in its decode layer.

**Fix:** `ParseHistoryStream(resp.Body, ...)` uses `json.NewDecoder(r)` and decodes messages one at a time via `dec.Decode(&ocMessage)` in a loop. No buffering of the full array. Peak memory is O(largest single message), not O(total history size).

**What could still go wrong:** If a single message is >2GB (unlikely but theoretically possible with a massive tool output), the `json.Decoder` would still work but memory would spike. Not a practical concern.

### 2. Messages disappear after sending

**User symptom:** User sends a message, sees it briefly, then it vanishes when switching sessions and coming back.

**Root cause:** `SendPromptAsync` (the frontend's send path via `sendAsync`) called `adapter.SendAsync` which used opencode's V2 prompt API with `delivery:"queue"`. On opencode 1.18.10, the V2 queue is admitted (returns 200 + messageID) but **never drained** — the queue processor depends on SSE events that have changed taxonomy. The message sits in the queue forever; opencode never processes it; it never appears in `/session/:id/message`.

**Live evidence:** Direct pod test — V2 queue prompt on 1.18.10 workspace: 0 messages after 30s. V1 message send on same workspace: 2 messages immediately.

**Fix:** `SendPromptAsync` now calls `adapter.Send` (V1 `POST /session/:id/message`) which is synchronous — blocks until the LLM turn completes and the response is persisted. The frontend receives the assistant message in the HTTP response body and also via SSE events.

**What could still go wrong:** If the LLM turn takes longer than the gin/proxy timeout (typically 120s for long reasoning models), the request will time out. The message IS persisted in opencode, but the frontend gets a timeout error. The user would need to refresh to see the response. This is the same behavior as the legacy pre-Epic-65 path.

### 3. Session stuck "busy" forever

**User symptom:** Session shows a busy/spinner indicator that never clears, even after the LLM has finished responding. Hard refresh does not fix it.

**Root cause (multiple layers):**

1. **In-memory `activeSess` map goes stale:** When `session.status=busy` SSE event fires, the session is added to `activeSess`. When `session.status=idle` fires, it's removed. If the idle event is missed (SSE disconnect, API pod restart, TCP corruption), the session stays in `activeSess` forever.

2. **Session list reads from stale map:** `GET /sessions` at router.go:1303 called `GetActiveSessions` which reads the in-memory map. A stale "active" entry made the session appear busy in the UI.

3. **SSE tracker not watching:** Read-path handlers (GetHistory, GetSession, ListSessions) never called `adapterEnsureSSEWatch`. Opening a busy session without sending a message never started the SSE tracker, so the idle event was never received.

**Fix (3 parts):**

1. **Ground-truth on read (#792 Pattern 1):** `GetAuthoritativeActiveSessions` queries `/v1/statusz` on the workspace pod for authoritative busy/idle. Zero stale window. Self-heals stale entries as a side effect. Used at both session-list endpoints (router.go:1303 and router.go:445).

2. **SSE watch on all read paths:** `adapterEnsureSSEWatch(wid)` added to GetHistory (line 357), GetSession (line 784), ListSessions (line 58), CreateSession (line 113), and SendPromptAsync (line 220).

3. **In-memory map remains as write-side fast-path:** `activeSess` is still used for `CheckAndAddActiveSession` (session-limit enforcement on write). The Redis variant has a 30-min TTL as a safety net. The ground-truth read path keeps it reconciled.

**What could still go wrong:** If the workspace pod is unreachable (crashed, restarting), `GetAuthoritativeActiveSessions` falls back to the in-memory map. During this window, stale state is possible. This is acceptable — the workspace is not usable anyway.

### 4. Chat history crash on new/empty sessions

**User symptom:** Opening a new session shows "Cannot read properties of null (reading 'filter')" and the chat view is blank.

**Root cause:** `paginateContractHistory(nil, limit, "")` returns `nil`. Go's `json.Marshal(nil)` produces `null` (4 bytes). The frontend calls `.filter()` on the response, which crashes on `null`.

**Fix:** `if page == nil { page = []session.Message{} }` before serialization. `json.Marshal([]session.Message{})` produces `[]`.

---

## Security fixes (PR #757, also on main)

| Issue | Fix |
|---|---|
| #757 TrustedProxies | `router.SetTrustedProxies(cfg.TrustedProxies)` — defaults to trust-nobody |
| #764 SameSite cookies | `c.SetSameSite(http.SameSiteLaxMode)` at all 3 cookie sites |
| #765 Lockout TOCTOU | Atomic `CacheService.Incr` replaces GET→SET |
| #763 Timing leak | Dummy bcrypt burn on recovery-code error paths |
| crypto/rand fallback | `panic` instead of `time.Now().UnixNano()` |

---

## Architectural issue #792 — STALE-FOREVER in-memory state

A systematic audit identified 9 in-memory state items that go stale forever without reconciliation. Issue #792 proposes 3 patterns:

- **Pattern 1 (ground-truth on read):** DONE — `GetAuthoritativeActiveSessions` for `activeSess`
- **Pattern 2 (reconcile on reconnect + periodic GC):** NOT DONE — `sessionTokenSeen`, `sessionCostSeen`, `sessionStartTime`, agentd `statuses`
- **Pattern 3 (wire cleanup hooks + liveness):** NOT DONE — `replay` buffers, `wsOwner`, subscriber slices, `subscriptions`, `connCount`, `sessionParentCache`

**PR #791** attempted Patterns 2 and 3 but was incomplete (review caught dead code, log-only theater, missing cancel paths). Needs rework.

---

## Issues NOT yet fixed (from the audit)

### Affects chat experience:
- **#782** — Frontend `client.ts:95` calls `res.json()` unconditionally. The legacy V2 path at `proxy_v2.go:413` still returns 202 with a JSON body, but if any endpoint returns 202 with an empty body, it crashes. Low risk since SendPromptAsync now returns 200.
- **#783** — 5 backend message types (shell, agent-switch, model-switch, compaction, system) are silently dropped by the frontend message transformer. Affects display of these message types, not core chat.

### Does NOT affect chat:
- #784 (no password-reset UI), #785 (dead endpoints), #787 (workspace list pagination), #788 (RoleConfig data loss), #789 (model selection dropped)
- #786 (aborted/deleted status events dropped — affects session cleanup UI)
- #796 (12 type-shape mismatches — frontend type definitions)

---

## What's deployed vs what's on main

**Production:** v0.13.1 (intentionally rolled back by the user because v0.14.x was broken)
**Main:** All fixes merged. Not yet released or deployed.

**To deploy:** Cut v0.15.0 (major version bump justified by the breaking SendPromptAsync change from 202→200), build images, bump talos-ops-prod. But the user has explicitly said: do not bump until proven working.

---

## PR #791 status (reopened findings)

PR #791 attempted to address reopened sub-findings on #751, #753, #747, #749, #754. The review caught:

1. **#753 F1 — `reconcileStatuses` dead code:** Was defined but never called. Fixed in latest push (call site added to SSE reconnect loop). But `fetchSessionStatuses` queries `/v1/statusz` via opencode's HTTP client, which may not have that endpoint — it's an agentd admin-mux endpoint.
2. **#753 F4 — Error "propagation" is log-only:** Config write errors are logged but not returned. Callers still see exit 0. kubelet won't detect the failure.
3. **#753 F3 — Dedup incomplete:** Immediate-restart path doesn't cancel in-flight deferred restart.
4. **#747 F5 — Wrong log level:** Uses `log.Debug` instead of `log.Warn`.
5. **#754 — Prometheus label:** Uses `workspace_id` (high cardinality) instead of `outcome`.

PR #791 needs rework before merging.

---

## Files changed on main (all PRs + direct pushes)

**Adapter / translator:**
- `pkg/agent/opencode/adapter.go` — streaming GetHistory, conditional CapDiff, SSE watch calls
- `pkg/agent/opencode/translate.go` — ParseHistoryStream, providerID/Agent/Status/Cost/Time/Summary drift fixes, reasoning fallback
- `pkg/agent/opencode/agent_client.go` — removed 10s hard timeout
- `pkg/agent/opencode/session_shape_test.go` — golden fixture tests
- `pkg/agent/opencode/history_regression_test.go` — >16MB regression test
- `pkg/agent/opencode/reasoning_capdiff_timeout_test.go` — reasoning + CapDiff + timeout tests
- `pkg/agent/opencode/model_agent_status_test.go` — provider/agent/status tests

**API handlers:**
- `api/internal/handlers/proxy_handlers.go` — null→[] fix, SSE watch on all read paths, SendPromptAsync→Send, error logging
- `api/internal/handlers/proxy_connections.go` — `GetAuthoritativeActiveSessions` + `fallbackActiveSessions`
- `api/internal/handlers/proxy_adapter_crosscutting.go` — cross-cutting helpers (resolveWorkspace, checkQuota, etc.)
- `api/internal/handlers/proxy_events.go` — persistContext observability, SSE dispatch debug log
- `api/internal/handlers/proxy_v2.go` — TTL pruning, busy-session guard, nil-guard

**Router:**
- `api/internal/server/router.go` — TrustedProxies, GetAuthoritativeActiveSessions at both session-list endpoints
- `api/internal/server/router_frontend_workspace_test.go` — router-level regression test

**Services:**
- `api/internal/services/workspace/workspace_service.go` — context_used backfill from CRD
- `api/internal/services/sse/tracker.go` — billing drift fixes, map cleanup, WaitGroup, IsWatching
- `api/internal/services/sessionindex/service.go` — drop warn log, Prometheus counter

**agentd:**
- `cmd/workspace-agentd/session_tracker.go` — reconcileStatuses, all-non-idle-as-busy
- `cmd/workspace-agentd/client.go` — body cap, debug logs, fetchSessionStatuses
- `cmd/workspace-agentd/mcp_server.go` — path traversal validation, body caps, status checks
- `cmd/workspace-agentd/secrets.go` — cascading restart dedup, config write error logging
- `cmd/workspace-agentd/relay_injector.go` — transient error retry

**Frontend:** (no changes — all fixes are backend-side)

**Config:**
- `opencode.json` — repaired corrupted JSON, removed bash deny-list
- `CHANGELOG.md` — v0.14.3 through v0.14.6 entries (releases were cut but prod not bumped past v0.13.1)

---

## Recommended next steps

1. **Cut v0.15.0** from main, deploy to a staging workspace, and have the user test all four symptoms end-to-end
2. **Fix #782** — make the frontend `client.ts` handle 202/204 gracefully (defense in depth)
3. **Rework PR #791** — properly implement Pattern 2 and 3 from #792
4. **Add contract tests** for the opencode 1.18.10 wire shapes (golden fixtures already captured in `testdata/`)
5. **Consider pinning workspace images to a known-good opencode version** until the platform is stable
