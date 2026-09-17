# Worklog: #1417 — dotted-path agent-prompt templating (post-#1420 rebuild)

**Date:** 2026-09-17
**Session:** #1420's parallel batch did NOT cover #1417: agent-node prompts still render top-level keys only. Rebuilt on main: renderTemplateRefs with nested/hyphenated paths, handler-level happy + unhappy integration legs over main's agentAddrAtomic/getAgentAddr seam.
**Status:** Complete

---

## Objective
Webhook-driven runs hand the fire envelope (payload under body); prompts must address {{.body.topic}} directly — parity with condition-node expression depth.

## Work Completed
- renderTemplateRefs: ONE left-to-right scan over the ORIGINAL prompt emitting into a strings.Builder (an earlier draft used sentinel tokens; the shipped design has none — no restoration phase exists to collide). At each match: exact top-level key hit first (any brace-free key charset — KEY matching is the pre-#1417 behavior; value rendering improved: composites as JSON, nil as null), then dotted-path walk (hyphenated segments addressable); unresolvable refs stay literal. The ref BODY excludes braces and newlines, so an unclosed {{.x can never swallow a following valid ref (cross-newline and same-line variants both pinned). Replacement values are never re-scanned — a value shaped like a ref stays inert (pinned by TestRenderTemplateRefs_NoDoubleRender and _InertControlShapedValues), so an externally-supplied payload field cannot smuggle other envelope fields into the prompt.
- handler-level integration rides main's agentAddrAtomic/getAgentAddr seam (an earlier draft carried its own seam var; the merge adopted main's).
- Handler-level happy + unhappy legs through the REAL handler + wire: nested/hyphenated/composite values reach the rendered prompt; unresolvable refs stay literal.

## Key Decisions
- Literal fallback over erroring: the agent sees exactly what failed to bind.

## Blockers
None.

## Tests Run
- go test ./cmd/workspace-agentd/ -run 'TestRenderTemplateRefs|TestWorkflowExecuteHandler_AgentNode' -v — 9 templating tests (7 unit: nested/hyphen/any-charset+flat-dotted/no-double-render/inert-control/unclosed-cross-newline/unclosed-same-line; 2 handler-integration: happy render + unresolved-literal through the real wiring) — all PASS.
- go test ./cmd/workspace-agentd/ -count=1 — full suite PASS.
- E2E (in-repo, kind): local/issue-1417-templating-e2e.sh — a workflow run whose agent prompt carries {{.body.topic}} + {{.missing.path}} executes against a mock OpenAI upstream that echoes the prompt; T1 asserts the rendered nested value in the run output, T2 the literal unresolvable ref (same turn). Structural pins: TestIssue1417E2EScript_* (bash syntax, row assertions, nightly-workflow registration).

## Next Steps
PR → review; live leg in the comprehensive test (#1427).

## Files Modified
- cmd/workspace-agentd/workflow_execute.go (+workflow_execute_test.go)
- cmd/workspace-agentd/mcp_server.go (workflow_create templating documentation) + mcp_server_test.go (guidance pin)
- local/issue-1417-templating-e2e.sh (+issue_1417_templating_e2e_script_test.go) — the kind-cluster e2e legs
- .github/workflows/e2e-nightly.yml — the e2e step (registered; pin-enforced)
- worklogs/this entry
