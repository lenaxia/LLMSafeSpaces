# Worklog: Board tooling hardening P2 — board_list/board_health + setCriteriaMet safety (#1837)

## Scope
Issue #1837 P2a+P2b, stacked on worklog 1198 (P1, PR #1863). Composed
with main @ cef43acf (post-#1859 task drawer/audit timeline — no
overlap: TaskEvents is per-task history; these are per-project /
fleet projections).

## P2a — board_list + board_health (agent manager surface)
- Engine reads (pkg/board/reads.go): ListTasksInWorkspace (project
  resolves WORKSPACE-scoped — foreign 404 — then the ListTasks
  projection) and BoardHealthInWorkspace (the five health queries
  scoped by target_workspace_id instead of user).
- Seam: GET /internal/v1/automation/board/projects/:id/tasks (stage
  +failed filters validated) and GET /health — both manager-gated
  (resolveManager: session identity fail-closed + worker belt).
- board_list response: {project_id, project, tasks, ready_set, count};
  the ready set is DERIVED from the unfiltered list (backlog +
  unblocked + unpaused + not cooling — every predicate is already a
  listed column; no new engine seam, per the review's "only if
  needed" bar).
- agentd: board_list/board_health registry entries + dispatch
  (mcpAutomation manager set + dispatchBoardManagerTool) + loopback
  GETs (identity rides the query for body-less GETs); origin plugin
  INJECTED_SESSION_TOOLS += both (the injection-supply freeze pin
  caught it).
- Parity: matrix row agentTool → agentTools []string (mechanical,
  30+ rows); board_read row now carries [board_read, board_list,
  board_health]; seam route pins += 2. Registry shape test += 2.

## P2b — setCriteriaMet safety
- pkg/board/criteria_check.go: CriteriaCheck{Index, Met} with
  UnmarshalJSON (object form {index, met?} default met=true; legacy
  integer N → {N, true}; everything else loud) + MarshalJSON (object
  form) + validateCriteriaUpdate (replace+check-off mutual exclusion;
  bounds against the effective set — both GuardInput).
- Service UpdateTask wires it (the old silent ignore + accidental
  check-only are gone); TaskInput.SetCriteriaMet is *CriteriaCheck.
- Surfaces: REST boardTaskRequest + seam body bind both forms via the
  unmarshaler; pkg/mcp BoardTaskBody + criteriaCheckArg (int/int64/
  float64/map) + tool schema/description; agentd registry schema.
- Echo: mutated_criterion {index, met, text} on task update responses
  (all surfaces — external MCP passes REST through).

## Tests (RED first — commit f83aaf85 quotes the failing output)
- pkg/board/criteria_check_test.go: decode matrix + validation matrix.
- pkg/board/criteria_check_integration_test.go (PG, CI): uncheck via
  object form, legacy check-off still works, combined refused with
  nothing stored, bounds/negative refused.
- api/internal/handlers/board_list_health_seam_test.go: list (one
  call, stage+claim columns, ready set), manager gate 403, health
  workspace-scoped, setCriteriaMet object form + echo, combined 400.
- cmd/workspace-agentd/board_list_health_test.go: registry + wire
  contracts (paths, query stamps, passthrough).

## Gates
go build/vet ./... clean; pkg/board, pkg/mcp, api/internal/handlers
(83s incl. parity), cmd/workspace-agentd (365s) ok;
make openapi-validate ✓ (setCriteriaMet oneOf; BoardEpic/BoardTask
content_echo_diff noted in 1198); make repolint ✓.
