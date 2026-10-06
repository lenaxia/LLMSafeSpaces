# Worklog: #1623 — the inward-offset gesture zone (stop fighting the OS back-gesture)

**Date:** 2026-10-06
**Session:** Implement the owner's Option A ruling on the swipe-to-open/back-nav conflict: retire the absolute-edge touchstart claim, add the inset drag-handle affordance, keep swipe-to-close and the hamburger; red-first jsdom pins
**Status:** Complete

---

## Objective

Edge-swipe-to-open triggered browser back-navigation ~50% of the time on iOS: the OS captures its back-gesture before page JavaScript often enough that no page technique wins reliably (the deployed a56430b7/#590 preventDefault-at-touchstart fix was provably insufficient — it did everything a page can do, and the OS still won half the races). Acceptance: the sidebar opens 100% on deliberate gesture, the OS edge-swipe stays back-nav, scrolling works edge-to-edge.

---

## Work Completed

### The redesign (Option A, owner ruling 2026-10-05)

- **`useSwipeableSidebar.ts`** — `EDGE_ZONE`/`isEdgeSwipe` and the absolute-edge touchstart `preventDefault` REMOVED (with it, the 30px vertical-scroll block — its acknowledged tradeoff). The open-gesture now engages only for touches starting inside the **drag-handle strip's live rect** (`startedInHandle` via `getBoundingClientRect` — safe-area insets honored by construction); ONLY that zone is claimed at touchstart (inset from the absolute edge ⇒ outside the OS gesture curve ⇒ no race). The tap-to-open threshold became `MIN_DRAG_PX` (same 30px travel, now unrelated to any edge). Swipe-to-close (drawer surface) untouched; the hamburger stays the always-works path.
- **`useCollapsibleSidebar.ts`** — `handleRef` plumbed through the state contract.
- **`SidebarDrawer.tsx`** — the affordance: a 28px-wide full-height strip starting 16px in from `env(safe-area-inset-left, 0px)` (Tailwind arbitrary-value class — `left-[calc(env(safe-area-inset-left,0px)+16px)]`), `touch-action: none` (a gesture affordance is not a scroll surface), a visually subtle centered grab bar (`h-12 w-1 rounded-full bg-foreground/25`), rendered only when mobile AND closed (open state: swipe-to-close lives on the drawer), `aria-hidden` (the hamburger is the accessible open path).

### Tests (red-first, 8 RED / 13 GREEN against the pre-change hook, commit 1cabb3e2)

RED pins (the new contract): handle-zone swipe opens; absolute-edge swipe neither opens NOR claims (the OS zone); handle-zone touchstart claims; no claim when open; edge-start vertical gestures un-intercepted at touchstart (edge-to-edge scrolling); handle-zone visual tracking; settle threshold at handle starts.
GREEN poles (unchanged semantics): close-swipe family, outside-zone no-open, multi-touch, disabled, cleanup/persist, touchmove-prevention poles.
Component pins (SidebarDrawer): handle rendered mobile+closed; positioned via the safe-area class; absent when open; absent on desktop.
Toolchain note: jsdom's CSSOM silently drops `env()` from inline styles (React writes via the CSSOM, so even the attribute is empty) — asserted the arbitrary-value class instead.

### Verification

Full frontend suite 1988/1988; `tsc --noEmit` clean; eslint 0 errors (6 pre-existing warnings in generated `abi/*_pb.ts`, untouched).

---

## Key Decisions

- The hit test uses the handle's **live rect** rather than hardcoded coordinates: the CSS (safe-area class) stays the single source of positioning truth; jsdom tests mock the rect (no layout engine).
- The strip blocks vertical scrolling ON ITSELF (`touch-action:none`) by design — a deliberate affordance owning its touches, vs the old design silently blocking the leftmost 30px of PAGE content.
- z-40 (drawer's layer): the handle overlays page content only while the drawer is closed; the drawer itself is fully off-screen then.

### Assumptions stated and validated (Rule 7)

1. `SidebarDrawer` renders inside AppShell's swipe-wired container (touchstart bubbles to the container listeners) — verified in `AppShell.tsx` (containerRef at line 33; SidebarDrawer within).
2. The old claim block's four pins described the RETIRED behavior — rewritten to the new contract in the red commit (the inverted assertions are the red evidence).
3. `env(safe-area-inset-left)` needs `viewport-fit=cover` to reflect notched insets — the meta tag was already audited by the original #590 work; the fallback `0px` keeps unnotched devices at exactly 16px.

---

## Blockers

None.

---

## Tests Run

- Red stage: 8 failed / 13 passed against the pre-change hook (the exact expected split).
- Final: `vitest run src/` — 1988 passed (178 files), including the 4 new component pins.
- `npm run typecheck` clean; `npm run lint` 0 errors.

---

## Next Steps

- PR, review iterate to APPROVED, notify the orchestrator.

---

## Files Modified

- frontend/src/hooks/useSwipeableSidebar.ts (+test)
- frontend/src/hooks/useCollapsibleSidebar.ts
- frontend/src/components/layout/SidebarDrawer.tsx (+test)
- worklogs/NNNN_2026-10-06_sidebar-gesture-zone.md (this worklog)
