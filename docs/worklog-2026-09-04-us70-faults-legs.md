# Worklog — US-70.0 faults legs F1–F5: first executions, the DEK-restore
# heal, and the bootstrap-token expiry gap

**Date:** 2026-09-03 → 2026-09-04
**Operator:** opencode
**Scope:** `local/us-70-faults-e2e.sh`, `api/internal/handlers/pod_bootstrap.go`
**Runs of record:** 33885850468, 33891666924, 33896081667 (pool, dind runner)

---

## Summary

The fault/chaos legs (F1–F5) had never executed — every prior pool run died
upstream. With the delivery suite, revisions, and the Epic 69 evidence leg
green (#1244 trail), the faults leg ran for the first time and, row by row:

- **F1 PASS** on first execution (fault-injected bootstrap 500s; autopush heals).
- **F2 PASS** on first execution (API partition at resume; sessionless degraded
  boot; converge after restore).
- **F3** passed its security assertions on first execution (corrupted
  `wrapped_dek` → sessionless degrade + `pod_bootstrap_dek_failed` audit) but
  the restore-convergence wait could never fire. Two runs established that no
  in-place heal exists **by design**: a DEK restore is not manifest data
  (`classify()` sees `applied==seq`, never notifies), the resync 304 path is
  blocked by the W2 apply-guard at the same seq, and `runMaterializeCommand`
  skips identically. The anchor wipes with the pod — the harness now performs
  suspend→resume after an exact-byte restore, the deterministic recovery an
  operator would run.
- **F4a PASS** (tampered mint → 401; untampered control passes auth).
- **F4b** found a real gap: an expired minted SA token authenticated 40
  minutes past its own `exp`. Direct reproduction on kind v1.35: TokenReview
  returns `authenticated: true` for a token 15 s past expiry — the cluster
  does not enforce the mint's expiry. Fixed in the handler (defense-in-depth)
  rather than trusted to the reviewer.
- **F5** (chaos kills) remains queued behind F4b at the time of writing.

## Decisions and their assumptions

1. **F3 heal = suspend/resume (harness), not an in-place product path.**
   Assumptions: (a) the revision system cannot see key material — verified by
   reading `classify()` and the W2 guard, and empirically (forced resync
   returns `200 not_modified`, no delivery, runs 33885850468/33891666924);
   (b) the anchor is pod-scoped — verified via the W2 comment ("Pod death
   wipes anchor and state together") and the passing resume heal.
2. **Expiry enforcement lives in the handler, not the reviewer.**
   Assumptions: (a) kind v1.35 TokenReview ignores `exp` — verified directly
   (`kubectl create --raw tokenreviews` on a 600 s mint, reviewed at +615 s:
   `authenticated: true`); (b) reading the `exp` claim without verifying the
   signature is sound BECAUSE the reviewer already validated signature and
   audience — the ordering is explicit in `Bootstrap` (review first, exp
   check second); (c) 60 s leeway covers API↔apiserver skew — boundary-tested
   (`TestExpiryLeewayBoundary`); absent/non-numeric `exp` fails open to the
   reviewer's verdict, matching legacy tokens that carry no self-declared
   expiry.
3. **F4b's mint uses `--duration=35m`.** Assumption: WAIT_S = exp−now+120s
   must fit the ~1 h row budget — with the 1 h default mint the row silently
   skipped EVERY run (WAIT_S ≈ 3718 > 3700 guard), which is exactly why the
   expiry bug survived until the first real execution. Verified: 35 m + 120 s
   ≈ 2220 s ≤ 3700.

## Validation

- `TestBootstrap_ExpiredTokenRejected` drives the real handler over HTTP with
  the static reviewer answering YES — past-leeway expiry is the only 401
  path. Mutation-verified red/green by the PR review (deleting the handler
  check turns it red).
- `TestUnverifiedJWTExp` / `TestExpiryLeewayBoundary`: parser + boundary.
- Full `api/internal/handlers` + `local` suites green; the faults suite's
  remaining rows run in the pool (dwell 60, built images).

## Files changed

### `api/internal/handlers/pod_bootstrap.go`
- Handler-level `exp` enforcement after successful TokenReview
  (`unverifiedJWTExp` + `jwtExpiryLeeway` = 60 s).

### `api/internal/handlers/pod_bootstrap_exp_test.go`
- Parser tests, leeway boundary, handler-level regression pin
  (no-op injector + static reviewer over a real HTTP server).

### `local/us-70-faults-e2e.sh`
- F3: suspend→resume after exact-byte DEK restore (was: 300 s wait on a
  signal that by design never comes; then: a forced resync the W2 guard
  correctly no-ops).
- F4b: `mint_token --duration=35m` (row executes instead of silently
  skipping); leeway-aware sleep (+120 s); truthful messages (the 401 is the
  API's own check — TokenReview does NOT enforce expiry).

## Follow-ups

- F5 chaos-kill rows: first execution pending behind this PR.
- AC-4-lite (bind racing a pod crash) remains a documented known-fail —
  US-70.3 durability question, not blocking.
- Consider a product-side "key-rotation bumps a visible epoch" design if
  in-place DEK-restore healing is ever wanted (today: resume is the answer
  and it is fine).
