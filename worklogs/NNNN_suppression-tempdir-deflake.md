# De-flake TestSuppressionUnresolvableDestinationNeverSuppresses (TempDir RemoveAll vs the debounce leg)

2026-10-30 · branch `deflake-suppression-test` · task tsk-visioned-spangle-hauls
(incident noted in PR #1986's round-2 AI review: execution-evidence residue)

## §1 The incident

One failure of `TestSuppressionUnresolvableDestinationNeverSuppresses`
across 13 local full-suite runs:

```
TempDir RemoveAll cleanup: unlinkat ... directory not empty
```

0/10 on the final loop, 0/8 on clean main — assertion logic fine, the
cleanup not. The reviewer's hypothesis (a test write outliving
RemoveAll) was correct in mechanism; this work pinned the exact writer.

## §2 The writer, identified

The test's TempDirs hold two stores: the short-id replica
(`withReplica`, session_notifier_delivery_test.go:77 — read-only here,
the cold-replica map is empty and `put` is never called) and the
fixture's subscription store (`storePath(t)`). Every store method that
writes does so synchronously under its own mutex — on the CALLING
goroutine. The frames are fed by `handleFrame` directly on the test
goroutine, so every persist up to the settle edge is done before the
test body can return.

The one goroutine that outlives the test body is the **debounce timer
leg**: the IDLE settle edge arms `time.AfterFunc(debounce, …)`
(session_notifier.go, armDebounceLocked), and the leg —

1. `debounceFired` → `collectSettlesLocked` → `store.enqueue` (**persist
   #1**, temp file + fsync + rename + dir fsync, inside the TempDir),
2. `deliverNotices` → `chanDeliverer.DeliverSettle` — the channel send
   is the instant `waitForNotice` returns and the test body ends,
3. `store.dismiss` (**persist #2**, the trailing clear, same
   atomic-rename mechanics).

`waitForNotice` observes step 2; step 3 is still pending. The fixture's
`quiesceNotifier` cleanup backstopped it with `cancelAllDebounces()` +
`time.Sleep(2 × debounce)` — a heuristic: under -race full-suite load a
descheduled leg can be parked between (or inside) the persists for
longer than the sleep. `t.TempDir`'s RemoveAll then lists and unlinks
the directory's entries while the racing persist re-creates
`session-subscriptions.json` (and its `.tmp`) inside it — the
`unlinkat: directory not empty` signature. One-in-thirteen was exactly
this interleaving landing once.

Why this test and not its siblings: every suppression-matrix test
shares the shape (last frame arms a timer that fires and delivers);
the corrupted-input row is simply where the scheduler happened to park
the leg in that run. The fix is class-wide, not row-specific.

## §3 The fix — a gated join, not a sleep

- `sessionNotifier.legWG sync.WaitGroup` counts IN-FLIGHT timer legs.
  `armDebounceLocked` reserves (`Add(1)`) BEFORE `time.AfterFunc`
  exists, under the same `mu` quiesce takes — every live reservation is
  visible to quiesce's critical section.
- The leg body's `defer n.legWG.Done()` releases it.
- `cancelDebounceLocked` retracts the reservation when
  `a.timer.Stop()` returns TRUE (proof the leg will never run). The
  Stop-false shapes (fired / running / finished) balance through the
  leg's own defer; a completed leg has already deleted its map entry,
  so a late cancel never double-Dones.
- `sessionNotifier.quiesce()` — the deterministic join, in two moves:
  1. **the gate**: `quiescing = true` under `mu` permanently refuses
     new arms (`armDebounceLocked` is the ONLY leg creator), so past
     this critical section the in-flight set can only shrink;
  2. **the join**: `legWG.Wait()` waits exactly the legs that existed
     at the gate — pending timers were stopped and retracted above;
     fired legs (running or parked in their deliverer) complete before
     Wait returns.
- `quiesceNotifier(n)` (fixture t.Cleanup) calls it — the deterministic
  join replaces the sleep. The `debounce` parameter is gone; all six
  call sites updated.

Review r1 shaped this: the first cut looped
cancel→Wait→recheck, which the reviewer correctly flagged as still
racy against a concurrent arm (the quiet-check orders the map, not the
Wait — `WaitGroup` Add racing a Wait that already read zero can be
missed, and a fired leg deletes its map entry BEFORE delivering, so an
empty map does not imply a quiet leg). The gate removes the race
structurally instead of polling around it: no leg can be BORN past the
gate, so the one Wait is exact. Corollary pinned below: once quiesce
returns, the engine can never write the store again — RemoveAll is
safe the moment it finishes, not merely probably-now.

Production semantics unchanged: nothing outside tests calls quiesce or
sets the gate — pure bookkeeping on the existing arm/cancel paths.
`Run`'s `defer cancelAllDebounces()` behaves identically. No CHANGELOG
entry: test-scoped, prod shutdown semantics untouched.

Failure-path ergonomics (review r1 R2, the #1986 deadlock-ergonomics
class): the join is unbounded by design, so every cleanup-path
park must be releasable. `seamSrv` gains `releaseBlock()` (close-once)
with a `t.Cleanup` guard in TestOutboxWriteBeforeSendClearOnSuccess —
a require.* failure before the happy-path close now unparks the POST
instead of stalling quiesceNotifier to the package timeout. The join
pin's `blockingDeliverer` carries the same close-once `unblock()`.

## §3a The pins (review r1's hard gate) — born red under mutation

- `TestQuiesceJoinsInFlightDebounceLeg` — a deliverer that parks
  mid-leg (enqueue persisted, dismiss pending); quiesce must NOT
  return while parked, MUST return after release. **M1** (drop
  `legWG.Wait` from quiesce): deterministic red.
- `TestQuiesceRetractsCanceledLegReservation` — arm then cancel inside
  the window (a winning Stop); quiesce must return promptly.
  **M3** (drop the Stop-true `Done`): red — quiesce hangs on the
  leaked reservation.
- `TestQuiesceGatesArmingAfterReturn` — after quiesce returns, a late
  fold (the racing-Run shape) must arm nothing and deliver nothing.
  **M2** (drop the `quiescing` gate): deterministic red — the fold
  arms, the timer fires, the notice lands inside assertNoNotice's
  window.

All three mutations re-checked red on this tree; restored after each.

## §4 Evidence

- `go test ./cmd/workspace-agentd -run
  'TestSuppressionUnresolvableDestinationNeverSuppresses' -race
  -count=30` — 30/30 PASS (both rounds).
- Notifier family (`TestSuppression|TestTracker|TestDebounce|TestOutbox|
  TestRate|TestNotify|TestDelivery|TestRun|TestAuthority|TestQuiesce`)
  `-race -count=5` — ok (152s, round 2).
- Quiesce pins alone `-race -count=5` — 15/15 PASS.
- Full `go test ./cmd/workspace-agentd` — ok once per round (round 2:
  375s).
- Mutations M1 (Wait dropped), M2 (gate dropped), M3 (retraction Done
  dropped): each reddens its pin deterministically; tree restored
  after each (diff-verified).
- `go vet ./cmd/workspace-agentd` clean; `gofmt -l` clean; `make
  fmt-check` clean (round 1).

## §5 Files

- `cmd/workspace-agentd/session_notifier.go` — legWG + quiescing gate
  fields, arm/cancel reservation accounting, `quiesce()` (test-support
  gated join).
- `cmd/workspace-agentd/session_notifier_test.go` — quiesceNotifier
  rewritten as the join; call sites simplified; the three quiesce
  contract pins + blockingDeliverer.
- `cmd/workspace-agentd/session_notifier_delivery_test.go` — call
  sites; seamSrv.releaseBlock close-once + failure-path cleanup guard.
