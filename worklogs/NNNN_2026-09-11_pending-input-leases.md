# Worklog: Pending-input leases — diff, status re-derivation, serve refresh (epic-71 / 2a, #1310 slice B)

**Date:** 2026-09-11
**Session:** #1310 slice B: the projection's pending set becomes a lease against harness truth — re-verified on every snapshot serve and on the reconcile cadence — plus BUSY re-derivation from harness status past the lease window.
**Status:** Complete

---

## Objective

S5/S8/L3/L4 for pending inputs: projected asks re-verify against the harness live registries (`/question` + `/permission` via the existing Store seam); dropped asks resolve with events (browsers clear); unknown live asks appear; BUSY never outlives a converged harness-idle past the bound; a gather failure never mutates the projection.

## Work Completed

- **`lease.go` (new)**: `diffPendingLeases(seeds)` — scope = projected-pending ∪ busy ∪ seeds; per session TryLock (1b's concurrency shape; mid-admission sessions skip to the next tick) then `diffSessionLease` under `a.mu` (lock order sessionLock→a.mu, acyclic per the 1a analysis): projected−live → seq'd `INPUT_RESOLVED`; live−projected → seq'd `INPUT_REQUEST` (fold upserts; sessions unknown to the projection materialize through the fold); `rederiveStatusLocked` — truth BUSY + not busy → busy event; truth IDLE/ERROR + busy past `leaseBound()` (default `LeaseConvergenceBound`, test-overridable) → status event; fresh busy-marks hold (events may be late).
- **Serve refresh** (`service.go` GetSnapshot): one store gather per serve, diff that session, then build — a browser refresh clears the stranded prompt (the incident's cure at serve time). Gather failure serves the projection degraded; TryLock failure serves current projection.
- **Cadence** (`reconcile.go`): the lease diff rides 1b's watchdog (`runSessionStateWatchdog` at `ReconcileCadence`) inside `Reconcile` — no new loop, one lease clock. `reconcileLocked` is now ledger-independent (lease-only authorities converge); the cheap no-op gate gained `knownSessionCount` — known sessions stay on the cadence because harness→busy status loss has NO projection-side signal (found by the L4 leg test failing red). `ReconcileStats` gains `LeaseResolved`/`LeaseAppeared`.
- **`markBusy` stamps `busySince`** (the lease-window clock).

## Tests (TDD — red first)

Diff matrix ({A,B}×{B,C}: A resolves with event, C appears with event, B untouched); empty-live empties; gather failure keeps projection; status re-derivation (busy-past-bound → idle; inside-window busy holds; idle+truth-busy → busy); serve refresh clears the stranded ask with exactly one gather (leg 9's no-stampede); degraded serve; leg 1 (silently-dropped ask converges in one cadence pass = L3).

All suites green with `-race`: sessionstate (55s), full agentd (231s), handlers (134s). golangci-lint 0 issues; gofmt/goimports clean.

## Key Decisions

1. **No new seam**: the Store's `SessionStates` (statuses + pending per session) IS the harness truth gather — the same read the reseed uses; the diff is the only new consumer. #1310's `PendingLister` suggestion is satisfied by the existing seam's shape.
2. **Diff through the event fold** (seq'd INPUT_RESOLVED/INPUT_REQUEST/SESSION_STATUS via `applyLocked`) — browsers converge on the fanout, snapshots stay consistent, and no direct projection mutation bypasses I1.
3. **Cadence gate includes known sessions** — the idle-projection/busy-harness direction has no projection-side signal; the gather is the prescribed slow cadence and stays pod-local.

## Fault legs

Leg 1 (ask silently dropped): covered red-green on cadence + serve. Leg 2 (stale click): 1a's resolve-by-absence + the serve/cadence refresh compose — the click cannot strand. Leg 3 (frame loss both hops): the diff is event-independent (store truth), so the leg-1 shape IS leg 3's convergence proof at this layer; the wire-level leg rides the #1312 harness knobs (0c/2b). Leg 9 (refresh storm): one gather per serve, asserted. L3: one cadence pass (15s) ≪ 30s bound; L4: same pass, same bound.

## Next Steps

- Wire-level fault-leg rows 1/2/3/9 against a real agentd process ride the #1312 harness (2b).
- #1302 (4a) lands the API emitter consolidation (`emitPendingInputRequests` → authority snapshot) at its #828 gate; the invariant (one pending source) now holds authority-side.

## Files Modified

- `cmd/workspace-agentd/sessionstate/lease.go` (new)
- `cmd/workspace-agentd/sessionstate/lease_test.go` (new)
- `cmd/workspace-agentd/sessionstate/authority.go` (leaseBoundOverride field)
- `cmd/workspace-agentd/sessionstate/projection.go` (busySince stamp)
- `cmd/workspace-agentd/sessionstate/reconcile.go` (gate + stats + lease-diff call)
- `cmd/workspace-agentd/sessionstate/service.go` (serve refresh)
- `worklogs/NNNN_2026-09-11_pending-input-leases.md` (this file)

---

## CI round 1: the US-69.4 zero-call pins moved with the design

Three CI legs failed on two `abiclient` tests pinning the OLD snapshot contract (`TestGetSnapshot_ZeroOpencodeCalls`, `TestSnapshotLatencyLocal`'s zero-store assertion). #1310 slice B explicitly orders serve-time lease refresh ("pod-local call, O(pending) — cheap"), superseding US-69.4's zero-harness-call read for the pending set: the ask set is a lease, not a cached fact, and the serve is the human-visible staleness moment. The pins are updated to the new contract, which is STRONGER where it matters: `TestGetSnapshot_LeaseRefreshBudget` — exactly ONE gather per serve (no per-part/message stampede) + a divergent ask CONVERGES on the serve (the stranding cure, asserted end-to-end through the real client); `TestSnapshotLatencyLocal` — the 250ms p99 budget unchanged (the gather is localhost and must stay cheap) + 300 serves ⇒ exactly 300 gathers (leg 9's bound). All `pkg/abi/...` green with `-race`; lint 0 issues.

---

## Review round 1 (PR #1329)

All four findings validated real, fixed:

1. **Endpoint-failure-as-empty (blocking):** the wiring's `fetchList` treats /question + /permission failures as authoritative-empty — a transient 5xx would resolve every live ask (a resolve/appear flap, the incident class agent-induced). New `StoreReader.PendingInputs` seam with STRICT semantics (any non-clean read → error → the diff skips; 404/conn-refused are ALSO errors here — indeterminate beats one free skipped tick); the wiring implements it via `fetchListStrict`; the lease diff consumes it. Pinned with the production failure shape (pending-endpoint 503 → projection untouched).
2. **Unbounded serve gather:** the serve's gather now carries `serveGatherTimeout` (2s — tighter than the pass budget: a browser refresh degrades fast, never hostages to a hung store). Pinned by `TestSnapshotServe_HungStoreIsBounded`.
3. **Lease-window hold dead in the ledger topology:** reconciled — 1b's seq-gated evidence sweep is the authoritative busy-clear in production; the window governs ledger-less authorities and is documented as the backstop. Pinned by `TestLeasePass_LedgerWiredCadenceComposes` (ledger-wired: pending converges AND busy clears through the sweep on one pass).
4. **Unfiltered materialization loop:** the rec==nil branch now applies the same nil/empty-ID filter; `TestLeasePass_MaterializesUnknownSession` pins exactly-the-well-formed-ask materializing (malformed entries consume no seq, emit nothing).

Plus the reviewer's missing tests 1–5: production endpoint-failure skip ✓, hung-store bounded serve ✓, ledger-wired topology ✓, unknown-session materialization ✓, concurrent-serve storm — coalesced by a per-Authority serve-gather singleflight (500ms TTL; one in-flight gather, waiters serve the projection; the abiclient storm pin now asserts gathers ≪ serves) ✓. The zero-record L3 gap the reviewer noted is closed by composition: production always reseeds at boot (S8), after which the known-session gate keeps the cadence open — pinned via the Reseed-modeled materialization test.

---

## Review round 2 (PR #1329)

All findings validated real, fixed:

1. **Lease outcomes invisible:** `Metrics()` gains cumulative `LeaseResolved`/`LeaseAppeared`/`LeaseGatherFails`; the metrics bridge exports them as deltas into the `llmsafespaces_ledger_reconciled_total` family (`input_resolved_by_absence`, `input_appeared_from_truth`); the watchdog logs a "pending leases converged" line beside the converged-stranded-state one.
2. **Discarded gather error:** the cadence pass now counts a failed pending gather into `EvidenceFailures` (counter + watchdog Warn — never just the inline line); `Metrics.LeaseGatherFails` carries the cumulative signal.
3. **Unbounded serveGathers pinning full gathers:** entries now hold THIS session's slice only (never the workspace map), the map prunes stale entries past a horizon when it exceeds `serveGatherMapLimit` (4096 — the sessionLimiter's own discipline), and `Close` clears it. Also fixed while here: the TTL cache never hit for pending-less sessions (`nil` slice) — an explicit `cached` flag makes coalescing uniform (the latency-storm pin now genuinely dedupes: 300 serves ≪ 300 gathers).
4. **TTL resurrect:** cached slices are trusted for the RESOLVE half only (`diffSessionResolveOnly`) — a just-resolved ask can never flicker back from a stale cache; fresh gathers apply both halves. Pinned by `TestSnapshotServe_CachedSliceNeverResurrects`.
5. **Stale contract text:** abi.proto's GetSnapshot doc, the client's doc comment, and the latency metric's Help now state the lease-refresh contract (one coalesced pod-local gather, degraded-served on failure); proto regenerated (Go+TS+connect), `buf breaking` green vs frozen.

---

## Review round 3 (PR #1329)

All findings validated real, fixed:

1. **Failure signal missing the scrape surface:** `recordReconcile` moved to the single pass-end site (reconcileLocked records complete stats — ledger sweep + lease diff + failures — so ledger-less authorities and canceled passes record too); `Metrics.LeaseGatherFails` is now consumed by the bridge as `llmsafespaces_lease_gather_failures_total` (delta-of-cumulative). The reseed-embedded sweep records through the same site.
2. **Race on `leaseGatherFails`:** incremented under `a.mu` (the Metrics() read lock — the module convention).
3. **Race on `serveGather.gatheredAt`:** the prune pass reads staleness under `g.mu` (the writer's lock); horizon is `serveGatherPruneHorizon` (var for tests).
4. **Close nil-map panic:** Close swaps a fresh map under the same mutex (never nil) and is once-guarded (idempotent — cleanups and shutdown may both close).
5. **First-scrape drop:** `leaseResolved/Appeared/GatherFail` deltas return the FULL cumulative on first sight (the file's own reconcileDeltas convention — the restart-heal window reaches the series).
6. **Stale Help:** `llmsafespaces_ledger_reconciled_total` Help now enumerates all six outcome labels and their owning issues.

New tests: failing-gather export + concurrent-Metrics race pin, outcomes export, prune/close-safety (internal-package test for the unexported cache). Correction (r4): the canceled-pass recording coverage is #1317's pre-existing `TestReconcile_ContextCancelRecordsOutcomes`, unchanged by this PR — this round's work re-homed its recording site and kept that test green, and r4 adds a canceled-pass-does-not-count-as-gather-failure pin below.
