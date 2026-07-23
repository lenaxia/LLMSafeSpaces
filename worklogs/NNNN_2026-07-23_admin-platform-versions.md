# Worklog: Platform Versions display in admin portal

**Date:** 2026-07-23
**Session:** Add a "Versions" tab to the platform admin portal showing the running version of every platform component.
**Status:** Complete

---

## Objective

Admins had no in-product visibility of which platform version was running across components. Add a read-only "Versions" display to the admin portal covering API, Controller, Frontend, Relay Router, and Base Runtime.

---

## Work Completed

### Design decision — read deployed image tags, not self-reported versions

Research (`#587` explore) found each component currently version-isolates with no aggregation point, and several have no version channel to the API at all (controller's ldflags are dead code; frontend has no version injection; relay-proxy runs on ephemeral VMs). Rather than modify every component to self-report, the handler reads **deployed Deployment image tags** — the most truthful "what is running" signal. Every Deployment already carries `app.kubernetes.io/component` + a container image tagged with semver at release time. This needs no per-component code changes.

### Backend

- `api/internal/handlers/platform_info.go` — `PlatformInfoHandler.GetPlatformInfo` (`GET /api/v1/admin/platform-info`, behind AuthMiddleware + AdminGuard):
  - API version: `pkg/version.Version` (in-process, set via ldflags).
  - Controller/Frontend/Relay Router: list Deployments in the release namespace by `app.kubernetes.io/name=llmsafespaces` label, parse first container image tag.
  - Base Runtime: `workspace.defaultImage` instance setting → parse image tag.
  - Degrades gracefully: K8s list / settings errors return 200 with partial data (never 500), logged at Warn via injected logger.
- `parseImageTag` pure function (handles registries-with-ports `registry:5000/repo:tag` and digest pins `repo@sha256:...`); unit-tested.
- `SetLogger` wired in `app.go` (initially missed — caught by review).
- RBAC: new Role granting the API `get,list` on `deployments` in the release namespace (the existing relay-router Role was name-scoped; `list` cannot be resourceName-scoped in K8s RBAC).
- Wired via `RouterConfig.PlatformInfoHandler` + route registration in `router.go`.

### Frontend

- New lazy **"Versions" tab** (`PlatformVersionsTab`) — table of component → version, loading spinner, error/retry.
- `platformInfo` API client + types.
- Nav item added to `PlatformAdminLayout`; route wired in `router.tsx` (lazy chunk, consistent with other admin sections).

---

## Key Decisions

1. **Image tags over self-reporting.** Reading deployed Deployment image tags is the most truthful signal and requires zero per-component changes. The alternative (each component pushes its version to the API) would be a much larger change touching controller/frontend/relay-router, with latent bugs (controller's local `version` var is disconnected from `pkg/version.Version`).

2. **Label-based discovery** (`app.kubernetes.io/name=llmsafespaces`), not name-based lookup. Avoids the Helm fullname-prefix problem entirely — works regardless of release name.

3. **Graceful degradation** (200 with partial data, not 500). An admin checking versions shouldn't be blocked by a transient K8s API error; they should see what's available.

---

## Blockers

None.

---

## Tests Run

```
# Backend — handler unit + integration + router-level AdminGuard
go test ./api/internal/handlers/ -run "TestPlatformInfo|TestParseImageTag" -v   # PASS
go test ./api/internal/server/ -run "TestPlatformInfoRoute" -v                 # PASS (AdminGuard blocks non-admin 404, allows admin 200)
go test ./api/internal/handlers/... ./api/internal/server/... -count=1         # all PASS

# Frontend
cd frontend && npx vitest run src/components/settings/PlatformVersionsTab.test.tsx   # 4/4
npx tsc --noEmit && npx eslint <changed>                                            # clean

# E2E (Playwright) — runs in CI (Frontend check); can't launch chromium in sandbox.
```

Test levels covered: unit (parseImageTag 7 cases + handler 3 happy/edge + 2 error-path), integration (router→handler HTTP wiring), router-level (real NewRouter + AdminGuard 404/200), e2e (Playwright happy + unhappy).

---

## Next Steps

1. After merge + next release cut, bump talos-ops-prod to ship to the cluster.
2. Optional follow-up: surface relay-proxy version (hardest — runs on ephemeral VMs; would track via `InferenceRelay` CR artifact config).
3. Optional: fix the controller's dead `version` var (logs always `dev`) — separate PR.

---

## Files Modified

- `api/internal/handlers/platform_info.go` — NEW. Handler + `parseImageTag` + logger.
- `api/internal/handlers/platform_info_test.go` — NEW. Unit + error-path + integration tests.
- `api/internal/server/router.go` — RouterConfig field + route registration.
- `api/internal/server/router_admin_platform_info_test.go` — NEW. Router-level AdminGuard test.
- `api/internal/app/app.go` — handler construction + `SetLogger`.
- `helm/templates/rbac.yaml` — new Role for `get,list` deployments.
- `frontend/src/api/platformInfo.ts` — NEW. API client + types.
- `frontend/src/components/settings/PlatformVersionsTab.tsx` — NEW. Tab component.
- `frontend/src/components/settings/PlatformVersionsTab.test.tsx` — NEW. Component tests.
- `frontend/src/components/platform-admin/PlatformAdminLayout.tsx` — nav item.
- `frontend/src/router.tsx` — lazy route.
- `frontend/tests/e2e/platform-versions.spec.ts` — NEW. Playwright e2e.
