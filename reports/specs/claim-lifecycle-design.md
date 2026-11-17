# Claim lifecycle design — event-driven entry, one progress contract, states as LEXICON (#2060 port; records lane #2066)

Provenance: this document is the port of issue #2060's high-level design (the epic's first
deliverable, #2060 §Purpose), landed by the records lane #2066 — folded into the #2065
due-marking PR per the 2026-11-15 audit ruling ("no separate records lane"). It is
**verbatim-plus-corrections**: #2060's text is carried with its incident traceability intact,
corrected where the landed code or an owner/audit ruling superseded it, and every correction is
marked.

**Relation to `seamless-lease-design.md` (reconcile, never supersede):** the seamless-lease doc
owns the LADDER semantics (movement/renewal, the sentinel's ordinal wedge question, the nudge
loop, the two clocks, the #2043 lapse arm, the #2044 budget — its Parts stand unchanged). THIS
doc owns the lifecycle state vocabulary, the epoch-evidence model, and the event-driven ENTRY
the ladder accelerates into. The seamless-lease doc's "observed the session do ANYTHING" was
already amended to the two-class movement contract (#2042, PR #2052) — that amendment stands,
is referenced here, and is not restated.

**The audit ruling that shapes this port (2026-11-15, governing):** the lifecycle states below
are **VOCABULARY for alert surfaces, not an implementable machine**. Nothing in this document
authorizes a state-machine rewrite, a new states column, or the 22→8 collapse — that is #2060's
phase 4, which keeps its owner go/no-go gate. The states exist so alerts (#2047's `worker_silent`
was first), dashboards, and incident prose can NAME the shapes the shipped machinery already
produces.

## Part 1 — the motivating measurement (verbatim, #2060)

`tsk-solvent-reservoir-speeds`: worker's last turn ended `stop` at **19:51:13**; the ladder's
first challenge (nudge) landed **21:38:59** — **1h47m detection latency** for a claim-holder
that had stopped working — and the worker answered in 20s. The gap between "worker stopped" and
"board noticed" is the design target this document closes (minutes, via events; the sweep
remains the convergence backstop).

## Part 2 — principles (verbatim; each traceable to an incident)

1. **One progress contract** — worker-activity + advancement defined once (the #2042 predicate),
   consumed as projections; never re-derived. *(#2040: five divergent alive-derivations.)*
2. **Bounded trust on every weak channel** — anything that is not worker-progress or a
   worker-issued tool call has capped or zero decision authority. *(#2040 unbounded weak
   authority; round-1's cap nearly re-created it via mutual re-arm.)*
3. **Event-driven entry, sweep as backstop** — transitions trigger on events; the 30s sweep
   guarantees convergence, never detection latency. *(solvent-cyclone: 1h47m.)*
4. **Provenance discipline** — control-plane emissions never ride the model-visible channel and
   never serve as workload-decision inputs. *(#2040 settle loop; #2030 stamp bleed → replay
   hygiene lane.)*
5. **Poor facts, rich semantics** — few facts at the seam; complexity lives in one decision
   plane.

## Part 3 — components and responsibilities (verbatim + landed corrections)

```
opencode harness   → transport truth: turns, parts, tool calls; emits turn events
agentd             → delivery ledger (owns the message-transport FSM);
                     turn-event emission (session id + board-advance flag);
                     origin attestation on the WIRE only (#1849 posture unchanged);
                     NO board logic, NO liveness publication
board engine       → per-claim epoch-evidence + ladder (the decision plane)
observation layer  → projections of the one contract (hub, alerts, UI badges)
```

*Correction (landed shape, #2064):* the advance flag is derived at agentd's own MCP dispatch
choke point (`callMCPTool` — the narrow set `board_record_progress` | stage move | verdict,
keyed on the harness-attested `lsp_injected_session` ONLY; presence reads and heartbeats never
advance), and the emission site is structurally pinned to the harness-attested settle fold on
BOTH harnesses (the anti-#2040 emission-site rule, audit ruling (a)).

## Part 4 — the lifecycle states — a LEXICON, not a machine

*Framing corrected per the 2026-11-15 audit ruling: these name the shapes the shipped machinery
produces, for alert surfaces and incident prose. They are not columns, not a states enum, not a
rewrite target.*

**Primary vocabulary:** `active` (mid-turn; busy gate) → `productive` (worker activity in
window) → **`stale`** (no worker progress > threshold — the #2043 progress clock past
`HeartbeatLapseThreshold`; catches stop-without-advance in minutes) → `challenged` (nudge armed)
→ `orbit-bound` (#2044 budget) → `wedge/reclaimed` → `re-armed`.

**Orthogonal dims (flags, not stages):** `pending_input`, `paused`, `persona` (in_review),
`cooldown`, escalation ledger (attempts/failures).

**Degraded modes (explicit + alerted):** `evidence-degraded` (quiet, never alive — the #2042
opaque class), `wedge-suspect` (#2045 canary), `deploy-lag` (fix merged, fleet stale — see the
decision record, Part 8; a named condition, deliberately NOT a lifecycle mode).

## Part 5 — the data model (verbatim, as FUTURE design)

*Status corrected: this is #2060's phase-4 proposal — owner go/no-go at that point. This port
records it as design-lexicon lineage for the `ClaimEvidence` concepts (epoch, LastAdvance,
ChallengeCount) that the SHIPPED code already carries as columns and counters; the collapse
itself is not scheduled by this document.*

```go
type ClaimEvidence struct {
    Epoch           int       // increments at every claim/re-claim
    LastAdvance     time.Time // stage move | verdict | board_record_progress
    LastTurnEnd     time.Time // last turn-end event (this epoch)
    TurnNoAdvance   int       // consecutive turn-ends without LastAdvance moving
    LastWorkerFrame time.Time // worker-activity stamp (telemetry; zero authority)
    ChallengeCount  int       // consecutive nudges without response-or-advance
}
```

Kept as columns today: `stage`, `claim_token`, `lease_expires_at` (backstop timer), and — as
landed by #2043/#2044 — `last_progress_at` (worker-anchored), `last_renewed_at` (renewal
clock), `no_advance_renews` (the budget), alongside the nudge/heartbeat slots. The 22 → ~8
collapse remains the phase-4 mechanical sweep behind its owner gate.

## Part 6 — the decision table (verbatim + where each row LIVES)

| Event | Rule | Where it lives (shipped) |
|---|---|---|
| worker-activity frame | telemetry bump; **no decision** | the hub's worker index (opaque class: zero authority) |
| turn-end, board advance in turn | epoch `LastAdvance` bumps; `productive` | `board_turn_advances_total` (telemetry only) |
| turn-end, no advance (claim holder only) | lease marked due → quiet arms immediately | **the #2065 due-mark** (`ConsumeTurnEnd` → guarded write → next sweep) |
| lease due, progress within threshold | renew | the ladder's evidence rungs |
| lease due, progress stale | nudge → silence ⇒ wedge (ordinal, unchanged) | the #2043 lapse arm + the seamless-lease ladder |
| nudge response + advance | `ChallengeCount=0`; renew | the response arm + #2044 reset semantics |
| nudge response, no advance | `ChallengeCount+1`; at budget ⇒ wedge | `no_advance_renews` + `NoAdvanceHeartbeatCap` |
| busy mid-turn | renew until busy cap; then treat as due | the busy gate + `boardBusyRenewCap` |
| management/settle frames | **nothing** (zero authority) | the #2042 opaque class |
| pending_input / paused / depose | existing semantics unchanged | the guards (sweep predicates + due-mark guards) |

Defaults (owner-ratified): stale threshold = 2 lease windows (90m) — **the ONE knob,
`HeartbeatLapseThreshold`** (audit ruling (c): the event path consumes #2043's knob; no second
90m tuning surface exists or is authorized); challenge budget = 3 (`NoAdvanceHeartbeatCap`).

## Part 7 — the shipped event path (what landed; the record)

- **Emission (#2064, PR #2075):** `EVENT_TYPE_BOARD_TURN_END{advanced, epoch, turn_duration_ms}`
  at the harness-attested settle fold, claim-holders only, both harnesses; the pi leg journals
  the same payload to the shared-volume turn-end journal (`/workspace/.opencode-board-turnends`)
  as the emission artifact — **unconsumed by the due-mark path today** (wiring its tail into the
  attested stream is story-1 follow-up scope; until then pi pods ride the sweep backstop
  verbatim, epic R5).
- **Consumption (#2065, this PR):** the usagestream fold's second observer → consumer-side
  epoch watermark (duplicate/stale-ordinal drop) → `Service.ConsumeTurnEnd` — ONE guarded
  UPDATE (`lease_expires_at=now()`) with the R4 guards (pending_input, paused, armed nudge slot,
  already-due idempotence, row-side mint/epoch guard), the workspace join, and both identity
  keys (#1817). The next sweep runs the UNCHANGED quiet arms.
- **Replica arbitration (#2060 open-question 1 — resolved):** the reconciler's own single-row
  guarded-UPDATE pattern (the freeze arm, `suspend.go`) — predicates re-checked at write time,
  `RowsAffected==0` means converged-by-them; cross-replica duplicate deliveries collapse on the
  already-due predicate. No SKIP LOCKED (that arbitration belongs to the sweep's multi-row
  victim scan), no leader election.
- **Observability:** `board_due_marks_total{trigger=event|sweep_lapse}` (the share signal,
  R6) + `board_turn_advances_total`; `board_due_marks_dropped` (expvar) for fold-overflow
  drops.
- **Zero liveness authority (#2042, unchanged):** the event is opaque-class in the movement
  allowlist (pinned); the consumption path touches no worker index, renews nothing, stamps no
  progress clock.

## Part 8 — decision record: deploy-lag surfacing (#2060 open-question 3)

**Decision:** deploy-lag surfaces as the metric `llmsafespaces_deploy_lag_seconds` — a gauge
exported from the version seam (`pkg/version.BuildTime`, the single source of truth every
version surface already reads; the release workflow ldflags-injects it from the tagged
commit/build), value `now − BuildTime`, ABSENT (not zero) when `BuildTime` reports `unknown`
(an unstamped build cannot measure lag; absence is loud by the seam's own missing-injection
design). Alert threshold: **14 days** (≈ two release trains beyond a remediation landing, at
the current weekly-ish cadence; an owner-ratifiable default — the same discipline as the
seamless-lease doc's 45m record). **Rejected alternative:** the release-train annotation — it
answers "when did the train leave", not "how stale is THIS replica's binary", and it is blind
to canary or pinned deployments that skip trains; revisit only if ldflags injection proves
untrustworthy in-chart. **Explicitly not a lifecycle mode** (audit ruling): deploy-lag is a
named condition in the degraded-mode lexicon (Part 4), never a board state, never a ladder arm.

## Part 9 — the phase-graph drift note (#2066 R4, recorded 2026-11-16)

The #2060 lane table drifted against the shipped record in three ways; the issue text stands as
history, this section is the correction of record:

1. **Citation error (audit ruling (e)):** the phase-1 row "#2053 replay hygiene" is WRONG —
   #2053 is the repolint schema-vs-code consumer pin (open, unrelated to replay). The replay
   hygiene work (identity keys out of the model-visible transcript) is a separate in-flight
   board lane.
2. **Status drift:** #2060's table lists #2043 (open), #2044 (open, after phase 1), #2045/#2047
   (open). Landed since: #2043 (PR #2067 — two clocks + lapse arm), #2044 (the heartbeat-orbit
   budget), #2045 (admission canary), #2047 (worker_silent + assignee_quiet). Phase 2 is this
   epic (#2061: #2064 + #2065 merged, #2066 folded here). Phase 4 (column collapse) remains
   owner go/no-go — and is narrowed by the lexicon ruling above.
3. **Open questions 1 and 2 are closed** (Part 7: replica arbitration resolved; the
   advance-flag join landed at the choke point). Open question 3 closes with Part 8.

## Part 10 — explicit non-goals (verbatim, #2060)

No unified state service (freshness spans 4 orders of magnitude: SSE sub-second → busy
per-frame → board 45m → billing per-message). No driver continuation on turn-end (loops
prose-emitting models; the ladder is the continuation). No removal of origin attestation (only
its transport channel changes). No new evidence channels without a provenance flag.

## Part 11 — pins (where the born-red contracts live)

- Emission (#2064): `cmd/workspace-agentd/sessionstate/board_turn_end_test.go`,
  `board_turn_ledger_test.go`, `pi_board_turnend_test.go`,
  `TestBoardTurnEndWiring_RealChainEmits` — emission-site rule, claim-holder filter, advance
  set, zero-liveness allowlist pin (`TestLeaseLivenessHub_ManagementEventsAreNotWorkerMovement`).
- Due-marking (#2065, this PR): `pkg/board/due_mark_2065_integration_test.go` (2065-A..G: the
  incident-shape delta pin, telemetry-only advance, guards, idempotence, foreign identity,
  row-epoch drop, sweep-lapse/degraded pin) and `api/internal/app/board_due_mark_test.go`
  (filter/identity/watermark/overflow/panic-containment + the composite zero-liveness pin).
