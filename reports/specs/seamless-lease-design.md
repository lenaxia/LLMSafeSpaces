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
   ANYTHING since the last sweep (delta, never wall-clock recency). Nudge state clears.
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
    MovedSinceLastSweep bool          // renewal evidence
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
