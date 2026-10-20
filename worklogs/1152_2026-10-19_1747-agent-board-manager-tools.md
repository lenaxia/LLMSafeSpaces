# NNNN — #1747: agent board manager tools + the full amendment set

**Date:** 2026-10-19 · **Lane:** feat/1747-agent-board-manager-tools · **Issue:** #1747 (all ruling rounds: base, consolidated 6261010901, KISS 6261037825, round 3 6261147201, pause-must-pause 6261172515, delete refinements 6261349020, no-reconnect 6261560142, vocabulary addendum)

## What landed

The full #1747 amendment set on the Task Board:

1. **Agent manager tools** — 21 manager tools on the automation seam
   (pod identity, workspace-bound by construction, session-attributed
   audit), 4 worker tools unchanged, `claim_next` engine-internal
   forever, `task_start` agent-dropped. Registry: 25 board_* tools in
   agentd (TestMCPBoard_RegistryShape), pinned by the parity matrix's
   agent column (TestBoardParity_AgentRegistryColumn) and the NEW
   source-scan route pins (TestBoardParity_AgentColumn over
   router.go's seam group; TestBoardParity_RestRouteTable over the
   /api/v1/me groups — the scanMCPPkgForBoardTools discipline applied
   to routes, so the fixtures and the router cannot drift).
2. **Grant differentiation** — two tiers: agentd's spawn-time worker
   registry (board_workers.go; the `board-worker` node dispatch
   records; fail-closed dispatch without an injected session identity)
   + the engine belt (IsWorkerSession over live assignee rows, refused
   at the seam with 403). Pins: TestMCPBoard_ManagerGrantGate,
   TestBoardAutomation_ManagerGrantBelt, TestKill_1747_IsWorkerSession.
3. **Cancel → delete** — five-stage machine (migration 000038 deletes
   cancelled rows + swaps the CHECK + drops the stage_update_guard
   trigger), snapshot-bearing delete events (per-task on cascade),
   orphan listing + `unblocked_by_delete` events, NO auto-reconnect
   with an explicit `reconnect=true` escape hatch, board_transitions'
   task FK dropped (history survives deletes). Pins:
   TestKill_1747_DeleteOrphanSemantics, TestKill_1747_DeleteEpicForce.
4. **Pause-must-pause** — tasks.paused/projects.paused flags, one
   effective-pause predicate at the claim sites + forward-write guard,
   HALT/RESUME directives over WithWorkerMessenger (app wiring rides
   the pkg/agent adapter's SendAsync/Abort — no new transport), LAPSE
   lease accounting (adopted proposal: no freeze; the #1685 evidence
   gate needs no paused special-case). Pins:
   TestKill_1747_PauseMustPause, TestKill_1747_ProjectPauseParksAll.
5. **FIFO-only ordering** — priority columns dropped everywhere
   (migration, engine, REST, external MCP, SDKs×4, OpenAPI, frontend);
   candidateSQL's ORDER BY t.created_at, t.id is THE claim-order site
   (comment-pinned + TestKill_ClaimOrderFIFO, which replaced the
   priority kill-test).
6. **DAG spec** — cross-workspace guard (trigger relaxed to
   same-target-workspace; Go pre-check + mapDepErr), duplicate-edge
   idempotence pinned, per-node/per-epic caps
   (LLMSAFESPACES_BOARD_MAXEDGESPERNODE/MAXNODESPEREPIC),
   epic_activate inactive-dep acknowledgment + initial ready set.
   Pins: TestKill_1747_DAGScopeAndCaps, TestKill_1747_EpicActivateAcknowledgment.
7. **import_plan** — atomic materialization + dry-run render
   (pkg/board/plan.go; edges by payload key; Go-side cycle DFS).
   Pins: TestKill_1747_ImportPlan.
8. **Backpressure** — user/global claim caps at the choke point (quiet
   no-grant; TaskStart 409s GuardConcurrency), waiting_on derivation
   on reads; agentd pressure sampler (cgroup/statfs primitives already
   in sysmetrics.go, static thresholds + hysteresis, 30s push over
   POST /internal/v1/automation/board/resource-pressure), engine
   pressure map gates claiming + decorates reads. Pins:
   TestKill_1747_ConcurrencyCaps, TestKill_1747_ResourcePressureBackpressure.

## Process notes (r2 review round lessons)

- **r1.7-r1.10 were CI stabilizers while three REQUEST_CHANGES reviews
  accumulated** — the core findings (belt fail-open, integration gap,
  cancelled remnants) sat unaddressed across four pushes. The reviews
  were correct every time. r2 addressed them; review 4 then caught that
  r2's own checklist overclaimed (a regex removal of the dead CancelEpic
  fake silently missed; openapi prose survivors past the first two sites;
  a flagship integration test pushed red and unwired). The corrective:
  every "FIXED" claim in a findings response must be verified by an
  executed check, not a text substitution — grep-with-assertion in the
  fix scripts, run-the-test before push, and a compile gate for every
  module in the diff (the Go canary's separate go.mod made it invisible
  to the main-tree vet; ci.yml now gates it explicitly).

## Process notes

- **Red-first**: the parity flip + registry shape + route scans landed
  as commit e45c32d6 BEFORE any implementation, with the quoted
  failures in its message (10 pin tests reddening: Coverage, RESTColumn,
  AgentColumn, AgentRegistryColumn, ExternalMCPColumn,
  WorkerOnlyAbsentOnREST, RestRouteTable, IdempotentReplay,
  AuditRowInputs, RegistryShape).
- **Pause semantics evolution**: the original charter's "pause never
  touches leases" was superseded mid-flight by the PAUSE-MUST-PAUSE
  amendment (halt/resume directives + forward-write refusal); the LAPSE
  lease-accounting proposal (freeze vs lapse) was proposed and adopted
  over send_message. The engine was restructured before the seam/agentd
  tiers consumed it — no rework needed.
- **Ruling churn absorbed**: reorder was chartered, extended, then CUT
  (final KISS ruling) before any ordering code shipped — the only
  trace is the priority REMOVAL that the final ruling added.
- **Ops**: shared-PVC ENOSPC twice during the lane (concurrent builds);
  resolved per runbook (own-scratch cleanup + go clean -cache),
  reported to the orchestrator both times.
- **SDK sweep**: Go/Python/TypeScript/Java clients + openapi.yaml +
   canary scenarios carry the vocabulary swap (cancel→delete, priority
  gone, pause/resume/import-plan added); `make -C sdks validate` green;
  Go SDK tests green.
- **Known CI deltas**: pkg/board kill-tests (integration tag) need the
  CI Postgres — they compile under `go vet -tags integration` and are
  written against the real harness; the agentd relay-renewal test is
  timing-sensitive under a loaded shared runner and passes in
  isolation (verified; unchanged by this lane).

## Follow-ups (recorded, not blocking)

- engineEventID-style deterministic ids for pause/resume lifecycle
  events (currently requestID-based like the other lifecycle writers —
  the #1728 requestId-threading follow-up covers the family).
- Adaptive (self-configuring) pressure thresholds — static + hysteresis
  is the r1-acceptable posture per the ruling.
- The board↔workflow shared-kernel extraction is explicitly deferred
  (owner: evidence first, its own lane).
