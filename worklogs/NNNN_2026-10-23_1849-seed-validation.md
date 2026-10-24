# Worklog: Atomic-create seed validation — #1849 items 1+6

**Date:** 2026-10-23
**Session:** level-canal-indicates (lane-1837 follow-up)
**Status:** In Progress

## Objective
Implement #1849 items 1+6 per the orchestrator adjudication (comment
6357191381): the seed-prevention layer composing with #1837's merged
guardrails — atomic task creates require a charter + criteria, and
placeholder-class charters reject at the boundary.

## Work Completed
- pkg/board/seed_validation.go: validateTaskSeed (non-empty trimmed
  description; >=1 criterion with text; GuardInput) + IsPlaceholderClass
  (the #1814 seven-token set, whole-value trim+case-fold; containment
  never matches; GuardTextRejected) applied to title/description/
  criteria. Wired in Service.CreateTask (after #1837 textguard) and
  validatePlan (dry-run refuses seedless nodes).
- send_message's guard delegates to the ONE shared set.
- Teaching surfaces: agentd + pkg/mcp tool descriptions/schemas
  (Required markings), openapi required list, docs/api/mcp.md row,
  TS BoardTaskCreateRequest required fields, Python create_task
  required kwargs, frontend TaskDialog create-mode gate + hint.
- Fallout seeding across every tier: pkg/board suite helpers +
  kill-tests, worker/manager-semantics + SSE-live integration, agentd
  E2E (incl. the new H0 full-chain refusal arm), hurl contract, Go/
  Python/TS canaries, SDK mock fixtures, vitest + playwright specs.

## Key Decisions
- UPDATE paths exempt (patches clear fields deliberately); create is
  the seed contract's scope.
- Textguard (corruption/caps) runs first, seed contract second.
- Placeholder set deliberately narrow, exact-match only (the #1814
  survivor discipline; short legit payloads and prose mentioning
  tokens pass).

## Blockers
None. (CI noise: lane-1884's usagestream timing flake appeared on 4
heads today incl. this PR's — passed on every rerun; zero diff
overlap; reported to the orchestrator.)

## Tests Run
Local: build, vet(+integration x3), gofmt, openapi-validate,
repolint, pkg/board, pkg/mcp, handlers, agentd (full 360s), vitest
217/217 + the board specs, tsc --noEmit, python ast-parse.
CI: per-head record at review time — SDK Contract + Frontend e2e +
secrets-integration PG suite green; the full-race job intermittently
red on the KNOWN lane-1884 usagestream timing flake (zero diff
overlap; cleared on every rerun).

## Next Steps
Merge per orchestrator adjudication; #1849 items 3+4 stay dropped,
item 2 already subsumed by #1864.

## Files Modified
(pkg/board) seed_validation{,_test,_integration_test}.go; service.go;
plan.go; and the fallout test files: 1747_kill, engine_kill,
event_publisher, harness, ids_phrase_integration, reconciler_kill,
remediation, service_kill, textguard_integration (_test.go each).
(cmd/workspace-agentd) mcp_server.go; mcp_tools.go;
board_manager_e2e_integration_test.go.
(pkg/mcp) board_tools.go; board_tools_test.go.
(api/internal/handlers) board.go; board_worker_semantics_integration_
test.go; board_manager_semantics_integration_test.go;
board_sse_live_integration_test.go; board_parity_test.go;
board_rest_test.go.
(frontend) TaskDialog.tsx; BoardPage.{validation,swimlane}.test.tsx;
api/board.ts; tests/e2e/board.spec.ts.
(sdks) openapi.yaml; go/{board,board_test}.go; python/{client,tests/
test_client}.py; typescript/src/{types,client}.ts + tests/client.test.
ts; java BoardServiceTest.java; canary/go s-board-crud; canary/python
s_board_crud.py; canary/typescript s-board-crud.ts; tests/contract/
board.hurl.
(docs) api/mcp.md; CHANGELOG.md.
