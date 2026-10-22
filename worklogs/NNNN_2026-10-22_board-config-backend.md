# Worklog: Board config backend — user-scoped board-wide defaults + per-project overrides (#1832 item 6, part 1/2)

**Date:** 2026-10-22
**Session:** Implement #1832 item 6 part 1: the Task Board config storage + API — a USER-SCOPED board-wide defaults map plus an optional per-project override map, merged at READ time (unset key in override = inherit user default; never-written user key = code default). Keys reviewColumn (off|manual|auto, default manual), autoAssignReviewer (bool, default false), defaultWorkerSlots (int > 0, default 2 — CONSUMED by CreateProject's slots fallback). Read/write API for user defaults (GET/PUT /api/v1/me/board/config), per-project overrides (GET/PUT /api/v1/me/projects/:id/config), and the effective merged config. Unknown keys, malformed values, and JSON nulls rejected at every belt. No UI (PR-A2), no review automation (PR-B1/B2).
**Status:** Complete

---

## Objective

Ship PR-A1 of the #1832 slice set (PR #1840): migration 000041 (both tables, key CHECK constraints, FK cascades), the pkg/board config model (typed override, per-key validation incl. null rejection, replace-semantics writes, read-time merge), the service seam (fail-closed decode of stored rows, guards-once R7 scoping), the REST projection (user + project config routes), and the OpenAPI contract — red-first throughout; then address the bot review's CHANGES_REQUESTED findings in-turn under the lane orchestrator's scope ruling.

## Scope ruling (lane orchestrator, recorded here deliberately)

- **User-scoped board-wide defaults, NOT instance-global.** "Global defaults" in #1832 item 6 means board-wide-across-the-user's-projects. Design 0067 R7 (boards user-level, owner full control) and the issue's merge model both pin it. `board_config_defaults` carries `user_id` (FK users ON DELETE CASCADE, PK (user_id, key)); routes sit on `/api/v1/me/board/config` — no admin gate (the original AdminGuard + the 403-vs-404 spec mismatch dissolve with the move). A future instance tier (instance > user > project) is documented in the migration header as deliberate future work.
- **Config-write audit rows: explicit owner deferral.** board_events deliberately carries no config vocabulary (replace-semantics writes are idempotent by construction; no consumer in #1832). Recorded in the migration header + PR body; revisit trigger: config provenance needs (e.g. "why did reviewColumn flip") → add a coalesced one-event-per-write later.

## Work Completed

### Red-first pins (commit 4bb9c9f5)

- `pkg/board/config_test.go`: merge semantics (override-wins, unset-inherits, nil/empty overlays), per-key validation, decode gate, code defaults.
- `pkg/board/config_integration_test.go`: Postgres round-trips for user + per-project replace-writes, unset=inherit across a later user-map flip, cascade on project delete, service-belt rejection, foreign-actor scoped-404.
- `api/internal/handlers/board_config_test.go`: REST round-trip, strict body gate, adapter actor stamping.
- RED demonstrated at 4bb9c9f5 as build-failure undefineds (exact commands in the commit message).

### Implementation (commit 3a127b67, then the review-fix commits)

- `pkg/board/config.go`: key vocabulary + typed `BoardConfig`/`BoardConfigOverride`/`ProjectBoardConfig`; `DefaultBoardConfig`; `ValidateBoardConfigKey` (GuardInput class; JSON null NEVER a value — unset is absent, never null); `DecodeBoardConfigOverride` (nothing outside the vocabulary rides the seam); typed service belt; per-key jsonb rows; REPLACE-semantics store writers (one tx each); service readers merge code → user → project at read time, resolving projects SCOPED to the acting user (design 0067:37 guards-once — the exported read can no longer be aimed at a foreign project by a future caller). Fail-closed reads on drift.
- `pkg/board/service.go`: CreateProject's slots fallback consumes the owner's effective `defaultWorkerSlots` (fail-closed on a drifted map — never a silent zero-value fallback); the fallback otherwise uses the `DefaultBoardWorkerSlots` constant (comment corrected: the 000036 SQL column default is pinned to the same value by convention; SQL cannot import Go).
- Migration `000041_board_config` (+ byte-identical helm mirror): `board_config_defaults` (user_id FK users ON DELETE CASCADE, PK (user_id,key), key CHECK) + `board_project_config_overrides` (PK (project_id,key), CHECK, FK projects ON DELETE CASCADE); header documents the user-scope ruling, the future instance tier, and the audit-deferral ruling.
- `api/internal/handlers/board_config.go` + `router.go`: GET/PUT `/api/v1/me/board/config` and GET/PUT `/api/v1/me/projects/:id/config`; strict bind (unknown key / malformed value / per-key null / top-level null / empty body → 400; `{}` is the only clear-all).
- `sdks/openapi.yaml`: `/me/board/config` operations (operationIds getUserBoardConfig/setUserBoardConfig), null-rejection documented on the override schema, user-defaults wording throughout.
- Parity matrix: config rows REST-live / MCP-absent BY DESIGN; route scanner extended to the `/me/board` group (regex `[a-z]+\.` — the bb group was never scannable before) and the table now pins search/health/config.
- `api/internal/handlers/board_config_integration_test.go`: the REAL wiring pin — BoardHandler → board.Service → Postgres: user round-trip, project inherit→override→clear loop, later-flip tracking, strict body gate, scoped-404 perimeter, user isolation.

### Review response (CHANGES_REQUESTED → addressed in-turn)

Every bot finding addressed: (1) false unified-constant claim → constant wired + comment corrected; (2) dead defaultWorkerSlots → consumed by CreateProject (integration-pinned); (3) null bypass → rejected at validate, decode, and bind tiers (unit + REST + integration pins); (4) 403-vs-404 spec mismatch → dissolved by the user-scope pivot (no admin route); (5) missing router/DB integration + real-perimeter coverage → TestIntegrationBoardConfig + the R7 service-scoped read; (6) drift fail-closed read → pinned (known key, malformed value, raw SQL → reads error, create fallback errors); (7) R7 scope → resolved by the orchestrator ruling (user-scoped, above); (8) audit deviation → explicit owner deferral (above).

### Review response — round 2 (CHANGES_REQUESTED → addressed in-turn, commit 204dda20)

(1) Empty-actor read fail-open → `GetProjectBoardConfig` rejects an empty actor (GuardAuthz) before any resolution — the service is the guards-once boundary; pinned. (2) Inert per-project defaultWorkerSlots → ruled USER-level: the project surface rejects the key (a project override would render an effective map the engine does not honor); OpenAPI documents the ruling. (3) Drift rendered as 400 → new `StorageDriftError` rides the 5xx arm (the caller did nothing wrong); handler-level 500 pin added; CreateProject propagates it fail-closed. (4) Concurrent REPLACE races → per-scope `pg_advisory_xact_lock` serializes writers (no 23505 PK races, no interleaved-key unions); `TestBoardConfig_ConcurrentReplaceWrites` (8 writers × both scopes) pins it. (5) Unreachable 403 on the project PUT spec → removed (perimeter is 404-by-design). (6) Production-router/e2e + 401 coverage → `TestIntegrationRouterBoardConfig` (server package): REAL NewRouter registration + REAL board.Service over PG; 401 on all four unauthenticated routes; authenticated round-trips; user-level-key 400; scoped 404; CI lane extended in secrets-integration.yml with `-p 1` (the suites share the PG service — parallel packages would collide). (7) Body hardening → 64 KiB MaxBytesReader. (8) Audit-deviation verifiability → orchestrator asked to record the rulings as a PR comment (trail visible to the reviewer).

### Reclaim note

Task reclaimed at 01:54:06Z after a predecessor lease lapse (tooling, not work failure). The predecessor's WIP was reviewed file-by-file and continued: signature drift in `config_integration_test.go` fixed; parity completion finished; then the review fixes above.

## Test Evidence (exact commands + outcomes; final-head full-suite runs recorded in the PR body)

- `go build ./...` → ok; `go vet ./...` + `go vet -tags integration ./pkg/board/ ./api/internal/handlers/` → ok
- `go test -count=1 -run 'TestBoardConfig|TestMergeBoardConfig|TestValidateBoardConfigOverride|TestDecodeBoardConfig' ./pkg/board/` → ok
- `TEST_DATABASE_URL=… go test -tags=integration -count=1 -run 'TestBoardSuite/TestBoardConfig' ./pkg/board/` → ok 3.35s (user round-trip, isolation, merge, create-consumption, validation+authz, cascade, CHECK belt, fail-closed drift)
- `go test -count=1 -run 'TestBoardConfigREST|TestBoardParity' ./api/internal/handlers/` → ok
- `TEST_DATABASE_URL=… go test -tags=integration -count=1 -run 'TestIntegrationBoardConfig' ./api/internal/handlers/` → ok 0.31s (real wiring, 6 subtests)
- `make openapi-validate` → valid
- Migration safety re-run after the user_id pivot: `PG*… make migration-roundtrip migration-idempotent migration-fk-cascade` (local PG 15.19; CI authoritative on 16) → all three passed. `migration-data-cleanup`: script absent at this ref (pre-existing, not-attributable).

## Validated Assumptions

- "Board-wide defaults" = the user's boards (orchestrator ruling per 0067 R7) — user_id-scoped storage, /me routes, no admin tier.
- Foreign-actor project reads AND writes render the scoped 404 (resolveProject user-scoping → NotFoundError) — both pinned.
- Config writes carry no requestId/board_events seam (replace-semantics idempotency) — pinned via replayExceptions; explicit owner deferral on audit rows.
- `users.id` is text and pre-seeded by the harnesses (FK target verified in 000001 + harness inserts).

## Residual Flags

- MCP tools for board config are future work (parity rows pin the absence by design; PR-A2 consumes the REST surface).
- Instance tier (instance > user > project): deliberate future work (ruling; migration header).
- Config-write audit events: deferred by owner decision; revisit on provenance needs.
- `make migration-data-cleanup` references a missing script at this ref (not-attributable).
- Local migration-safety ran on PG 15.19 (host pg_dump constraint); CI runs postgres:16.
- CI "Origin plugin freeze pin" + "Test (full suite, race)" also fail on main (run 39711266972) — not-attributable; heal rides #1819.

## Mutation-verify Suggestions

- Drop `requireOwner`/scoped resolution in `SetProjectBoardConfig`/`GetProjectBoardConfig` → the foreign-actor scoped-404 pins go red.
- Make `DecodeBoardConfigOverride` skip unknown keys instead of rejecting → `TestDecodeBoardConfigOverride/unknown_key` + the REST 400 pins go red.
- Remove the merge layering in `GetProjectBoardConfig` → `TestBoardConfig_ProjectOverrideMerge` later-flip pin goes red.
- Revert the CreateProject slots fallback to a hardcoded 2 → `TestBoardConfig_DefaultWorkerSlotsFeedsCreate` goes red.
- Drop the null guard in `ValidateBoardConfigKey` → the null-value pins (unit + REST + integration) go red.
- Relax the 000041 key CHECK → migration roundtrip snapshot diff fires.
