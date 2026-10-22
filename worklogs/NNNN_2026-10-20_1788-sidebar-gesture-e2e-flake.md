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
- Before the re-open tap, `waitForFunction` until the toggle is again the topmost element at its own center (`elementFromPoint` + `closest('button[aria-label="Open menu"]')` — name-pinned to the toggle in its closed-state labeling per review r2) — kills the slide-out race with no sleeps and no loosened assertions (left-half opens, closes, center opens; label flips verified both ways).

## Work Completed

- Diagnosis-first lane: pulled both failing job logs (118506390338, 118536660923), source-geometry analysis, temporary Playwright probe (race-window instrumentation + settled-state repro + fix-path validation), then the spec restructure above. Probe deleted pre-commit; its evidence is recorded in this worklog, the PR thread, and the issue thread (issuecomment-6304285916).
- Review iterations on PR #1804: r2 name-pinned the wait selector; r3/r4 worklog sentinel restorations (a local auto-numberer kept self-assigning numbers — reverted twice, per README-LLM.md the post-merge bot numbers worklogs); r6 rebase onto post-#1819 main (prior full-workflow build reds were deterministic stale-base toolchain skew, not an outage).

## Key Decisions

- Backdrop tap (350,400) chosen over swipe-to-close for the close step: a point the 256px panel can never cover at any transform position is deterministic at ANY animation age; the swipe path stays covered by the hook's own unit lanes.
- The slide-out wait is a hit-test (`elementFromPoint` topmost at the toggle's own center), not a sleep or a state-only assert — the precondition the re-open tap physically needs, pinned by name to the toggle.
- No product escalation: the covered header X while the drawer is open is standard drawer-over-header geometry with working close affordances (backdrop, swipe, navigation); AT activation dispatches on the node regardless of hit-testing. Recorded for the owner on the issue thread; no action ruled needed.

## Blockers

- None. (Historical note: rounds r3–r6 were evidence-head/stale-base and worklog-format churn, not spec defects.)

## Tests Run

- Diagnostic probe (temporary, deleted): race-window table + settled-state CI-signature repro + backdrop-close validation — 2/2 identical.
- Local, pre-PR: fixed row ×10 iterations (`--workers=1 --grep "both halves"`) → 10/10; full spec file → 5/5; `npm run typecheck` + eslint → clean.
- CI determinism, final heads (orchestrator-ratified instrument: full-workflow reruns on one head):
  - `4035ba54` (post-#1819 rebase, r6 head; run 39704085765): Frontend (unit + typecheck + e2e) **5/5 green**; full workflow fully green 3/5 (a1, a4, a5); isolated single-job reds: SDK Contract Tests (Hurl-vs-Prism step, exit 2) in a2 only, Origin plugin freeze pin in a3 only — each green in the other four executions of identical content; non-deterministic, outside this diff's surface. Table: PR #1804 issuecomment-6333320084.
  - Superseded heads: `556f9745` Frontend 5/5; `ce9fe72c` Frontend 8/8 with attempts 4–8 full-green (the a3 red there was the unrelated Go cron-timing test; builds on that head were pre-#1819 skew, corrected in the PR body per r6).

## Next Steps

- Merge (squash takes the PR title; post-merge bot numbers this worklog; "Fixes #1788" closes the issue). No further rebase expected — head is post-#1819; if main moves again before merge, rebase + a fresh 5× Frontend set on the new head per the standing rule.

## Files Modified

- frontend/tests/e2e/sidebar-gesture.spec.ts (+25/−4, test-only)
- worklogs/NNNN_2026-10-20_1788-sidebar-gesture-e2e-flake.md — this worklog
