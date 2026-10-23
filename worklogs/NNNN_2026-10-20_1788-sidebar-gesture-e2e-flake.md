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
- **Superseded in-tree by main's equivalent (r11 round):** #1805 (`dc7aefd0`) landed the same-shape remedy on main (backdrop tap at viewport-relative `(width−60, height/2)` + name-pinned monotonic hit-test wait + both-way label flips). This branch now carries main's spec verbatim — this lane's remaining unique content is the diagnosis record, evidence set, and this worklog.

## Work Completed

- Diagnosis-first lane: pulled both failing job logs (118506390338, 118536660923), source-geometry analysis, temporary Playwright probe (race-window instrumentation + settled-state repro + fix-path validation), then the spec restructure above. Probe deleted pre-commit; its evidence is recorded in this worklog, the PR thread, and the issue thread (issuecomment-6304285916).
- Review iterations on PR #1804: r2 name-pinned the wait selector; r3/r4 worklog sentinel restorations (a local auto-numberer kept self-assigning numbers — reverted twice, per README-LLM.md the post-merge bot numbers worklogs); r6 rebase onto post-#1819 main (prior full-workflow build reds were deterministic stale-base toolchain skew, not an outage); r9 union-merge of origin/main `cef43acf` (143 commits — new playwright harness with `E2E_PORT`, 3 new e2e specs, board surface) per the stale-base finding; spec passed the new harness first try (canary run 39864417270).
- Sibling-fix convergence (r11 round): main advanced through #1805 (`b64b1ff5`, incl. `dc7aefd0` "test(frontend): #1793 lane — de-flake sidebar-gesture hamburger close step") — an equivalent remedy for the SAME row landed on main while this branch collected evidence. Per the standing head-pinned rule, union-merged origin/main (`40c4d51e`); the spec conflicted (both sides restructured the same close/re-open block) and was resolved by adopting **main's version verbatim** — verified semantically equivalent to this lane's remedy (backdrop close; monotonic name-pinned hit-test wait `closest('button[aria-label="Open menu"]')`; label flips asserted both ways; no sleeps). This branch's diff vs main is now this worklog only.

## Key Decisions

- Backdrop tap (350,400) chosen over swipe-to-close for the close step: a point the 256px panel can never cover at any transform position is deterministic at ANY animation age; the swipe path stays covered by the hook's own unit lanes.
- The slide-out wait is a hit-test (`elementFromPoint` topmost at the toggle's own center), not a sleep or a state-only assert — the precondition the re-open tap physically needs, pinned by name to the toggle.
- No product escalation: the covered header X while the drawer is open is standard drawer-over-header geometry with working close affordances (backdrop, swipe, navigation); AT activation dispatches on the node regardless of hit-testing. Recorded for the owner on the issue thread; no action ruled needed.

## Blockers

- None. (Historical note: rounds r3–r6 were evidence-head/stale-base and worklog-format churn, not spec defects.)

## Tests Run

- Diagnostic probe (temporary, deleted): race-window table + settled-state CI-signature repro + backdrop-close validation — 2/2 identical.
- Local, pre-PR: fixed row ×10 iterations (`--workers=1 --grep "both halves"`) → 10/10; full spec file → 5/5; `npm run typecheck` + eslint → clean.
- CI determinism (orchestrator-ratified instrument: the **Frontend (unit + typecheck + e2e) lane** — 5 consecutive green executions of that job on the final head; NOT full-workflow reruns):
  - **Final head (r15 union-merge of origin/main `1335fa8a` + this worklog sync; the branch is quiet here):** the criterion-2 set (5 consecutive green Frontend executions of this exact head) is collected AFTER this push via CI **`workflow_dispatch`** on this branch ref — the pull_request lane structurally cannot supply it anymore on a worklogs-only diff (ci.yml's docs-only fast path skips the Frontend job; observed on this head's PR runs 39889863122/39898725094, Frontend = skipped), while push/dispatch events bypass that gate (ci.yml `frontend-test.if`); the dispatch run's consecutive attempts (run rerun) re-execute the Frontend job each time. The per-attempt table (run/job ids, conclusions) is posted on the PR #1804 thread and linked from the PR body Green section naming this head. Nothing here is asserted as already collected.
  - Superseded heads: `1ce1b671` (r13 docs head): dispatch run `39898731768` a2–a6 — **Frontend 5/5 green, zero completed reds** (jobs 120243814010, 120270412887, 120288585425, 120307065782, 120322508926; verified by r15 against the API); a1 of that run was cancelled mid-Playwright-e2e (job 120236539677 ran 5m16s; cancel ~2min into the e2e step — not "before any job executed"); set VOID for merge per r15: main advanced 86 commits past the merge-base incl. 9 frontend files (2 e2e specs + production code), falsifying r14's zero-frontend-delta ratification premise — the standing rule fired. `627adde6` (post-#1805 union-merge, pre-docs-sync): dispatch run `39890521675` a1 — Frontend job 120208961185 green, full workflow green; superseded by the r13 docs commit per the head-pinned rule. `c8437fc2` (pre-#1805 head): 3 of 5 Frontend greens collected on run `39878451660` — a2 (full workflow green), a3 (Frontend job 120195356507 green; run-level red from a non-Frontend job, outside the instrument per r8 precedent), a4 (Frontend job 120202921064 concluded success at 02:26:17Z, before the run-level cancellation at 02:27:05Z); that a4 cancellation was a deliberate `gh run cancel` issued by this lane once #1805's merge superseded the head — not a concurrency effect (PR-ref runs are auto-cancelled only by same-ref pull_request events) — set void per the standing rule. `655430cc` (r9 worklog-sync head; run 39866606366): Frontend green a1 (full workflow green), a2 (full workflow green), a3 (Frontend job 120158613746 green; build tail cancelled by the `c8437fc2` branch sync's same-ref concurrency cancel-in-progress). `80a5949b` (r9 union-merge of origin/main `cef43acf` — new playwright harness (E2E_PORT) + 3 new e2e specs): Frontend job green in run 39864417270 before that run was cancelled by the follow-up push (run-level conclusion: cancelled). `6b56edf8`: full workflow green incl. Frontend (run 39733129550). `4035ba54` (post-#1819 rebase, r6 head; run 39704085765): Frontend **5/5 green**; full workflow fully green 3/5 (a1, a4, a5); isolated single-job reds: SDK Contract Tests (Hurl-vs-Prism step, exit 2) in a2 only, Origin plugin freeze pin in a3 only — each green in the other four executions of identical content; non-deterministic, outside this diff's surface. Table: PR #1804 issuecomment-6333320084. `556f9745` Frontend 5/5; `ce9fe72c` Frontend 8/8 with attempts 4–8 full-green (the a3 red there was the unrelated Go cron-timing test; builds on that head were pre-#1819 skew, corrected in the PR body per r6).

## Next Steps

- Merge (squash takes the PR title; post-merge bot numbers this worklog; "Fixes #1788" closes the issue). Standing rule (replaces the r7 "no further rebase expected" premise, stale since main advanced 143 commits — and again at #1805): the criterion-2 evidence is head-pinned — if main moves again before merge and the branch is merged forward, a fresh 5× Frontend set on the new head supersedes the current one. No "branch goes quiet" premise is asserted after r10: #1805's merge proved the rule fires as long as the PR is open.

## Files Modified

- frontend/tests/e2e/sidebar-gesture.spec.ts — originally +25/−4 (test-only); since the r11 union-merge, byte-identical to main's `dc7aefd0` (zero spec diff vs main)
- worklogs/NNNN_2026-10-20_1788-sidebar-gesture-e2e-flake.md — this worklog
