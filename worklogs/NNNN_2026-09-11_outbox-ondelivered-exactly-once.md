# Worklog: OnDelivered exactly-once across completers (main-red hotfix)

**Date:** 2026-09-11
**Session:** Main went red post-2a-merge: `TestStress_AmbiguityStormMultiReplica` — "OnDelivered fired 2 times for storm-1-0". Root cause is 0b's (#1318): the sweeper and the park guard became SECOND completers racing the verify path on a peer replica, and every complete site fired the hook unconditionally — even when its LRem removed nothing because the peer had already completed the entry.
**Status:** Complete

---

## Objective

Restore `DeliveredHook`'s "exactly once per entry" contract across ALL completion sites, cross-replica included.

## Work Completed

- Every completion site now uses **LRem's removal count as the exactly-once token**: the hook fires only from the caller whose LRem actually removed the entry (winner-takes-the-hook). Applied uniformly: the sweeper's `completed` branch, `applyParkGuardDisposition`'s completes arm (guard + transient-threshold sites), `verifyOne`'s VerdictDelivered arm, and the inline success path in `deliverOne`.
- Losers of the race do nothing — the entry is gone; their no-op LRem is harmless.

## Key Decision

LRem-by-value's return count is the natural cross-replica atomic claim: Redis executes LRem atomically, so exactly one caller (per copy) receives a non-zero count. No new locks, tokens, or round-trips.

## Tests Run

`TestStress_AmbiguityStormMultiReplica` green at `-count=5 -race` (the failing row); full outbox suite green with `-race`; golangci-lint 0 issues. The stress suite IS the regression test (it caught the bug on main) — no duplicate hand-rolled flake added.

## Next Steps

Land hot-fix; main CI re-run green; release checklist unaffected (the double-fire was observability/metering-level, not message duplication — S9’s no-re-POST was never violated).

## Files Modified

- `api/internal/services/outbox/parked_sweeper.go`
- `api/internal/services/outbox/outbox.go`
- `worklogs/NNNN_2026-09-11_outbox-ondelivered-exactly-once.md` (this file)

---

## Review round 1 (PR #1339)

- **Fifth completion site (fixed):** `verifyOne`'s unverifiable-park guard site still fired unconditionally — the lock-loss two-replica window (our verifier/probe I/O spanning a lock expiry, the peer completing meanwhile) double-fired. Same `n > 0` LRem token applied. Pinned deterministically by `TestVerifyOne_CompleteFiresExactlyOnceUnderLockLoss`: the verifier itself plays the peer (removes the entry mid-window); the completion must not fire our hook.
