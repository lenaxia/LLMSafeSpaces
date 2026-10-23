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

## Review r2 (CHANGES_REQUESTED on d847910c) — fixes and evidence

Rebased onto origin/main post-#1902/#1903 (union-merged the
reconcileOutcomes help text with #1882's reaped_carrier; no other
conflicts). Review findings → resolution:

1. **Defect 1 — walk-error poisoned the cooldown tracker**
   (record() before the err check): FIXED. record() now runs only
   after a successful walk; the error path leaves NO tracker entry.
   Red pin TestListingCount_WalkErrorRearmsNotCooldowns: errored walk
   → immediate re-listing RETRIES (not listing_count_cooldown), and
   the healed harness converges on the next poll (truth served +
   persisted). Mutation M8 (record moved back before the check)
   reddens exactly this pin.
2. **Defect 2 — >100-row reach limit**: RESOLVED BY REBASE. #1886
   (PR #1902, merged to main before this PR's r2) removed the
   `LIMIT 100` from ListSessionIndex (deterministic ORDER BY, death
   marks filtered pre-order). The serve-through and the rebuild both
   iterate the full row set — NULLS-LAST now affects only display
   order, never reach. Scoping claim in the r1 worklog ("≤100-row
   workspaces") is superseded; CHANGELOG states it explicitly.
3. **Thin unhappy paths**: BOTH PINNED.
   TestListingCount_PersistErrorServesTruthAndRetries (unit: walked
   truth on the wire despite persist failure, listing_count_persist_error
   counted, listing_count_served NOT, next listing re-walks because
   index ≠ walked, heal converges — mockSessionIndex.failRebuild)
   and TestRouterSessionList_WalkErrorServesIndexValue (wiring tier:
   walk error through the production router → 200 with the INDEX
   value on the wire, walk attempted/observable). Both are
   protective pins (green at the r1 head — the branches existed;
   they were untested), mutation carriers per the house convention.
4. **Issue closure gaps**:
   - "not-scheduled" skip class: count_pass_ran now fires whenever a
     pass's count leg executes with a live present-set — the
     denominator. Sidebar traffic with zero count_pass_ran = the pass
     is not being scheduled (distinguishable from metrics alone, the
     issue's ask). Red pin TestSessionIndexReconcile_CountPassRanDenominator;
     mutation M10 (outcome removed) reddens it.
   - Thread Finding 2 (rebuild stationarity): FIXED with the rotation
     the thread investigation specified — when candidates exceed the
     budget, the walk window starts at (passIndex × K) mod N with
     passIndex a wall-clock bucket of the 30s cadence (stateless;
     consistent under the cadence claim — one replica per window).
     Union of P consecutive windows covers P×K consecutive
     candidates → full coverage in ceil(N/K) passes; ≤budget
     workspaces keep the deterministic oldest-first walk.
     TestSessionIndexReconcile_CountBudgetOldestFirst (pre-existing
     #1481 pin) now pins window 0 via countPassIndexOverride — its
     contract (oldest-first within the window) is unchanged; without
     the pin the >budget walk set became wall-clock-dependent by
     design. Red pin TestSessionIndexReconcile_RebuildRotatesThrough-
     NullGroup (six-row all-NULL group: window 1 MUST walk the
     starved tail); mutation M9 (rotation replaced by plain
     truncation — the original bug) reddens exactly it.
   - Direction 2 follow-up: FILED as issue #1906 (agentd→API
     message-event telemetry for board-spawned sessions' incremental
     counts) — the thread triage explicitly blessed the deferral and
     asked for the filing.
5. **Nits**: listingCountBudget comment corrected ("a converged
   workspace walks nothing" overstated — never-traversed rows
   re-walk on each cooldown expiry, bounded staleness by design);
   CHANGELOG entry added (#1865/#1881 precedent).

### r2 red-first + mutation evidence

- RED commit 62c55f6f: TestListingCount_WalkErrorRearmsNotCooldowns,
  TestSessionIndexReconcile_CountPassRanDenominator, and
  TestSessionIndexReconcile_RebuildRotatesThroughNullGroup FAIL at
  the r1 head (verified: walk-error pin cooldown-skips the retry;
  count_pass_ran never fires; window-1 pass re-walks ids[0:3]).
  TestListingCount_PersistErrorServesTruthAndRetries (after fixture
  correction: each listing re-reads the INDEX's stale value — the
  mutated slice is per-request) and the router walk-error pin are
  green-at-head protective pins.
- Mutations re-executed at the r2 head: M8 (record-before-error
  restored) → ONLY TestListingCount_WalkErrorRearmsNotCooldowns;
  M9 (rotation → plain truncation) → ONLY …RebuildRotatesThrough-
  NullGroup; M10 (count_pass_ran removed) → ONLY …CountPassRan-
  Denominator. Each restored → suite green.

### r2 assumptions

- The rotation's pass index uses wall-clock cadence buckets. If two
  passes ever run in one window (claim bypass — not the current
  ClaimReconcileTurn contract), they share a window and walk the
  same set: at worst a wasted pass, never a regression to starvation.
- countServeCap wholesale map reset (r1 "minor", self-documented,
  bounded) left as-is per the skeptical-reviewer verdict.
- Serve-through (direction 3) intentionally does not touch
  last_message_at; rows persist in the hot signature by design
  (growing never-traversed sessions keep converging; staleness
  bounded by countServeCooldown).
