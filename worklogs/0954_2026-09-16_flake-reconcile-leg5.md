# Worklog: epic-71 flake — TestReconcile_Leg5HarnessDeadMidTurn root cause

**Date:** 2026-09-16
**Session:** flake-reconcile-leg5 — root-cause and fix the intermittent `TestReconcile_Leg5HarnessDeadMidTurn` failure (CI run 35026102272, commit 494d2dad, 2026-09-15T22:01Z). Refs #1311, #1312, #1314.
**Status:** Complete

---

## 1. The failure

CI (`Test (-short, with coverage)`, -race) failed once on main; the test passes most
runs (green ×3 in a prior local assessment). Failure output:

```
--- FAIL: TestReconcile_Leg5HarnessDeadMidTurn (0.00s)
    reconcile_test.go:464: expected ReconcileStats{Promoted:1 ...}
                         actual   ReconcileStats{Promoted:0, TurnEnded:0, Failed:0, BusyCleared:0, EvidenceFailures:0, ...}
    reconcile_test.go:466: expected 3 (LedgerStatePromoted), actual 2 (LedgerStateAdmitted)
                         "harness-dead turn resolves via store evidence"
```

All-zero stats with the row still ADMITTED. Test duration 0.00s — not a
timeout/budget flake; the pass genuinely did nothing to the row.

## 2. Reproduction

- Uncontended `-race -count=50`: green (fast machine, as expected).
- Under CPU contention (2 `nice`-spinners + concurrent test loops): **red within
  `-count=100`** in 2/2 completed loops, byte-identical failure output to CI.
  (A third concurrent loop died on `link: no space left on device` — disk, not test.)
- Post-fix: green under the same contention harness (below).

## 3. Root cause (Rule 7 assumptions stated and validated)

The test drives the real delivery path (`a.deliver.deliver` → `go driveAdmission`)
and then waits for the row to read ADMITTED before asserting one `Reconcile` pass
converges it. The unsynchronized window:

- `attemptAdmission` (ledger.go:765-829) holds the session's single-flight mutex
  (`a.sessionLock("s1")` — injected at authority.go:275) for its whole body;
  `markAdmitted` runs **inside** that scope and the deferred `m.Unlock()` runs only
  at return — a handful of instructions after `transition` releases the ledger
  mutex (post-fsync), at which point the row first becomes pollable as ADMITTED.
- The test's `waitFor` (5 ms poll) can therefore observe ADMITTED while the
  admission goroutine is still inside the lock scope. If the test goroutine then
  outruns the admission goroutine's epilogue (preemption under CI load: -race
  instrumentation, coverage atomics, 2-vCPU runners), `Reconcile`'s
  `lock.TryLock()` (reconcile.go:235) fails and the session is **correctly
  SKIPPED** — the deliberate r1 discipline: mid-admission sessions are never
  resolved on possibly-stale evidence; the next cadence tick converges them.
  Result: `ReconcileStats{}` all zeros + row stays ADMITTED — exactly the CI
  signature, and the ONLY reachable all-zero path for this fixture (every other
  sweep arm yields Promoted/TurnEnded/Failed/EvidenceFailures ≥ 1).

Assumptions validated:

1. *The driver's lock is the authority's `sessionLock` map* — verified
   authority.go:275 (`newDeliveryDriver(ledger, cfg.Admitter, cfg, a.sessionLock)`);
   `TestReconcile_LockedSessionSkippedNotBlocked` (reconcile_test.go:613) already
   relies on the same identity.
2. *Nothing else takes the session lock after the admission releases it* — no
   in-package cadence goroutine exists (the ReconcileCadence ticker lives in the
   agentd wiring; the only goroutines here are the admission driver and the SSE
   subscription pump, authority.go:602, which never touches session locks);
   `act`/`Reseed` are never called in this test.
3. *The TryLock skip is correct, reviewed production behavior* — pinned by
   `TestReconcile_LockedSessionSkippedNotBlocked` and
   `TestReconcile_SerializesWithInFlightAdmission` (head-of-line blocking, the
   30 s `LeaseConvergenceBound`). Production code must NOT change; the test's
   model of leg 5 was wrong (it sampled inside the admission's single-flight
   scope, i.e. "mid-admission", not "harness dead after the turn landed").

## 4. The fix (assertions byte-identical — no weakened pins)

`TestReconcile_Leg5HarnessDeadMidTurn` gains a deterministic happens-after edge on
the single-flight lock between the ADMITTED wait and the asserted pass:

```go
gate := a.sessionLock("s1")
gate.Lock()
gate.Unlock()
```

Because the row only *becomes* ADMITTED via `markAdmitted` under that lock, the
gate either blocks for the admission's release (edge gained) or acquires an
already-free lock that nothing retakes (assumption 2). Leg 5's semantics are now
modeled faithfully: the harness dies AFTER the admission landed, and the asserted
pass observes the post-admission world deterministically.

**Before/after assertion (unchanged, quoted for the reviewer):**

```go
stats := a.Reconcile(context.Background())
assert.Equal(t, ReconcileStats{Promoted: 1}, stats)                      // before == after
row, _ := a.ledger.status("e1", 1)
assert.Equal(t, LedgerStatePromoted, row.State, "harness-dead turn resolves via store evidence")  // before == after
```

The single-pass convergence pin (one pass → Promoted:1, row PROMOTED via store
evidence, within the pass's own deadline — the L5 shape; the 30 s bound remains
the cadence wiring test's) is exactly as strong as before.

## 5. Proof (post-fix)

| Run | Result |
|---|---|
| `-race -count=50` (targeted) | ok, 2.9s — ×2 green (2.5s second round) |
| `-race -count=100` under contention (2 CPU hogs) | ok ×2 (8.8s, 2.9s) |
| no-race `-count=50` (targeted) | ok ×2 (0.7s, 0.8s) |
| full package `go test -race -timeout 600s ./cmd/workspace-agentd/sessionstate/` | ok, 68.0s |

Reproduction stats: red ≤ 100 iterations under contention (2/2 loops) before the
fix; green at 100 (contended) and 50 (both modes) after; full package green.

## 6. Adversarial self-review (Rule 11)

- *Could the gate deadlock if the admission never releases?* No — the waitFor
  already observed ADMITTED, so `markAdmitted` returned; only the epilogue
  remains. The driveAdmission retry ladder cannot re-take the lock
  (attemptAdmission returned true on admission).
- *Does the fix mask a real production bug?* No — the skip is the reviewed r1
  design, separately pinned by two dedicated tests; converging a locked session
  here would violate the stale-evidence discipline (r1-2).
- *Other tests with the same shape?* Audited reconcile_test.go: every other
  Reconcile test uses the synchronous `seedRow` helper (no goroutine);
  `TestReconcile_SerializesWithInFlightAdmission` intentionally tests the skip.
  Leg5 was the only test racing the real deliver→admission epilogue.
- *Is the CI failure signature uniquely explained?* Yes — all-zero stats +
  ADMITTED row is reachable only via the TryLock skip for this fixture (every
  other sweep arm increments a stats counter); reproduced byte-identically under
  contention pre-fix.
- False alarm considered: "budget too tight per #1312" — rejected: the test
  failed in 0.00s with no timer involvement; the #1312 30 s bound is asserted by
  the wiring test (`TestLeaseConvergenceBoundIsExported`) and is untouched.

## 7. Findings / deferred

- Fixed: the leg-5 test sampled inside the admission's single-flight scope.
- Deferred (pre-existing, out of scope, no action required for this flake): none
  identified in this package beyond the disk-pressure environment note below.
- Environment note: pod disk hit 100% during concurrent sibling test builds;
  freed only regenerable caches (/tmp scratch) — `go clean -cache` never run.
