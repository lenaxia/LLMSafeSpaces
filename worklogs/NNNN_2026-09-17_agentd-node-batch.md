# Worklog: #1414/#1415/#1417 — agentd node-error surfacing, true contracts, nested templating

**Date:** 2026-09-17
**Session:** Agentd-side batch: script failures surfaced honestly (#1414), MCP tool descriptions corrected to the real contracts (#1415), agent-prompt templating gains dotted paths (#1417).
**Status:** Complete

---

## Objective
The workflow node executor and its documentation must tell the truth: real error causes, real node vocabulary and field spellings, and template placeholders that reach nested input.

## Work Completed
- #1414 (error surfacing): execScriptNode never prints a bare "exit -1: " again — empty stderr falls back to the real error (unsupported language, temp-dir failure, ...), and signal-death errors carry both. Spec-time validation added: unsupported script languages (anything but python|node) rejected by ValidateSpec at create, not at runtime.
- #1417: renderTemplateRefs resolves {{.a.b.c}} through nested maps (scalars bare, composites as compact JSON, unresolvable refs stay literal) — condition-node parity for path depth; webhook-driven agent prompts can finally address {{.body.topic}}.
- #1415: workflow_create description now names exactly script/agent/http/condition (with data contracts, otherwise-edge rule, single-start rule, dotted-path templating) and drops the invented transform/parallel/delay/mcp_call vocabulary; trigger_create teaches expr validation, next-slot anchoring ("never fires at creation moment"), camelCase workflowId, and the pod-scoping rule for DAG targets. Guidance pins updated to enforce the corrected contracts (NotContains for invented types and snake_case).

## Key Decisions
- Unresolvable template refs render literally rather than erroring — prompts stay inspectable and the agent sees what failed to bind.
- Language vocabulary pinned at spec validation (python|node) mirrors scriptwrap's executor set exactly.

## Blockers
None. (The interpreter-side gap — scratch sidecar has no python3/node — is the separate #1414 follow-up: needs a design decision on where scripts execute.)

## Tests Run
- renderTemplateRefs: nested scalars, composite-as-JSON, unresolvable-stay-literal, top-level back-compat.
- execScriptNode: unsupported language names the real cause (no bare exit -1).
- ValidateSpec: bash rejected at spec time.
- Guidance pins: new vocabulary/spelling/anchoring asserted; invented types and snake_case NotContains-pinned.
- Full agentd + pkg/workflows suites green.

## Next Steps
- PR → review. Then #1416 (CA bundle in image), #1418 (specYaml YAML), then the comprehensive live test of the whole system.

## Files Modified
- cmd/workspace-agentd/workflow_execute.go (+tests) — error surfacing, renderTemplateRefs
- cmd/workspace-agentd/mcp_server.go (+guidance pins) — true contracts
- pkg/workflows/dag.go (+test) — spec-time language validation
