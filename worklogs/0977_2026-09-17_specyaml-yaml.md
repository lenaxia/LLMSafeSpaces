# Worklog: #1418 — specYaml accepts YAML (as its name promises)

**Date:** 2026-09-17
**Session:** extractSpecJSON only passed through JSON-looking strings and handed raw YAML to a JSON parser — the field's name promised a dialect the API never supported.
**Status:** Complete

---

## Objective
Honor the specYaml contract: JSON passes through untouched; actual YAML converts via yaml.v3 (repo-standard) to the same JSON shape ParseSpec consumes; garbage names its dialect ("neither JSON nor YAML") instead of a JSON parse artifact.

## Work Completed
- extractSpecJSON returns (string, error); YAML branch normalizes through yaml.v3 → json.Marshal.
- Both create and the update path's specYaml replacement use the same dialect handling.
- The stored specYaml keeps the caller's dialect verbatim (as before); SpecJSON carries the normalized validated spec.

## Key Decisions
- JSON pass-through without round-trip (canonical stays canonical); YAML is converted, never echoed as JSON into specYaml.

## Blockers
None.

## Tests Run
YAML spec creates (content preserved into SpecJSON); garbage rejected with the dialect error; full handlers suite green.

## Next Steps
PR → review; ships with the batch release; the comprehensive live test authors one workflow in YAML.

## Files Modified
- api/internal/handlers/workflows.go (+tests)
- sdks/openapi.yaml, docs/api/mcp.md (contract surfaces)
- cmd/workspace-agentd/mcp_server.go (+pin), pkg/mcp/workflow_tools.go (+pin)
