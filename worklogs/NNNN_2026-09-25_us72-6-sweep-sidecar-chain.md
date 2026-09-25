# Worklog: US-72.6 sweep fix — the boot-scrub chain in sidecar mode + the convergence gate (run 36135708380)

**Date:** 2026-09-25
**Session:** The sweep's FIRST REAL EXECUTION (run 36135708380, the same run that took the drill fully green — R1-R4 PASS, CredentialsStaged=True at R2+R4, D6.1 complete: US-72.5's executed evidence closed) failed 5 rows: R1 (3 canary hits post-flip), R2 (2 hits post-scrub + LegacyKeysScrubbed=None), R3 (the residue-boot migration never fired; 1 hit post-boot). The drill/sweep scripts are this lane's; the fallback machinery is design-0061's (checked: no open PR collides with these files).
**Status:** Complete

---

## Objective

Make the epic's exit-criterion sweep (zero canary bytes in uid-1000 space) runnable on the nightly's actual posture: the three failures share two root causes, both diagnosable from the run log + the code.

## Work Completed

### The diagnosis (from run 36135708380's sweep job log + the code)

- **R1 (3 hits in a FRESH post-flip workspace): the sweep swept the BOOT TRANSIENT, not the converged posture.** The script gated only on `Active`; the drill's own R2 (which PASSED zero-canary in the same run) gates on `CredentialsStaged=True`. Design 0061 M2's migration-mode fail-open fallback — the very fix that made the drill green — DELIBERATELY delivers a raw batch on any boot where controller staging hasn't converged (workspaces must not strand). That window's live files (agent-config.json, rt/auth.json) legitimately carry the raw canary until the token batch lands: 3 hits, by design, not a K1 violation.
- **R2's two failures: the same window + a dead mirror.** The 2 post-scrub hits were R1's transient residue (the scrub correctly removed the planted legacy file — its own report `authKeysRemoved:1`). The `LegacyKeysScrubbed=None` mirror failure exposed the bigger defect below.
- **R3 (the migration never fired) + R2's mirror: the #1537 scrub chain was SINGLE-CONTAINER-ONLY.** The nightly installs `agentdSidecar.enabled=true`: `--sidecar` and `supervise-opencode` both dispatch (main.go:110/:101) BEFORE the scrub wiring (:275) — the boot scrub NEVER runs in sidecar mode; and the sidecar (uid 2000) cannot see `/workspace` at all, so it could not run it either; its `serverDeps` carries no `legacyScrub` (sidecar_mode.go's deps literal) — the healthz never carried the slice, so the controller's mirror could never land. `LegacyKeysScrubbed=None` on every sidecar install, and R3's trigger dead — exactly what the run showed.

### The fix (TDD; 7 new tests, the boot-trigger three red-first against the wiring)

- **The uid-1000 supervisor runs the scrub** (the only /workspace-visible process in sidecar mode): `runSuperviseOpencodeCommand` creates the tracker and fires `bootLegacyScrub` at boot; the single-container main path fires it too, and the #1537 first-Present hook REMAINS wired as belt-and-braces (the tracker's sync.Once collapses whichever fires first — the hook alone was starved by M2's fallback: a raw batch carries no relay-fronted entries, Present never evaluates true).
- **The report chain crosses the containers** (the supervisor-status pattern, mirrored): the control socket's `status` method embeds the report under `legacy_scrub` (nil seam → field absent, wire-compat); `controlClient` decodes it into `controlStatus.LegacyScrub`; the sidecar's `supervisorStatusStore.legacyScrubHealth()` surfaces it; `serverDeps.legacyScrubSnapshot` (a new override seam; the tracker-derived snapshot remains the fallback) wires it into healthz — the controller's `LegacyKeysScrubbed` mirror now works in BOTH topologies.
- **The sweep script's R1 gates on `CredentialsStaged=True`** (the drill's own gate; up to 300s): the criterion evaluates the CONVERGED posture, with a comment naming the M2 fallback window as the designed raw-delivery transient it is. `sweep_hits` now prints `HIT <path> xN` to stderr for every hit path (2>&1 | tail -1 keeps the count as stdout) — the run's single biggest diagnosability gap was that 3 hits told us nothing about WHERE.

## Key Decisions

1. **Unconditional boot scrub, not a smarter Present gate**: M2's fallback can starve Present on ANY boot (including the residue-boot resume — exactly when the migration must run); the scrub is safe on every posture (strips key material only from legacy platform-shaped residue files, never the live delivery surfaces).
2. **The status-method seam over any new control method**: the supervisor-status poll already exists at the 15s cadence the controller's healthz scrape tolerates; no new protocol surface.
3. **Wire-compat nil-seam**: a pre-US-72.6 supervisor omits the field; old clients ignore it.
4. **The K1 carve-out unchanged** (non-frontables excluded, owner decision pending).

## Blockers

None. Pre-existing (NOT this PR): `TestUploadApply_ConcurrentWallTime` fails on the CLEAN main tree (verified via stash) — the design-0060/0061 lane's area.

## Tests Run

- `go test ./cmd/workspace-agentd/ -run 'TestBootLegacyScrub|TestLegacyScrubTracker_Boot|TestControlSocketStatusCarries|TestControlClientDecodesLegacyScrub|TestSupervisorStatusStoreMirrors|TestServerDepsLegacyScrub' -count=1 -v` — 7/7 green (the boot-trigger three grown red-first against the pre-wiring tree).
- `go test ./cmd/workspace-agentd/ -count=1` — FAILS ONLY the pre-existing `TestUploadApply_ConcurrentWallTime` (clean-tree reproduced via stash; filed for the 0061 lane).
- `bash -n` on the sweep script; golangci-lint 0 issues; repolint passed; `go build ./...` clean.

## Next Steps

1. Merge → the dispatched nightly should produce the sweep-green run (the epic's exit criterion; the closure draft's sweep rows fill from it).
2. The vacuous-sweep caveat retires with the green run (with the drill green upstream + the convergence gate, the sweep's zero-assertion is load-bearing again).
3. `TestUploadApply_ConcurrentWallTime` to the 0061 lane.

## Files Modified

`cmd/workspace-agentd/main.go` (the unconditional boot call + the M2-starvation note), `cmd/workspace-agentd/supervise_opencode.go` (the supervisor boot scrub + the status seam), `cmd/workspace-agentd/legacy_scrub_tracker.go` (`bootLegacyScrub`, the supervisor-global, the root-from-env), `cmd/workspace-agentd/control_socket.go` (the status payload + seam), `cmd/workspace-agentd/control_client.go` (the decode), `cmd/workspace-agentd/supervisor_status.go` (the store mirror), `cmd/workspace-agentd/server.go` (the snapshot-override seam), `cmd/workspace-agentd/sidecar_mode.go` (the sidecar wiring), `cmd/workspace-agentd/scrub_boot_trigger_test.go` + `scrub_sidecar_chain_test.go` (new, 7 tests), `local/us-72-rogue-agent-sweep.sh` (the convergence gate + hit-path printing), this worklog.
