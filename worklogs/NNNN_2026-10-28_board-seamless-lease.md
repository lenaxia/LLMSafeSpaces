# Worklog — the seamless lease + pending_input-as-flag (000050 design family)

Lane: `board-lease-seamless` (tsk-ethereal-steeple-generates).
Design: `reports/specs/seamless-lease-design.md` (owner-approved 2026-10-28; rulings quoted
verbatim there). This worklog records the landing, the knob rationale, and the rejected
alternatives the spec's §Constraints demands.

## 1. Live evidence (why this exists)

Three mass-churn cycles 2026-10-27/28: workers lost claims mid-verification to lease lapse
(15m default; the watermark stamps completed activity only — a tool-call START never counts),
were re-fed to fresh claimants, burned attempts to the cap, and escalated wearing an
owner-question costume. A pod memory-reap reclaimed all four in-flight rows at the same
microsecond on wall-clock with no session-death evidence. The lease fix and the failed-stage
fix were themselves among the eaten rows — the board could not land its own rescue. During
THIS landing the old machinery struck again: the surviving worker's claim lapsed at 18:52Z
mid-suite-run (442s agentd suite, no watermark stamp), the row hit the attempts cap, and two
zombie siblings from earlier attempts kept editing the worktree — coordinated to stand-down
via agent-message, work merged from the tree.

## 2. The loop (final form, amended — see §2b)

1. **Movement → renew.** The reconciler renews when the harness observed the assignee do
   ANYTHING since the last sweep (delta, never wall-clock recency). Nudge state clears.
2. **Expiry, no movement → NUDGE.** One sentinel nudge; the row records a single nullable
   `nudge_id` (latest only, overwritten per nudge). The nudge requires the worker to RESPOND
   via `board_heartbeat` FIRST (the response channel — §2b), then continue / advance / fail.
3. **Next expiry → the ordinal check:** any session events AFTER the sentinel's transcript
   position, or any heartbeat whose arrival postdates the nudge? Neither → WEDGED (`failed` +
   evidence + reclaim). Either → back to 1.

The sentinel rides the existing agentmessage-v1 additive grammar; the wedge question is
ORDINAL (position in the harness's totally-ordered event log + the row's server-clock
`last_heartbeat_at` — no timestamp is ever compared across processes). Even a thinking model
emits turn-start; silence on BOTH channels after a positioned sentinel is a machine verdict.

## 2b. board_heartbeat: repurposed as the nudge-response channel (2026-10-28 ruling)

The branch briefly REMOVED board_heartbeat wholesale (the first addendum, f9379730b: "that
should not exist. agent is the least reliable way to do it."). The owner superseded it hours
later: **"When we nudge, we also require the call to board_heartbeat, rather than having to
history dive."** The tool survives as the CONCRETE REPLY to a lease-nudge:

- **Periodic self-report semantics die.** A heartbeat on a row with NO live nudge never
  extends the lease — no standing renewal discipline anywhere (liveness is platform-observed;
  the movement ladder renews). The arrival is still recorded (`last_heartbeat_at`, server
  clock) because the wedge predicate reads it ordinally (arrival > nudged_at = response).
  The 409 claim_token arbitration and the #1847 future-clamp are unchanged (worker stamps are
  response events now, not liveness input — the future direction still never stores).
- **A heartbeat on a nudged row renews**: nudge slot cleared, lease extended, watermark
  stamped with the clamped observedProgressAt. Respond FIRST, then run long commands.
- **The wedge predicate is three-legged**: nudged ∧ no post-sentinel events ∧ no post-nudge
  heartbeat ⇒ wedge. The heartbeat is OR'd in at EVERY rung; at the no-evidence rung (Nil
  source, degraded adapter, unindexed session) it is the ONLY wedge detector —
  deliver-a-message + call-a-tool is the minimal harness capability (the harness-agnostic
  wedge floor). That rung's ladder now runs nudge → heartbeat-or-wedge whenever a sender is
  wired; pin 6's honest wall-clock lapse now applies only when there is NO sender either.
- **The stillborn consultation leaves Heartbeat**: a zero-turn session cannot call tools, so
  it can never answer a nudge — the wedge verdict owns corpses. The `turnEvidence` seam
  survives (the #1959c bounce re-dispatch is its consumer).
- **Recorded consequence (deliberate)**: `in_review` persona claims never carry a nudge slot
  (the ladder observes and nudges `in_progress` rows only), so reviewer heartbeats no longer
  renew — reviewers keep the 45m window, the re-arm pass's #1813 accounting, and verdict
  writes; reviewer-side nudge machinery is out of scope for this branch (the worklog records
  it as the known follow-up if reviewer churn reappears).
- The nudge prose carries the FULL contract (verbatim intent): "First RESPOND (call
  board_heartbeat once) to confirm you are active. Then: continue working if there is more
  work to do; otherwise move your task to its next stage (board_move_stage with your
  claimToken), or declare it failed with a reason if you cannot complete it. Do NOT restart —
  assess your current state."

## 3. The harness-agnostic seam

`pkg/board.AssigneeLivenessSource` (`liveness.go`): per-session movement-delta, existence
(dead-evidence only when provably gone), and the ordinal events-after-nudge answer. Nil =
inert; every engine guarantee holds at Nil (today's wall-clock semantics — pinned). The
production adapter (`api/internal/app/board_liveness.go`) composes two same-source evidence
legs — the 0069 frame stream (a PART_START tool-part is movement the instant it happens: a
worker mid-command longer than the lease window renews; usagestream `Config.OnFrame`,
late-bound via `ProxyHandler.SetFrameObserver`) and session_index message_count deltas.
Honesty mapping: deleted_at = dead-evidence; unindexed = NO-evidence (never death); a failed
batched read degrades the sweep to no-evidence, loud.

`NudgeSender` (in `api/internal/app/board_liveness.go`): the agentmessage-v1 `lease-nudge`
sentinel + the full contract prose (§2b: RESPOND first via board_heartbeat, then continue /
advance / declare failed with a reason — never restart), delivered over the V1 route with the
bounce seam's fast-fail window. Delivery runs as ONE bounded-concurrent batch per RC1 pass
(8 lanes; each delivery carries the ~10s fast-fail window and the victims' FOR UPDATE locks
are held until commit — sequential delivery would hold them N windows during exactly the
mass-quiet incident class).

## 4. Lease default: 15m → 45m (the recorded rationale)

Wall-clock is the DEATH detector, not the liveness detector. Under the ladder, movement
renews and the quiet case gets the nudge — so the window's only remaining job is bounding how
long a PROVABLY gone assignee's row can stay pinned. 15m demonstrably burned live workers
mid-verification (long tool runs the watermark never stamped — the exact incident); 45m
covers the longest observed legitimate silent stretch (a 10+ min integration suite mid-command)
with three full nudge-ladder cycles inside a working window, while still returning a dead
session's row the same day. Two knobs only (window + escalation ceiling) — see rejected
alternatives. Changed: `DefaultEngineConfig`, viper default `board.leaseSeconds` (900→2700),
helm values + template dig fallback.

## 5. pending_input becomes a FLAG; failed is the terminus

The stage is REMOVED from the storable set: a `pending_input` flag (+ origin) rides the row on
its real stage — no return-to-original-stage machinery (the row never moved). Attempts
exhaustion lands `failed` + origin=escalated (re-arm exits it); the lease wedge verdict lands
`failed` with evidence. Claim predicates exclude flagged and failed rows (pinned). The WIRE
keeps the literal as the flag-op alias: filter = flagged rows, move = set the flag — the
engine maps it (`StageInVocabulary`/`WireStageVocabulary` the single sources). Surfaces
re-pinned: api handlers, pkg/mcp + agentd MCP vocabularies/descriptions, frontend (badge on
the real stage; Failed side column; stageDragKey drag affordance), sdks ts/go + openapi.
Migration `000052_seamless_lease`: existing pending_input rows reconstructed (escalated →
failed; deliberate parks keep their real stage + flag); the tasks_pending_input_set check
admits exactly the paired flag form plus the failed/escalated terminus.

## 6. Rejected alternatives (with reasons)

- **Recency-based renewal** — cross-process timestamps are skew-fragile (cached observation,
  90s clock offset → renewing dead workers). Deltas + ordinals only.
- **Three knobs** (window + nudge-grace + escalation tier) — over-knobbling; the ordinal
  ladder needs exactly the window and the ceiling.
- **Affinity state machines** (pin rows to known workers) — re-creates stuck rows; the
  spawner-mint invariant already guarantees deliverability.
- **Nudge-grace tiers** (mid-turn nudge escalation) — a mid-turn nudge is undeliverable
  (queues behind the frozen turn); the sentinel-by-position design makes tiers moot.
- **Attested sentinels** — over-engineering per ruling ("forgery is not the end of the
  world"; single-tenant: a forged nudge at worst talks a worker into a recoverable
  attempt-burn).
- **Per-episode nudge bounds** — superseded by latest-nudge-only (one nullable slot,
  idempotent by nudgeId).

## 7. Tests

- The 11 design pins (spec §Pins, pins 9-11 added by the §2b amendment) + ladder-contract
  pins (`pkg/board/lease_pins_test.go`): deterministic RC1 via the `rc1.post` failpoint +
  the expireLease clock seam. Includes the decisive production-wiring pin: a PART_START
  frame renews an expired claim (3×).
- The #1847 heartbeat pins re-pinned to the response semantics (`heartbeat_1847_test.go`):
  the future-stamp rejection and mild-skew clamp survive; the evidence-gate/watermark-heal
  pins die WITH the gate (extension no longer consults the watermark — the C1 lockout class
  is unreachable by construction, not healed).
- 32 old-semantics tests re-pinned to the flag/failed shapes (stage-equality assertions →
  flag assertions; transition-row counts → flag events); the re-pinning surfaced and fixed
  four real engine defects (migration constraint, stage.go fromKey alias + exit guards +
  drift-review landing, phantom escalation transition, reconciler secs::$n syntax errors).
- Suites: pkg/board `-tags integration` (private DB clone — the shared test DB is contended),
  api/internal/handlers, cmd/workspace-agentd (442s), frontend vitest 2516/2516, tsc,
  golangci-lint 0, sdks go build/vet + tsc.

## 8. Files

`pkg/board/{liveness.go,reconciler.go,engine.go,stage.go,board.go,rearm.go,reads.go,...}`,
`api/migrations/000052_seamless_lease.*` + `helm/migrations/` mirror, the app wiring
(`api/internal/app/{board_liveness.go,app.go,...}` + usagestream OnFrame),
surfaces (api handlers, pkg/mcp, agentd mcp_server, frontend board family, sdks, openapi),
this worklog + the spec, CHANGELOG, and the populated-migration pin
(`api/migrations/migration_000052_populated_test.go`).

## 9. Review round 2 (the AI reviewer's CHANGES_REQUESTED, all findings addressed)

- **000052 populated-DB orderings (findings 1+2)**: the constraint swaps now run
  drop-old → rehome-UPDATEs → add-new in BOTH directions (a constraint swap validates
  existing rows at ADD time; rehoming under the still-old vocabulary is what makes the
  live production rows migratable). Pinned end-to-end on a throwaway database parked at
  000051 with seeded escalated/deliberate-park/backlog rows
  (`api/migrations/migration_000052_populated_test.go` — up shapes, down reversal); CI's
  empty-schema idempotency/round-trip gates alone let the original defect through.
- **Episode-slot inheritance (finding 3)**: nudge_id/nudged_at/last_heartbeat_at are
  CLAIM-LIFECYCLE state — every mint (claimOnce, TaskStart, personaClaimSQL, bounce
  re-entry, RC2 re-entry, exitPendingReclaim), every reclaim (RC1 wall-clock, both
  persona lapse passes), throttle, the wedge verdict, and every stage transition
  (updateStageTx) reset all three. Pinned (TestPin_LeaseLadder_ReclaimClearsEpisodeSlots):
  a dead-evidence reclaim after a nudge clears the slots; the fresh claimant's periodic
  heartbeat is advisory; a later quiet expiry at the no-evidence rung opens a FRESH
  nudge episode — never a wedge inherited from the dead one.
- **RC2 re-entry flag blindness (finding 4)**: both re-entry branches write
  `pending_input=false` alongside `pending_origin=NULL` (a drift_review-parked row
  previously violated the paired-form constraint and poison-pilled every sweep at RC2).
- **Send→stamp race (finding 5)**: NOT lost — safe by Postgres clock semantics: now() is
  the TRANSACTION clock, so nudged_at stamps at the sweep-tx START, which strictly
  precedes every delivery made inside it; a heartbeat CAUSED by the nudge starts its own
  transaction after the delivery, blocks on the sweep's FOR UPDATE, commits after it, and
  the response arm's `nudge_id IS NOT NULL` predicate matches (or, racing the slot write,
  its advisory arrival still stamps strictly later — the ordinal rescue). The comment at
  the nudge apply arm now states this trace, and TestPin_LeaseLadder_HeartbeatInSendWindowIsNotLost
  pins it mechanically: a heartbeat fired mid-delivery lands as the RESPONSE (Extended),
  and the episode is over (slot cleared, no wedge on the next quiet sweep).
- **Episode-unique audit ids (finding 6)**: the nudge and wedge events key on the
  SENTINEL id (`reconciler:nudge:<task>:<nudgeId>`, `reconciler:wedge:<task>:<nudgeId>`)
  — deterministic AND episode-unique (renewals never bump failure_count; the human re-arm
  resets it; the old count-keyed ids collided across episodes and ON CONFLICT DO NOTHING
  silently dropped the audit rows).
- **Claim predicates (spec Part 2's pinned claim set)**: candidateSQL +
  eligibleProjectsSQL (both EXISTS arms) + TaskStart's pre-check and under-lock re-check
  exclude pending_input-flagged rows (`stage='failed'` was already invisible to the
  backlog-only candidate set). Pinned: TestPin_ClaimPredicates_ExcludeFlaggedAndFailed
  (the claim takes the healthy row; TaskStart refuses the flagged row with the guard
  class).
- **Multi-replica ordinal hazard (robustness)**: AssigneeObservation gains
  NudgeMarkKnown — EventsAfterNudge is only a MEASUREMENT when the source holds the
  sentinel's send-mark; a replica that never saw the sentinel (it didn't send it; a
  restart; an eviction) answers mark-unknown and the engine RE-NUDGES (latest-only makes
  the duplicate benign) — an unmeasured silence is never a wedge verdict.
- **Robustness others**: the seam carries the sweep ctx (ObserveAssignees(ctx, ...)); the
  hub's send-marks TTL-evict (2h — windows are 45m) and the frame/cursor maps cap at
  8192 entries with oldest-quarter pruning over the cap; a degraded observation sweep
  increments an expvar gauge (`board_liveness_degraded`, /debug/vars) — the spec's
  "silent fleet regression to wall-clock semantics must be visible" made observable.
- **Style**: board.go's wireStageVocabulary is one composite expression (dead store +
  init-append gone); the e2e V1 fixture helper's latent -1 index fixed; openapi.yaml
  re-based onto main's formatting (the ~300-line quote-style/flow-seq churn reverted;
  only the semantic hunks remain — 55 diff lines).
- **Worklog filename**: carries the `NNNN_YYYY-MM-DD_slug` date segment the renumber
  bot's sentinel requires (repolint WorklogSentinelPattern).
