# Seamless lease handling + pending_input-as-flag (owner-approved design, 2026-10-28)

Owner rulings (verbatim, this session):
- "Work shouldn't be moving to fresh claimants. The lease length is wrong then and the lease handling."
- "No the lease mechanism shouldn't be so disruptive. We need to make it more robust and seamless."
- "Must be harness agnostic."
- "If a thread goes idle, we shouldn't reclaim the lease from it. We should nudge the thread and tell it to either move itself to the next step or to failed if it cannot complete."
- "We should be able to detect a wedge: we sent a nudge, and then the lease expires again and we check and there is no response to our nudge. Even a thinking model would result in some kind of response."
- "The point of the sentinel is to avoid clock skew." / "Forgery is not the end of the world. We don't need to over engineer it." / "We only need to track the most recent nudge."

Live evidence: three mass-churn cycles 2026-10-27/28 — workers lost claims mid-verification to
lease lapse (15m default; watermark stamps completed activity only, never tool-call START), were
re-fed to fresh claimants, burned attempts to the cap, and escalated to pending_input wearing an
owner-question costume. A pod memory-reap reclaimed all four in-flight rows at the same
microsecond on wall-clock with no session-death evidence. The lease fix and the failed-stage fix
were themselves among the eaten rows — the board cannot land its own rescue.

## Part 1 — the seamless lease

### The loop (final form)

1. **Movement → renew.** The reconciler renews when the harness observed the assignee session do
   WORKER-ACTIVITY since the last sweep (delta, never wall-clock recency; #2040 amendment:
   message/part activity only — management frames (session-state settles, reseeds, lease-state
   transitions, input materializations, errors, unknown types) are the OPAQUE class and carry
   ZERO renewal authority: the row takes the quiet arms (nudge → verdict) on the FIRST
   settle-only evidence — a settle after a sentinel is not an answer, and settle-true-forever
   cannot hold the lease). Nudge state clears.
2. **Lease expiry, no movement → NUDGE.** One sentinel nudge; the row records a single nullable
   `nudge_id` — overwritten on each nudge, nothing older tracked. The nudge requires the worker to
   RESPOND via `board_heartbeat` FIRST (the response channel — item 6 amendment below), then
   continue / advance / declare failure.
3. **Next expiry → the ordinal check: any session events AFTER the sentinel's transcript
   position, or any heartbeat AFTER the sentinel was sent?**
   - **Neither → WEDGED**: `failed` with evidence, reclaim. (Even a thinking model emits
     turn-start; a delivered message plus a callable tool is the minimal harness capability —
     silence on BOTH channels after a positioned sentinel is a machine verdict, not a guess.)
   - **Either → back to 1.** (The worker self-advances or self-fails per the nudge prose.)

### The sentinel (clock-skew solution, by position not time)

```
<!-- lsp:agent-message-v1 {"mode":"lease-nudge","taskId":"tsk-…","nudgeId":"<uuid>","expects":"respond|advance|fail"} -->
BOARD LEASE NUDGE: your claim on task <task> has gone quiet. First RESPOND (call board_heartbeat
once, with your claimToken) to confirm you are active — respond before running any long command so
the wedge clock clears. Then: continue working if there is more work to do; otherwise move your
task to its next stage (board_move_stage with your claimToken), or declare it failed with a reason
if you cannot complete it. Do NOT restart — assess your current state.
```

- Rides the existing `agentmessage v1` additive grammar + the existing `WorkerMessenger` seam
  (one more directive alongside halt/resume).
- **The wedge question is ordinal**: "are there session events after the message carrying this
  nudgeId, OR a heartbeat whose arrival postdates the nudge?" — answered in the harness's
  totally-ordered event log plus the row's server-clock `last_heartbeat_at`. No timestamp is
  ever compared across processes. The engine's wait window is engine-local and only decides
  WHEN TO ASK.
- **No attestation machinery** (single-tenant ruling: forged nudge at worst talks a worker into a
  recoverable attempt-burn; the sentinel is an operational marker, not an auth token).
- The heartbeat response is the REQUIRED reply (owner ruling: "when we nudge, we also require
  the call to board_heartbeat, rather than having to history dive") — but ANY post-sentinel
  movement still saves at the observed rungs; prose replies count where the grammar is unknown
  (forward-compat for grammar-ignorant workers).

### The harness-agnostic seam (Nil = inert, guarantees hold at Nil)

```go
// pkg/board — imports nothing harness-specific; adapters live harness-side.
type AssigneeLivenessSource interface {
    // ObserveAssignees reports, per session: movement since the last sweep
    // (delta), existence (dead-evidence when provably gone), and whether any
    // events follow the given nudge marker (ordinal query; "" = skip).
    // Calls carry the sweep's context + a ~2s budget; failure = no-evidence,
    // NEVER death, and a degraded sweep must be VISIBLE (the production hub
    // exports an expvar gauge — board_liveness_degraded).
    ObserveAssignees(ctx context.Context, sessions []AssigneeRef) map[string]Observation
}

type Observation struct {
    MovedSinceLastSweep bool          // renewal evidence (worker activity only — #2040)
    MovedOpaque         bool          // management-frame delta: ZERO renewal authority
                                      // (telemetry; quiet arms on first evidence; #2042)
    Exists              bool          // false = dead-evidence → fast reclaim
    EventsAfterNudge    bool          // ordinal; valid only when NudgeMarkKnown
    NudgeMarkKnown      bool          // THIS source holds the sentinel's mark —
                                     // an unmeasured silence is NEVER a wedge
                                     // verdict (the engine re-nudges instead)
}
```

The mark-known rule is the multi-replica safety arm: send-marks are
process-local, so a replica that did not send the row's sentinel answers
mark-UNKNOWN (never a false "nothing after it") and the engine re-nudges —
a duplicate sentinel is benign under latest-only; a wrongful wedge is not.

- Wake capability is a harness-level wiring fact (adapter + messenger present), NOT per-session:
  board assignees are spawner-minted top-level sessions by construction (subagents never hold
  claims). PIN: claims issue only to spawner-minted sessions — the invariant that keeps the
  nudge always deliverable.
- Degradation ladder (the wedge predicate `nudged ∧ no post-sentinel events ∧ no post-nudge
  heartbeat ⇒ wedge` runs at every rung; the heartbeat arm — `board_heartbeat` arrival recorded
  on the row at server clock — is OR'd in as a strong ALIVE signal EVERYWHERE):
  - no source, sender wired: the nudge ladder still runs — deliver-a-message + call-a-tool is
    the MINIMAL harness capability (the harness-agnostic wedge floor); the heartbeat is the
    ONLY wedge detector at this rung.
  - no source, no sender: pure wall-clock honest lapse (today's semantics; the sentinel ordinal
    query remains answerable by ANY harness with a session event log — a lower bar than
    timestamp reporting).

### Why the ordinal form collapses prior machinery

- Busy worker: renewing — never nudged.
- Frozen-busy worker: nudged; sentinel queues behind the frozen turn; no post-sentinel events
  can ever exist, and no heartbeat can be mid-turn → wedge. Correct verdict with no
  delivered/queued/presented tracking.
- Idle worker: sentinel presents at the boundary; any turn it starts is positionally after, and
  the contract's first instruction (respond via board_heartbeat) clears the wedge clock
  immediately → alive.
- Dead session: no sentinel in any transcript → dead-evidence path.

### Knobs and bounds

- **Two knobs only**: the lease window (raised, ~45m rationale in worklog: wall-clock is the
  DEATH detector, not the liveness detector) and the escalation ceiling.
- **Ceiling** (held too long WHILE moving — activity≠progress is information-theoretic; the
  looping worker): escalate to owner (genuine pending_input flag), never silent re-pool. The
  wedge detector owns the non-motion class; the ceiling owns motion-without-progress.
- **Renewals write no audit events** (transitions only). Nudge state persists ON THE ROW
  (restart-safe; the memory-reap must not forget or double-nudge — nudgeId makes duplicates
  idempotent).

### Pins (born-red; fake clock + deterministic sweep injection are part of the job)

1. Live worker (advancing activity) never lapses across 3× the lease window.
2. Lease expiry + no movement → exactly one nudge recorded on the row.
3. Nudged + next expiry + no events after sentinel AND no post-nudge heartbeat → `failed` with
   wedge evidence + reclaim.
4. Nudged + any post-sentinel activity (events OR heartbeat response) → renewed, nudge state
   cleared.
5. Dead-evidence (session gone) → fast reclaim within one window, no nudge wasted.
6. Silent assignee, no source AND no nudge seam (no evidence either way) → honest lapse within
   one window (today's semantics; never a wrong reclaim of a provably-moving worker). With a
   sender wired but no source, pin 9's floor applies instead.
7. Claims issue only to spawner-minted top-level sessions (the wakeability invariant).
8. 409 claim_token arbitration untouched. Existing suite passes unchanged with Nil wired.
9. Nudge → heartbeat response ⇒ renewed (the response channel works at EVERY rung — including
   the no-evidence rung where it is the only detector; the response clears the nudge slot and
   the worker may then run long commands safely).
10. Nudge → silence on BOTH channels (no post-sentinel events, no post-nudge heartbeat) ⇒ wedge
    — including at the no-evidence rung with only a sender wired (the harness-agnostic floor).
11. Periodic-only heartbeat with no nudge ⇒ NO extension (the standing discipline stays dead;
    the arrival is recorded, the lease is not moved).

## Part 2 — pending_input becomes a FLAG (not a stage)

- `pending_input` stage is REMOVED; a `pending_input` flag (+ origin/reason) rides the row while
  the row keeps its real stage. No "return to original stage" machinery — the row never moved.
- Escalation (attempts exhausted) lands in **`failed`** (the destination the wedge verdict and
  the owner's exhaustion ruling share).
- Flag set/clear = 2 audit events; UI shows a badge over the real stage (where the blocked thing
  lives), not a costume column.
- Claim predicates: `stage ∈ claimable AND !pending_input_flag AND stage != failed` — PINNED
  (a missed flag check would feed an owner-blocked row).
- Migration (one-time): existing pending_input rows reconstructed — escalated → `failed`;
  deliberate parks keep their prior real stage. Wire/openapi/frontend enum shrinks: breaking
  change, versioned.

## Constraints

- pkg/board stays harness-clean (interface + engine; adapters wire at the opencode integration
  point; sessionstate consulted read-only).
- Worklog records the REJECTED alternatives with reasons: recency-based renewal (skew), three
  knobs (over-knobbling), affinity state machines (re-create stuck rows), nudge-grace tiers
  (mid-turn nudge undeliverable), attested sentinels (over-engineering per ruling), per-episode
  nudge bounds (superseded by latest-nudge-only).

## Ruling addendum (2026-10-28, later same day): board_heartbeat is REPURPOSED, not removed

The earlier addendum in this file ("board_heartbeat is REMOVED") is SUPERSEDED by the owner's
follow-up ruling: "when we nudge, we also require the call to board_heartbeat, rather than
having to history dive." The tool survives as the CONCRETE REPLY to a lease-nudge — the
nudge-response channel — and the sentinel's ordinal wedge question gains its second leg:

- **The periodic self-report semantics die.** No standing renewal discipline anywhere: a
  heartbeat on a row carrying no live nudge NEVER extends the lease (liveness is
  platform-observed; the movement ladder renews). The catalog/charter prose stops suggesting
  periodic heartbeats. The arrival is still recorded (row column `last_heartbeat_at`, server
  clock) because the wedge predicate reads it.
- **The wedge predicate is three-legged**: nudged ∧ (no post-sentinel events) ∧ (no post-nudge
  heartbeat) ⇒ wedge. The heartbeat arrival is a strong ALIVE signal OR'd in at EVERY rung, and
  at the no-evidence rung (no adapter/transcript access) it is the ONLY wedge detector —
  deliver-a-message + call-a-tool is the minimal harness capability (the harness-agnostic
  wedge floor), so that rung's ladder now runs nudge → heartbeat-or-wedge whenever a sender is
  wired.
- **The nudge prose carries the full contract** (verbatim intent): "First RESPOND (call
  board_heartbeat once) to confirm you are active. Then: continue working if there is more
  work to do; otherwise move your task to its next stage (board_move_stage with your
  claimToken), or declare it failed with a reason if you cannot complete it. Do NOT restart —
  assess your current state." Respond FIRST so the wedge clock clears before any long command.
- **The #1847 future-clamping stays**: worker stamps are response events now, not liveness
  input — a response stamp is still stored on the row (clamped/watermark), so the future
  direction stays server-authoritative.
- Pins 9-11 (above) pin the channel: response ⇒ renewed; silence on both channels ⇒ wedge;
  periodic-only heartbeat ⇒ no extension.

## Review round 2 amendments (2026-10-28, later still)

The AI reviewer's CHANGES_REQUESTED round, folded in (worklog §9 carries the full record):

- **Migration ordering is a populated-DB contract**: constraint swaps validate existing
  rows at ADD time, so 000052 (both directions) runs drop-old → rehome → add-new. Pinned
  on a throwaway database seeded with the pre-000052 world (escalated park, deliberate
  park, plain backlog) — CI's empty-schema gates alone let the original order through.
- **The episode slots are claim-lifecycle state**: nudge_id/nudged_at/last_heartbeat_at
  reset at every mint, reclaim, throttle, wedge, and stage transition — a dead episode's
  sentinel must never inherit into a fresh claim (pinned: the reclaim clears them; the
  fresh claimant's periodic heartbeat stays advisory; a fresh quiet episode re-nudges,
  never wedges).
- **The send→stamp window is safe by Postgres clock semantics**: now() is the transaction
  clock, so nudged_at stamps at the sweep transaction's START — strictly before every
  delivery made inside it — and the response's arrival therefore stamps strictly after it
  (pinned mechanically: a heartbeat fired mid-delivery lands as the response).
- **Audit ids are sentinel-keyed** (episode-unique and deterministic): a count-keyed id
  collides across episodes and ON CONFLICT DO NOTHING silently drops the audit row.
- **The claim predicate is pinned on the worker path**: candidateSQL, the project
  eligibility EXISTS arms, and TaskStart's guards exclude flagged rows (failed rows were
  already invisible to the backlog-only candidate set).
- **Nudge delivery is one bounded-concurrent batch per sweep** (the victims' row locks
  are held until commit; sequential delivery would hold them N fast-fail windows during
  the mass-quiet incident class this design answers).
- **The hub evicts**: send-marks TTL out at 2h (windows are 45m); the frame/cursor maps
  cap with oldest-quarter pruning; a degraded sweep increments a visible gauge.

## #2043 amendment (2026-11-15, the FMD-Web incident remediation): the two clocks + the heartbeat-lapse arm

The 2026-11-15 incident (#2040) exposed the ladder's original sin: the renewal write stamped
`now()` into `tasks.last_progress_at` (reconciler's `ladderRenew` arm), so a row the platform kept
renewing was indistinguishable from a row the worker kept progressing — each renewal deferred
wedge/reclaim detection by a full lease window, indefinitely (renewal stamps 00:27/00:32/01:12 read
as a working thread for hours). The fix splits one overloaded column into two clocks and builds an
arm on the honest signal.

### The two clocks (the definition)

- **The renewal clock — `tasks.last_renewed_at`.** When the LADDER last extended the lease on
  evidence. Writers: the ladder's `ladderRenew` arm ONLY (`reconciler.go` — movement, post-nudge
  reaction, busy-gate renewals — the write that used to lie). Two deliberate exclusions, so an
  operator correlating this column against `board_lease_renewals_total` reads renewal recency for
  LADDER renewals only: the DIRECT heartbeat-response arm (`stage.go`'s live-nudge response write)
  extends `lease_expires_at` on evidence but does not stamp the column (its renewal is counted by
  the counter's `source=heartbeat`, and its progress write anchors the PROGRESS clock instead —
  v1 telemetry scope, per review round 1), and the suspension freeze (suspend.go) keeps its
  deliberate no-clock discipline (it never touched `last_progress_at` either; its renewals are
  evented with source `workspace_suspended` — extending it here changes no decision).
- **The progress clock — `tasks.last_progress_at`.** When the WORKER last evidenced progress.
  Writers: claim mints (`stage.go`, `engine.go`, `persona.go` — claim time is a legitimate anchor),
  the drift-review closed-unmerged re-claim (`reconciler.go` — a fresh claim epoch, claim-mint
  semantics), and heartbeat RESPONSES (`stage.go` — the worker-declared `observedProgressAt`,
  #1847-clamped server-side). NOTHING else writes it; in particular the ladder renewal does not,
  ever (born-red pinned: `TestPin_2043A_RenewalWritesRenewedClockNotProgress`).

Backfill (migration 000055, conservative mid-deploy): `last_renewed_at` initializes from
`last_progress_at` — in-flight rows carry today's semantics across the deploy; the arm first fires
only after the threshold has genuinely elapsed with no worker progress. No behavior jump at the
boundary. NULL progress rows stay NULL in the renewal clock.

### The heartbeat-lapse arm

When a lease-due row's PROGRESS clock is older than `HeartbeatLapseThreshold` (new
`board.EngineConfig` knob beside `LeaseDuration`, validated the same way; default 90m = 2 lease
windows: one nudge-measurement window + one response-grace window), the observation plane loses
renewal authority for the row: `MovedSinceLastSweep`, `Busy`, and `EventsAfterNudge` are read as
false — the row proceeds through the ordinary quiet arms (nudge → heartbeat-or-wedge). Frames
without progress were exactly the incident's renewal-forever orbit, so evidence of ACTIVITY is no
longer evidence of LIFE past the threshold. Deliberately preserved by the override's placement
(after the response rung, before the evidence rungs):

- **The heartbeat response still wins** (`hbAfterNudge` is checked before the override): a worker
  that answers the sentinel renews — and its response's `observedProgressAt` write refreshes the
  progress clock, so the arm is reset for free. The arm is STATELESS (recomputed from the row each
  sweep): any worker-anchored progress write — heartbeat response or a fresh claim mint — resets it.
- **Dead-evidence reclaim is preserved** (a provably-gone session still takes the fast wall-clock
  path; the arm suppresses RENEWALS, not reclaims).
- **The no-evidence floor is unchanged** (Nil source rows were already nudge → heartbeat-or-wedge;
  the arm only hardens the observed rungs).
- **Nil progress = lapsed-by-default** (no evidence of worker progress is no movement veto — the
  honest direction; pinned).
- **The arm touches no accounting**: attempts, `failure_count`, and `busy_renews` are untouched by
  the arm itself — escalation belongs to the wedge verdict the arm feeds, and to #2044's orbit
  budget.
- **The suspension freeze outranks the arm** (freeze runs before the ladder): a suspended workspace
  cannot answer, so its rows renew without progress evaluation — the 2026-11-13/14 class stays
  closed.

The lapse computation is SERVER-side in the victims query (`last_progress_at IS NULL OR
last_progress_at < now() - threshold`) — one clock domain (Postgres `now()`), the same discipline as
every other ladder comparison; never a cross-process wall-clock read.

Observability (R3): the arm is always-on visible through (a) the new counter
`board_ladder_heartbeat_lapse_total` — renewal evidence suppressed by a stale progress clock, the
incident's counterfactual in one scrape — and (b) the nudge payload's `Reason` field
(`heartbeat_lapse`), so the harness-side sentinel and any bus projection attribute WHY the worker
was asked. Deliberately NOT a label on `board_lease_renewals_total`: a lapse suppression is the
opposite of a renewal decision, and that counter's forensic value is its honesty (renewals with a
movement/heartbeat source on sessions that emitted nothing is the #2040 signature) — the issue's
"or the nudge payload carries it" alternative arm is the chosen shape. Board events are unchanged
(nudges and wedges already audit).

### Boundary with #2044 (the no-advance budget — stated per the issue)

A heartbeat RESPONSE writes the worker-DECLARED `observedProgressAt` into the progress clock: a
compliant-but-stuck worker could declare fresh progress every nudge episode and keep the lapse arm
reset forever. That loop is #2044's jurisdiction — declared progress without stage advance does not
reset ITS budget counter. This issue's arm trusts the progress clock exactly as far as
`last_heartbeat_at` semantics already do (an ordinal arrival signal), no further; the two stories
compose: the lapse arm bounds evidence-based renewal, #2044's budget bounds response-based renewal.
Pins here (`TestPin_2043C_WorkerProgressResetsArm`) enforce the RESET, not the budget — mutating
them to also bound declared-without-advance stamps belongs to #2044's born-red set.

## #2044 amendment — the heartbeat-orbit budget (the third orbit channel, closed)

The live-verified orbit (#2040, `tsk-polished-keystone-waits` 2026-11-15): a worker that answers
every sentinel with `board_heartbeat` seconds after it lands satisfies the nudge contract's letter
(FIRST respond) while doing none of its substance (then continue / advance / declare failure).
Post-#2042 and post-#2043 that worker's honest declarations — "my last observed progress" — never
cross the claim mint, but `hbAfterNudge` renews at every rung regardless, so the row orbits
nudge → response → renew indefinitely with `attempts` never moving. The budget closes it.

- **The detector is server-side, one clock domain**: forward motion per claim epoch =
  `last_progress_at > stage_entered_at`. The claim mint writes both in one statement (equal at
  mint), so the claim itself never reads as motion; heartbeat responses are the only other writer
  of the progress clock (#2043's split). A declared stamp past the mint = motion; a stale or nil
  stamp = motionless. No cross-process comparison ever runs.
- **`NoAdvanceHeartbeatCap` (K, default 3)** on the board engine config (deploy knob
  `board.noAdvanceHeartbeatCap` / `LLMSAFESPACES_BOARD_NOADVANCEHEARTBEATCAP`, helm-wired; <=0
  clamps to the default). `tasks.no_advance_renews` (migration **000056**, the 000055 cohort —
  the sibling landed first and stayed single-purpose) counts CONSECUTIVE motionless heartbeat
  responses per claim epoch: +1 at every motionless response renewal (the handler's response arm
  AND the ladder's heartbeat belt), 0 on motion, on every non-heartbeat renewal (movement, busy,
  reaction — activity implies advance), and at every claim-path mint; the wedge verdict and the
  suspension freeze zero it (epoch breaks).
- **The decline**: the K+1th motionless response still records its arrival (and counts its cycle)
  but does NOT renew — the sentinel stays ARMED, the lease is not extended, and the result's
  reason names the budget ("N heartbeats without advancement"). The next lapse then demotes
  EVERY renewal arm for that lapse — `hbAfterNudge` skips, and `MovedSinceLastSweep`, `Busy`, and
  `EventsAfterNudge` read false — so the row proceeds to the ordinary quiet arms and the existing
  wedge verdict (`StageFailed` through the unchanged reconciler path, evidence prose +
  `board_events` payload carrying `no_advance_renews` + `no_advance_cap`).
- **Why every arm — the #2042 interaction**: `board_heartbeat` is itself a tool call; its
  PART_START/PART_DELTA/PART_END frames are worker-activity frames under #2042's allowlist and
  stamp the movement index and the ordinal query like any tool's frames. A budget that demoted
  only the heartbeat arm would leave the orbit alive through `RenewSourceReaction` (and the
  movement arm). At cap, presence-shaped frames renew nothing — for ONE lapse, with the belt that
  a row whose progress clock HAS crossed the mint never demotes (unreachable through the write
  discipline; guards drift).
- **The mover boundary, stated honestly**: because the seam cannot distinguish a mover's tool
  frames from the heartbeat's own, a cap-exhausted row whose worker starts real work between
  responses cannot prove it through frames alone — and must not (that proof re-opens the orbit).
  The mover's protection is the respond-first contract the sentinel prose already demands: its
  next heartbeat declares a watermark past the mint, which is MOTION — reset and renewal in the
  same guarded write, at cap (pinned). Below cap, movement renewals reset the budget continuously,
  so a worker that moves never approaches it.
- **R5 — the response-arm guard, verdict**: the incident's 4.4s-late heartbeat took the advisory
  path with the sentinel's row state committed. By code trace the response and advisory predicates
  differ ONLY in `nudge_id IS NOT NULL` (same stage set, same token, same pending_input leg) — no
  token-identity mismatch exists, and the guarded UPDATE evaluates its WHERE on the live locked
  row (the send-window pin proves the racing shape lands RESPONSE by lock ordering). The reachable
  advisory outcome for an ANSWERED sentinel is the EARLY-side race: the worker responds to a
  sentinel whose row write has not landed (at-least-once bus redelivery of an old sentinel, or
  delivery preceding the sweep's commit) — the row genuinely carries no armed nudge, the advisory
  path is the predicate doing its job, and the worker's NEXT response converges (pinned). The
  deterministic committed-sequence pin locks the RESPONSE contract; born-green, documented here.

Pins (born-red against pre-fix main, `pkg/board/heartbeat_orbit_2044_integration_test.go`):
2044-A the orbit cycle sequence (K renewing motionless responses → decline → wedge; pre-fix: the
counter stayed 0 and the row renewed forever), 2044-B the demotion per arm, 2044-C fresh
declarations never bounded, 2044-D movement resets + the ladder belt counts/skips + the at-cap
mover's response-channel recovery, 2044-E the R5 committed-sequence response pin + the early-side
race documentation, 2044-F reviewer personas structurally exempt (no sentinel ever arms on
in_review).
