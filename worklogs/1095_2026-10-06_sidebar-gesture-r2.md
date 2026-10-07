# Worklog: #1626 r1 review — integration/e2e legs, the viewport-fit correction, and honest device-QA status

**Date:** 2026-10-06
**Session:** Address the #1626 CHANGES_REQUESTED verdict: missing test levels (the load-bearing seam was untested — proven by reviewer mutation), a failed Rule-7 validation (viewport-fit=cover was claimed "already audited" but appears nowhere in the repo), partial acceptance on device QA, and three minors
**Status:** Complete

---

## Objective

Close the r1 gates without weakening the honesty bar the review enforced.

---

## Work Completed

### 1. Test levels (hard gate)

- **Integration (layouts → hook → drawer)**: two new `AppShell gesture integration` tests drive REAL touch events through the full AppShell tree (memory router + AuthProvider + QueryClient), locating the rendered handle strip and pinning its jsdom rect at the shipped geometry. **Mutation-verified against the reviewer's exact deletion**: removing `ref={handleRef}` from SidebarDrawer makes the handle-zone test RED (previously all 36 tests stayed green — the seam was untested). The absolute-edge leg pins the full-tree non-open.
- **Playwright e2e** (`tests/e2e/sidebar-gesture.spec.ts`, hasTouch mobile context): real Chromium, real layout — the handle's live boundingBox asserted ≥16px in; a handle-zone swipe (real hit-testing via `elementFromPoint`) opens the drawer; an absolute-edge swipe leaves it closed; the viewport meta is pinned at runtime. **3/3 passing locally** (chromium headless-shell installed; the 35-spec suite gains its 36th).
- jsdom's missing `Touch` constructor (jsdom 20) was polyfilled in the shared `src/test/setup.ts` (extracted from the hook suite's file-local polyfill) so integration tests can dispatch touch sequences.

### 2. The Rule-7 failure, corrected

The r1 worklog claimed `viewport-fit=cover` "was already audited" — the reviewer verified it appears NOWHERE in the repo (zero commits). **The claim was wrong; without `viewport-fit=cover`, `env(safe-area-inset-left)` is INERT in browsers and the handle sat at a fixed 16px on every device — the notched-device geometry the fix bets on was unproven.** Fixed in code: `viewport-fit=cover` added to `frontend/index.html`'s viewport meta, pinned at runtime by the e2e meta test. The r1 worklog stands append-only; this entry is the correction.

### 3. Device QA — stated honestly, not claimed

The acceptance criterion "opens 100% on deliberate gesture on iOS Safari + Android Chrome" cannot be proven from this environment; the #590 precedent (jsdom-green, device-broken) is exactly why. In-repo levels now cover everything up to the OS boundary: unit (recognition/claim semantics), integration (the wired tree), e2e (real browser, real layout, full stack). The OS-gesture lane itself — handle-zone swipe vs the iOS back-gesture curve on a notched device, plus Android Chrome gesture-nav — is **pending owner device verification**, checklist posted to the PR:
1. iOS Safari, notched device: swipe from the visible handle strip → sidebar opens, no back-nav, every time.
2. iOS Safari: swipe from the absolute screen edge → OS back-nav fires (app code leaves it untouched).
3. Android Chrome (gesture nav): both of the above.
4. Vertical scroll starting on the handle strip is intentionally captured (the affordance tradeoff); everywhere else scrolls.

### 4. Minors

- **Hitchhiker disclosed**: the source-map-js 1.2.2 bump (CVE-2026-93749) rides this branch identically to #1621/#1625 (repo-wide Trivy red; per-branch carriage endorsed by the orchestrator) — now stated in the PR body.
- **"Edge-to-edge scrolling" overstated — reworded**: the handle strip is a VISIBLE 28px dead column for scrolling (touch-action:none), by design; the honest statement is "edge-to-edge except the visible handle affordance" vs the old INVISIBLE 30px block. PR body and the in-code comment corrected.
- **Self-contradictory comment fixed**: the handle is offset CLEAR of the OS back-gesture zone (not "inside the curve") — both comment blocks reworded.

---

## Key Decisions

- The e2e dispatches on `document.elementFromPoint(x, y)` — real hit-testing — rather than a locator's element: the test fails if the strip moves, is covered, or fails to render, not just if the handler logic breaks.
- Polyfill hoisted to setup.ts rather than another file-local copy.

---

## Blockers

None in-repo. Device QA (above) is owner-side and does not block the code review.

---

## Tests Run

- Mutation: ref-deletion → integration RED; restored → green (copy-protected per mutation hygiene).
- `vitest run src/` — 1990/1990 (178 files; +2 integration).
- `playwright test tests/e2e/sidebar-gesture.spec.ts` — 3/3 (real Chromium).
- `tsc --noEmit` — 0 errors; `eslint` — 0 errors (6 pre-existing generated-file warnings).

---

## Next Steps

- Push r2, PR comment addressing each finding, notify the orchestrator.

---

## Files Modified

- frontend/index.html (viewport-fit=cover)
- frontend/src/hooks/useSwipeableSidebar.ts, frontend/src/components/layout/SidebarDrawer.tsx (comment corrections)
- frontend/src/components/layout/AppShell.test.tsx (integration leg)
- frontend/src/test/setup.ts (shared Touch polyfill)
- frontend/tests/e2e/sidebar-gesture.spec.ts (e2e leg)
- worklogs/1095_2026-10-06_sidebar-gesture-r2.md (this worklog)
