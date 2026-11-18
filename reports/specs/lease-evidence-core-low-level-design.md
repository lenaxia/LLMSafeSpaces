# Lease/nudge evidence core — low-level design (implementation contract)

**Status:** Design (2026-11-18) — the HOW companion to
`lease-evidence-core-high-level-design.md` (the WHAT/WHY and the Part A verdict table).
Anchors verified against `0aedbaf0a` (origin/main, 2026-11-18). Four phases, each a lane with
born-red pins; each phase lands independently valuable and leaves main green. THE pin for the
whole design (Phase 1's): the live 90.5-minute park→nudge shape, asserted as state deltas,
compressing to ≤ 2 sweeps.

---

## Phase 1 — #2100 remediation (prod goes honest this release)

Scope = the un-filed fix story the charter named (`tsk-stirred-harbor-hums` does not exist on
the board — this phase IS that story's scope; file it against this design).

### 1.1 Bug B — cursor rebaseline at due-mark time (the masking fix)

The masking: the hub's first-observation zero-baseline plus the parking turn's own frames read
as `MovedSinceLastSweep` on the first post-mark sweep (`board_liveness.go:466-485`), renewing
the just-marked lease and erasing the acceleration.

- `boardLivenessHub` gains `Rebaseline(ctx, refs []string)`: one `resolveRefs` batch, then
  under `h.mu` set `cursors[ref] = boardLiveCursor{frameStamp: <current worker stamp for the
  row's raw+phrase keys>, opaqueStamp: <current opaque>, dbCount: <row.messageCount>}` for
  every ref form (the engine's ref AND the resolved raw/phrase keys — cursors key on the ref
  as passed, `board_liveness.go:527`). Idempotent; safe to call repeatedly.
- Wiring: `boardTurnEndConsumer` gains the hub (constructor param, `app.go:1910`); when its
  sink (`Service.ConsumeTurnEnd`) returns `true`, it calls `hub.Rebaseline(ctx, e.SessionRefs)`
  on the drain goroutine. Failure logs loud and is retried by nothing — the next due-mark
  re-runs it; the cost of a missed rebaseline is one window (today's behavior).
- **Pin (born-red, the composition pin #2100 says is missing):**
  `TestPin_2100B_DueMarkThenSweep_QuietArmsWithinOneSweep` — seed frames for a session
  (pre-mark movement in the delta window), fire turn-end(no-advance) → due-mark →
  `Rebaseline` → next `ObserveAssignees` reports `MovedSinceLastSweep=false` → the ladder
  NUDGES (pre-fix: `ladderRenew`). Asserted as row/state deltas, not wall-clock.
- **Pin (the incident, end-to-end):** `TestPin_2100_Timeline_ParkToNudgeTwoSweeps` — claim,
  park (turn-end, no advance), assert: due-mark fired (trigger=event counter +1), first sweep
  nudges (not renews), nudge_id recorded. With Phase 1's bug-A remediation live this is the
  90m→≤1 sweep compression, pinned.

### 1.2 Bug A remediation (minimal, knowingly disposable) — registry under /exchange

Phase 2 deletes this machinery; it exists so prod is honest for the releases between:

- `boardWorkersDefaultPath` → `/exchange/.opencode-board-workers` and
  `boardAdvancesDefaultPath` → `/exchange/.opencode-board-advances` (the dual-RW subtree,
  `pod_builder.go:231-241`; FLAT names — the 0070 no-nested-exchange-paths rule; both files
  are JSONL tails ≤ their compaction bounds, far under the 256 KiB-class posture of payload
  files). **Threat model, stated honestly:** `/exchange` is a SHARED agent-writable subtree
  by design (mounted RW in the runtime container for the agent's write tool — 0070 §2.4a's
  flat contract), so these files are agent-readable AND agent-writable; they are not 0070
  payload files and never ride the payload read discipline. Consequences, owned rather than
  papered over: (a) hydrate MUST tolerate poisoned/corrupt lines exactly as today's per-line
  scanner already does (`board_workers.go:66-73` skips unparseable lines; the tail's
  `readBoardAdvanceStamps` unmarshal-skips) — pinned below; (b) through Phase 1's lifetime a
  worker could fabricate an advance record into the agent-writable tail and suppress its own
  claim's due-mark (the R2 advance arm) — the worker-declared lying class §5/Phase 3 exists
  to close, bounded here to self-harm (the row falls back to sweep-lapse semantics, today's
  behavior; single-tenant pod, the owner's forgery ruling applies) and DELETED with the tail
  in Phase 2, which is the permanent closure.
- Hydrate-on-demand: `boardWorkerRegistry.isWorker` re-reads the file when the id is unknown
  (mtime-gated: stat, rehydrate only on change — the file is append-only; the consult stays a
  bounded small read). The main half then sees sidecar appends without restarts.
- Loud degradation: `record()` and the ledger append increment new expvars
  `board_workers_registry_write_failures` / `board_advance_tail_write_failures`; `record()`
  additionally gains a WARN (it is the silent one today — `board_workers.go:81-101` returns
  on file error with no signal, which is how the freeze stayed invisible); the tail already
  WARNs per append/open failure (`board_turn_ledger.go:180`, `:122`) and gains only the
  counter and the once-per-process discipline. The real gap both halves share is that no
  surface compares claims-active against events-folded end-to-end — that is 1.3's, not this
  bullet's.
- Compat: one release of reading BOTH paths (old `/workspace` file if the new one is absent)
  so an in-place upgrade keeps pre-existing entries; then the old paths are never written.
- **Pin:** `TestPin_2100A_SidecarRegistryWriteLands` — write via the sidecar path env, read
  from a fresh registry hydrated at the new path (pre-fix: the write EROFS-fails silently and
  the reader never sees the id). **Pin:** `TestPin_2100A_LoudOnWriteFailure` — unwritable
  path ⇒ counter increments, WARN logged (pre-fix: `record()`'s silence). **Pin:**
  `TestPin_2100A_HydrateToleratesCorruptLines` — a registry/tail file seeded with poisoned
  lines (garbage JSON, wrong-shape records, a fabricated session id) hydrates per-line-skip
  without error, and a poisoned CORRUPT line never flips an advance verdict the memory
  ledger contradicts (the scanner posture, pinned against the new path).

### 1.3 Channel heartbeat (the monitoring half of "silent failure impossible")

- `boardTurnEndConsumer` gains `foldedTotal` (expvar `board_turn_end_events_folded_total`).
- `board_health` gains an `event_channel` line: `{last_fold_age_seconds, claims_active}` —
  computed in the health assembly pass (reads.go); when `claims_active > 0` and
  `last_fold_age > 10m` the line renders `degraded: true`. This is the surface a human reads
  in their hourly check; it is what #2100 lacked (claims existed, events folded = 0, nothing
  compared them).
- **Pin:** health fixture with a stale fold age + active claims renders the degraded line.

## Phase 2 — evidence transport v2 (the class-kill; subsumes 1.2)

### 2.1 Emission: drop the claim-holder gate

- `emitBoardTurnEndLocked` (`sessionstate/board_turn_end.go:81-121`) emits for EVERY
  harness-observed busy→idle transition; the `facts.ClaimHolder` consult
  (`board_turn_end.go:97-99`, fed by `board_turn_ledger.go:368-374`) is deleted. Payload
  additive: `turn_started_at` (the authority's `busySince` anchor — the consumer's window
  needs an absolute start; proto field addition, both in-tree harnesses emit it).
- The advance flag in the payload becomes `false` always at emission (the derivation moves
  API-side, 2.2) — the field is kept for wire compat within the release train and marked
  deprecated in the proto comment.
- Volume/privacy: one content-free event per turn per session (ids, epoch, window,
  duration); user chats match no claim downstream (the consumer's assignee-match,
  `due_mark.go:112`). The R1 "claim-holders only" filter is RE-SPECIFIED as a consumer-side
  property — the #2065 pin "user sessions' events are ignored by the consumer" stays green
  and gains a pin asserting it end-to-end post-un gating.

### 2.2 Consumption: derive claim AND advance store-side

- `Service.ConsumeTurnEnd` (pkg/board/due_mark.go) gains the advance derivation INSIDE the
  guarded UPDATE (one statement, predicates re-checked at write time — the house discipline;
  no lookup-then-write race window). The WHERE grows:
  `AND NOT EXISTS (SELECT 1 FROM board_transitions bt WHERE bt.task_id = t.id AND
  bt.at > $4 AND bt.actor->>'session_id' = ANY($2))` ($4 = the event's `turn_started_at`;
  the actor JSON's `session_id` key is the #1747 view, `store.go:472` — manager/reconciler
  moves carry no session and never match), plus the verdict arm (`NOT EXISTS (... FROM
  board_events be WHERE be.item_id = t.id AND be.action = 'verdict' AND be.created_at > $4
  AND be.actor->>'session_id' = ANY($2))`). Rows matching an advance never due-mark — the
  DECISION is the guarded statement's own predicate. The `turn_advances` counter
  stays honest via a cheap ADVISORY pre-check (one SELECT of the same EXISTS, telemetry
  only, no authority: a race between check and write can miss a count, never a due-mark).
  When `board_record_progress` lands (the #2044 lane), its write joins the EXISTS set by
  table, not by derivation change.
- The consumer's epoch watermark, overflow posture, panic containment: unchanged.
- **Deleted machinery:** `boardAdvanceLedger` + tail + choke-point stamps
  (`board_turn_ledger.go` wholesale), the pi turn-end journal's claim-holder gating (the pi
  leg emits every settle-fold turn-end; `pi_board_turnend.go` drops its registry consult),
  and `boardWorkerSessions`' emission role. `boardWorkerSessions` itself SURVIVES for the
  self-certification refusal (`board_workers.go:15-26`) — which runs in the half that serves
  board MCP, where the in-memory registry is authoritative within process lifetime; its file
  becomes belt-only for THAT half's restarts (kept at the /exchange path from 1.2, hydrate
  best-effort; the pin's strict "ever was a worker" posture within the serving half is
  unchanged, and a cross-half gap is pin-visible, not silent).
- **Pins:** `TestPin_P2_EmissionUngated_UserChatInert` (a folded user-session turn-end
  matches no row, folds the counter, due-marks nothing); `TestPin_P2_AdvanceFromStore`
  (turn-end after a stage move by the assignee ⇒ `advanced_store=true`, lease untouched,
  telemetry +1 — with the ledger deleted, pre-fix on main this shape derives agentd-side);
  `TestPin_P2_NoRegistryNeeded` (a worker claim's turn-end due-marks with NO registry entries
  anywhere — the pure assignee-match path); `TestPin_P2_DeadChannelDegradesToSweep`
  (no events ⇒ today's semantics; the R5 pin re-run).

### 2.3 Observability delta

`board_turn_end_events_folded_total{workspace}` (from 1.3, now load-bearing), the
`event_channel` health line, and the existing `board_due_marks_total{trigger}` share remain
the adoption spine. Emitter side keeps `boardTurnEndsPublished`. The
registry/tail write-failure expvars from 1.2 are deleted WITH the machinery (their surfaces
cannot fail anymore — the honest monitor is the fold counter).

## Phase 3 — challenge window + response-observed presence + one clock

### 3.1 `NudgeMeasureWindow` (config knob; the latency lever)

- `board.EngineConfig` gains `NudgeMeasureWindow time.Duration` (default 10m; `<=0` clamps
  to default; validated like `LeaseDuration`, `service.go:234`; deploy knob
  `board.nudgeMeasureWindow` / `LLMSAFESPACES_BOARD_NUDGEMEASUREWINDOW`, helm-wired — the
  `NoAdvanceHeartbeatCap` pattern).
- The nudge rung's write (`reconciler.go:783-787`) extends by
  `greatest(NudgeMeasureWindow, 5m)` instead of `LeaseDuration`. NOTHING else in the ladder
  changes: the victims scan, arms, ordinal question, wedge write, and freeze are untouched —
  the row simply re-enters the scan sooner.
- Interplay audit (the edges the review will probe, answered here): (a) busy rows never arm
  the sentinel (`reconciler.go:612-631` order), so mid-turn workers never see the short
  window; (b) a heartbeat response renews immediately via the handler's response arm
  (`stage.go:795-810`) — no expiry wait, so a responder inside the window is safe even at
  +9m59s; (c) re-nudge cadence for `NudgeMarkKnown=false` rows is unchanged (a replica
  missing the mark re-nudges; latest-only keeps duplicates benign); (d) the suspension freeze
  outranks everything as today. Documented as a `seamless-lease-design.md` amendment: the
  measurement window is now its own knob; the 45m rationale text moves from "the window" to
  "the backstop + coverage quantum".
- **Pins:** `TestPin_P3_WedgeAtMeasureWindow` (quiet row: nudge at T, wedge verdict by
  T+window+1 sweep — asserted with the fake clock; pre-fix on main this needs +45m);
  `TestPin_P3_LateResponderInsideWindowRenews` (response at window−60s ⇒ renewed, slot
  cleared); `TestPin_P3_PostSentinelTurnFramesRenew` (any new-turn frames postdate the mark ⇒
  `EventsAfterNudge` ⇒ renew — the thinking-worker save).

### 3.2 Response-observed presence; the watermark retires

- `Heartbeat` (`stage.go:737+`): `observedProgressAt` becomes optional (zero time ⇒
  arrival-only: `last_heartbeat_at=now()`, no progress write, no motion CASE). When present
  it is IGNORED for clock purposes (accepted for wire compat one release, then the parameter
  is removed): the response write's `last_progress_at=$2` and the `$2 > stage_entered_at`
  motion comparisons re-key to `last_advance_at` (3.3). The future-clamp tolerance
  (`heartbeatFutureTolerance`) dies with the parameter.
- Nudge prose (`leaseNudgeText`, `board_liveness.go:566-582`) re-composed: "First RESPOND —
  call board_heartbeat once with your claimToken (the explicit fast-path), or simply show any
  activity (a tool call, a message); either clears the wedge clock. Then: continue / advance
  the stage / declare failure. Do NOT restart." The MCP tool description for
  `board_heartbeat` matches (agentd `mcp_tools.go` + the API handlers' doc strings).
- The ordinal legs already answer presence (worker-map frames, DB count delta);
  `hbAfterNudge` stays the strong OR'd alive signal at every rung and the ONLY detector at
  the no-evidence floor — pins 9/10 of the seamless-lease suite stay green unedited.
- **Pins:** `TestPin_P3_ProseReplySavesAtObservedRung` (post-sentinel PART frames, no
  heartbeat ⇒ renew — extends the #2042 suite); `TestPin_P3_WatermarklessHeartbeatRenews`
  (arrival-only response on a nudged row ⇒ renew, slot cleared; a zero-time stamp writes no
  clock); `TestPin_P3_DeclaredWatermarkIgnored` (a response carrying a fresh stamp does NOT
  reset the #2044 budget — pre-fix on main it does; the lying-class closure).

### 3.3 One advance clock (`tasks.last_advance_at`)

- Migration `000061_task_last_advance_at` (renumber-on-collision per the 000057 precedent;
  mirrored under helm/migrations): `ALTER TABLE tasks ADD COLUMN last_advance_at timestamptz`;
  backfill `last_advance_at = COALESCE(last_progress_at, stage_entered_at)` (conservative
  mid-deploy — a declared-history row keeps its semantics for the boundary release);
  `last_progress_at` is retained read-only for one release, then dropped by
  `000062_task_drop_last_progress` (the additive-only house contract: drop in its own
  later migration).
- Writers (each already holds the guards; one term each): claim mints
  (`stage.go`/`engine.go`/`persona.go` — `last_advance_at=now()` beside
  `stage_entered_at=now()`), forward stage moves, `RecordVerdict`, the #1946 criteria grant,
  the future `board_record_progress`. NOT the heartbeat, NOT the ladder renewal, NOT the
  freeze — `last_renewed_at` also dies here (its only readers were humans correlating the
  #2040 counterfactual; that correlation moves to the renewal-source counter, which already
  exists and is always-on). Renewal/nudge/wedge writes touch neither clock.
- Readers re-key: the victims scan's lapse predicate (`reconciler.go:319-320`) reads
  `last_advance_at` staleness against `HeartbeatLapseThreshold` (STAYS 90m — it now bounds
  advance staleness, an epoch-scale property; the doc amendment states the decoupling so it
  is never "harmonized" to the lease window); the #2044 motion detector
  (`last_advance_at > stage_entered_at`) in the response write and the budget belt.
- **Pins:** `TestPin_2043A` re-keyed (`TestPin_2043A_RenewalWritesNoClock` — the renewal
  writes neither clock); `TestPin_P3_AdvanceClockOnlyPlatformWritten` (a watermark-less
  heartbeat response leaves `last_advance_at` untouched; a stage move bumps it);
  `TestPin_2044A..F` re-run green with the re-keyed detector (the orbit still bounds:
  motionless responses still count, still decline at cap, still wedge at window end);
  reviewer-persona exemption pin unchanged (2044-F).

## Phase 4 — lease retune 45m → 20m (gated, kill-switch, no migration)

- Admittance gate (all three, measured over one full release cycle after Phase 3):
  `board_due_marks_total{trigger=event}` / total ladder entries ≥ 90%; zero false-wedge
  escalations attributable to window/quantum (reviewed in board_events); the busy-cap
  counter (`RenewSourceBusy` renewals) shows no cluster at cap.
- Staged: 45 → 30 (one release), 30 → 20 (next), counter review between each. Config-only
  (`LeaseDuration` is already a knob; `service.go:234` clamps); kill switch = set it back.
- Coverage arithmetic on record (the high-level doc §6): cap×lease = 60m at 20m ≥ observed
  silent-tool-call max + margin; backstop worst-case 50m; renewal volume ≈ 2.2× today's
  (noise). `HeartbeatLapseThreshold` explicitly NOT scaled (90m stands).
- **Pins:** `TestPin_P4_CoverageFloor` (a busy-silent row at 20m lease survives
  cap×lease of sweeps — the false-wedge guard); `TestPin_P4_BackstopDetection`
  (events-dead quiet row: verdict ≤ 2 windows + 1 sweep at the tuned value).

## 5. The phased pin map, summarized (born-red = fails on the prior main)

| Phase | THE pin | Guards |
|---|---|---|
| 1 | `2100_Timeline_ParkToNudgeTwoSweeps` (the live 90m shape → ≤2 sweeps, state deltas) | `2100B` masking composition; `2100A` write-lands + loud; health degraded line |
| 2 | `P2_NoRegistryNeeded` (pure assignee-match due-mark) | user-chat inert; advance-from-store; dead-channel→sweep |
| 3 | `P3_WedgeAtMeasureWindow` (verdict ≤ window, not ≤ lease) | late-responder; post-sentinel frames; watermark-less renewal; declared-watermark ignored; 2043A re-keyed; 2044A-F green |
| 4 | `P4_CoverageFloor` (no false wedge at 20m) | backstop bound |

Order discipline: Phase 1 lands first and alone (prod honesty); Phase 2 may stack on 1 in the
same train if review capacity allows (it deletes 1.2's machinery — sequence the deletion
commit after the 1.2 pins exist on main so the pins' pre-fix baselines are recorded); Phase 3
after PR #2104 merges (its writers overlap the wait-clear sites); Phase 4 last, gated on
measured counters, never on ambition.

## 6. Sequencing against the in-flight record (the orchestrator's map)

- **PR #2104 (typed waits, OPEN)** — merges untouched; Phase 3 adds `last_advance_at=now()`
  to its wait-clearing write sites (one term each, after it lands).
- **PR #2103 (salvage-prefer, OPEN)** — merges untouched; no interaction (exhaustion sites
  do not touch the clocks; the salvage path's `wait cleared` write gains the advance term in
  Phase 3 with the others).
- **`tsk-pristine-henna-loses` (wait-event-broker, stage=failed)** — ABOVE this core, no
  interaction; needs re-arm + fresh attempt (orchestrator action, not this design).
- **The #2100 fix story** — un-filed; Phase 1 (+2) is its scope. File it against this
  design's Phase 1 charter text (the issue's own fix directions map: A→1.2 remediation then
  2.x deletion; B→1.1; loud→1.2/1.3).
- **#2060 phase 4 (ClaimEvidence collapse)** — explicitly out, owner gate stands; this
  design ships its semantic v1 (two clocks→one; declared evidence deleted; file artifacts
  deleted; registry cross-process role deleted). The remaining column set after Phase 3:
  `lease_expires_at, nudge_id, nudged_at, last_heartbeat_at, busy_renews,
  no_advance_renews, last_advance_at` (+ anchors) — every one platform-written, every one
  load-bearing in a server-side predicate; the case where a JSONB struct beats THAT set is
  the owner's to argue at the gate.

## 7. Observability (the whole spine, one place)

Counters/gauges after all phases: `board_lease_renewals_total{source}` (unchanged),
`board_ladder_nudges_total`, `board_ladder_wedges_total`, `board_ladder_heartbeat_lapse_total`
(re-keyed input), `board_heartbeat_noadvance_cycles_total`,
`board_due_marks_total{trigger=event|sweep_lapse}`, `board_turn_advances_total`,
`board_turn_end_events_folded_total` + the `event_channel` health line (the channel's
heartbeat), `boardTurnEndsPublished` (agentd expvar). Deleted with their machinery:
registry/tail write-failure counters (Phase 2), the `last_renewed_at` column's forensic role
(moves to the source counter). No new metrics classes; the Redis lease-eventbus stays
disarmed.
