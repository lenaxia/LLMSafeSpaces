# Worklog: portrait focus-column mode (tsk-robust-chalk-dines)

## Context

Owner direction 2026-11-15 (settled in conversation), the fast-follow to
the 1x4 pipeline rail (#2077 / tsk-profound-teapot-seizes): a
focus-column mode for PORTRAIT only — the column you're working gets the
width, neighbors compress. Landscape/desktop has NO focus mode (all four
columns already visible). The binding rules: focus follows SNAP-SETTLED,
never live swipe position (no mid-gesture reflow — geometry frozen until
snap settle, then one discrete transition + an explicit re-snap); overview
is the default and the board (every mount starts there, focus never
persisted, mode ephemeral); no auto-transitions; enter via a visible
header expand chevron (arrows + Enter on keyboard); transit via swipe-settle
or compressed-neighbor header tap (swiping never changes mode, only
position within it); exit via the same chevron (toggle symmetry), Escape,
or pinch-out — and the exit re-centers the overview on the column just
left (no jump to position zero). A11y per the #2021 discipline:
aria-expanded on the chevron, arrows navigate (overview) / transit (focus),
Escape exits, mode changes announced.

## The reachability deviation (done keeps its full page in focus mode)

The one place the letter of "neighbors compress" yields to physics: a
column's snap-start is only reachable when the widths from it to the end
cover the rail width (start_last ≤ maxScroll ⟺ w_last ≥ rail width —
the same invariant that made done a FULL PAGE in the rail's overview,
see the STAGE_COL_WIDTH comment). At a uniform 70/10/10/10 split the
content totals ~100% of the rail: max scroll collapses to the gutter
remainder and every column past the first sits beyond it — mandatory
snap FREEZES at the clamp, and the carousel cannot reach in_review or
done at all (hit empirically: the first e2e transit run settled back on
the already-focused column, exactly the prior story's share-width
lesson). So in focus mode the focused column takes 70% (or IS the full
page, when done is focused) and the non-focused done column keeps its
full-page overview width — rendered as a distinct `periphery` state:
wide, cards visible (Done cap + drawer untouched), chevron still
transits. The compressed 10% stubs are backlog/in_progress/in_review
when not focused.

## Implementation (frontend/src/pages/BoardPage.tsx)

All inside `StageRail` + `StageCell` (the side §4.8 columns pass no
`colMode` — no affordance there, unchanged):

- Mode state: `focusedIndex: number | null` — null IS overview;
  component state only, no storage read or write (the ephemeral-mode
  pin; unit test proves no focus-flavored storage write and a remount
  starts in overview).
- The frozen-geometry pin is STRUCTURAL: no scroll event handler exists
  at all. The only width-changing moments are the user actions
  (chevron / Enter / Escape / pinch / arrow transits) and the snap-settle
  transit on the native `scrollend` — which browsers fire only once
  scrolling (gesture AND snap animation) has fully stopped. Discrete
  states, one `transition-[width] duration-200` per change, then the
  explicit re-snap (`scrollIntoView` to the snapport start) scheduled
  220ms out — just past the transition, targeting the settled geometry.
- Settled-column resolution reads layout-stable offsets
  (`offsetLeft` deltas vs `scrollLeft + scroll-padding`), never rects.
  Settling back on the focused column is a no-op (idempotent against
  the re-snap's own scrollend).
- Enter: header chevron (`col-focus-toggle-<stage>`, `md:hidden`,
  ChevronsLeftRight → ChevronsRightLeft when expanded, aria-expanded,
  aria-label names the action) or Enter on the roving-stopped column
  (guarded `e.target === e.currentTarget` so Enter on a card is the
  card's key). Transit: swipe-settle (scrollend), compressed-header tap
  (the stub header itself, cursor-pointer), compressed/periphery
  chevron, or arrows (which in focus mode move the lens AND the roving
  stop). Exit: the focused chevron (toggle symmetry), Escape (guarded
  by the r1 overlay closest('[role="menu"], [role="dialog"]') check so
  open menus/dialogs keep their key), pinch-out (two touches spreading
  >25%, once per gesture). Exit re-centers: the re-snap targets the
  last-focused column under restored overview widths.
- Landscape/desktop: chevrons are `md:hidden` and a matchMedia
  (min-width: 768px) listener force-exits an open lens on the crossing —
  there is no way back in from md+.
- Announcements: a per-lane `rail-mode-notice-<lane>` polite live
  region (role=status, sr-only — absolute, never a flex child) carries
  "Focused <label> column" / "Overview — <label> column centered".
- Compressed stubs: 10% width with a 3rem floor replacing the 4.5rem
  overview floor (cn/tailwind-merge); cards hidden (a ~3rem stub cannot
  render a readable card — the header carries the column; the drop zone
  stays armed, drops still arbitrate).
- The rail region's aria-label now teaches the full keyboard contract
  (arrows / Enter / Escape).

## Interpretation note

"Every mount starts in overview (in_progress-centered)": read as the
overview resting state the rail already ships — the active trio sharing
95% of the width centered on the work, done peeking (the #2077 pinned
geometry and initial roving stop are untouched; changing either would
churn the predecessor story's green pins).

## Tests

- `frontend/src/pages/BoardPage.focus.test.tsx` (20 tests): overview
  default + no persistence + remount; chevron enter (state matrix,
  70/10 classes, floors, aria-expanded, announcement, done periphery);
  keyboard Enter (+ the card-target guard); compressed-header and
  chevron transits; arrow transits in focus mode; the frozen-geometry
  pin (scroll events never reflow; only the settled scrollend transits)
  and the re-snap target; settle-back no-op; overview scrollend no-op;
  chevron/Escape/pinch exits (+ sub-threshold pinch holds, menu-Escape
  guard); exit re-center; md force-exit (controllable matchMedia stub)
  + md:hidden; stub card hiding; announcer semantics; done-focused full
  page; drained-board entry.
- `frontend/tests/e2e/board.spec.ts` (+6, 52 total): portrait enter with
  real geometry (focused ≈70% of the rail, stubs ≈10%, done >90%,
  measured past the transition's re-snap anchor); the real
  scroll-to-settle transit (scrollend → lens moves → re-snap aligns);
  compressed-header tap transit; Escape exit re-centering on the last
  column's snap point (scrollLeft > 5, not zero); landscape rotation
  force-exit + hidden affordances; reload returning to overview.

## Evidence (exact head at push time)

- `npx vitest run` — 2747/2747 (2727 base + 20 new).
- `E2E_PORT=5207 npx playwright test tests/e2e/board.spec.ts` — 52/52.
- `npx tsc --noEmit` clean; `npx eslint src/pages/BoardPage.tsx
  src/pages/BoardPage.focus.test.tsx` — 0 errors (1 pre-existing
  exhaustive-deps warning, present on the base commit).
- `make fmt-check` clean (no Go changes).
