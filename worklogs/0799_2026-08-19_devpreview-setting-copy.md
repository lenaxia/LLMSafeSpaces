# Worklog: devPreview.enabled copy fix — label reads as enable-toggle

**Date:** 2026-08-19
**Session:** The admin-UI copy for the Epic 66 kill-switch was ambiguous — "Dev Preview" + "Kill-switch…" description let ON be read as "kill engaged". Rewrite as an explicit enable-toggle.
**Status:** Complete

---

## Objective

Make the instance setting's polarity unmissable: ON = feature available, OFF = 503 platform-wide.

## Work Completed

- `pkg/settings/schema.go` — `devPreview.enabled` Label "Dev Preview" → **"Enable Dev Preview"** (follows the `mcp.allowOrgAdminServers` → "Allow Org-Admin MCP Servers" enable-toggle pattern); Description rewritten to state both directions explicitly: "ON: workspaces that also enable Dev Preview in their own settings can serve live previews… OFF: every preview URL returns 503 platform-wide. Takes effect after an API restart." Dropped the "Kill-switch" phrasing that invited the inverted reading.
- `SchemaVersion` 12 → 13 (property modification of an existing key — same class as the v6 precedent, which bumped "so admin UI and frontend schema caches must refresh to show the new description"). Canary twins + TESTPLAN updated in lockstep.
- HTTP schema test now pins `label` for the three devPreview keys so future copy drift is a deliberate test change, with a comment recording why the label matters.

## Key Decisions

1. **Frontend untouched** — the workspace-level toggle already reads "Enable dev preview tunnel" (checkbox semantics, unambiguous). Only the instance setting was confusing.
2. **Label pinned, description not** — the label is the polarity-bearing UI contract; pinning full prose would make every wording tweak a test edit without adding safety.

## Blockers

None.

## Tests Run

- `go test -run "TestCanary_SchemaVersion_TwinParity|TestAdminSettings_SCHEMA_ContainsDevPreviewKeys|TestInstanceSettings_DevPreviewKeys"` on pkg/repolint, api/internal/handlers, pkg/settings — all ok
- gofmt clean

## Next Steps

- PR review; ride-along into the next patch release (no deployment urgency — purely cosmetic until an admin reads the toggle).

## Files Modified

- `pkg/settings/schema.go` — label/description + SchemaVersion 13 entry
- `api/internal/handlers/settings_test.go` — label pinned in schema test
- `sdks/canary/go/scenarios/s-user-settings/main.go`, `sdks/canary/typescript/scenarios/s-user-settings.ts`, `sdks/canary/python/scenarios/s_user_settings.py`, `sdks/canary/TESTPLAN.md` — version twins
- `worklogs/0799_2026-08-19_devpreview-setting-copy.md` — this worklog
