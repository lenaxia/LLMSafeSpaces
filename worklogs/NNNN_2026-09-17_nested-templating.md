# Worklog: #1417 — dotted-path agent-prompt templating (post-#1420 rebuild)

**Date:** 2026-09-17
**Session:** #1420's parallel batch did NOT cover #1417: agent-node prompts still render top-level keys only. Rebuilt on main: renderTemplateRefs with nested/hyphenated paths, the opencodeAddr test seam, handler-level happy + unhappy integration legs.
**Status:** Complete

---

## Objective
Webhook-driven runs hand the fire envelope (payload under body); prompts must address {{.body.topic}} directly — parity with condition-node expression depth.

## Work Completed
- renderTemplateRefs, ONE expansion pass: top-level keys substitute by exact match behind sentinel tokens (any key charset — KEY matching is the pre-#1417 behavior; value rendering improved: composites as JSON, nil as null), then dotted paths walk nested maps (hyphenated segments addressable). Sentinels restore AFTER the scan — a value shaped like a ref is never re-expanded (pinned: an externally-supplied payload field cannot smuggle other envelope fields into the prompt). Unresolvable refs stay literal.
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
