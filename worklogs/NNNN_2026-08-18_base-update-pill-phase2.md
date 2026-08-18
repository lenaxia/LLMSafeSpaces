# Worklog: base-update pill phase 2 — refresh flow + launch-picker pill

**Date:** 2026-08-18
**PR:** (this PR)
**Issue:** #928 (closes the remaining scope)
**Session:** continuation of the v1 work merged in #930
**Status:** phase 2 complete — #928 fully delivered

## Objective

Deliver the action half of the base-update pill: the one-click refresh
flow (prefill → diff review → save new hash; original untouched) and
the launch-picker staleness signal.

## Work Completed

- Refresh flow (WorkspaceImagesTab): "Refresh to <target>" button on
  stale expanded cards; prefills the create form (name, selection, base
  pre-targeted at migration-default or same-name latest); amber banner
  states the contract — same extensions, new base, NEW config on save,
  original untouched and launchable; migration variant adds the
  ruling-29 Debian-suite caveat. Cancel restores the empty form; save
  toast distinguishes refresh from create. The form IS the diff review:
  extensions visibly identical, new base in the select. Explicit
  consent per ruling #29 — nothing auto-migrates.
- Launch picker (NewWorkspaceSplitButton): ↻ pill on stale ready
  configs with a tooltip directing to Settings. Launching stale
  configs stays fully allowed — the frozen image remains valid; the
  pill is informational.
- Defensive: prefill targets only bases present in the catalog
  (retired-base race → error toast, not a silent old-base prefill).

## Assumption validated (Rule 7)

- createConfig flow accepts an explicit baseName/baseVersion pair (the
  form already sends both; the API defaults baseVersion only when
  empty) — the prefill reuses the exact existing save path; no new
  API surface.

## Tests Run

- 3 refresh-flow component tests: prefill (banner + name field
  populated), cancel (empty form restored), fresh-configs-have-no-
  button.
- 1 launch-picker test: ↻ pill present with explanatory title on a
  stale ready config.
- Frontend suites run in CI (sandbox lacks node_modules).

## Blockers

None.

## Next Steps

- None for #928 — issue closes with this PR. Future: SDK surface
  (includes updatesAvailable from day one per the reworded AC).

## Files Modified

frontend/src/components/settings/WorkspaceImagesTab{.tsx,.test.tsx};
frontend/src/components/workspace/NewWorkspaceSplitButton{.tsx,.test.tsx}.
