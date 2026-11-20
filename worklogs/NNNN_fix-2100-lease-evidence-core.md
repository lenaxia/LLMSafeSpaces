# Worklog: fix #2100 — P1 rebaseline + loud artifacts, P2 store-native evidence transport (behind its flag)

Task: the #2100 fix lane (due-marking dead/masked in prod). Design of record:
`reports/specs/lease-evidence-core-{high,low}-level-design.md` (merged as PR
#2105). Charter: `reports/charters/fix-2100-session-charter.md`. Evidence:
`reports/evidence/tsk-redolent-garden-rockets/README.md` — the measured
incident (park 16:56:20.80Z → nudge 18:26:55.08Z = 90m35s = exactly two 45m
lease windows).

## What landed

### P1 — rebaseline + loud failures (unconditional)

- **Bug B (masking), the design's §1.1:** `boardLivenessHub.Rebaseline`
  (api/internal/app/board_liveness.go) re-bases the observation cursors at the
  CURRENT stamps for every ref form (engine ref + resolved raw/phrase keys).
  The turn-end consumer calls it on the drain goroutine after every due-mark
  the sink lands. The first-observation zero-frame-baseline can no longer read
  the parking turn's trailing frames as `MovedSinceLastSweep` one sweep later.
  Born-red proven behaviorally: with the rebaseline call disabled, both
  `TestPin_2100B_*` and `TestPin_2100_Timeline_ParkToNudgeTwoSweeps` FAIL on
  this tree (re-enabled: green).
- **Bug A (dead recording), §1.2:** the three board file artifacts default
  under `/exchange` (the ONE dual-RW subtree, FLAT names per 0070):
  `.opencode-board-workers`, `.opencode-board-advances`,
  `.opencode-board-turnends`. Compat: the legacy `/workspace` paths are read
  ONLY when the remediated file is absent (one release of in-place-upgrade
  grace), never written. Hydration is per-line-skip (poisoned lines never
  error) and ON-DEMAND: `isWorker` on an unknown id re-reads the file on mtime
  movement — the emitting half sees sidecar appends without restarts.
- **Loud degradation, §1.2:** `record()` WARNs and increments
  `board_workers_registry_write_failures` (the silent-EROFS class dies); the
  advance tail's open failures increment `board_advance_tail_write_failures`;
  the pi journal already errored loudly per append.
- **Channel heartbeat, §1.3:** `board_turn_end_events_folded_total` (expvar,
  counted at fold ACCEPTANCE — post-watermark) +
  `Service.SetTurnEndFoldSource` → the `board_health` `event_channel` line
  `{last_fold_age_seconds, claims_active, degraded}` on BOTH health surfaces
  (user + workspace scope). Stale fold age (>10m) with active claims renders
  `degraded: true` — the #2100 observability gap (claims existed, events
  folded 0, nothing compared them).

### P2 — store-native evidence transport v2 (behind its flag, default ON)

Flag: `LLMSAFESPACES_BOARD_PRESENCEV2` (BOTH halves — engine + agentd, default **true** — the
class-kill IS the fix; the flag is the kill-switch restoring v1 emission
verbatim after a process restart). The charter's separately-gated "(default
off)" applies to the 45→20m LEASE retune, which is deliberately NOT here
(design §6: admitted only after one release cycle of counter evidence; the
decouple is the latency lever, and `LeaseDuration` remains its own knob).

- **Emission ungated (§2.1):** `sessionstate.Config.BoardTurnEndUngated` —
  every harness-attested busy→idle transition publishes
  `EVENT_TYPE_BOARD_TURN_END`; the claim-holder registry consult (the gate the
  incident proved EROFS-dead) is not even called. The pi leg mirrors it
  (no registry scan, no advance-tail consult, journals `advanced=false`).
  Payload is additive: `turn_started_at_unix_ms` (the busy-mark anchor; ABI
  regen via buf, sessiongen re-run).
- **Consumption store-side (§2.2):** `Service.ConsumeTurnEnd` derives
  `advanced` from the store — `NOT EXISTS` on `board_transitions` (actor
  session inside the turn window) plus the verdict arm on `board_events`,
  inlined INTO the guarded UPDATE's WHERE (the decision is the statement's
  own predicate; an advisory pre-check keeps `turn_advances_total` honest).
  Wire-compat: a pre-v2 emitter (no turn window) falls back to the payload
  flag verbatim. File artifacts are no longer authoritative when the flag is
  on: nothing consults them (the registry keeps ONLY its same-half
  self-certification-refusal role; the tail is belt for a flag flip-back).
- **`NudgeMeasureWindow` (§3.1):** new engine knob (default 10m; `<=0`
  clamps at validation; the config loader floors the deploy knob at 300s).
  The nudge rung extends the lease by THIS, not `LeaseDuration` — the row
  re-enters the victims scan at +window. Deploy surface:
  `board.nudgeMeasureWindowSeconds` / `LLMSAFESPACES_BOARD_NUDGEMEASUREWINDOW`,
  helm-wired (values + api-deployment env). Deviation from the design's
  letter, stated: the "floor 5m" is enforced at the DEPLOY loader (not the
  engine rung) so the integration pins can run sub-floor windows exactly like
  the 200ms-LeaseDuration precedent — the reconciler honors any validated
  positive value.
- **Response-observed presence (§3.2, scoped):** a heartbeat carrying NO
  `observedProgressAt` is a valid response — the arrival substitutes the
  SERVER clock (pre-fix: the zero time stored year-1, a client-hoped clock
  the store must never trust). The full watermark retirement (declared
  stamps ignored for motion, `last_advance_at` consolidation) is design
  Phase 3.3, sequenced behind its own migration and NOT in this PR — the
  charter's pins pin the response contract, not the clock rename.

## Pins (all green; the masking + window pins proven born-red by mutation)

- app: `TestPin_2100B_DueMarkThenRebaseline_NextSweepIsQuiet`,
  `TestPin_2100_Timeline_ParkToNudgeTwoSweeps` (the incident, ≤2 sweeps,
  state-delta asserted), `TestPin_2100_ConsumerFoldsCounter`.
- agentd: default-paths-under-/exchange, sidecar-write-lands,
  loud-on-write-failure (expvar + WARN), tail-open-loud,
  hydrate-on-demand, frozen-old-path-never-wins (+ poison tolerance),
  pi v2 ungated/gated.
- sessionstate: ungated user-session emits + probe unconsulted, gated default
  preserves R1, payload carries turn_started_at.
- board (integration): advance-from-store suppresses the due-mark (telemetry
  +1, lease untouched), pure-assignee-match due-mark with NO registry,
  user-chat inert under store derivation, nudge-rung-extends-by-window,
  wedge-at-window-not-lease, late-responder-inside-window renews,
  watermark-less heartbeat renews + stamps the server clock,
  health event_channel line (absent unwired / degraded stale / healthy fresh).

## The deploy caveat (the charter's Done-means item)

**The fix must roll to the workspace pods.** The emitting halves live in
`cmd/workspace-agentd` (sidecar + supervise-pi): until the runtime image
rolls, prod keeps burning on the OLD binaries (registry consult gated,
`/workspace` artifact paths, no rebaseline) — API-side gains are inert for
pods on old agentd. Post-roll, the frozen `/workspace/.opencode-board-workers`
artifact is read at most ONCE (legacy grace, only while the `/exchange` file
is absent) and never again written.

## The fold-in (round 2, orchestrator direction)

The engine-side flag arrived from a parallel WIP worktree
(`/workspace/presence-v2-wt`, uncommitted): its `PresenceTransportV2`
EngineConfig field + API config plumbing + the unified env name
(`LLMSAFESPACES_BOARD_PRESENCEV2`) are folded in — BOTH halves of the
transport now honor ONE kill-switch, and the store-side derivation is
gated on it (off ⇒ the declared payload flag is trusted verbatim,
pinned). Its Phase-3.2-full heartbeat shape (declared watermark ignored
for ALL clock purposes, `last_progress_at` unwritten by responses) is
NOT folded: it re-designs the #2044 budget's motion input (2044-C's
fresh-declare pin) and belongs with design Phase 3.3's advance clock —
the WIP worktree keeps it for that lane.

## Sequenced follow-ups (out of this PR by design)

- Phase 3.3 (`tasks.last_advance_at`, declared-watermark retirement for
  motion, `000062` drop of `last_progress_at`) — needs its own migration +
  the #2044 re-key pins.
- Phase 4 lease retune 45→30→20 — gated on measured
  `board_due_marks_total{trigger=event}` share ≥ 90% over a release cycle.
- The nudge prose re-compose ("or simply show any activity") — rides WITH
  3.2's full watermark retirement so the prose and the clock agree.
