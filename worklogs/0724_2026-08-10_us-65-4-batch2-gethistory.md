# Worklog: US-65.4 batch 2 - GetHistory contract shapes + frontend migration

**Date:** 2026-08-10
**Session:** US-65.4 batch 2 - migrate GetHistory endpoint to return contract-shaped session.Message[] via the Adapter, with frontend transformHistory updated.
**Status:** Complete

---

## Objective

US-65.4 batch 2: migrate the GetHistory endpoint from returning raw opencode bytes to returning contract-shaped `session.Message[]` via the Adapter. Update the frontend's `transformHistory` to consume the new shapes. This is the first client-facing handler migration in US-65.4.

---

## Work Completed

### Backend

- `proxy_handlers.go` GetHistory: adapter path calls `adapter.GetHistory` + `paginateContractHistory` (typed cursor pagination). Returns `session.Message[]` JSON. Legacy path retained.
- `paginateContractHistory`: new function, same cursor logic as `paginateOpencodeHistory` but on typed `[]session.Message`.
- 6 tests for `paginateContractHistory`: first page, second page, unknown cursor, fewer-than-limit, empty input, partial last page.

### Frontend

- `messages.ts` transformHistory: updated from `OpenCodeMessage[]` (opencode raw with `info.role`, `info.time.created` epoch millis) to `ContractMessage[]` (contract with `type`, `createdAt` ISO, `model.id`). Tool parts read `input`/`output` from `p.tool` directly (not `p.tool.state`).
- `messages.test.ts`: 10 tests with contract-shaped fixtures. Tests for text, reasoning, tool, and file_change parts.
- `messages.pagination.test.ts`: mock data updated to contract shapes.
- TypeScript typecheck clean. All 14 message-related tests pass.

### Infrastructure

- `mockAdapter.GetHistory` now configurable via `getHistoryFn` field.

---

## Tests

- `go build ./...` - clean
- `go test -timeout 30s -count=1 -run "TestPaginateContractHistory" ./api/internal/handlers/` - PASS (6 tests)
- `npx vitest run src/api/messages.test.ts src/api/messages.pagination.test.ts` - PASS (14 tests)
- `npx tsc --noEmit` - clean
- Pre-commit hooks - all pass

---

## Files Modified

**Backend:**
- `api/internal/handlers/proxy_handlers.go` (GetHistory adapter path + paginateContractHistory)
- `api/internal/handlers/mock_adapter_test.go` (getHistoryFn field)
- `api/internal/handlers/paginate_contract_history_test.go` (new, 6 tests)

**Frontend:**
- `frontend/src/api/messages.ts` (ContractMessage type + transformHistory)
- `frontend/src/api/messages.test.ts` (10 tests updated)
- `frontend/src/api/messages.pagination.test.ts` (mock data updated)
