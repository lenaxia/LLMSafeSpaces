# Worklog — 2026-11-13 owner ask: the Done column caps its inline length, the full history moves into a drawer

Task: `tsk-cloven-magnet-thirsts` (epic "Frontend reliability mopup", project
"Agent DX: reference patterns"). Frontend scope only, worktree off `origin/main`.

## The ask

The owner (2026-11-13): the Sprint Board's Done column can get excessively
long — hundreds of completed stories render inline, pushing the board layout
and making it hard to scan. Two acceptable shapes: (a) scrollable column with
a capped visible length, or (b) an expandable drawer — (b) preferred; a hybrid
satisfying both readings ideal.

## What landed (the hybrid)

- **Cap** — `DONE_COLUMN_MAX_VISIBLE = 10` (exported beside
  `EPIC_LIST_MAX_VISIBLE` in `BoardPage.tsx`): the Done column of every
  swimlane renders at most the 10 NEWEST-done inline cards plus the
  "Show all N done" affordance (a native `<button>`, keyboard reachable,
  `aria-haspopup="dialog"`). The header's count badge keeps the lane's FULL
  done count — the cap is visual, nothing is dropped. At ≤10 done stories the
  column renders exactly as before (every card inline, no affordance — the
  regression pin). 10 keeps the terminal column's height in line with the
  other through-flow columns.
- **Drawer** — new `frontend/src/components/board/DoneDrawer.tsx`, the
  StoryDrawer shape (Radix right-side `Dialog`: Overlay + fixed `right-0`
  Content + `Dialog.Title` + Close). The FULL done set renders as the board's
  own `StoryCard`s inside an `overflow-y-auto` region — the drawer is the
  column's overflow surface, so the page never grows with the done count (the
  scrollable reading of the ask). Every column affordance works in place:
  details opens the story-detail drawer ABOVE it, the primary click opens the
  edit dialog, the kebab opens the delete modal — each returns to the done
  list on close (Radix portals stack in open order; the DoneDrawer renders
  before the page's dialogs in JSX so later-opened overlays sit above).
  Focus trap + Escape + focus return ride the existing dialog stack — no new
  overlay system. Collapse state is per board-view mount (no persistence;
  the board has no UI-prefs pattern to ride).
- **Ordering** — `sortDoneNewestFirst` in `frontend/src/lib/boardRules.ts`:
  `stage_entered_at` desc (when the row entered Done — stamped by every stage
  write engine-side), `updated_at` as the degrade fallback, `short_id` asc
  for equal/absent clocks; pure (no input mutation). The server list arrives
  `created_at`-asc (`pkg/board/reads.go` ORDER BY t.created_at), so the
  newest-done-first order is applied VIEW-SIDE in both the capped preview and
  the drawer — and below the cap too (the ordering is the column's semantic,
  not the cap's). Every other column keeps the server order verbatim.

## Decisions worth recording

- **Drawer universe = the column's filtered view.** The drawer lists what the
  column would render uncapped under the live filters (search/tag/FAILED) —
  the drawer is the column's overflow, so its count (and the affordance's N)
  stay consistent with the column's header badge. A search that narrows the
  done set below the cap simply hides the affordance.
- **Per-lane drawer.** `doneDrawerProjectId` state (null = closed); the
  derived `doneDrawerProject` reads as closed if the lane vanishes mid-view
  (archived cross-context — the #1964 r1 stale-focus posture). Archived-drawer
  lanes work too: their swimlanes render through the same `renderSwimlane`.
- **StageCell stays one component.** The done specialization is a small
  branch (`stage === STAGE_DONE`) with two optional props
  (`laneShortId` for the per-lane affordance testid, `onShowAllDone`); the
  other three columns' render path is byte-identical (the `visibleCards`
  alias equals `cards` for them).

## Tests (TDD: red first — 13 new pins born-RED on the pre-change tree)

- `frontend/src/pages/BoardPage.donecolumn.test.tsx` (NEW, 8 pins): >10 →
  exactly 10 inline + affordance "Show all 13 done" + header badge stays 13 +
  other lanes independent; inline preview = the 10 newest, newest first;
  drawer lists all 13 newest-first in an `overflow-y-auto` region; Escape and
  the close button dismiss with the board intact; the affordance is a
  focusable native button carrying `aria-haspopup="dialog"`; drawer cards
  keep the details affordance (story drawer opens above, closing returns to
  the list); at exactly 10 all inline, no affordance; below 10 every card
  inline newest-first, no affordance.
- `frontend/src/lib/boardRules.test.ts` (+5): the sort's stage_entered_at
  desc order, the updated_at degrade, the quiet-row short_id tail, the
  equal-clock tiebreak, input non-mutation.
- `frontend/tests/e2e/board.spec.ts` (+1): the layout contract — exactly 10
  inline newest-first, the affordance text, the drawer's 13 cards in a real
  scroll region (`scrollHeight > clientHeight`), close returns to the capped
  column. Mocked-backend harness (the epic-list cap precedent).

## Verification (this head)

- `npx vitest run` — 2618 passed / 229 files (baseline 2605 on the
  pre-change tree: +8 board-page pins, +5 sort units = the 13 new).
- `npx playwright test tests/e2e/board.spec.ts` — 39 passed (38 + 1 new).
- `npm run typecheck` (`tsc --noEmit`) clean; `npm run lint` 0 errors
  (7 pre-existing warnings, same set as the base branch); `make fmt-check`
  exit 0 (Go untouched).
