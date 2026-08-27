# Worklog: MCP SendMessage live content from session.event deltas (#1053)

**Date:** 2026-08-27
**Session:** Close the follow-up filed from PR #1052's adversarial review: SendMessage's SSE accumulation read a top-level `content` field the broker envelope never carries — live capture was dead code and every response fell through to the history round-trip.
**Status:** Complete

---

## Objective

`session_message` returns streamed assistant text as it arrives (contract `part.delta` events inside `session.event` envelopes), with the history fallback retained for the no-idle/timeout paths.

## Work Completed

- `pkg/mcp/client.go` SendMessage scanner: for `session.event` envelopes, decode `data` as a contract event and accumulate `delta` (session-scoped: deltas carrying another sessionId on the same workspace stream are ignored). Removed the phantom `content` field read.
- Unit tests repinned from the phantom shape (`{"type":"content","content":...}` — a field the broker never emits) to the real envelope+delta shape; added the cross-session isolation test.
- Gate test `TestMCPClientSessionMessage_LiveContentFromSessionEvents` proves live capture through the production router + broker (publish two deltas + idle → result is the concatenated stream, not the history fallback).

## Key Decisions

- Deltas with an empty/absent sessionId are accepted (envelope session-scoping already filters by the broker's outer `session_id` for workspace streams); explicit mismatched sessionId is ignored.
- Idle-break with captured content returns the stream (existing contract, now reachable); fallback still covers SSE-error/timeout/no-idle.

## Tests Run

- `go test -race ./pkg/mcp/` — ok (repinned SSE suite + isolation test)
- `go test -race -run TestMCPClientSessionMessage ./api/internal/server/` — ok (gate)
- golangci-lint 0 issues; build + gofmt clean

## Files Modified

- `pkg/mcp/client.go`, `pkg/mcp/client_test.go`, `api/internal/server/mcp_router_integration_test.go`
