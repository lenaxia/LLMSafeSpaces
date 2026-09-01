# Worklog: Epic 69 pool evidence leg — wiring #1218/#1219 into the delivery pool

**Date:** 2026-09-01
**Session:** Close out the two tracking issues left by the Epic 69 close-out: the admission-ID matrix (#1219) and the pool evidence set (#1218) had committed harnesses but NO pool leg ever ran them — the evidence pipeline existed only on paper (and the pool itself had been failing on an unrelated quoting bug since #1204, fixed by #1217).
**Status:** Complete — leg wired, workflow_dispatch armed; first run dispatched after merge.

---

## Work Completed

- `local/us-69-evidence-leg.sh` (new): the Epic 69 evidence row, sourcing the shared US-70 harness —
  - **A. Admission-ID matrix (#1219)**: seeds a workspace, waits Active, reads the pod password secret, creates a session via the API, port-forwards the pod's opencode :4096, runs the committed `spike-admission-id.sh` probe (baseline / fresh-unique / duplicate-reuse). The matrix outcome is DATA (recorded to the artifact; the disposition rule — fresh-unique accept + duplicate 409 ⇒ delete the oracle — is applied by the tracker on #1219); only harness ERRORS fail the step.
  - **B. Authority-flip operational drill (#1218)**: promotes the harness user to admin if the G8 first-registrant invariant didn't hold (the pool owns the DB), reads `ledger_in_flight` off a live pod via the admin endpoint (the drain signal's first live-cluster read), drives `authority-flip.sh park/unpark`, and FAILS on a park/unpark round-trip mismatch. The load-scale zero-loss verdict stays pinned in-repo (`rollback_drill_test.go`); this leg proves the operational path at cluster scale.
- `local/authority-flip.sh`: added the missing `inflight <workspaceId>` subcommand (the leg and runbook both reference it; the case arm didn't exist) + usage row.
- Pool workflow: new "Run Epic 69 evidence leg" step (SEAM-INERT, before the fault seam arms) + the evidence file joins the `us70-pool-results-*` artifact.
- `TestUS69EvidenceLegScript`: syntax + structure pins (sources the shared harness, invokes both sub-harnesses, `set -euo pipefail`).

## Key Decisions

1. The matrix step reports rather than asserts — a "duplicate admission" outcome would force the oracle fallback to STAY (recorded on #1219), which is a legitimate result, not a CI failure.
2. Admin access via the G8 first-registrant invariant with a psql fallback (the pool owns the database; no new credentials).

## Tests Run

- `bash -n` on both scripts; `go test ./local/` green (incl. the new pins).
- `go test ./tests/ghaworkflows/` green (workflow lint).

## Next Steps

1. Dispatch the pool (workflow_dispatch) after merge; record the matrix outcome on #1219 → on pass, file the oracle deletion (the recorded trigger).
2. Record the drill + in-flight evidence on #1218; the soak/p95 items keep accumulating on the pool's schedule.

## Files Modified

- `local/us-69-evidence-leg.sh` (new), `local/authority-flip.sh`, `.github/workflows/us-70-delivery-pool.yml`, `local/us70_harness_script_test.go`
