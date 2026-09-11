# Worklog: Epic 71 / 2b (part 4) — leg-6/7 replay, leg-9 storm cheapness, the soak driver

**Date:** 2026-09-11
**Session:** Stream 2b unit 4 (epic-71 / 2b, #1312 change items 1-2 closure for the in-process topology). Agent: opencode-kestrel [glm-5.3].
**Status:** In Progress

---

## Objective

Close the in-process matrix: the delivery-replay row (legs 6/7 → S2/S9), the serve-storm cheapness row (leg 9), and the soak driver with a CI self-test — the in-memory shape of #1312's "N workspaces × M sessions × fault rate λ" soak.

---

## Work Completed

- **`InstantAdmitter` fidelity fix (the load-bearing one):** it now models 0a's entry-level admission idempotency — the passed `messageID` IS the harness-store dedupe key (attempt-independent) and the transcript write is a keyed upsert: re-admission overwrites, never appends. The old fake invented fresh ids per admission, which would have masked exactly the #1315 sixteen-copies class the S2 row exists to catch.
- **`TestRow_Leg6_7_DeliveryReplay_S2_S9`:** deliver → admitted; the rollover replay of the same (entry, attempt) — S9 asserted as "the ledger did not grow" (idempotence is no-second-row, NOT frozen state; a duplicate ack lawfully carries the row's current state — the #1331 review's own lesson, initially mis-encoded here); the attempt+1 re-arm resolves both rows from the SAME keyed evidence with the transcript at exactly ONE user message (S2).
- **`TestRow_Leg9_ServeStorm_Cheapness`:** 50 concurrent GetSnapshot serves → every serve answers with the truth's pending set; the gather counter (new `PendingInputsCalls` observable on the fake) stays ≤3 — 2a's singleflight + TTL cache makes a storm cost O(gather windows), not O(serves).
- **The soak driver (`soak.go`):** `RunSoak(ctx, authority, store, SoakConfig{Sessions, FaultsPerTick, Tick, Duration, Rand})` — a deterministic fault stream (silent ask drop / lost ask event / silent turn end) over N sessions; after each fault the affected session's projection must converge to the CURRENT truth's pending-shape inside the lease window (ticking Reconcile — the cadence contract); breaches land in Violations, spans in ConvergenceLog. An unprojected session reads as zero pending (S5 holds vacuously — the diff surfaces the session when truth gives it asks).
- **`TestSoak_SelfTest_HighRateZeroViolations`:** 4 sessions, λ=0.9, 2s — zero violations, samples recorded, max span within the bound (+100ms epsilon for the timer-read drift observed under `-race`).

---

## Key Decisions

1. **Convergence against CURRENT truth, not a sampled `want`:** the fault stream keeps moving the target; chasing a stale snapshot breaches spuriously. The continuous-diff semantics (projection tracks truth) is the honest gate.
2. **S9 as ledger-growth, not state-equality** — see above; encoded with a depth-sum comparison.
3. The soak's CI self-test is minutes-scale and high-λ; the kind row reuses the same driver hours-scale (the ≥2h gate is a cluster/CI-infra decision — owner).

---

## Blockers

None. Remaining for full 2b closure: the hours-scale soak execution on the kind pool (driver is done; the run is infra).

---

## Tests Run

- `go test -race ./cmd/workspace-agentd/faultmatrix/` — ok (19 test functions: 6 rows + 10 engine/fake + 3 new)
- `go test -race -count=3 -run "TestSoak|TestRow_Leg6_7|TestRow_Leg9"` — stable
- `golangci-lint` — 0 issues

---

## Next Steps

1. PR; review-iterate.
2. The kind soak row: `RunSoak` at hours-scale on the pool (owner scheduling).

---

## Files Modified

- cmd/workspace-agentd/faultmatrix/faultmatrix.go (keyed-upsert admitter; PendingInputsCalls/TranscriptCount observables)
- cmd/workspace-agentd/faultmatrix/soak.go (new — the driver)
- cmd/workspace-agentd/faultmatrix/soak_test.go (new — the rows + self-test)
