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

---

## Review round 1 (PR #1348)

- **Residual-copy path closed:** `lrem` count 1 → **0** per key — the claim drains EVERY byte-identical copy from both lists, so the dual-stage race (two identical staging copies) leaves nothing for next boot's Recover to re-fire. Pinned by `TestClaimDelivered_DrainsDuplicateCopies` (main+2 staging copies → one atomic claim of 3, no residual).
- **Error arm covered:** `TestClaimDelivered_ErrorArmNoFireEntryRetained` — miniredis `SetError` at claim time → 0 return, no fire, entry retained, recovered claim wins (the at-least-once direction). The dead `redis.Nil` comparison removed.
- **Dead parameter removed:** `withStaging` (all five sites claim both lists; the `#KEYS > 1` branch was unreachable).
- **Contract comments corrected:** `DeliveredHook` states exactly-once per entry for concurrent completers with at-least-once failure direction; the script/helper docs scope the invariant honestly and note the single-instance Redis constraint (no hash tags — cluster mode would CROSSSLOT; today's deployment is standalone Valkey).
- **Faithful second-completer row:** `TestCompleteSites_SecondCompleterStaleSnapshot` — B's claim runs against its stale snapshot after A drained; removes nothing, fires nothing.

---

## Review round 2 (PR #1348)

- **Vacuous second-completer row (fixed, honestly this time):** the r1 row never reached the completed site — after A's drain the session was undiscoverable, and even the r2 first attempt's fresh LRange no longer held e1 (mutation-verified passing before I shipped it). The faithful construction: the REAL stale-snapshot window is inside the sweep itself (snapshot → probes → claim) — B's sweep snapshots e1, blocks mid-probe; A claims e1 during the block; B resumes and its claim on the snapshot value removes nothing. Mutation-verified: unconditional-fire at the sweeper site FAILS this row; gated passes. Probe activity asserted (non-vacuous).
- **Comment block repaired:** releaseLockScript's orphaned doc restored to its declaration; claimDeliveredScript's doc rewritten to the count-0 truth (drains every copy, not "one copy"); the dead `#KEYS > 1` branch removed (the script is two unconditional LRems).
