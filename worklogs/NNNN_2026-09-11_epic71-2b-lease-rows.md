# Worklog: Epic 71 / 2b (part 3) — the 2a-gated S5/L3 + leg-3 lease rows

**Date:** 2026-09-11
**Session:** Stream 2b unit 3 (epic-71 / 2b, #1312 change item 2 completion for the in-process matrix). Agent: opencode-kestrel [glm-5.3].
**Status:** In Progress

---

## Objective

With 2a landed (PR #1335 — pending-input leases: diff, status re-derivation, serve refresh), ship the deferred S5/L3 and leg-3 rows: both directions of the pending-lease diff, asserted through the harness engine.

---

## Work Completed

- `EvidenceStore.PendingInputs` — the fake grew the StoreReader's new truth source (live ask registries keyed by session; STRICT semantics preserved: derived from the same session truth the reseed reads).
- **`TestRow_S5_Leg1_SilentDrop_ConvergesWithinL3`** — the projection holds an ask the harness silently dropped (truth no longer lists it, no event fired); the reconcile cadence's lease diff resolves the projected straggler; the snapshot converges to truth within the L3 bound (30s, tick-driven). S5: projection ⊆ truth modulo the lease window.
- **`TestRow_Leg3_LostAskEvent_AppearsWithinL3`** — the inverse fault: the harness ASKED but the INPUT_REQUEST event was lost (frame loss, harness→agentd hop); the diff's live−projected arm surfaces the ask in the projection within L3, with identity pinned (the appearing input IS the truth's).
- Both rows ride the existing engine (Violations + ConvergenceLog + WaitConverges) and the two must-hold gates.

---

## Key Decisions

1. Leg 3's violation names L1/L3 (not S5): a live ask the projection never surfaced is the prompt-path direction (the S5 containment direction is leg 1's row); naming at the call site per the engine's contract.
2. The rows tick `Reconcile` inside the convergence wait — the cadence contract (one ReconcileCadence tick converges inside LeaseConvergenceBound), never first-pass instant effects.

---

## Blockers

None. The in-process fault matrix (legs 1,2,3,4,5 + S5/S6/S7/S8 + L2-L5) is now complete for the agentd topology. Remaining for 2b closure: leg-6/9 rows (API-side composition) and the soak driver — per the reserved comment's sequencing.

---

## Tests Run

- `go test -race -count=3 -run "TestRow_S5|TestRow_Leg3" ./cmd/workspace-agentd/faultmatrix/` — ok (stable)
- `go test -race ./cmd/workspace-agentd/faultmatrix/` — ok (16 test functions: 6 rows + 10 engine/fake)
- `golangci-lint run ./cmd/workspace-agentd/faultmatrix/...` — 0 issues

---

## Next Steps

1. PR; review-iterate.
2. Leg-6/9 rows: compose the API `e2eFaultInjection` seam (#1182) with the outbox ladder, and the client-driven snapshot storm against 2a's serve-gather (leg 9's cheapness bound).
3. The soak driver (N×M×λ, ≥2h) — the 2b merge gate.

---

## Files Modified

- cmd/workspace-agentd/faultmatrix/faultmatrix.go (PendingInputs on the fake)
- cmd/workspace-agentd/faultmatrix/faultmatrix_test.go (two rows)
