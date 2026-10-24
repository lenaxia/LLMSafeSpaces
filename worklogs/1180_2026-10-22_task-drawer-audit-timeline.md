# Worklog: Task-detail drawer + full audit timeline (#1832 item 4 / PR-D)

**Date:** 2026-10-21 (lane charter date; session ran into 2026-10-22 UTC)
**Session:** PR-D of the #1832 board-UX lane — the task-detail drawer: header info (title, stage, claim/lease, blocked-by with live dep stages, attempts), charter + acceptance criteria with met state, and the FULL audit timeline (stage transitions, claims who/when, review verdicts when present, dependency-graph changes, orphan/unblock), plus the minimal card→drawer open affordance.
**Status:** Complete

---

## Objective

Charter `/workspace/charters-1832/d.md`: surface the audit history the engine already persists (board_events) in a task-detail drawer. FIRST verify what existing read APIs expose; add a read API ONLY if genuinely missing (red-tested if added). No config UI / review automation / UX-batch scope beyond the drawer's own open affordance; no #1831 phrase-id changes; #1824 SSE join untouched.

## Work Completed

### Exposure verification (the charter's FIRST gate)

- Existing reads audited: `GetTask` (row snapshot only — no events), `SearchTasks` (projection, no events), the §4.6 SSE `board.event` fan-out (**post-subscribe commits only** — persist-then-publish, §4.6; it is a live feed, not history). No read API returns a task's board_events history → the read API is genuinely missing, so it is added here, red-first.
- The write-path vocabulary was verified against source, not assumed: `engine.go` `claimOnce` persists action **`claim`** with payload `{task_id, worker, attempts}` (the FIFO engine grant — the timeline's who/when); `TaskStart` persists action **`task_start`** `{task_id, attempts}` (§4.2 manual dispatch, worker stamped at spawn, R9); `move_stage` payloads carry `{from_stage, requested_to, resulting_stage, reclaimed}` (a review-bounce carries PR-B's structured verdict); `add_dependency`/`remove_dependency`/`unblocked_by_delete` ride the affected/orphan task's rows; the reconciler writes system rows (`reclaim`, `drift_*`, `escalate`, `pending_stage`) with deterministic `engine:`/`reconciler:` ids.

### Read API (red-first: `pkg/board/reads_events_test.go` + `api/internal/handlers/board_events_rest_test.go` before the seam)

- `Service.TaskEvents(ctx, taskID, shortIDOrID, limit)` (pkg/board/reads.go): every board_events row scoped `item_kind='task' AND item_id=<uuid>`, **chronological ascending** (newest-N window: default 200, max 500 — heartbeats write a row per call, so the read is bounded), ownership via the same `resolveTask` read perimeter as GetTask.
- Handler `TaskEvents` (api/internal/handlers/board.go) behind the same owner perimeter as `GET /me/tasks/:id` (foreign rows plain 404s, seam never fires); `?limit=N` validated (1–500; unusable → 400, never a silent default); route `GET /api/v1/me/tasks/:id/events` in router.go + the REST test router.
- openapi (sdks/openapi.yaml): `getBoardTaskEvents` with `BoardEventList`/`BoardEvent`/`BoardEventActor` schemas; field-level self-teaching descriptions (the action vocabulary, payload shapes, the newest-N window, the SSE-is-not-history note).

### Frontend drawer (red-first: TaskDrawer.test.tsx + BoardPage.drawer.test.tsx before the component)

- `frontend/src/api/board.ts`: `BoardAuditEvent` (+ `BoardVerdict`/`BoardVerdictFinding`) types and `boardApi.taskEvents(id, limit=200)`.
- `TaskDrawer.tsx` (new): Radix right-side drawer mirroring EpicDrawer's shape — header (title, exact short id, stage label, attempts, claim/lease with assignee + lease-until, blocked-by rows with live `data-dep-stage` chips), charter (explicit empty state) + criteria with `data-met`, and the timeline: per-action payload arms (claim → worker+attempt; task_start; move_stage → from→resulting(+reclaimed); add/remove_dependency; unblocked_by_delete → the deleted dependency; delete_task → orphan count), structured verdict view (`data-finding`/`data-severity`/`data-criterion-index`) when one rides a move payload, loading/empty/error+retry states (error renders in place, board stays usable).
- `TaskCard.tsx`: the drawer's open affordance — an ⓘ details button (`details-<short_id>`, stopPropagation) so the card's primary click keeps its PR-C edit-dialog behavior.
- `BoardPage.tsx`: drawer state + `onCardDetails` wiring through both StageCell call sites; `onEdit` hands the task to the existing edit dialog.

### Predecessor-WIP continuation (lease-lapse handoff)

This session continued substantial uncommitted WIP from a lapsed predecessor. Continued deliberately, not clobbered. Fixes to the inherited pins (intent preserved, seams corrected against the real write path):
- testify suite shadowing: the service tests' local `t` (*board.Task) shadowed TestingT → `s.T()` on package-level asserts, no TestingT on suite methods (testify v1.11 Assertions style).
- **claim seam**: the pins drove `TaskStart` but asserted action `claim`; the engine's `ClaimNext` (claimOnce) is what persists `claim` (payload `worker`). Tests now drive the FIFO path (`claimOrFail`) — dep granted first (FIFO by creation), then the watched task.
- `BoardPage.drawer.test.tsx` line read `.textContent` off the `findByTestId` promise → awaited element first.

## Key Decisions

1. **Read API added** — justified by the exposure audit (SSE is post-subscribe only; no REST read carried history). Red-tested at both layers (service integration + REST handler), per the charter's only-if-missing gate.
2. **Timeline order is chronological ascending, window keeps the NEWEST rows** — a drawer renders history top-to-bottom; the window bounds heartbeat-noisy long-lived claims without flipping order.
3. **Drawer asks for the default window (200) explicitly** — deterministic, matches the service default, keeps `taskEvents(id, limit)` honest for future callers.
4. **Deterministic UTC HH:MM for the timeline's when-column** (`toISOString().slice(11,16)`) — timezone-stable renders across hosts.
5. **Card's primary click unchanged** — the drawer opens ONLY from the ⓘ affordance (PR-C scope untouched; pinned by a test).
6. **No e2e/nightly job added** — coverage is component+handler+service level; no PORTFWD_PORT registry entry needed (the us72/nightly port registry is untouched).

## Assumptions (stated — per Rule 7)

- A1 board_events rows are the audit source of truth and task-scoped rows carry the full timeline → validated by reading engine.go's recordEventTx call sites + the integration test's real-write-path timeline `[create_task claim move_stage move_stage add_dependency unblocked_by_delete]`.
- A2 the §4.6 SSE projection is post-subscribe-only (not history) → validated against the publisher's persist-then-publish design notes and the openapi description the lane already carries.
- A3 verdict payloads ride review-bounce `move_stage` rows when present → validated from PR-B's payload shape (rendered defensively — absent verdict ⇒ plain move row).
- A4 deps are within-project (R4) so `projectTasks` resolves live dep stages → same assumption TaskCard's blocked-by chips already make.

## Blockers

None. Environmental notes: fleet memory sat near/over the 7.2e9 gate for much of the session (three concurrent lane workers); verification loops were filtered/board-scoped per the orchestrator's stagger directive. Full-suite sweeps ride CI.

## Tests Run

- `go test -count=1 -run 'TestBoardREST_TaskEvents' ./api/internal/handlers/` — ok (4 new REST pins: render/order/payload-threading, limit 400s, owner-scoped 404 + seam-never-fires, error class).
- `go test -tags=integration -count=1 -run 'TestBoardSuite/TestReads_TaskEvents' ./pkg/board/ -v` — PASS both (real write path; scoping, no cross-task bleed, newest-N window, chronological order).
- `cd frontend && npx vitest run src/components/board/TaskDrawer.test.tsx src/pages/BoardPage.drawer.test.tsx` — 15/15 (11 drawer pins incl. verdict/empty/error-retry/no-task, 4 page wiring pins incl. edit-dialog-untouched + close-restores-board).
- Board blast-radius sweep: `npx vitest run src/pages/BoardPage.actions.test.tsx src/pages/BoardPage.drag.test.tsx src/pages/BoardPage.scope.test.tsx src/pages/BoardPage.sse.test.tsx src/pages/BoardPage.test.tsx src/pages/BoardPage.validation.test.tsx` — green (pre-existing suites unaffected by the drawer wiring).
- `npx tsc --noEmit` — clean. `npx eslint` on changed frontend files — clean.
- `go test -run TestOpenAPIRouterContract ./api/internal/server/` — ok (route registered in the contract).
- `cd sdks && go run ./validate/main.go openapi.yaml` — ok.
- `make repolint` — pass (worklog sequence/migrations/chart drift).

## Next Steps

- Reviewer: mutation-verify suggestions — (a) flip the `ORDER BY created_at DESC, id DESC`→ASC in TaskEvents → window/order pins go red; (b) drop the `ownerTask` guard in the handler → owner-scope 404 pin goes red; (c) remove `data-finding` rendering in TaskDrawer → the verdict pin goes red.
- PR-B's verdict payload (structured findings) already renders; if its field names shift on merge, only VerdictView needs touching.

## Files Modified

- pkg/board/reads.go, pkg/board/reads_events_test.go (new)
- api/internal/handlers/board.go, board_rest_test.go, board_events_rest_test.go (new); api/internal/server/router.go
- sdks/openapi.yaml
- frontend/src/api/board.ts; frontend/src/components/board/TaskDrawer.tsx (new), TaskDrawer.test.tsx (new), TaskCard.tsx; frontend/src/pages/BoardPage.tsx, BoardPage.drawer.test.tsx (new), boardTestUtils.tsx
- CHANGELOG.md; worklogs/1180_2026-10-22_task-drawer-audit-timeline.md (this file)

## R2 (2026-10-22, post-ruling): rebase + construction-parity mirror

Rebased onto the post-#1840/#1833 main tip. The #1747-hardened parity
suite made a REST-only timeline inadmissible — every board surface
lives on every surface — so this round adds the external MCP mirror:
`board_task_events` (read-only; task_id + limit) on pkg/mcp with the
client seam `BoardTaskEvents`, matrix row + replay/audit entries in
board_parity_test.go, tool-list and integration counts updated.
Interface adaptations from the rebase: ListTasks' failed param through
handlers/reads/router/board.ts, #1823's failed fields in the drawer
test fixture. Gates: handlers + pkg/mcp suites green (incl. the three
forcing suites), sweep 2394/2394, tsc clean, eslint 0 errors, repolint
green (worklog 1176 — 1175 held by C1's branch), board e2e 12/12.
