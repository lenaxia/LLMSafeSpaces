# Worklog: Epic 71 / 2b (part 2) — the assertion harness engine + legs 2/4/5 rows

**Date:** 2026-09-11
**Session:** Stream 2b unit 2 (epic-71 / 2b, #1312 change item 2). Agent: opencode-kestrel [glm-5.3].
**Status:** In Progress

---

## Objective

The assertion harness: fault-leg rows driven through the REAL sessionstate authority with harness fakes, asserting S-invariants via violation counters and L-bounds via convergence samples — the in-memory shape the soak row reuses. Rows for the legs whose targets are stable on main (2 — S6/L2, 5 — S7/L4, 4 — S7/S8/L5); S5/lease rows wait for a landed 2a (#1329 was closed unmerged after r4; the stream's owner is reworking).

---

## Work Completed

### `cmd/workspace-agentd/faultmatrix` (new package, test-scaffolding-only)
- **Engine:** `Violations` (S/L counters; a row ends empty or fails — the naming stays at the call site where semantics live), `ConvergenceLog` (per-bound samples with `Max`/`Within` — the soak histograms' in-memory ancestor), `WaitConverges` (bound-bounded polling; the false return is the row's cue to add the L-violation).
- **Harness fakes:** `EvidenceStore` (the harness-store stand-in backing BOTH the `StoreReader` seam AND the row's fault levers — truth and evidence read the same transcript, as in production), `AnswerActor` (live asks answer; absent asks 404 — the leg-2 trigger resolve-by-absence converts), `InstantAdmitter` (admission writes evidence; the promotion event's absence is the stranding lever), `NoopParser`.
- **Rows (each ends with the two must-hold gates — violations empty, convergence within bound):**
  - `TestRow_Leg2_StaleClickResolves_S6_L2`: harness truth drops the ask silently; the click answers; the authority converts the harness 404 to SUCCESS (S6) and the projection clears; click→cleared recorded as L2 samples.
  - `TestRow_Leg5_StatusEventLost_S7_L4`: the store says IDLE (turn ended), the SESSION_STATUS event never arrived, the projection is BUSY; one reconcile pass re-derives idle (`BusyCleared ≥ 1` — S7) and the loss→idle span records the L4 sample (real 30s bound).
  - `TestRow_Leg4_CrashRestart_S7_S8_L5`: delivery admits (evidence written), promotion event never arrives → stranded ADMITTED; the boot reseed's embedded sweep promotes it from store evidence (S7) within L5; a SECOND reseed keeps S7 holding (S8 — reconstitutability).

### Row-writing notes (validated against source)
- Leg 5's evidence truth must be IDLE — the fault is the LOST EVENT, not a busy store (sweepAgainstEvidence's `turnEnded` derivation).
- Leg 4 must await the async admission ladder reaching ADMITTED before the "restart" — else the row sweeps a LEDGERED row inside its admission deadline (correctly stays).
- The authority requires Capabilities declaring ANSWER_QUESTION for the Act path (actionDeclared).

---

## Key Decisions

1. **The harness asserts through the authority's public surface** (Act/GetSnapshot/Reseed/Reconcile/Metrics) — never its internals; the same rows scale to kind and the soak unchanged.
2. **Rows ship only for stable-on-main legs** — S5/lease rows are 2a-gated by design (my reserved comment's sequencing); no speculative rows that fail on main.
3. **Violation naming at the call site** — the engine counts; the row says WHICH invariant breached and why. The soak aggregates the same counters.

---

## Blockers

None for this unit. S5/L3 rows + the full matrix completion wait on a landed 2a.

---

## Tests Run

- `go test -race ./cmd/workspace-agentd/faultmatrix/` — ok (3 rows)
- `go build ./cmd/...`, `golangci-lint run ./cmd/workspace-agentd/faultmatrix/...` — clean

---

## Next Steps

1. PR this unit; review-iterate.
2. Post-2a: the S5/L3 row (silent drop → projection converges to harness truth within the lease window) + leg-3 composition.
3. Leg 6/9 rows (compose API `e2eFaultInjection` + client-driven storms against 2a's serve-refresh), then the soak driver (N×M×λ).

---

## Files Modified

- cmd/workspace-agentd/faultmatrix/faultmatrix.go (new)
- cmd/workspace-agentd/faultmatrix/faultmatrix_test.go (new)

---

## Review r1 remediation (2026-09-11, PR #1337)

- **Engine contract pinned directly** (`faultmatrix_engine_test.go`, 8 tests): WaitConverges immediate-hold / breach (elapsed carries the full bound — the L-sample's evidence) / ctx-cancel; Violations copy-semantics + Empty; ConvergenceLog Max/Within; **Within now FAILS CLOSED on unrecorded bounds** (a row that never measured has not converged within anything — the vacuous-pass hole closed in code, pinned in test); AnswerActor live-ask success + non-answer CodeUnimplemented; InstantAdmitter distinct ids + Out=nil no-panic.
- **Leg-4 resolution arm pinned both ways:** the evidence-present row now asserts `LedgerDepths["promoted"] ≥ 1 || ReconcilePromoted ≥ 1` (the promoted-from-evidence arm, not the turn-ended fallback); a new evidence-absent variant (`InstantAdmitter{Out:nil}` — the harness-OOM shape) asserts `ReconcileTurnEnded ≥ 1` and `ReconcilePromoted == 0`.
- Also fixed en route: the CI-exposed leg-4 race — convergence is the cadence contract (tick Reconcile inside L5), not first-pass instant promotion (the sweep's TryLock skip of a live admission ladder is correct behavior, now relied upon rather than raced).

Counts at this revision: 13 test functions across the package's two test files (4 rows incl. the leg-4 evidence-absent companion + 9 engine/fake contract tests); all `-race` green; lint 0.

## Tests Run (r1)

- `go test -race ./cmd/workspace-agentd/faultmatrix/` — ok
- `golangci-lint run ./cmd/workspace-agentd/faultmatrix/...` — 0 issues

---

## Review round 2 remediation (2026-09-11, PR #1337; the posted review's commit header was stale but its minor findings referenced the remediated code — all three fixed)

1. `WaitConverges` post-deadline successes now return `(elapsed, false)` — a success observed past the deadline is a breach; the two row gates can no longer disagree.
2. The hand-rolled `itoa` replaced with `strconv.Itoa`.
3. Both leg-4 convergence predicates tightened: a nil `LedgerDepths` map is NOT converged (the soak-reuse footgun).

Counts at this revision: 13 test functions (4 rows + 9 engine/fake contract tests).

## Tests Run (round 2)

- `go test -race ./cmd/workspace-agentd/faultmatrix/` — ok
- `golangci-lint run ./cmd/workspace-agentd/faultmatrix/...` — 0 issues
