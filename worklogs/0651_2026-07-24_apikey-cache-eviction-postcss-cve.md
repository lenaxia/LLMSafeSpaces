# Worklog: Fix two pre-existing CI failures — API key cache eviction + postcss CVE

**Date:** 2026-07-24
**Session:** Fix the two unrelated pre-existing CI failures surfaced on PRs #595 and #596: (1) `deleted-key: expected 401` SDK canary, (2) `Trivy: postcss GHSA-r28c-9q8g-f849`.
**Status:** Complete

---

## Objective

Both failures were blocking the `SDK Integration (live API canaries)` and `Trivy (multi-language + config)` CI checks on every PR since PR #594 wired the canaries into CI. Neither was caused by #595 or #596; both are latent bugs that surfaced when CI coverage expanded.

---

## Fix #1 — API key deletion does not evict the validation cache

### Root cause

`Service.validateAPIKey` (`api/internal/services/auth/auth.go:730-840`) caches successful API key lookups in Redis under `apikey:<sha256(plaintext)>` for **15 minutes** (`auth.go:836`). The cache hit short-circuits the DB lookup entirely (`auth.go:735-750`).

`Service.DeleteAPIKey` (`auth.go:1363`) deleted the DB row but never evicted the cache entry. A deleted key therefore continued to authenticate for up to 15 minutes — the canary's P6 ("deleted key rejected") failed because the stale cache still returned the userID.

### Fix

- `api/internal/services/database/database.go:896-919` (`GetAPIKey`) — populate `k.Key` with the stored hash from the `api_keys.key` column (previously read into a discarded local `keyStr`). This is an internal call used only by `DeleteAPIKey` (`auth.go:1364`); it is NOT exposed by any handler. `ListAPIKeys` already discards the key column (`database.go:881`, scans into `new(string)`), so the public List response continues to omit the hash — verified by the existing canary assertion `list-keys: key value absent in list`.
- `api/internal/services/auth/auth.go:1363-1394` (`DeleteAPIKey`) — after the DB delete succeeds, evict `apikey:<existing.Key>` from the cache. Skips eviction when `existing.Key == ""` (legacy rows / DB-layer regression — avoids bogus cache key). Cache failure is non-fatal: the DB row is gone (source of truth), so the stale entry self-heals on TTL expiry; the failure is logged at Warn so operators notice persistent Redis issues.

The `api_keys.key` column stores `sha256(plaintext)` (set at create time, `auth.go:1291`), which is exactly the suffix `validateAPIKey` uses for its cache key (`auth.go:733`). No new hashing required.

### Tests

Three new tests in `auth_test.go`:
- `TestDeleteAPIKey_EvictsCache` — verifies `cache.Delete(ctx, "apikey:<hash>")` is called with the exact key.
- `TestDeleteAPIKey_EvictsCache_SkipsWhenHashEmpty` — verifies no cache call when the DB row lacks a hash (guards against a bogus `"apikey:"` key).
- `TestDeleteAPIKey_EvictsCache_ContinuesOnCacheError` — verifies cache failure does NOT fail the delete (DB is source of truth).

The existing `TestDeleteAPIKey_Success`, `_NotFound`, `_DBError`, `_DeleteFails` all continue to pass unchanged.

---

## Fix #2 — postcss GHSA-r28c-9q8g-f849 (Path Traversal)

### Root cause

`postcss@8.5.15` (a transitive dep of `vite@5.4.21`) has a HIGH-severity path-traversal vulnerability in source map auto-loading. Trivy's `--severity HIGH,CRITICAL --exit-code 1` config (`security-scan.yml`) fails CI on it.

### Fix

`frontend/package.json` — added an npm `overrides` block forcing `postcss` to `^8.5.18` (the patched version) across the entire dependency tree:

```json
"overrides": {
  "postcss": "^8.5.18"
}
```

This is the standard npm mechanism for forcing a transitive dependency to a patched version without bumping the direct dep (`vite` constrains postcss to `^8.4.43`, which already allows `8.5.x` — the override just ensures the lock file resolves to the patched version). `npm install` resolved it to `postcss@8.5.23`.

`npm audit fix` (non-breaking) was also run, reducing total vulnerabilities from 13 → 8. The remaining 8 are all **moderate** severity (esbuild dev-server CORS, react-router open-redirect) and require breaking major-version bumps (vite 5→8, react-router 6→7). Trivy only fails on HIGH/CRITICAL, so these do not block CI. They are flagged for separate dedicated upgrade work.

### Verification

- `npx tsc --noEmit` — clean.
- `npx vitest run` — 1437 passed, 1 skipped (pre-existing skip).
- `npm ls postcss` — `postcss@8.5.23` (above the 8.5.18 fix version).

---

## Out of scope (flagged for follow-up)

- **vite 5 → 8 / vitest 2 → 3 major upgrade.** Required to clear the esbuild dev-server CORS advisory (`GHSA-67mh-4wv8-2f99`, moderate). Major version bump with breaking config changes; needs dedicated testing.
- **react-router 6 → 7 major upgrade.** Required to clear the react-router open-redirect advisories (`GHSA-wrjc-x8rr-h8h6`, `GHSA-337j-9hxr-rhxg`, moderate). v7 has data-router API changes; needs dedicated testing.

Both are moderate severity and do not block CI.

---

## Tests Run

```
# Go — API key cache eviction
go test ./api/internal/services/auth/... -run TestDeleteAPIKey -v    # 7/7 pass
go test ./api/internal/services/auth/... ./api/internal/services/database/... ./api/internal/server/...   # all pass
go vet ./api/internal/services/auth/... ./api/internal/services/database/...   # clean

# Frontend — postcss bump
cd frontend && npx tsc --noEmit                                       # clean
cd frontend && npx vitest run                                         # 1437 pass / 1 skip
cd frontend && npm ls postcss                                         # 8.5.23
```

---

## Files Modified

- `api/internal/services/auth/auth.go` — `DeleteAPIKey` evicts the validation cache after DB delete.
- `api/internal/services/auth/auth_test.go` — 3 new cache-eviction tests.
- `api/internal/services/database/database.go` — `GetAPIKey` populates `Key` (hash) on the returned row.
- `frontend/package.json` — added `overrides.postcss = "^8.5.18"`.
- `frontend/package-lock.json` — regenerated (postcss 8.5.15 → 8.5.23, plus non-breaking audit fixes).
- `worklogs/0651_2026-07-24_apikey-cache-eviction-postcss-cve.md` — this worklog.
