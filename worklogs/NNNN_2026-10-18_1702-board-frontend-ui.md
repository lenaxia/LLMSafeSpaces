# Worklog: #1702 — Board frontend UI: kanban/swimlanes, drag, scope switcher, SSE feed

**Date:** 2026-10-18 (r1: 2026-10-18/19)
**Session:** Lane 4 of the Task Board (design 0067 §7): the BoardPage frontend projection over the #1699 REST surface — kanban/swimlanes with server-arbitrated drag, the project/epic scope switcher, the SSE live feed on the #1687-hardened user stream, and the create/edit surfaces incl. OR4 pr:N tag validation.
**Status:** Complete — PR #1738 r1 remediation pushed, orchestrator merges on APPROVED

---

## Objective

Build `frontend/src/pages/BoardPage.tsx` per design 0067 §4.8 (normative UI sections): kanban columns = stages with Pending Input + Cancelled as collapsible side columns; swimlanes = projects; drag = `move_stage` optimistic + server-arbitrated snap-back with the engine's 409 guard codes rendered honestly; the scope switcher (project focus + epic chips + tag filters + search with did-you-mean); SSE live updates (board rows update as the engine's events stream, reseed-on-reconnect); create/edit surfaces for project/epic/task with client-side OR4 `^pr:[1-9][0-9]*$` validation. Component tests for drag/switcher/validation/SSE + e2e rows in the established Playwright patterns.

---

## Work Completed

### `feat(board)` — the UI

- `frontend/src/api/board.ts` — the board REST client: the engine's canonical snake_case row shapes verbatim (BoardProject/BoardEpic/BoardTask/BoardClaim + the §4.6 BoardEventPayload), all 23 operations, requestId per logical operation (`newBoardRequestId`).
- `frontend/src/lib/boardRules.ts` — the rule mirrors (hints, never arbiters): stage vocabulary + the manager-surface view of stage.go's legal transition map (in_progress sources are claim-token-guarded → not offered as drag affordances; the engine still arbitrates every drop), the OR4 anchor pattern + `validateBoardTags`, the Done-readiness mirror (vacuous-truth on zero criteria, per store.go `criteriaAllMet`), bounded-Levenshtein did-you-mean (0066 shape, ≤2, top-3), guard-code → message map, `describeBoardError` (404+suggestions / 409+code).
- `frontend/src/components/board/TaskCard.tsx` — §4.8's card: mono short ID, priority chip, tags, the structural blocked badge (🔗 N expanding to the blocked-by view with live dependency stage chips, ruling R3 — no evaluator), the assignee busy indicator (BusyIndicator on in_progress + assignee_session), attempts counter, pending-origin badge, the OR4 anchor chip.
- `frontend/src/components/board/TaskDialog.tsx` — create/edit: title/description (charter seed)/priority/TagInput with OR4 validation blocking submit, acceptance-criteria editor with reviewer check-off + Done-readiness preview (explicitly NOT a client-side veto — the server's done_guard still arbitrates), dependency add/remove (the only blocking mechanism, R3/R4) rendering the 409+code+suggestions and 404+suggestions shapes with clickable suggestion chips.
- `frontend/src/components/board/EpicDrawer.tsx` — outcome front and center, DERIVED progress (`progress_done/progress_total`, "—" when not derived — never 0/0), activate/pause, worker budget (slots in use), New task.
- `frontend/src/components/board/BoardErrorNotice.tsx` — the shared error renderer: guard code chip + suggestion chips.
- `frontend/src/pages/BoardPage.tsx` — the kanban: swimlanes = projects (name, short id, slots-in-use, archived + slot-cooldown badges), stage columns, drag-and-drop with optimistic overlay + snap-back; → done drops open the evidence-URL prompt (required on done, R2) with the criteria preview; scope switcher (project select + epic chips + tag chips + search with the did-you-mean miss panel); create-project (workspace select, R8 single target), create-epic, create-task dialogs; 30s `refetchInterval` on the tasks query as the periodic-reconciliation belt.
- `frontend/src/router.tsx` + `Sidebar.tsx` — the `/board` route + nav entry beside Workflows/Triggers.
- `frontend/src/api/types.ts` — `ApiError.suggestions?: string[]` (additive).

### SSE feed (§4.6 — reuse, not reinvent)

- `useUserEventStream` grows the `board.event` branch (invalidate `["board"]`) and the reconnect-reseed invalidation — the #1687/#1646-hardened connection (liveness watchdog, backoff, Last-Event-ID factory) is the machinery; no second connection is opened. The board page consumes the invalidations through react-query (snapshot refetch = the §4.6 reseed). The backend `board.event` publisher is an engine-lane gap (see Blockers) — the frontend feed + hardening landed against the designed payload shape and is driven end-to-end in tests.

### `test(board)` — pins

- `boardRules.test.ts` (34): OR4 accept/reject tables (pr:1 ok; pr:0/pr:01/PR:12/pr:abc rejected; free-form untouched), stage-vocabulary + manager-drag map arms, done-readiness, did-you-mean (near-miss, title, truncated-prefix, cap-3, far-miss), error-shape mirrors.
- `board.test.ts` (10): wire encoding (snake_case verbatim, `{projects}`/`{tasks}` envelopes, move-stage body, requestId threading, defensive id encoding).
- `BoardPage.test.tsx` (12): swimlanes/columns/cards, busy indicator only on claimed rows, blocked badge + live dep chips, OR4 chip, collapsible side columns, pending-origin badge, empty state, attempts.
- `BoardPage.drag.test.tsx` (9): optimistic survival under a deferred server answer, confirm-on-authoritative-row, snap-back + code render for done_guard / stage_transition / claim_token, evidence-URL prompt (threaded, disabled until present, criteria preview without veto), side-column drop, no-op drop ignored.
- `BoardPage.scope.test.tsx` (7): project focus (single-project fetch), epic chips + All epics, derived chips, tag filter, search (id/title/tag), did-you-mean panel + click-to-apply, far-miss.
- `BoardPage.validation.test.tsx` (13): OR4 arms on the task editor (submit blocked), project-create OR4 + submit shape, epic creation, task creation via the epic drawer, dependency 409/404 suggestion rendering + click-to-fill, title-required.
- `BoardPage.sse.test.tsx` (3, integration): the REAL useUserEventStream + REAL BoardPage on one QueryClient — a streamed board.event moves the card; stream close → reconnect → reseed picks up a new project; non-board events leave the board untouched.
- `useUserEventStream.board.test.tsx` (3): board.event invalidation, reconnect reseed, foreign-event isolation.
- `tests/e2e/board.spec.ts` (6 rows): render (swimlanes/columns/cards/blocked/slots), drag-to-move via move-stage, refused done-guard drag (evidence prompt → 409 → snap-back + code), SSE live move via the fetch monkey-patch emitter (chat-scroll-tail pattern), scope switcher (focus/chips/did-you-mean), OR4 validation blocking save.

### `docs` — this worklog + CHANGELOG `[Unreleased]`.

---

## r1 (CHANGES_REQUESTED remediation, one commit set)

The verdict's pre-specified remedies, all landed:

1. **Lane-4 e2e arms** — `board.spec.ts` grows the design-named cancel arm (terminal write → dialog closes → card lands the collapsible Cancelled side column) and reclaim arm (streamed `board.event` action=reclaim converges an in_progress row to Backlog), plus the start arm (task_start → claim notice + in_progress). 9 rows total.
2. **§4.8 reconciliation** — (a) the epic drawer's SSE activity feed is BUILT: `lib/boardFeed.ts` (an in-page pub/sub carrying board.event payloads from the user stream's board branch — one SSE connection remains the only transport) feeds the drawer filtered to the epic's tasks, action-label rendered, integration-tested incl. the foreign-epic filter; (b) the attempt counter gains its escalation preview (⚠ "escalates next claim" at attempts ≥ the R6 documented default 3 — the configured N is a deploy knob the API does not surface; the title says so, the engine stays the authority); (c) health panel remains DECLARED-blocked on the missing `board_read(health)` seam.
3. **task_start wired** — backlog cards carry a Start affordance (the designed UI exit; backlog stage moves are not machine-enumerated); the Claim renders honestly (escalated → "escalated to Pending Input" notice; dispatched → notice); failures render at page level with the guard code.
4. **The swallowed error** — `lifecycleMutation` (epic activate/pause) grows `onError` → renders inside the drawer (epic_state 409 pinned); `openEpicDrawer` failures render at page level (they previously landed in a dialog-scoped error state that was invisible with no dialog open); `submitTask`/`onCancel`/dependency-add rejections are caught locally (dialog stays open, fields intact — no unhandled rejections) and the failed dependency id is now KEPT in the input.
5. **The dead mirror wired** — `isManagerDragAllowed` now drives the drag AFFORDANCE: cells the manager-surface map does not enumerate for the dragged source render non-landing (no primary highlight, struck-through header, `data-droppable="false"`), while the drop itself still attempts and renders the engine's honest 409 (the server stays the single arbiter — unchanged, tested).
6. **Coverage for the two untouched paths** — `BoardPage.actions.test.tsx` (15): cancel happy (terminal + invalidation + side-column placement) and unhappy (409 code + dialog stays open); epic activate AND pause (status flip) and the failed-activate drawer notice; task_start dispatched/escalated/error; escalation preview shown/hidden; failed save keeps the dialog open with the notice; dependency failure preserves the input; concurrent drags (per-slot optimistic map — a second drag no longer visually snaps the first); the drawer's activity feed filter; getEpic 404 at page level. The reclaim convergence pin lives in `BoardPage.sse.test.tsx` (real hook + page: streamed reclaim event → row converges to Backlog).
7. **Concurrency fix** — optimistic state is a per-short-id map (r1 correctness finding): concurrent drags are independent slots.
8. **PR body corrected** — the two overclaims the verdict flagged are gone: the drag-affordance sentence now matches the wired implementation, and "Closes #1702" narrowed to an Addresses claim with the health-panel residue explicit.

r1 verification: full vitest **2301/2301** (one flaky ChatPage timing test failed on ONE of three full runs — pre-existing load sensitivity, zero failures on the other two incl. the JSON-reporter run); tsc clean; eslint 0 errors (6 pre-existing generated-file warnings); Playwright `board.spec.ts` **9/9**.

## Key Decisions

- **Scope switcher = project/epic focus, NOT Personal|Org** — R7 deleted the org model and its Personal|Org switcher (design §2c/§4.8 "no scope switcher — user-level only"). The charter's "scope switcher (project/epic focus)" is the board's focus controls: project select + epic chips + tag chips + search.
- **Epics derived from task rows** — there is no ListEpics REST seam (#1699 stopped it on the engine gap: epic listing/search/health). Display inheritance (R2) materializes epic_id/epic_name on every task row, so epic chips, the epic-picker for task creation, and the drawer (GetEpic by UUID for derived progress) are honest over live data; a session-local knownEpics cache holds created/opened epics. Bridge documented here; the engine follow-up shrinks it.
- **Health panel omitted** — §4.8's health panel is sourced from `board_read(health)`, a seam #1699 stopped on the same engine gap. Not worked around (STOP-item discipline); swimlane headers surface what rows DO carry (slots in use, slot-cooldown badge) without pretending to be the panel.
- **Drag affordances mirror stage.go's manager view; the server stays the single arbiter** — in_progress-sourced moves are claim-token-guarded for the REST actor (board.go stamps no token by design) so they are not offered, but ANY drop still attempts: unenumerated moves render the engine's stage_transition 409 verbatim (tested). Backlog rows leave via the engine's claim (task_start), not a drag.
- **Done drops pre-collect the evidence URL and preview unmet criteria WITHOUT vetoing** — the prompt shows the done_guard consequence and still sends the move; the engine's 409 renders honestly. A client-side veto would hide the guard the charter demands surfacing.
- **Criteria check-off sends the full replacement array** (PATCH semantics: acceptanceCriteria replaces when present) — this allows un-check for review correction, equivalent to setCriteriaMet for check-on; the engine supports both.
- **SSE: extend the root user stream, don't open a second connection** — `board.event` invalidates `["board"]`; the reconnect path reseeds. The §4.6 "periodic version-stamp check" has no board version seam; a 30s tasks-query refetchInterval (the reconciler's default sweep cadence) is the documented stand-in.
- **BoardPage.sse.test / useUserEventStream.board.test drive the real sseConnection parser** through mocked fetch streams — the #1687 pin class (SSE-death reseed, event application), not a mock of the hook.
- **e2e SSE transport** uses the established fetch monkey-patch with per-connection emitters (`window.__user`) — route.fulfill cannot stream (the limitation documented in chat-scroll-tail.spec.ts / session-activity.spec.ts).

---

## Blockers

- **Engine-lane gaps inherited from #1699 (flagged, not worked around):** (1) no ListEpics seam → epic list derived from task rows (see Key Decisions); (2) no board search seam → search filters loaded rows client-side with the 0066-shape did-you-mean over them; (3) no board health seam → health panel omitted.
- **Backend `board.event` SSE publisher does not exist yet** (design §4.6 says board events fan out on the user+workspace channels; nothing in pkg/board or the handlers publishes them today — Lane 1/2 landed persistence + audit only). The frontend feed consumes the designed event shape and its hardening (reseed/reconcile) is real; live production updates begin when the engine lane publishes. Honest limitation, stated on the PR body.
- Environmental: the 15G PVC sat at 90-94% through the lane; no other lanes' worktrees touched, only my own caches managed (playwright chromium installed under my own ~/.cache).

---

## Tests Run

- `npx vitest run` (frontend, full): r0 **205 files / 2285 tests passed** (91 new); r1 **206 files / 2301 tests passed** (107 new across 9 files) — see the r1 note for the one flake.
- `npx tsc --noEmit`: clean.
- `npx eslint .`: 0 errors (6 pre-existing warnings in generated `src/abi/**/*_pb.ts` — untouched).
- `npx playwright test tests/e2e/board.spec.ts`: r0 **6/6 passed** (after one selector fix; re-run 6/6 in 14.1s); r1 **9/9 passed** (47.4s — cancel/reclaim/start arms added).
- `npx playwright test tests/e2e/sidebar-hierarchy.spec.ts sidebar-gesture.spec.ts app-shell-viewport.spec.ts` (Sidebar/router neighbors): 11 passed + 1 flaky-passed-on-retry (sidebar-gesture hamburger arm — pre-existing gesture-timing flake, not the board surface).

---

## Next Steps

- Engine lane follow-ups (flagged on #1699): ListEpics/search/health read seams + the `board.event` SSE publisher — the frontend consumes each the day it lands (epic chips switch to the list endpoint; the feed lights up).
- The drag affordance map can flip to server-driven if the engine ever exposes its transition table; today's mirror is tested against stage.go's enumerated set.

---

## Files Modified (r1 adds/edits)

- `frontend/src/lib/boardFeed.ts` (new) — the §4.8 activity-feed fan-out seam.
- `frontend/src/components/board/EpicDrawer.tsx` — activity feed + lifecycle error notice.
- `frontend/src/components/board/TaskCard.tsx` — escalation preview + Start affordance.
- `frontend/src/components/board/TaskDialog.tsx` — boolean submit contract, local catches, dep-input preservation, cancel testid.
- `frontend/src/pages/BoardPage.tsx` — optimistic map, affordance wiring, task_start mutation, page-level notices, activity subscription, boolean handlers.
- `frontend/src/pages/BoardPage.actions.test.tsx` (new, 15) + `BoardPage.sse.test.tsx` (reclaim pin), `boardTestUtils.tsx` (mockStartTask).
- `frontend/src/hooks/useUserEventStream.ts` — boardFeed emit beside the invalidation.
- `frontend/tests/e2e/board.spec.ts` — cancel/reclaim/start arms (9 rows).

## Files Modified

- `frontend/src/api/board.ts` (new) — board REST client (23 ops, wire-verbatim types, requestId).
- `frontend/src/api/board.test.ts` (new).
- `frontend/src/api/types.ts` — `ApiError.suggestions` (additive).
- `frontend/src/lib/boardRules.ts` (new) — stage machine mirror, OR4, did-you-mean, error shapes.
- `frontend/src/lib/boardRules.test.ts` (new).
- `frontend/src/components/board/TaskCard.tsx` (new).
- `frontend/src/components/board/TaskDialog.tsx` (new).
- `frontend/src/components/board/EpicDrawer.tsx` (new).
- `frontend/src/components/board/BoardErrorNotice.tsx` (new).
- `frontend/src/pages/BoardPage.tsx` (new).
- `frontend/src/pages/boardTestUtils.tsx` (new) — shared page test harness.
- `frontend/src/pages/BoardPage.test.tsx`, `BoardPage.drag.test.tsx`, `BoardPage.scope.test.tsx`, `BoardPage.validation.test.tsx`, `BoardPage.sse.test.tsx` (new).
- `frontend/src/hooks/useUserEventStream.ts` — board.event branch + board reconnect reseed.
- `frontend/src/hooks/useUserEventStream.board.test.tsx` (new).
- `frontend/src/router.tsx`, `frontend/src/components/layout/Sidebar.tsx` — /board route + nav.
- `frontend/tests/e2e/board.spec.ts` (new).
- `CHANGELOG.md`, this worklog.
