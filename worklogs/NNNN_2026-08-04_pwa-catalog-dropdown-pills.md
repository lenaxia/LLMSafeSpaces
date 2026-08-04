# Worklog: PWA autoUpdate, dark mode dropdowns, catalog additions, UI pills

**Date:** 2026-08-04
**Session:** Quick fixes batch addressing 7 issues from user feedback on the image factory UI.
**Status:** Complete

---

## Objective

Fix the admin portal 404 (PWA stale cache), dark mode dropdown readability, add trixie + R/Julia to catalog, add status/scope pills, and make preferences a dropdown.

---

## Work Completed

1. **PWA autoUpdate** — switched `registerType` from `"prompt"` to `"autoUpdate"` in `vite.config.ts`. The stale SW served old chunk hashes that 404 after deploy. AutoUpdate activates immediately on next visit.

2. **Dark mode dropdowns** — global CSS `.dark option { background; color }` in `styles/index.css` + `bg-background text-foreground` on `WorkspaceImagesTab` select.

3. **Catalog additions** — added trixie (Debian 13) base, R + r-devtools, Julia LTS. All existing extensions now support both bookworm and trixie.

4. **Split-button pills** — popup shows Ready (green pill, clickable) + Building (yellow pill, greyed out) sections.

5. **Workspace Images tab** — expandable drawer showing extensions as chips, scope pills (Platform/Org/Personal), dark-mode-safe status pills.

6. **Preferences dropdown** — `preferredRuntime` renders as `<select>` of Ready configs instead of freeform text. Falls back to text input when no configs.

7. **Removed dead TODO buttons** — rename/delete buttons replaced with "coming soon" text (API not built yet).

---

## Key Decisions

1. **autoUpdate over prompt** — eliminates the split-brain stale-cache problem at the cost of a momentary reload. Acceptable for a single-user platform.
2. **"Coming soon" text over TODO buttons** — the review correctly flagged dead buttons shipping to users. The rename/delete API will be built in PR 2.
3. **Global CSS for option dark mode** — browser-native `<option>` elements can't be styled with Tailwind classes; the global rule is the only reliable fix.

---

## Blockers

None.

---

## Tests Run

- `go build ./...` — pass
- `npx tsc --noEmit` — pass
- CI: Lint ✓, Test (full+race) ✓, Test (-short+coverage) ✓, Frontend ✓, review ✓

---

## Next Steps

1. Merge PR #648.
2. PR 2: delete/rename config API + admin portal management.
3. Release + deploy.

---

## Files Modified

- `frontend/vite.config.ts` — autoUpdate
- `frontend/src/styles/index.css` — dark mode option CSS
- `frontend/src/components/settings/WorkspaceImagesTab.tsx` — drawer, pills, select fix
- `frontend/src/components/settings/SettingsForm.tsx` — RuntimeSelect dropdown
- `frontend/src/components/workspace/NewWorkspaceSplitButton.tsx` — Ready/Building pills
- `frontend/src/components/workspace/NewWorkspaceSplitButton.test.tsx` — updated tests
- `api/internal/imagefactory/catalog.seed.yaml` — trixie + R + Julia
