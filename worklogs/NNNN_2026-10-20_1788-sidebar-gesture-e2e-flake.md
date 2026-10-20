# Worklog: #1788 — sidebar-gesture e2e flake: covered-toggle race (backdrop close + slide-out hit-test wait)

**Date:** 2026-10-20
**Session:** Frontend e2e flake lane (#1788), polluting every PR lane regardless of diff content. Same class as #1779's escalation pattern.
**Status:** Complete

---

## Objective

`tests/e2e/sidebar-gesture.spec.ts:113` ("hamburger opens the drawer from both halves of the button (the always-works path)") failed intermittently at :134 — `getByRole('button', { name: 'Open menu' })` "element(s) not found" (5s) after `page.touchscreen.tap` on the close button's center. Hit PR #1781 (zero frontend diffs) 2×/3 runs. Diagnose first; no test-loosening over a product bug; keep the spec NAME.

---

## Root Cause (evidence, deterministic local repro 2/2)

- Geometry: on mobile with the drawer open, `SidebarDrawer` renders panel `fixed inset-y-0 left-0 z-40` (256px, full height incl. header) + overlay `fixed inset-0 z-30`; the header toggle (`SidebarToggleButton`, measured box {x:12,y:8,w:36,h:36}) sits z-auto UNDER both. The same physical button flips aria-label Open↔Close menu (`SidebarToggleButton.tsx:13`).
- The row's close-tap (old :133) tapped the toggle's center (30,26) while the drawer was OPEN — a covered point. Measured hit target (rAF-sampled `elementFromPoint`): t≈10ms → overlay (its onClick closes — the test passed BY ACCIDENT); t≥~40-60ms of the 200ms slide-in → the sidebar's own content (h1 "Safe Space", no handler) → drawer stays open → label stays "Close menu" → `Open menu` = 0 elements → the exact CI signature. Which side of ~40ms the assert→boundingBox→tap round-trips land on is runner-load-dependent: the flake.
- Settled-state repro of the exact CI failure: 2/2 local runs (openMenuNotFound=true, label "Close menu", aria-expanded=true after tap+5s).
- Second race, same family: the re-open tap (old :135) at the toggle center raced the slide-OUT (panel still covering the toggle after the close state-flip) — reproduced 2/2. Explains why CI always surfaced at :134 (it gates :135).
- vite ECONNREFUSED bursts DISPROVEN as causal: un-mocked API calls from OTHER specs proxied to a nonexistent backend (1941 lines in a phase where 219/220 tests pass); this spec boots, auths, and passed :117/:128 in the failing runs. The locator semantics (same button, flipped name) prove the app was mounted and the drawer open.
- Product-defect check: NONE. The hamburger opens deterministically from both halves whenever tappable (closed-state hit = the button itself). Open-state close affordances = backdrop + swipe-left (backdrop proven 2/2). The header X being pointer-covered while the drawer is open is standard drawer-over-header geometry; AT activation still works. No escalation, per the lane constraint.

## Fix (spec restructure only — zero production diff; name kept)

- Close via backdrop tap at (350,400) — beyond the 256px panel's reach at every transform position; deterministic at any animation age.
- Before the re-open tap, `waitForFunction` until the toggle is again the topmost element at its own center (`elementFromPoint` + `closest('button[aria-expanded]')`) — kills the slide-out race with no sleeps and no loosened assertions (left-half opens, closes, center opens; label flips verified both ways).

## Tests Run

- Diagnostic probe (temporary, deleted): race-window table + settled-state CI-signature repro + backdrop-close validation — 2/2 identical.
- `npx playwright test sidebar-gesture --workers=1 --grep "both halves"` × 10 iterations → 10/10 passed.
- `npx playwright test sidebar-gesture --workers=1` (full file) → 5/5 passed.
- `npm run typecheck`, `npx eslint tests/e2e/sidebar-gesture.spec.ts` → clean.
- Determinism instrument (per orchestrator): N=5 full-workflow reruns on the final head, GitHub-side.

## Next Steps

- PR review loop to APPROVED → rebase-current → N=5 reruns green on final head → final packet.

## Files Modified

- frontend/tests/e2e/sidebar-gesture.spec.ts (+22/−4, test-only)
- worklogs/NNNN_2026-10-20_1788-sidebar-gesture-e2e-flake.md — this worklog
