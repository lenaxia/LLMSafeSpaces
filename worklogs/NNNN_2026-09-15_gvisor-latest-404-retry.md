
---

# Worklog NNNN addendum — 2026-09-15 (same session, second PR): the GCS standalone binaries are GONE, not flipping

The retry wrapper (#1373) was the wrong diagnosis carried one step too far: pool attempt 4 (34953918062, WITH the fix merged) retried `containerd-shim-runsc-v1` five times and failed loudly — because the artifact is a PERMANENT 404. Verified from the dev pod: `…/releases/release/latest/x86_64/containerd-shim-runsc-v1` → 404 on x86_64 AND aarch_64 (versioned paths too: `release/20260907.0/…` → 404), while `runsc` still serves 200. Upstream gVisor stopped publishing standalone release binaries; the artifacts now ship only inside the GitHub release tarballs (`gvisor-x86_64.tar.zstd` + `SHA512SUMS`, present on every release since at least 20260817).

## Work completed (TDD + live-verified)

1. **RED:** `TestUS70GvisorFetch_AllFetchSitesRideTheWrapper` re-pinned to the bundle flow — failed against the GCS fetches.
2. **`local/lib/gvisor.sh` rewritten to the bundle flow:** resolve the latest release tag from the `/releases/latest` redirect (NO `-L` — following it empties `%{redirect_url}`; validated live: `release-20260907.0`, guard on the `release-*` shape); `fetch_retry` the bundle + `SHA512SUMS` from that ONE tag (pair coherence across flips — a straddle fails the checksum, never installs a mixed pair); verify the bundle hash (the 128-hex guard marker unchanged); `tar --zstd -xf` both binaries; install. `zstd` joins the apt line.
3. **A quoting bug caught before it shipped:** the first draft parsed the sums line with `awk "$2 == f …"` — inside the single-quoted `docker exec bash -c` block the inner bash would have expanded `$2` to empty. Replaced with `grep " $BUNDLE\$" | cut -d" " -f1`.
4. **Live end-to-end of the block's core** (dev pod, real network): tag resolve → both fetches → checksum verify → extraction → `runsc --version` = `release-20260907.0`, shim executes.
5. `fetch_retry` itself is UNCHANGED — the bound/give-up/ride-through pins from the first PR keep enforcing it.

## Key decisions

- One source of truth: the bundle carries BOTH binaries consistently versioned (previously runsc and shim were fetched and verified independently — a mixed pair was possible mid-flip).
- The tag is resolved once and both artifacts come from it; a mid-flight re-tag fails the checksum loudly (acceptable: seconds-wide window, re-runnable).

## Tests run

- `go test -timeout 300s ./local/` — ok (all US70 pins incl. the re-pinned bundle flow); `bash -n` clean; `golangci-lint` 0 issues
- Live e2e of fetch+verify+extract against the real GitHub release (above)

## Files modified (this addendum's PR)

- `local/lib/gvisor.sh`, `local/us70_harness_script_test.go`, this worklog
