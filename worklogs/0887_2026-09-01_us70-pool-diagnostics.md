# Worklog: US-70.0 pool run 5 — failure diagnostics for wait_phase

**Date:** 2026-09-01
**Session:** Pool run 5 (33465026792): all three harness layers green (session seeded with OWNER_ID, env-secret bound) — the workspace then stuck in Creating and the run died with no way to see why. Add failure diagnostics.
**Status:** Complete.

## Work Completed

- `wait_phase` timeout now calls `diagnose_workspace`: CR status/conditions (yaml), pod describe tail, first-available container logs (agentd first — the sidecar gates the main container), controller log tail, recent events — to stderr, straight into the CI log.
- Pin: `TestUS70Harness_FailureDiagnostics`.

## Key Decisions

1. **Diagnose before fixing** — runs 4-5 each burned ~40 minutes on blind cycles; the stuck-Creating failure has too many candidate causes (runtime image allow-list, migration, sidecar-boot v2 first run) to guess.
2. Container probe order agentd → workspace → opencode → init containers: the sidecar's startup probe gates everything, so its logs are the highest-value first.

## Blockers

None — merge + re-dispatch; read the dump.

## Tests Run

`go test -timeout 60s ./local/` ok; bash -n clean.

## Files Modified

- `local/lib/us70-common.sh`, `local/us70_harness_script_test.go`
