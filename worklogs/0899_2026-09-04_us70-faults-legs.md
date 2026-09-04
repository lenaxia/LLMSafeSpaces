# Worklog: US-70.0 faults legs F1–F5 — first executions; DEK-restore heal; bootstrap-token expiry gap

**Date:** 2026-09-04
**Session:** The fault/chaos legs ran for the first time (behind the green delivery suite, revisions, and Epic 69 evidence legs — #1244 trail). Row-by-row first-execution triage, the F3 DEK-restore heal decision, and the F4b expiry enforcement fix.
**Status:** In Progress

---

## Objective

Execute the never-run fault rows (F1 fault-injected bootstraps, F2 API partition at resume, F3 corrupted wrapped_dek, F4 SA-token rows, F5 chaos kills) on the dind runner pool; triage every failure into harness bug vs product gap; fix at the right level.

---

## Work Completed

### F1, F2 — green on first execution

No harness defects; the fault seam (LLMSAFESPACES_FAULT_INJECTION), autopush healing, and API-partition recovery behaved as designed.

### F3 — restore-convergence wait could never fire (harness fix)

Security assertions passed first try (sessionless degrade + pod_bootstrap_dek_failed audit). The 300 s post-restore wait then waits on a signal that by design never comes:

- A DEK restore is not manifest data — classify() sees applied==seq and never notifies.
- A forced resync returns 200 not_modified: the W2 apply-guard blocks re-materialization at the same seq (verified on run 33891666924 after adding the resync nudge).
- runMaterializeCommand skips identically.
- The anchor is pod-scoped ("Pod death wipes anchor and state together") — suspend→resume after an exact-byte restore is the deterministic recovery an operator would run. Verified green on run 33896081667.

### F4b — expired SA tokens authenticate (product fix, this PR)

A minted token 40 minutes past its own exp returned 200 on /internal/v1/pod-bootstrap (run 33896081667). Direct reproduction: kind v1.35 TokenReview returns authenticated:true for a 600 s mint reviewed at +615 s — the cluster does not enforce TokenRequest exp. Fix: after successful TokenReview (signature + audience validated), the handler reads the token's own exp claim (unverified parse of an already-authenticated token) and 401s past exp + 60 s clock-skew leeway; absent exp fails open to the reviewer, matching legacy tokens.

- Regression pin: TestBootstrap_ExpiredTokenRejected drives the real handler over HTTP with the static reviewer answering YES — past-leeway expiry is the only 401 path.
- mint_token --duration=35m: WAIT_S = exp−now+120s ≈ 2215 s vs ≈3720 s under the same formula on the 1 h mint (which would trip the 3700 s guard) — the shorter mint saves ≈1392 s ≈ 23 min of sleep per run at the same loud-die-on-regression guarantee.
- Messages corrected: the 401 is the API's own check; TokenReview does NOT enforce expiry. The first commit's messages credited "TokenReview enforcement" before any layer was reproduced — the direct tokenreview repro (two minutes) rewrote the story; recorded here per the README's validation-before-record discipline.

---

## Key Decisions

1. **F3 heal = suspend/resume, harness-side.** Validated: revision blindness to key material (classify() read + empirical not_modified), W2 guard (read + empirical), pod-scoped anchor (code comment + passing heal). An in-place product path would require key-rotation to bump a visible epoch — noted as a follow-up design idea, not needed now.
2. **Expiry enforcement in the handler, not delegated to the reviewer.** Validated: reviewer blindness (direct tokenreview repro), soundness of unverified-parse-after-signature-validation (ordering explicit in Bootstrap), 60 s leeway (boundary test), fail-open on absent exp (legacy-token compatibility).
3. **35 m mint duration** — validated arithmetic above; the guard stays loud.

---

## Blockers

None — F4b fix under review in this PR; F5 executes behind it.

---

## Tests Run

- `go test ./api/internal/handlers/` — green (incl. TestBootstrap_ExpiredTokenRejected, TestUnverifiedJWTExp, TestExpiryLeewayBoundary).
- `go test ./local/` — green (harness pins).
- Pool run 33896081667: F1/F2/F3 green, F4a green, F4b red (the finding), F5 pending.

---

## Next Steps

- Merge this PR; dispatch the pool; F4b should 401 and F5 (chaos kills) executes for the first time.
- Triage F5 results with the same harness-vs-product split.
- Then the full 1 h-gate run (dwell 3600) for the end-to-end green.

---

## Files Modified

- api/internal/handlers/pod_bootstrap.go — handler-level exp enforcement (unverifiedJWTExp + jwtExpiryLeeway).
- api/internal/handlers/pod_bootstrap_exp_test.go — parser tests, leeway boundary, handler-level regression pin.
- local/us-70-faults-e2e.sh — F3 suspend/resume heal; F4b 35 m mint + leeway-aware sleep + truthful messages; exp-decode hardening.
