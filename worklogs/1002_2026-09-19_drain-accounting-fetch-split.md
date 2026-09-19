# Worklog: Fix #1473 — close the routine-drain lifecycle gaps (targetless accounting, transient-fetch split, fireWorkflowTarget dedup)

**Date:** 2026-09-19
**Session:** #1473 lane on branch `fix/drain-accounting-fetch-split` (worktree wt-1453; follows #1453/PR #1462, #1467/PR #1468, #1454/PR #1472 — all merged). Issue filed by this session per orchestrator routing.
**Status:** Complete

---

## Objective

Land the three follow-ups #1454 deliberately preserved and pinned: (1) account + auto-disable drain-targetless fires with the unified `trigger_has_no_target` payload (the drain twin of #1440's cron guard); (2) split the drain's trigger-fetch error handling (transient → leave pending for re-drive; deleted → best-effort loud failure); (3) mechanical: `fireWorkflowTarget`'s two remaining inline accounting blocks → `accountTriggerFailure`.

---

## Work Completed

### Assumptions stated and validated (Rule 7)

| # | Assumption | Validation |
|---|---|---|
| 1 | Gap 2's not-found arm can mirror fireWorkflowTarget INCLUDING accounting | **DISPROVED — recorded as a finding on #1473.** `trigger_fires.trigger_id` is `ON DELETE CASCADE` (migration 000016:349): a deleted trigger's fires vanish with it, so `ErrNotFound` on a pending fire is only the mid-tick ListPendingRoutineFires→fetch race; and `IncrementTriggerFailures` on a missing row returns no row (store.go:871-878) — no counter exists to hold. Corrected design: not-found arm fails the fire best-effort, NO accounting |
| 2 | The transient arm is the production-relevant fix | Webhook receiver 202s before the tick drains; a pool blip previously permanently failed an acknowledged fire — the exact class #1412 fixed for fireWorkflowTarget (engine.go:606-614) |
| 3 | The real store maps misses to `wf.ErrNotFound` | store.go:396-398; the engine MOCK did not — parity fixed (miss → `wf.ErrNotFound`, plus `getTriggerByIDErr` transient-injection field) |
| 4 | Unifying the drain payload to the cron guard's is safe | Both routes' consumers are the fire-audit surface (`GET /triggers/:id/fires`) + the nightly e2e (R4/R4d pin `trigger_has_no_target` on the CRON route only — drain pins were none); grep confirms no reference to the old `has no workspace_id` string anywhere |

### TDD — pins flipped RED first

`engine_fire_lifecycle_test.go` rewritten from #1454's characterization pins to the fixed expectations; RED confirmed pre-implementation (8 failing assertion groups across 6 subtests):

- `TestProcessPendingRoutineFire_TargetlessDrain_AccountsAndDisables` — nil/empty workspace + at-threshold variants: failed, byte-exact `trigger_has_no_target` payload, updated-not-minted, exactly one increment, threshold → disabled.
- `TestProcessPendingRoutineFire_TransientFetchError_LeavesPending` — direct + tick-drain drivers: NOTHING written (no status, no result), zero accounting, never disabled.
- `TestProcessPendingRoutineFire_TriggerDeleted_FailsLoudUnaccounted` — `ErrNotFound`: failed + byte-exact `{"error":"trigger not found"}` + zero accounting (guard for the split's not-found arm; coincides with pre-fix behavior for that arm).
- `TestScheduler_PendingDrainTick_TargetlessTrigger_Accounts` — tick-level wiring: pending webhook fire + workspaceless trigger drains through `tick` → failed + accounted + disabled at threshold.
- Retained from #1454: `TestExecuteRoutine_AccountingCallCounts` (5 subtests) and `TestFireRoutineTarget_Targetless_ExactPayloadAndCounts`.

### Implementation (engine.go)

- `processPendingRoutineFire`: transient (`!goerrors.Is(err, wf.ErrNotFound)`) → log + return (fire stays pending, next tick re-drives); not-found → log + best-effort failed write with cause payload, no accounting; targetless → unified payload + `accountTriggerFailure` (persistence stays update-vs-create per route).
- `fireWorkflowTarget`: both inline accounting blocks → `accountTriggerFailure` (gap 3, mechanical).
- No other behavior touched; #1470/#1471's zone (agent-call block, their helper anchor) untouched.

---

## Key Decisions

1. **Not-found arm takes no accounting** — schema-excluded (assumption 1 finding); documented in code and on #1473.
2. **Payload unification** — one cause name (`trigger_has_no_target` + hint) across both doors; the drain's old `trigger has no workspace_id` string is dead.
3. **Transient semantics mirror #1412 exactly** — platform's problem ≠ fire's problem; pending re-drive, never counted.

---

## Blockers

None.

---

## Review Rounds 2–3 (adversarial reviewer, PR #1474)

- **R1 CHANGES_REQUESTED**: drain-route e2e demanded via a workspace-delete recipe + a superseded-test nit. I pushed back with three source-validated impossibility proofs (soft workspace delete; hard delete has no production caller; receiver mint guard) — nit taken (test deleted, 6b2829c1).
- **R2 CHANGES_REQUESTED — my pushback partially REFUTED (correctly)**: the reviewer accepted all three proofs but found the route I missed: **trigger patch retarget** — `PUT {"workflowId":X,"workspaceId":""}` over a pending webhook routine fire (pair-check only rejects both-non-empty; #1442 guard satisfied by the new workflow target; store `NULLIF($8,'')` clears the column; the drain checks `WorkspaceID` alone). Every link independently verified (triggers.go:391/427-429/515-518, store.go:431, engine.go:948-953). Lesson recorded: "unreachable via API" claims need enumeration of ALL setters of the column, not just the delete paths.
- **Addressed**: R10 e2e row added to `local/issue-1410-1412-automation-e2e.sh` (create schema-less workflow → webhook routine trigger bound to dummy workspace, autoDisableAfter=1 → rotate-secret → signed HMAC delivery → 202 → immediate retarget patch → wait one tick → assert failed fire with `trigger_has_no_target` + consecutiveFailures ≥ 1 + auto-disabled; one retry guard against the tick race). Pin needles added; #1473 threat-model comment corrected on the issue thread (their non-blocking item); branch merged forward to origin/main (base was behind after #1470/#1471 landed — also noted by the reviewer).

## Tests Run (rounds 2–3 additions)

- `bash -n` the e2e script — ok; `go test -run TestIssue1410 ./local/` — ok (R10 needles).
- Targeted engine pins re-run green post-merge-from-main (memory directive: no full sweeps locally; CI carries the full board).

## Review Round 3 (bash harness defects — all four confirmed by my own simulation)

R3 blocking finding: the R10 harness could not execute its contract — (1) `verdict=$(fn)` takes the function's exit status → `return 1/2` aborted the whole script under `set -e`; (2) `ok` writes stdout, captured into the verdict variable → pass line swallowed; (3) polluted verdict forced arithmetic on a message → attempt "b" always double-executed after success; (4) the retry/verdict branches were unreachable in all worlds and a twice-lost race silently passed. **Fix**: verdict-by-stdout contract — the attempt function's ONLY stdout is the verdict code, it always `return 0`, all diagnostics via `warn`/`note_fail` (stderr), `ok` printed by the caller from the `case`. Verified by isolated bash simulation of all six verdict sequences (1 1 / 1 0 / 2 / 3 / 0 / 9) under `set -euo pipefail`: correct branch per sequence, no abort, retry works, unexpected codes guarded. (The first simulation run exposed a stub bug — subshell state mutation — fixed before trusting results; recorded as its own lesson in method.)

## Review Round 4 (the harness could never execute — two system findings, both fixed)

R4 blocking findings, both empirically reproduced by the reviewers and confirmed by me:
1. **`api()`'s status side-channel died in every caller's subshell** — `api_status` is a plain assignment, but every status-reading call captured stdout via `resp=$(api ...)`, so the variable never existed at top level; under `set -u` the script aborted at R1a. **No row of the harness had ever executed** (the current `api()` shape landed after the last nightly run).
2. **R10 verdict-3 bookkeeping died in the attempt subshell** — `note_fail`'s `failures` increment and the cleanup-array appends never reached the parent, so the row's core assertions could not gate the nightly (same silent-pass class as r3-D4).

Fixes:
- **`api()` no-subshell contract**: sets `api_status`+`api_body` globals AND prints the body (pipe callers keep working); every capture call site (24 across the 1410 script) mechanically converted to `api M P B; var="${api_body}"` (depth-counting transformer over nested `$(jq ...)`; 4 body-only pipe captures left as-is deliberately); curl transport failure guarded (`|| out=$'\n000'`).
- **R10 direct call**: `r10_attempt` runs in the current shell (always `return 0`, verdict via the `r10_verdict` global, no stdout) — `note_fail`/cleanup arrays propagate; re-verified by isolated simulation of all six verdict sequences (failures counter and array counts proven to reach the parent).
- **Sister scripts fixed in the same pass (Rule 5, reviewer-directed)**: `issue-1417-templating-e2e.sh` (12 sites) and `issue1452-routine-session-index-e2e.sh` (4 sites) shared the identical `api()` defect; same contract applied, pin tests green.
- **Execution smoke added** (reviewer's strongly-recommended missing case): `TestIssue1410E2EScript_ExecuteSmoke` runs the real script under curl/sleep/kubectl shims and asserts it traverses to the final verdict gate (`die "N row(s) failed"`, exit 1) with no unbound-variable/command-not-found abort signatures. This smoke is what would have caught r3-D1–D4 and r4's api() death before review did — and it caught TWO real bugs during its own construction (the shim's literal-`\n` -w mishandling, and immediately flagged the first transformation attempt's traversal failure). 9s runtime, skipped under `-short`.

## Tests Run (original verification, pre-directive)

- RED-first: 6 subtests failing pre-implementation (targetless ×3, transient ×2, tick-level ×1).
- Mutation: revert only `engine.go` → 8 failing assertion groups; restored.
- `go test -timeout 600s -count=1 ./api/internal/workflows/` — ok (42.8s full package).
- `go test -race -count=1` (same package) — ok (44.0s); `go vet` + scoped build ok (disk-constrained).
- `gofmt`/`goimports` clean; `golangci-lint --new-from-rev=9c6624ed` — 0 issues.

---

## Next Steps

- Review loop to APPROVED; orchestrator merges (may carry "Fixes #1473" — all three gaps land here).

---

## Files Modified

- `api/internal/workflows/engine.go` — processPendingRoutineFire fetch-split + targetless accounting/payload; fireWorkflowTarget ×2 → accountTriggerFailure
- `api/internal/workflows/engine_fire_lifecycle_test.go` — pins flipped to fixed expectations + tick-level test
- `api/internal/workflows/engine_test.go` — mock parity: `wf.ErrNotFound` on miss, `getTriggerByIDErr` injection
- `local/issue-1417-templating-e2e.sh`, `local/issue1452-routine-session-index-e2e.sh` — sister-script `api()` no-subshell fix (Rule 5, r4 review)
- `local/issue_1410_automation_e2e_script_test.go` — R10 needles + `TestIssue1410E2EScript_ExecuteSmoke` (harness-execution smoke)
- `worklogs/1002_2026-09-19_drain-accounting-fetch-split.md` — this worklog

## Review Round 5 (conversion-mechanics defects — three, all mine, all fixed)

R5 blocking findings, each a defect my r4 mechanical conversion introduced:
1. **Captured-helper stdout pollution**: `create_trigger` (1410) and `make_routine_trigger` (1452) call `api` directly — the body echoed to stdout landed in the helper's CAPTURED output alongside the returned id (`R1_ID` = `<body><id>` → URL globbing breaks every derived call). Fix: `>/dev/null` on the api call inside both helpers; `resp` rides `api_body`.
2. **Pipe-form misconversion (R4d)**: the transformer converted `r4d_result=$(api … | jq | head -1)` into a pipeline whose `api` member runs in a subshell — the follow-on `r4d_result="${api_body}"` read a STALE top-level value (the workflow-delete body). Fix: direct `api … >/dev/null` + `r4d_result=$(printf '%s' "${api_body}" | jq … | head -1 || true)`. Audited all three scripts for other pipe-form conversions: none remain.
3. **Secret leak**: six unredirected rotate-secret calls now printed the one-time `webhookSecret` into CI logs (1410 ×2, 1417 ×2, 1452 ×2). Fix: `>/dev/null` on all six (values ride `api_body`); 1417's run-poll loop quieted too. The ExecuteSmoke now bans `whsec_` in script output (the shim emits the secret ONLY on rotate paths, so the ban is meaningful, not a tautology); shim body scoped accordingly.

Verification: manual shim run of the full 1410 script — verdict gate reached (exit 1, 20 expected row failures under the generic shim), ZERO secret occurrences, no id corruption; bash -n all three scripts; Go pins + smoke green (13s).
