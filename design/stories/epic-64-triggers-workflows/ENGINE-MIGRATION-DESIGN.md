# ENGINE-MIGRATION-DESIGN.md — Move the Workflow Engine from API to Controller (Item 9)

**Status:** Design
**Created:** 2026-08-09
**Depends On:** Epic 64 v1 (shipped engine in API), Epic 23 (controller leader election), `ROUTINES-REDESIGN.md`
**Replaces:** The "running in the API server" deployment model described in `api/internal/workflows/engine.go:6-11`

---

## Problem Statement

Epic 64 shipped its workflow engine (reconciler + scheduler) as background goroutines inside the **API server** (`api/internal/workflows/engine.go`, 907 lines). This was the pragmatic v1 choice: the API already holds the `pgxpool`, the K8s client, and HTTP connectivity to workspace pods, so wiring the engine there avoided a new dependency. The header comment in `engine.go:6-11` records the rationale explicitly:

> The API already has the pgxpool, K8s client, and HTTP connectivity to workspace pods. Background goroutines (jwtSessionJanitor, pendingOrgCleaner) are the established pattern. FOR UPDATE SKIP LOCKED provides multi-replica safety (no leader election needed).

That rationale is **load-bearing for the OOM class of bugs** — and it is wrong for run-driving goroutines. The janitor goroutines are short, idempotent, restart-safe ticks (prune expired rows, retry on next tick). The workflow reconciler is a **stateful run driver**: it holds a goroutine per in-flight run, blocks on multi-minute agentd HTTP calls, and loses all in-flight work when its host pod dies. Conflating the two is the defect this design fixes.

### Why the API is the wrong host

README-LLM.md:43 defines the platform contract in one line:

> **Stateless API server — horizontally scalable, no sticky sessions required**

The workflow engine violates that contract in three concrete ways:

1. **It is stateful.** A run-driving goroutine carries a claim on a `workflow_runs` row, an acquired global-semaphore slot, an open context tied to a multi-node DAG walk, and in-flight HTTP connections to a workspace pod. None of that survives a pod recycle. README-LLM.md's "fail-on-restart" rule (D3, Edge Case 11) was a *concession* to this fragility, not a feature.
2. **It is non-idempotent.** A janitor that gets OOM-killed mid-tick loses nothing — the rows it would have pruned are still there next tick. A reconciler that gets OOM-killed mid-node marks the run `failed (api_restart)` and the user re-runs. That is the documented behaviour today; it is also the symptom of the engine being in the wrong process.
3. **API pod OOM kills in-flight runs.** The API runs alongside request-serving memory pressure (proxy buffering, SSE fan-out, large spec uploads). An OOM during a 10-minute `agent` node tears down the run with `error_code=api_restart`. Moving the engine to a process whose entire job is run-driving removes this failure class.

The controller is the stateful control plane by design (README-LLM.md:48 — "Kubernetes operator ... manages Workspace CRD lifecycle"). It already runs leader-elected reconcilers and is the correct host for a run driver.

---

## Design

### Net effect

The engine's reconciler + scheduler move from `api/internal/workflows/` to `controller/internal/workflows/`. The controller gains a `pgxpool` dependency. The API keeps the trigger/workflow/run CRUD handlers (writes rows, returns) but **stops driving runs**. The engine header comment in `engine.go:6-11` is corrected to point at the controller.

```
BEFORE                                          AFTER
─────                                           ─────
┌── API (stateless — but secretly stateful) ──┐ ┌── API (stateless — actually) ──────────┐
│  handlers/workflows.go   CRUD               │ │  handlers/workflows.go   CRUD (stays)  │
│  handlers/triggers.go    CRUD               │ │  handlers/triggers.go    CRUD (stays)  │
│  handlers/webhook_triggers.go  receive      │ │  handlers/webhook_triggers.go (stays)   │
│  ───────────────────────────────────────    │ │  (no engine, no goroutines)            │
│  workflows/engine.go  reconciler+Sched. ◄── │ │   ▼ writes rows + returns              │
│  app.go:1325  go wfReconciler.Start()       │ └────────────────────────────────────────┘
│  app.go:1336  go wfScheduler.Start()   ◄─── │              │ queued runs / due triggers visible in PG
│  pgxpool ✓  K8s client ✓  HTTP to pods ✓    │              ▼
└──────────────────────────────────────────────┘ ┌── Controller (stateful, leader-elected) ┐
                                                  │  internal/workflows/reconciler.go        │
                                                  │  internal/workflows/scheduler.go         │
                                                  │  K8s client ✓ (existing)                 │
                                                  │  pgxpool  ✓ (NEW)                        │
                                                  │  HTTP to pods ✓ (via agentd svc/K8s)     │
                                                  │  manager.Runnable + NeedLeaderElection   │
                                                  └──────────────────────────────────────────┘
```

### What moves vs. what stays

| Concern | Today | After migration | Location |
|---|---|---|---|
| Workflow/trigger/webhook/run CRUD handlers | API | **API (unchanged)** | `api/internal/handlers/` |
| Webhook HMAC receive + dedup + rate-limit | API | **API (unchanged)** | `api/internal/handlers/webhook_triggers.go` |
| `workflow_runs` row insert (run initiation) | API | **API (unchanged)** — writes the row, returns | `api/internal/handlers/` |
| `ClaimQueuedRuns` (`FOR UPDATE SKIP LOCKED`) | API engine | **Controller** | `controller/internal/workflows/reconciler.go` |
| DAG state machine (drive nodes, retries, timeouts) | API engine | **Controller** | moves with the reconciler |
| Routine executor (`fireRoutineTarget`) | API engine | **Controller** | moves with the scheduler |
| Cron scheduler tick | API engine | **Controller** | `controller/internal/workflows/scheduler.go` |
| Circuit-breaker auto-disable + audit | API engine | **Controller** | moves with the scheduler |
| Store implementations (the `ReconcilerStore` / `SchedulerStore` interfaces in `engine.go:48-75`) | API (`pkg/workflows/store.go`) | **Shared** — interfaces stay in `pkg/workflows/`, controller links the same store package | `pkg/workflows/store.go` |
| Engine goroutine wiring (`go wfReconciler.Start`) | `app.go:1325-1341` | **Deleted from API**; controller registers via `mgr.Add(...)` | `controller/main.go` |
| `engine_test.go` (858 lines) | `api/internal/workflows/` | **Moves** to `controller/internal/workflows/` (the logic under test moves with it) | moves with engine |

### The controller's new dependency: `pgxpool`

The controller today has **no** PG access. `grep -r 'pgxpool\|postgres' controller/` returns zero production hits. Every stateful concern it owns (Workspace CRD status, free-models ConfigMap, relay orphan detection) flows through the K8s API server, not Postgres. This migration adds the first PG dependency.

**Scoping the access tightly is the design rule.** The controller's pool must be:

| Table set | Access | Rationale |
|---|---|---|
| `workflow_runs`, `workflow_node_runs`, `trigger_fires` | **read + write** | Claim runs, persist node transitions, write fire rows |
| `workflows`, `triggers`, `webhooks`, `webhook_deliveries` | **read-only** | Read spec snapshots, trigger config, webhook secrets for routine execution |
| `session_origins` | **read + write** | Routine executor tags preserved sessions |
| **Everything else** (`workspaces`, `users`, `orgs`, `credentials`, `audit_log`, `session_index`, ...) | **NO ACCESS** | The controller must not become a general PG client. A DB role with table-level grants enforces this. |

**Enforcement mechanism (not advisory):** the controller's pool connects with a dedicated PG role (`lsp_controller`) granted `SELECT` on the read-only set and `SELECT, INSERT, UPDATE` on the read-write set — nothing else. `REVOKE` on `public`. This makes the boundary a DB-level constraint, not a code-review hope. The pool is constructed with this role's DSN; the controller binary never sees the migration/superuser DSN. This mirrors how the API's `pgxpool` is already role-scoped — we are extending an existing discipline, not inventing one.

### Multi-replica safety: leader election + SKIP LOCKED, together

The v1 engine in the API relies on `FOR UPDATE SKIP LOCKED` for multi-replica safety precisely *because* the API has no leader election (`engine.go:10-11`). The controller does: every `manager.Runnable` with `NeedLeaderElection() == true` runs only on the leader (`freemodels/refresher.go:179-185`, `relay/orphan_detector.go:70-73`).

After migration, **both mechanisms coexist** — and they should:

| Mechanism | What it protects | Kept after migration? |
|---|---|---|
| Leader election (`NeedLeaderElection`) | Only one controller replica starts the reconciler/scheduler goroutines at all | **Yes — primary** |
| `FOR UPDATE SKIP LOCKED` on `ClaimQueuedRuns` | Even within the leader, two claim-batch ticks don't double-claim; survives a leader handover race where the old leader's goroutine hasn't fully stopped | **Yes — defense-in-depth** |

Leader election is the top-level gate (the whole engine runs on one replica). `SKIP LOCKED` is the row-level atomic gate that makes a leader-handover window safe — during the few seconds where two replicas might both believe they are running, neither can corrupt a run because the row claim is atomic. Dropping `SKIP LOCKED` would reintroduce the TOCTOU class Epic 23 hardened against; it stays.

---

## Migration Plan

### Phase 1 — Controller gains PG access (1 day)

- Add a `pgxpool` constructor to the controller's startup path (`controller/main.go`), reading a `DATABASE_URL` (or the same secret the API reads). Wire it with the scoped `lsp_controller` role.
- Add the role to the helm chart's init/seed SQL (or a new migration) with the exact grant matrix above. `make chart-sync-migrations`.
- Land a minimal read smoke test in the controller (e.g. a `workflows_max_per_owner` query) to prove the pool is alive and scoped. No engine code yet.

### Phase 2 — Move the engine package (1–2 days)

- Create `controller/internal/workflows/`. Move `engine.go` → `reconciler.go` + `scheduler.go` (the two were already one struct each; the split makes the package readable). Move `engine_test.go` alongside.
- The `ReconcilerStore` / `SchedulerStore` interfaces (`engine.go:48-75`) are already interface-based — the controller satisfies them with the same `pkg/workflows/store.go` implementation, now constructed against the controller's pool. **No store rewrite.** This is the payoff of the interface seam the v1 code already has.
- Register both as `manager.Runnable` via `mgr.Add(...)` in `controller/main.go`, mirroring `freemodels/refresher.go:291` exactly:

```go
// controller/main.go (sketch — not committed code)
if err := mgr.Add(&workflows.Reconciler{
    Store:   workflows.NewPGStore(dbPool),
    Client:  mgr.GetClient(),
    Agentd:  agentdClient,   // existing K8s-based agentd caller
    Metrics: metrics,
}); err != nil { /* ... */ }

if err := mgr.Add(&workflows.Scheduler{
    Store:     workflows.NewPGStore(dbPool),
    Activator: workspaceActivator,   // existing controller path
    Redis:     redisClient,
}); err != nil { /* ... */ }
```

Both structs already implement `Start(ctx) error` and need only the `NeedLeaderElection() bool` method — a 3-line addition each, copied from `refresher.go:183`.

### Phase 3 — Cut the API over (0.5 day)

- Delete `go a.wfReconciler.Start(...)` / `go a.wfScheduler.Start(...)` and the surrounding block at `app.go:1325-1341`.
- Delete `api/internal/workflows/engine.go` and `engine_test.go` (now in the controller).
- The CRUD handlers in `api/internal/handlers/workflows.go`, `triggers.go`, `webhook_triggers.go` stay untouched — they only wrote rows before and still only write rows.
- Update `engine.go`'s successor header comment (now in the controller) to reflect the corrected rationale.

### Phase 4 — Verify + harden (1 day)

- `make test` (the moved tests run against the controller package now).
- E2E (US-64.13 scenarios) re-run against the controller-hosted engine: cron fire, webhook fire, manual run, workspace-dies-mid-node, cancel, single-in-flight reject, circuit-breaker trip, missed-fire skip-log.
- Confirm the `lsp_controller` role cannot write a `workspaces` row (negative test — connect with the controller DSN, attempt an out-of-scope write, expect permission denied).

### Rollback

If the controller-hosted engine misbehaves in production: revert the controller deploy (the API CRUD still writes rows), re-add the `app.go:1325-1341` goroutine block, and re-deploy the API. The schema is unchanged by this migration — there is no data migration to roll back. The window during which *both* API and controller could run the engine is the risk; gate it by feature flag (an env var that gates the controller's `mgr.Add` calls) so the deploy order is: API keeps engine → controller gains engine → flag flip → API drops engine.

---

## What Does NOT Change

- **No schema migration.** The tables (`workflow_runs`, `workflow_node_runs`, `trigger_fires`, `triggers`, `workflows`, `webhooks`, `session_origins`) are identical. The controller reads/writes the same rows the API did.
- **No store rewrite.** `pkg/workflows/store.go`'s query implementations are pool-agnostic; they take a `pgxpool.Pool` (or `pgx.Tx`) and are reused as-is. The interface seam (`engine.go:48-75`) was the enabler.
- **No API contract change.** Every REST endpoint (`POST /workflows/:id/runs`, webhook receiver, CRUD) behaves identically. SDKs and the frontend are unaffected.
- **No SSE/metrics change.** The reconciler already publishes via the existing `eventbroker` and Prometheus registry; those clients are injected, not bound to the API process. They move with the engine.
- **Run-to-completion / fail-on-restart semantics unchanged.** The controller marking `running` runs `failed (api_restart)` on its own restart is the same behaviour, now triggered by a controller restart instead of an API restart — rarer (the controller is not under request-serving memory pressure) but the recovery is identical.

---

## Risks

### R1 — The controller gains its first PG dependency

This is the central architectural change. Until now the controller's stateful concerns lived entirely in K8s (CRDs, ConfigMaps). A PG pool means the controller now depends on Postgres being up for run-driving to function — a dependency the API always had.

**Mitigation:** the `freemodels/refresher.go` precedent already shows the controller tolerating transient upstream outages (`refreshOnce` logs and retries on next tick — `refresher.go:190` comment: "Errors are logged but not returned"). The reconciler applies the same pattern: a failed `ClaimQueuedRuns` is logged and retried next tick; queued runs are not lost, only delayed. PG is already a hard dependency of the API and the product; this does not add a new SPOF to the system, it adds a new *client* of an existing one.

**Scope creep risk:** the danger is "well, now that the controller has a pool, let it also read `users`/`audit_log`/`credentials`." The `lsp_controller` role grant matrix is the hard stop. Any code path that needs a non-workflow table is a sign the migration is leaking its boundary.

### R2 — Leader-election handover window

During a leader change, there is a window where the old leader's engine goroutine may not have fully stopped and the new leader's has started. If both claim runs in that window, `FOR UPDATE SKIP LOCKED` prevents double-claim — but a routine executor (`fireRoutineTarget`) running on both replicas could fire a session message twice.

**Mitigation:** the routine path is idempotent at the `trigger_fires` level (single-transaction fire-row + run-create, same as the webhook receiver's atomicity rule in the v1 README). For routine sessions specifically, the fire-row insert is the gate. This is the same correctness argument the webhook receiver already makes; the migration does not weaken it.

### R3 — Engine goroutine lifetime vs. controller manager

The API started the engine with a bare `go wfReconciler.Start(a.ctx)` (`app.go:1330`). The controller's `manager.Runnable.Start` is lifecycle-managed by controller-runtime: the manager calls `Start` on the leader and cancels its context on shutdown/demotion. This is **better** (graceful drain instead of a process kill), but the engine's current `Start` must honour context cancellation cleanly — it already does (the run loop selects on `<-ctx.Done()`), but the move is the time to audit that every agentd HTTP call and every PG write is context-bound. If a stuck HTTP call ignores its context, the goroutine leaks on leader demotion.

**Mitigation:** the Phase 4 verify step explicitly checks leader-demotion drain: demote the leader, confirm the reconciler's in-flight runs are released (the row stays `running` and is marked `failed (api_restart)` by the new leader's startup sweep).

### R4 — The "secretly stateful" engine header comment misleads future readers

`engine.go:6-11` currently asserts the API is the right home. Leaving that comment in place (even after the move) invites a future contributor to move it back.

**Mitigation:** Phase 3 rewrites the header comment in the controller's new `reconciler.go` with the corrected rationale and a pointer to this design doc.

---

## Adversarial Review

### Weaknesses

1. **"This is churn for no user-visible benefit."** True — no user-facing behaviour changes. The benefit is operational: removing the `api_restart` failure class for runs and restoring the stateless-API contract from README-LLM.md:43. Whether that is worth 3–5 days depends on how often API OOMs are actually killing runs in production. **Decision driver:** if run `error_code=api_restart` appears in the run history at any non-trivial rate, the migration pays for itself; if it is zero, defer.

2. **"The controller is now coupled to PG."** Yes, and that coupling must be bounded. The `lsp_controller` role grant is the enforcement; without it (if someone wires the superuser DSN to "make it work"), the boundary is advisory and erodes. **Hard rule:** the controller's pool DSN is the scoped role, full stop. A CI check (or a unit test asserting the role lacks `INSERT` on `workspaces`) makes the boundary testable.

3. **"Moving `engine_test.go` churns the blame history."** Unavoidable. `git mv` preserves rename detection; the test bodies are unchanged.

### False alarms

- **"FOR UPDATE SKIP LOCKED is now redundant with leader election — remove it."** No. It is defense-in-depth for the leader-handover window (R2). Removing it reintroduces a TOCTOU the codebase has already paid to close. Both stay.

- **"The controller should own workflow/trigger CRUD too, for consistency."** No. CRUD is request-response work that belongs in the stateless, horizontally-scalable API (README-LLM.md:43). Only the stateful run-driver moves. Conflating CRUD with the engine re-introduces stateful work into the API via the back door.

- **"This unblocks multi-replica controllers, so we can scale the engine horizontally."** Leader election means the engine still runs on one replica. Horizontal scaling of run execution is a queueing problem (deferred per D8) and is not what this migration delivers. What it delivers is *correctness under restart*, not throughput.

---

## Effort Estimate

**3–5 days.**

| Phase | Effort |
|---|---|
| 1 — Controller PG pool + scoped role + smoke test | 1 day |
| 2 — Move engine package, register as `manager.Runnable` | 1–2 days |
| 3 — Cut over (delete API goroutines + engine files) | 0.5 day |
| 4 — E2E re-run + boundary negative tests + drain audit | 1 day |

The variance is in Phase 2: if the `AgentdExecutor`/`WorkspaceActivator` dependencies the engine holds need controller-side adapters (the API constructed them from its own clients; the controller constructs them from K8s), wiring those adapters is the 2-day case. If the existing controller already has equivalent callers (likely — it already activates workspaces and talks to agentd for health), it is the 1-day case.

---

## Next Steps

1. Confirm the `api_restart` failure rate in production run history (decision driver for Weakness 1).
2. Land Phase 1 (controller PG pool + scoped role) behind a feature flag that does not yet start the engine.
3. Move the engine in Phase 2; run the moved test suite green in the controller package.
4. Flag-gated cutover in Phase 3; monitor run history for a day; then delete the API-side engine code.
5. Update `engine.go`'s successor header comment and add a cross-reference to this doc.
