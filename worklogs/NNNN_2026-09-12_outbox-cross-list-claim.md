# Worklog: Cross-list delivered claim — atomic dual-LRem (main-red, second storm double-fire)

**Date:** 2026-09-12
**Session:** Main-red again on the storm row (run 34659706059, `-short`, 1/100+-rare): the same double-fire class through a DIFFERENT interleaving — the stage-out crash window legitimately holds the entry in BOTH lists (main + staging); two completers each won their own single-list LRem on different copies and both fired. #1339's token was exactly-once per COPY; the stress row (and the hook's real consumers — metering, SSE sent) need exactly-once per ENTRY.
**Status:** Complete

---

## Fix

`claimDeliveredScript` — one Lua script atomically LRems the entry from BOTH the main queue and staging and returns the total removed. All five completion sites now claim through `claimDelivered(ctx, ws, ses, val, withStaging=true)`; only a non-zero winner fires the hook. The both-copies window collapses: whichever completer runs first claims every copy atomically; the second removes nothing.

## Key Decision

Lua over check-then-two-LRems: the claim must be atomic across the two keys or the window merely narrows. Redis executes the script atomically; no new state, no tombstones, one round trip.

## Tests

- `TestCompleteSites_BothCopiesWindow`: the entry seeded in BOTH lists; verify-path completer fires exactly once and drains both; the sweeper completer afterward fires nothing. This is the deterministic reproduction of the main-red interleaving.
- Storm row green at `-race -count=15` (was 1-in-many on main).
- Full outbox suite `-race` green; all prior loser-suppression rows still green (12 subtests pass); lint 0 issues.

## Files Modified

- `api/internal/services/outbox/outbox.go` (script + claim helper + sites 1-3)
- `api/internal/services/outbox/parked_sweeper.go` (sites 4-5)
- `api/internal/services/outbox/parked_sweeper_test.go` (both-copies window pin)
- `worklogs/NNNN_2026-09-12_outbox-cross-list-claim.md` (this file)
