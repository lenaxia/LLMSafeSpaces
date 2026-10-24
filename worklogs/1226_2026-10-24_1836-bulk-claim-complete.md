# NNNN — #1836 bulk task claim/complete variants (per-item result arrays)

Issue: https://github.com/lenaxia/LLMSafeSpaces/issues/1836
Branch: fix/1836-bulk-variants

## Charter + validate-first (owner ruling: confirm the spec's premises on CURRENT origin/main before designing)

The spec (filed 2026-10-21, P2) proposes bulk variants "where actions
naturally repeat", with a four-clause contract: per-item result arrays
(partial failures, never all-or-nothing), idempotent retry ("already-done
items return done, not conflict"), per-item validation (one malformed id
must not sink the batch), and a batch cap with a clear overflow error.
This lane's scope: the **bulk claim** (task_start batch) and **bulk
complete** (move_stage batch) variants — the review-settlement sweep is
the lived round-trip pain the issue documents.

Seam validation on origin/main @ 8ee3a0f91 (post #1863/#1864/#1904/
#1930/#1933/#1935):

- **HOLDS**: `Service.TaskStart` (engine.go:128) and `Service.MoveStage`
  (stage.go:44) are the single-task claim/complete seams, unchanged in
  shape; `board_transitions` keeps one row per transition via
  `writeTransition` (store.go:383). Bulk = per-item delegation to these
  seams (each item its own transaction, audit event, transition row,
  spawn/directive side effects) — no second write path exists.
- **MOVED (#1864 ruling, CHANGELOG 0.41.0)**: "the bulk projection never
  carries live claim tokens — bulk surface = no tokens" landed AFTER the
  spec. Delta: `BulkItemResult` carries `{id, status, escalated?, error?}`
  ONLY — TaskStart's `Claim` envelope (with `claim_token`) is NOT echoed
  by the bulk projection; pinned by
  `TestBulk_NoClaimTokensInBulkResults`.
- **MOVED (#1831/#1930/#1935)**: item refs are word-phrase short ids
  (legacy random + UUID still resolve — dual-accept window). Delta: the
  per-item `id` echoes the caller's ref VERBATIM (self-identifying);
  resolution rides the same `resolveTaskForActor` as the single forms.
- **MOVED (round-3 #2, router.go:1362)**: `task_start` is dropped from
  agent surfaces ("execution = activation + engine claiming"). Delta: the
  bulk CLAIM follows the single form's boundary — REST owner surface +
  external platform MCP only, deliberately absent from the agentd pod
  seam (pinned as a forbidden name in the agentd registry-shape test).
  The bulk MOVE lands on the pod seam (mirrors #1832 WS2's
  BoardManagerMoveStage).
- **HOLDS with consequence**: auto-review (#1832 lane, live via #1930+):
  `MoveStage →in_review` consults `maybeAutoAssignReview`. Delegation
  preserves this per item — a bulk →in_review batch may dispatch N
  reviewers, exactly as N single calls would.
- **NEW bulk-tier semantics (spec clause 2)**: same-stage moves stay
  un-enumerated at the single-task seam (GuardStage). The spec's
  "already-done items return done" is implemented as a bulk pre-check:
  an item already at the target stage (move) / already in_progress
  (claim) returns `done` with NO write. Retry idempotency is belt and
  braces: per-item requestID derivation `batch#ref` replays into the
  eventWriter's 200-with-original, and is distinct per item (two
  same-project items under one batch key never collide into one audit
  event — pinned).

## Design

`pkg/board/bulk.go`: `BulkMaxItems=50`, `BulkItemResult`,
`BulkTaskStart`, `BulkMoveStage`. Batch-level refusals (empty batch,
over-cap, off-vocabulary toStage) are whole-batch `invalid_input`
(GuardInput → 400), nothing processed. Per item: resolve +
never-disclose perimeter (a foreign row renders the same `not_found`
item a miss renders — REST read-perimeter parity), the idempotent
pre-check, then delegation to the single-task seam with the derived
per-item key. Guard errors keep their engine codes verbatim
(`stage_transition`, `done_guard`, `claim_token`, `task_paused`, …).

Surfaces (each a thin adapter; parity rows added to the §4.3 matrix):

- REST: `POST /api/v1/me/tasks/bulk-start`, `POST /api/v1/me/tasks/bulk-move-stage`
  (static siblings of the `/:id` routes; gin routes statics first);
  documented in sdks/openapi.yaml (the router↔spec contract test
  enforces it).
- Pod seam: `POST /internal/v1/automation/board/tasks/move-stage-bulk`
  behind the same resolveManager belt (fail-closed identity, worker
  refusal, belt-error=503).
- agentd manager tool: `board_move_stage_bulk` (catalog + dispatch +
  plugin INJECTED_SESSION_TOOLS — the injection-supply test pins both
  directions).
- External platform MCP: `board_start_tasks` + `board_move_stage_bulk`
  riding the REST routes via the client; tool-tier validation
  front-runs doomed wire calls (required ids, non-empty, cap, enum —
  the #1904 discipline).

Naming: `board_start_tasks` (plural object — the issue's own
"delete_task(s)" style) and `board_move_stage_bulk` (ONE shared target
stage across tasks; per-item stages would be N single calls).

## RED-first evidence (all three tiers failed on absent symbols before any implementation)

- `pkg/board/bulk_integration_test.go` — `undefined: board.BulkItemResult`,
  `s.svc.BulkTaskStart undefined` (compile-RED), then green after
  bulk.go: 12 suite tests covering per-item arrays, partial failure
  (blocked row / done-guard miss / paused-class pass-through),
  malformed-id isolation, idempotent resubmit (no double spawn, no
  duplicate transitions/events), escalation flag, cap+empty refusals
  with nothing processed, foreign-row never-disclose (both variants),
  distinct-items-under-one-batch-key audit, worker claim-token
  arbitration per item, and the no-tokens pin.
- `api/internal/handlers/board_bulk_rest_test.go` +
  `board_manager_bulk_test.go` — `undefined: board.BulkItemResult`,
  `h.BoardManagerMoveStageBulk undefined` (compile-RED), then green:
  binding, actor stamping (rest + agent-manager surfaces), requestID
  threading, engine guard pass-through, belt fail-closed.
- `pkg/mcp/board_tools_bulk_test.go` — `undefined: board.BulkMaxItems`
  (compile-RED), then green: allow arms (exact client calls), required
  args + cap + enum refusals with zero wire calls, registration set
  (29 board tools) + no-claim-token schema pin.

## Tests

- Engine: `pkg/board/bulk_integration_test.go` (PG integration, the
  shared BoardSuite harness).
- REST/pod: `api/internal/handlers/board_bulk_rest_test.go`,
  `board_manager_bulk_test.go`; parity matrix rows (`bulk_start`,
  `bulk_move_stage`) + route tables updated
  (`board_parity_test.go`); fake `fakeBoardManager` grew the two seam
  arms.
- agentd: registry-shape pin + `board_move_stage_bulk` wire subtest
  (verbatim array + session identity on the wire) in
  `cmd/workspace-agentd/mcp_board_tools_test.go`.
- External MCP: `pkg/mcp/board_tools_bulk_test.go`; registration count
  pins updated (27→29 tools; FullWorkflow 54→56).

Local: full `go test ./pkg/board/ (integration) ./pkg/mcp/ ./api/internal/handlers/ ./api/internal/server/ ./cmd/workspace-agentd/ ./pkg/agent/...` green; `golangci-lint run` 0 issues on touched trees; `gofmt` clean.

## Deliberate scope boundary

Typed SDK methods (sdks/go|python|typescript|java) and frontend UI for
the two new REST routes are NOT included: #1836's mandate is tool
round-trip economy (the lived pain is MCP tool calls re-sending
conversational context), the issue lists no SDK/frontend surface, and
the SDKs are hand-rolled subsets with no completeness pin against
openapi.yaml. openapi.yaml rows ARE included (the contract test
enforces route↔spec agreement). A follow-up can add typed SDK methods
mechanically if wanted.

## Review round 1 (CHANGES_REQUESTED @ f34722c8) — remediation

Hard gates:
1. **Issue closure**: the PR claimed `Closes #1836` while this lane
   delivers the chartered subset (move_stage-across-tasks + the bulk
   claim addition; the issue's session/delete/dependency/create batches
   stay open for sibling lanes). Fixed: PR body now `Refs #1836 —
   partially addresses`; no auto-close.
2. **Pod-seam real-wiring e2e**: added
   `TestIntegrationBoardManagerMoveStageBulk` (real BoardService + real
   belt + PG): per-item envelope through real wiring (done +
   failed(done_guard) in one 200; foreign-workspace item per-item
   not_found), the live-assignee worker refusal from Postgres on the
   bulk route, session-attributed audit for landed items, no-tokens.

Non-blocking findings fixed:
- **Incorrect comment** (house rule): bulkFailedItem's "resolution
  misses keep their did-you-mean detail" was FALSE — the bulk envelope
  renders a bare not_found for misses AND foreign rows (miss ≡ foreign,
  the never-disclose shape; the single-task 404's suggestions are safe
  there, not here). Comment + PR body corrected.
- **Missing pin — read-then-act boundary**:
  `TestBulk_PreCheckBoundaryDelegatesAuthority` (converge arm via the
  move_stage.effect failpoint flipping an item between writes; refuse
  arm: pre-check passes, machine refuses → failed with guard_stage,
  nothing written) — the pre-check never fabricates outcomes; the
  row-locked single-task seam is authoritative.
- **Missing pin — agent-surface perimeter**:
  `TestBulk_AgentSurfacePerimeter` (agent-manager actor, workspace
  binding: a same-owner foreign-workspace row renders bare not_found on
  both variants — the owner≡workspace-owner invariant made
  load-bearing-visible) + the e2e's foreign item arm at the HTTP tier.
- **requestId blast radius teaching**: tool descriptions (external MCP
  both tools + agentd) now say reusing a batch key for a DIFFERENT
  batch replays the first batch's outcomes.
- **Style — vocabulary consolidation**: single source
  `stageVocabulary`/`StageInVocabulary`/`StageVocabulary()` in
  pkg/board (bulk.go's set, both handlers' presence checks, the
  manager list filter's inline literal all render from it now).

## Files

- CHANGELOG.md
- api/internal/handlers/board.go
- api/internal/handlers/board_automation_manager.go
- api/internal/handlers/board_automation_test.go
- api/internal/handlers/board_bulk.go
- api/internal/handlers/board_bulk_rest_test.go
- api/internal/handlers/board_manager_bulk_integration_test.go
- api/internal/handlers/board_manager_bulk_test.go
- api/internal/handlers/board_parity_test.go
- api/internal/handlers/board_rest_test.go
- api/internal/server/router.go
- cmd/workspace-agentd/board_manager_tools.go
- cmd/workspace-agentd/mcp_board_tools_test.go
- cmd/workspace-agentd/mcp_server.go
- cmd/workspace-agentd/mcp_tools.go
- docs/api/mcp.md
- pkg/agent/opencode/loopback.go
- pkg/board/board.go
- pkg/board/bulk.go
- pkg/board/bulk_integration_test.go
- pkg/mcp/board_tools.go
- pkg/mcp/board_tools_bulk_test.go
- pkg/mcp/board_tools_test.go
- pkg/mcp/client.go
- pkg/mcp/integration_test.go
- pkg/mcp/server_test.go
- runtimes/opencode/plugins/llmsafespaces-origin.js
- sdks/openapi.yaml
