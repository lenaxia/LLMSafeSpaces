# Worklog: Fix epic-71 flake — outbox storm double-fire root cause (Recover pre-lock snapshot)

**Date:** 2026-09-16
**Session:** flake-hunter stream epic-71 / flake-storm-outbox — root-cause the `TestStress_AmbiguityStormMultiReplica` CI failure (run 35038952063, main @ 266d66a3) and verify the `TestOutboxDeliver_V2NoPromotionNeverFalselyCompletes` ~1/5 recurring flake (#1314 cross-stream note)
**Status:** Complete (flake 1 fixed and proven; flake 2 verification disk-blocked locally, evidence recorded)

---

## Objective

Two convergence/stress-class flakes were dispatched:

1. `TestStress_AmbiguityStormMultiReplica` (api/internal/services/outbox) — FAILED on main CI 2026-09-16T00:19Z: `outbox_stress_test.go:164: OnDelivered fired 2 times for storm-2-0`.
2. `TestOutboxDeliver_V2NoPromotionNeverFalselyCompletes` (api/internal/handlers) — recurring ~1/5 per the 2026-09-15 #1314 cross-stream note, hardened on #1349; verify ≥100 runs and record as un-reproducible-at-head if green.

---

## Work Completed

### Flake 1 — root cause found, fixed, proven

**Reproduction.** 3 concurrent `go test -race -count=10` loops under `nice`: RED at cumulative run ~45–50 of loop C (`OnDelivered fired 2 times for storm-0-0`), ≈245 total runs to first red — same signature as CI. Quiet single-loop runs stayed green 300+, confirming a contention-dependent interleaving.

**Root cause.** `Service.Recover` (outbox.go) read the staging list (`LRange outboxd`) BEFORE acquiring the per-session delivery lock, then made the requeue decision from that snapshot after `acquireLockWithRetry` finally succeeded. The interleaving that breaks the exactly-once delivered hook:

1. Worker W1 stages entry E (RPUSH staging + LREM main) and blocks mid-send holding the session lock.
2. A `Run` loop start (second replica, or the same replica's boot) runs `Recover`; its pre-lock `LRange` captures `[E]`; `acquireLockWithRetry` parks in its retry sleep.
3. E completes normally: W1 restores E to main (verifying), a verify pass claims it cross-list and fires `OnDelivered` (fire #1); both lists are now clean.
4. `Recover` acquires the lock, re-reads main (E absent — it completed), and the STALE snapshot fails the `inMain` ID-dedupe → `LPush` requeues E as a verifying zombie.
5. A later pass verifies (the transcript confirms — the send persisted) and claims the zombie → **`OnDelivered` fires a second time for exactly one send**.

This is a production boot race (every replica start runs `Recover` while peers deliver), not test-only. Two prior mitigations in this area (#1339 LRem-count claim token, #1348 cross-list atomic claim) made the CLAIM atomic but could not prevent a stale WRITER from inserting a new copy after a completed claim — the requeue was exactly such a writer.

Evidence that ruled out the alternative hypothesis (lock TTL expiry mid-critical-section, the theory recorded in `shrinkStormTimers`' comment): miniredis v2.34's clock is frozen — TTL keys never expire by wall-clock passage in this stack (verified empirically: `SET k v EX 1` still exists after 1.1s in an isolated module; `SetNX` refuses a second acquire). Locks therefore cannot expire inside the test environment, yet the failure reproduces — expiry is not the mechanism. The CI-visible mechanism needs only scheduler delay between `Recover`'s `LRange` and its lock acquisition, which contention amply provides.

**Fix** (production ordering fix, no budgets touched, no assertions weakened):

- `Recover` now acquires the per-session lock BEFORE reading staging; `recoverSessionLocked` performs the `LRange` itself under the held lock. The requeue decision is now made against state no concurrent delivery can change.

**Deterministic red-first pin** — new `TestRecover_UnderLockSnapshotNeverResurrectsCompletedEntry` (outbox_recover_race_test.go):

- W1 blocked mid-deliver holding the lock (channel-gated deliverer); `Recover` parked in its retry sleep (`sweepLockRetryEvery` tuned to 200ms so the test's synchronous completion lands deterministically inside one retry window); entry completes (fire #1, lists clean); `Recover` wakes and decides from the stale snapshot.
- RED on the pre-fix code with the exact CI message class (`OnDelivered fired 2 times for one send`); GREEN post-fix. Mutation drill: reintroducing the pre-lock read (mutant `recoverSessionLockedPreLock`) re-reddens the test; restored, green.

**Proof.**

- Pre-fix: red at ~45–50 runs (contended 3-loop).
- Post-fix: 300 contended runs (3 × `go test -race -count=100`... executed as 3 concurrent loops of 10-run batches ×100) green; `go test -race -count=50` green; `go test -count=50` (no race) green. 400 total post-fix runs, 0 failures.
- Full outbox package suite `-race` green (includes `TestStress_CrashWindowsRecoverUnderLoad`, which pins `Recover`'s requeue count — unchanged semantics when uncontended).

### Flake 2 — hardened at head; local ≥100-run verification blocked by disk, evidence recorded

- The #1349 hardening is present at HEAD (proxy_outbox_verify_test.go:552-568: due-pass polling via `assert.Eventually`, admit-counter `assert.Eventually`).
- Local looping could not be executed: the handlers test binary cannot be linked in this environment — every writable filesystem is the shared 15G longhorn PVC (100% full; the shared Go build cache alone is 9.2G and `go clean -cache` is forbidden by dispatch), `/` is read-only, `/dev/shm` and `/sandbox-runtime` are 64M/96M tmpfs, smaller than the link output. Three attempts failed at compile/link with "no space left on device" (logs quoted in session record).
- CI-history evidence at head: scanned the 11 `--status failure` runs on main since 2026-09-15 — none failed on `TestOutboxDeliver_V2NoPromotionNeverFalselyCompletes` (visible failures were unrelated: a Leg5 reconcile flake, agent-harness respond steps). Combined with the #1349 note's 20× local `-race` green record, the flake is un-reproducible-at-head to available evidence — recorded with the local-loop caveat stated honestly rather than claimed as run.

---

## Key Decisions

1. **Fix the ordering, not the timers.** No test timer or production budget was changed; `shrinkStormTimers` and the storm test are untouched. The #1312 budget table is unaffected (no DeliveryTimeout/LockTTL/ParkedSweepInterval change anywhere).
2. **Lock-expiry fencing deferred (documented finding, not silently dropped).** The stale-write-after-lock-expiry class is real on production Valkey (LockTTL expiry mid-critical-section leaves unfenced index-based `LSet`/restore writes) but is INVISIBLE to the entire current test suite because miniredis's frozen clock never expires TTL keys. Fixing it properly (token-checked fenced Lua mutations + a periodic staging-recovery pass) plus testability (FastForward-driven virtual-time harness) is a self-contained hardening follow-up; bundling it here would add untestable-in-CI surface to a hot path. The frozen-clock fact is recorded above so the follow-up starts from evidence.
3. **Deterministic pin over brute force.** The new regression test forces the exact interleaving with channel gates and a tuned retry cadence instead of relying on scheduler luck — reproducible in 0.2s what CI hit once per ~250 contended runs.

## Assumptions (Rule 7) — stated and validated

1. *The CI failure mechanism must reproduce without lock expiry (miniredis never expires keys).* Validated empirically (isolated-module EX/PX probes) and by the deterministic red test, which uses no expiry.
2. *All other list mutators already snapshot under the session lock.* Validated by enumerating every `LSet/LPush/RPush/LRem/LInsert/Del` site in the package (grep transcript in session record); only `Recover` read before locking.
3. *Moving the staging read under the lock does not change `Recover`'s return-count semantics.* Validated: `TestStress_CrashWindowsRecoverUnderLoad` (asserts exact requeue counts) green.
4. *The fix does not introduce boot-time lock contention regressions.* Reasoned: sessions actively delivering were already waited on (their staging was non-empty pre-lock); sessions between deliveries acquire/release immediately. Bounded by the existing 2s retry budget and ctx.

## Blockers

- Local `./api/internal/handlers` test execution: disk (shared PVC 100% full; build cache protected). CI on the PR will exercise the package.

## Tests Run

- `go test -race -timeout 60s -run 'TestRecover_UnderLockSnapshotNeverResurrectsCompletedEntry$' -count=5 ./api/internal/services/outbox/` — green (was RED pre-fix, count=1).
- `go test -race -timeout 600s -count=1 ./api/internal/services/outbox/` — green (full package).
- `go test -race -timeout 300s -run '^TestStress_AmbiguityStormMultiReplica$' -count=50 ./api/internal/services/outbox/` — green; plus 3 concurrent loops × 100 runs contended — green; plus `-count=50` without `-race` — green.
- Mutation drill (pre-lock read reintroduced) — RED as intended; restored — green.
- `go test -short -run '^TestOutboxDeliver_V2NoPromotionNeverFalselyCompletes$' ./api/internal/handlers/` — blocked at link (no space left on device); see Blockers.

## Next Steps

1. Reviewer loop on the PR; do not merge (owner action per protocol).
2. Follow-up hardening (separate stream): fenced lock-token mutations for the delivery path + periodic staging recovery + a `mr.FastForward`-based virtual-time test harness — motivated by the frozen-clock finding.
3. When PVC space recovers, complete the ≥100-run local loop for `TestOutboxDeliver_V2NoPromotionNeverFalselyCompletes` and append the evidence to #1314.

## Files Modified

- `api/internal/services/outbox/outbox.go` — Recover reads staging under the session lock (root-cause fix).
- `api/internal/services/outbox/outbox_recover_race_test.go` — new deterministic regression pin (red-first).
- `worklogs/0955_2026-09-16_flake-storm-outbox.md` — this worklog.
