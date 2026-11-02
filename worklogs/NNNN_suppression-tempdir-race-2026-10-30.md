# 1234 — Suppression-suite TempDir cleanup race: deterministic debounce-leg join

Task `tsk-visioned-spangle-hauls`. The incident (noted by the AI reviewer in
PR #1986's round-2 review, 2026-10-30): one failure of
`TestSuppressionUnresolvableDestinationNeverSuppresses` across 13 local
full-suite runs — `TempDir RemoveAll cleanup: unlinkat ... directory not empty`
— with 0/10 on the final loop and 0/8 on clean main. Assertion logic fine; the
cleanup raced.

## Root cause

The async writer is the notifier's **debounce timer leg**
(`armDebounceLocked` → `time.AfterFunc(debounce, debounceFired)`): on fire it
runs `collectSettlesLocked` → `store.enqueue` (persist: marshal + file fsync +
rename + **dir fsync**), delivers through the seam, then `store.dismiss`
(**another** fsync-rename persist). The test's `waitForNotice` returns the
moment the notice hits the channel — strictly BETWEEN the two persists — so the
dismiss persist is always potentially in flight when the test body returns.

`quiesceNotifier` "joined" that leg by guessing: `cancelAllDebounces()` +
`time.Sleep(2 × debounce)` (50ms). On a loaded runner one fsync pair can stall
past 50ms, the trailing write then lands inside `t.TempDir`'s RemoveAll
readdir→rmdir window, and `unlinkat` fails ENOTEMPTY. A sleep is a bound, not
a join — the flake is the stall distribution's tail, which is why 18 clean
reruns never reproduced it.

(The `withReplica` short-id store was ruled out: fully synchronous, and with
an empty mapping map it performs zero writes.)

## Fix — join, don't guess

- **`session_notifier.go`** (additive, no behavior change): `sessionNotifier`
  gains `debounceLegs sync.WaitGroup`. `armDebounceLocked` does
  `Add(1)` BEFORE creating the timer and the timer body resolves it in a
  `defer Done()`; `cancelDebounceLocked` ABSORBS the unit when
  `timer.Stop()` returns true (never fired ⇒ body will never run). Stop's
  return value is the exact discriminator between the fired path's Done and
  the cancel path's Done — the counter stays exact, no double-Done, no leak.
  New `waitDebounceLegs()` waits the group. No prod call site changes: `Run`,
  wiring, and main are untouched (bookkeeping only — hence no CHANGELOG entry:
  prod shutdown semantics unchanged).
- **`session_notifier_test.go`**: `quiesceNotifier(n)` drops the sleep and the
  debounce parameter — `cancelAllDebounces()` + `waitDebounceLegs()`, exact.
  `runNotifierOnAuthority` now JOINS the Run goroutine (cancel → bounded 5s
  join → quiesce, via cleanup registration order): arming must be dead before
  waiting, or an arm-while-waiting at counter zero is the WaitGroup reuse
  hazard. `TestRunResubscribesAfterDrop` previously quiesced BEFORE its
  deferred cancel while Run was live — restructured to shutdown order
  (cancel → `<-runDone` → quiesce).
- **`session_notifier_wiring_e2e_test.go`**: `stopConsumer`'s trailing
  `time.Sleep(100ms)` (same guess, same comment, same race class) replaced by
  `notifier.waitDebounceLegs()` — safe there because the loops' WaitGroup join
  has already returned Run.

## Evidence

- `go test -race -count=30 -run TestSuppressionUnresolvableDestinationNeverSuppresses ./cmd/workspace-agentd/` — ok (2.3s).
- Whole notifier family under `-race -count=10` (suppression matrix, tracker,
  outbox, rate, delivery, Run/drop, authority resync) — ok (303s): exercises
  cancel/fire/re-arm interleavings; a WaitGroup miscount would hang, not pass.
- Full `go test -race ./cmd/workspace-agentd/` — ok (423s).
- `make fmt-check`, `go vet` — clean.

## Constraints honored

Conventional commits; worklog (this file); no CHANGELOG (test-scoped fix, prod
change is pure bookkeeping with zero call-site behavior change); no
sleep-polling introduced — two cleanup sleeps removed.
