# Worklog: #1481 — message_count rebuilt from the harness during reconcile

**Date:** 2026-09-19
**Session:** The #754 fold-in via #1340/#1479 (reviewer-contracted follow-up, filed by worker 5, implemented on this lane); branch `fix/1481-count-rebuild-foldin`
**Status:** Complete

---

## Objective

Replace message_count's incremental-only maintenance with harness-ground-truth rebuilds during the #1479 reconcile pass — repairing the duplicate-SSE-event double-count #754 documented, without regressions to the ghost-row convergence machinery.

---

## Work Completed

- **Measurement (issue scope item 1, live-pod):** a full 500-message page costs 240–490ms over localhost; typical sessions fit ONE page. The pinned 1.18.15 session list carries NO count field (verified during #1452 and again here), so the bounded walk IS the cheapest source — no cheaper seam exists at this pin (item 2 answered: none).
- **Seam:** `Adapter.CountMessages(ctx, userID, workspaceID, sessionID)` on the interface (the #1127 on-interface lesson, VerifyDelivery precedent) — paged raw walk (`?limit=500&before=<cursor>` + `X-Next-Cursor`, `[]json.RawMessage` lengths only, 40-page/20k ceiling), mirroring loopback.SessionMessageCount so the cursor knowledge stays in `pkg/agent/opencode`; 404 → the typed gone error the #1479 machinery classifies.
- **DB:** `UpsertSessionMessageCount` — absolute-set upsert with a `WHERE message_count IS DISTINCT FROM EXCLUDED` guard: converged workspaces are write-free (no updated_at churn every 30s).
- **Fold-in (handler):** `runSessionIndexReconciliation` now memoizes the harness present-set (ONE ListSessions per pass — shared by Plan and rebuild), then `rebuildMessageCounts`: present rows only, oldest-touched first, budget K=3/pass (`reconcileCountBudget` with the measured sizing doc: ≤3 single-GET walks typical ~1.5s/pass; convergence ceil(N/3)×30s — ≤17min at the 100-row cap, <4min typical; the 15s pass timeout bounds the pathological multi-page tail), drift-only writes, walk errors skip (vanished sessions are the miss-machinery's business). Incremental RecordMessage unchanged (the between-passes approximation).
- **Metrics:** count outcomes as VALUES on worker 5's existing single-label family per their collision requirement: `count_rebuilt` / `count_unchanged` / `count_walk_error` — no label-set or signature change; their r4 pin untouched.
- **Collisions handled (worker 5's map):** `reconcileSpyAdapter` (server router pin) got an explicit CountMessages override (their preference over a budget-gate skip); `mockAdapter.CountMessages` zero-defaults per the Capabilities/ContextUsageFromEvent precedent (my rebuild loop is an unconditional background path — a panic default would break every unconfigured SSE test); their keptForOperator/delete_failed counter-pinning semantics untouched by the rebuild leg.
- **#1312 invariant-table row** posted on the spec issue (ground truth, writers, staleness bound, fault legs).

---

## Key Decisions

- **Rebuild in the existing reconcile pass, not a new loop** — the pass already owns harness-vs-index convergence, cadence claiming, and the present-set; a second loop would double harness load.
- **Budget K=3 oldest-first** (stagger over sampled/on-demand): deterministic per-row convergence bound, cheapest rows first, worst-case cost empirically bounded; documented in the constant's comment.
- **Absolute-set + DISTINCT guard at the DB level** rather than handler-side compare-then-write: the guard also protects any future caller, and one round-tray does the read-free no-op skip.
- **Count as interface method** (not handlers-local assertion) — production adapters are wrapped; seam assertions fail silently through wrappers (#1127).

### Assumptions stated and validated (Rule 7)

| # | Assumption | Validation |
|---|---|---|
| A1 | No cheaper count source at the 1.18.15 pin | Live session-list inspection (no count field); the walk is loopback's production answer for the same question |
| A2 | Walk cost fits the pass budget | Measured 240–490ms/page localhost; K=3 typical ≈ 1.5s of the 15s pass timeout |
| A3 | PlanReconciliation calls harnessList once | Worker 5 confirmed; memoization makes it once regardless |
| A4 | Count-walk errors must not touch the miss machinery | Kept: walk errors only skip + metric; reaping stays presence-driven (pinned by AbsentRowsNeverCounted) |

---

## Blockers

None.

---

## Tests Run

- `go test -run TestAdapter_CountMessages ./pkg/agent/opencode/` — ok (pagination wire incl. `before=` param, short-page stop, 40-page ceiling, typed gone)
- `go test -run TestUpsertSessionMessageCount ./api/internal/services/database/` — ok (columns + DISTINCT guard, sqlmock-pinned)
- `go test -run TestSessionIndexReconcile ./api/internal/handlers/` — ok with `-shuffle=on` (existing #1479 suite + 7 new: headline double-count repair, match-writes-nothing, budget oldest-first K pin, walk-error skip, absent-never-counted, harness-error-skips-rebuild)
- Full touched-surface pass: `go test -count=1 ./api/internal/services/auth/ ./api/internal/handlers/ ./api/internal/server/ ./api/internal/services/sessionindex/ ./api/internal/services/database/ ./api/internal/services/workspace/ ./api/internal/app/ ./pkg/agent/...` — all ok (race deferred to CI per memory directive)
- `go vet ./...` clean; gofmt/imports clean
- Mutation checks: dropping the fold-in call fails the 7 handler rows; removing the budget slice fails the K pin; removing the DISTINCT guard fails its sqlmock row; per the metric-label collision requirement, worker 5's r4 delete-failed pin passes unmodified.

---

## Next Steps

- Adversarial review loop until APPROVED; orchestrator merges (rides the v0.34.6-class train).
- Live-pod re-verification of a rebuilt count post-deploy (a drifted row heals within one window — observable via the sidebar's message counts).

---

## Files Modified

- `pkg/agent/adapter.go` — CountMessages on the Adapter interface
- `pkg/agent/opencode/adapter.go` — the bounded paged-walk impl + constants
- `pkg/agent/opencode/adapter_count_test.go` — new (4 wire tests + countWalkServer)
- `api/internal/interfaces/interfaces.go` — RebuildMessageCount + UpsertSessionMessageCount
- `api/internal/services/database/database.go` — UpsertSessionMessageCount (DISTINCT-guarded upsert)
- `api/internal/services/database/session_index_test.go` — 2 sqlmock pins
- `api/internal/services/sessionindex/service.go` — RebuildMessageCount wrapper
- `api/internal/handlers/proxy_session_index.go` — memoized present-set + rebuildMessageCounts + reconcileCountBudget
- `api/internal/handlers/proxy_session_reconcile_count_test.go` — new (7 tests + countTestHandler)
- `api/internal/handlers/mock_adapter_test.go` — countMessagesFn + zero-default (doc updated)
- `api/internal/handlers/opencode_upgrade_test.go` — mockSessionIndex.rebuiltCounts + RebuildMessageCount
- `api/internal/mocks/database.go` — generated-style mock method
- Mechanical implementer additions (RebuildMessageCount/UpsertSessionMessageCount/CountMessages no-ops or overrides): `api/internal/server/router_session_reconcile_test.go` (worker-5's spy — the coordinated override), `api/internal/services/workspace/workspace_session_test.go`, `api/internal/handlers/proxy_test.go` (×2 mocks), `api/internal/handlers/proxy_backfill_test.go`, `api/internal/app/e2e_suspend_test.go`, `api/internal/services/auth/auth_e2e_all_test.go`, `api/internal/services/auth/auth_e2e_secrets_test.go`, `api/internal/services/auth/auth_sessionid_test.go`, `pkg/agent/adapter_test.go`
