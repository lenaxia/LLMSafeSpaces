# Worklog: Cron Trigger UUID ID Fix

**Date:** 2026-08-09
**Session:** Fix silent-failure bug where cron triggers never produced fire rows.
**Status:** Complete

---

## Objective

User-scope cron trigger `Test1we` (`*/15 * * * *`) produced zero fire rows across 18+ attempts over 6+ hours. The trigger appeared to fire (`last_fired_at` advanced, `next_fire_at` advanced) but no agent invocation ever ran. The DeliveryLog panel in TriggersPage showed 0 rows.

---

## Root Cause

The cron scheduler in `api/internal/workflows/engine.go` generated `trigger_fires.id` and `workflow_runs.id` values via:

```go
fireID := fmt.Sprintf("fire-%s-%d", trigger.ID, now.Unix())
runID  := fmt.Sprintf("run-%s-%d", trigger.ID, now.Unix())
```

Both PG columns are `uuid NOT NULL` (migration `000016:209,290,327`). Postgres rejected every insert with SQLSTATE 22P02 (`invalid input syntax for type uuid`). The scheduler logged the error via `logger.Error` and swallowed it (`_ = ... ; return`), then proceeded to call `UpdateTriggerFireTimestamps` — advancing `last_fired_at`/`next_fire_at`. Result: the trigger looked fired but produced zero agent invocations.

Same bug class in the reconciler path (`engine.go:310`):

```go
nodeRunID := fmt.Sprintf("%s-%s-%d", run.ID, node.ID, attempt)
```

`workflow_node_runs.id` is `uuid NOT NULL` (migration `000016:290`). Every workflow node execution silently dropped its audit row.

The webhook receiver (`api/internal/handlers/webhook_receiver.go`) was already correct — it used `uuid.New().String()`. Only the engine-internal paths (scheduler + reconciler) were broken.

---

## Why It Was Not Caught

- The existing scheduler unit tests (`engine_test.go`) use a `mockSchedulerStore` that accepts any string as an ID. The mock does not enforce the UUID format the real PG column requires.
- The existing reconciler tests use a `mockStore` with the same loose acceptance.
- The integration test for `ListDueCronTriggers` exercised the SELECT path but never the fire-row INSERT path through the engine.
- The bug lived in a background goroutine (`app.go:1336` → `wfScheduler.Start`), not in any HTTP request path. No 5xx responses, no failed requests — pure silent failure.

---

## Work Completed

### Fix

`api/internal/workflows/engine.go` — replaced 5 `fmt.Sprintf` ID generators with `uuid.New().String()`:

1. `:455` missed-fire row (`fire-missed-%s-%d`)
2. `:494-495` workflow-target fire + run IDs (`fire-%s-%d`, `run-%s-%d`)
3. `:515` skipped-fire row (`fireID + "-skipped"`)
4. `:532` routine-target fire ID (`fire-%s-%d`) ← the one hitting `Test1we`
5. `:310` reconciler node-run ID (`%s-%s-%d`) — second instance of the same bug class, found by the AI reviewer

### Regression tests

**Unit (`api/internal/workflows/engine_test.go`):**

- `uuidEnforcingSchedulerStore` wraps `mockSchedulerStore` and calls `uuid.Parse` on every fire/run ID. `TestScheduler_FireAndRunIDs_AreUUIDs` covers all 3 ID-emitting scheduler paths (workflow-target, routine-target, missed-fire-skipped).
- `uuidEnforcingReconcilerStore` wraps `mockStore` and calls `uuid.Parse` on every node-run ID. `TestReconciler_NodeRunID_IsUUID` covers the reconciler path.
- Both were verified to fail against the buggy code and pass against the fix.

**Integration (`pkg/workflows/store_integration_test.go`):**

- `TestTriggerFiresUUIDColumn_RejectsNonUUIDIDs` pins the DB-side invariant: inserts the old buggy shape (`"fire-"+triggerID+"-1786260973"`), asserts SQLSTATE 22P02; inserts a UUID, asserts success + readback.

---

## Key Decisions

1. **Fix all 5 sites in one PR.** The reconciler node-run ID bug was the same class, same file, same execution flow. Deferring it would leave a known silent-failure in the workflow execution path. The AI reviewer correctly flagged this as a Rule 5 violation (Zero Technical Debt).
2. **Wrap the mock rather than change its signature.** The existing `mockSchedulerStore`/`mockStore` accept any string and are shared across many tests. A wrapper (`uuidEnforcingSchedulerStore`/`uuidEnforcingReconcilerStore`) lets the regression tests enforce the UUID invariant without disturbing existing tests.
3. **No worklog-number migration.** Worklog assigned via the repolint bot's normal post-merge numbering.

---

## Tests Run

- `go vet ./api/internal/workflows/... ./pkg/workflows/...` — clean
- `go test -timeout 120s ./api/internal/workflows/...` — PASS (including new regression tests)
- `go test -timeout 120s ./api/internal/handlers/...` — PASS
- `go test -timeout 120s ./pkg/workflows/...` — PASS
- Regression tests verified to fail against buggy code (reverted one fix, re-ran, confirmed failure message)
- CI: Lint, Gitleaks, Trivy, govulncheck, Build Frontend, review — ALL PASS

---

## Next Steps

1. Deploy + verify `Test1we` produces fire rows on the next cron tick.
2. The `pkg/secrets integration` CI check is failing on `main` for unrelated reasons (CHECK constraint `workflows_on_missing_workspace_chk` + non-UUID fixtures `wf_1`/`ws-1` in pre-existing tests) — separate fixup PR.
