# Worklog: #1433 — inputSchema write gate requires object-rooted schemas

**Date:** 2026-09-17
**Session:** `workflow_create`/`workflow_update` accepted `inputSchema: {"type":"string"}` — the #1420 write-path check compiled the schema but never checked the root type, while run inputs are always JSON objects, so every subsequent run 400'd with "got string, want object". This session re-establishes the object-root gate lost in the #1420/#1421 collision and pins the explicit-JSON-null semantics (null ≠ 400; null = schema-less).
**Status:** Complete

---

## Objective

Extend `ValidateInputSchema` (pkg/workflows/input_schema.go): the compiled schema must be object-rooted (type `"object"`, a type list containing `"object"`, or no root type); string/number/array-rooted schemas are rejected 400 with the named error `ErrInputSchemaNotObject`. Explicit JSON null at write time must not 400 (schema-less, same as absent), and a stored literal-`null` row must behave schema-less at run time.

## Work Completed
- Reimplemented the lost `validateInputSchemaDeclarable` helper (own style): compile → root-type gate. Compile failures stay "invalid inputSchema"; non-object roots return wrapped `ErrInputSchemaNotObject`.
- `isSchemalessInputSchema`: absent (`len==0`) and the JSON literal `null` (whitespace-tolerant) both mean schema-less. Wired into `ValidateInputSchema` and `ValidateRunInput` — a stored jsonb `'null'` row no longer dies on compilation at run time.
- `NormalizeInputSchema` (exported): collapses explicit null to absent at the storage boundary. Both DTOs bind `"inputSchema": null` into a NON-pointer `json.RawMessage` as the raw bytes `"null"` (len 4, non-nil), so create stores nil (never writes a literal null row) and update's `WorkflowUpdate.InputSchema` becomes nil → keep existing, identical to an omitted field.
- Call sites in api/internal/handlers/workflows.go: create validates raw + stores normalized; update (schema-only PATCH included) validates non-nil raw + carries normalized into the update. The `WorkflowUpdate`/store CASE already treats nil as keep-existing — no store changes needed.
- Tests: handler-level create/update matrix (string-rooted/garbage/valid/null on BOTH paths, run-time null-row) + pkg-level table pins (sentinel via `errors.Is`, union `["object","null"]` accepted, `["null"]`-only rejected, type-less accepted, `NormalizeInputSchema` matrix).

## Key Decisions
- Union types containing "object" pass: an absent run input is validated as the JSON null document, so `{"type":["object","null"]}` is satisfiable; only roots that can NEVER match an object input are rejected.
- Update-path null = keep existing (same as absent), not clear: the update seam has no three-state clear mechanism for `input_schema` and the issue defines null as "same as absent"; clearing is a `{"inputSchema": {}}`-style explicit declaration away.
- Null rows are tolerated at run time rather than migrated: pre-#1433 rows can carry jsonb `'null'`; `ValidateRunInput` treats them as schema-less so no backfill is needed.

## Blockers
None.

## Tests Run
- RED pre-fix (verified against unfixed tree before implementing): create string-rooted → 201 (bug), create null → 400 "got null, want boolean or object", update string-rooted → 200, update null → 400, run against stored `null` row → 400.
- GREEN post-fix: `go test ./pkg/workflows/... ./api/internal/handlers/... ./pkg/types/... ./api/internal/server/...` all ok; `go build ./...` clean; gofmt/goimports clean; `golangci-lint run ./pkg/workflows/... ./api/internal/handlers/...` 0 issues.

## Next Steps
PR → review; the garbage/valid legs pin already-enforced behavior, the string-rooted/null legs are the #1433 regression.

## Files Modified
- pkg/workflows/input_schema.go (+tests in input_schema_test.go)
- api/internal/handlers/workflows.go (+tests in workflows_test.go; mock now applies `upd.InputSchema` to mirror the store's CASE)
