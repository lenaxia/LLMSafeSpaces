# Worklog: #1626 r3 review — documentation-and-consistency convergence (and the pin that bit back)

**Date:** 2026-10-07
**Session:** Address the r3 verdict's four documentation-and-consistency items; one pin exposed a real code inconsistency and forced the fix
**Status:** Complete

---

## Objective

The convergence tail: PR-body accuracy, safe-area surfaces finished, comment/code alignment with the edge contract, two missing pins.

---

## Work Completed

### 1. PR body rewritten to the shipped mechanism

The r2-era body still described touchstart claiming and the dead-column tradeoff. Now states: recognition-at-start / claim-at-move / only-our-gestions; the input-transparent strip; edge-to-edge scrolling; the four safe-area consumers; the pinned contracts.

### 2. Safe-area consumers finished (no waivers needed)

- Portal header (`PortalLayout.tsx`) and drawer header (`Sidebar.tsx` top row): `pt-[env(safe-area-inset-top,0px)]` — full-bleed top surfaces under cover.
- Composer (`Composer.tsx`, bottommost row): `pb-[env(safe-area-inset-bottom,0px)]` — home indicator.

### 3. The edge contract — the pin found the code was the wrong side

The new pin ("never claims a horizontal drag that starts at the absolute edge, at any stage") FAILED: `onMove` still blanket-preventDefaulted every horizontal drag — including edge-started ones, which is precisely the OS-fighting the ruling retired (and it claimed unrelated content drags too). Fixed by scoping the move-stage claim to OUR gestures only (handle-start rightward drag, or leftward while open); everything else is left unclaimed at every stage. The hook's header comment now states exactly that. This is the r3 finding's real payload: "align the comment with the code" surfaced that the code needed aligning to the contract.

### 4. The strip-band vertical pass-through pin

Vertical gesture starting IN the handle band (x=30): never prevented at any stage — pinned.

### Mid-round incident (honesty note)

My first cut of the Composer consumer put a JSX comment in a bare-expression position — 16 suites failed to transform, 6 TS errors, Playwright starved. Caught by the full-suite gate, fixed (comment above `return (`), all suites re-run green on the final tree. Separately, the shared node_modules (wt-1453 symlink) raced the orchestrator's cache-clean mid-run producing transient suite failures — clean re-runs confirmed both trees green.

---

## Key Decisions

- Scope the move-stage claim to owned gestures rather than re-adding any start-stage claim — the ruling's logic all the way down (never claim what isn't ours, at any stage).

---

## Blockers

None in-repo.

---

## Tests Run

- New pins: edge-never-claimed (forced the code fix), strip-band vertical passthrough.
- Final tree: `vitest run src/` 1992/1992 (178 files); `tsc --noEmit` 0 errors; eslint 0 errors; `playwright test sidebar-gesture.spec.ts` 4/4 local.

---

## Next Steps

- Push, PR comment, notify. Issue closes on owner device QA (checklist stands).

---

## Files Modified

- frontend/src/hooks/useSwipeableSidebar.ts (claim scoped to owned gestures; comment aligned) + test (2 pins)
- frontend/src/components/layout/PortalLayout.tsx, layout/Sidebar.tsx, chat/Composer.tsx (safe-area consumers)
- PR body rewrite
- worklogs/1098_2026-10-07_sidebar-gesture-r4.md (this worklog)
