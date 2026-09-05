# Worklog: busy-stuck sessions + wrong-model admission (#1292)

**Date:** 2026-09-05
**Session:** Two user reports on the live system: (a) sessions stay BUSY after the turn ends; (b) agents run muse-spark despite the session being configured for glm-5.3. Both root-caused from live captures on the production pod.
**Status:** In Progress

---

## Objective

Make turn-end register and the configured model actually run.

## Work Completed

### (a) Busy-stuck: the wire never says idle

240-second post-turn capture on the production pod (opencode reports version 1.18.15): ZERO `session.idle` / `session.status` events — the live stream is `session.next.*` only, and nothing clears the BUSY set by `session.next.prompted`. The terminal marker that DOES exist: `session.next.step.ended` with `finish:"stop"` (`finish:"tool-calls"` = mid-turn; the projection's own comment says "the turn ends when the status says so" — the status never says so on this build).

Fix: the terminal step.ended translates to SESSION_STATUS_IDLE (cost rides on evt.Message); mid-turn stays MESSAGE_END. step.failed already cleared busy at the projection (the 2026-08-15 orphaned-busy class). IDLE clearing the in-flight fold is by design — idle means reconcile-from-history.

### (b) Wrong model: the outbox path never applied the session model

Live proof: a steer body carrying `model: glm-5.3/thekaocloud` ran `muse-spark-1.3-contributor-free` — the V2 prompt endpoint strips per-prompt overrides (their own SetSessionModel comment documented this). The API adapter path applies the model to the SESSION before sending; the outbox path (Accept → terminus → agentd Deliver → admitter) lost that step, AND the provider was dropped at the Deliver boundary (GetId() only).

Fix: the admitter POSTs `/api/session/{id}/model` (object wire form) BEFORE the steer admission when a model is present; fails closed on rejection. The provider crosses the ledger as "provider/id" and re-splits in the admitter.

## Key Decisions

1. Terminal step.ended → IDLE (not a second synthetic event): one frame in, one event out; the final step's cost is preserved on evt.Message.
2. Model-set fail-closed: a bogus model must fail loudly (matching the adapter path), not silently run the session default.
3. Provider encoded "provider/id" in the ledger's string field (WAL-compat; no schema change).

## Blockers

None.

## Tests Run

- `go test ./pkg/agent/opencode/ ./cmd/workspace-agentd/...` — green.
- `-race` green.
- New: TestStepEnded_FinishStopMapsToIdle (terminal vs mid-turn), TestOpencodeAdmitter_SetsSessionModelBeforeSteer (ordering + wire form), TestOpencodeAdmitter_ModelSetFailureFailsClosed.
- Integration reworked to sample DURING the turn (idle clears the fold by design) and pins the terminal shape: IDLE + empty fold + busy-was-seen — the #1292a pin.

## Next Steps

- Ship 0.27.3 + production roll + user verification.

## Files Modified

- pkg/agent/opencode/translate_abi.go — terminal step.ended → IDLE.
- cmd/workspace-agentd/sessionstate_wiring.go — admitter sets the session model first (post helper + provider/id split).
- cmd/workspace-agentd/sessionstate/service.go — provider crosses the Deliver boundary.
- Tests + integration rework as above.
