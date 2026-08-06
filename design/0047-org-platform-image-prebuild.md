# Design 0047 — Org/Platform-Scoped Image Prebuild & Org Image Restriction

**Status:** Decisions finalized
**Issue:** #659
**Date:** 2026-08-06

## Problem

The image factory supports three scopes (`member`, `org`, `platform`) in
its data model, read paths (`ListVisibleConfigs`, `resolveConfigByHash`,
`resolveImageFactoryConfig`), and launch hierarchy. But **creation is
member-only** — `CreateConfig` hardcodes `Scope: ScopeMember` at both call
sites (`imagefactory_create.go:188` coalesce path, `:254` dispatch path).
Org admins and platform admins cannot pre-build images at their scope.

Additionally, the platform admin portal has **no image-factory UI** — the
backend catalog endpoints (`/admin/image-factory/bases`,
`/extensions`, `/known-failures`) exist but are unreachable from the
frontend.

## Architecture (current state, verified)

### Data flow: config creation → build → launch

```
User clicks "Create" in WorkspaceImagesTab
  → POST /api/v1/image-factory/configs (CreateConfig handler)
    → validate request (name, selection, base)
    → resolve extensions, compute hash
    → check known_failures (permanent block)
    → check coalescing (existing in-flight/succeeded build?)
      → YES: link config to existing build, return 201
      → NO:  dispatch to GH Actions (BEFORE DB commit)
        → CreateConfigAndBuild in a single tx (config + build row)
        → return 201

GH Actions image-build.yml runs
  → docker buildx build → push to ghcr.io/lenaxia/llmsafespaces-images/ws:{hash}-{base_version}
  → POST /internal/image-factory/builds/:id/callback (succeeded|failed)
    → transitions build + config status atomically

User clicks "Launch" in NewWorkspaceSplitButton
  → POST /api/v1/workspaces (workspace_service.go)
    → resolveImageFactoryConfig(hash, userID, orgID)
      → search scopes: member → org → platform (first ready match wins)
      → returns image_ref from the build row
    → sets workspace image to the resolved ref
```

### Scope invariant (application-enforced, NOT DB-enforced)

| Scope | owner_id | org_id | Created by |
|---|---|---|---|
| `member` | user's UUID | NULL | the user (CreateConfig) |
| `org` | NULL | org's UUID | **nobody (impossible today)** |
| `platform` | NULL | NULL | **nobody (impossible today)** |

The migration (`000013`) has CHECK constraints on the `scope` and `status`
enums but **no constraint linking scope to owner_id/org_id presence**. The
invariant is upheld by application code only.

### Key files

| File | Role |
|---|---|
| `api/internal/handlers/imagefactory_create.go` | CreateConfig handler (hardcodes ScopeMember at :188, :254) |
| `api/internal/handlers/imagefactory_manage.go` | Delete/Rename (rejects non-member scope at :79, :114) |
| `api/internal/handlers/imagefactory.go` | ListConfigs, GetConfig, resolveConfigByHash |
| `api/internal/handlers/imagefactory_admin.go` | Catalog CRUD (bases, extensions, known-failures) |
| `api/internal/handlers/imagefactory_dispatcher.go` | GH Actions dispatch (GitHub App auth, POST workflow_dispatch) |
| `api/internal/services/database/imagefactory.go` | Store layer (CreateConfigAndBuild, ListVisibleConfigs, etc.) |
| `api/internal/imagefactory/types.go` | Config, ConfigScope, ConfigStatus types |
| `api/internal/services/workspace/workspace_service.go:1296-1346` | resolveImageFactoryConfig (launch-time scope search) |
| `api/migrations/000013_image_factory.up.sql` | Schema (scope/owner_id/org_id columns, partial unique indexes) |
| `.github/workflows/image-build.yml` | GH Actions build workflow (receives dispatch, builds, callbacks) |
| `frontend/src/components/settings/WorkspaceImagesTab.tsx` | User image config UI (create/delete/rename, scope pills) |
| `frontend/src/api/imageFactory.ts` | API client (consumer endpoints only, no admin methods) |

## Design

### D1. Parameterize CreateConfig scope

**Goal:** let the same create logic run at any scope without duplicating
the 200-line handler.

**Approach:** extract a `createConfig` helper that accepts a `scope`
parameter (plus `ownerID`/`orgID` derived from the scope). The existing
`CreateConfig` handler calls it with `ScopeMember`. Two new handlers call
it with `ScopeOrg`/`ScopePlatform`.

```go
// createConfigAtScope is the shared create logic. scope determines which
// owner/org IDs are set on the Config. All validation, coalescing,
// known-failure checking, and dispatch logic is identical regardless of
// scope — the build is the same image regardless of who initiated it.
func (h *ImageFactoryHandler) createConfigAtScope(
    c *gin.Context, scope imagefactory.ConfigScope, ownerID, orgID *string,
) {
    // ... existing CreateConfig body, but:
    //   Scope:   scope,         (was: ScopeMember)
    //   OwnerID: ownerID,       (was: &userID)
    //   OrgID:   orgID,         (was: nil)
}
```

**New routes:**

| Route | Guard | Scope | OwnerID | OrgID |
|---|---|---|---|---|
| `POST /api/v1/image-factory/configs` | AuthMiddleware | `member` | caller's userID | nil |
| `POST /api/v1/orgs/:id/image-factory/configs` | OrgAdminGuard | `org` | nil | `:id` from URL |
| `POST /api/v1/admin/image-factory/configs` | AdminGuard | `platform` | nil | nil |

The `ImageFactoryHandler` is wired behind all three guards — the guard
middleware determines authorization, the handler parameterizes scope.

**Why not a separate handler?** The logic is identical — same validation,
same coalesce, same dispatch, same build. Duplicating it would drift.
The scope only affects which fields are set on the `Config` struct.

### D2. Extend Delete/Rename ownership checks

**Current:** `DeleteConfig` and `RenameConfig` hardcode `scope != ScopeMember
→ 403`. Org/platform configs cannot be deleted or renamed by anyone.

**Change:** the handler needs to know the caller's role to determine
which scopes they can mutate:

| Caller role | Can delete/rename |
|---|---|
| Regular member | `member` (own configs only) |
| Org admin | `member` (own) + `org` (their org's) |
| Platform admin | `member` (own) + `org` (any) + `platform` (all) |

**Approach:** `resolveConfigByHash` already returns the resolved scope.
Add a role check: if the config is `org` scope, verify the caller is an
org admin for the config's org. If `platform`, verify the caller is a
platform admin. The existing member-scope path (ownership via
`ownerArg = &uid`) stays unchanged.

### D3. Org image restriction policy

**New policy key:** `allowed_image_configs` — a `[]string` of config hashes.

**Semantics:**
- **Empty (default):** unrestricted — members can launch any visible config (current behavior).
- **Non-empty:** members can only launch workspaces using configs whose hash is in the list. Their own member-scoped configs are always allowed (can't restrict self-service).

**Enforcement point:** `workspace_service.go:resolveImageFactoryConfig`.
After resolving a config, check: if the caller is an org member AND the
org has a non-empty `allowed_image_configs` AND the config is org/platform
scope AND its hash is not in the list → reject with validation error.

Member-scoped configs are exempt — an org admin publishes a curated set,
but members can still build and launch their own.

**Note:** this is deliberately NOT enforced at `ListConfigs` time. Members
can see all visible configs (including ones they can't launch). The
restriction is at launch, not at discovery — matches the existing comment
at `workspace_service.go:1292-1295` ("the published_only policy concerns
which configs are listed in the picker, not which are launchable once
visible"). Reconsider if the UX is confusing.

### D4. Platform admin Image Factory tab

**New nav item** in `PlatformAdminLayout`: "Image Factory".

**Two sections:**
1. **Configs** — list all configs (all scopes) with a "Create Platform
   Image" button. Reuses the `WorkspaceImagesTab` component pattern but
   calls `POST /admin/image-factory/configs`.
2. **Catalog** — bases CRUD, extensions publish/retire, known-failures
   management. Calls the existing `/admin/image-factory/*` endpoints.
   Requires a new `adminImageFactoryApi` frontend client.

### D5. Org admin Images tab

**New nav item** in `OrgAdminLayout`: "Images".

Reuses `WorkspaceImagesTab` with org scope — calls
`POST /orgs/:id/image-factory/configs` for creation. The tab already
renders scope pills and handles delete/rename.

## Resolved questions

### Q1. Build cost attribution

**Decision: org-scoped builds billed to the org; platform builds carried by the platform owner.**

Org-scoped builds are attributed to the org's billing (the org admin who
initiated them). Platform-scoped builds are carried by the platform owner
(no per-org billing). This requires the build row or an audit trail to
record the initiating scope so billing can attribute costs correctly.

**Implementation note:** today the build row stores no scope/billing
field. The `dispatchRequest` and `image-build.yml` workflow don't track
who initiated. For the initial PR we can record `scope` + `org_id` on
the build row for audit purposes; full billing integration is a separate
effort.

### Q2. Cross-scope coalescing

**Decision: coalesce. Images are shared across the whole platform.**

The same selection+base produces the same hash regardless of scope. If
an org admin requests the same image a member (or another org) already
built, it coalesces onto the existing build — no duplicate build. This
is a feature: images are platform-wide artifacts, not per-scope.

**Future consideration:** org admins may add custom packages (org-specific
extensions). Even then, if two orgs request the same package combination,
it should produce one shared build. The hash is derived from the
selection, not the scope — so identical selections always coalesce.

**No change needed** — `GetInFlightOrSuccessfulBuild` is already
scope-agnostic (checks by hash+baseVersion). Cross-scope coalescing works
today; it just wasn't reachable because only member-scope configs could
be created.

### Q3. Org admin visibility

**Decision: member configs shown separately from org/platform configs.**

The org admin Images tab should NOT mix member configs with org/platform
configs in one flat list. Member configs should be in a separate section
(or hidden behind a toggle) so org admins can focus on the configs they
manage (org + platform) while still having visibility into what members
are building.

**Implementation:** `ListVisibleConfigs` already returns all scopes. The
frontend org Images tab groups by scope: "Org Images" and "Platform
Images" sections first, then a "Member Images" section (or collapsed).

### Q4. Restriction policy UX

**Decision: both — filter at listing AND reject at API.**

When `allowed_image_configs` is non-empty:
- **Frontend (listing):** the launch picker filters out restricted
  org/platform configs so they don't appear as launchable options.
- **API (backstop):** `resolveImageFactoryConfig` in `workspace_service.go`
  rejects any org/platform config whose hash is not in the allowed list.

Member-scoped configs are always exempt (members can always launch their
own configs regardless of the restriction).

### Q5. Admin catalog UI

**Decision: one tabbed page (Configs | Bases | Extensions | Known Failures).**

The platform admin Image Factory page has four tabbed sections:
1. **Configs** — list all configs (all scopes) + create platform-scoped
   images
2. **Bases** — CRUD for base images
3. **Extensions** — publish/retire extensions
4. **Known Failures** — manage known-failure entries

Catalog management is closely related to config creation (admins pick
bases/extensions when creating a config), so one page with tabs is the
right UX.

## PR plan

1. **Backend (D1 + D2):** parameterized scope creation + extended
   ownership checks. New routes wired. Add `scope` + `org_id` to the
   build row for billing attribution (Q1). Integration tests for
   org/platform creation and delete/rename. No frontend.
2. **Frontend (D4 + D5):** platform admin Image Factory tab (4 sections:
   Configs/Bases/Extensions/Known Failures — Q5) + org admin Images tab
   (member configs in separate section — Q3). Depends on #1.
3. **Restriction (D3):** new `allowed_image_configs` policy key +
   enforcement in `workspace_service.go` (API backstop — Q4) + frontend
   listing filter (picker filtering — Q4). Managed via org Settings tab.
   Depends on #1.
4. **E2e:** extend `org-admin.spec.ts` and add platform-admin image
   factory specs. Depends on #2.

Cross-scope coalescing (Q2) requires no code change — it already works
once multi-scope creation is enabled.
