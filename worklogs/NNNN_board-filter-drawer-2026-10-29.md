# Worklog — 2026-10-29 owner ruling: the board's filter pills collapse into a drawer

Task: `tsk-noble-spiral-notices` (epic "File-CRUD hardening follow-ups (0.44.5
live-fire findings)", project "Session Subscription Feature").

Owner ruling, verbatim:

> "I think those pills need to be in a collapsed drawer. They eat up a lot of
> screen real estate esp on mobile."

Context: post-#1976 (the 2026-10-28 ruling 1) the toolbar still rendered a
second row of REAL filter pills — the #1813 FAILED overlay toggle + the tag
facets — which ate vertical space on every viewport and squeezed the 4-column
mobile board hardest. This lane folds them away.

## Scope note — "stage filter" mapping

The task text names three pill families ("stage filter, failed-attempt
overlay toggle, tag facets"). On post-0.44.7 main exactly TWO exist: the
FAILED overlay and the tag facets. The "stage filter" is the FAILED overlay —
the 2026-28 ruling 1's own words ("We can keep ones for filtering such as by
stage") and the scope suite's comment ("only real filters (stage overlay,
tag facets) remain") both use that name for it. No new stage filter is
invented; the drawer holds every filter control that exists.

## The change (frontend/src/pages/BoardPage.tsx)

- New `filterDrawerOpen` state (default closed). The toolbar's ml-auto group
  gains the funnel button (`ListFilter` icon, `board-filter-button`) FIRST —
  the board's one filter affordance — with `aria-expanded`/`aria-controls`
  wired to the drawer.
- The pill ROW is deleted; the same controls (testids unchanged:
  `failed-filter-toggle`, `tag-filter-<tag>`) render inside
  `board-filter-drawer`, a `role="group"` panel that is CONDITIONALLY
  rendered — closed = not in the DOM = zero height. In flow (a slide-down
  panel): open, it pushes the swim lanes down, never covers them; the same
  shape serves desktop and mobile (the ruling's "on mobile a bottom sheet or
  slide-down panel").
- Count badge (`board-filter-count`): `(failedOnly ? 1 : 0) + (tagFilter ? 1 :
  0)` — no badge at zero. Search and project focus are NOT drawer filters
  and never count.
- Clear all (`board-filter-clear`, INSIDE the drawer): resets
  `failedOnly` + `tagFilter` in one click; disabled at zero active filters.
- Escape collapses the drawer (window keydown listener armed only while
  open) WITHOUT touching the applied filters — collapse is chrome, not reset.
- The toolbar keeps its essentials: board title, project-focus select,
  search, filter button, Board defaults, New epic, New project. (The ruling
  is about PILLS; the focus select and search are not pills and stay. The
  job's "essentials" list — defaults button, filter button, title — is what
  must REMAIN, and all three do.)
- `data-testid="board-toolbar"` added to the scope-switcher container so the
  e2e geometry row can pin "board content starts immediately below the
  toolbar".

## Tests

New `BoardPage.filterdrawer.test.tsx` (7 tests):

1. opens/closes via the funnel button (aria-expanded flips, drawer in/out of
   the DOM);
2. Escape collapses; applied filters + badge SURVIVE the collapse;
3. badge count 0 (absent) → 1 (FAILED) → 2 (FAILED+tag) → 1 (untoggle) —
   tracks the LIVE count, not a session history;
4. Clear all resets every filter; rows return; badge drops; FAILED control
   reads unselected;
5. Clear all disabled at zero;
6. filtering parity inside the drawer (tag facet + FAILED overlay filter
   identically to the old inline pills);
7. reopen reveals the CURRENT selections (tag facet in selected state).

Re-pinned suites (open the drawer where the pills used to be inline):
`BoardPage.scope.test.tsx` (the no-epic-pills pin now also pins no-ANY-pills
inline + the drawer reveal), `BoardPage.test.tsx` (FAILED toggle),
`BoardPage.archived.test.tsx` (the chip-leaves-with-its-lane pin runs with
the drawer OPEN so the disappearance still bites; the archived-tag pin opens
the drawer so it isn't trivially green).

E2E (`tests/e2e/board.spec.ts`): the scope-switcher row now pins
drawer-closed → no pills → open → FAILED inside; NEW describe
"2026-10-29 filter drawer" with two rows — (a) desktop: badge 1→2, clear-all
restores rows + drops badge, toggle-close leaves the DOM; (b) mobile 375px:
drawer absent + zero inline pills, `board-grid.top === board-toolbar.bottom`
(±1px) both before and after an open/close cycle, the open drawer overlaps
the lanes by 0px (in-flow push), and an applied filter + badge survive the
collapse. The e2e `E2ETask` fixture grows `last_attempt_failed`/`failure_count`
(defaulted false) so the desktop row can stage a failed survivor.

## Verification

- `vitest run`: 223 files / 2555 tests green (was 2548 pre-change on the
  same tree: +7 net new).
- `tsc --noEmit`: clean.
- `eslint .`: 0 errors (7 pre-existing warnings — 6 generated-code
  unused-disable directives + the `rememberEpic` exhaustive-deps warning,
  verified byte-identical on the unmodified origin/main base).
- `E2E_PORT=5199 playwright test tests/e2e/board.spec.ts`: 33 passed
  (31 re-pinned + 2 new).

## Process

Branch `board-filter-drawer` off origin/main 6ce1859f6 (post-0.44.7), a fresh
worktree at /tmp/opencode/filter-drawer-wt. Conventional commits; PR iterated
with the pr-review.yml AI reviewer until APPROVE per the task's binding
process — never self-merged. Review rounds appended below.
