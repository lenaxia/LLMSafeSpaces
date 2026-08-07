# Worklog NNNN — Image factory org/platform-scoped config creation (backend)

**Session:** 2026-08-06
**Status:** Ready for merge
**Scope:** Backend PR #1 of design/0047 — parameterize CreateConfig scope,
extend Delete/Rename ownership, wire org/admin routes.

## Objective

The image factory's data model supports three scopes (member/org/platform)
but `CreateConfig` hardcoded `ScopeMember` at both call sites. Org admins
and platform admins could not pre-build images at their scope. Delete/Rename
rejected any non-member scope. This PR adds org-scoped and platform-scoped
config creation and extends ownership checks.

## Key Decisions

1. **Shared helper, not duplicated handler.** Extracted `createConfigAtScope`
   which takes scope + owner/org pointers. The existing `CreateConfig`
   calls it with `ScopeMember`; new `CreateOrgConfig` and
   `CreatePlatformConfig` call it with `ScopeOrg`/`ScopePlatform`. All
   validation, coalescing, dispatch, and commit logic is identical — only
   the scope/owner/org fields on the `Config` struct differ.
2. **Cross-scope coalescing works as-is.** `GetInFlightOrSuccessfulBuild`
   is scope-agnostic (checks by hash+baseVersion). An org config coalesces
   onto a member's build of the same selection — no redundant builds.
3. **canMutateScope for delete/rename.** Extracted a shared authorization
   check: member scope always allowed (ownership verified by
   `resolveConfigByHash` filtering on owner_id); org scope allowed if
   caller is in the config's org; platform scope allowed if caller's role
   is "admin".
4. **Routes mounted behind proper guards.** Org route behind
   `OrgAdminGuard`; admin route behind `AdminGuard`; consumer delete/rename
   behind `AuthMiddleware` only (ownership checked in-handler via
   `canMutateScope`).

## Work Completed

- `imagefactory_create.go` — extracted `createConfigAtScope`; added
  `CreateOrgConfig` and `CreatePlatformConfig` handler methods
- `imagefactory_manage.go` — replaced hardcoded `scope != ScopeMember → 403`
  with `canMutateScope` authorization check
- `router.go` — added `POST /orgs/:id/image-factory/configs` (OrgAdminGuard)
  and `POST /admin/image-factory/configs` (AdminGuard)
- `imagefactory_test.go` — fixed `fakeIFStore.GetConfigByHash` to respect
  scope filter (was ignoring it, returning any hash match)
- `imagefactory_scope_test.go` (new) — 8 tests covering org/platform
  creation, member-scope regression, cross-scope coalescing, org admin
  delete, non-org-member block, platform admin delete, regular user block

## Tests Run

- `go build ./...` — clean
- `go test -race -short ./api/internal/handlers/` — ok (full package)
- `gofmt` — clean

## Blockers

Q1 (scope/org_id on build row for billing attribution) deferred — requires
a migration + store layer changes. Will be a follow-up commit or separate PR.

## Next Steps

- Frontend: org admin Images tab + platform admin Image Factory tab (PR #2)
- Restriction policy: `allowed_image_configs` enforcement (PR #3)
- Build row billing fields (Q1)

## Files Modified

- `api/internal/handlers/imagefactory_create.go` (refactored + new handlers)
- `api/internal/handlers/imagefactory_manage.go` (canMutateScope)
- `api/internal/server/router.go` (new routes)
- `api/internal/handlers/imagefactory_test.go` (fake fix)
- `api/internal/handlers/imagefactory_scope_test.go` (new)
