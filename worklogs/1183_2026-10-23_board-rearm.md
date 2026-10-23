# Worklog: Task Board re-arm — attempts-exhaustion re-pooling (#1832 WS3)

## Scope
Escalation (pre-increment attempts >= cap, both claimOnce and
TaskStart) parks a row pending_input/escalated and the engine never
grants again — no path back into the pool existed. The re-arm action
is the explicit, audited manager judgment, shipped on ALL FOUR
surfaces (standing directive #20):

- pkg/board rearm.go: Service.RearmTask — pending_input/escalated
  only (stage + origin guards, server-arbitrated), → backlog with
  attempts=0 (the fresh-judgment ledger; the PRE-RESET count preserved
  in the rearm event payload), failure overlay + cooldown cleared (a
  re-arm is not a failure), one `rearm` event idempotent per
  requestId, transition row, pokeDrive after commit. Red-first: the
  parent commit carries the pins over an ErrNotImplemented skeleton.
- REST: POST /api/v1/me/tasks/:id/rearm (BoardHandler.RearmTask).
- External MCP: board_rearm_task (+ client method; tool count 53→54).
- Pod seam manager: POST /internal/v1/automation/board/tasks/:id/rearm
  (resolveManager belt) + loopback BoardRearmTask.
- agentd: board_rearm_task in the manager catalog + the MCP dispatch
  case list; the origin plugin's INJECTED_SESSION_TOOLS gains
  llmsafespaces_board_rearm_task (the injection-supply freeze pin).
- openapi: /me/tasks/{id}/rearm (rearmBoardTask).
- Parity: matrix row (all four columns live), REST + agent-seam route
  tables, replay + audit drivers.
- UI: DEFERRAL TICKET (flagged, not a silent gap) — the swimlane can
  surface a Re-arm affordance on pending_input/escalated cards; the
  REST surface is live for it.

## Tests
- pkg/board rearm_integration_test.go (red-first): re-pool semantics
  (backlog, attempts 0, claimable again counting from 1), replay
  200-with-original (one event), escalated-only guards
  (worker-origin pending_input and backlog refuse).
- Handlers/parity: matrix coverage (allow/deny/replay/audit), agent
  seam route table, tool counts, injection supply.

## Gates
go build/vet clean; pkg/board, pkg/mcp, handlers suites green;
agentd MCP suites green (full suite CI); openapi-validate ok. No
schema change; worker seam wire sets untouched.
