# Worklog: Board config backend — global defaults + per-project overrides (#1832 item 6, part 1/2)

**Date:** 2026-10-21
**Session:** Implement #1832 item 6 part 1: the Task Board config storage + API — a global defaults map plus an optional per-project override map, merged at READ time (unset key in override = inherit global default; never-written global key = code default). Keys reviewColumn (off|manual|auto, default manual), autoAssignReviewer (bool, default false), defaultWorkerSlots (int > 0, default 2). Read/write API for global defaults (admin-guarded), per-project overrides (owner-scoped), and the effective merged config. Unknown keys and malformed values rejected at every belt. No UI (PR-A2), no review automation (PR-B1/B2).
**Status:** Complete

---

## Objective

Ship PR-A1 of the #1832 slice set: migration 000041 (both tables, key CHECK constraints as the physical belt, FK cascade on project delete), the pkg/board config model (typed override, per-key validation, replace-semantics writes, read-time merge), the service seam (fail-closed decode of stored rows, R7/#1747 guards on project writes), the REST projection (GET/PUT /api/v1/me/projects/:id/config; GET/PUT /api/v1/admin/board/config behind AuthMiddleware+AdminGuard), and the OpenAPI contract — red-first throughout.

## Work Completed

### Red-first pins (commit 4bb9c9f5)

- `pkg/board/config_test.go` (216 lines): merge semantics (override-wins, unset-inherits, nil/empty overlays), per-key validation (enum membership, bool typing, int>0, unknown-key naming the known set), decode gate, code defaults.
- `pkg/board/config_integration_test.go` (222 lines): Postgres round-trips for global + per-project replace-writes, unset=inherit across a later global flip (read-time merge, never materialized), cascade on project delete, service-belt rejection, foreign-actor scoped-404 (the no-authz-discipline), unknown project 404.
- `api/internal/handlers/board_config_test.go` (241 lines): REST round-trip, strict body gate (unknown key 400 naming known set, malformed value 400 naming key), adapter actor stamping, fake-backed route mounts.
- RED demonstrated at 4bb9c9f5 as build-failure undefineds (the commit message carries the exact commands).

### Implementation

- `pkg/board/config.go`: key vocabulary + typed `BoardConfig`/`BoardConfigOverride`/`ProjectBoardConfig`; `DefaultBoardConfig`; `ValidateBoardConfigKey` (GuardInput class); `DecodeBoardConfigOverride` (nothing outside the vocabulary rides the seam); `ValidateBoardConfigOverride` (service belt); per-key jsonb rows (instance_settings shape); REPLACE-semantics store writers in one tx each; service readers merge code defaults → global map → project overrides at read time. Unknown key surfacing from storage is an error, never silently ignored (fail-closed read).
- `api/migrations/000041_board_config.up/down.sql` (+ helm mirror via the byte-identical convention, `make chart-sync-migrations` shape): `board_config_defaults` (PK key, CHECK key vocabulary) and `board_project_config_overrides` (PK project_id+key, CHECK, FK → projects ON DELETE CASCADE). No board_events vocabulary: config writes are replace-semantics — naturally idempotent, no event writer (migration header documents the ruling).
- `api/internal/handlers/board_config.go` + `board.go` boardManager interface rows + `router.go`: owner routes under /api/v1/me/projects/:id/config; admin routes under /api/v1/admin/board/config (AdminGuard — the /admin/settings instance-scope discipline).
- `sdks/openapi.yaml`: BoardConfig / BoardConfigOverride (additionalProperties: false) / BoardProjectConfig schemas + the four operations with REPLACE-semantics descriptions.
- Parity matrix rows (board_parity_test.go): config rows declared REST-live / MCP-absent BY DESIGN (UI-facing surface, PR-A2 consumes it); config GETs in boardReadActions; config PUTs in replayExceptions (no requestId seam — idempotent by construction); REST route table extended.
- `DefaultBoardWorkerSlots = 2` unified constant (create-project fallback, 000036 column default, config code default).

### Reclaim note

Task reclaimed at 01:54:06Z after a predecessor lease lapse (tooling, not work failure). The predecessor's uncommitted WIP was reviewed file-by-file and continued: signature drift in `config_integration_test.go` (six 2-arg `SetGlobalBoardConfig` calls vs the implemented actor-taking seam) fixed; parity matrix completion (read-actions/replay-exceptions/fixture admin-guard userID stand-in) finished to green.

## Test Evidence (final head, exact commands + outcomes)

- `go build ./...` → ok
- `go test ./pkg/board/ -run 'TestBoardConfig|TestMergeBoardConfig|TestValidateBoardConfigKey|TestDecodeBoardConfigOverride' -count=1 -v` → PASS (9 subtests)
- `go vet -tags integration ./pkg/board/` → ok
- `TEST_DATABASE_URL='postgres://…@127.0.0.1:5433/llmsafespaces_test?sslmode=disable' go test -timeout 300s -race -tags=integration -count=1 ./pkg/board/...` → ok 29.126s (local PG 16.15, migrations 000001→000041 applied in order)
- `go test -timeout 480s -race -count=1 ./api/internal/handlers/` → ok 133.902s (includes the four new config rows in IdempotentReplay/AuditRowInputs/RestRouteTable + TestBoardConfigREST)
- `go test -count=1 ./api/internal/server/... ./pkg/board/` → ok
- `TEST_DATABASE_URL=… go test -timeout 300s -race -tags=integration -count=1 -run 'TestIntegrationBoardWorkerSemantics' ./api/internal/handlers/` → ok 2.085s (the CI kill-test seam unharmed)
- `make openapi-validate` → valid
- `PGHOST=… make migration-roundtrip migration-idempotent migration-fk-cascade` (local PG 15.19; CI's migration-safety workflow remains authoritative on postgres:16) → all three passed. `migration-data-cleanup` not run: `hack/migration-data-cleanup.sh` absent at this ref (pre-existing target rot, not this branch).

## Validated Assumptions

- AdminGuard placement on the router mirrors `/admin/settings` (instance scope) — verified against existing admin groups in router.go.
- Foreign-actor project writes render the scoped 404 (`resolveProject` user-scoping → NotFoundError), not an authz disclosure — pinned accordingly, matching the UpdateProject discipline.
- Config writes carry no requestId/board_events seam (replace-semantics idempotency) — pinned via replayExceptions; migration header documents why board_events deliberately has no config vocabulary.

## Residual Flags

- MCP tools for board config are future work (parity rows pin the absence by design; PR-A2's UI consumes the REST surface first).
- `make migration-data-cleanup` references a missing script at this ref (not-attributable to this branch).
- Local migration-safety ran on PG 15.19 (host pg_dump constraint); CI runs postgres:16.

## Mutation-verify Suggestions

- Revert `CHECK` constraint in 000041 up → `TestBoardConfig_GlobalRoundTrip` storage belt pin + fk-cascade unaffected, but roundtrip snapshot diff fires.
- Drop the `requireOwner` call in `SetProjectBoardConfig` → `TestBoardConfig_ValidationAndAuthz` foreign-actor pin goes red.
- Make `DecodeBoardConfigOverride` skip unknown keys instead of rejecting → `TestDecodeBoardConfigOverride/unknown_key` + REST 400 pins go red.
- Remove the merge layering in `GetProjectBoardConfig` → `TestBoardConfig_ProjectOverrideMerge` global-flip re-inheritance pin goes red.
