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

## 2. The loop (final form)

1. **Movement → renew.** The reconciler renews when the harness observed the assignee do
   ANYTHING since the last sweep (delta, never wall-clock recency). Nudge state clears.
2. **Expiry, no movement → NUDGE.** One sentinel nudge; the row records a single nullable
   `nudge_id` (latest only, overwritten per nudge).
3. **Next expiry → the ordinal check:** any session events AFTER the sentinel's transcript
   position? No → WEDGED (`failed` + evidence + reclaim). Yes → back to 1.

The sentinel rides the existing agentmessage-v1 additive grammar; the wedge question is
ORDINAL (position in the harness's totally-ordered event log — no timestamp is ever compared
across processes). Even a thinking model emits turn-start; silence after a positioned
sentinel is a machine verdict.

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

`NudgeSender` (`board_nudge.go`): the agentmessage-v1 `lease-nudge` sentinel + the surgical
prose ("Do NOT restart; assess; advance or declare failed with a reason"), delivered over the
V1 route with the bounce seam's fast-fail window.

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

- The 8 design pins (spec §Pins) + ladder-contract pins (`pkg/board/lease_pins_test.go`):
  deterministic RC1 via the `rc1.post` failpoint + the expireLease clock seam. Includes the
  decisive production-wiring pin: a PART_START frame renews an expired claim (3×).
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
(`api/internal/app/{board_liveness.go,board_nudge.go,app.go,...}` + usagestream OnFrame),
surfaces (api handlers, pkg/mcp, agentd mcp_server, frontend board family, sdks, openapi),
this worklog + the spec, CHANGELOG.
