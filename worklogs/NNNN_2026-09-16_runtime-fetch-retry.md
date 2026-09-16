# Worklog NNNN — delivery-pool build flake: retry the runtime image's release-asset fetches

**Date:** 2026-09-16
**Session:** Coordinator hot-fix — the post-merge US-70 pool on main (run 35159837345) died in "Build and load images": the mise release-asset fetch got a transient GitHub 500 (`curl: (22) … 500`, exit 22) and every downstream suite row failed as a cascade. The pinned URL (v2026.5.15) serves fine — a one-shot 5xx, same flake family as the gVisor latest-alias 404s (#1373/#1375).
**Status:** Complete

## Work completed (TDD)

1. **RED:** `local/runtime_dockerfile_test.go` — `TestRuntimeBaseDockerfile_ReleaseFetchesRetry` fails on main (line 96 flagged, the gh CLI fetch).
2. **Fix:** all five `curl --fail` release-asset fetches in `runtimes/base/Dockerfile` (gh, gh-checksums, mise, AWS CLI, AWS CLI checksum) gain `--retry 5 --retry-delay 3 --retry-all-errors` — retry-all-errors covers 5xx AND the release-flip 404 window; the pin requires both flags on every fetch site (a sixth un-retried fetch fails the suite).

## Key decisions

- Retry flags in-place rather than a shell loop: the Dockerfile fetches are single-RUN layers; curl's own retry keeps the Dockerfile flat and the failure mode identical to the gvisor.sh precedent (bounded, loud after exhaustion).
- The pin lives in local/ (the harness-test home) with a skip-guard for checkouts without runtimes/.

## Tests run

- `go test -timeout 60s -run TestRuntimeBaseDockerfile ./local/` — red on main, green with the fix; full `go test ./local/` ok; `make repolint` passed.

## Files modified

- `runtimes/base/Dockerfile`, `local/runtime_dockerfile_test.go`, this worklog
