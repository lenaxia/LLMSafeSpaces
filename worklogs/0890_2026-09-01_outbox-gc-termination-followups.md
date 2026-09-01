# Worklog: Epic 69 revalidation + #1211 F2 follow-ups — outbox GC on termination, drill hardening

**Date:** 2026-09-01
**Session:** Post-close-out revalidation of Epic 69 (owner ask: "are we sure we're done?") + the item routed from the v0.26.0 post-upgrade verification (#1211): "F2 → Epic 69 agent".
**Status:** Complete.

---

## Revalidation results (adversarial, fresh main)

| Check | Result |
|---|---|
| main CI after all merges | green (every post-merge run success) |
| Worklog bot numbering | 0 `NNNN_` files remain on main; 0888/0889 assigned |
| design 0055 | Status = Implemented; 22 checkboxes ticked; the 4 unticked are exactly the documented pool items (zero-divergence soak, admission-ID matrix, resume p95, rollback drill under load) |
| Full Go suite (`./api/... ./pkg/... ./cmd/...`) | green — **except one real catch**, below |
| Frontend | 1786/1786 vitest, 166 files |
| Dead-code gates | green (ran within the handlers suite) |
| Dialect remnants | production code clean; one stale comment in `frontend/src/hooks/useWorkspaces.ts` still described the deleted `session.event` SSE stream — fixed here |

**The catch**: `TestRollbackDrill_UnderLoad` passed standalone (12×) and in CI's layout, but **flaked once under local full-suite parallelism** — CPU contention starved the 30s drain window. Fixed: deliverer delay 2ms→1ms, deadline 30s→90s with a comment explaining the suite-parallelism hazard. Re-verified green in the full `./api/... ./pkg/...` run.

## #1211 F2 → the #1119 orphaned-outbox-keys gap

The verification's F2 triage (on #1211) found: deleting a Workspace CR leaves `outboxq/outboxd/outboxdedupe/outboxlock` keys orphaned in Valkey — no agent to verify against, no queue UI to dismiss from, no expiry; the rows inflate `llmsafespaces_outbox_entries{status="error"}` forever and are re-scanned every worker tick. One live instance was manually remediated; the fix lands here.

- `outbox.CleanupWorkspace(ctx, workspaceID)` — SCAN+DEL across the four key families, idempotent, returns the key count. TDD: deletes only the target workspace's keys (queues/staging/dedupe/lock), other workspaces untouched, empty/idempotent no-ops.
- Wired in `onPhaseChange`'s Terminating/Terminated branch (same site as the activity-tracker delete): detached + 15s-bounded goroutine, warn-only on failure — a missed sweep retries on the next transition (the package's crash-safety stance).

## Tests Run

- `go test ./api/internal/services/outbox/ -run TestCleanupWorkspace -count=3` — ok.
- `go test ./api/internal/services/outbox/ ./api/internal/handlers/ -count=1` — ok (110s).
- Full `go test ./api/... ./pkg/... -count=1` — green (the drill included).

## Next Steps

- The remaining Epic 69 open items are unchanged and on their own records: US-69.14 (upstream-blocked), admission-ID pool matrix (gates the oracle deletion), the pool evidence set.

## Files Modified

- `api/internal/services/outbox/cleanup.go` (new), `cleanup_test.go` (new)
- `api/internal/handlers/proxy_events.go` (Terminated-phase cleanup hook)
- `api/internal/services/outbox/rollback_drill_test.go` (timing hardening)
- `frontend/src/hooks/useWorkspaces.ts` (stale comment)
