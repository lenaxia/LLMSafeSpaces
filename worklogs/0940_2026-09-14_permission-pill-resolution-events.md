# Worklog: #1365 permission pills never dismiss — reply paths emit input-resolved events

**Date:** 2026-09-14
**Session:** Fix issue #1365 (P1): permission/question pills never dismiss because the new reply paths (resolve-by-absence 200, inbox late-answer 202) published no input-resolved event; add frontend optimistic-clear hardening, pill-stack ordering, and SSE liveness watchdog.
**Status:** Complete

---

## Objective

Close all three defects of #1365:

1. Missing resolution event on the new reply paths (backend).
2. Stale pill shadowing the live ask; frontend requiring the event instead of treating 2xx as clear authority (frontend).
3. Silently-dead SSE freezing the pending set indefinitely (frontend liveness).

---

## Work Completed

### Backend — one publish seam, unconditional events (#1365 change item 1)

- `api/internal/handlers/proxy_inbox.go`: replaced `publishInboxResolved` (record-shaped, owner-gated) with `publishInputResolved(c, workspaceID, sessionID, requestID, kind, reason)` — the single publish seam for every API-originated input resolution. Publishes to the workspace owner AND the authenticated clicker (deduped); a stale `wsOwner` map on a fresh replica can no longer silently drop the event. Warns when no target exists; Debug-logs each publish.
- `api/internal/handlers/proxy_input.go`: `resolveInboxOnProxySuccess` now publishes UNCONDITIONALLY on reply-path success. Previously it returned early when `LookupPending` found no pending record — exactly the incident (dead ask, no record, 200 answered, zero events, immortal pill). The record's kind/session win when present; without a record the kind derives from the handler-validated ID prefix (`que_`/`per_`) and the session from `inputRequestSession` (terminus) or the record (adapter).
- `api/internal/handlers/proxy_actions.go`: `dispositionInboxOnAction` (typed actions / MCP-SDK replies) routed through the same seam; no longer returns early on `h.inbox == nil` or a missing record.
- All six REST call sites updated (`QuestionReply`/`QuestionReject`/`PermissionReply` × terminus/adapter) plus `lateAnswerInboxAsk` (fresh + duplicate 202 paths) and `DismissInboxRecord`.

### Frontend — optimistic clear hardening, ordering, liveness (#1365 change items 2–3)

- `frontend/src/providers/SessionActivityProvider.tsx`:
  - Resolved-tombstones (`resolvedTombstonesRef`, FIFO-capped 1000): every `removePendingAction` (resolved event, optimistic 2xx clear, fold-sync drop) tombstones the request ID; `addPendingAction`, `addPendingQuestion`/`addPendingPermission`, and the D10 snapshot-commit staging loop all refuse to re-add a tombstoned ID. This closes the resurrection race (flight fetched pre-click, committed post-clear) and the whileAway re-presentation of a resolved ask. IDs are unique per ask, so a tombstone can never block a genuinely new ask.
  - First-seen timestamps (`requestFirstSeenRef`): `pendingQuestionsForSession` / `pendingPermissionsForSession` sort newest-activity-first — the live ask leads the stack, a stale pill cannot shadow it.
- `frontend/src/lib/sseConnection.ts`: `onKeepalive` callback fired on heartbeat comment frames (`:\n\n`) — byte-level liveness without a data event.
- `frontend/src/hooks/useUserEventStream.ts`: liveness watchdog — `onEvent`/`onKeepalive`/`onConnect` refresh `lastAlive`; a 10s interval (plus a `visibilitychange` listener for resumed suspended tabs) forces `conn.reconnect()` after 60s of event silence even when bytes still flow (half-open proxy / stuck fetch loop the 35s read-timeout cannot see). The reconnect re-runs the server-side per-workspace snapshot flights (`snapshotUserWorkspaces` → `emitPendingInputRequests`), so a frozen pending set converges without a manual refresh. `touchAlive()` after a forced reconnect bounds the watchdog to at most one forced reconnect per 60s (no hot loop, no backoff reset storm).

### Tests

- Backend (`api/internal/handlers/proxy_input_resolved_test.go`, new): absence-resolve with no record publishes permission AND question events (the exact incident shape), reject-no-record publishes dismissed, adapter-path-no-record publishes, owner-unknown publishes to the authenticated clicker's stream, second-click idempotency (2xx + exactly one event per click, no extras), typed-action no-record publishes.
- `proxy_question_e2e_test.go`: round-trip now pins the API-side resolved event (step 3.5) before the harness-side bridge event.
- Frontend vitest: tombstone vs re-presented live event; whileAway re-presentation of a resolved ID ignored; optimistic 2xx clear survives a racing flight commit; newest-first stack order (system-time stepped per add); sseConnection keepalive frames; watchdog reconnects on semantic silence (garbage bytes, no events) and does NOT flap while heartbeats flow.
- Playwright (`tests/e2e/input-requests.spec.ts`): pill clears on 200 and on 202 with NO resolved event and NO fold change (pure optimistic authority); resolved pill not resurrected by a stale re-snapshot still carrying the dead ask.

---

## Key Decisions

1. **Publish unconditionally, resolve the record when present.** The record is an accelerator (kind/session enrichment), not a precondition for the event. S6's completion invariant makes the event leg part of the path; a record-gated publish is exactly how the incident's pill became immortal.
2. **Owner + authenticated clicker as publish targets.** `WorkspaceOwner` comes from the K8s informer watch (`RecordWorkspaceOwner`); a fresh replica whose informer has not caught up returns "" and the old seam silently dropped the event. The clicker is authenticated on every reply route (`c.Get("userID")`, auth middleware), so the event reaches at least the user who acted. Duplicated only when distinct.
3. **Tombstones instead of unstage-on-reply.** The optimistic clear (component `onResolved`) does not know about open snapshot flights; gating every re-add path on the tombstone set is the single choke point and also covers whileAway re-presentations and stale fold re-adds.
4. **Watchdog window 60s (heartbeat 25s, read-timeout 35s).** Two heartbeat intervals + margin; the watchdog complements the byte-level read timeout, which cannot see a connection that still delivers bytes but no events/heartbeats. Forced reconnects are rate-limited to one per window by touching `lastAlive` on reconnect.
5. **Live-ask double-publish retained.** A live answer may publish API-side and again via the usagestream bridge when the harness emits INPUT_RESOLVED — pre-existing, documented, idempotent client-side (removal keys on request ID).

## Assumptions Stated and Validated (Rule 7)

- *"Request IDs are unique per ask"* — validated from the ID patterns (`que_[a-zA-Z0-9]+`, `per_...`) and the inbox dedupe design (#1313 S2 ask-scoped markers). Underpins tombstone safety. If a harness ever reused IDs, tombstones would need eviction on session clear.
- *"`userID` gin context key is set by auth middleware on reply routes"* — validated: `api/internal/middleware/auth.go:120` (`c.Set("userID", userID)`); consumed identically in `StreamUserEvents`.
- *"The server runs snapshot flights on user-stream connect"* — validated: `stream_user_events.go` `snapshotUserWorkspaces` → `emitPendingInputRequests` per Active workspace; so a watchdog reconnect re-runs flights without any new endpoint.
- *"Heartbeats are bare `:` comment frames"* — validated: `stream_user_events.go:142` writes `":\n\n"`; `sseConnection` treats `block.trim() === ":"` as keepalive.
- *"Production v0.30.0 == repo HEAD"* — validated via `git describe --tags HEAD` = v0.30.0; the code read is the code that served the incident.

## Adversarial self-review (Rule 11) — findings

- **LookupPending transient error leaves the record pending while the event publishes.** Real but pre-existing class (the resolve was already best-effort with a Warn); self-heals via whileAway re-presentation → idempotent 202 re-click; the frontend tombstone suppresses the UX wart. Not fixed here to avoid speculative retry machinery.
- **Owner+clicker double publish appends two replay-buffer entries when distinct users.** Correct behaviour (two distinct streams), one entry each.
- **Watchdog could reset reconnect backoff during an outage.** Bounded: forced reconnect touches `lastAlive`, so at most one forced reconnect per 60s. Accepted.
- **`dispositionInboxOnAction` kind derivation on unvalidated client inputID** degrades to `question.resolved` for malformed IDs — same degrade as the cross-replica usageBridge kind map; frontend removes by request ID.

---

## Blockers

None.

---

## Tests Run

- `go test -timeout 600s ./api/internal/handlers/` — ok (67s).
- `go test -timeout 900s ./api/...` — ok.
- `go build ./...` — ok; `go vet ./api/...` clean; `gofmt -l` clean; staticcheck clean (golangci-lint binary unavailable in this environment — pre-commit hook will run it in CI).
- `npx vitest run` (frontend) — 1820 passed.
- `npm run typecheck` — clean; `npm run lint` — 0 errors (6 pre-existing warnings in generated `src/abi/**/*_pb.ts` files, untouched by this change).
- `npx playwright test tests/e2e/input-requests.spec.ts` — 11 passed (8 pre-existing + 3 new).
- TDD discipline: the seven backend tests in `proxy_input_resolved_test.go` were written first and confirmed failing (event timeouts) before implementation.

---

## Next Steps

1. Deploy and verify on production: the `input resolved event published` Debug log (or the metric/log level bump if Debug is filtered) should appear for every reply click; re-run the incident scenario (dead pill + live ask).
2. Consider a delivered-events counter for `agent.*.resolved` on the user broker (mirroring `#901`'s `llmsafespaces_sse_broker_delivered_events_total`) so "zero events" is observable without log-level changes.
3. Optional follow-up: `usageBridge.InputResolved` cross-replica kind degrade could consult the inbox record (authoritative kind) instead of the per-replica memory map — small, but touches the consumer path and was not required by #1365.

---

## Files Modified

- `api/internal/handlers/proxy_inbox.go` (publish seam; late-answer + dismiss call sites)
- `api/internal/handlers/proxy_input.go` (resolveInboxOnProxySuccess unconditional publish; 6 call sites)
- `api/internal/handlers/proxy_actions.go` (disposition routed through the seam)
- `api/internal/handlers/proxy_question_e2e_test.go` (API-side event pinned)
- `api/internal/handlers/proxy_input_resolved_test.go` (new — 7 tests)
- `frontend/src/providers/SessionActivityProvider.tsx` (tombstones, first-seen ordering, commit-loop gate)
- `frontend/src/providers/SessionActivityProvider.test.tsx` (4 new tests)
- `frontend/src/lib/sseConnection.ts` (onKeepalive)
- `frontend/src/lib/sseConnection.test.ts` (2 new tests)
- `frontend/src/hooks/useUserEventStream.ts` (liveness watchdog)
- `frontend/src/hooks/useUserEventStream.test.tsx` (2 new tests)
- `frontend/tests/e2e/input-requests.spec.ts` (3 new Playwright tests)
- `worklogs/0940_2026-09-14_permission-pill-resolution-events.md` (this file)

---

## Review Iteration (r1 — automated reviewer CHANGES_REQUESTED, same session)

### Findings addressed

1. **Dual-target/dedupe branches untested (blocking).** Added `TestInputResolved_DualTarget_OwnerAndClickerEachGetOneEvent` (owner=user-1 + clicker=user-77 → exactly one event per stream) and `..._OwnerEqualsClicker_DedupesToOneEvent` (single publish; an inverted dedupe would now fail a test).
2. **Both-targets-absent path untested.** Added `..._NoTargets_WarnsAndPublishesNothing` — degrades to the logged warn, no panic, reply still 200.
3. **LookupPending transient error untested.** Added `..._LookupPendingError_StillPublishes_RecordUntouched` — miniredis `SetError` forces the miss; the event still publishes with caller-derived kind/session; the record stays pending (enrichment is best-effort, resolution is not silently dropped).
4. **Negative control restored.** Added `..._ActFailure_NoEventRecordUntouched` — stub Act `not_found` → 4xx, zero events, record untouched. Pins the no-publish-on-failure invariant.
5. **False-tombstone vector (robustness r1.1) — fixed by restriction, not re-arm.** Tombstones are now RESOLUTION EVIDENCE ONLY (resolved event, optimistic 2xx clear, prompt dismissal). Absence evidence never tombstones: the snapshot-commit doom loop no longer tombstones, and the ChatPage fold-sync removal now uses a new non-tombstoning `dropPendingAction` (absence-evidence sibling of `removePendingAction`). Rationale: the incident itself proved projections go stale — a successful-but-incomplete flight would doom a live ask, and a no-TTL tombstone would hide it for the tab's lifetime. Post-#1365 every reply path emits the resolved event, so a ghost pill left by absence is clickable-and-clearing; hiding a live ask is the strictly worse failure. Regression pinned in vitest: flight 1 ok-omitting → flight 2 re-carrying → re-added.
6. **Bulk-clear inconsistency (robustness r1.2) — resolved by the same restriction.** Bulk clears (`clearWorkspacePendingActions`, `clearSessionPendingPrompts`) are lifecycle, not resolution: they must NOT tombstone, and whileAway re-presentation after a bulk clear re-adds — now pinned as intended semantics with a test and comments.
7. **Session fidelity in the typed-action path (minor).** `dispositionInboxOnAction` now receives the route's authoritative `:sessionId`; record-less typed-action replies carry a real `session_id` instead of `""`.
8. **Dead guard removed (minor).** `resolveInboxOnProxySuccess`'s `c.Writer.Status() >= 400` check was unreachable at every call site (all callers return before it on failure) — deleted.
9. **Undelivered issue test-plan rows — delivered.**
   - *Dead pill + live ask in one view*: Playwright `pill-lifecycle-1365.spec.ts` renders both asks (no shadowing) and converges to the live ask after refresh.
   - *Stale-stream kill → flight re-run → convergence*: Playwright kills the user stream server-side (heartbeat then FIN); the browser heals via reconnect, the re-run flight's whileAway union re-presents the ask, and the pill renders WITHOUT reload — then clears on the 202 reply.
   - *Unhappy e2e row*: 5xx reply keeps the pill actionable and surfaces the inline error.
10. **Count correction.** The PR body claimed "+10" vitest tests; the r0 diff added **8** (4 provider + 2 hook + 2 sse). r1 adds 3 more provider tests → **11** total across the three files.

### Tests Run (r1)

- `go test -run 'TestInputResolved' ./api/internal/handlers/` — 12/12 (7 from r0 + 5 new).
- `go test -race -count=3 -run 'TestMCPCompact' ./cmd/workspace-agentd/` and `-count=5 -run 'TestSweeperE2E' ./api/internal/handlers/` — flake fixes verified.
- Frontend vitest: provider file 83/83 (3 new r1 tests + the r0 four); full suite re-run below.
- Playwright: `pill-lifecycle-1365.spec.ts` 3/3; `input-requests.spec.ts` 11/11.

### Files Modified (r1)

- `api/internal/handlers/proxy_actions.go`, `proxy_input.go` (fidelity + dead-guard removal)
- `api/internal/handlers/proxy_input_act_test.go` (expose miniredis via env)
- `api/internal/handlers/proxy_input_resolved_test.go` (+5 tests)
- `api/internal/handlers/outbox_sweeper_test.go` (CI flake: wait for the delivered hook)
- `cmd/workspace-agentd/mcp_tools_test.go` (CI race: wait for the detached compact goroutine)
- `frontend/src/providers/SessionActivityProvider.tsx` (restricted tombstones; dropPendingAction)
- `frontend/src/pages/ChatPage.tsx` (fold-sync uses dropPendingAction)
- `frontend/src/providers/SessionActivityProvider.test.tsx` (+3 tests)
- `frontend/tests/e2e/pill-lifecycle-1365.spec.ts` (new — 3 tests)

### Note (r1 push)

The two synchronize pushes after r0 (a4eb1d05, 6ede7fb8) produced no
workflow runs on GitHub Actions (zero runs by head_sha; other branches
kept running) — the events appear to have been dropped/queued out. This
commit re-triggers the synchronize wave; if it also fails to materialize,
close/reopen fires `reopened` (covered by ci.yml's unfiltered
pull_request trigger) though not pr-review.yml's [opened, synchronize].

### Outcome (r1 close)

- Automated reviewer verdict on `ab676e52`: **APPROVE** (all findings
  addressed; red-first reproduction independently verified against main;
  two non-blocking follow-up notes: flag-off adapter-path reject
  disposition maps to answered — pre-existing on main — and record-less
  adapter publishes carry `session_id: ""`).
- CI: the pull_request webhook path stayed dead for this branch (three
  synchronize pushes + a fresh PR produced zero runs while issue_comment
  events flowed); CI was run via the sanctioned `workflow_dispatch` path
  on `ab676e52` — **success** (lint, test, test-full -race,
  frontend-test, abi-schema).
- PR #1369 (supersedes #1368, same branch/commits) — approved and
  CI-green; left unmerged pending the user's call.
