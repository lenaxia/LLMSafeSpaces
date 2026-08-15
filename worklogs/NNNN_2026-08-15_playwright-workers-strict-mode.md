# Worklog: Playwright e2e stability — pinned workers + strict-mode locator

**Date:** 2026-08-15
**Session:** PR #647 remediation — accurate worker-cap rationale + strict-mode fix
**Status:** Complete

---

## Objective

Stabilize the Playwright e2e suite against two distinct flake sources: resource oversubscription on high-core local hosts, and a strict-mode locator collision in `session-activity.spec.ts` test 46.

---

## Work Completed

1. **`frontend/playwright.config.ts`** — pin `workers: 2`.
   - Playwright's unset default is **50% of CPU cores** (pinned `@playwright/test` 1.60.0, `lib/common/index.js`: `Math.max(1, Math.floor(cpus * 0.5))`) — 2 on the 4-vCPU `ubuntu-latest` CI runners, 18 on a 36-core local host. The original revision of this branch used `workers: 4`, which would have **doubled** CI concurrency in three pipelines (`e2e-pr.yml:38`, `ci.yml:416`, `release.yml:217`) — the exact resource-contended-30s-timeout failure mode this PR set out to fix. Withdrawn per review; `workers: 2` preserves CI behaviour exactly while capping local oversubscription (the PR's legitimate goal).
2. **`frontend/tests/e2e/session-activity.spec.ts`** (test 46) — `getByText("Alpha")` substring-matched both the workspace header (`<button>`, accessible name = workspace name, `Sidebar.tsx:409-433`) and the `Task Alpha` session row → strict-mode violation passing only on retry timing. Now `getByRole("button", { name: "Alpha", exact: true })`, matching the click locator.
   - **Caveat:** main's `a348901d` marked tests 41/42/45/46/53 `test.fixme` (fetch-based SSE cannot be mocked by `route.fulfill` — tracked separately for an SSE-mock helper). The locator fix therefore does not currently execute in CI; it is correct by inspection and applies the moment the SSE helper revives the test. Review's live run confirmed: the new locator resolves; the test still fails later (line 260) for the documented SSE-wiring reasons.

---

## Key Decisions

1. `workers: 2` over `4`: no CI-class evidence exists that 4 concurrent Chromium + Vite stay under the 30s timeouts on shared 4-vCPU runners; the deterministic cap loses nothing locally.
2. Keep the test-46 fix despite the fixme: strict-mode correctness shouldn't regress when the test is revived.

---

## Tests Run

- Full local suite at `workers: 2` (Chromium 1223): **109 passed / 1 flaky / 13 skipped / 0 failed** in 2.6m.
  - 1 flaky: `input-requests.spec.ts:118` (unrelated spec; passed on retry).
  - 13 skipped = 7 `chat.spec.ts` (no real-backend creds) + 5 fixme-marked `session-activity.spec.ts` (main's `a348901d`: tests 41/42/45/46/53) + 1 unconditional `passkey.spec.ts:240`. (JSON-reporter breakdown; fixme tests report as skipped.)
- Suite composition at head: 123 tests listed across 22 files (`playwright test --list`); 110 executable at the original branch point.
- CI run on this push is the authoritative CI-class number.

---

## Next Steps

- SSE-mock helper for Playwright (streaming route handler) — revives the 5 fixme'd `session-activity.spec.ts` tests; this PR's test-46 locator fix activates with it. Tracked separately on main (see `a348901d`).
- Optional follow-up: the `input-requests.spec.ts:118` flake (permission-deny feedback) observed once locally at workers:2 — likely the same SSE-replay class; investigate when the helper lands.

## Blockers

None.

---

## Files Modified

- frontend/playwright.config.ts
- frontend/tests/e2e/session-activity.spec.ts
- worklogs/NNNN_2026-08-15_playwright-workers-strict-mode.md (this file)
