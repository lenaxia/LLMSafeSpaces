# Worklog NNNN — delivery-pool gVisor provisioning: retry the moving-alias 404 window

**Date:** 2026-09-15
**Session:** Hot-fix from the epic-71 / 4b post-merge gate — two consecutive US-70 delivery-pool runs on main (34930483199 attempts 1–2) died in the "Provision gVisor (runsc) on the kind node" step: `curl: (22) The requested URL returned error: 404` against `https://storage.googleapis.com/gvisor/releases/release/latest/x86_64/…` — the `latest` alias briefly 404s while gVisor flips a release, and curl's `--retry` does not cover 404 (it treats 4xx as permanent). The branch pool run for #1371 passed the same step hours earlier; the URL serves 200 again now.
**Status:** Complete

## Work completed (TDD)

1. **RED:** `TestUS70GvisorFetch_AllFetchSitesRideTheWrapper` + `TestUS70GvisorFetchRetry_RetriesThroughTransient404` (extract-and-execute, same pattern as the checksum-guard tests) — failed against the bare `$CURL` fetches.
2. **Fix:** `fetch_retry()` in `local/lib/gvisor.sh` — the whole fetch retried 5× with 15s backoff (≈75s window, inside the pool's provisioning step); all four artifact fetches (runsc, runsc.sha512, shim, shim.sha512) route through it. The behavior row executes the extracted function against a stub curl that fails exactly twice then succeeds — it must ride through (the release-flip window).
3. `bash -n` clean; the full `./local/` suite green.

## Key decisions

- Retry the fetch, not pin a version: `latest` is the verified source (sha512-checked on arrival); pinning a version would add a manual rotation duty. A 75s retry window covers the observed flip; a longer flip fails loudly as before.
- No workflow change — the fix is inside the script the pool already calls (`local/lib/gvisor.sh install`), so the pool pins (`TestUS70PoolWorkflow_Pins`) stay untouched.

## Tests run

- `go test -timeout 300s ./local/` — ok (new rows included)
- `bash -n local/lib/gvisor.sh` — clean

## Files modified

- `local/lib/gvisor.sh`, `local/us70_harness_script_test.go`, this worklog
