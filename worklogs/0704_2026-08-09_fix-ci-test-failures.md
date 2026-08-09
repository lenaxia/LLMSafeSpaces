# Worklog: Fix CI Test Failures (Frontend + Integration)

**Date:** 2026-08-09
**Session:** Fix two pre-existing CI test failures on main.
**Status:** Complete

---

## Objective

Two CI checks were failing on `main`:
1. **Frontend (unit + typecheck + e2e)** — 3 tests in `TriggersPage.test.tsx` failing with `Found multiple elements with the text: Edit`
2. **pkg/secrets integration (Postgres + Redis)** — 8 tests in `store_integration_test.go` failing with CHECK constraint violations and UUID parse errors

---

## Root Causes (Proven)

### Frontend failure

PR #694 (trigger editor parity) introduced a second "Edit" button in the `TriggerEditor` component — the "Target Workspace" section now has an Edit button alongside the pre-existing "Schedule" section's Edit button. Three tests used `screen.getByText("Edit")` which is ambiguous when multiple matching elements exist.

Secondary issue: the test mock for `workflowApi.list` was `vi.fn()` (returns `undefined`), causing `Query data cannot be undefined` warnings for the `["workflows"]` query key.

### Integration test failures

**Bug A — CHECK constraint (`workflows_on_missing_workspace_chk`):** `store.go:208` used `COALESCE($11, 'abort')` for the `on_missing_workspace` column, but `$11` is `row.OnMissingWorkspace` (a Go `string`, zero value `""`). `COALESCE('', 'abort')` returns `''` because empty string is not NULL — so the INSERT sends `''` which violates the CHECK constraint `IN ('abort', 'create')`. The DB column default `'abort'` only applies when the column is omitted from INSERT entirely, not when an explicit empty string is sent.

The API handler (`workflows.go:262-263`) works around this by defaulting empty to `"abort"` in Go before calling the store — but the store is also called directly by tests and other callers, making this a store-layer robustness bug.

**Bug B — Non-UUID fixture IDs:** Integration tests used `strPtr("wf_1")` for `WorkflowID` (8 occurrences) and `'ws-1'` in raw SQL (1 occurrence). Both `triggers.workflow_id` and `triggers.workspace_id` columns are `uuid` type — non-UUID strings fail with SQLSTATE 22P02.

---

## Work Completed

### Frontend (`TriggersPage.test.tsx`)

- Added `within` import from `@testing-library/react`.
- Changed `workflowApi.list` mock from `vi.fn()` to `vi.fn().mockResolvedValue([])` to prevent undefined query data warnings.
- Scoped all 3 failing test assertions to the Target section using `within(targetSection).getByText("Edit")` instead of `screen.getByText("Edit")`. The target section is located via `screen.getByText("Target Workspace").closest("div.rounded-lg")`.

### Store (`pkg/workflows/store.go`)

- Changed `COALESCE($11, 'abort')` → `COALESCE(NULLIF($11, ''), 'abort')` for `on_missing_workspace`.
- Changed `COALESCE($12, 'draft')` → `COALESCE(NULLIF($12, ''), 'draft')` for `status` (same bug class — empty string would violate any status CHECK constraint if one exists).
- `NULLIF('', '')` returns NULL, so COALESCE then applies the default. This makes the store robust for all callers, not just the API handler.

### Integration test fixtures (`store_integration_test.go`)

- Replaced all 8 occurrences of `strPtr("wf_1")` with `strPtr(uuid.New().String())`.
- Replaced `'ws-1'` literal in raw SQL with `$4` parameter bound to `uuid.New().String()`.

---

## Key Decisions

1. **Fix the store, not just the tests.** The `COALESCE(NULLIF(...))` fix addresses the root cause: the store should treat empty-string-as-default the same as NULL-as-default. Fixing only the test fixtures would leave the store fragile for any future caller that doesn't pre-default the field.
2. **Scope tests to the Target section** rather than using `getAllByText("Edit")[1]` (positional indexing). Section scoping via `within` is more robust against DOM reordering and self-documenting.
3. **Single PR for both fixes** since they're both CI-blocking and small.

---

## Tests Run

- `npx tsc --noEmit` — clean (frontend)
- `go vet ./pkg/workflows/...` + `go vet -tags integration ./pkg/workflows/...` — clean
- `go test -timeout 120s ./api/internal/workflows/... ./api/internal/handlers/... ./pkg/workflows/...` — PASS
- Frontend vitest + integration tests require CI (Node 22 / TEST_DATABASE_URL)
