# Worklog: Fix #1467 — update path enforces the last_result ⇒ captureMode=full cross-constraint

**Date:** 2026-09-18
**Session:** Fix issue #1467 on branch `fix/1467-update-path-memory-constraint` (worktree wt-1453, second lane after #1453/PR #1462)
**Status:** Complete

---

## Objective

The trigger CREATE handler enforces `memoryMode=last_result ⇒ captureMode=full` (`api/internal/handlers/triggers.go` create arm, the constraint that guarantees every delivered routine result row stores the agentd envelope the memory read path expects — the invariant #1453's envelope-shape reasoning depends on). The UPDATE handler did not: a PATCH could flip a trigger to `last_result` while leaving `captureMode=errors_only`. Make update enforce the same constraint with the same error shape, plus guard-matrix tests.

---

## Work Completed

### Assumptions stated and validated (Rule 7)

| # | Assumption | Validation |
|---|---|---|
| 1 | The constraint must hold on the POST-PATCH MERGED view (a patch may flip one side while leaving the other stored) | `UpdateTriggerRequest` uses `*string` nil-keeps semantics (`pkg/types/workflows.go:433-435`); store `UpdateTrigger` keeps absent fields; the V7 input-mapping block (`triggers.go` update arm) established the merged-view precedent |
| 2 | `existing` is fetchable whenever the guard needs it | Extended the existing fetch condition (`req.SourceConfig != nil \|\| req.Enabled != nil \|\| touchesTarget \|\| touchesMemoryCapture`); fetch error paths already 404/500 |
| 3 | No side effects from the wider fetch | The cron-recompute block only acts on `req.SourceConfig != nil` or the re-enable case (`req.Enabled != nil`); a memory/capture-only patch triggers neither |
| 4 | Legacy-invalid rows must stay editable | Untouched stored state is NOT re-scanned — the guard fires only when the patch touches memoryMode or captureMode; `TestTriggerUpdate_MemoryCaptureCrossConstraint/unrelated patch on legacy-invalid row` pins 200 |
| 5 | Pod-automation/MCP update routes inherit the guard | `validateTriggerInputMapping` doc: delegation "forwards through these handlers" (`triggers.go` update-arm comment) |

### TDD: guard-matrix tests first

Added to `api/internal/handlers/triggers_test.go` (failing-first: the four violating-flip cases returned 200 before the fix):

- `TestTriggerUpdate_MemoryCaptureCrossConstraint` — 12-case matrix: flip to last_result without full → 400 (exact create-path error string); flip with full → 200; narrow capture under last_result → 400; re-set full → 200; loosen memory to none → 200; capture full on plain trigger → 200; both-provided violating → 400; both-provided none+errors_only → 200; unrelated patch on valid row → 200; unrelated patch on legacy-invalid row → 200 (editability preserved); legacy empty-string row flip without full → 400 / with full → 200. Accepted patches assert persistence (memoryMode/captureMode land in the store).
- `TestTriggerUpdate_MemoryCaptureCrossConstraint_NotFound` — existence precedes the cross-constraint (merged view needs the stored row): violating patch on a missing trigger → 404.
- `seedRoutineTriggerRow` helper — seeds rows directly so every stored combination is testable, including states create refuses to produce.

### Fix

`api/internal/handlers/triggers.go`, update handler only:

1. `touchesMemoryCapture := req.MemoryMode != nil || req.CaptureMode != nil` added to the existing-row fetch condition.
2. After the fetch block: merged-view cross-check — `mergedMemoryMode`/`mergedCaptureMode` (patch value when present, stored otherwise); `last_result && != full` → 400 `{"error": "memoryMode 'last_result' requires captureMode 'full'"}` — byte-identical to the create-path error.

---

## Key Decisions

1. **Merged-view evaluation, single code path** — always fetch when either field is touched (even when both are patched): one evaluation point, consistent 404-before-constraint precedence matching the cron-revalidate ordering.
2. **Same error shape as create** (issue requirement) — exact string match pinned in tests.
3. **No re-scan of untouched state** — legacy-invalid rows remain editable; a repair patch can always reach the store. Repair+flip in one patch (`last_result`+`full`) is the documented path off an invalid row.
4. **Legacy empty-string rows**: `"" != full`, so flipping to `last_result` on a pre-#688 row requires `captureMode=full` in the same patch — correct under the invariant, pinned by test.

---

## Blockers

None.

---

## Tests Run

- `go test -count=1 -run 'TestTriggerUpdate_MemoryCaptureCrossConstraint' ./api/internal/handlers/` — 4 subtests FAILING before fix (violating flips returned 200); ok after.
- Mutation: revert only `triggers.go` → 4 failing subtests; restored.
- `go test -timeout 600s -count=1 ./api/internal/handlers/` — ok (82.6s, full package).
- `go test -race -count=1 -run 'TestTrigger' ./api/internal/handlers/` — ok.
- `go build ./...` — ok. `gofmt`/`goimports` clean; `golangci-lint run --new-from-rev=a7b2a83c` — 0 issues.

---

## Next Steps

- Adversarial PR review loop until APPROVED; orchestrator merges.

---

## Files Modified

- `api/internal/handlers/triggers.go` — update handler: fetch-condition extension + merged-view cross-constraint guard
- `api/internal/handlers/triggers_test.go` — guard-matrix table (12 cases) + not-found precedence test + `seedRoutineTriggerRow` helper
- `worklogs/NNNN_2026-09-18_update-path-memory-constraint.md` — this worklog
