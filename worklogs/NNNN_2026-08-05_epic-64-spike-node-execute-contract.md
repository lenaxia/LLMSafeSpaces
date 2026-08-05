# Worklog: Epic 64 Spike — Node Execution Contract

**Date:** 2026-08-05
**Session:** US-64.1 validation spike — probe opencode + agentd + expr-lang + script wrapper to de-risk workflow engine implementation
**Status:** Complete

---

## Objective

Validate assumptions A1–A9 from the Epic 64 design doc before any dependent production code merges. Produce `NODE-EXECUTE-CONTRACT.md` with captured evidence.

---

## Work Completed

### A6 — expr-lang condition type-checker (VALIDATED)
- Added `github.com/expr-lang/expr v1.17.8` dependency
- `pkg/workflows/exprlang/validate.go` — `SchemaToEnv()` converts JSON Schema → `reflect.StructOf` → typed env
- `pkg/workflows/exprlang/validate_test.go` — 7 tests: valid expressions, missing fields, wrong types, wrong methods, nested objects, syntax errors
- **Finding:** maps do NOT type-check in expr-lang; must use `reflect.StructOf`. Field names are CamelCase (snake_case → CamelCase).

### A3 — Script wrapper function-call contract (VALIDATED)
- `pkg/workflows/scriptwrap/wrapper.go` — Python + Node wrapper generators
- `pkg/workflows/scriptwrap/wrapper_test.go` — 6 tests: round-trips, exception handling, non-dict return, module import, context cancellation
- Handler defines `def handler(input) -> dict`; agentd generates a thin wrapper, execs via runtime, captures JSON stdout.

### A1 — opencode session/message API (VALIDATED)
- Probed live opencode 1.15.12 via `curl` against the running workspace pod
- `POST /session/:id/message` returns synchronous JSON: `{info:{metadata}, parts:[content]}`
- Text output extractable from `parts[]` where `type=="text"`
- Token/cost data in `info.tokens`
- **Correction:** endpoint is `/session/:id/message` (not `/sessions/:id/prompt` as the design doc assumed)

### A2 — Structured output (VALIDATED: negative)
- opencode 1.15.12 config schema has no `outputSchema`/`response_format`/`structured` field
- **Impact:** We MUST validate+retry on our side (design doc already plans this — confirmed necessary)

### A8 — Named agent validation (VALIDATED: differs from spec)
- opencode silently falls back to default agent for non-existent `agentID` (no error)
- `GET /agent` returns the list of configured agents for pre-validation
- **Solution:** agentd calls `GET /agent` before dispatch; rejects unknown names with `agent_not_found`

### A4 — agentd → opencode call path (VALIDATED)
- `OpenCodeClient` (`cmd/workspace-agentd/client.go`) already connects to opencode locally with basic auth
- No new auth surface needed; just add `PostMessage(ctx, sessionID, body)` method

### A9 — Secret resolution path (VALIDATED)
- `/sandbox-runtime/secrets-env` confirmed in source (`pkg/agentd/secrets/secrets.go:174`)
- Format: `KEY=VALUE` lines, written by `applyEnvSecret()` for `env-secret` type credentials
- File may not exist when no env-secrets are bound → handle gracefully

### Contract document
- `design/stories/epic-64-triggers-workflows/NODE-EXECUTE-CONTRACT.md` — full API contract with captured response shapes

---

## Key Decisions

1. **expr-lang via reflect.StructOf** — maps don't type-check; generated structs do. CamelCase field names.
2. **POST /session/:id/message** — correct synchronous endpoint, not `/prompt`.
3. **Validate+retry for structured output** — mandatory since opencode doesn't enforce schemas.
4. **Pre-validate agent names via GET /agent** — opencode's silent fallback masks config errors.
5. **Go script language deferred to v2** — original design listed Python/Node/Go; spike validated Python/Node only. Go is compiled (plugin/generated-main pattern), materially different from source-import; no concrete workflow demands it; `runtimes/go` already supports ad-hoc `go run`. (Reviewer finding #3.)
6. **scriptwrap does NOT enforce dict returns** — `Execute` returns `json.RawMessage`; US-64.7 must validate dict shape on top. (Reviewer finding #2.)
7. **Both `env-secret` AND `api-key` write to `/sandbox-runtime/secrets-env`** — earlier draft of the contract incorrectly listed only `env-secret`. `applyAPIKey` at `pkg/agentd/secrets/secrets.go:485-497` writes `API_KEY_<NAME>=value` lines. (Reviewer finding #1.)

---

## Blockers

None. A7 (workspace activation timing) deferred — needs a live cluster.

---

## Tests Run

```
go test -timeout 30s -race -v ./pkg/workflows/exprlang/     → 7/7 PASS
go test -timeout 30s -race -v ./pkg/workflows/scriptwrap/    → 6/6 PASS
```

---

## Next Steps

1. Update the design doc with the 5 minor corrections identified in NODE-EXECUTE-CONTRACT.md
2. Begin US-64.2 (data model: migration `000016`) — the design doc + contract are the spec
3. US-64.3 (storage layer) follows directly

---

## Files Modified

- `pkg/workflows/exprlang/validate.go` (new)
- `pkg/workflows/exprlang/validate_test.go` (new)
- `pkg/workflows/scriptwrap/wrapper.go` (new)
- `pkg/workflows/scriptwrap/wrapper_test.go` (new)
- `design/stories/epic-64-triggers-workflows/NODE-EXECUTE-CONTRACT.md` (new)
- `go.mod`, `go.sum` (expr-lang dependency added)
