# Worklog: #1699 — Board REST API + openapi + 4 SDKs + contract/canary parity (Lane 2)

**Date:** 2026-10-17 (r2: 2026-10-18)
**Lane:** lenaxia/LLMSafeSpaces#1699 (implementation), design 0067 at main (merged), engine PR #1719 (merged, untouched)
**Status:** Complete — PR #1728 open, orchestrator merges on APPROVED

## Objective

Lane 2 of the Task Board (design 0067 §7): the REST projection — `/api/v1/me/projects|epics|tasks`, user-level only (ruling R7) — plus the product contract tier: `sdks/openapi.yaml` additions, the four hand-written SDKs, the §4.3/§6 parity-matrix harness, hurl contract suite, and canary scenarios in the three SDK canary suites. Owner ruling OR4 settled mid-lane: the PR-drift anchor is the EXPLICIT `pr:N` tag form only (pattern-constrained in the openapi schema, documented on endpoint descriptions, pinned in contract tests; no engine change).

## What shipped (test → impl → docs ordering; r2 = one fix commit set on top)

1. `test(board)` — red-first pin suite: `board_rest_test.go` (verified RED first: 8 test functions / 40 assertions, all `board.ErrNotImplemented`), `sdks/tests/contract/board.hurl` (happy + unhappy + authz arms), `s-board-crud` canaries ×3 suites (+ Makefile registration), red wire-level SDK tests (Go/Java stub services compile; TS/Python red).
2. `feat(board)` — the projection: handler (23 operations), router groups + contract-fixture stub + app.go wiring, openapi.yaml (23 ops + schemas + board error shapes + OR4), 4 SDKs.
3. `docs` — this worklog + CHANGELOG `[Unreleased]` + the 0.33.2 dedupe tick.
4. `r2` (review fixes, CHANGES_REQUESTED verdict): hurl dedupe (self-contradictory duplicated cancel block — the stale arm expected 409 on cancel-of-cancelled while the engine is idempotent 200); the §6 parity-matrix harness (`board_parity_test.go`); StartTask chunked-empty-body fix (ContentLength −1 misclassified as a present body → spurious 400; EOF-on-empty is now accepted, malformed JSON still 400s); counts corrected everywhere (23 operations, 34 hurl arms); worklog conformed to the mandated template; route-shape ruling note recorded (Key Decisions).

## Key Decisions

- **Route shape (ruling note per r1-5):** design §4.3a sketches `/api/v1/me/projects[/:projectId[/epics|tasks…]]`. The implementation adds flat sibling groups `/me/epics/{id}` and `/me/tasks/{id}` beside the nested ones because resolution is global by short id (engine `resolveEpic`/`resolveTask` are unscoped; FKs are identity, §4.1) — nesting everything under `/projects/{id}/...` would force a redundant scope the engine does not use, and the flat groups keep epic/task PATCH/lifecycle/action URLs stable regardless of parent. All groups stay under `/me` (user-level, R7). Recorded here and on the PR body as an additive interpretation, not silence; a design addendum can ratify or reshape at the owner's discretion.
- **OR4 encoding:** `BoardPrAnchor` schema (`type: string, pattern: ^pr:[1-9][0-9]*$, example: pr:1234`) with the ruling transcribed in its description (explicit tag ONLY; no free-text guessing, no URL scraping; structured-field form deferred additive). Path params pattern-constrained to short-id/UUID forms; documented on create/update-task + the Task `tags` property; pinned in hurl (`jsonpath "$.tags[0]" matches /^pr:[1-9][0-9]*$/`), all three canary suites, and SDK unit tests.
- **Row encoding:** the engine's canonical JSON (snake_case `short_id`) renders verbatim — identical to the internal automation seam's rendering; no second encoding invented at the adapter.
- **Error-class parity:** every error maps through the SAME `renderBoardError` the internal seam uses; the parity harness pins (status, code) equality across surfaces on the shared action.
- **Reads perimeter:** owner check at the adapter for unscoped engine reads — foreign rows are plain 404s (no suggestions across authorization boundaries, §5); mutations rely on the engine's own scoped resolution (project mutations 404) / `requireOwner` (epic/task mutations 403) — pinned as-is.
- **Engine-seam gaps → STOP items, not workarounds** (see Blockers).
- **Counts (corrected at r2):** 23 REST operations (8 project-group, 8 epic-group, 7 task-group — reflection-pinned by the parity coverage test); 34 hurl arms after the dedupe. hurl + canary suites are CI-REGISTERED, not locally "green" — they need a live API; local verification covers compile + handler/unit/parity suites.

## Surface inventory (23 operations, all AuthMiddleware, user-level per R7)

| Route | Op | Engine seam |
|---|---|---|
| `/me/projects` | GET, POST | ListProjects / CreateProject |
| `/me/projects/:id` | GET, PATCH | GetProject / UpdateProject (rename cascades materialized display) |
| `/me/projects/:id/archive` + `/unarchive` | POST | Archive/UnarchiveProject |
| `/me/projects/:id/epics` | POST | CreateEpic (draft default) |
| `/me/projects/:id/tasks` | GET (stage filter, enumerated set) | ListTasks |
| `/me/epics/:id` | GET, PATCH | GetEpic (derived progress) / UpdateEpic |
| `/me/epics/:id/activate\|pause\|archive\|unarchive\|cancel` | POST | epic lifecycle + CancelEpic (→ paused, R2) |
| `/me/epics/:id/tasks` | POST | CreateTask |
| `/me/tasks/:id` | GET, PATCH | GetTask (blocked-by) / UpdateTask (criteria check-off) |
| `/me/tasks/:id/move-stage\|start\|cancel` | POST | MoveStage / TaskStart (Claim render) / CancelTask |
| `/me/tasks/:id/dependencies` + `/:dependsOnId` | POST, DELETE | Add/RemoveDependency (re-derived task render) |

Adapter contract (pinned): actor = `{kind: user, userID: JWT, surface: rest}`, NO claim token (manager-surface snap-back guard); requestId rides the body to the event writer; error classes per the shared mapping.

## The parity matrix (§4.3/§6, added at r2)

`board_parity_test.go` — rows = every §4.2 action (22), columns = REST / external platform MCP / agent-internal MCP:
- **REST column live:** allow arm per action + representative deny arm (mapped class) through the router fixture.
- **Agent column live** (Lane 1 seam): worker actions present (heartbeat/throttle/move-stage/read), manager rows absent-by-design (route-table pin — exactly the four worker routes), and guard-class parity on the SHARED action (move_stage: four error classes render identical (status, code) on both surfaces).
- **External MCP column pinned pending-Lane-3:** every cell is `pendingLane3` (except claim_next — permanently absent everywhere) and a source-scan proves no `board_*` tool is registered in pkg/mcp today; the scan reddens the moment Lane 3 registers one, forcing the cell to flip to a live row with its own guard arms.
- **Construction guarantee:** reflection pins every exported `*BoardHandler` method to exactly one matrix row — an endpoint added without a row (or a row naming a missing endpoint) fails `TestBoardParity_Coverage`.
- Declared asymmetries are absent-cells, not missing coverage: heartbeat/report_throttle absent on REST; claim_next absent on all three surfaces (engine-internal, R9); manager rows absent on the agent registry.

## Blockers

- **Engine-seam gaps (STOP items per charter — flagged on the PR, not worked around):** (1) epic listing — `GET /me/projects/:id/epics` has no `ListEpics` seam; (2) board search — no seam; (3) board health — no seam. Omitted from routes/SDK/openapi rather than editing `pkg/board`; needs an engine-lane follow-up (additive read seams) or a ruling.
- **Event-loss:** no CI run on first push (3048e94c); PR comment posted per charter, orchestrator recovered pipelines (2b03100b) and union-merged main (f679803c).
- Environmental, resolved: the 15G workspace PVC hit 100% mid-lane; cleared regenerable caches only (/tmp build junk, go build cache, stale main-checkout node_modules, playwright/golangci caches). Other lanes' worktrees untouched.

## Next Steps

- Lane 3 (MCP projections) flips the external-MCP matrix cells to live rows — the source-scan forces it.
- Engine lane: ListEpics/search/health read seams (additive) → the three stopped REST endpoints + SDK methods land in a follow-up.
- Lane 4 (frontend) consumes the REST surface + SSE feed.
- CI: hurl board suite + `s-board-crud` ×3 run against the live API; SDK publishes are CI-side.

## Files Modified

- `api/internal/handlers/board.go` — REST projection adapter (23 ops).
- `api/internal/handlers/board_rest_test.go` — handler pins (actor stamping, perimeter, error-class parity, validation, envelopes).
- `api/internal/handlers/board_parity_test.go` — §4.3/§6 parity-matrix harness (r2).
- `api/internal/handlers/board_automation_test.go` — fake worker-seam service grows `moveErr` (shared-action parity arms).
- `api/internal/server/router.go` + `router_openapi_contract_test.go` — route groups + fixture stub.
- `api/internal/app/app.go` — handler wiring.
- `sdks/openapi.yaml` — board tag: 23 ops, schemas, params, error shapes, OR4.
- `sdks/{typescript,python,go,java}` — board surfaces + wire-level tests.
- `sdks/tests/contract/board.hurl` — 34-arm contract suite (deduped at r2).
- `sdks/canary/{typescript,python,go}/scenarios/s-board-crud*` + `sdks/canary/Makefile` — canary scenarios (CI-registered).
- `CHANGELOG.md` — `[Unreleased]` Lane 2 entry (counts corrected at r2) + 0.33.2 dedupe.
- `worklogs/NNNN_2026-10-17_1699-board-rest-sdks.md` — this worklog.

## Verification

- `go test -count=1 ./api/...` — green (full suite).
- `go test -race -count=1 ./api/internal/handlers/ ./api/internal/server/` — green (race mode caught + fixed an order-dependent flake in the reads pin at r1-time).
- `TestOpenAPIRouterContract` green (spec↔router bidirectional, 23 new ops); `make -C sdks validate` green.
- SDKs: go test (sdk/go), tsc + vitest 86, pytest 138, mvn 52 — all green locally.
- hurl board suite + canaries: CI-registered (live API), not claimed locally green.
- Pre-commit gates green (repolint, gofmt, goimports, golangci-lint new-only).
