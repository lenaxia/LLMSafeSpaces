# Worklog: Image Factory Auto Base Sync + Hash UX

**Date:** 2026-08-27
**Session:** Investigate the "weird bookworm 0.21.2" image-factory state post-PR-1086; land auto-latest base tracking, hash display/re-selection, and platform-release labeling
**Status:** Complete

---

## Objective

Three user asks after discovering the image-factory catalog was stale (default base `bookworm 0.21.2` while the platform ran 0.23.0):

1. The catalog's default base must track the latest platform runtime automatically — no operator action. Existing builds/configs must stay immutable.
2. Existing images must show their schematic hash, and the create form must accept a pasted hash to re-select the same options.
3. The version shown next to a base name reads as the Debian codename's own version — label it as what it is: the platform release.

---

## Work Completed

### Investigation findings (context for the changes)

- Live catalog (`image_factory_bases`): `bookworm 0.6.0` (below `MinBaseVersion=0.15.7` floor), `bookworm 0.8.0` (seed row, below floor), `bookworm 0.21.2` (default, digest-pinned, created 2026-08-26 12:14 UTC by admin upsert during the sidecar-flip train). No 0.23.0 row exists even though `base:0.23.0` is in ghcr (`sha256:3bdf41…`, verified anonymously) and the cluster runs platform 0.23.0.
- Root cause of "builds only go to 0.21.2": (a) no 0.23.0 catalog row, and (b) `createConfigAtScope` resolves an unpinned `baseVersion` to the `IsDefault` row, not the latest (`api/internal/handlers/imagefactory_create.go`).
- Root cause of staleness: base rows enter only via the boot seed (which only ever upserts 0.8.0 from `catalog.seed.yaml`) or the admin API. Nothing reconciles the catalog with the release train — the same class as the 2026-08-25 incident (worklog 0821: "catalog row was 5 weeks stale; nothing reconciled it").
- Platform-dependency answer: in sidecar mode agentd is injected from `controller.agentdDelivery.image`, but the base still ships platform-owned content (redact, entrypoints, mise wiring/PATH — PR #1086 changed exactly that, opencode pin). The base tag tracks the platform release because base content is released on the platform train; the coupling shrinks at migration step 5 but never reaches zero.

### Auto base sync (`api/internal/imagefactory/base_sync.go`, `registry.go`)

- `ComputeBaseSync(BaseSyncInput) *Base` — pure decision: normalize the deployed API version (`v`-prefix stripped, `unknown`/garbage → no-op), floor-guard against `MinBaseVersion`, derive target name/image from the current default, no-op when the catalog is at/ahead or no default exists (operator-deleted defaults are never fought). Returns the row to upsert with `IsDefault=true`.
- `HTTPManifestResolver` — anonymous pull token + manifest HEAD (OCI index/Docker list Accepts); returns `docker-content-digest`. Derives the registry host from the image ref's first path segment when host-like; ghcr.io default.
- `SyncBaseOnce` — one pass: compute → digest-resolve (this is also the existence gate: never catalog a tag GH Actions cannot pull) → `UpsertBase` (transactional default move, #950 semantics).
- Wired in `app.go`: hourly ticker, first pass off the construction path so a 15s registry timeout on an egress-restricted cluster cannot delay API startup. Failure is warn + retry.

### Hash resolution (`GET /image-factory/resolve/:hash`)

- `imagefactory.IsValidHash` (`^s-[0-9a-f]{16}$`) — anchored shape check.
- `Service.ResolveHash` (database) — aggregates builds by hash (`succeeded`/`dispatched` only; failed combos excluded), recovers the selection from the preferred build's frozen `resolved_values` (`ResolvedValues.Selection()`), distinct versions sorted newest-first via exported `CompareVersions`.
- Handler `ResolveHash` — any authenticated user; hashes are content addresses over public catalog extension IDs and builds coalesce across scopes by design. 422 malformed / 404 unknown / 500 store error.
- Route registered; `sdks/openapi.yaml` gained `/image-factory/resolve/{hash}` + `ImageHashResolution` schema (contract test enforces route↔spec parity).

### Frontend (`WorkspaceImagesTab.tsx`, `imageFactory.ts`)

- Expanded config cards now show `Base: bookworm (platform 0.6.0) · N extensions · s-…` — platform-release label plus the schematic hash with a tooltip pointing at the new input.
- Base picker options read `bookworm — platform 0.21.2`; update-pill copy updated to match (`platform 0.9.0 available`).
- "Build from hash" input + Load button (Enter submits): resolves via the new endpoint, prefills the selection (retired extensions dropped with a notice, all-retired aborts like the #928 refresh prefill), pre-targets the newest built version, and leaves the name for the user.

---

## Key Decisions

- **API-side reconciler over release-workflow API calls.** Self-heals already-drifted catalogs on next boot; no workflow credentials or reachability dependency. Registry digest resolution doubles as the existence gate so a catalog row is only added when the image verifiably exists (a branch build stamped 0.99.0 cannot pollute the catalog).
- **Immutability preserved.** Sync only inserts a new `(name, version)` row and moves the default transactionally. Existing configs keep their pin; the base-update pill + explicit re-save remains the only migration path (ruling #29). Verified with the user before implementation.
- **Open hash resolution (any authenticated user).** A hash reveals only catalog extension IDs; `GET /configs/:hash` remains scope-filtered for name/owner data. Matches coalescing semantics ("images are platform-wide artifacts").
- **Selection recovered from builds, not decoded from the hash.** `HashSelection` is one-way SHA-256 by design; the builds table already stores the frozen selection projection.
- **`CompareVersions` exported** from `imagefactory` (was package-private `compareVersions`) — the store layer needs semantic ordering for newest-first version lists.

---

## Blockers

None.

---

## Tests Run

- `go test -timeout 60s -race ./api/internal/imagefactory/` — ok (base_sync, registry, selection hash-shape, floor, updates, dockerfile)
- `go test -timeout 600s -race ./api/internal/services/database/` — ok (ResolveHash happy/dedupe/not-found/query-error/corrupt-JSON)
- `go test -timeout 600s -race ./api/internal/handlers/` — ok (resolve handler 200/422/404/500 + full suite)
- `go test -timeout 900s ./...` — ok (full root module)
- `go test -timeout 300s -race ./api/internal/server/ -run TestOpenAPIRouterContract` — ok
- `cd frontend && npx vitest run src/components/settings/` — 219 passed
- `cd frontend && npx tsc --noEmit` — clean
- `make lint` — 0 issues

---

## Next Steps

- Merge + release train: after deploy, verify the API log line `image factory default base advanced to platform release` fires and the catalog gains `bookworm 0.23.0` (default). The 0.6.0/0.8.0 below-floor rows can be deleted at the operator's leisure (they are unbuildable).
- When 0.24.0 ships (carrying PR #1086's mise fix — no released base has it yet), the reconciler should advance the default automatically; spot-check once.
- Consider ratcheting `MinBaseVersion` at migration step 5 per the sidecar-flip runbook.

---

## Files Modified

- api/internal/imagefactory/base_sync.go (new)
- api/internal/imagefactory/base_sync_test.go (new)
- api/internal/imagefactory/registry.go (new)
- api/internal/imagefactory/registry_test.go (new)
- api/internal/imagefactory/selection.go (IsValidHash)
- api/internal/imagefactory/types.go (HashResolution)
- api/internal/imagefactory/base_updates.go (+ dockerfile.go, tests: compareVersions → CompareVersions export)
- api/internal/services/database/imagefactory.go (ResolveHash + interface)
- api/internal/services/database/imagefactory_test.go (ResolveHash tests)
- api/internal/handlers/imagefactory.go (store interface + ResolveHash)
- api/internal/handlers/imagefactory_resolve.go (new)
- api/internal/handlers/imagefactory_resolve_test.go (new)
- api/internal/handlers/imagefactory_test.go, imagefactory_e2e_test.go (fakes gain ResolveHash)
- api/internal/server/router.go (route)
- api/internal/app/app.go (reconciler wiring + version import)
- sdks/openapi.yaml (resolve path + schema)
- frontend/src/api/imageFactory.ts (HashResolution + resolveHash)
- frontend/src/components/settings/WorkspaceImagesTab.tsx (hash display, labels, load-from-hash)
- frontend/src/components/settings/WorkspaceImagesTab.test.tsx (new tests + pill copy)
- frontend/src/components/settings/WorkspaceImagesTab.scope.test.tsx (#814 dialog scoping)
