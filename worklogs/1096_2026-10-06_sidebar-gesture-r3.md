# Worklog: #1626 r2 review — the tap-through fix, the viewport-fit consumer, comment hygiene

**Date:** 2026-10-06
**Session:** Address the r2 CHANGES_REQUESTED: the handle strip empirically broke the hamburger (occlusion), the app-wide viewport-fit=cover question, the stale test-file comment
**Status:** Complete

---

## Objective

Close the r2 gates: restore "hamburger always works" under the strip, resolve the viewport-fit question with a real consumer, fix the stale comment.

---

## Work Completed

### 1. The occlusion fix — recognition at start, claim at move, strip input-transparent

The reviewer reproduced it: the full-height z-40 strip sat over the hamburger (header `px-3` puts the button's center at x≈28 — inside [16,44]) and swallowed taps. Two coupled causes, both fixed:

- **The strip is now `pointer-events: none`** — purely visual plus the coordinate region the hook's rect check reads. Taps under it hit the real targets (hit-testing skips it); vertical scrolls over it reach the content beneath (the r1 "visible 28px dead column" tradeoff DISSOLVES — scrolling is genuinely edge-to-edge now). The strip gained `data-sidebar-handle` as the stable test selector (the old `touch-none` selector died with the class; `pointer-events-none` collides with the closed overlay's class).
- **The touchstart claim is REMOVED** (it suppressed synthetic clicks — the other half of the broken tap). The strip is inset clear of the OS zone, so beating the OS never required a touchstart claim there; the gesture is claimed at the qualifying horizontal MOVE (onMove's existing `preventDefault` — required anyway to block scroll mid-drag).
- **Mutation-verified**: removing the strip's transparency turns the new e2e tap-through test RED (real hit-testing via `touchscreen.tap` at the button's own center — the exact repro shape).

### 2. viewport-fit=cover — resolved with a consumer, not a waiver

The one surface cover newly exposes is the top inset (the header is the topmost content row of the h-screen root): the mobile header now carries `pt-[env(safe-area-inset-top,0px)]`. The other full-bleed surfaces are chrome by design (the drawer's own edge; the handle consumes the left inset in its calc — that's its purpose). No waiver needed; the consumer is real and the rationale is in-code.

### 3. Stale comment (test file) — corrected

The r1 test-file comment block still said the handle zone "starts inside the OS back-gesture zone's curve" (the phrasing fixed in source at r1, missed in the test file). Rewritten to the actual contract (inset CLEAR of the zone; recognition-at-start/claim-at-move; why no touchstart claim preserves tap-through).

### 4. Device QA

Checklist unchanged, still pending owner verification, still stated in the PR.

---

## Key Decisions

- Coordinate-region recognition makes the strip's INPUT role unnecessary — `pointer-events:none` is strictly better than z-reordering (no geometry coupling to the header) or conditional hit areas.
- The tap-through pin lives in Playwright, not jsdom: the occlusion was a hit-testing bug, and jsdom has no hit-testing — only the real browser can catch this class (the r1 suite's blindness to it proves the level gap).

---

## Blockers

None in-repo.

---

## Tests Run

- Mutation: strip transparency removed → e2e tap-through RED; restored → 4/4 green.
- `vitest run src/` — 1990/1990; `tsc --noEmit` 0 errors; eslint 0 errors (6 pre-existing generated-file warnings); `playwright test sidebar-gesture.spec.ts` — 4/4 local.

---

## Next Steps

- Push r3, PR comment, notify the orchestrator.

---

## Files Modified

- frontend/src/hooks/useSwipeableSidebar.ts (+test — claim block inverted, comment corrected)
- frontend/src/components/layout/SidebarDrawer.tsx (+test — selector/pointer-events pins)
- frontend/src/components/layout/AppShell.tsx (+test) — header safe-area consumer
- frontend/tests/e2e/sidebar-gesture.spec.ts — tap-through + transparency pins
- worklogs/1096_2026-10-06_sidebar-gesture-r3.md (this worklog)
