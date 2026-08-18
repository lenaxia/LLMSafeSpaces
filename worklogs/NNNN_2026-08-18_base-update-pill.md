# Worklog: base-update pill — read-side signal (#928 v1)

**Date:** 2026-08-18
**PR:** (this PR)
**Issue:** #928 (v1 scope: computation + API + pill; refresh flow deferred)
**Session:** image-factory option-E implementation, phase 1
**Status:** v1 complete; refresh flow (prefill/diff/save) is phase 2

## Objective

Surface base staleness on image-factory configs: the pill payload that
tells a user their config's base has a newer version or that the
platform default base moved (per ruling #29, the sanctioned
version-migration path).

## Work Completed

- `imagefactory.ComputeBaseUpdates` (pure): version_bump / base_migration
  / nil-fresh semantics; migration outranks bump (sanctioned path);
  retired base name → nil (nothing to suggest); numeric-semantic version
  compare (0.10.0 > 0.9.0); no downgrade suggestions.
- Config type gains `updatesAvailable` (computed-on-read, omitempty,
  never stored). Handler enrichment via ONE ListBases per request for
  any config count; advisory only — catalog-read failure logs-and-skips,
  never fails the read path. Wired into GET /configs (list) and
  GET /configs/:hash (decode).
- Frontend: amber pill beside the status pill in WorkspaceImagesTab —
  two variants (migration names the new default base; bump names the
  version), tooltip explains the ruling-29 migration semantics. API
  client type added.
- SDK: no image-factory surface exists in any SDK yet — the issue's SDK
  AC is adjusted to "when an SDK surface lands, include the field".

## Assumption validated (Rule 7)

- Versions in image_factory_bases are dot-numeric ("0.6.0" seen in seed
  and tests); non-numeric segments tolerated lexically.
- ListBases is cheap and already on every handler's critical path
  (catalog/create) — one extra read per configs-list request is noise.

## Tests Run

- 7 pure ComputeBaseUpdates table tests (fresh/bump/migration/
  retired/semver/no-default/no-downgrade) — all pass.
- 2 handler e2e tests (list + decode enrichment, fresh field omitted)
  — pass; caught a real copy-semantics bug during development
  (slice-literal boxing didn't write back; fixed with box-and-read-back).
- Frontend: 3 pill render tests added (migration/bump/absent) —
  vitest runs in CI (no node_modules in this sandbox).
- Full api handlers + imagefactory suites green.

## Blockers

None. Frontend typecheck pending CI (sandbox lacks node_modules).

## Next Steps

- #928 phase 2: refresh flow (pre-filled picker from resolved_values,
  diff preview, save → new hash; old config untouched).
- Canary assertion when an SDK image-factory surface exists.

## Files Modified

api/internal/imagefactory/base_updates{,_test}.go, types.go;
api/internal/handlers/imagefactory.go, imagefactory_e2e_test.go;
frontend/src/api/imageFactory.ts;
frontend/src/components/settings/WorkspaceImagesTab{.tsx,.test.tsx}.
