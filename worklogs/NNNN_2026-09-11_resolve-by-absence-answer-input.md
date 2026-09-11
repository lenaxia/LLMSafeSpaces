# Worklog: Resolve-by-absence in AnswerInput + AnswerInputAction.reply (epic-71 / 1a, #1310 slice A)

**Date:** 2026-09-11
**Session:** Reclaim of stream 1a after the original claimant's session died at its push (credential failure; branch unreachable). Reimplemented to their published blueprint — **credit: the design, scope, and test list are the original claimant's** (reserved comment 5626017252); hand-off notice at #issuecomment-5630153513.
**Status:** Complete

---

## Objective

Kill the stale-prompt-on-click class (S6, the ses_f73747f8 incident): an opencode ask is a lease the harness drops with no lifecycle event; the user's reply click then 404s and the projection strands the prompt forever. Slice A: absence IS the resolution at the authority, plus the additive contract delta carrying the permission vocabulary.

## Work Completed

- **Resolve-by-absence** (`sessionstate/actions.go`): `act()` intercepts a harness `CodeNotFound` on `answer_question` → `resolveByAbsence` drops the projected entry via a seq-assigned `INPUT_RESOLVED` through the standard fold (browsers clear on the fanout) and returns SUCCESS. Answer-only — every other verb's NotFound stays typed. Conditional emit: an ask the projection never held returns SUCCESS without consuming a seq or minting a phantom session record. Lock order documented: caller holds the session single-flight, then `a.mu` — the reverse edge exists nowhere (verified against `observeEvent`/ledger/reconcile paths).
- **Contract delta** (`action.proto`): `AnswerInputAction.reply = 4` (`optional string`, vocabulary `once|always|reject`, disjoint from `option_ids`/`custom_text`). `buf generate` + sessiongen regenerated (Go + TS); `buf lint` clean; **`buf breaking` green against the frozen e58cecd9**; regeneration idempotent (freshness gate clean).
- **Validation**: the two answer forms are disjoint — reply XOR (option_ids|custom_text), at least one required (7-case table test).
- **Wiring actor** (`sessionstate_wiring.go`): a set `reply` routes directly to `/permission/{id}/reply` (`{"reply": ...}`) — no question-first probe, no lossy option encoding; legacy forms keep the question-first contract (regression-pinned).
- **Abitest fault knobs (legs 1–2, #1312)**: live pending-input registry (`SeedPendingInput`/`PendingInputs`, per-session, sorted); `DropAskSilently` (leg 1: drop with no event); one-shot `SetResolveNotFound` (leg 2: next answer 404s); the Act answer path consumes the registry (absent → NotFound — the resolve-by-absence trigger) and `GetSnapshot` serves the seeded registry.

## Tests (TDD — written first, red before implementation)

Authority: drops-projection-and-succeeds (entry gone + exactly one resolved event + SUCCESS), nothing-projected-no-seq (seq unchanged, no phantom record), answer-only (switch_model NotFound passes through typed), other-error-codes-leave-projection (Internal never resolves by absence). Validation: 7-case disjoint-forms table. Wiring: reply-direct routing + legacy-forms regression. Knobs: registry round-trip/scoping, drop-silently, one-shot 404, absent-from-registry 404, composability. **Wire-level incident replay**: authority → real connect client → abitest over HTTP — ask projected → silently dropped (leg 1) → click 404s (leg 2) → projection cleared, SUCCESS, resolved event fanned out.

## Tests Run

- `./cmd/workspace-agentd/sessionstate/` green with `-race` (57s).
- `./cmd/workspace-agentd/` green full package (227s).
- `./pkg/abi/...` (incl. freeze gate, roundtrip, surface completeness), `./pkg/session/...` green with `-race`.
- `./api/internal/handlers/` green with `-race` (133s).
- `make abi-lint`, `make abi-breaking` (FROZEN-armed, exit 0), codegen idempotent; golangci-lint 0 issues; gofmt/goimports clean.

## Key Decisions

1. **SUCCESS with an empty-ish result on absence** (not an error, not a silent no-op) — the caller learns the input resolved; browsers learn it from the resolved event.
2. **Conditional emit** — no seq, no phantom session record for unprojected asks (seq is durable; minting records for ghosts corrupts the stream's meaning).
3. **`reply` as a string field per #1302** (not an enum) — the vocabulary is opencode's; the contract stays additive and the validation enforces disjointness at the authority boundary.

## Next Steps

- 2a (#1310 slice B) now unblocked: lease reconcile (snapshot-serve + cadence pending-set diff), busy re-derivation, snapshot-flight consolidation — consumes the abitest registry added here for its diff tests.
- #1302 (4a) consumes `reply` at the edge when its gate (#828 batch 3) opens.
- L1/L2 wall-clock rows and kind e2e legs 1–2 ride the #1312 harness waves.

## Files Modified

- `pkg/abi/llmsafespaces/abi/v1/action.proto` (+ regenerated `pkg/abi/v1/action.pb.go`, `frontend/src/abi/llmsafespaces/abi/v1/action_pb.ts`, connect stubs, `pkg/session/contract_gen.go`)
- `cmd/workspace-agentd/sessionstate/actions.go`
- `cmd/workspace-agentd/sessionstate_wiring.go`
- `pkg/abi/abitest/server.go`
- `cmd/workspace-agentd/sessionstate/resolve_absence_test.go` (new)
- `cmd/workspace-agentd/sessionstate_actor_test.go` (reply-routing tests)
- `pkg/abi/abitest/server_test.go` (knob tests)
- `worklogs/NNNN_2026-09-11_resolve-by-absence-answer-input.md` (this file)
