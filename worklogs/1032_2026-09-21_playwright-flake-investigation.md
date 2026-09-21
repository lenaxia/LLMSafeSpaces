# Worklog: #1520 — the Playwright setInputFiles dispatch race (E-family e2e flakes)

**Date:** 2026-09-21
**Session:** Investigate the accumulated frontend-job flakes (E4 ×2+, passkey, E5, E6 one-offs) as a suspected shared-infrastructure class; localize with evidence, fix; branch `fix/playwright-flake-investigation`
**Status:** Complete

---

## Objective

Determine whether the four flake signatures share one infrastructure cause; fix at the right layer.

---

## Work Completed

- **CI evidence harvest:** three failing frontend-job runs pulled and diffed — E4+E5 (run 35566585334), E1+E6+passkey (run 35607825528) — every failure is the chip-never-appears/element-not-found shape, specs unrelated to their touching diffs.
- **Local repro:** the attachments spec flakes ~30-50% per full run on this pod (cold vite + default workers ≈ CI shape); isolated single-test runs pass — the load-churn shape.
- **The three-mechanism probe (the decisive experiment):** a probe spec with a document-capture change listener + input-remount poller ran the same page state through `page.setInputFiles`, `locator.setInputFiles`, and a single-turn evaluate dispatch. Result: Playwright's two APIs delivered the change event to the document ZERO times across failing iterations (while reporting success); the evaluate dispatch delivered it 6/6 with `connected:true`. Mechanism: the CDP round-trip between element resolution and dispatch straddles composer remounts (StrictMode dev double-mount + the post-load re-render wave); the event lands on a detached node, which reaches neither React's root delegation nor even a document-capture listener. UPLOAD_POST never fires; the 15s chip waits time out.
- **The fix:** `helpers/attachFiles.ts` — single-turn dispatch (live-node query + DataTransfer + bubbling input/change in one evaluate), with a bounded waitForSelector for the input's late-mount window (the E5 residual, isolated as "input not found in the DOM" in a follow-up failure). All 5 spec call sites converted; the spec's inline mocks extracted to `attachments-helpers.ts`; the helper's own regression rows added (`helpers/attachFiles.spec.ts`, single + multi file).
- **The passkey one-off:** 4/4 clean local full-suite runs — not reproduced; classified contention-colocation (its 10s ceremony wait vs the attachments retry storm on the sibling worker), plausibly retired by the E-family fix, stated as residual-observation in the issue.

---

## Key Decisions

- **Fix at the helper layer, not the app layer:** the remount churn is dev-mode React behavior (StrictMode) that cannot be "fixed" in the app without weakening dev-mode guarantees; the correct seam is the test-side dispatch. The helper documents the mechanism in its doc comment so the next reader doesn't re-litigate.
- **The waitForSelector before dispatch:** converts the residual late-mount race into a bounded wait (the Playwright actionability convention), rather than a thrown evaluate error.

### Assumptions stated and validated (Rule 7)

| # | Assumption | Validation |
|---|---|---|
| A1 | The failures share one mechanism | The three-mechanism probe: same page state, same mocks, only the dispatch path differs |
| A2 | The Locator API doesn't close the window | Tested directly — same zero-event result |
| A3 | The fix generalizes to all E-rows | 6/6 clean full-spec runs post-fix across all call sites (was ~30-50% flake) |
| A4 | Passkey is a different (contention) class | 4/4 clean local runs; no dispatch dependency in its flow |

---

## Blockers

None.

## Tests Run

- Probe spec (since deleted — investigation scaffold): the three-mechanism matrix, 3 rounds
- `npx playwright test tests/e2e/attachments.spec.ts` × 6 post-fix: 7/7 each (was ~30-50% flake)
- `npx playwright test tests/e2e/helpers/attachFiles.spec.ts`: 2/2
- Full e2e dir: 157 passed; the 3 residual flakies were the attachments rows BEFORE the waitForSelector addition (post-addition: 0 attachment flakies across all runs)
- `npx tsc --noEmit` clean

## Next Steps

- PR (Refs #1520), iterate to APPROVED, notify the orchestrator.
- Watch the next CI frontend-job runs for the passkey row (residual-observation).

## Files Modified

- `frontend/tests/e2e/helpers/attachFiles.ts` (new — the fix)
- `frontend/tests/e2e/helpers/attachFiles.spec.ts` (new — regression rows)
- `frontend/tests/e2e/attachments-helpers.ts` (new — extracted mock surface)
- `frontend/tests/e2e/attachments.spec.ts` (5 call-site conversions + import restructure)
