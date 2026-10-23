# Worklog: Task Board delegated-review completion coupling (#1832 WS1)

## Scope
The tsk-zbdeufiw incident: a delegated review task (in_review, ALL
criteria met) could not reach done after its subject PR merged — no
agent-visible path existed. This lane workstream gives the coupling a
structural form and an engine settle, entirely inside the existing
stage machine (no schema change, no new surface):

- pkg/board/review.go (NEW): the `review-of:<subject-short-id|uuid>`
  tag anchor (the pr:N discipline — explicit, malformed-inert, never
  guessed; workspace-scoped candidate resolution via the GIN tags
  index). completeReviewTasks settles the subject's anchored review
  tasks IN the →done transaction: criteria ALL met → done (guarded
  UPDATE, transition row, deterministic engine:review_complete event,
  reverse-edge unblock + epic completion check — the doneCascade
  effects); criteria UNMET → NEVER done — the in_review row parks
  pending_input/drift_review with a review_park event (orchestrator
  guard ruling: stranded-in_review rots invisibly; the pending_input
  column is the owner-visible surface, serviceable via the WS2 manager
  exit). Chained delegation recurses over a visited set (bounded:
  every completion lands done; done rows never re-enter candidacy).
- pkg/board/stage.go: doneCascade gains the settle + the evidence URL;
  nudge carries the settle's nested events for post-commit SSE
  publication (r1-C5 preserved — rollback publishes nothing).
- pkg/board/reconciler.go: the RC2 drift_done branch runs the same
  settle (a drift-completed subject settles its reviews in the same
  transaction; events ride the existing commitPublishing).
- Docs where pr:N is documented: external MCP board_create_task, the
  agentd tool schema, REST CreateEpicTask comment, openapi
  createBoardTask description. No wire-form change anywhere
  (worker_compat untouched).

## Tests
- pkg/board/review_completion_integration_test.go (PG harness, CI —
  red-first on PR #1866's test-only commit): manager-move subject-done
  completes an in_review review task (transition + review_complete);
  RC2 drift subject-done rescues a drift_review-parked review task
  (the incident's exact shape); criteria-unmet parks with review_park
  and NEVER completes; malformed anchors inert.
- Existing suites: pkg/board, pkg/mcp (description pins), agentd
  (tool-schema pins), handlers, openapi-validate all green locally.

## Gates
go build/vet clean; make openapi-validate ok; local PG unavailable
(client-only pod) — integration red/green evidence rides CI
(secrets-integration, PG service), disclosed to the orchestrator.
