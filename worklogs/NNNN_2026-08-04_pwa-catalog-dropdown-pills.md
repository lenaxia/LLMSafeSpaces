# Worklog: PWA autoUpdate, dark mode dropdowns, catalog additions, UI pills

**Date:** 2026-08-04
**Session:** Quick fixes batch addressing 7 issues from user feedback on the image factory UI.
**Status:** Complete

---

## Objective

Fix the admin portal 404 (PWA stale cache), dark mode dropdown readability, add R/Julia to catalog, add status/scope pills, and make preferences a dropdown.

---

## Work Completed

1. **PWA autoUpdate** — switched `registerType` from `"prompt"` to `"autoUpdate"`. The stale SW served old chunk hashes that 404 after deploy.
2. **Dark mode dropdowns** — global CSS `.dark option { background; color }` + `bg-background text-foreground` on selects.
3. **Catalog additions** — added R + r-devtools (CRAN build deps) and Julia LTS. Trixie (Debian 13) was initially added but removed because the base image doesn't exist (Dockerfile hardcodes bookworm-slim). Deferred to a separate infrastructure PR.
4. **Split-button pills** — popup shows Ready (green, clickable) + Building (yellow, greyed out) sections.
5. **Workspace Images tab** — expandable drawer with extension chips, scope pills (Platform/Org/Personal), dark-mode-safe status pills.
6. **Preferences dropdown** — `preferredRuntime` renders as `<select>` of Ready configs instead of freeform text.
7. **Removed dead TODO buttons** — rename/delete replaced with "coming soon" text.

---

## Key Decisions

1. **autoUpdate over prompt** — eliminates stale-cache split-brain at the cost of a momentary reload.
2. **Trixie deferred** — no base image exists; adding without it would cause build failures.
3. **"Coming soon" text over TODO buttons** — dead buttons ship to users; text is honest.

---

## Tests

- `go build ./...` — pass
- `go test ./internal/imagefactory/` — pass (seed test validates R + Julia)
- `npx tsc --noEmit` — pass
- CI: Lint ✓, Test (full+race) ✓, Frontend ✓, review pending

---

## Files Modified

- `frontend/vite.config.ts` — autoUpdate
- `frontend/src/styles/index.css` — dark mode option CSS
- `frontend/src/components/settings/WorkspaceImagesTab.tsx` — drawer, pills, select fix
- `frontend/src/components/settings/WorkspaceImagesTab.test.tsx` — scope pill + drawer tests
- `frontend/src/components/settings/SettingsForm.tsx` — RuntimeSelect dropdown
- `frontend/src/components/settings/SettingsForm.test.tsx` — RuntimeSelect tests
- `frontend/src/components/workspace/NewWorkspaceSplitButton.tsx` — Ready/Building pills
- `frontend/src/components/workspace/NewWorkspaceSplitButton.test.tsx` — updated tests
- `api/internal/imagefactory/catalog.seed.yaml` — R + Julia (trixie removed)
- `api/internal/imagefactory/seed_test.go` — R + Julia assertions
