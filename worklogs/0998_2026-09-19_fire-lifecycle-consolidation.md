# Worklog: Refs #1454 — behavior-identical consolidation of the routine-fire lifecycle accounting/validation

**Date:** 2026-09-19
**Session:** #1454 lane on branch `fix/1454-fire-lifecycle-dedup` (worktree wt-1453; preceded by #1453/PR #1462 and #1467/PR #1468, both merged)
**Status:** Complete

---

## Objective

Consolidate the duplicated failure-accounting and target-validation logic across the routine-fire lifecycle (`fireRoutineTarget`, `executeRoutine` failure legs, `processPendingRoutineFire`) — the drift that produced #1440's silent zombie and #1411/#1441's accounting bugs — as a **behavior-identical pure refactor** (orchestrator scope; the issue's fuller "one entry point" vision is split into follow-ups, see Key Decisions).

---

## Work Completed

### Context validated first (Rule 7)

1. **The orphaned automation report on the issue** (branch `opencode/issue1454-…`, run 35376334525) described consolidation PLUS two behavior fixes (accounted drain-targetless leg; transient-vs-not-found fetch split). Its branch never reached origin (`git ls-remote` empty; the run died on git auth) — **no PR exists**; implemented fresh against main @ cea33f5e.
2. **The finding still holds post-#1462/#1464/#1466**: the drain targetless leg (`{"error":"trigger has no workspace_id"}`, no accounting, updates the existing row) and the cron guard (`trigger_has_no_target` payload, accounted, mints a row) are still divergent twins; the increment/disable block was copy-pasted 4× in routine-land.
3. **Coordination**: #1470 worker (wt-1452) sent a hunk map — no semantic overlap; adjacent-but-distinct helper anchors (mine after `indexPreservedSession`, theirs before it); my tail change never touches their ErrorCode-branch zone. Keep-both insertions in either merge order.

### TDD (characterization pins FIRST — green pre-refactor, green post)

New `api/internal/workflows/engine_fire_lifecycle_test.go`:

- `TestProcessPendingRoutineFire_TargetlessDrain_PreservedBehavior` — nil and empty workspace variants: fire updated-not-minted, payload byte-identical, ZERO accounting, never disables. **Documents the preserved gap deliberately.**
- `TestProcessPendingRoutineFire_FetchError_PreservedBehavior` — any-error → failed with exact `{"error":"trigger not found"}` payload, zero accounting.
- `TestExecuteRoutine_AccountingCallCounts` — 5 subtests: activation/script/agent-error/agent-error-code legs each increment exactly once and never reset; delivered resets exactly once and never increments. The mutation seam for the consolidation.
- `TestFireRoutineTarget_Targetless_ExactPayloadAndCounts` — cron payload byte-identical (key order pinned), exactly one minted failed fire, exactly one increment, no premature disable.

Mock additions (mockSchedulerStore, additive): `results` (last written payload per fire), `increments`/`resets` call counters (`triggerFail` stays the running total).

### Refactor (engine.go, behavior-identical)

- `accountTriggerFailure(ctx, trigger)` — the single accounting primitive (increment + threshold disable); replaces the 4 inline copies (fireRoutineTarget guard, activation leg, script leg, executeRoutine tail).
- `routineTargetWorkspace(trigger) (string, bool)` — single target-validation predicate for both entry routes; `executeRoutine`'s deref contract documented.
- `processPendingRoutineFire` — routed through the predicate with explicit preserved-behavior comments naming the pin tests and the follow-up class.
- No changes to: the agent-call block (#1470's zone), payloads, ordering of store operations, the reset leg, fireWorkflowTarget (out of scope per the orphaned report's own scoping — its 2 accounting blocks are a mechanical follow-up).

---

## Key Decisions

1. **Behavior-identical scope per orchestrator** — the two behavioral gaps the orphaned report found are REAL (validated against current main) but are behavior changes: (a) drain-targetless fires are unaccounted (silent-zombie class through the drain door), (b) ANY trigger-fetch error permanently fails a pending fire the webhook already 202'd. Both are pinned as characterization tests here (making the eventual fix-PR red-ready) and surfaced to the orchestrator as recommended follow-ups; fixing them inside this PR would violate the pure-refactor constraint.
2. **No flag-parameter mega-helper** — merging the two targetless guards (which differ in payload, persistence verb, and accounting) would require parameterized behavior — the exact drift-enabling shape this issue decries. Kept as two bodies sharing the predicate + accounting primitive.
3. **PR says "Refs #1454"** (not Fixes) — the issue's full consolidation lands across this PR + the behavior-change follow-ups it sets up.

---

## Blockers

None.

---

## Tests Run

- Pins first: `go test -run 'TargetlessDrain|FetchError|AccountingCallCounts|FireRoutineTarget_Targetless'` — **GREEN pre-refactor** (characterization baseline).
- `go test -timeout 600s -count=1 ./api/internal/workflows/` — ok (42.4s, full package, post-refactor).
- `go test -timeout 600s -race -count=1 ./api/internal/workflows/` — ok (43.6s).
- `go vet ./api/internal/workflows/` + `go build ./api/internal/workflows/` (disk-constrained scoped build) — ok.
- `gofmt`/`goimports` clean; `golangci-lint --new-from-rev=cea33f5e` — 0 issues.

---

## Next Steps

- Review loop to APPROVED; orchestrator merges.
- Recommended follow-ups (surfaced to orchestrator): (1) account the drain-targetless leg + unify payload via one failure site; (2) transient-vs-not-found fetch split mirroring fireWorkflowTarget; (3) mechanical fireWorkflowTarget accounting dedup through accountTriggerFailure. Pins for (1)+(2) already exist here.

---

## Files Modified

- `api/internal/workflows/engine.go` — `accountTriggerFailure` + `routineTargetWorkspace` helpers; 4 call-site replacements; predicate wiring + preservation comments
- `api/internal/workflows/engine_test.go` — mockSchedulerStore: `results`/`increments`/`resets` mirrors (additive)
- `api/internal/workflows/engine_fire_lifecycle_test.go` — new: 4 characterization pin funcs (9 subtests)
- `worklogs/0998_2026-09-19_fire-lifecycle-consolidation.md` — this worklog
