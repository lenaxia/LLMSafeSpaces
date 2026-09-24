# Worklog: SR-6 nightly failure triaged — Admit's budget-before-cap order broke §6.6's 429 boundary

**Date:** 2026-09-24
**Session:** Triage and fix of the nightly upload-staging-stress SR-6 failure (both 09-24 nightlies + the 09-23 nightly); verdict: product bug in Admit's check order, fixed with a TDD regression pin
**Status:** Complete

---

## Objective

Determine which side is wrong in the SR-6 nightly failure — the row's expectation or the product's admission behavior — and fix that side per design 0060's contract. Failure shape (runs 36026824047, 36011237476, 35872827066, identical): `FAIL: SR-6: 5-concurrent storm lacks a literal 429 or incomplete (delivered=4 refused=1 other=0 total=5, has429=0)`.

---

## Work Completed

### The triage (verdict: PRODUCT BUG — the row is §6.6-faithful, no retune)

- The one refusal was a **507**, not a 429 (`storm_report`'s refused bucket lumps 507|429|504; has429=0).
- **Deterministic arithmetic, not environment**: `Admit` checked the 48MiB budget BEFORE the count cap — at the 5th concurrent 10MiB admission, reserved(40MiB)+10MiB = 50MiB > 48MiB → `staging_full` (507) fired before the `len ≥ maxConcurrent` (429) check could run. At the shipped budget/cap/sizes, §6.6's characterized boundary was structurally unreachable.
- **The design is unambiguous**: §6.6 — "the count cap's default IS 4 — higher concurrency is unreachable by design … the matrix characterizes the CAP BOUNDARY instead: the 5th concurrent upload's 429" (size matrix includes 10MiB). §4.2 — the cap rejects "with 429 (`staging_busy`) — clean, retryable, and distinct from budget exhaustion". §4.6 keeps 507/429 as distinct client recoveries. At the double-violation point (5×10MiB trips both clauses), §6.6 fixes the class: 429. Budget-first told the client to wait for tmpfs when the binding constraint was concurrency.
- **The pre-#1545 passes were misattributed** (run 35752548377: "literal 429 present; delivered=3 refused=2"): that 429 was the applyMu TryLock busy — #1539's defect, removed by #1545 — not the count cap. The row was green for the wrong reason: satisfied by the serialization defect itself. First post-#1545 nightly (35872827066 @ 5b97bb11 ⊇ c8fe2c64) failed; all since identical.

### The fix (TDD)

- **Red first**: `TestStagingAdmission_CountCapPreemptsBudgetAtTheBoundary` — the shipped cap/budget arithmetic (cap=4, budget=48MiB, 5×10MiB, clause B non-binding by construction): 4 admissions pass, the 5th must be `staging_busy`. Watched fail: `got "staging_full"` — the nightly defect in miniature.
- **Fix**: the check-order swap in `Admit` (cap → budget → clause B), §6.6/§4.2/§4.6 contract cited in-code and in the file-header admission contract. Clause A still bounds every ADMITTED reservation — reordering changes only the rejection class of already-doomed admissions, never which admissions succeed (`TestStagingAdmission_ConcurrentNeverExceedsBudget` stays green).
- **Handler-level pin** (r1 review's recommended addition): `TestStagedUpload_CountCapBusy429` — 4 held reservations through the real `uploadFilesHandler` → 429/`staging_busy`, no apply runs, and the slot reopens on Release (the retryable contract). This bug class surfaced in nightly CI, not PR CI; the pin moves it to PR time.
- **Harness**: the SR-6B row's expectation is §6.6 verbatim — unchanged; its comment now carries the verified history so the next reader doesn't re-triage the misattribution.

---

## Key Decisions

- **Cap-first, not budget-tuning.** Raising the budget so 5×10MiB fits would also unmask the 429, but it would change the shipped default the SR-1 gauge pins assert against (≤48MiB) and quietly alter clause (A)'s semantics for every other size. The check order is the actual contract violation; one swap fixes it.
- **The unit pin uses the shipped cap/budget arithmetic** (48MiB/4/10MiB) — it mirrors the nightly row, not just an abstract ordering property.
- **No row retune** — the lane's "row stale" branch required §6.6 to disagree with the row; it agrees verbatim.

---

## Blockers

None.

---

## Tests Run

- `go test -count=1 -run 'TestStagingAdmission_CountCapPreemptsBudgetAtTheBoundary' ./cmd/workspace-agentd/` — FAIL pre-fix (`got "staging_full"`), PASS post-fix (watched red first).
- `go test -count=1 -run 'TestStagedUpload_|TestStagingAdmission' -v ./cmd/workspace-agentd/` — 20 PASS post-fix (patterns verified non-vacuous via `-v` RUN/PASS count); includes the new `TestStagedUpload_CountCapBusy429` and the budget-only (`ClauseABinds`) / cap-only (`ConcurrencyCap`) orderings in both directions, plus `TestStagingAdmission_ConcurrentNeverExceedsBudget`.
- `bash -n local/us-1500-upload-stress-e2e.sh` — syntax ok (comment-only harness edit).

---

## Next Steps

- The next nightly's SR-6B row should pass at head (5th concurrent admission → cap first → literal 429 → `total=5 && has429=1`). If it fails again, re-triage from the run log — the misattribution history is in the row comment.
- The SR-6 latency SKIP-DOWN (p95@4 788ms > 2×286ms — the #1539 known-issue path) is separate and stays as designed: manual tightening after the nightly characterizes the I/O-contention residual (per #1545's PR record).
- Parked elsewhere, unaffected by this lane: #1560 (chart-pins loud skip, implementation parked in this worktree's `fix/chart-pins-loud-skip` branch); the worklog self-numbering hook fix.

---

## Files Modified

- `cmd/workspace-agentd/upload_staging.go` — Admit's cap-before-budget order; file-header precedence note
- `cmd/workspace-agentd/upload_staging_test.go` — `TestStagingAdmission_CountCapPreemptsBudgetAtTheBoundary`, `TestStagedUpload_CountCapBusy429`
- `local/us-1500-upload-stress-e2e.sh` — SR-6B row comment: the verified history
- `worklogs/NNNN_2026-09-24_sr6-admit-count-cap-precedence.md` — this worklog
