# Worklog: Org-core CRUD documented (#1088)

**Date:** 2026-08-27
**Session:** Document the 13 org-core routes in sdks/openapi.yaml and remove the last sizeable implOnly allowlist block
**Status:** Complete

---

## Objective

Resolve #1088 option (a): document org-core CRUD, dropping the stale "org admin is a separate UX" exclusion that contradicted the rest of the (fully documented) org surface.

---

## Work Completed

- **7 path entries / 13 operations**: `/orgs` (GET bare-array list, POST create w/ ownerEmail), `/orgs/{id}` (GET member-scope, PUT partial, DELETE 204 soft-delete), `/orgs/{id}/workspaces` (paginated items+pagination), `/orgs/{id}/members` (GET bare-array, POST add-by-userId), `/orgs/{id}/members/{userID}` (PUT role-change w/ three distinct 409s, DELETE w/ last-admin guards), `.../verify` (idempotent), `/orgs/{id}/billing/{checkout,portal}` (Stripe URL responses w/ 503/409 contracts).
- **7 new schemas**: `Organization` (status/planId/subscriptionStatus enums), `OrgResponse` (allOf + userRole/memberCount), `CreateOrgRequest` (name/slug/ownerEmail required, planId defaults enterprise), `UpdateOrgRequest`, `AddOrgMemberRequest`, `ChangeOrgMemberRoleRequest`, `OrgWorkspaceMetadata` — deliberately NOT reusing `WorkspaceListItem` (the org list serializes `WorkspaceMetadata`, which has no `phase`; documented the difference on the schema).
- **implOnly allowlist**: the 13 org-core entries removed, replaced with a note pointing at #1088. The contract test now balances org-core in both directions with zero allowlist entries.
- **Hurl contract case** (`orgs.hurl`): happy paths (list/get-members/workspaces wrappers) + documented error contracts (400 validation, 403 non-member, 409 slug conflict, 503 billing-unconfigured) via `Prefer: code=N`.
- Non-obvious wire facts baked in: create is platform-admin-gated (403); members are added by userId not email; member-add returns `{}` 201; org workspaces list caps limit at 100; billing returns `{url}`.

## Key Decisions

1. `OrgResponse` via `allOf` instead of duplicating Organization fields — single source for the base shape (also used implicitly by admin OrgSummary).
2. New `OrgWorkspaceMetadata` schema rather than stretching `WorkspaceListItem` — the two handler types serialize differently; aliasing them would lie to consumers.
3. Delete-204 not exercised in Hurl (Prism `Prefer` limitation noted in-file); route presence is covered by the contract test.

## Assumptions validated

- All shapes cross-checked against `api/internal/handlers/orgs.go` / `org_billing.go` + `pkg/types/orgs.go` / `workspace.go` by a dedicated exploration pass (file:line evidence), not from memory.

## Blockers

None.

## Tests Run

- `make openapi-validate` ✓
- `TestOpenAPIRouterContract` green BOTH directions with the allowlist entries removed
- Full `api/internal/server` suite ✓; `sdks/validate` ✓
- Full Hurl suite (12 files incl. new `orgs.hurl`) PASS against local Prism (hurl 5.0.1)

## Next Steps

1. PR (stacked on #1089) → review loop → merge → closes #1088.
2. Post-merge branch cleanup.

## Files Modified

- `sdks/openapi.yaml` — 13 operations, 7 schemas
- `api/internal/server/router_openapi_contract_test.go` — −13 allowlist entries
- `sdks/tests/contract/orgs.hurl` — NEW
