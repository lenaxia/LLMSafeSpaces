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

## §3 The fix — a join, not a sleep

- `sessionNotifier.legWG sync.WaitGroup` counts IN-FLIGHT timer legs.
  `armDebounceLocked` reserves (`Add(1)`) BEFORE `time.AfterFunc`
  exists — a leg fired the instant AfterFunc returns finds its
  reservation made (Add concurrent with Wait at counter zero is the
  one forbidden shape; the arm path holds `mu` and quiesce's
  cancel-then-check takes the same `mu`, ordering them).
- The leg body's `defer n.legWG.Done()` releases it.
- `cancelDebounceLocked` retracts the reservation when
  `a.timer.Stop()` returns TRUE (proof the leg will never run). The
  Stop-false shapes (fired / running / finished) balance through the
  leg's own defer; a completed leg has already deleted its map entry,
  so a late cancel never double-Dones.
- `sessionNotifier.quiesce()` — cancel every armed timer, `legWG.Wait()`,
  and LOOP until an inspection under `mu` finds the armed map empty
  (an entry whose timer fired stays mapped until its leg takes the
  lock; a fold racing a canceled Run can re-arm one — each pass
  shrinks the set).
- `quiesceNotifier(n)` (fixture t.Cleanup) now calls `n.quiesce()` —
  the deterministic join replaces the sleep. The `debounce` parameter
  is gone; all six call sites updated.

Production semantics unchanged: nothing outside tests waits on the
counter — it is pure bookkeeping on the existing arm/cancel paths.
`Run`'s `defer cancelAllDebounces()` behaves identically (the extra
`Done()` on a winning Stop only balances the counter). No CHANGELOG
entry: test-scoped, prod shutdown semantics untouched.

Failure-path tradeoff, accepted: the join is unbounded. A leg parked
on a blocked deliverer (e.g. the seam server's `block` channel in
TestOutboxWriteBeforeSendClearOnSuccess) hangs quiesce instead of
failing fast — but that shape already means the test is broken, and
the old sleep's "clean" failure raced RemoveAll instead. On every
green path the leg has completed by cleanup time, so the Wait returns
immediately.

## §4 Evidence

- `go test ./cmd/workspace-agentd -run
  'TestSuppressionUnresolvableDestinationNeverSuppresses' -race
  -count=30` — 30/30 PASS (2.3s).
- Notifier family (`TestSuppression|TestTracker|TestDebounce|TestOutbox|
  TestRate|TestNotify|TestDelivery|TestRun|TestAuthority`) `-race
  -count=5` — ok (152s).
- Full `go test ./cmd/workspace-agentd` — ok once (378s).
- `go vet ./cmd/workspace-agentd` clean; `make fmt-check` clean.

## §5 Files

- `cmd/workspace-agentd/session_notifier.go` — legWG field, arm/cancel
  reservation accounting, `quiesce()` (test-support join).
- `cmd/workspace-agentd/session_notifier_test.go` — quiesceNotifier
  rewritten as the join; call sites simplified.
- `cmd/workspace-agentd/session_notifier_delivery_test.go` — call sites.
