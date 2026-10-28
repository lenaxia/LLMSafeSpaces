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
   `nudge_id` — overwritten on each nudge, nothing older tracked.
3. **Next expiry → the ordinal check: any session events AFTER the sentinel's transcript
   position?**
   - **No → WEDGED**: `failed` with evidence, reclaim. (Even a thinking model emits turn-start;
     silence after a positioned sentinel is a machine verdict, not a guess.)
   - **Yes → back to 1.** (The worker self-advances or self-fails per the nudge prose.)

### The sentinel (clock-skew solution, by position not time)

```
<!-- lsp:agent-message-v1 {"mode":"lease-nudge","taskId":"tsk-…","nudgeId":"<uuid>","expects":"advance|fail"} -->
Your claim on <task> has gone quiet. Do NOT restart. Assess your current state: either move the
task to its next stage, or declare it failed (with a reason) if you cannot complete it.
```

- Rides the existing `agentmessage v1` additive grammar + the existing `WorkerMessenger` seam
  (one more directive alongside halt/resume).
- **The wedge question is ordinal**: "are there session events after the message carrying this
  nudgeId?" — answered in the harness's totally-ordered event log. No timestamp is ever compared
  across processes. The engine's wait window is engine-local and only decides WHEN TO ASK.
- **No attestation machinery** (single-tenant ruling: forged nudge at worst talks a worker into a
  recoverable attempt-burn; the sentinel is an operational marker, not an auth token).
- Ack sentinels are advisory telemetry only — ANY post-sentinel movement is the discriminator;
  prose replies count (forward-compat for grammar-ignorant workers).

### The harness-agnostic seam (Nil = inert, guarantees hold at Nil)

```go
// pkg/board — imports nothing harness-specific; adapters live harness-side.
type AssigneeLivenessSource interface {
    // ObserveAssignees reports, per session: movement since the last sweep
    // (delta), existence (dead-evidence when provably gone), and whether any
    // events follow the given nudge marker (ordinal query; "" = skip).
    // Calls carry a ~2s timeout; failure = no-evidence, NEVER death.
    // K consecutive timeouts circuit-break the source to Nil until recovery
    // (loud gauge — silent fleet regression to heartbeat semantics must be visible).
    ObserveAssignees(ctx context.Context, sessions []AssigneeRef) map[string]Observation
}

type Observation struct {
    MovedSinceLastSweep bool          // renewal evidence
    Exists              bool          // false = dead-evidence → fast reclaim
    EventsAfterNudge    bool          // ordinal; valid only when nudged
}
```

- Wake capability is a harness-level wiring fact (adapter + messenger present), NOT per-session:
  board assignees are spawner-minted top-level sessions by construction (subagents never hold
  claims). PIN: claims issue only to spawner-minted sessions — the invariant that keeps the
  nudge always deliverable.
- Degradation: no source → wall-clock + worker heartbeat (unchanged semantics); the sentinel
  ordinal query is answerable by ANY harness with a session event log — a lower bar than
  timestamp reporting.

### Why the ordinal form collapses prior machinery

- Busy worker: renewing — never nudged.
- Frozen-busy worker: nudged; sentinel queues behind the frozen turn; no post-sentinel events
  can ever exist → wedge. Correct verdict with no delivered/queued/presented tracking.
- Idle worker: sentinel presents at the boundary; any turn it starts is positionally after → alive.
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
3. Nudged + next expiry + no events after sentinel → `failed` with wedge evidence + reclaim.
4. Nudged + any post-sentinel activity → renewed, nudge state cleared.
5. Dead-evidence (session gone) → fast reclaim within one window, no nudge wasted.
6. Silent assignee, Nil source (no evidence either way) → honest lapse within one window
   (today's semantics; never a wrong reclaim of a provably-moving worker).
7. Claims issue only to spawner-minted top-level sessions (the wakeability invariant).
8. 409 claim_token arbitration untouched. Existing suite passes unchanged with Nil wired.

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

## Ruling addendum (2026-10-28): board_heartbeat is REMOVED

"that should not exist. agent is the least reliable way to do it." — the agent-facing
self-report surface dies with this branch (agentd catalog + dispatch + tests), completing
the #1847 arc that already "retires worker-declared stamps as the liveness input entirely."
The engine-internal watermark survives, fed SOLELY by platform observation (the adapter).
Rung 3 (Nil liveness) becomes pure wall-clock + headroom — honest, and the incentive to
wire an adapter is the correct pressure. The rung-3 "worker heartbeat fallback" language
above is superseded by this addendum.
