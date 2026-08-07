# Worklog NNNN — allowed_image_configs policy (design/0047 D3)

**Session:** 2026-08-07
**Status:** Ready for merge
**Scope:** PR #3 of design/0047 — org-level image config restriction policy.

## Objective

Org admins need to restrict which org/platform images members can launch.
Without this, any visible config is launchable. Design/0047 D3 + Q4:
defense-in-depth — filter at listing (picker) AND reject at API (backstop).

## Key Decisions

1. **New policy key `allowed_image_configs`** — `[]string` of config hashes.
   Empty (default) = unrestricted. Non-empty = only listed org/platform
   configs are launchable. Member-scoped configs are always exempt.
2. **Enforcement at `resolveImageFactoryConfig`** — after resolving a config
   at org or platform scope, check the org's policy. If the hash is not in
   the allowed list, reject with a validation error.
3. **Member configs always exempt** — members can always launch their own
   configs regardless of the restriction.
4. **Frontend management via org Settings tab** — toggle + textarea for
   image config hashes, same pattern as model/provider restrictions.

## Work Completed

### Backend
- `pkg/types/orgs_policy.go` — added `PolicyAllowedImageConfigs` key +
  `AllowedImageConfigs` field on `OrgPolicyValues`
- `api/internal/handlers/policies.go` — added key to `isValidKey` +
  `isValidValue` ([]string validation)
- `api/internal/services/workspace/workspace_service.go` — added
  `isImageConfigRestricted` helper + enforcement in
  `resolveImageFactoryConfig` (org/platform scope only, member exempt)

### Frontend
- `frontend/src/components/org-admin/OrgSettingsTab.tsx` — new "Image
  Restrictions" card with toggle + textarea for config hashes

### Tests
- `api/internal/services/workspace/image_config_restriction_test.go` — 5
  tests: blocked, allowed, member exempt, empty policy unrestricted, no
  policy checker

## Tests Run

- `go build ./...` — clean
- `go test -race -short ./api/internal/services/workspace/ ./api/internal/handlers/` — ok
- `npx tsc --noEmit` — clean
- `npx vitest run` — 1544 pass

## Files Modified

- `pkg/types/orgs_policy.go`
- `api/internal/handlers/policies.go`
- `api/internal/services/workspace/workspace_service.go`
- `api/internal/services/workspace/image_config_restriction_test.go` (new)
- `frontend/src/components/org-admin/OrgSettingsTab.tsx`
