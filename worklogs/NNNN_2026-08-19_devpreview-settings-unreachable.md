# Worklog: Dev-preview settings registered in KnownKeys only — kill-switch unreachable (#946)

**Date:** 2026-08-19
**Session:** Fix issue #946 — Epic 66's three dev-preview instance settings were registered in `pkg/settings/registry.go` (`KnownKeys`) but never added to `InstanceSettings()` (`pkg/settings/schema.go`), making them unreachable through every consumer.
**Status:** Complete

---

## Objective

Make `devPreview.enabled` / `devPreview.maxResponseBytes` / `devPreview.maxConnsPerWorkspace` actually configurable: admin-UI visible, boot-readable with a live default, and settable via `PUT /admin/settings/:key`. Stop the boot-time read errors from being silently swallowed in `app.go`.

---

## Work Completed

### Diagnosis (live deployment, v0.15.11)

- Workspace with `spec.networkAccess.devPreview: true` returns `503 {"error":"dev preview is disabled on this instance"}` from `dev_preview.go:116` — the instance kill-switch, before any per-workspace checks.
- Admin settings page renders no Dev Preview section: `AdminSettingsPage` is schema-driven and `InstanceService.Schema()` → `InstanceSettings()` had no devPreview entries.
- `app.go:1015` `dpEnabled, _ := instanceSettings.GetBool(...)`: index miss → `unknown instance setting key` error → swallowed → zero value `false`. Every stock deployment boots with dev preview hard-disabled regardless of DB state.
- No remediation path: `Set()` rejects unknown keys (admin PUT 4xx), direct DB rows are ignored by `get()`, and `SetHelmOverrides` ignores keys not in the schema.
- Root cause confirmed as the same bug class PR #656 review caught for the Epic 64 keys: "registered in KnownKeys only". The regression-test pattern (`TestInstanceSettings_WorkflowTriggerKeys`) existed but was never extended to Epic 66.

### Fix

1. **`pkg/settings/schema.go`** — added the three keys to `InstanceSettings()` (Tier 2, `Category: "Dev Preview"`), defaults mirroring `registry.go` exactly: `true` / `52428800` (50 MiB) / `50`. Bounds: maxResponseBytes 1 KiB–1 GiB; maxConnsPerWorkspace 1–1000.
2. **`api/internal/app/app.go`** — the three `_`-swallowed read errors now log a warning and fall back to the typed-key defaults (`settings.KeyDevPreview*.Default()`), instead of silently disabling the feature with GetBool's zero value. The `Default()` type assertions are pinned by the new schema test.

### Tests (TDD — red verified before implementation)

- `TestInstanceSettings_DevPreviewKeys` (schema_test.go) — follows the `WorkflowTriggerKeys` pattern: index presence (the actual bug), type, tier, KnownKeys agreement, typed-key name + default match, and schema-default match.
- `TestInstanceService_DevPreview_DefaultsAndRoundTrip` (instance_service_test.go) — empty-store defaults serve `true`/`52428800`/`50`; `Set` accepts `devPreview.enabled=false` and `maxConnsPerWorkspace=10` and reads back (the admin PUT path).
- Both failed pre-implementation exactly as the bug predicted; pass post-implementation.

---

## Key Decisions

1. **Schema defaults must mirror `registry.go`** — the typed `Key` constants are the compile-time reference; the test now enforces both directions (typed default == schema default == documented value).
2. **Warn + typed-default fallback in app.go** rather than failing startup: a settings-read error shouldn't take the API down, but it must be visible in logs. This matches the warn-and-fall-through pattern used in `workspace_service.go:1273-1275`.
3. **Bounds (1 KiB–1 GiB, 1–1000)** are new policy (the epic README specifies only "operator-configurable, default 50 MiB / 50"); chosen to bracket the defaults by ~4 orders of magnitude. Flagged for maintainer review in the PR.
4. **Out of scope, filed as follow-up in issue #946**: the boot-time capture means flipping `devPreview.enabled` requires an API restart — weak as a kill-switch in both directions. Per-request reads (the 60s-TTL cache path already exists) belong in a separate change.

---

## Blockers

None.

---

## Tests Run

- `go test -timeout 120s -count=1 ./pkg/settings/` — ok (both new tests red pre-fix, green post-fix)
- `go test -timeout 300s -race -count=1 ./pkg/settings/ ./api/internal/handlers/` — settings ok; handlers has 2 pre-existing env-dependent failures (`TestE2E_BootstrapMaterialize_TokenRejected_StillBoots`, `TestE2E_PasswordReset_FullPurgeThenBoot_NoProviders`) — **verified identical on clean main via git stash**; caused by `INFERENCE_RELAY_BASEURL` being set in this workspace pod's env (relay provider resurrects in the test config). CI (no such env) passes; not a regression.
- `go test -timeout 300s -count=1 ./api/internal/app/` — ok
- `go build ./api/... ./pkg/...` — clean; `go vet ./pkg/settings/ ./api/internal/app/` — clean
- `gofmt -l` on changed dirs — clean; `golangci-lint run ./pkg/settings/... ./api/internal/app/...` — 0 issues

---

## Next Steps

- PR review; if approved, deployment reminder: after upgrade, dev preview is ON by default (`true` schema default) for any workspace that toggles the CRD field — operators who want it off should set `devPreview.enabled=false` via admin settings (now visible in the UI) and restart the API.
- Follow-up (issue #946 secondary): make the kill-switch live (per-request read) so it works as an incident lever without a restart.

---

## Files Modified

- `pkg/settings/schema.go` — 3 new `SettingDef` entries (Dev Preview category)
- `api/internal/app/app.go` — dev-preview settings reads: warn + typed-default fallback instead of swallowed errors
- `pkg/settings/schema_test.go` — `TestInstanceSettings_DevPreviewKeys`
- `pkg/settings/instance_service_test.go` — `TestInstanceService_DevPreview_DefaultsAndRoundTrip`
- `worklogs/NNNN_2026-08-19_devpreview-settings-unreachable.md` — this worklog
