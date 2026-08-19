# Worklog: runtimes/go toolchain 1.20.5 → 1.26.6 (#854)

**Date:** 2026-08-19
**Session:** Bump the EOL tenant-facing Go toolchain; remove the dead /go machinery the PVC migration obsoleted.
**Status:** Complete

---

## Objective

#854: `runtimes/go/Dockerfile` pinned Go 1.20.5 (EOL, years of stdlib CVEs) for the tenant sandbox image while the platform runs 1.26.6.

## Work Completed

- **Toolchain 1.20.5 → 1.26.6** (ARG form so Renovate's dockerfile manager maintains it, matching `OPENCODE_VERSION`'s pattern). Chose 1.26.6 over 1.27.0: platform line alignment; 1.27 is untested here.
- **Removed the `/go` GOPATH machinery** (ENV GOPATH=/go, PATH entry, `chmod -R 777 /go`, src/bin dirs): the base image redirects GOPATH to the PVC (`/workspace/.local/share/go` — base ENV ordering wins at runtime), so the old GOPATH was dead config; the 777 mode was a wart regardless.
- **Removed `GOPROXY=direct` + `GOSUMDB=off`**: pre-PVC relics that only served the build-time pre-installs; tenants now get proxy + checksum verification by default.
- **Dropped the 2023-era pre-installed module set** (gorilla/mux 1.8.0, gin 1.9.1, cobra 1.7.0, testify 1.8.4, gonum 0.13.0): `go install`ed binaries are toolchain/GOOS-specific artifacts outside module resolution — stale exactly when the toolchain moves — and duplicated what tenants install into the PVC-backed GOPATH anyway. Kept two current starter tools (goimports, staticcheck@latest).
- Doc correction: runtime-environments.md claimed Go installs "via mise" — it's a pinned tarball (also available via mise at runtime); fixed the row.

## Key Decisions

1. **1.26.6, not 1.27.0** — align with the platform's tested line; Renovate bumps forward.
2. **Delete rather than update the pre-installed set** — any pinned set re-creates this exact issue on the next EOL cycle; tenants' own installs land in the persistent GOPATH.
3. **Verified wrapper compatibility**: `go-security-wrapper.go` builds clean under 1.26 (stdlib-only).
4. Assumption validated: the wrapper is NOT on PATH as `go` (no aliasing in base/entrypoints) — tenants invoke the real toolchain; the wrapper is a helper binary, so no PATH behavior changes.

## Blockers

None. Local docker unavailable — image build validated by CI's Runtime Base pipeline pattern (structural checks + wrapper 1.26 build done locally).

## Tests Run

- Structural Dockerfile checks (pin ≥1.26, no proxy/sumdb bypass in code lines, no 777, no 2023 pins, wrapper build step intact) — pass
- `go build` of go-security-wrapper.go under go1.26.6 — pass

## Next Steps

- If image-factory seeds a `go` runtime later, its base-version axis (ruling #29) supersedes this pin naturally.

## Files Modified

- `runtimes/go/Dockerfile`
- `docs/operator/runtime-environments.md`
- `worklogs/NNNN_2026-08-19_runtimes_go_toolchain.md` (this file)
