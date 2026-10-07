# Worklog: #1629 — overscroll-behavior-x + the restored full-edge gesture (the #1623/#1626 redo)

**Date:** 2026-10-07
**Session:** Replace the #1626 inward-offset drag-handle with the CSS-native full-edge gesture: `overscroll-behavior-x: none` (explicit longhands) on the root scroller + the restored a56430b7/#590 EDGE_ZONE contract; delete the strip, its safe-area positioning, and the claim-at-move scoping.
**Status:** Complete

---

## Objective

Owner ruling on #1629: the shipped #1626 handle (v0.34.19) is a horrible experience; research found the standard PWA mechanism we missed — the browser's horizontal swipe-back navigation is disabled at the CSS level (`overscroll-behavior-x: none` on the root element), so the full left edge can be the app's gesture zone again.

---

## Work Completed

### 1. Verification before code (Rule 7 assumptions, all validated)

- **`none` vs `contain`** (the issue asked to verify): MDN `overscroll-behavior-x` (fetched 2026-10-07) — `contain`: "no scroll chaining … **disables native browser navigation, including the vertical pull-to-refresh gesture and horizontal swipe navigation**"; `none` = the same chaining/nav suppression **plus** local overscroll effects prevented. Both kill the nav gesture; `none` is correct for an overflow-hidden app (wants neither bounce nor chaining) and is the shipped value → kept `none`, made explicit.
- **Honest finding:** `overscroll-behavior: none` (both-axis shorthand) was ALREADY on `html` and `body` — added 77850fc9 (2026-05-28) — and present through the whole #1623 window. "Add the property" is literally a no-op here; the redo's operative deltas are the explicit+pinned x longhand, the full-edge restore, and the handle deletion. Stated on the issue before the first push (issuecomment-6043903485).
- **Placement:** the swipe-nav gesture is a root-scroller overscroll action → effective on `html` (propagated from `body`); `#root` is a plain div where the property has no effect. Landed as explicit `overscroll-behavior-x/-y: none` longhands on both existing rules in `frontend/src/styles/index.css`.
- **Ruling adaptation (hamburger):** the ruling keeps "hamburger stays the always-works path" — but the restored touchstart claim covers x<30 and the hamburger's left half lives there (AppShell header `px-3` = 12px + 36px button). The #1626 r2 review PROVED empirically that a touchstart preventDefault over a tappable control suppresses its synthetic click. Fix: the claim (and gesture recognition) skips touches whose target is an interactive element (`button, a, input, textarea, select, [role=button/tab/option/switch]`). Controls are tap targets, not swipe origins.

### 2. Deleted (the #1626 shipped shape)

- `SidebarDrawer.tsx`: the entire inset grab strip (`data-sidebar-handle` div, `left-[calc(env(safe-area-inset-left,0px)+16px)]` positioning, grab-bar visual) + the `handleRef` destructure.
- `useSwipeableSidebar.ts`: `handleRef` option, `isHandleSwipe`, `startedInHandle()`, the claim-at-move scoping (`ours` check), `MIN_DRAG_PX`.
- `useCollapsibleSidebar.ts`: all `handleRef` plumbing.
- Tests: every handle-zone pin + the `data-sidebar-handle` selector (kept only as a negative pin guarding reintroduction).

Kept from #1626 (independent, real fixes): `viewport-fit=cover`, calc-composed safe-area paddings (r4), swipe-to-close semantics, shared Touch polyfill.

### 3. Restored (a56430b7/#590 shape, adapted)

- `EDGE_ZONE = 30`: touchstart records `isEdgeSwipe` (clientX < 30, non-interactive target) and claims via `preventDefault` (non-passive listener) — no OS race left to lose per the CSS mechanism.
- Blanket horizontal `preventDefault` at touchmove (original semantics; the dy>|dx| vertical guard unchanged).
- `EDGE_ZONE` as the tap threshold in `onEnd` (same 30px travel as #1626's `MIN_DRAG_PX` — no user-visible value change).

### 4. Test coverage (TDD: red → green)

- Hook suite rewritten to the full-edge contract: 28 tests — edge-start opens (incl. 29px inner-boundary pin), 30px/non-edge never open, touchstart claim poles (edge/29px/open-state/multi-touch), interactive-target claim-skip ×3, close-swipe family, vertical passthrough poles, visual tracking, blanket-move-claim pins, disabled/cleanup/persist.
- `SidebarDrawer.test.tsx`: strip-gone pin (selector null + exactly overlay+drawer children).
- `AppShell.test.tsx`: full-tree edge swipe opens (claim asserted), non-edge never opens/never claims, hamburger-in-band claim-skip through the real tree, container `overscroll-none`/`pan-y` surface pin.
- NEW `src/styles/index.test.ts`: reads the stylesheet source (vitest stubs CSS imports — `?raw` returns empty under `css:false`; node `fs` read instead, cwd-resolved) and pins x+y longhands on html/body + no shorthand regression.
- e2e `sidebar-gesture.spec.ts` rewritten (6 tests): runtime computed `overscrollBehaviorX === "none"` on documentElement+body, full-edge swipe opens through the real stack, non-edge doesn't, hamburger opens from BOTH halves of the button (left half inside the band — the r2 regression class, real touchscreen taps), viewport-fit pin.

### 5. Mutation verification (the r1 seam lesson)

- Delete the touchstart claim → 3 tests RED.
- Kill edge recognition (`isEdgeSwipe = false`) → 8 tests RED.
- CSS back to shorthand → 2 tests RED.
- Re-add the strip → drawer pin RED.

---

## Key Decisions

- `overscroll-behavior-x: none` (not `contain`): verified superset; identical to shipped computed style, now explicit/greppable/pinned. No behavioral CSS change — honestly stated; the behavior delta is the JS contract.
- Interactive-target claim-skip over a smaller EDGE_ZONE or "ship as #590": preserves the ruling's own "hamburger stays the always-works path" against r2's empirical click-suppression repro; costs nothing (CSS owns the back-nav lane; the move-claim still fires for real drags).
- No `#root` rule: dead declaration on a non-scroller div; the root scroller is where the mechanism works.

---

## Blockers

None. Device QA (iOS Safari / Android Chrome OS back-gesture lane) remains pending owner verification — CI cannot emulate the OS gesture (same honest posture as #1626 r1). iOS caveat stated on the issue: the property predates #1623 and the race was still observed, so if the owner's device still races, the next suspect is the iOS OS-level gesture (outside CSS/JS reach; standalone-PWA mode is the known full escape).

---

## Tests Run

- RED (pre-implementation): 17 failed / 33 passed across the four targeted files (interface + behavioral red); second cycle: 4 failed / 34 (interactive-guard red).
- Targeted GREEN: hook+drawer+AppShell+styles+useCollapsibleSidebar → 64/64.
- Full suite: vitest 1997/1997 (179 files).
- `tsc --noEmit`: 0 errors. `eslint`: 0 errors (6 pre-existing warnings).
- Playwright `sidebar-gesture` + `sidebar-hierarchy`: 9/9 local (real Chromium, hasTouch).
- 4 mutations each verified caught (above).

---

## Next Steps

- Merge → owner device-QA checklist on #1629 (same as #1626's).

---

## Files Modified

- frontend/src/hooks/useSwipeableSidebar.ts (+test) — restored EDGE_ZONE contract + interactive-target claim-skip
- frontend/src/components/layout/SidebarDrawer.tsx (+test) — strip deleted
- frontend/src/components/layout/AppShell.test.tsx — full-tree gesture + claim-skip + surface pins
- frontend/src/hooks/useCollapsibleSidebar.ts — handleRef de-plumbed
- frontend/src/styles/index.css — explicit overscroll-behavior-x/-y longhands on html/body
- frontend/src/styles/index.test.ts (new) — stylesheet pins
- frontend/tests/e2e/sidebar-gesture.spec.ts — #1629 rewrite
- COORDINATE.md (claim row), this worklog
