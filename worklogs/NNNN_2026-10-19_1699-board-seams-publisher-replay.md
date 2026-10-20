# Worklog: #1699 — Board read seams + board.event publisher + lifecycle replay keys

**Date:** 2026-10-19
**Session:** The four #1699 engine-seam closings on top of Lane 2's merged REST projection (PR #1728): ListEpics, board search, board health, the §4.6 board.event SSE publisher, and the lifecycle requestId threading PR #1728's review flagged.
**Status:** Complete — PR open, orchestrator merges on APPROVED

---

## Objective

Design 0067 §4.2 names board_read(list/get/search/health) and §4.6 mandates SSE fan-out of committed board events; Lane 2 landed the 23-op REST projection with three of those reads STOPPED on missing engine seams (worklog 1133 Blockers) and the UI lane (worklog 1139) shipped its SSE feed dark against "the designed payload shape". This lane closes the four tracked gaps (lenaxia's #1699 comment: 1 ListEpics, 2 search, 3 health, 4 board.event publisher) plus the Q1 amendment (lifecycle requestId replay protection — orchestrator-ratified IN SCOPE).

---

## Work Completed

### Red-first (commit 9d10cb1e, demonstrated red before any fix)

- `pkg/board/reads_seams_test.go` + `event_publisher_test.go` (integration tag, CI-executed): ListEpics derived-progress/empty/foreign arms; search hit/near-miss-suggest/far-miss/validation/perimeter arms; health fixture snapshot + zeroed-board; publisher persist-then-publish, no-publish-on-abort (failpoint), no-republish-on-replay, engine claim + reconciler reclaim publish, nil-publisher writes succeed, lifecycle replay protection.
- `api/internal/handlers/board_seams_rest_test.go`: the three endpoint adapter contracts + lifecycle requestId bodies (6) + remove-dependency query param + REAL DuplicateError→200-with-original pin.
- `pkg/mcp/board_tools_searchhealth_test.go`: board_read q/kind/health modes + board_health tool (4 red).
- Parity: `replayExceptions` emptied — 7 actions reddened on BOTH columns; ghost-cell guard reddened on the unregistered board_health.
- Red counts at the red commit: handlers/parity 6 test functions + 16 subtests; mcp 4; server contract 1 (routes without spec).

### Engine seams (pkg/board)

- `ListEpics`: one aggregate query (LEFT JOIN over per-epic done/total counts) — derived progress per row, no N+1. Denominator = ALL tasks per the #1747 owner ruling (see Cross-lane conformance).
- `SearchTasks(userID, q, filters)`: validation (trim, ≤200, stage vocabulary → new `GuardInput` 400-class); user-scoped query with short-ID-exact OR escaped title ILIKE; project/epic filters resolve INSIDE the user-scoped subqueries (a foreign filter renders empty — no disclosure); tags via `@>`; LIMIT 50. Zero-hit near-misses suggest top-3 over the caller's short IDs + titles (0066 shape, `suggestTop3`, candidates capped at 1000).
- `BoardHealth(userID)`: six queries — active leases (+assignee, expiry), task cooldowns, project slot cooldowns, pending_input by origin, 20 recent reclaims (from `board_events` action='reclaim'), per-project slots in use.

### §4.6 publisher (pkg/board + handlers + app)

- `EventPublisher func(Event)` + `WithEventPublisher`/`SetEventPublisher` (Service and Reconciler — the reconciler delegates to its service).
- `eventWriter.run`: INSERT … RETURNING created_at; publish strictly after Commit (the replay/race branches return before the publish — a replayed requestId never republishes).
- `insertEventTx` → `recordEventTx` returning the committed view (ON CONFLICT DO NOTHING → ErrNoRows → zero Event → nothing to publish); all 12 engine/reconciler call sites collect views and commit through `commitPublishing` (commit failure publishes nothing).
- `api/internal/handlers/board_sse.go` (new file, zero conflict surface): `boardSSEEvent` renders the flat frame (fields siblings of `type` — the merged frontend's contract, pinned by test incl. no `data` nesting); `publishBoardFanout` → workspace stream + owner's user stream (owner = projects.user_id, NOT WorkspaceOwner — R7); `BoardEventPublisher(pool, log)` resolves the project row per event (indexed PK read), nil-broker → no publisher, lookup failure → log-only drop. app.go wires it on both boardSvc and boardReconciler inside the `cfg.Board.Enabled` block (the orchestrator-ratified single insertion).
- `api/internal/types/sse_event.go`: additive omitempty board fields (one block, declared to the orchestrator for #1760 keep-both mapping; no other event family's wire shape changes).

### requestId threading (Q1 amendment) + the found defect

- `projectLifecycle`/`epicLifecycle` thread the requestID their public signatures already accepted; REST lifecycle endpoints bind the optional `{requestId}` body (EOF-tolerant, the StartTask r1-4 discipline); remove-dependency reads `?requestId=`; MCP lifecycle tools + `BoardRemoveDependency` carry `request_id` (client + interface + mocks); all four SDKs thread the key (Go: additive variadic `BoardLifecycleOptions`; TS/Python: optional params; Java: overloads).
- **FOUND (pre-existing, fixed here — Rule 5):** no surface translated `DuplicateError` — a live replay answered 500 everywhere (the #1728 fake modeled 200-with-original but the adapter never implemented it). `renderBoardError` now renders the duplicate as 200 + the original recorded result (nil-result race path renders an honest minimal envelope). Pinned by `TestBoardREST_DuplicateReplayRendersOriginal`.

### Contract tier

- openapi: `GET /me/projects/{id}/epics`, `GET /me/board/search`, `GET /me/board/health` (+BoardSearchQ/Project/Epic/StageFilter/RequestIdQuery params, BoardEpicList/BoardSearchResult/BoardHealth schemas); lifecycle requestBody (BoardRequestIdOnly) on 6 ops; DELETE ?requestId; stale "listing absent" note removed. `make -C sdks validate` green; TestOpenAPIRouterContract green.
- SDKs: Go (ListBoardEpics/SearchBoard/GetBoardHealth + types), TypeScript (listBoardEpics/searchBoard/getBoardHealth + types), Python (list_board_epics/search_board/get_board_health + replay keys), Java (same + overloads) — wire tests in all four suites.
- hurl board.hurl: +88 lines (seam arms, replay ×2 including 200-with-original, DELETE query key, 400 arms). Canaries: s-board-crud ×3 grow list-epics/search-hit+near-miss/health/lifecycle-replay arms (post-cancel activate — no dispatchable work, so no worker spawns).
- MCP: `board_read` grows q/kind/epic_id/tags/health; `board_health` tool (19 total, registration + count pins updated); client wire methods real (stubs deleted); lifecycle request_id params (NoClaimTokenParams still green — reworded one description that tripped the forbidden-substring scan).

---

## Key Decisions

- **Board-wide /me/board group (orchestrator Q2 ruling):** search + health are user-level board-wide (the UI's search box and health panel are board-scoped; R7 user-level); epic listing nests under the project (design §4.3a's sketch). Additive interpretation recorded here per the worklog-1133 r1-5 precedent.
- **Cross-lane conformance (OWNER RULING, mid-lane):** external PR #1763 (#1747 agent manager tools) carries owner rulings that SUPERSEDE merged design 0067's semantics — design authority transfers to #1763 pending its landing. This lane conformed BEFORE gating: (1) ListEpics' progress denominator is Done-over-ALL-tasks (R2's "−Cancelled" exclusion is out — pin flipped 2→3 on the 1-done/1-canceled/1-backlog fixture); (2) search results render in creation order (created_at, id — the FIFO read-side mirror; the priority column leaves the schema with #1763, so no priority ORDER BY survives the rebase); (3) the reconciliation direction is ONE-WAY — this lane conforms to #1763, whose manager-tool feature (delete/pause/import/messenger/backpressure) is untouched here. Files hottest at rebase time: pkg/board/reads.go (their decorate*/order changes end where my seams insert), service.go (struct + NewService both extended), engine.go/reconciler.go (their SQL predicates vs my recordEventTx conversions), handlers/board*.go, router.go, app.go.
- **stage.go:608 writeAudit — NAMED MECHANISM, no threading (orchestrator-ratified):** heartbeat's monotonic evidence gate (`observedProgressAt.After(last_progress_at)`, audit rides the extends branch only) makes a same-stamp replay inert — it cannot re-extend or re-write the audit row. First-heartbeat edge safe (after the first success LastProgressAt is set). Threading would churn the agentd tool + automation seam + Heartbeat signature for zero behavioral change.
- **eventWriter site audit (15 non-test sites):** 12 already threaded; projectLifecycle + epicLifecycle fixed; writeAudit named-mechanism. The 12 engine/reconciler `insertEventTx` sites carry deterministic ids + ON CONFLICT DO NOTHING (deterministic-id replay protection) — publish-only-on-actual-insert keeps replays silent on the stream.
- **Search filters resolve inside the user-scoped query** (subqueries), never via unscoped pre-resolution — a foreign project/epic filter renders empty with no existence disclosure; suggestions draw only from the caller's rows.
- **Publisher fail-open semantics:** writes never depend on the projection (nil publisher = no-op; lookup failure = log-only drop); consumers are covered by the UI's reseed belt. Panic-safety is the api-side closure's concern if ever needed — the broker publish is in-memory/non-blocking today.
- **DuplicateError→200 at the shared mapping** (renderBoardError): one translation serves REST, MCP (rides the REST handlers), and the automation seam.
- **Replay-first ordering (self-review find, fixed pre-CI):** epicLifecycle's allowed-map reads the state the ORIGINAL write changed — a replayed activate (active→active) answered the epic_state 409, not 200-with-original (the stateless guards of move_stage/throttle/creates all sit inside or behind the eventWriter's fast-path; only the lifecycle helpers validated mutable state before the duplicate gate). Both lifecycle helpers now run `replayOf` (the eventWriter fast-path lifted to caller scope) after authz, before the transition validation — authz still precedes replay (policy outranks idempotency), state-dependent guards no longer do.
- **SDK replay-key ergonomics:** Go variadic options keep the released signatures source-compatible (v0.39.0 shipped Lane 2's SDKs — additive-only freeze discipline).

---

## Blockers

- None. Local PG server unavailable (client-only package, no root) — the pkg/board integration suite is CI-executed (secrets-integration.yml path filter covers pkg/board/ + api/internal/handlers/board); compile-verified locally (`go vet -tags integration`). Same tier as lane 2's CI-registered hurl/canaries.

---

## Tests Run

- Red (pre-fix, commands + counts in the lane record / milestone (a) message): handlers/parity 6 functions + 16 subtests red; pkg/mcp 4 red; TestOpenAPIRouterContract red.
- Green: `go test -count=1 ./api/...` (handlers incl. new SSE/parity/replay pins, server contract); `go test ./pkg/board/ ./pkg/mcp/` (+ `-tags integration` vet-clean); SDK suites — Go `ok`, TS typecheck + 90 tests, Python 140 passed, Java mvn BoardServiceTest 9/9; `make -C sdks validate` green. hurl board suite + canaries: CI-registered (live API), not claimed locally green.

---

## Next Steps

- CI: secrets-integration runs the pkg/board seam suite; contract + canary suites run against the live API; SDK publishes are CI-side.
- Frontend follow-ups (their lane's records): epic chips switch to `GET /me/projects/{id}/epics`; the health panel unblocks on `GET /me/board/health`; the SSE feed lights up with no frontend change (shape pinned by BoardPage.sse tests).

---

## Files Modified

- pkg/board: rows.go (search/health types), reads.go (three seams), events.go (EventPublisher, recordEventTx, commitPublishing, post-commit publish), service.go (publisher plumbing, lifecycle threading), board.go (GuardInput), engine.go + reconciler.go (recordEventTx conversions), reads_seams_test.go, event_publisher_test.go, event_publisher_unit_test.go (new tests).
- api/internal/handlers: board.go (three endpoints, optional-body binding, remove-dependency query key, q validation), board_automation.go (DuplicateError→200, GuardInput→400), board_sse.go + board_sse_test.go (new), board_seams_rest_test.go (new), board_rest_test.go (fake growth), board_parity_test.go (replayExceptions emptied, drivers, board_read row), types/sse_event.go (board fields).
- api/internal/server/router.go (three routes); api/internal/app/app.go (publisher wiring).
- pkg/mcp: board_tools.go (board_read modes, board_health, request_id params), client.go (wire methods, signatures), server.go (boolArg), server_test.go + board_tools_test.go + integration_test.go + client_test.go + board_tools_searchhealth_test.go (tests/fixtures).
- sdks: openapi.yaml; go/ (board.go + board_test.go); typescript/ (client.ts, types.ts, tests); python/ (client.py, tests); java/ (BoardService.java, BoardServiceTest.java); tests/contract/board.hurl; canary ×3 s-board-crud scenarios.
- CHANGELOG.md ([Unreleased]); this worklog.
