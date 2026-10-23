# Worklog: #1860 post-merge residuals — e2e harness hygiene (lifecycle error gate + dead route purge)

**Date:** 2026-10-23
**Branch:** `fix/1860-e2e-harness-hygiene`
**Status:** post-merge completion lane for #1860 (r2 formal review's two carried findings)
**Scope:** `frontend/tests/e2e/board.spec.ts` ONLY (test hygiene; no production code)

---

## Objective

PR #1860 merged with two residuals from its final formal review (round 2,
commit `6ad113c0`):

1. **Mandatory test gate:** `handleProjectLifecycle`
   (`BoardPage.tsx:567-580`) had an error branch
   (`setPageError(toBoardError(err))`, `BoardPage.tsx:576`) with ZERO
   unhappy-path coverage at any layer — no component mock rejection, no
   e2e failure row. Round-1's CI red had already proven the branch real
   and renderable (the archive 404 surfaced a page error no test
   asserted): the blind spot was demonstrated, not theoretical.
2. **Dead code:** five of six duplicated `page.route` registrations in
   `setupBoardMocks` were dead — Playwright resolves newest-first, so the
   second block won and the first block's ~110 lines never answered a
   request. Two of the dead copies (`projects/*/epics`, `epics/*/tasks`)
   were the LOSSY pre-r2 versions (no GET ListEpics arm, no
   `failEpicCreate`/`failTaskCreate` levers, no zero-task-epic
   `state.epics` handling): any future reordering would silently activate
   the weaker handlers and break the r2/r3 rows.

## The six duplicates (inventory)

| URL pattern | dead copy (deleted) | live copy (kept) | lossy? |
|---|---|---|---|
| `me/epics/*` (DELETE) | removed in #1860's delta | board.spec.ts `me/epics/*` | — |
| `me/projects/*/tasks*` | first registration | second registration | no (identical) |
| `me/projects/*/epics` | first registration | second registration | **YES** — no GET ListEpics arm, no `failEpicCreate` |
| `me/epics/*/tasks` | first registration | second registration | **YES** — no `failTaskCreate`, no `state.epics` zero-task parent |
| `me/tasks/*/move-stage` | first registration | second registration | no (identical) |
| `me/tasks/*` (DELETE) | first registration | second registration | no (identical) |

Deleted the dead block whole (the first five registrations, ~104 lines).
The archive/unarchive lifecycle registrations immediately above it are
the block's only LIVE siblings (registered exactly once) and are kept
verbatim — their 404 arm is the new row's failure injection point.

## Change 1 — lifecycle error-branch coverage (the mandatory gate)

New e2e row in the `#1832 item 3` describe: "project kebab: failed
lifecycle call surfaces the page error; board stays consistent, retry
recovers". It drives the lifecycle mock's OWN 404 arm (the
`project not found` branch) — the harness's natural failure injection
point, no override route needed: after `openBoard`, the project is
removed from `state.projects` (the handler reads it at request time),
modelling the project vanishing server-side between the board's read and
the lifecycle write. The failed path never invalidates queries, so the
UI keeps its cached read while the write 404s. Pins:

- **surfaced, not swallowed:** `[data-board-error]` renders with the
  `"error"` class attribute (no guard code on this arm) and the engine's
  message verbatim (`board: project "prj-aaaa1111" not found`);
- **board state consistent:** no crash, swimlane + cards survive on the
  cached read, the `archived` badge did NOT land (write never succeeded);
- **behaves on retry:** project restored server-side → retry archives,
  post-invalidation badge lands, page error clears (`setPageError(null)`
  at handler top).

Mutation-verified RED: replacing the catch's `setPageError(toBoardError(err))`
with a swallow reddens exactly the surfaced-error assertion
(`element(s) not found` for `[data-board-error]`), both attempts —
reverted, never committed. The gate has teeth at the e2e layer.

## Change 2 — dead route purge + the r2 robustness rider

- Deleted the five dead first-copy registrations (~104 lines; the live
  second copies were already strictly more capable — identical for
  `tasks*`/`move-stage`/`tasks/*`, supersets for the two lossy epic
  copies).
- Per the r2 robustness note ("extract one shared helper when the dead
  block is removed"): the epic-delete route's inline `wireEpic` arrow and
  the `derivedEpicsFromTasks` mapper's inline template both derived
  `epc-<compact-id>`; drift between the two would silently unmatch the
  epic-delete guard arms. Extracted ONE module-level
  `wireEpicShortId(id)` used by both.
- Style rider from r2: `__base` → `base` in `openBoard`.

## Verification (local, this branch's only file: board.spec.ts)

- `E2E_PORT=5199 npx playwright test tests/e2e/board.spec.ts` —
  **28 passed** (27 pre-existing rows green post-purge, proving the
  deleted registrations were dead + 1 new row). Grep-isolated rerun of
  the new row: 1 passed.
- `npx vitest run` — **2482 passed / 217 files** (no unit layer touched;
  confirms no cross-layer fallout).
- `npx tsc --noEmit` — clean.
- `npm run lint` — 0 errors (7 pre-existing warnings, all in untouched
  `src/` files; none in board.spec.ts).

## Residuals

- The task-delete wire-level failure row (`task_paused` refusal at e2e
  layer) remains component-only — carried from r2's "acceptable as a
  follow-up" note; the shared modal error path is e2e-covered via the
  epic refusal row.
- The component-layer lifecycle rejection
  (`mockArchiveProject.mockRejectedValueOnce`) stays unadded — this lane
  is `frontend/tests/e2e` ONLY (fleet no-touch on `frontend/src`); the
  e2e row satisfies the gate's "at any layer" requirement.
- `acknowledgeForce` re-arm after a failed force attempt
  (EpicDeleteDialog deps `[open, epic?.id]`) — carried non-blocking
  robustness note from r1/r2; production code, out of this lane's scope.

## Mutation-verify suggestion (for review)

Swallow the lifecycle catch in `BoardPage.tsx` (catch → no-op): the new
row must fail at `expect(notice).toHaveAttribute("data-board-error",
"error")`. Restore one dead registration (e.g. re-add the lossy
`epics/*/tasks` copy BEFORE the live one): no row reddens — that is the
dead-code proof (the live copy still answers) — then move it AFTER the
live one with the `failTaskCreate` lever armed and the failed-New-task
row silently breaks on the lossy copy: the regression-masking hazard
the purge removes.
