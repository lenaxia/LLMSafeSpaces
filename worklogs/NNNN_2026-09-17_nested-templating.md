# Worklog: #1417 — dotted-path agent-prompt templating (post-#1420 rebuild)

**Date:** 2026-09-17
**Session:** #1420's parallel batch did NOT cover #1417: agent-node prompts still render top-level keys only. Rebuilt on main: renderTemplateRefs with nested/hyphenated paths, the opencodeAddr test seam, handler-level happy + unhappy integration legs.
**Status:** Complete

---

## Objective
Webhook-driven runs hand the fire envelope (payload under body); prompts must address {{.body.topic}} directly — parity with condition-node expression depth.

## Work Completed
- renderTemplateRefs: ONE left-to-right scan over the ORIGINAL prompt emitting into a strings.Builder (an earlier draft used sentinel tokens; the shipped design has none — no restoration phase exists to collide). At each match: exact top-level key hit first (any brace-free key charset — KEY matching is the pre-#1417 behavior; value rendering improved: composites as JSON, nil as null), then dotted-path walk (hyphenated segments addressable); unresolvable refs stay literal. The ref BODY excludes braces and newlines, so an unclosed {{.x can never swallow a following valid ref (cross-newline and same-line variants both pinned). Replacement values are never re-scanned — a value shaped like a ref stays inert (fuzz-verified by review), so an externally-supplied payload field cannot smuggle other envelope fields into the prompt.
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
