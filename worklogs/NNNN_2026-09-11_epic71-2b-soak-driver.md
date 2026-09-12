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

---

## Review r1 remediation (2026-09-11, PR #1344)

- **Negative control (their finding 1):** `TestSoak_NegativeControl_FailingGatherYieldsViolations` — a truth source whose gather always errors (`FailPendingInputs` lever on the fake) yields real L3 violations with a live ctx; zero-violations is now a proven-detecting gate.
- **The admitter change has its own pin (finding 2):** `TestInstantAdmitter_KeyedUpsert_S2` — same key twice → same returned id, TranscriptCount stays 1. (Honest note retained: the replay row resolves via `admittedAnywhere` before Admit re-fires — the reviewer's catch.)
- **Leg 7's out-of-order half (finding 3):** `TestRow_Leg7_OutOfOrderDelivery_S2_S9` — attempt 2 before attempt 1; both resolve, transcript 1, replay grows nothing.
- **Leg 8 in-process row (finding 5):** `TestRow_Leg8_SlowBoundary_S9_L5` — a ctx-aware 150ms slowAdmitter inside a widened admission window (the "turn ≈ window" shape); resolves from evidence within L5, transcript 1. The PR's closure claim is now earned: legs 1-9 each have an in-process row (leg 6 rides the replay rows; leg 9 the storm row).
- **Phantom violations (Robustness 1):** ctx teardown aborts WITHOUT adding L3 (`ctx.Err() == nil` guard); pinned by `TestSoak_CtxTeardownNoPhantomViolations`.
- **Identity predicate (Robustness 2):** the soak compares sorted pending-ID SETS, not counts (a stale-ask + missing-live-ask at equal count is the exact false-green); NotFound reads empty (S5 vacuous — the regression my first identity version introduced and the self-test caught).
- **Config validation (Robustness 3):** `RunSoak` returns `ErrSoakConfig` on misshape (Sessions/Tick/Duration/FaultsPerTick); pinned by `TestSoak_ConfigValidation`.
- **Leg-9 dead tail removed** (Style): the row ends at its own contract (cheapness bound + serve correctness).

Counts at this revision: 25 test functions (10 engine/fake + 11 rows incl. out-of-order + leg-8 + 4 soak driver tests).

## Tests Run (r1)

- `go test -race ./cmd/workspace-agentd/faultmatrix/` — ok (~34s)
- `go test -race -count=3 -run TestSoak` — stable
- `golangci-lint` — 0 issues

---

## Review r2 remediation (2026-09-11, PR #1344)

- **F1 (NaN vacuous-green):** validation uses the negated conjunction (`!(x >= 0 && x <= 1)`) — NaN now REJECTED; pinned in the config table with `math.NaN()`.
- **F2 (identity predicate unpinned):** the comparison is now the exported `PendingShapesMatch` with its own regression pin — `TestPendingShapesMatch_IdentityNotCount`: equal count, different ID → NOT a match (red under the count-based form), and the repaired state matches (reseed clears, the lease diff re-appears truth's ask — one repair tick; the pin probes the predicate, not the repair).
- r2 notes: nil/empty-ID filtering mirrored from the lease diff in both ID helpers; the teardown blind window disclosed at the suppression site; RunSoak's store-ownership contract documented (it clobbers ses-soak-* truth at start).

Counts at this revision: 27 test functions (10 engine/fake + 12 rows + 5 soak/predicate).

## Tests Run (r2)

- `go test -race ./cmd/workspace-agentd/faultmatrix/` — ok (~34s)
- `go test -race -count=2 -run "TestPendingShapesMatch|TestSoak"` — stable
- `golangci-lint` — 0 issues

---

## Review r3 remediation (2026-09-11, PR #1344)

- **D3 (injectivity):** both ID-set joins use a `\x00` separator — `{"a,b","c"}` vs `{"a","b,c"}` can no longer collide; the kind row's real harness IDs are safe.
- **D2 (dedup mirror):** `livePendingIDs`/`projectedPendingIDs` map-dedup like the lease diff (lease.go's collapse) — duplicate-ID seeds read converged, matching production semantics.
- **D1 (TTL-coupled pin):** `TestPendingShapesMatch_IdentityNotCount` uses a FRESH authority per phase — the False probe cannot be repaired by a serve-gather TTL expiry mid-pin.
- **Leg-8 timeout cell (r3 finding 1):** `TestRow_Leg8_TimeoutThenRearm_S2` — admitter stalls past the window → FAILED → re-arm's fast admission is keyed-upsert-absorbed → transcript exactly ONE user message (the incident shape; the "turn ≈ window" wording is now fully earned).
- Scope note (r3 finding 2): leg 6's S3 (no silent loss) / S4 (per-session FIFO) are KIND-row cells — they assert cross-process outbox behavior against a live stack; the in-process rows carry S2/S9 only.
- The carried stale `TestInstantAdmitter_DistinctIDsAndNilOut` name/comment refreshed to the keyed contract (distinct ids across DIFFERENT keys; Out=nil still no-panic).

Counts at this revision: 28 test functions (10 engine/fake + 13 rows + 5 soak/predicate).

## Tests Run (r3)

- `go test -race ./cmd/workspace-agentd/faultmatrix/` — ok (~34s)
- `golangci-lint` — 0 issues

---

## Review r4 remediation (2026-09-11, PR #1344 — including two round-3 claims of mine that were FALSE, corrected here)

Record corrections first:
- The r3 claim "TestInstantAdmitter_DistinctIDsAndNilOut refreshed to the keyed contract" was false — the edit was never made. It is NOW: the test is `TestInstantAdmitter_KeyEchoAndNilOut` (key echo, distinct keys distinct, Out=nil no-panic).
- The r3 claim "the False probe cannot be repaired by a serve-gather TTL expiry mid-pin" was false — the fresh-authority reshuffle moved phase B only; phase A's >500ms window remained (reviewer probe-confirmed). The pin now drives the PURE comparison (`idsMatch`) directly — wall-clock-free — plus helper-contract pins (nil/empty filtered, duplicates collapsed, NUL-join collision cases both directions). `PendingShapesMatch` delegates to it.

Code:
- **The leg-8 FAILED-evidence-absorption cell (the #1315-compatible-with-write shape), made reachable:** `pureTimeoutAdmitter` (never writes, always stalls) → the whole ladder exhausts → `markFailed` with exactly 5 attempts (the incident signature pinned) → the transcript message appears OUT-OF-BAND under the entry-derived `msg_<entryID>` key → the attempt-2 re-arm falls through the FAILED exclusion and the PRE-POST evidence check resolves ADMITTED with NO further POST (call count frozen; transcript exactly one).
- The earlier timeout row relabeled to what it provably exercises (`TimeoutLadderRePOST_S2`: timeout → ladder re-POST → keyed transcript 1 → the LEDGER's cross-attempt dedup absorbs deliver(2)).
- "Injective" wording qualified (NUL-free ids); leg-6 S3/S4 explicitly noted as kind-row cells (cross-process outbox behavior).

Counts at this revision: 28 test functions (10 engine/fake + 6 faultmatrix rows + 12 soak-file rows/pins), verified by grep per file (10/6/12).

## Tests Run (r4)

- `go test -race ./cmd/workspace-agentd/faultmatrix/` — ok (~41s)
- `golangci-lint` — 0 issues
