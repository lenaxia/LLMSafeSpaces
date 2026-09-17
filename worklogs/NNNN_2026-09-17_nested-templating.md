# Worklog: #1417 — dotted-path agent-prompt templating (post-#1420 rebuild)

**Date:** 2026-09-17
**Session:** #1420's parallel batch did NOT cover #1417: agent-node prompts still render top-level keys only. Rebuilt on main: renderTemplateRefs with nested/hyphenated paths, the opencodeAddr test seam, handler-level happy + unhappy integration legs.
**Status:** Complete

---

## Objective
Webhook-driven runs hand the fire envelope (payload under body); prompts must address {{.body.topic}} directly — parity with condition-node expression depth.

## Work Completed
- renderTemplateRefs: {{.a.b.c}} walks nested maps (charset [a-zA-Z0-9_.-] — hyphenated header keys addressable); scalars render bare, composites as compact JSON, unresolvable refs stay literal (inspectable, never silent empties).
- opencodeAddr var seam (was a hardcoded const URL) so handler-level integration can point the harness calls at a stub.
- Handler-level happy + unhappy legs through the REAL handler + wire: nested/hyphenated/composite values reach the rendered prompt; unresolvable refs stay literal.

## Key Decisions
- Literal fallback over erroring: the agent sees exactly what failed to bind.

## Blockers
None.

## Tests Run
4 templating tests (2 unit, 2 handler-integration); full agentd suite green.

## Next Steps
PR → review; live leg in the comprehensive test (#1427).

## Files Modified
- cmd/workspace-agentd/workflow_execute.go (+tests)
