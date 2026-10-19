# Worklog: #1701 — agent-internal board_* MCP tools + claim-token semantics (Lane 3, agentd half)

**Date:** 2026-10-18
**Lane:** lenaxia/LLMSafeSpaces#1701 (implementation), design 0067 at main (merged; §4.3c normative), engine PR #1719 + REST adapter PR #1728 (merged, untouched)
**Status:** Complete — PR open, orchestrator merges on APPROVED+green

## Objective

Lane 3's agentd half (design 0067 §4.3c / §7): the agent-internal MCP
registry tools the in-workspace worker sees — names/shapes per the
design, claim-token semantics, attempt accounting, the fresh-worker
discipline (R9; the charter's "(R5)" citation is read as the
fresh-worker ruling — R5 itself is the no-DAG-entities ruling, which
this lane also honors: no new entities, no new claim logic), the
parity-matrix agent-column update, and agentd registration wiring. No
`pkg/board` edits; everything delegates to the engine through the Lane
1 seam (`/internal/v1/automation/board/*`, board_automation.go).

## What shipped (red → green → pins → docs)

1. `test(agentd,api)` RED — `mcp_board_tools_test.go`: registry shape
   (exactly four tools; `board_claim_next`/`board_claim` and every
   manager row pinned ABSENT from tools/list), claim-token schema pins
   (`claimToken` required on heartbeat/throttle/move_stage; never on
   board_read), wire contract (paths/methods/SA-token/workspace stamp/
   verbatim field passthrough incl. claimToken + requestId; nested
   `{"task":{…}}` tolerance), error passthrough (409 claim_token and
   404 did-you-mean survive the hop verbatim), pre-wire ref validation
   (traversal/malformed refs never reach the wire), dispatcher wiring
   (callMCPTool routes; token-less move_stage refused with NO wire
   call). Verified RED first: all 5 test functions failed with
   `unknown automation tool: board_read` / `unknown tool:
   board_heartbeat` / empty registry. Plus the description-guidance
   pins in `TestMCPHandler_ToolDescriptionGuidance` (RED: `Should be
   true` on absent tools/list entries).
2. `test(api)` RED — the parity matrix's agent column gained the
   agentd REGISTRY tier: `TestBoardParity_AgentRegistryColumn` +
   `scanAgentdForBoardTools` (catalog `Name:` lines + dispatcher
   `case` lines). RED first: registry scan found nothing vs the
   matrix's four live `agentTool` cells.
3. `feat(agentd)` GREEN — `mcp_server.go` catalog entries (four tools,
   guidance-grade descriptions) + `callMCPTool` dispatch; `mcp_tools.go`
   `mcpAutomation` board cases + `requireBoardClaimToken` (the
   session_archive-boolean discipline: shape refusal pre-wire, the
   platform stays the arbiter) + `boardTaskRef`;
   `pkg/agent/opencode/loopback.go` client methods
   (`BoardTaskGet/BoardHeartbeat/BoardReportThrottle/BoardMoveStage`
   over a shared `boardWorkerPost` with the `withWorkspaceID` stamp)
   + `boardRefPattern` (`^[0-9A-Za-z-]{1,64}$`) for path-interpolation
   safety — board refs (tsk-…, UUIDs) are a superset of
   `automationIDPattern`'s hex charset, hence their own guard.
4. `test(api,ci)` — `board_worker_semantics_integration_test.go`
   (build-tagged; runs in secrets-integration.yml beside the pkg/board
   kill-tests, gate widened for board handler files / agentd mcp_* /
   loopback.go): the REAL BoardService behind the REAL resolve chain,
   six arms (below). Not red-first-able — the engine + seam predate the
   lane; these are the lane's characterization pins at the composition
   surface the tools ride. EXECUTED locally against real Postgres 15
   (portable binaries; migrations 000001-000036 applied by hand),
   race-enabled, all green.
5. `docs` — this worklog + CHANGELOG `[Unreleased]`.

## Key Decisions

- **Tool names per design §4.3c verbatim:** `board_heartbeat`,
  `board_report_throttle`, `board_move_stage`, `board_read`. The
  design names all four; nothing was left open.
- **board_read's v1 shape = the item get** (`GetTaskInWorkspace`,
  shortId/UUID): the design's board_read covers list/get/search/health,
  but the engine exposes only the item read on this surface today
  (epic listing, search, health are flagged #1699 engine follow-ups
  from PR #1728 — the router comment pins their absence). The
  DESIGN's tool name is kept; the list/search/health projections grow
  INTO the tool when their seams land. Recorded here per the charter's
  "follow the engine's own vocabulary and record the decision".
- **Argument keys mirror the seam wire verbatim** (camelCase:
  `taskId`, `shortId`, `claimToken`, `observedProgressAt`,
  `retryAfterSeconds`, `toStage`, `evidenceUrl`, `requestId`) — the
  mcpAutomation passthrough discipline; agentd invents no second
  schema. The nested `{"task":{…}}` wrapper is tolerated (unwrapped on
  the wire) via `passThroughBody(body, "task")`, matching trigger_/
  workflow_ tools.
- **Pre-wire enforcement scope:** the dispatcher refuses ONLY
  token-less in-flight writes (heartbeat/throttle/move_stage) and
  malformed/missing task refs. `toStage` presence, stage legality,
  Done-guard, pending_input exits, workspace binding, attempts — ALL
  platform-side (the platform validates; a malformed toStage gets the
  platform's verbatim error, which is the contract). This keeps the
  arbiter singular (Postgres + BoardService) per the design's "no new
  arbitration".
- **claim_next "cell flip" interpretation:** the charter says the
  parity matrix pins "claim_next ABSENT on all surfaces — YOUR lane
  flips that cell". Registered tools now EXIST at the agent tier, so
  the claim_next row's agent-side absence flipped from
  seam-route-table-only enforcement to REGISTRY-source-scan
  enforcement (`TestBoardParity_AgentRegistryColumn` fails if
  `board_claim_next` — or any manager tool — is ever registered). The
  cell stays `absent` by design (R9: engine-internal, absent on ALL
  surfaces, permanently); what flipped is the tier that proves it.
  Making the tool live would violate §4.2/§4.3c/§6 — not done.
- **Foreign-task 404 vs 403:** on this engine, a pod resolving a task
  whose project targets another workspace gets a plain 404
  (resolveTaskInWorkspace resolution miss, empty suggestions) — the
  GuardWorkspaceBinding 403 class fires on rows resolvable in-scope.
  The integration pin pins the 404-class behavior the engine actually
  implements (r1-S1's structural binding: resolution itself is
  workspace-scoped).
- **Integration-test isolation:** every arm seeds through the service
  (real mint/events path) into the POD's workspace; arms run
  sequentially and each leaves NO claimable backlog row (terminal /
  in-flight / pending_input / cooldown-backlog), so each claim lands
  on its own task — asserted loudly by `claimFor` (short-ID equality).
  The foreign arm targets a second workspace and claims through it.
- **Postgres for honest verification:** no PG server in the sandbox
  (no root/docker) — portable zonky PG 15.12 binaries run as the
  sandbox user on :5433, migrations applied by hand (the CI job's
  psql loop, verbatim), so the integration pins were EXECUTED, not
  just CI-destined. Cache discipline on the shared 15G PVC: gocache
  kept under /tmp/opencode, cleaned twice on ENOSPC (only own caches;
  other lanes' worktrees untouched).

## Verification (what was actually run, locally)

- RED runs (before implementation): `go test ./cmd/workspace-agentd/
  -run 'TestMCPBoard'` → 5 FAILs (unknown tool); `./api/internal/
  handlers/ -run 'TestBoardParity'` → AgentRegistryColumn FAIL (empty
  registry); ToolDescriptionGuidance board subtests FAIL (absent).
- GREEN runs: `go test ./cmd/workspace-agentd/ ./api/internal/handlers/
  ./pkg/agent/opencode/ -count=1` → all ok (327s / 80s / 12s).
- Integration: `go test -race -tags=integration -count=1 -run
  'TestIntegrationBoardWorkerSemantics' ./api/internal/handlers/` → ok
  against local PG 15 (six arms: claim_token refusal; evidence gate;
  single-use + expiry; attempts accounting; foreign-task 404-class;
  pending_input self-exit refused manager_surface_only).
- `go vet -tags=integration ./api/internal/handlers/` clean;
  `golangci-lint run --new-from-merge-base=origin/main` (v2.13.1, the
  CI pin) 0 issues on touched packages; gofmt/goimports clean.
- NOT run locally (CI-owned): the secrets-integration job end-to-end,
  pkg/board kill-tests (untouched package), full `make test` tree,
  frontend (untouched).

## The integration arms (design §6 mapping)

| Arm | Pins | Design |
|---|---|---|
| claim_token_refusal | no/wrong token → 409 `claim_token` on heartbeat/throttle/move_stage; the right token passes | §4.2 in-flight guard |
| heartbeat_evidence_gate | stale evidence → `extended:false`; fresh → `extended:true` | §4.4 / #1685 |
| token_single_use_and_expiry | forward exit consumes the token (read no longer carries it); lapse → sweep reclaim → old token dead on heartbeat AND move_stage; re-claim mints a fresh WORKING token | §4.4 / §6 case 1 |
| attempts_accounting | claim = the only increment (0→1); live 429 rolls it back (→0, backlog+cooldown); re-claim counts again (→1) | §4.4 throttle |
| foreign_task_404_class | pod cannot GET/heartbeat/move a task whose project targets another workspace — 404, empty suggestions, real token buys nothing | §4.4 binding / r1-S1 |
| pending_input_self_exit_refused | sweep stages pending_input KEEPING the token; the worker's still-valid token gets 409 `manager_surface_only` on exit | §4.2 / §4.3c |

## Blockers

None. Engine follow-ups remain #1699 (ListEpics/search/health —
board_read's growth path, documented in the tool description and the
matrix cell comment); no `pkg/board` edits were needed or made.
