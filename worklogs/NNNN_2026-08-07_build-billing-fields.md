# Worklog NNNN — Build row billing attribution fields (design/0047 Q1)

**Session:** 2026-08-07
**Status:** Ready for merge
**Scope:** Add scope + org_id to image_factory_builds for billing attribution.

## Objective

Design/0047 Q1 decision: org-scoped builds are billed to the org;
platform-scoped builds are carried by the platform owner. The build row
previously had no scope or org_id, making it impossible to attribute
build costs without joining to the config table (which may be deleted
while the build history remains).

## Key Decisions

1. **Mirror the config's scope/org_id onto the build row at creation time.**
   The `createConfigAtScope` handler now sets `Scope` and `OrgID` on the
   `Build` struct, which are persisted in the same `CreateConfigAndBuild`
   transaction.
2. **Both columns nullable** — backward compatible. Existing rows are
   backfilled from their config's scope/org_id via an UPDATE in the
   migration. Coalesced builds (where the build was initiated by a
   different scope) have the initiating scope, not the config's scope.
3. **Billing indexes** — `idx_builds_org` (partial, WHERE org_id IS NOT NULL)
   for "all builds for org X" queries, and `idx_builds_platform` (partial,
   WHERE scope='platform') for "platform builds this month".
4. **scanBuild reads scope as NullString** — existing rows have NULL scope
   until backfilled; the scan treats NULL as empty string (ConfigScope("")).

## Work Completed

### Migration
- `000018_build_billing_fields.up.sql` — ALTER TABLE ADD COLUMN for
  scope + org_id; backfill UPDATE from configs; two partial indexes
- `000018_build_billing_fields.down.sql` — DROP COLUMN + DROP INDEX

### Go types
- `imagefactory/types.go` — added `Scope ConfigScope` and `OrgID *string`
  to the `Build` struct

### Store layer
- `database/imagefactory.go` — `buildColumns` extended with scope, org_id;
  `scanBuild` reads two new NullString columns; `CreateConfigAndBuild`
  INSERT includes the new columns

### Handler
- `imagefactory_create.go` — the `Build` literal in `createConfigAtScope`
  now sets `Scope: scope, OrgID: orgID` (the same values as the config)

### Tests
- Extended 3 existing scope tests to assert the build row carries the
  correct scope/org_id: `TestCreateOrgConfig_SetsOrgScope`,
  `TestCreatePlatformConfig_SetsPlatformScope`,
  `TestCreateConfig_MemberScope_Unchanged`

## Tests Run

- `go build ./...` — clean
- `go vet ./...` — clean
- `gofmt` — clean
- `go test -race -short ./api/internal/handlers/ ./api/internal/services/workspace/ ./api/internal/imagefactory/` — ok
- `npx tsc --noEmit` — clean
- `npx vitest run` — 1547 pass
- `make repolint` — all checks pass

## Files Modified

- `api/migrations/000018_build_billing_fields.up.sql` (new)
- `api/migrations/000018_build_billing_fields.down.sql` (new)
- `helm/migrations/000018_build_billing_fields.up.sql` (new, mirror)
- `helm/migrations/000018_build_billing_fields.down.sql` (new, mirror)
- `api/internal/imagefactory/types.go`
- `api/internal/services/database/imagefactory.go`
- `api/internal/handlers/imagefactory_create.go`
- `api/internal/handlers/imagefactory_scope_test.go`
