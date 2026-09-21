# Worklog: #1525 — the send_message self-send guard

**Date:** 2026-09-21
**Session:** Loud-warning (not refusal) pre-flight guard when send_message's target equals the caller's own origin; branch `fix/1525-self-send-guard`
**Status:** Complete

---

## Objective

Close the orchestrator's 11-misfire class: send_message addressed at the caller's own session delivered silently (the message boomeranging as the caller's next turn). Deliver anyway (legitimate self-notes exist — the issue's explicit ruling), but WARN loudly in the result.

---

## Work Completed

- **TDD red-first:** three rows before the guard — self-send injected (warns + delivers), self-send declared (warns, self-declared mode), normal send (NO warning — the guard must not fire). All failed pre-implementation; all green post.
- **The guard** (`mcp_tools.go` mcpSendMessage): after origin resolution and before the detached delivery, `sessionID == origin` → `selfWarn` text (names the condition, states the boomerang consequence, points at re-checking vs carrying on). The result map gains `warning` ONLY when non-empty (an empty-string field would pollute every normal result — caught by the absent-on-normal-send row when the first draft serialized unconditionally).
- **Tool description** updated: the self-send sentence now states the warning exists.

## Key Decisions

- Warning-only, per the issue's ruling — the teaching-error pattern (#1469): the result teaches the correct action instead of refusing.
- The guard sits AFTER origin resolution (it needs the resolved origin, injected or declared — both modes covered by rows) (no behavioral ordering constraint; compose is origin-stamping of the payload, unaffected — r2 corrected the r1 placement claim, which said "before compose").

### Assumptions (Rule 7)

| # | Assumption | Validation |
|---|---|---|
| A1 | The equality check is the right trigger (no fuzzy matching needed) | The evidence base: all 11 misfires were exact self-addressing; near-miss IDs are a different (nonexistent) class |
| A2 | Warning must be ABSENT on normal sends | The absent-on-normal-send row (failed the first draft — the map always serialized the key) |

## Blockers

None.

## Tests Run

- `go test -run TestMCPSendMessage_ ./cmd/workspace-agentd/` — the full send_message family green (the 3 new + the existing idle/busy/abort rows)
- Full agentd package — ok 288.9s; vet clean; gofmt clean

## Next Steps

- PR (Fixes #1525), iterate to APPROVED, notify the orchestrator.

## Files Modified

- `cmd/workspace-agentd/mcp_tools.go` — the guard + conditional warning field
- `cmd/workspace-agentd/mcp_tools_test.go` — the three rows
- `cmd/workspace-agentd/mcp_server.go` — tool description sentence
