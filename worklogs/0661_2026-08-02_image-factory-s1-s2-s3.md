# Worklog NNNN — Image Factory: Design + S1-S3

**Date:** 2026-08-02
**Scope:** Design documents (0046, 0047), migration 000013, pure-logic package, DB store, read-only consumer endpoints.

## Summary

Designed and partially implemented a Talos-style image factory for custom
workspace images. Two design docs capture 28 decisions and exact contracts.
Three implementation stories landed: migration + pure logic (S1), DB store
with sqlmock tests (S2), and read-only consumer endpoints (S3).

The core design insight: extensions are immutable-once-published (publish-
new + retire; never edit build fields in place). This mirrors how package
managers and Talos extensions work, and dissolves the snapshot/blocklist/
retry complexity that an earlier draft spent machinery patching.

## Assumptions (stated + validated)

1. **A1 (extension immutability):** Validated against Talos factory model.
   Immutable IDs make content-addressing + blocklist simple by construction.
2. **A2 (apt builds succeed ~99%):** One transient retry inside the GH
   Actions workflow replaces 3-attempt API-level machinery.
3. **A3 (GetUserOrgID location):** Validated at `pg_org_store.go:866` — on
   `*PgOrgStore`, not `*database.Service`. Handler uses a separate
   `orgResolver` interface (ISP fix per reviewer feedback).
4. **A4 (migration numbering):** 000013 is next after 000012. Verified via
   `ls api/migrations/`.
5. **A5 (pq.Array named-slice quirk):** `Selection` is `type Selection
   []string`. `pq.Array` only special-cases `*[]string`, so the scan
   boundary casts `(*[]string)(&c.Selection)`.

## What was built

### Design docs
- `design/0046_2026-08-01_image-factory.md` — 28 decisions, revised through
  two stress-test passes.
- `design/0047_2026-08-02_image-factory-contracts.md` — exact Go types, DB
  row shapes, interface seams, handler contracts, test plan, 10-story
  build order.

### S1 — Migration + pure logic
- `api/migrations/000013_image_factory.up.sql` — 6 tables (platform_config,
  bases, extensions, known_failures, configs, builds) + partial unique
  indexes per scope. Synced to `helm/migrations/`.
- `api/internal/imagefactory/` — `HashSelection`, `ValidateSelection`,
  `ResolveSelection`, `ValidateResolved`, `RenderDockerfile`. All pure,
  deterministic, TDD with happy + unhappy + edge coverage.

### S2 — DB store
- `api/internal/services/database/imagefactory.go` — `ImageFactoryStore`
  interface on `*Service`; 29 methods covering catalog/admin/known-failures/
  configs/builds. Coalescing probe (`GetInFlightOrSuccessfulBuild`).
  `ErrNotFound` sentinel exported for `errors.Is` at the handler boundary.

### S3 — Read-only consumer endpoints
- `api/internal/handlers/imagefactory.go` — `GET /catalog`, `GET /configs`,
  `GET /configs/:hash`. ISP-split store interface (`imageFactoryStore` +
  `orgResolver`). Wired in `app.go` behind `AuthMiddleware`.
- `api/internal/server/router.go` — route registration.

## Review-driven fixes (3 automated review passes)

1. **apt-block `&&` vs `\` bug** — `renderAptBlock` used `&&` separator
   instead of `\` line-continuation. For ≥2 apt packages, only the first
   was installed; the rest were executed as commands. Fixed to mirror the
   mise block pattern. Added `TestRenderDockerfile_AptMultiPkgStructural`
   that asserts every package line ends with `\` and never contains `&&`.
2. **ISP violation** — handler's store interface included `GetUserOrgID`
   (lives on `*PgOrgStore`, not `*database.Service`). Split into
   `imageFactoryStore` + `orgResolver`; handler takes both as constructor
   args.
3. **Unwired handler** — `NewImageFactoryHandler` was never called in
   `app.go`. Added construction after `dbSvc` + `pgOrgStore` are available
   and assignment to `ServerConfig.ImageFactoryHandler`.
4. **`isNotFound` fragile string match** — replaced with `errors.Is(err,
   database.ErrNotFound)`. Exported `ErrNotFound` sentinel.
5. **Dead `var _ = errors.New`** — removed from `imagefactory_test.go`.

## What remains

- **B2 (done):** Real `postgres:16` integration test for the S2 store per
  design/0047 mandate. Delivered as `imagefactory_integration_test.go`
  with `//go:build integration` + `testharness.New(t)`. Covers CRUD per
  table, partial-unique-index enforcement, coalescing probe preference
  ordering, pq.Array round-trip, JSONB file_spec round-trip.
- **S4–S10:** POST /configs + coalescing + dispatch, callback + status
  derivation, failure explainer, admin portal, GH Actions workflow, e2e,
  frontend.
