# Worklog — 2026-10-28 owner rulings: board view declutter + mobile layout + Board workers subtree

Task: `tsk-polished-finch-paints` (epic "File-CRUD hardening follow-ups", project
"Session Subscription Feature"). SIX owner rulings, verbatim, applied to the
frontend board view (`frontend/src/pages/BoardPage.tsx` + `frontend/src/components/board/*`)
and the sidebar (`frontend/src/components/layout/Sidebar.tsx` + `frontend/src/lib/sessionTree.ts`):

1. "Get rid of the epic pills at the top. We can keep ones for filtering such as by stage, but we don't need all the epics as pills."
2. "Per project, the epic boxes at the top of each project do not need to show the tasks under them. We can have rows for every epic, but we don't need to show their task lists underneath. Clicking the epic highlights all related cards, so that already addresses that issue."
3. "For the epic list, we should have a max visible, otherwise the list can get unwieldy long — anything beyond max should be scrollable. So maybe show like 4 epics and then scroll through the rest, sorted by activity or alpha."
4. "All things are showing their epc-/tsk- ids — we don't need those except in the metadata popup when clicked on. Showing the user-friendly title of what the work is is more important."
5. "The task cards are not properly word-wrapped. The kebab, resume and other buttons overflow into other columns on mobile. 4 columns might be a bit tight for mobile unless you can find a proper wrap."

## Ruling 1 — epic pills out of the toolbar

The `focusEpic` state, the `derivedEpics`/`scopedEpics` flat lists that fed them,
and the pill row (`epic-chip-all` + per-epic `epic-chip-*`) are deleted from
`BoardPage.tsx`. The toolbar now renders ONLY real filters: the #1813 FAILED
overlay toggle, the tag facets, and the project-focus select. Epic association
discovery rides the epic rows' click-highlight (ruling 2) — the ruling's stated
replacement. The `filteredTasks` memo drops its `focusEpic` arm.

## Ruling 2 — one epic ROW per project, no nested task lists

The collapsible epic bands (`collapsedBands` state, `toggleBand`, the
`band-toggle-*` chevron, and the task rows grouped beneath each band —
`band-task-*`, `band-empty-*`, `band-filtered-empty-*`) are gone. Each epic is
now ONE row: title (truncating, `band-title-*`), progress counts, paused badge,
kebab. The row's click still selects the epic (highlight its task cards across
the columns, dim the lane's others, scope New task) — now the ONLY
epic→task navigation, so the row carries the tooltip "click to highlight its
tasks; double-click for the epic drawer" and stays intact end-to-end
(unit-pinned + e2e-pinned, including the deselect pass).

## Ruling 3 — capped list, own scroll region, activity-first

New `EPIC_LIST_MAX_VISIBLE = 4` export. The rows live in
`epic-list-{project}` — a `max-h-[7.75rem] overflow-y-auto` flex column
(~4 × 1.75rem rows): overflow scrolls INSIDE the list container, never growing
the page. Every row still renders (the cap is visual; scrolling reaches the
rest) — the declutter tests count all rows inside the capped container.

Sort choice (noted here and in the PR): **activity-first** — an epic's activity
clock is the newest of its own `updated_at` and its task rows' `updated_at`
(both already in the view layer — no new API fields); ties and timestamp-less
(quiet/derived) rows fall to alpha (`localeCompare`). Implemented in
`epicActivity()` + the per-lane `sortedLaneEpics` sort.

## Ruling 4 — ids out of the chrome, titles first

- Lane headers: the `prj-` mono chip is gone (the name is the label).
- Epic rows: the `epc-` chip is gone; the title is the label.
- Task cards: the `tsk-` mono line is gone; the TITLE is the primary label
  (`card-title-*`, wraps per ruling 5), `aria-label` is the title alone.
- Blocked-by panel: dependencies are named by TITLE (`title ?? id` fallback);
  the id rides the tooltip + `data-dep-id`.
- Project-focus options: `NAME` only (the id stays the option's `value`).
- Notices/prompts: start/dispatch, archive, epic-delete, and the Done prompt
  lead with the title/name instead of the short id.
- Search did-you-mean chips carry the TITLE (fills the search with the title).
- Ids render ONLY in click-through metadata popups: the task details drawer
  (`drawer-short-id`), the epic drawer (`epic-drawer-short-id`, secondary line
  under the title), and the delete/task dialogs' titles (secondary mono ref).
- `data-testid`/`data-*` attributes KEEP the ids everywhere for tests.

## Ruling 5 — wrap + responsive columns

- Card: `min-w-0`; title `min-w-0 flex-1 break-words` (wraps inside the card);
  the action cluster (details/start/kebab) is `shrink-0` — never squeezed out
  of the card's bounds. Pause/concurrency badges moved into the wrapping chip
  row so they can't force column bleed either.
- Grid: `grid-cols-2 md:grid-cols-4` on `stage-grid-{project}` — the standard
  responsive collapse (2 columns on small screens, 4 from md up), chosen over
  horizontal snap-scroll because vertical card stacks keep buttons contained
  without a second scroll axis fighting the page's.

## Tests

- `BoardPage.declutter.test.tsx` (new, 8 tests): cap + own-scroll-region
  attributes, activity-first order (with quiet-rows-alpha fallback), NO
  `prj-|epc|tsk-` text anywhere in a lane, titles as labels, ids in the
  click-through drawers only, blocked-by titled, wrap/containment class pins,
  `grid-cols-2`/`md:grid-cols-4`, and the click-highlight survival row.
- Existing board suites re-pinned to the new shapes (scope: no epic chips;
  swimlane: rows-not-bands, no nested tasks, no toggles; actions/validation:
  title-first prompts and notices).
- `tests/e2e/board.spec.ts`: the pills/collapse/band-task rows re-pinned to
  their absences; two new declutter tests — (a) 9-row capped list with real
  `getComputedStyle().overflowY` + `scrollHeight > clientHeight` + bounded
  box height + exact activity order; (b) 375px viewport: 2 columns computed,
  4 at 768px, no document horizontal overflow, a 170+-char unbroken title
  wraps to multi-line, and title/card/kebab boxes stay inside the column
  cell's bounds.

Green (rulings 1–5 as carried over, re-verified by this attempt before
building on them): `vitest run` board suites + `tsc --noEmit` clean;
`playwright test tests/e2e/board.spec.ts` 15 passed (2.3m).

## Rulings 6+7 — the EXISTING Workers group, provenance-populated; subtasks nest under their top-level parent

> 6. "The workers group is the group to use, don't create a new one."
> 7. "Subtasks spawned by a top-level task should show up under that
>    top-level session. There's currently a bug where the latter is not
>    happening."

(Charter history: the original attempt-1 charter carried a "Board
workers" rename framing; the charter was rewritten mid-flight to the
verbatim rulings above — the EXISTING "Workers" folder stays, no new
group. The label/aria revert is commit 4.)

The #1966 "Workers" folder (0.44.4) classified `origin === 'worker' ||
!!parentId` AND built the main tree from user sessions only AND
rendered the folder as a flat `children: []` map — so a user session's
Task-tool subagent could NEVER nest under it (ruling 7's reported bug)
and a worker's subagents could never nest either. The fix: parentage
no longer auto-banishes — parented sessions FOLLOW THEIR PARENT
wherever it renders; the folder's roots are origin-class workers
only, as a real tree.

### Frontend (Sidebar.tsx + sessionWorker.ts + sessionTree.ts)

- The group keeps its name/testid/aria ("Workers", `workers-group`,
  "Expand worker sessions"/"Collapse worker sessions") — ruling 6.
- `isWorkerSession` drops the parentId arm: origin-class `worker`
  (the spawner's stamp) is the ONE signal. New `sortByRecentActivity`
  (most recent of lastMessageAt/lastSeenAt desc; timestamp-less sink,
  alpha fallback) orders the group.
- ONE tree is built over the full live set (`buildSessionTree`), then
  partitioned: worker-provenance roots (+ their genuine subagent
  subtrees, children attached by the tree builder) → the renamed
  "Board workers" group; non-worker roots → the main list;
  orphan-by-parentage rows → promoted to top-level main-list rows
  (their parent is gone; visibility preserved — the synthetic
  "Orphaned subtasks" group stays dead, and the `orphans` bucket
  semantics in sessionTree are unchanged).
- Group rows are the SAME `SessionTreeRow` renderer — busy/idle rides
  the existing per-session status indicator (BusyIndicator when busy),
  pending-question indicators bubble up worker subtrees (the group now
  passes the real `pendingIndicatorIds`), and each worker's children
  nest with working chevrons (the group shares the main tree's
  expansion set — a worker subtree expands/collapses exactly like a
  main-tree subtree).
- Auto-expand: selecting a worker OR one of its nested descendants
  opens the group (ancestor-chain check crosses the group boundary).
- Archived still outranks the class (archived workers row under
  Archived); the group hides entirely with zero board workers; the
  /session-origins merge stays as a compatibility fallback.

### Platform — provenance on the session list payload, end-to-end

The spawn marker was ALREADY persisted (`board_spawner.go` stamps a
`session_origins` row — origin `worker`, kind `session` — at spawn,
#1966), but the LISTING never carried it (the frontend had to merge
the separate `/session-origins` endpoint client-side). Now
`ListSessionIndex` LEFT JOINs `session_origins` (1:1 on session_id)
onto every row, so `SessionListItem.origin` — a field that already
existed wire-visible — is populated server-side:
`GET /workspaces/:id/sessions`. SDK Go + TypeScript types and
`sdks/openapi.yaml` gain the field (openapi-validate green).

### Tests (ruling 6)

- `sessionWorker.test.ts`: provenance-only pins (parentId is NOT a
  worker signal) + the activity sort (order, null-sink/alpha, no
  mutation).
- `Sidebar.workers.test.tsx` (rewritten, 10 tests): group renders
  workers with count/collapse; busy badge on a busy worker; activity
  order; hides entirely with subagents-but-no-workers (subagent nests
  under its user parent); payload-carried origin classifies without
  the fallback endpoint; worker's subagent children nest beneath it
  inside the group (hidden until the subtree chevron, never a flat
  root); routine/manual/workflow stay main-list; auto-expand on worker
  AND on worker-descendant selection; archived worker → Archived only;
  the query keeps `{ includeArchived: true }` (presentation-only).
- `Sidebar.test.tsx` / `Sidebar.hierarchy.test.tsx` /
  `Sidebar.forcestop.test.tsx` re-pinned to the restored nesting
  (subtask spinner/unread/pending rows live in the main tree again;
  the pending indicator bubbles to the nesting parent once more;
  orphans promote; navigating to a parent auto-expands its children).
- `tests/e2e/sidebar-hierarchy.spec.ts` re-pinned to the provenance
  contract (payload-carried origin; group count 1 with a parented +
  orphan session present; direct-to-worker auto-expansion; user
  subagent does not open the group; no group when worker-free).
- Platform: `session_index_origin_join_test.go` (sqlmock — the join
  must be in the SQL, the origin scanned onto the item) +
  `session_index_origin_join_integration_test.go` (build-tagged
  Postgres round-trip: worker/routine marks and the unmarked row).

Green (full): `vitest run` 222 files / 2527 tests; `tsc --noEmit`
clean; `go build ./api/...` + SDK builds; `make openapi-validate`;
board + sidebar-hierarchy e2e specs.

## Process notes

Worktree `/tmp/opencode/board-ui` (branch `board-view-declutter`) off
`origin/main` @ 326151fe2. Attempt lineage (honest record, correcting
this section's earlier draft which pre-claimed a PR): the attempt-2
worker's board claim lapsed at its 19:44:43Z OOM restart; the engine
re-granted the task; its uncommitted rulings-1–5 implementation
carried over WHOLE and was re-verified green by this attempt (board
suites) before ruling 6 was built on top. No PR existed before this
attempt's push. Per the binding process ruling: PR opened, iterated
with the pr-review.yml AI reviewer to APPROVE (verdict + head SHA in
the PR), NOT merged — merging is the orchestrator's move.

Attempt-1 lease lapse (honest record): the claim lapsed at 21:17:59Z
while awaiting the first PR Review run (a sleep-poll gap exceeded the
lease window); the engine re-granted as attempt 2 with the charter
rewritten to the seven-ruling form. This session landed the ruling-6
label revert (commit 4) on the shared branch so the successor
inherits a charter-compliant head, then ceded the board writes.

## Review r1 (pr-review.yml, REQUEST_CHANGES on 027388d1e) — fixes landed

Attempt 2 (claim re-granted 21:19:26Z after attempt 1's lease lapse)
drove the review loop. CI fully green on 027388d1e (33 checks — the
uuid=text join fix + go-jose 4.1.6 verified); the AI reviewer
REQUEST_CHANGES with four findings, all addressed in commit 5:

1. **Stale "Board workers" naming** (the ruling-6 revert left it in
   consumer-facing docs): fixed in `sdks/openapi.yaml`,
   `sdks/go/types.go`, `sdks/typescript/src/types.ts`, CHANGELOG, and
   the test titles/comments (`Sidebar.workers`, `Sidebar.hierarchy`,
   `sidebar-hierarchy.spec.ts`, `session_index_origin_join_test.go`).
2. **Overstated pending-indicator claim — the gate is FIXED, not the
   claim**: `SessionTreeRow` gains `rootLevel`; group roots render at
   depth 1 but are top-level in the indicator sense, so the
   pending-input indicator (and the unread pulse) now genuinely fires
   on workers awaiting input inside the group. Born-green pin added
   ("shows the pending-input indicator on a worker awaiting input
   INSIDE the group" — quiet-worker negative arm included).
3. **Orphaned workers**: the orphan promotion no longer bypasses the
   provenance split — `tree.orphans` is partitioned like `tree.roots`
   (orphaned workers root the Workers group; non-worker orphans keep
   promoting to main-list top-level rows). Born-green pin added
   ("roots an ORPHANED worker in the group too").
4. **Zero-slack cap math**: `max-h-[7.75rem]` → `max-h-[8.5rem]`
   (4 rows + gaps + headroom), plus a new e2e pin: at exactly 4 rows
   the list does NOT scroll internally (`scrollHeight ===
   clientHeight`) and the 4th row's box is fully inside the window.

go-jose riding the fix commit: reviewer called it acceptable; left
as-is (history rewrite would invalidate the reviewed head).

## r1.5 — merge origin/main (PR #1975 board-config-parity landed mid-review), 2026-10-28 ~23:10Z

Main moved 12 commits under the branch after the r1 APPROVE
(058f7fdaa): PR #1975 merged board-config-parity, leaving PR #1976
`mergeable: CONFLICTING` — the APPROVE was on a head the orchestrator
could no longer merge. Merged origin/main into the branch
(ae825e20b); two conflicts, both mechanical unions:

- `CHANGELOG.md` — both [Unreleased] sections kept (ours: rulings
  6+7 + rulings 1-5; main's: board config parity).
- `frontend/src/pages/BoardPage.test.tsx` — main's three new
  board-config tests kept + our ruling-4 swimlane rename kept (main
  hadn't touched the title; the bodies merged clean).

Verification on the merge head: BoardPage/declutter/config-panels
seams 49/49; full vitest 222 files / 2548 tests green (2527 + main's
21); `tsc --noEmit` clean; `go build ./...` clean; eslint clean on
the touched test file; e2e board + sidebar-hierarchy + board-config
37 passed. Pushed for a fresh PR Review round on ae825e20b (every
push re-earns its own APPROVE — the 058f7fdaa verdict died with the
conflict).
