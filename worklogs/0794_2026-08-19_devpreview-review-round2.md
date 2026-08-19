# Worklog: #946 round 2 — review remediation (SchemaVersion, app-wiring tests, HTTP round-trip)

**Date:** 2026-08-19
**Session:** Address PR #948 automated-review CHANGES_REQUESTED findings for issue #946.
**Status:** Complete

---

## Objective

Close the four required items from the PR #948 review: (1) testable extraction + tests for the app.go dev-preview read/fallback, (2) HTTP-level admin round-trip + bounds tests, (3) `SchemaVersion` 11→12 with canary twins + TESTPLAN, (4) correct the false "boots NewApp with the new reads" claim in the PR description.

---

## Work Completed

### 1. App-wiring extraction + tests (`api/internal/app/dev_preview_wiring.go` + test)

- Extracted the dev-preview settings read from `app.go` into `devPreviewConfigFromSettings(ctx, instanceSettings, log)` following the `*_wiring_test.go` pattern (`wirePolicyEnforcement` precedent). `app.go` now constructs the handler config through the helper — the changed code is no longer inline-unreachable.
- 4 tests with a local `fakeInstanceStore`: empty store → schema defaults (`true`/52428800/50 — the exact #946 failure mode now pinned); store error → warn + typed-key defaults (NOT the zero value); service-written values honoured end-to-end; out-of-band DB garbage (`-1`, `0` past the validator) clamped to defaults.

### 2. HTTP-level admin tests (`api/internal/handlers/settings_test.go`)

- `TestAdminSettings_SCHEMA_ContainsDevPreviewKeys` — schema endpoint serves all three keys with correct type/category/default/min/max, `readOnly=false` (the admin-UI-switch contract).
- `TestAdminSettings_PUT_DevPreview_RoundTrip` — PUT `devPreview.enabled=false` + `maxConnsPerWorkspace=10` → 200, both visible on `GET /admin/settings`.
- `TestAdminSettings_PUT_DevPreview_BoundsRejected` — 10-case table, both boundary sides: out-of-range (0, -5, 1001, 1023, 1073741825, wrong type) → 400; boundary values (1, 1000, 1024, 1073741824) → 200. Pins the new Min/Max policy the review flagged as unpinned.

### 3. SchemaVersion 11→12 (+ lockstep surfaces)

- `pkg/settings/schema.go` — version-history comment entry (same class as v10: added keys) + `const SchemaVersion = 12`.
- Canary twins in lockstep per `TestCanary_SchemaVersion_TwinParity`: `sdks/canary/go/scenarios/s-user-settings/main.go`, `sdks/canary/typescript/scenarios/s-user-settings.ts`, `sdks/canary/python/scenarios/s_user_settings.py`, `sdks/canary/TESTPLAN.md:261`.

### 4. PR description correction

- The round-1 claim "`go test ./api/internal/app/` boots `NewApp` with the new reads" was false — `New()` aborts at `validateMasterSecret`/`kubernetes.New` long before the dev-preview block (reviewer measured 1.8% New coverage). Corrected in the PR body; noted here per the append-only worklog rule.

---

## Key Decisions

1. **Helper takes `pkginterfaces.LoggerInterface`** (not `*logger.Logger`) so the wiring test reuses `nopLogger` — same shape as `wirePolicyEnforcement`.
2. **Local `fakeInstanceStore` in the app test** — pkg/settings' mock is unexported; cross-package reuse would require exporting it (broader change than warranted).
3. **Schema-default test asserts float64-normalized JSON defaults** in the HTTP schema test (JSON numbers unmarshal as float64 — comparing against `int` silently fails).

---

## Blockers

None. The `SDK Integration (live API canaries)` CI failure (`/livez: not rate-limited`) was rerun-requested; the round-1 occurrence is consistent with this workspace pod's env contamination (the same env that fails 2 handlers e2e tests locally — both pass with `env -u INFERENCE_RELAY_BASEURL`, matching CI where the full suite is green).

---

## Tests Run

- `go test -run "TestDevPreviewConfigFromSettings|TestAdminSettings_SCHEMA_ContainsDevPreviewKeys|TestAdminSettings_PUT_DevPreview|TestCanary_SchemaVersion_TwinParity|TestInstanceSettings_DevPreviewKeys|TestInstanceService_DevPreview" ./api/internal/app/ ./api/internal/handlers/ ./pkg/repolint/ ./pkg/settings/` — all ok
- `env -u INFERENCE_RELAY_BASEURL go test -timeout 600s -race -count=1 ./api/internal/handlers/ ./api/internal/app/ ./pkg/settings/ ./pkg/repolint/` — all ok (196s handlers, incl. the 2 previously env-failing e2e tests)
- `gofmt`/`goimports` — clean; `golangci-lint run` on touched packages — 0 issues

---

## Next Steps

- Await re-review; merge on APPROVE; post-merge the bot assigns the real worklog numbers.

---

## Files Modified

- `api/internal/app/dev_preview_wiring.go` (new) — extracted helper
- `api/internal/app/dev_preview_wiring_test.go` (new) — 4 wiring tests
- `api/internal/app/app.go` — dev-preview block calls the helper
- `api/internal/handlers/settings_test.go` — 3 HTTP-level test funcs (+ `ptr` helper)
- `pkg/settings/schema.go` — SchemaVersion 12 + comment entry
- `sdks/canary/go/scenarios/s-user-settings/main.go`, `sdks/canary/typescript/scenarios/s-user-settings.ts`, `sdks/canary/python/scenarios/s_user_settings.py`, `sdks/canary/TESTPLAN.md` — version twins
- `worklogs/0794_2026-08-19_devpreview-review-round2.md` — this worklog
