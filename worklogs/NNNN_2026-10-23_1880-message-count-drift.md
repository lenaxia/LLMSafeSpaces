# Worklog: #1880 — session_index.message_count drifts to 0 on live sessions (listing serve-through + loud rebuild skips)

## Charter

Issue #1880: 10 of 15 unarchived session_index rows sat at
message_count = 0 for 1–5h while the harness held full histories
(3–389 messages). The #1481 rebuild pass was NOT converging them.
Orchestrator-approved shape: direction (3) harness-count-authoritative
in the listing read path + direction (1) loud skip logging, one PR;
direction (2) documentation-only.

## Root cause (why the rebuild cannot converge this class)

The drift class is spawn-created sessions (create_session / board
dispatch — every row in the issue's evidence table). Three facts:

1. **The incremental path never sees their messages.** RecordMessage
   fires only from proxy-traversing paths (proxy_lifecycle.go's SSE
   bridge, proxy_adapter_crosscutting.go). Spawn prompt delivery is a
   pod-local POST (the #1850 worklog's hole 2) and background turns
   have no SSE subscriber → the API sees ZERO events → count stays 0.
2. **The rebuild's candidate set is present[]-derived.**
   rebuildMessageCounts filters on the harness LIST; these sessions
   are exactly the #1452 list-invisibility class (live by ID, absent
   from the list — the same gap #1865 patched on the REAP side with
   the by-ID probe veto). The count rebuild never got that treatment:
   list-invisible rows are excluded from candidates FOREVER — a
   structural present-miss, not a transient one.
3. **Every skip was silent.** Walk failures logged at Debug (invisible
   in prod), the present-miss had no metric, and a failed harness LIST
   skipped the whole rebuild with no trace. Three suspected classes in
   the issue; all three were real, none observable.

Direction (2) documentation: a true incremental fix needs agentd→API
message-event push (the #1847 PR2 inventory already concludes
platform-derived data needs an agentd push surface); cross-referenced,
out of this lane's blast radius.

## The fix

**Direction 3 — ServeHarnessMessageCounts (proxy_session_index.go):**
the listing route (router.go, beside the reconcile piggyback) now
serves the HARNESS count for the drift class. For eligible rows
(sessionindex.CountServeEligible: count==0 OR last_message_at==nil —
the never-counted/never-traversed signature — aged past reapGraceWindow,
not archived) it walks CountMessages BY ID (the #1865 by-ID probe
precedent — immune to the list gap), serves the walked count on the
response slice in place, and persists it through the register +
RebuildMessageCount seam (InternalID-keyed, #1756/#1881 discipline —
self-healing like backfillContextUsed). Bounded: listingCountBudget=3
walks/request, never-walked-first then oldest-walked rotation (in-replica
countServeTracker), unchanged-row cooldown (5 min) so converged rows
stop consuming budget. A failed walk serves the index value (never
worse) and counts listing_count_walk_error. The signature deliberately
stays hot after persist (the rebuild seam writes count but not
last_message_at) so a growing never-traversed session keeps
converging on the listing — staleness bounded by the cooldown.

**Direction 1 — loud skips in rebuildMessageCounts:**
count_skip_not_present (metric + per-row Warn) for count-zero rows the
LIST omits — the convergence hole made attributable; history-bearing
absent rows stay with the ghost/operator machinery (kept_for_operator)
so real ghosts don't pollute the signal. count_skip_pass_no_list when
the harness LIST failed and the whole rebuild silently returned. Walk
failure Debug→Warn. Help text updated with the five new listing
outcomes (listing_count_served/_unchanged/_walk_error/_persist_error/
_cooldown).

**Reap-guard interaction (the #1881 belt):** serve-through persistence
flips drifted rows 0→N; DefaultReapGuard then classifies them
history-bearing (never admitted) — strictly safer. Fresh rows inside
reapGraceWindow are excluded from the overlay by the same window, so
the spawn-grace semantics are untouched (all four #1881 pins green).

## Red-first verification

RED commit: **578a2acf** — 6 overlay pins + eligibility matrix + 2
loudness pins against a stored skeleton (no-op seams), all primary
pins failing at commit time (companion scope pin passes trivially —
mutation carrier). Implementation commit turns them green.

## Mutation-verify recipes (executed, each reddens only its named pin set)

- M1 — serve-through removed: comment `row.MessageCount = count` in
  ServeHarnessMessageCounts → TestListingCount_ServesHarnessTruthForDriftRow
  AND TestRouterSessionList_ServesHarnessCountForDriftRow redden.
- M2 — spawn-grace gate removed from CountServeEligible (drop the
  CreatedAt-window return) → TestCountServeEligible/fresh_spawn (2
  subtests) + TestListingCount_SkipsFreshGraceAndConvergedRows redden.
- M3 — persist keyed by the phrase: RebuildMessageCount(..., row.ID, ...)
  in the overlay → TestListingCount_WritesInternalIDNotPhrase reddens.
- M4 — present-miss signal removed (`if false && row.MessageCount == 0`)
  → TestSessionIndexReconcile_PresentMissIsLoud reddens.
- M5 — cooldown check removed (`if false && walked && ...`) →
  TestListingCount_CooldownBacksOffConvergedRows reddens.
- M6 — rotation ordering removed (replace the SliceStable block with
  `_ = sort.SliceStable`) → TestListingCount_RotatesBudgetAcrossPasses
  reddens (phase 3: expects never-walked f,g before stale-walked a).
- M7 — router piggyback removed (replace the ServeHarnessMessageCounts
  call with `_ = sessions`) → TestRouterSessionList_ServesHarnessCountForDriftRow
  reddens with every handler unit green (the wiring-tier gap class).

## Verification

- New pins: handlers 8 + router wiring 1 + eligibility matrix + 2
  loudness, green ×5 repeats (deterministic — one pin construction
  fix during development: per-key time.Now() backdating introduced
  nanosecond ordering noise; single shared instant now).
- #1881 quartet green: TestDefaultReapGuard_GraceWindow,
  TestPlanReconciliation_FreshRowSurvivesTwoMisses,
  TestGetSession_Harness404_FreshCountZeroRow_KeptBySpawnGrace
  (+ aged twin), TestPhraseBirthUpsertsCarryShortID.
- Suites: api/internal/handlers, services/sessionindex,
  services/database, server — all green, -count=1.
- No DB-layer change (no migration, no sqlmock fixture churn; rotation
  state is in-replica by design). PG-gated disclosure: none needed.
- repolint: all checks passed.

## Notes

- Orchestrator's pointer worklogs/1163_2026-10-22_1837-stillborn-spawn-
  durability.md exists on NO reachable ref (main history, all branches,
  feat/1832-d-task-drawer included — verified by cat-file walk). The
  definitive recent context is worklogs/1196 (#1850/#1881, read in
  full) + worklogs/1192; proceeding on those + the issue body.
- Pre-accepted main-flake classes noted for CI: TestUpload_Streams_
  WithoutFullBuffering, TestRequestSessionStatusSnapshot_Coalesces.
