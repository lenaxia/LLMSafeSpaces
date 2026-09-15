# Worklog: #1342 — interrupt-before-force-kill, orphan sweep, progress-keyed defer

**Date:** 2026-09-15
**Session:** Implement issue #1342 (S12/L11): session-aware restart must not force-kill in-flight turns without aborting them; retire the fixed 15-minute maxDefer; surface deferred credential applies.
**Status:** Complete

---

## Objective

Close the 2026-09-11 incident class: a fleet-wide credential delivery force-restarted opencode mid-turn on three workspaces (30+ minute bash commands), orphaning in-flight tool parts the UI renders as eternally "running". Implement the issue's four change items: (1) interrupt-before-force-kill, (2) orphan sweep on restore, (3) retire the fixed 15-minute maxDefer in favor of progress-keyed defer, (4) surface pending credential applies on the Workspace status.

---

## Work Completed

### Item 1 — Interrupt before force-kill (`cmd/workspace-agentd/secrets.go`)

- `makeSessionAwareRestartDecision` rewritten around `restartDecisionConfig` (collaborators + tunables bundled; the old 7-positional-param signature had grown to 9 inputs).
- New force path `forceInterruptRestart`: issues the Act `interrupt` for each busy session (bounded per-call by `interruptCallTimeout` 3s), waits `GraceWindow` (default 5s, matching `defaultRestartGrace`) for the harness to write terminal part state, THEN restarts. Interrupt errors are logged and the restart proceeds — a wedged harness must not wedge the credential apply.
- `sessionInterrupter` seam (`func(ctx, sessionID) error`) implemented by `newSessionInterrupter` (`sessionstate_wiring.go`) — the existing `opencodeActor` V1 abort route. No opencode wire shape leaked into the restart path (Rule 12).

### Item 2 — Orphan sweep on restore (`cmd/workspace-agentd/sessionstate/authority.go`)

- `Reseed(ReseedReasonGenerationChange)` captures non-terminal TOOL parts from the pre-restart projection (`captureOrphanedPartsLocked`), then folds them into the restored projection as `PART_END` with `ToolState{Status: TOOL_STATUS_ERROR, Error: "harness restart", CompletedAt}` (`OrphanSweepReason`). Subscribers see reseeded → aborted PART_ENDs in seq order.
- CRITICAL ordering (found in self-review, pinned by `TestOrphanSweep_CapturesBeforeEvidenceSweep`): the capture runs BEFORE the #1311 evidence sweep — that sweep's `clearBusyFromEvidence` nils `inFly` for ledgered sessions, which would silently drop the orphans in exactly the ledger-wired production topology.
- Reason-gated: `ReseedReasonStallWake` does NOT sweep (live turn state); boot reseed is vacuous (empty projection).
- Cumulative `OrphanPartsAborted` in `Authority.Metrics()` + `llmsafespaces_orphan_parts_aborted_total` Prometheus counter (delta-bridged in `sessionstate_metrics.go`).
- Generation-change reseed now rides the retrying `startStateAuthorityReseed` driver (bgCtx-bounded): opencode may not answer `/session` for seconds after a restart, and a one-shot reseed left the sweep unfired until the NEXT generation.

### Item 3 — Retire the fixed 15-minute maxDefer (progress-keyed defer)

- `defaultMaxDefer` deleted. `restartStallBound = sessionstate.LeaseConvergenceBound` (30s) — DERIVED from the #1312 lease-clock family, no second constants table (budget coherence).
- `sessionStatusTracker` gains `lastEventAt` (per-session last SSE event) + `busyPartitions(stallBound)` → (progressing, stalled). Activity marking happens in `processEvent` for every session-scoped event: part updates (incl. non-usage `message.part.updated` — the 40-min-build streaming shape), step-finish usage, session.status, and the legacy nested envelope. Session-ID extraction for non-usage events lives in the wire seam (`wire.SessionIDFromProps`).
- Defer is UNBOUNDED while any busy session progresses; the force path fires only when every busy session is stalled (no activity for `stallBound`). A busy session with no recorded event falls back to its busy-mark — the mark is the activity floor, so a never-observed busy session stalls one bound after the mark, never instantly (H1b liveness preserved).

### Item 4 — Pending credential apply surfaces on the Workspace status

- `pendingApplyTracker` (`cmd/workspace-agentd/pending_apply.go`): begin/refreshBusy/clear/snapshot, nil-safe methods; wired through `applySecretsDeps` → the deferred goroutine refreshes it per tick and clears it when the restart fires (or the defer is canceled).
- `agentd.HealthzResponse.PendingApply *PendingApplyHealth` (reason/waitingSeconds/busySessions) — healthz stays process-only (cached snapshot, no I/O).
- Controller (`controller/internal/workspace/health.go`) mirrors it into the new `CredentialsApplyPending` condition (`ReasonCredentialsApplyDeferred`); cleared on apply and on unreachable/undecodable/unhealthy scrapes (no evidence from a dead pod).

### Tests (TDD — written first, red before green)

- `session_tracker_activity_test.go` (12): activity marking per event shape, partitions, busy-mark fallback, prune, sessionless events.
- `session_aware_restart_1342_test.go` (14): interrupt→grace→restart order, interrupts-every-busy-session, interrupter error/nil paths, unbounded defer under streaming (the 40-min build rule), mixed progressing+stalled defers, stall-then-force, zero-fallbacks, ctx-cancel during grace, pending lifecycle (idle/force/cancel), nil-tracker safety, WaitGroup tracking.
- `sessionstate/authority_orphan_sweep_test.go` (9, external) + `authority_orphan_sweep_internal_test.go` (1, the ledger-ordering pin — mutation-verified to fail against the pre-fix ordering).
- `session_interrupter_test.go` (3): V1 abort route + Basic auth, harness 404 surfacing, ctx deadline.
- `restart_interrupt_integration_test.go` (4): the full `applySecretsBatch` pipeline against a fake harness — the incident replay (streaming defer → stall → interrupt → grace → restart → pending clear), idle fast path, harness-ignoring-interrupt, healthz surfacing.
- `healthz_pending_apply_test.go` (3), `pkg/agentd/types_test.go` (+2), `controller/internal/workspace/health_pending_apply_test.go` (3).
- Updated pre-existing pins to the new contract: `session_aware_restart_test.go` (H1b re-expressed as the stall-bound force path), `healthz_test.go`/`supervisor_status_test.go` (handler arity), `xdg_config_layer_test.go` (relayKillFunc wiring pin).

### Review round 1 (AI reviewer CHANGES_REQUESTED — all findings remediated)

- **Owner triage front 3 — transcript repair for ALREADY-orphaned running parts** (real, blocking): the incident's eternal spinners render from opencode's DURABLE store via GetHistory. Implemented read-time repair at the adapter seam (`pkg/agent/opencode/adapter.go repairOrphanedRunningTools`, both V1 and V2 store paths): when a served page carries a running tool part and the live `/session/status` registry (the same truth agentd's drain gate trusts) says the session has no running turn, the part is closed as `error`/`"harness restart"`. STRICT failure semantics — a status-fetch error or unknown status value never repairs (never false-aborts a live tool). Repairs the existing fleet damage on first read; no harness-store writes. The reason constant lives in `pkg/session` (`ToolAbortReasonHarnessRestart`); sessionstate's `OrphanSweepReason` is value-duplicated (module seal forbids the import) with the equality pinned by `orphan_reason_pin_test.go`. 8 adapter tests (`transcript_repair_test.go`).
- **E2E row missing** (real, blocking): added `local/issue-1342-graceful-restart-e2e.sh` (R1 incident replay happy path: streaming defer + `CredentialsApplyPending` + interrupt-first apply + no eternal spinner; R2 kill-9 backstop: sweep + transcript repair + the orphan metric), registered in `e2e-nightly.yml`, with structural pin tests (`local/issue_1342_e2e_script_test.go`) so rows cannot be silently dropped. Runs on the pool/nightly cluster like its us-70 siblings — not executable in this dev sandbox (no kind cluster).
- **Unasserted dead-pod condition clears** (real): `TestCheckAgentHealth_DeadPodScrapes_ClearPendingCondition` covers all three branches (unreachable / undecodable / unhealthy).
- **Metric overcount on cursor-persist failure** (real, trivial): the orphan counter advances only when the fold actually published (seq advanced).
- **Concurrent deferred applies sharing one pending surface** (real): the tracker is now reference-counted — one goroutine's clear cannot erase a sibling deferral's pending state (pinned by `TestPendingApplyTracker_ConcurrentDefers_Refcounted`).
- **Integration gaps** (real): `TestIntegration1342_HarnessHonorsInterrupt_TurnEndsTerminalNoSweep` (projection-level terminal state via the REAL SSE ingestion path when the harness honors the interrupt — sweep count stays 0), `TestIntegration1342_HarnessIgnoresInterrupt_KillThenSweepRestoresHonesty` (force-kill → generation reseed → sweep chain through the real authority + store reader), `TestIntegration1342_GenerationReseedRetriesUntilStoreAnswers` (the retrying driver), `TestGenerationChangeReseedWiredToRetryingDriver` (source pin against regression to the one-shot reseed).

---

## Key Decisions

1. **Stall bound = `LeaseConvergenceBound` (30s), by identity.** The issue says "`LeaseConvergenceBound`-class duration" and requires the one coherence-pinned #1312 table. Identity (not a multiple) is the strongest coherence; the owner's direction ("a turn silent for 30s+ has likely already wedged and the interrupt path is safe") accepts the false-stall cost because the force path is now graceful (interrupt writes terminal state; the sweep backstops).
2. **Force fires only when NO busy session progresses** (multi-session semantics). Alternative (any stalled → force all) would kill a sibling 40-min build — violating the issue's sharpest invariant ("a 40-min build streaming output must never be force-killed"). Consequence: with one eternally-streaming session plus one wedged sibling, the credential waits for the streaming turn's natural end — surfaced by the item-4 condition. Stated as an assumption in the PR.
3. **Sweep encodes "aborted" as `TOOL_STATUS_ERROR` + reason string.** The ABI's ToolStatus enum is schema-frozen; ERROR-with-reason is the honest existing encoding ("marked aborted (with a synthetic reason)" per the issue). No enum addition.
4. **Sweep is reason-gated to generation changes.** A stall-wake reseed is not a harness restart; sweeping there would abort live parts.
5. **Activity tracking in the tracker, not the authority.** The tracker already receives every SSE event, is always constructed (authority may be degraded/nil), and is the restart path's existing busyness source. The wire-shape knowledge (properties.sessionID) is contained in `wire.SessionIDFromProps` (Rule 12).
6. **Pending-apply surface on healthz (15s controller cadence), mirroring the secretsDelivery precedent** — smallest honest mechanism; statusz (60s, expensive) would delay operator visibility.
7. **Generation-change reseed retried via `startStateAuthorityReseed`** (also replays unresolved ledger rows after success — #1311-correct after a generation death). Required for the sweep ("backstop") to actually fire when opencode is slow to answer post-restart.
8. **The E2E row's "agent's next turn references the interruption" clause is proxied, not asserted literally (r2 decision).** An LLM-prose assertion is non-deterministic by construction, and for the kill-9 case it is unsatisfiable by the disclosed read-time repair design (opencode's own store/prompt context is never written — writing it would be a worse architecture than the read-seam repair). The deterministic proxies stand in: history shows the tool terminal (no phantom spinner) with the harness-restart reason, the restart/credential-apply observed, and the sweep metric visible. Recorded here per Rule 11 rather than silently dropped.

## Assumptions (stated + validated — Rule 7)

| # | Assumption | Validation |
|---|---|---|
| A1 | opencode part-update SSE events carry `properties.sessionID` | `pkg/agent/opencode/wire/wire.go` parse fixtures + `TestTrackerActivity_PartUpdateJSONDecodes` pin; wire_test.go golden `message.part.updated` carries it |
| A2 | The Act interrupt route (`/session/:id/abort`) aborts a turn such that the harness writes terminal part state | `opencodeActor` comment (regression-pinned V1 abort, the only route ≥1.18.10); integration test proves the call lands; harness-side terminal-state write is the harness's own contract |
| A3 | Every opencode restart emits `onChildStarted` (generation boundary) → reseed(GenChange) | `managed_process.go` supervisor loop calls it on every child start; design 0055 "Placement" section (agentd is the parent — authoritative generation signal) |
| A4 | A turn's part stream reaches agentd while busy (no long silent gaps < 30s in a healthy turn) | Owner direction recorded in the issue (authoritative); false stalls degrade to a graceful interrupt, not a kill |
| A5 | The projection swap at reseed drops in-flight parts (pre-existing), so the sweep must re-fold them | `Reseed` replaces `a.sessions` from seeds; `SessionSeed` comment: in-flight parts are never seeded |
| A6 | `LeaseConvergenceBound` is the sanctioned stall bound pending the #1312 budget-table freeze | Issue text: "stall detection (`LeaseConvergenceBound`) … in the one coherence-pinned table"; issue notes the table decision is pending with the owner |

## Blockers

None. (The #1312 budget-table freeze is pending with the owner; `LeaseConvergenceBound` is used as the issue itself specifies, with the derivation pinned by test.)

---

## Tests Run

- `go test -race ./cmd/workspace-agentd/... -count=1` — pass (incl. faultmatrix + sessionstate soak).
- `go test ./controller/internal/workspace/ ./pkg/agentd/ ./pkg/agent/opencode/... ./pkg/session/... ./local/ -count=1` — pass.
- `make test` (full `go test -v ./...`) — 0 failures (re-run after review round 1: 0 failures).
- `make lint` (golangci-lint full repo) — 0 issues.
- `go build ./...` — pass; `gofmt -l` clean.

---

## Next Steps

- Run `local/issue-1342-graceful-restart-e2e.sh` on the pool/nightly cluster (this dev sandbox has no kind cluster; the script + workflow row + structural pins land with this PR).
- When the #1312 budget table freezes, re-point `restartStallBound` at the frozen table entry (single-line change; pinned by `TestRestart1342_ZeroStallBoundFallsBackToDefault`).
- Consider an early-exit from the grace window when all interrupted sessions observe idle (optimization only; the fixed window is correct).

---

## Files Modified

- `cmd/workspace-agentd/secrets.go` — decision rewrite, force path, stall bound, interrupter/deps plumbing
- `cmd/workspace-agentd/session_tracker.go` — `lastEventAt`, `noteActivity`, `busyPartitions`, prune, processEvent wiring
- `cmd/workspace-agentd/pending_apply.go` (new) — pending-apply tracker (refcounted post-r1)
- `cmd/workspace-agentd/sessionstate_wiring.go` — `newSessionInterrupter`
- `cmd/workspace-agentd/main.go` — deps wiring, relayKillFunc arity, retrying generation-change reseed
- `cmd/workspace-agentd/server.go` — serverDeps fields, applyDeps wiring, healthz call
- `cmd/workspace-agentd/sidecar_mode.go` — sidecar deps wiring
- `cmd/workspace-agentd/healthz.go` — pendingApply snapshot param
- `cmd/workspace-agentd/sessionstate_metrics.go` — orphan-parts counter bridge
- `cmd/workspace-agentd/sessionstate/authority.go` — orphan sweep + capture + counter (publish-gated post-r1)
- `pkg/agentd/types.go` — `PendingApplyHealth`, `HealthzResponse.PendingApply`
- `pkg/agent/opencode/wire/wire.go` — `SessionIDFromProps`
- `pkg/agent/opencode/adapter.go` — `repairOrphanedRunningTools` (V1+V2 store paths, r1)
- `pkg/session/session.go` — `ToolAbortReasonHarnessRestart` (r1)
- `pkg/apis/llmsafespaces/v1/workspace_types.go` — `CredentialsApplyPending` condition + reason
- `controller/internal/workspace/health.go` — condition mirror + clears
- `local/issue-1342-graceful-restart-e2e.sh` (new, r1) + `e2e-nightly.yml` row (r1)
- Tests (new): `session_tracker_activity_test.go`, `session_aware_restart_1342_test.go`, `pending_apply_test.go`, `session_interrupter_test.go`, `restart_interrupt_integration_test.go`, `healthz_pending_apply_test.go`, `sessionstate/authority_orphan_sweep_test.go`, `sessionstate/authority_orphan_sweep_internal_test.go`, `controller/internal/workspace/health_pending_apply_test.go`, `pkg/agent/opencode/transcript_repair_test.go` (r1), `cmd/workspace-agentd/orphan_reason_pin_test.go` (r1), `local/issue_1342_e2e_script_test.go` (r1)
- Tests (updated): `session_aware_restart_test.go`, `healthz_test.go`, `supervisor_status_test.go`, `xdg_config_layer_test.go`, `opencode_overlay_test.go`, `pkg/agentd/types_test.go`
