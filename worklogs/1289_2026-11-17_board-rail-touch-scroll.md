# Worklog: the rail touch-scroll fix (tsk-rugged-tabby-puzzles)

## Context

Owner report on release v0.44.15 (2026-11-16): the portrait 1x4 rail
renders correctly (the active trio at 95%, done discoverable at the 5%
edge) but horizontal swiping does NOTHING — done can never be scrolled
into view, swiping moves nothing, the focus-column lens never engages
via swipe transit. "The rail is visually right and functionally dead."

The task's prime suspect — total content width built to equal the
viewport (no overflow to scroll into) — was DISPROVEN FIRST: the rail
already overflows (measured `scrollWidth 713 > clientWidth 364` at
390x844); done renders its full page in CONTENT (`w-[calc(100%+0.75rem)]`
from #2077), and the existing e2e pin `railMetrics.scrolls === true` was
green on main. The 95/5 split is the VIEWPORT framing, not the content
width — that part of the shipped geometry is correct.

## The diagnosis (measured with trusted touchscreen input)

None of the existing rail tests drive a real touch pan: they scroll
programmatically (`el.scrollTo`, `scrollIntoView`, keyboard), and those
paths bypass `touch-action` entirely and never run a touchmove's
default action — which is exactly why CI was green while the owner's
finger was dead. The repro is CDP `Input.dispatchTouchEvent`
(touchStart → touchMove ×N → touchEnd) in a `hasTouch: true, isMobile:
true` context at 390x844, instrumented with a window-level touchmove
listener recording `defaultPrevented`:

1. **As shipped**: every horizontal-dominant touchmove arrives
   `defaultPrevented: true`; `scrollLeft` stays 0. Cause #1:
   `useSwipeableSidebar`'s non-passive `touchmove` on the AppShell root
   called `preventDefault()` on EVERY horizontal-dominant move app-wide
   — claimed or not — canceling the browser's native pan before it
   began.
2. **touch-action freed everywhere, preventDefault still live**: still
   `prevented: true`, still 0 — the preventDefault alone is sufficient
   to kill the pan.
3. **preventDefault neutralized too**: `prevented: false`,
   `scrollLeft` settles at 349 (= maxScroll) with done fully in view —
   the rail, its snap points and its reachability geometry all work
   once both suppressions lift. Cause #2 (latent behind #1):
   `touch-action: pan-y` on `html`, `body`, `AppShell`, `PortalLayout`
   — touch-action intersects along the ANCESTOR chain, so a root pan-y
   forbids horizontal touch panning for every descendant (a descendant
   can never re-enable what an ancestor disabled).

Both suppressions predate the rail (#590/#1629/#117 lineage: the
sidebar edge gesture and the swipe-back/back-nav remediation). The rail
was the first app surface that NEEDED horizontal touch panning.

## The fix

- `frontend/src/hooks/useSwipeableSidebar.ts` — `onMove` claims (and so
  cancels the default for) ONLY the gestures the sidebar owns: an
  edge-zone rightward drag opening a closed drawer, or a leftward drag
  closing an open one. Everything else — including horizontal swipes
  over the board — passes through uncanceled so the native pan runs.
  History swipe-back stays dead regardless: `overscroll-behavior-x:
  none` on html/body (the load-bearing #1629 declaration per its own
  comment) owns that, and edge-zone touches were already claimed at
  touchstart.
- `frontend/src/styles/index.css` (html + body), `AppShell.tsx`,
  `PortalLayout.tsx` — `touch-action: pan-y` → `pan-x pan-y`. The
  document never scrolls horizontally (h-dvh overflow-hidden shells),
  so pan-x changes nothing about the viewport; it only un-restricts
  sanctioned nested scrollers. Pinch-zoom and double-tap-zoom stay
  disabled (any non-auto value disables them — pan-x pan-y is not
  auto). Rationale comments at all four sites.
- NO geometry changes: the rail's widths, snap, focus-column mode,
  Done cap + drawer, skeletons — all byte-identical. The interaction
  spec was right; two global gesture suppressors were sitting on it.

## The pins

- **NEW `frontend/tests/e2e/board-rail-touch.spec.ts`** (the born-red
  spec): trusted-touch swipes with a control-calibrated shape — 5px
  move steps (the ~300px/s regime delivers ≈0.85× the drag distance;
  measured), a stationary deceleration tail (zero release velocity, no
  fling), and geometry-sized drags off the measured snap boundaries
  (the endpoint must clear `((neighbor.hi + target.lo)/2)`, ranges not
  points because done's full page is wider than the snapport — CSS
  snap admits [337,349], not a single position). Born-red record on
  pristine main: the three touch pins RED (swipe pans nothing,
  snap-per-column dead, focus swipe-transit dead), keyboard + landscape
  GREEN; with the fix 5/5, three consecutive runs.
  - born-red: a real swipe pans the rail and done becomes FULLY
    visible; arrow keys reach done too (keyboard parity on the phone).
  - snap lands per column, both directions, asserted by settled
    scroll position inside each column's valid snap range.
  - focus mode under real touch: chevron tap enters (widths after the
    200ms transition + the explicit re-snap), a swipe TRANSITS the lens
    via the native scrollend, Escape exits re-centered on the column
    just left; overview is the default on every mount.
  - landscape (md+): all four columns visible in one row, no scroll,
    chevrons hidden (md:hidden keeps them in the DOM — asserted
    hidden, not absent), and a swipe pans nothing.
  - Hazards the spec now encodes (each one cost a debugging round):
    the board reflows after the epic-band fetch (layout-stability gate
    before reading geometry — an off-rail touch pans nothing); the
    sidebar's 30px edge zone CLAIMS any touch that starts inside it
    (swipe start points are floored above it); the mobile drawer can
    auto-open (deterministically closed first); and a swipe start on a
    draggable card risks the DnD pipeline (safeSwipeStart scans for a
    rail-owned point).
- `frontend/src/hooks/useSwipeableSidebar.test.ts` — the old
  "blanket horizontal claim" pin (which pinned the exact behavior that
  killed the rail) FLIPPED to the new contract, with the rationale at
  the anchor; plus a five-arm "claim scoping (the rail's native pan
  must survive)" describe: unclaimed moves (non-edge leftward/
  rightward, rightward-while-open) pass uncanceled; the edge-open and
  open-close gestures still cancel.
- `frontend/src/styles/index.test.ts` — `pan-x pan-y` pinned on
  html/body, bare `pan-y` pinned away.
- `frontend/src/components/layout/AppShell.test.tsx` — the shell
  root's `pan-x pan-y` pinned (was `pan-y`).

## Evidence

- `E2E_PORT=5199 npx playwright test tests/e2e/board-rail-touch.spec.ts`
  — 5/5, three consecutive runs (27s each after the direction fix).
- Born-red on pristine origin/main (8ed843500) with the same spec:
  3 failed / 2 passed — the three touch pins red, keyboard + landscape
  green.
- `E2E_PORT=5199 npx playwright test tests/e2e/board.spec.ts
  tests/e2e/sidebar-gesture.spec.ts tests/e2e/app-shell-viewport.spec.ts`
  — 60/60 (the #1629 edge gesture end-to-end, the full board suite,
  the focus-column suite, the app-shell viewport pins — all hold).
- `npm test` — 2753/2753 full.
- `npm run typecheck` clean; `npm run lint` 0 errors (7 pre-existing
  warnings: generated `*_pb.ts` unused-disable directives and
  BoardPage's pre-existing exhaustive-deps, present on base).
- `make fmt-check` clean (no Go changes).
