# NNNN — SR-6 nightly failure triaged: Admit's budget-before-cap order broke §6.6's 429 boundary (product bug, fixed)

Date: 2026-09-24
Branch: fix/upload-admit-count-cap-precedence
Lane: the SR-6 upload-staging-stress failure (both 09-24 nightlies + the 09-23 nightly)
Related: #1539 (the serialization defect), #1545 (the fix that exposed this), design 0060 §4.1.1/§4.6/§6.6

## The triage (verdict: PRODUCT BUG — the row's expectation is §6.6-faithful)

Failure shape (runs 36026824047, 36011237476, 35872827066 — identical):
`FAIL: SR-6: 5-concurrent storm lacks a literal 429 or incomplete (delivered=4 refused=1 other=0 total=5, has429=0)`

### Evidence chain

1. **The refused upload was a 507, not a 429.** The row demands a literal 429; `storm_report`'s refused bucket lumps 507|429|504; has429=0 with refused=1 → the one refusal was 507 or 504.
2. **The 507 is deterministic arithmetic, not environment.** `Admit` (upload_staging.go) checked the 48MiB budget BEFORE the count cap: at the 5th concurrent 10MiB admission, reserved(40MiB)+10MiB = 50MiB > 48MiB → `rejectStagingFull` (507) fires before the `len(reservations) >= maxConcurrent` (429) check can run. With the default budget/cap/sizes the row uses, the cap's 429 was UNREACHABLE.
3. **The design is unambiguous.** §6.6: "the count cap's default IS 4 — higher concurrency is unreachable by design, so the matrix characterizes the CAP BOUNDARY instead: the 5th concurrent upload's 429" — across the size matrix INCLUDING 10MiB. §4.1.1: the cap rejects "with 429 (staging_busy) — clean, retryable, and distinct from budget exhaustion". §4.6 keeps 507 and 429 as distinct client recoveries. At the double-violation point (5×10MiB trips BOTH clauses), §6.6 fixes the class: 429. Budget-first order told the client to wait for tmpfs when the binding constraint was concurrency.
4. **The pre-#1545 passes were misattributed.** Run 35752548377 (09-22, pre-#1545): `5th-concurrent 429 boundary observed (literal 429 present; delivered=3 refused=2)` — that literal 429 was the applyMu TryLock's busy (the #1539 defect #1545 removed), NOT the count cap. The row was green for the wrong reason: satisfied by the serialization defect itself. Every post-#1545 nightly fails (first: 35872827066 at 5b97bb11, which contains #1545's c8fe2c64).

### Verdict per the lane's dichotomy

Product bug, not a stale row. The row's expectation ("the 5th must 429 — the count cap") is §6.6 verbatim and needs NO retuning; Admit's check order violated it.

## The fix (TDD)

1. **Red first**: `TestStagingAdmission_CountCapPreemptsBudgetAtTheBoundary` — the default arithmetic (cap=4, budget=48MiB, 5×10MiB, clause B non-binding): 4 admissions pass, the 5th MUST be `staging_busy`. Watched it fail: `got "staging_full"` — the nightly's defect in miniature.
2. **Fix**: `Admit` checks the count cap BEFORE the budget clause (upload_staging.go), with the §6.6/§4.6 contract in the comment. Clause A still bounds every ADMITTED reservation (reordering changes only the rejection CLASS of already-doomed admissions, never which admissions succeed).
3. **Green**: the new pin + `TestStagingAdmission*` (5) + `TestStagedUpload_*` shaping + `TestStagingAdmission_ConcurrentNeverExceedsBudget` (the race invariant) — 28 tests, all green, `-run` patterns verified non-vacuous via `-v` count.
4. **Harness**: the row itself unchanged (its expectation is the design's); its comment now carries the verified history (the misattributed pre-#1545 429, the runs, the pin).

## Why the row should pass at the next nightly

At head, the 5th concurrent 10MiB admission → cap check first → 429 staging_busy → the row's `total=5 && has429=1` holds. The SR-6 latency SKIP-DOWN (788ms > 2×286ms guard — the #1539-known-issue path) is unrelated to this failure and stays as designed (manual tightening after characterization, per #1545's PR record).

## Key decisions

1. **Cap-first, not budget-tuning.** Raising the budget so 5×10MiB fits would ALSO unmask the 429, but it would change the shipped default SR-1 pins (≤48MiB budget gauges) and quietly alter clause (A)'s semantics for every other size. The check order is the actual contract violation; one order swap fixes it.
2. **The pin uses the shipped defaults' exact arithmetic** (48MiB/4/10MiB) — it mirrors the nightly row, not just an abstract ordering property.
3. **No row retune** — the dichotomy's "row stale" branch required §6.6 to disagree with the row; it agrees verbatim.

## Follow-ups

None. (The chart-pins lane #1560 remains parked separately; the worklog self-numbering hook fix remains queued.)
