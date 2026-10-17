# Worklog: #1699 — Board REST API + openapi + 4 SDKs + contract/canary parity (Lane 2)

**Date:** 2026-10-17
**Lane:** lenaxia/LLMSafeSpaces#1699 (implementation), design 0067 at main (merged), engine PR #1719 (merged, untouched)
**Status:** Complete — PR open, orchestrator merges on APPROVED

---

## Objective

Lane 2 of the Task Board (design 0067 §7): the REST projection — `/api/v1/me/projects|epics|tasks`, user-level only (ruling R7) — plus the full product contract tier: `sdks/openapi.yaml` canonical additions, the four hand-written SDKs, hurl contract tests, and canary scenarios in the three SDK canary suites. Owner ruling OR4 settled mid-lane: the PR-drift anchor is the EXPLICIT `pr:N` tag form only (pattern-constrained in the openapi schema, documented on endpoint descriptions, pinned in contract tests; no engine change).

## What shipped (3 commits, test → impl → docs ordering)

1. `test(board)` — the full red-first pin suite: `board_rest_test.go` (verified RED first: 8 test functions / 40 assertions, all `board.ErrNotImplemented`), `sdks/tests/contract/board.hurl` (happy + unhappy + authz arms for every endpoint), `s-board-crud` canaries in TS/Python/Go (+ Makefile registration, shallow class — the workspace fixture is row-only), and red wire-level SDK tests (Go/Java stub services, TS/Python tests against the coming surface).
2. `feat(board)` — the projection: handler (25 operations), router groups + contract-fixture stub + app.go wiring, openapi.yaml (25 ops + schemas + board error shapes + OR4), 4 SDKs. All green.
3. `docs` — this worklog + CHANGELOG `[Unreleased]` + the 0.33.2 double-header dedupe tick.

## Surface inventory (25 operations, all AuthMiddleware, user-level per R7)

| Route | Op | Engine seam |
|---|---|---|
| `/me/projects` | GET, POST | ListProjects / CreateProject |
| `/me/projects/:id` | GET, PATCH | GetProject / UpdateProject (rename cascades materialized display) |
| `/me/projects/:id/archive` + `/unarchive` | POST | Archive/UnarchiveProject |
| `/me/projects/:id/epics` | POST | CreateEpic (draft default) |
| `/me/projects/:id/tasks` | GET (stage filter, enumerated set) | ListTasks |
| `/me/epics/:id` | GET, PATCH | GetEpic (derived progress) / UpdateEpic |
| `/me/epics/:id/activate|pause|archive|unarchive|cancel` | POST | epic lifecycle + CancelEpic (→ paused, R2) |
| `/me/epics/:id/tasks` | POST | CreateTask |
| `/me/tasks/:id` | GET, PATCH | GetTask (blocked-by) / UpdateTask (criteria check-off) |
| `/me/tasks/:id/move-stage|start|cancel` | POST | MoveStage / TaskStart (Claim render) / CancelTask |
| `/me/tasks/:id/dependencies` + `/:dependsOnId` | POST, DELETE | Add/RemoveDependency (re-derived task render) |

Adapter contract (pinned by tests): actor = `{kind: user, userID: JWT, surface: rest}`, NO claim token (the manager-surface snap-back guard); requestId rides the body to the event writer; reads never disclose foreign rows (plain 404, no suggestions — the 0066 §4.7 boundary); error classes map through the SAME `renderBoardError` the internal automation seam uses (404+suggestions / 403 authz+workspace_binding / 409 guards / 400 binding) — the parity matrix's REST column. Row encoding is the engine's canonical JSON (snake_case `short_id`), identical to the worker seam's rendering.

## OR4 — pr:N anchor encoding

- `BoardPrAnchor` schema component: `type: string, pattern: ^pr:[1-9][0-9]*$, example: pr:1234`, with the ruling transcribed in its description (explicit tag ONLY; no free-text guessing, no URL scraping; structured-field form deferred additive).
- Path params for tasks pattern-constrained to `^(tsk-[0-9a-z]{8}|UUID)$` etc.; short-id shapes pinned `^[a-z]+-[0-9a-z]{8}$` on every row schema.
- Documented on create/update-task descriptions + the Task `tags` property; contract test pins `jsonpath "$.tags[0]" matches /^pr:[1-9][0-9]*$/`; all three canary suites assert the same regex; SDK unit tests decode it.

## Engine-seam gaps (STOP items per charter — flagged on the PR, not worked around)

The engine's reads are GetProject/ListProjects/GetTask/ListTasks/GetEpic only. Three board_read projections from the design's REST surface have NO seam and were omitted rather than editing `pkg/board`:
1. **List epics** (`GET /me/projects/:id/epics`) — no ListEpics.
2. **Search** — no search seam.
3. **Health** — no health/lease/cooldown read seam.
Route, SDK methods, and openapi paths deliberately exclude them; the PR comments name them for the engine lane (#1698 follow-up) or a ruling.

## Testing

- Handler pins: actor stamping across all 16 mutating actions, requestId threading, owner-scoped read perimeter (foreign = plain 404), error-class parity table (10 classes), adapter-owned validation (required fields, stage filter enumeration, malformed bodies), envelope rendering, 201-on-create, Claim decode, dependency re-derived-row envelope.
- Contract: `board.hurl` — 40 arms incl. validation 400s, foreign-workspace 404, near-miss 404 with suggestions (dependency target, project-scoped), self/cycle 409s, epic-state 409, stage-machine 409 from backlog, cancel idempotence, epic-cancel → paused, unauthenticated 401.
- Canaries: `s-board-crud` × 3 suites (TS/Python/Go), shallow class, workspace fixture created/deleted in-scenario.
- SDK unit tests: Go 5 fns (paths + wire encoding + OR4 + stage filter + claim decode), TS 7 (vitest), Python 3 (respx), Java 6 (MockWebServer-style HttpServer).
- Race-mode found + fixed a real flake in my own reads pin (per-path call counts asserted inside a map-iteration loop — order-dependent; now asserted after the loop with the ownership-check fan-out documented).

## Verification

- `go test -count=1 ./api/...` — green (full suite).
- `go test -race -count=1 ./api/internal/handlers/ ./api/internal/server/` — green (handlers 126s, server 3.0s).
- `go test -race -count=3 -run TestBoardREST ./api/internal/handlers/` — green.
- `TestOpenAPIRouterContract` green (spec↔router bidirectional, 25 new ops); `make -C sdks validate` green (refs resolve, unique operationIds).
- SDKs: `go test ./...` (sdk/go), `npx tsc --noEmit && npx vitest run` (86), `pytest` (138), `mvn test` (52) — all green.
- Pre-commit gates green (repolint, gofmt, goimports, golangci-lint new-only, helm render).
- Hurl + canaries are CI-side (live API); SDK publishes are CI-side per charter.
- Note: the 15G workspace PVC hit 100% mid-lane; cleared regenerable caches (/tmp build junk, go build cache, stale node_modules, playwright + golangci caches) to complete race verification.

## Housekeeping tick

CHANGELOG.md carried a pre-existing `## [0.33.2] - 2026-09-17` double-header directly above `## [0.33.2] - 2026-09-18` (the empty 09-17 header block, content under 09-18) — verified pre-existing on main via git history; the empty duplicate header is deleted in this docs commit with all released content intact.
