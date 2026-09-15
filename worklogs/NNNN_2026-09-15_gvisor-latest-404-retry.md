
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

## Review r1 remediation (this PR, #1375)

**f1 (the S5 copy — same dead flow, weekly):** `s5-overlay-validation.sh` S5.6 now DELEGATES — `lib/gvisor.sh install "$NODE"` + `runtimeclass` — replacing the inline GCS block (a verbatim copy of the pre-fix flow) and its duplicate RuntimeClass heredoc. The two orphaned S5 guard rows (their subject moved to the lib; the US70 rows pin it there, the delegation test pins the wiring) removed with `shQuote` restored to the package. The stale "keeps its own inline copy deliberately / do not modify" rationale corrected in the gvisor.sh header and `local/lib/README.md` — that rationale was written when the copy still worked.

**f2 (the tag resolve was the only un-retried moving-alias fetch):** `resolve_tag()` — 3 attempts / 10s backoff, printed-tag contract, fail-closed on empty redirect (the `-L` footgun) and non-`release-*` shapes.

**The executable-coverage gap (mutation-verified blind spots):** `resolve_tag` and `verify_bundle` extracted as named functions and pinned extract-and-execute: tag happy/followed-redirect-empty/non-release-shape (all three mutations from the review now fail the suite); verify happy / missing-line→guard diagnostic / hash-mismatch→abort, plus a structural no-install-in-verify pin (abort strictly precedes install). The 128-hex guard marker keeps its historical `"$EXPECTED"` form so the existing guard rows extract unchanged.

## Tests run (r1)

- `go test -timeout 300s ./local/` — ok; `bash -n` both scripts; `golangci-lint` 0 issues
- Live: `resolve_tag` executed against the real redirect → `release-20260907.0`

## Review r2 remediation (#1375)

**f1 (BLOCKER — the delegation was mis-wired):** `CLUSTER_NAME`/`CTX` are plain vars in the s5 script and never crossed the process boundary — the child `gvisor.sh runtimeclass` would have targeted `kind-llmsafespaces-ci`, a context that does not exist on the s5 runner (its cluster is `s5-ovl`): install succeeds, RuntimeClass apply dies, S5.6 red with a misleading diagnosis. Fixed with EXPLICIT per-call env assignments (`CLUSTER_NAME="$CLUSTER_NAME" CTX="kind-$CLUSTER_NAME" bash …`), pinned by `TestS5Gvisor_DelegationCarriesClusterEnv` — extract-and-execute of the delegation lines against a stub gvisor.sh recording its inherited environment (the r1 claim "the delegation test pins the wiring" was false — the substring pins could not see this; recorded as a correction).

**Test-gap closures (all mutation-verified blind in r2):**
- `resolve_tag` retry rows: ride-through (stub failing non-zero twice — r2's stub exited 0 which the `&& break` treats as success; corrected semantics) and give-up-loudly under errexit; the stub is `-L`-aware (a followed redirect yields exit-0-empty — GitHub's real shape), so the happy row also proves the script never passes `-L`.
- `TestUS70Gvisor_VerifyBeforeExtractAndInstall` — call-site ordering pinned (verify < extract < install), the ordering half the r1 body-level pin missed.

## Tests run (r2)

- `go test -timeout 300s ./local/` — ok; `golangci-lint` 0; `bash -n` both scripts

## Review r3 remediation (#1375, final)

- The delegation pin now asserts BOTH invocations' inherited env (two record lines, each `CLUSTER_NAME=s5-ovl CTX=kind-s5-ovl`, the second being `runtimeclass` — the CTX consumer the r2 anchor missed).
- The `-L` stub catches combined short-flag clusters (`-*L*`, e.g. `-fsSL`) — the `-fsSL` mutation now fails the happy row.
- The exhaustion row uses an always-non-zero stub so the retry loop actually exhausts (the exit-0-empty shape rides the `HappyAndFailClosed` row); `for i in 1` truncation is caught by the ride-through row.
- Both of the reviewer's mutations re-executed against the new pins: FAIL as required; restored state green (`./local/` ok, lint 0).

## Tests run (r3)

- `go test -timeout 300s ./local/` — ok; `golangci-lint` 0; mutation checks (above)
