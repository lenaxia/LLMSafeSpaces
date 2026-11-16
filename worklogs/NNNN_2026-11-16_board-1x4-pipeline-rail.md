# Worklog: the board's 1x4 pipeline rail (tsk-profound-teapot-seizes)

## Context

Owner direction 2026-11-15: the 2x2 stage grid isn't working — a board is
a pipeline and pipeline state reads left-to-right; `in_review` sitting
under `in_progress` in the second grid row destroys the flow axis. The
settled layout spec: a single-row 1x4 rail with scroll-snap per column;
portrait shows the three active columns at 95% of the rail width with
done's edge peeking in the final 5% (carousel edge-peek — done is
terminal reference, hence the existing Done cap + drawer stay);
landscape/desktop (the landscape rule doubles as the desktop rule) shows
all four columns; empty columns keep their width (skeleton, never
collapse); keyboard parity per the #2021 a11y discipline (arrow keys
navigate columns, focus visible).

## Geometry confirmation (the pre-removal check the direction asked for)

The board has NO vertical-relationship dependency on the 2x2 grid:
- Drag/drop is event-target based — drops resolve through
  `data-stage-drop` targets and `closest()` lookups (see
  `BoardPage.drag.test.tsx`), never grid geometry. Note the direction's
  parenthetical said "kebab actions only, no drag-drop"; the tree
  actually carries manager drag/drop, but it is equally
  geometry-independent — confirmed by the untouched drag suite passing
  on the rail.
- Card interactions (kebab `stopPropagation`, details/start buttons,
  click-through editor) hang off the cards, not the grid.
- The e2e containment pins (`card within cell`, kebab within card) pass
  identically inside flex columns.

## The rail

`StageRail` (frontend/src/pages/BoardPage.tsx) replaces the
`grid grid-cols-2 … md:grid-cols-4` div:

- One row: `flex snap-x snap-mandatory gap-2 overflow-x-auto`; the
  `stage-grid-<lane>` testid is preserved (the e2e suite's anchor).
- PORTRAIT weights: backlog 29% / in_progress 36% / in_review 30% — the
  active trio shares 95%; done renders at 30% so its leading 5% is the
  continuation affordance. in_progress runs widest (titles + assignee +
  lease chips are the densest column). Every column cedes 0.375rem so
  the three gap-2 gutters (1.5rem) are funded without eating the peek:
  widths are `calc(P% − 0.375rem)`.
- DONE renders as a FULL-WIDTH final page in portrait
  (`w-[calc(100%+0.75rem)]`): at a share width, done's snap point —
  and even in_progress's — sits past the rail's maximum scroll (max
  scroll = last offset + doneWidth − railWidth, and mandatory snap
  FREEZES at the clamp instead of settling; measured: scrollTo(104)
  landed at 93 and locked). The full page keeps every column's
  snap-start reachable, so swipe and arrow keys truly move
  column-to-column. The RESTING view is unchanged — done still peeks
  at 5%; its width only shows once you navigate to it.
- LANDSCAPE/DESKTOP doubles as the md+ rule: 24/28/24/24 splits the
  full row (all four visible) over `md:min-w-[7.5rem]` floors
  (in_progress `md:min-w-[8rem]`) plus `md:grow` to fill the row. The
  floors matter more than the naive reading: the board's left panel
  narrows the RAIL well below the viewport (measured 562px of rail at
  an 844px viewport) — 11rem floors would have engaged and forced a
  scroll at landscape widths; 7.5/8rem keep the exact-fit percentages
  in charge. Narrower rails (a 768px viewport leaves ~470px) scroll
  honestly — min-widths hold, the scroll container absorbs the rest. md (768px) matches
  the previous grid's 4-up threshold and covers landscape phones
  (844px) and desktops.
- Widths live on fixed-basis `shrink-0` wrappers with a portrait
  `min-w-[6rem]` floor, never on content — empty columns keep their
  width across every drain state (the all-done board renders three
  skeleton columns at full width, not a 95% void).
- Keyboard parity (#2021): the columns are a roving-tabindex group —
  exactly one column is the rail's tab stop; ArrowLeft/ArrowRight move
  focus column-to-column (clamped at the ends) and
  `scrollIntoView({inline: "start"})` snap-scrolls the target into
  view; `focus-visible` outline on the wrapper is the visible-focus
  affordance. The rail is a labelled `region`; each column a named
  `group` (`aria-label="Backlog column"` etc.).
- Card readability at the cramped portrait widths: the house answer is
  wrap-not-clamp — titles keep `min-w-0 break-words` inside the card,
  the action cluster stays `shrink-0`. The e2e long-title containment
  pins now run at the ~100px portrait column widths (title wraps
  multi-line, nothing bleeds).
- Unchanged by construction: the Done column's 10-newest cap + show-all
  drawer (StageCell untouched), kebab actions, drag arms, the Failed
  side column.

## Tests

Component (`BoardPage.rail.test.tsx`, new — 9 tests): rail structure and
MAIN_STAGES DOM order; portrait 29/36/30=95 weights (exact class pins);
landscape md weights summing to 100 with min-width floors; roving
tabindex + arrow-key focus movement + `scrollIntoView` spy with
`{inline: "start"}` + end clamping; the a11y tree (region/groups/labels
+ focus-visible class); all-done and fully-drained boards retaining all
four columns at width with skeleton cells; the Done cap + drawer opening
from inside the rail; kebab/drag-arm presence.

Updated `BoardPage.declutter.test.tsx`: the old `grid-cols-2`/
`md:grid-cols-4` pin now guards the REMOVAL (no grid classes, snap-x
present).

E2E (`tests/e2e/board.spec.ts`): the mobile test now pins the real
geometry at 375px — flex rail, 4 columns, `scroll-snap-type: x
mandatory`, active trio fill ≈95%, done peek ≈5%, no page-level
overflow, long-title wrap/containment; a new describe block pins
snap-settling per column (scrollLeft lands column-aligned),
keyboard parity (ArrowRight focuses + scrolls, ArrowLeft returns), the
landscape rule at 844×390 (4 columns fully visible, no scroll,
left-to-right order), and empty-column width retention on an all-done
board.

Commands and outcomes (all on the worktree head):
- `npx vitest run src/pages` — 50 files / 497 tests passed.
- `E2E_PORT=5199 npx playwright test tests/e2e/board.spec.ts` — 42
  passed (includes the 5 new/rewritten rail tests and the untouched
  Done cap/drawer tests, which run at the 1280×720 default viewport —
  the desktop arm of the landscape regression pin).
- `npx vitest run` (FULL suite) — 2726/2726 passed.
- `npx tsc --noEmit` — clean. `npx eslint` on the touched files — 0
  errors (one pre-existing `rememberEpic` exhaustive-deps warning,
  present on origin/main, untouched). `make fmt-check` — clean.

Process note: an earlier eslint/e2e "pass" was measured against a
stashed (pre-change) tree — a careless `git stash` during a baseline
check reverted the working tree mid-session. Caught by the commit-time
status check (no modified files), restored via `git stash pop`, and
every number above was re-run on the true tree. The failing-red first
e2e run on the real tree is what surfaced the snap-freeze and the
562px-rail facts.

## Key decisions

- md (768px) is the landscape/desktop threshold — the previous grid's
  own 4-up breakpoint; true orientation queries would leave an 844px
  landscape phone on the portrait split, contradicting the spec.
- Percent widths include each column's 0.375rem gutter cede rather
  than using container gap arithmetic — the 95/5 split holds exactly at
  any rail width where no min-width floor engages (the floors only
  guard degenerate narrowness).
- done = full page, not 30%: the snap-reachability math above forced
  it; a share-width done cannot be snap-start-aligned inside a rail
  whose content is only ~125% of its width.
- md floors sized against the RAIL's real width (562px at an 844px
  viewport, thanks to the left panel), not the viewport — verified by
  measurement, not assumption.
- cn() is tailwind-merge: per-stage md:min-w entries REPLACE the base
  floor (no accumulation) — the tests assert each column's own floor.
- Wrap-not-clamp retained for titles: the owner asked to VERIFY
  readability at target widths; the e2e containment pins now exercise
  exactly that, and changing card internals would violate the
  "card interactions unchanged" pin.

## Blockers

None.

## Next Steps

Reviewer rounds on the PR; nothing further planned in this lane.

## Files Modified

- frontend/src/pages/BoardPage.tsx — StageRail + column width map; the
  grid call-site replaced; `useRef` import.
- frontend/src/pages/BoardPage.rail.test.tsx — new.
- frontend/src/pages/BoardPage.declutter.test.tsx — grid pin updated.
- frontend/tests/e2e/board.spec.ts — mobile rail pin + the new 1x4
  rail describe block.
