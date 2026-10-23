# Worklog: Task Board manager-grade pending_input exits (#1832 WS2)

## Scope
The tsk-3do6n458 incident: an escalated row (attempts=3, all work done
and pushed) froze the whole dependency chain — the lane worker AND the
orchestrator-class session both present on the agent surface, whose
pending_input exits are manager-surface-only, and the pod manager seam
carried NO move_stage route. Fix, across the ratified seams:

- pkg/board: SurfaceAgentManager ("agent_manager") — a DISTINCT audit
  vocabulary for the pod seam's manager projection (tonight's freeze
  diagnosis needed exactly this distinction in the actor trail).
  stage.go's pending_input guard narrows to SurfaceAgent (the ordinary
  worker rule UNCHANGED: token-holding workers stay denied). The
  workspace binding (requireManagerAction /
  requireAgentWorkspaceTarget / the retarget refusal / workspace-
  scoped reads) extends to the manager grade via isAgentSurface.
- api/internal/handlers board_automation_manager.go: managerActor
  stamps the manager-grade surface; NEW BoardManagerMoveStage behind
  the same resolveManager belt (harness-attested sessionId,
  live-assignee worker refusal, fail-closed omission, belt-error
  refusal). Router: POST /internal/v1/automation/board/tasks/:id/
  move-stage. Parity route-table row updated.
- pkg/agent/opencode loopback.go: BoardManagerMoveStage client method
  (the boardManagerPost stamping discipline).
- cmd/workspace-agentd: board_move_stage dual-routes (#1832 WS2) —
  claimToken present → worker projection (wire unchanged, worker
  compat untouched); absent → dispatchBoardManagerTool (identity gates
  both tiers; a spawned worker omitting its token gains NOTHING: the
  manager gate refuses it). Tool schema: claimToken documented, not
  required; description rewritten (manager set + pending_input exits
  named).

## Tests
- pkg/board pending_exit_manager_integration_test.go (red-first on
  this branch's parent commit): worker-with-token still DENIED;
  agent-manager exits (re-claim branch w/ assignee, backlog branch on
  escalated rows, the ONE attempts reset); workspace binding 403 on a
  foreign-workspace manager actor; surface vocabulary pin.
- handlers board_manager_move_stage_test.go: belt fail-closed
  (omission 403, worker session 403, belt error 503), success stamps
  SurfaceAgentManager + session identity, no claim token.
- agentd: ClaimTokenSchema + DispatcherWiring updated for the dual
  route (token-less identity-less never reaches the wire; token-less
  WITH injected identity rides the :id manager route);
  board_manager_e2e_integration_test.go section E — the FULL path
  exits an escalated pending_input row to backlog, attempts reset.
- ManagerActorStamping + parity route-table pins updated (the
  agent→agent_manager surface change is deliberate and pinned).

## Gates
go build/vet clean; handlers, agentd (full 365s), pkg/board,
pkg/agent/opencode suites green; make openapi-validate ok (no REST
surface change); no schema change; worker seam wire sets untouched
(additive only).
