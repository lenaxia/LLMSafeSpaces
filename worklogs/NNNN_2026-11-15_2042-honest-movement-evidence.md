# Worklog: #2042 honest movement evidence — the two-class movement contract (the #2040 incident's engine fix)

**Issue:** [#2042](https://github.com/lenaxia/LLMSafeSpaces/issues/2042) (sub-issue of [#2040](https://github.com/lenaxia/LLMSafeSpaces/issues/2040), the 2026-11-15 FMD-Web 3-of-4-slots incident)
**Branch:** `fix/2040-honest-movement-evidence` (off `origin/main` @ bd1526a16)

## What this lane is

The incident: agentd's session-state reconcile published synthesized `SESSION_STATUS`
settles (#1722 fanout) for sessions that emitted nothing; the board's liveness hub
stamped ANY frame carrying a session id as worker movement (`ObserveFrame`), and the
ladder's renewal arm (`MovedSinceLastSweep`, `reconciler.go`) outran the nudge/wedge
ladder indefinitely — three halted workers held their slots 1–7 hours, and one
compliant-but-stuck worker orbited nudge→heartbeat→renew for 4+ hours of zero work.
The root contract defect: the seam defined movement as "the harness observed the
session do ANYTHING", which conflates platform bookkeeping with worker progress.

## The fix (two halves at one seam, `MovedOpaque` as the only new fact)

**API half — the allowlist** (`api/internal/app/board_liveness.go`,
`board_busy_source.go`):
- `workerActivityStampsEvidence` — the single definition of worker activity:
  MESSAGE_*/PART_* are renewal evidence; everything else (settles, reseeds,
  lease-state transitions, input materializations, errors, unknown future types)
  routes to the OPAQUE class. Two per-class stamp maps (`framesWorker`,
  `framesOpaque`) over ONE global counter — a single clock domain for every
  ordinal comparison. Opaque events stamp their own index (never invisible — an
  uncounted class could never be bounded).
- The ordinal wedge query reads the WORKER index only: a settle after a sentinel
  is not an answer (the incident's second poisoning leg).
- The busy-gate distrust guard (`stamp > 0`) reads the worker index: the event
  that flips busy (a PART_START consult) is worker activity, so busy-with-no-
  worker-frame-ever stays unknown.
- `boardLiveCursor` carries both stamps; `pruneLocked` covers both maps.

**Engine half — the bounded authority** (`pkg/board/reconciler.go`,
`engine.go`, `stage.go`, `rearm.go`, `lease_events.go`, `ladder_metrics.go`):
- New rung after the strong-renewal arm: `MovedOpaque && opaqueRenews < boardOpaqueRenewCap`
  (3) → renew (`RenewSourceOpaque = "opaque_movement"`, counter increments);
  at the cap the row falls through to the quiet arms as if unmoved (nudge →
  wedge) — the busy-cap discipline applied to the second weak channel.
- `tasks.opaque_renews` (migration **000055**, api + helm mirror): consecutive
  opaque-only count per claim epoch. Resets wired at claim, requeue, heartbeat
  response, wedge verdict, and re-arm — the full epoch-counter lifecycle.
- Always-on telemetry (knob-independent): `board_lease_renewals_total{source}`,
  `board_ladder_nudges_total`, `board_ladder_wedges_total` (prometheus, following
  the `stampWriteFailures` package precedent — NOT expvar; the issue AC said
  /debug/vars, the package's metrics stack is prometheus — deviation recorded on
  the issue). A movement-class renewal on a silent session is the incident's
  signature; it is now visible in one scrape.

**Design contract amended:** `reports/specs/seamless-lease-design.md` — "observed
the session do ANYTHING" replaced by the two-class movement definition; the seam
gained `MovedOpaque` (poor-facts kept: one boolean, bounded authority, fail-safe
directions: wrongful-nudge is cheap, wrongful-renewal is the incident).

## Pins (born-red evidence in-session)

- Hub unit pin `TestLeaseLivenessHub_ManagementEventsAreNotWorkerMovement` —
  compile-red first (the seam did not exist), then green: settles never stamp
  the worker index under either identity key; ERROR/INPUT/SESSION_UPDATED are
  opaque; PART_DELTA stamps both keys.
- `TestIntegrationLeaseLiveness_ManagementFramesDoNotRenew` — the incident
  shape: a settle does not renew and stamps `MovedOpaque`; PART_END renews.
- `TestIntegrationLeaseLiveness_OrdinalLegIsWorkerActivityOnly` — post-sentinel
  settle is NOT a reaction; a worker part is.
- Engine pins 2042-A/B/C (`opaque_renews_2042_integration_test.go`):
  A — opaque-within-cap renews and counts (1); B — parked at cap, the lapse
  arms a sentinel instead of renewing, and a silent follow-up lapse lands the
  wedge verdict (the orbit closes); C — worker movement resets the counter.
  Two authoring errors caught and fixed in-session (an inverted count assert;
  the ordinal-unknown shape — the fake must carry `NudgeMarkKnown` to model a
  hub that holds the mark, otherwise the engine honestly re-nudges instead of
  wedging).

## Honest process notes (things the lane got wrong first)

- A patch script `.replace()` silently no-op'd on the victims SELECT and the
  heartbeat-response reset (indentation mismatch) — the sweep then failed at
  runtime with "field descriptions must equal number of destinations, got 10
  and 11" (39 cascade failures in the first full integration run). Fixed with
  asserted-exactly-one replacements; the stillborn-corpses pin passing again is
  the regression evidence.
- `pkg/board` integration tests are `//go:build integration` + skip without
  `TEST_DATABASE_URL` — the first "green" run had SKIPPED them. The real gate
  runs with the tag and env; the shared test DB needed migration to 000055
  (the `integration`-tagged testharness run applies pending migrations).
- Full-suite status at worklog time: run 2 had exactly ONE failure
  (`TestPin_LeaseLadder_HeartbeatInSendWindowIsNotLost` — a race-window pin)
  which passes in isolation; run 3 in flight to classify flake vs regression.
  The pin races a heartbeat goroutine against the sweep's row lock — the
  timing class is inherently scheduling-sensitive; verdict recorded below.

## Gates

- `gofmt` clean; `golangci-lint` 0 issues on touched packages; `go vet` clean
  (both tags); `api/internal/app` + `pkg/board` unit suites green; the three
  API integration pins green; engine pins 2042-A/B/C green.
- Residual (handed to review): the mutation check on the cap (arm disabled ⇒
  2042-A red) is recorded below; the SendWindow classification verdict below.

## Round-1 adversarial review (verdict: request-changes) — findings and resolutions

The reviewer caught a real design error in the first push, plus three wiring
gaps and one record gap. All five findings accepted; the resolution is a
DESIGN CHANGE, not a patch:

- **F1 (ac1-first-settle-renews)** — the issue's AC1 pins "quiet + first
  settle + sweep → nudge, not renewal", but the shipped engine renewed on
  opaque movement up to a cap: the AC's literal scenario held the slot ~3
  lease windows per epoch. **Resolution: opaque movement now carries ZERO
  renewal authority** (the reviewer's second option; delegated design call).
  A settle is never worker evidence, so it renews nothing — the capped
  intermediate was structurally weaker AND (per F2) mutually re-arming.
  `boardOpaqueRenewCap`, the `opaque_renews` column and migration 000055 are
  REMOVED (dead code under zero authority); `MovedOpaque` stays on the seam
  as telemetry. AC1 is now literally implemented and pinned (2042-A).
- **F2 (weak-channel-mutual-re-arm)** — busy-only renewals zeroed
  opaque_renews while opaque-only zeroed busy_renews: a stale-latched busy
  gate + continuous settles renewed unboundedly. **Evaporated structurally**
  by F1's fix (no opaque counter, no opaque renewals); the busy cap's own
  consecutive semantics are unchanged from main.
- **F3 (mint-sites-skip-opaque-reset)** — four claim-mint sites
  (bounceTx/exitPendingReclaim/personaClaimSQL/review re-entry) leaked
  opaque_renews across epochs. **Evaporates with the column's removal.**
- **F4 (cap-mutation-check-missing)** — the worklog promised a cap mutation
  and didn't record one (the verdict rewrite dropped it — sloppy). **With
  the cap removed, the equivalent load-bearing mutation ran on the final
  tree**: `workerActivityStampsEvidence` mutated to `return true`
  (allowlist disabled) → **3/3 pins red**
  (`TestLeaseLivenessHub_ManagementEventsAreNotWorkerMovement`,
  `TestIntegrationLeaseLiveness_ManagementFramesDoNotRenew`,
  `TestIntegrationLeaseLiveness_OrdinalLegIsWorkerActivityOnly`);
  restored → green. Command: the three-pin `-run` filter above, mutant
  armed via the one-line `return true` early-return.
- **F5 (depose-guard-opaque-divergence-doc, advisory)** — RESOLUTION
  CORRECTED in round 2 (the first record claimed "fixed in-tree" for an
  edit that never landed — the python insertion no-op'd and the commit
  message asserted an edit that does not exist; exactly the
  claim-vs-tree drift this lane polices). Under zero authority the
  concern EVAPORATED: `deposeShowsLife` consults exactly the ladder's
  renewal set (worker movement / busy≤cap / post-sentinel reaction), so
  its mirror-the-ladder contract comment is consistent again — no
  comment needed, no edit made. The reviewer's own analysis agrees.

**AC amendments recorded on #2042** (owner-delegated decisions, 2026-11-15):
AC1 satisfied literally; R2 superseded — the strict bound on opaque
authority is ZERO (the cap's residual value — tolerance for misclassified
future activity types — buys a 3-window slot hold at the price of the
mutual-re-arm class; one wrongful nudge a healthy worker answers is the
cheap direction); AC6's cap mutation is moot (no cap) and replaced by the
recorded allowlist mutation above.

## Round-2 adversarial review (verdict: request-changes) — findings and resolutions

All five round-1 findings verified FIXED by the reviewer (it re-ran the
recorded allowlist mutation live on this tree). Three new findings — all
record-layer completeness, zero engine changes:

- **phantom-opaque-migration-shipped** — the four 000055 files still
  existed at HEAD: the working-tree `rm` was never staged (`git rm`),
  so the commit shipped a column with zero consumers under operator
  notes denying a schema change. **Fixed: the deletions are now staged
  and committed** — main's migration set is restored and the records
  are true. The reviewer's suggested schema-vs-code grep pin is filed
  as its own follow-up (see the issue link in this PR's description).
- **zero-authority-records-stale** — the redesign was half-recorded:
  design-doc loop prose, the seam comment, the hub comment, the
  CHANGELOG telemetry source list and the PR body still described the
  capped intermediate. **Fixed**: every site now states zero renewal
  authority (a settle is never worker evidence; quiet arms arm on the
  first settle-only evidence).
- **f5-fix-claim-unsubstantiated** — see the corrected F5 record above;
  the issue-thread correction is posted alongside this push.

## Verdicts

- **Flake classification (the honest one):** across four full-suite runs the
  failure sets were: run 1 — 39 (the SELECT/scan mismatch cascade, my bug,
  fixed); run 2 — 1 (`HeartbeatInSendWindowIsNotLost`, race signature);
  run 3 — 2 (SendWindow with a transient scan-error signature +
  `NudgeDeliveryFailureFallsBack`); run 4 — 1 (SendWindow, race signature
  again). The **decisive control**: a FULL-suite run on CLEAN origin/main
  (`/tmp/opencode/baseline` worktree) fails the SAME class —
  `TestPin_Lease12_BusyMidTurnNeverWedges` AND
  `TestPin_LeaseLadder_HeartbeatInSendWindowIsNotLost` — while both pass in
  isolation on both trees. These are pre-existing race-window pins
  (heartbeat-goroutine × sweep-lock scheduling; `Lease12` rides the same busy
  gate) that are order/load-sensitive in full-suite runs in this environment.
  Not this branch's regression; flagged for the reviewer as a known-flaky pair
  worth deflaking separately. My diff's decision path for every affected
  fixture is unchanged (the new opaque arm cannot match their observations:
  `liveQuiet()`/busy fixtures never set `MovedOpaque`).
- **Scan-error signature note:** run 3's one-off
  `cannot scan NULL into *time.Time (col: max)` came from the pre-existing
  `maxEventAt` (`SELECT max(created_at) FROM board_events`, reconciler.go:235)
  which cannot tolerate an empty event table — exercised only in interleaved
  full-suite states; not reproducible in isolation on either tree afterwards.
  Pre-existing latent fragility, not touched by this lane; noted here because
  the reviewer will see it in the run-3 log.

## Files

`api/internal/app/board_liveness.go`, `api/internal/app/board_liveness_test.go`,
`api/internal/app/board_busy_source.go`, `pkg/board/liveness.go`,
`pkg/board/reconciler.go`, `pkg/board/engine.go`, `pkg/board/stage.go`,
`pkg/board/rearm.go`, `pkg/board/lease_events.go`, `pkg/board/ladder_metrics.go` (new),
`pkg/board/opaque_renews_2042_integration_test.go` (new),
`api/migrations/000055_opaque_renews.{up,down}.sql` (new) + `helm/migrations/` mirror,
`reports/specs/seamless-lease-design.md`, `CHANGELOG.md` (Unreleased).
