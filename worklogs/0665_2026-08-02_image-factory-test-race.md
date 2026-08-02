# Worklog 0663 — Fix Image Factory test data race (gin.SetMode under t.Parallel)

**Date:** 2026-08-02
**Scope:** Unblock main CI + v0.7.1 release by fixing the `gin.SetMode` data
race in the Image Factory handler tests (introduced by PR #619).

## Summary

PR #619 (Image Factory S4+S5) merged with failing tests. Root cause: the
Image Factory test files combined `t.Parallel()` with per-test
`gin.SetMode(gin.TestMode)` calls. `gin.SetMode` writes a package-global
variable; with tests running in parallel, concurrent writes tripped the race
detector in `Test (-short, with coverage)` and `Test (full suite, race
detector)` — red CI on main since 2026-08-02 07:45.

This blocked the v0.7.1 release gate: the "Wait for CI" step in the Release
workflow refuses to publish while any CI check on the release commit is red.
Consequence: the v0.7.1 container images were never built/pushed, and the
pre-merged talos-ops-prod bump pointed Flux at a tag with no images.

## Fix

`imagefactory_test.go` already had the correct pattern — a package-level
`func init() { gin.SetMode(gin.TestMode) }` that runs once before any test
(and an explanatory comment). The six per-test `gin.SetMode(gin.TestMode)`
calls in the other three files were redundant writes to the same global and
were removed:

- `imagefactory_integration_test.go` (4 sites)
- `imagefactory_callback_test.go` (1 site, in `newCallbackRouter`)
- `imagefactory_create_test.go` (1 site, in `newIFRouterWithDispatcher`)

## Validation

- `go test -race -short -count=1 -run TestIF_ ./api/internal/handlers/` — ok
- `go test -race -short -count=1 ./api/internal/handlers/` — ok (full package)
- Scoped check: the only `_test.go` files in `api/internal/handlers/` that
  combine `t.Parallel()` with `gin.SetMode` were the four Image Factory
  files; the package-level `init()` is the single remaining writer.
