# Session subscriptions on the PVC + boot-time missed-settle reconciliation

2026-11-14 · branch `fix/subscriptions-pvc-store` · task tsk-quixotic-column-speeds

## §1 The bug

Every session-subscription edge was silently lost on pod recreation.
The registry persisted to `/sandbox-runtime/session-subscriptions.json`
(session_notifier_store.go:45) — but `/sandbox-runtime` is an emptyDir
**tmpfs** (pod_builder.go:265): memory-backed, wiped on every pod
recreation (drain, suspend/resume, eviction). A manager mid
subscribe-then-wait simply never woke. The persistence mechanics
(atomic-rename + fsync writes, the outbox, the quiesce-joined debounce
legs — PR #1986's work) were correct; only the LOCATION was wrong.

## §2 The fix (owner ruling 2026-11-14)

REJECTED alternatives (not implemented): Redis, Postgres/direct-DB,
controller-API registry — a local PVC file matches the lifetime
semantics of the sessions the edges reference.

### 2.1 The relocation

Default path → `/platform/session-subscriptions.json`
(`sessionstate.DefaultPlatformDir` — the platform/ PVC subPath, the
durable seq cursor's exact storage class and durability envelope; the
sidecar mounts it RW via the `workspace` volume's `platform` subPath,
agentd_sidecar.go:247; single-container mode mounts it at
pod_builder.go:525). The constructor best-effort `MkdirAll(0o750)`s the
parent (the `openSeqCursor` precedent).

Migration: on boot, when the PVC file is absent and the legacy
`/sandbox-runtime` document exists, the legacy document is ADOPTED —
loaded, persisted to the new home once; the legacy copy stays on disk,
readable, never re-read (the exchange-dir "readable for one release"
posture). A torn/unparseable legacy boots empty and adopts nothing
(the advisory posture — the next boot can retry once the file is
whole). Hermetic via `newSessionSubscriptionStoreWithLegacy(path,
legacyPath)`; the production wrapper passes the legacy path ONLY for
the production default — explicit test paths never migrate. The
`StorePath` wiring seam (main.go / tests) is unchanged.

### 2.2 The residual window: in-flight-at-kill

The outbox covers a hard death between enqueue (write-before-POST) and
delivery. The UNCOVERED window: the settle fired, the notification sat
in the armed debounce (5s default) when the pod died hard. Durable
state at death: latch busy-edge CLEARED at settle time — so the §7.3
latch-vs-snapshot boot machinery cannot see the episode (a fresh
snapshot reads "was never busy"); outbox empty (debounce never
enqueued). The wake-up was simply gone.

Fix: a per-edge **delivered-watermark** (`SubscriptionRow.
LastDeliveredSettle`) plus the broadcaster latch's **settle stamp**
(`sessionLatch.LastSettleAt/LastSettleKind`), both persisted in the
same file:

- The settle edge stamps the latch BEFORE the debounce arms
  (`observeBusyEdgeLocked`; same for the snapshot-reconciled settle in
  `reconcileAndSwapLocked`).
- Every processed settle advances the watermarks of the broadcaster's
  kind-matching edges — delivered, suppressed, or skipped (final
  outcomes) — on both delivery layers (`collectSettlesLocked` direct
  leg, `ConsumeSettleEvent` bus leg).
- A FRESH edge initializes its watermark to the broadcaster's current
  settle stamp: pre-subscription idleness never wakes a late
  subscriber.
- Boot: `ReconcileMissedSettles` (sequenced in the boot leg AFTER the
  harness wait — facts are fresh, deliveries are possible — and BEFORE
  `RetryOutbox`, so a failed reconciled delivery rides the outbox's
  retry-once within the same boot). Watermark trailing the stamp and
  the event kind matches ⇒ exactly ONE reconciled notification,
  enqueued write-before-send, watermark advanced to the stamp.

### 2.3 The live-vs-boot race (found by the suite, fixed by the guard)

First full-suite run reddenend `TestWiringE2E_DropResubscribeLatch
Reconciliation`: the §7.3 snapshot path stamps the latch and arms the
live debounce in ONE critical section, but its collect (watermark
advance) only runs when the debounce FIRES — 40ms later. The boot
reconcile, running in that gap, saw stamp > watermark and duplicated
the delivery. Guard: an ARMED debounce visible under `mu` means the
live machinery owns the settle — skip WITHOUT advancing (if the
process dies before the leg fires, the next boot's reconciliation sees
the same trailing watermark and delivers then — at-least-once, never
lost). Both orderings (reconcile-first on the pre-resync stampless
latch; resync-first with the arm visible) now deliver exactly once.

## §3 Timing invariants, argued

- **Exactly once per settle, delivered**: watermark advances to the
  same value the latch carries (same read), so post-advance
  `stamp.After(watermark)` is false on every later boot.
- **At-least-once, never lost**: reconcile enqueues the intent BEFORE
  advancing; a death between the two leaves the intent durable for
  `RetryOutbox` (duplicate-tolerant contract) and the trailing
  watermark re-fires on the next boot — a duplicate wake-up, never a
  lost one (§4.5's failure ordering).
- **Clock-independence**: stamp and watermark are the SAME persisted
  value; comparison is equality/after on one writer's lineage, not
  cross-process wall-clock racing.
- **Busy onset erases the stamps** (the `{true,true}` latch write) —
  a settle canceled by re-busy (§4.3 debounce cancel) cannot
  re-fire at boot: the next settle stamps fresh.

## §4 Tests

- `TestSessionSubscriptionStorePathsPinned` — the default/legacy path
  string pins (the dev-preview-headers pattern).
- `TestSessionSubscriptionStoreLegacyMigrationAdoptsOnce` — adoption
  without double-registration; legacy copy stays; torn legacy never
  adopts.
- `TestSessionSubscriptionStoreReloadCarriesWatermarks` — the whole
  comparison state (rows, watermarks, latch stamps) round-trips;
  forward-only advance; kind-mismatched edges untouched.
- `TestReconcileMissedSettlesFiresOnceWhenTrailing` / `...SilentWhen
  Current` / `...FreshSubscribeDoesNotWakeOnOldSettle` — the unit
  truth table.
- `TestReconcileMissedSettlesHardKillDuringDebounce` — THE window end
  to end across three simulated generations (the hour-long debounce
  never fires; the quiesce cancel is the kill).
- `TestWiringE2E_ReconcileMissedSettleOnRestart` — the real restart
  path, real vehicle, real boot-leg placement; a third boot silent.
- Existing tracker/debounce/suppression/outbox/wiring suites as
  regression pins (one latch-equality pin updated for the stamp
  contract).

`make fmt-check` clean; full `go test ./cmd/workspace-agentd/` green
(375s).

## §5 Notes for review

- Metrics: one new §8 label `reconciled_missed_settle` on
  `llmsafespaces_session_notifications_total` (closed taxonomy
  extended in place).
- No controller/helm change: the platform/ subPath is already mounted
  RW for agentd in BOTH modes (the seq cursor's own dependency).
- design/0071 §4 corrected in place with a dated note (the original
  text claimed suspend/resume survival for the emptyDir home — the
  exact claim the bug falsified).

## §6 Review round 1 — the phantom citation (bounce 2026-11-14)

Verdict FAIL (sappy-wasabi-dines), two findings:

1. **major — the wiring e2e citation was true on paper, false in the
   tree.** CHANGELOG:16 and §4 above cited
   `TestWiringE2E_ReconcileMissedSettleOnRestart`; the test was
   authored (the edit reported success, a follow-up suite run passed)
   but the change never reached the file — the commit was assembled
   from a `git status` taken before the append landed, and the diff
   (7 files, no wiring e2e) went out without it. Nobody grepped the
   tree for the cited symbol before pushing. The reviewer did
   (repo-wide: zero hits) and correctly called it fabricated evidence.
   Process fix, now binding on this lane: every cited pin is verified
   against the working tree (grep + git status) before commit. The
   test now exists for real — appended to
   `session_notifier_wiring_e2e_test.go`, `go test -count=5` green
   deterministically (it pins the boot-leg ordering through the real
   `wireSessionNotifier`/`startSessionNotifierLoops` restart path,
   exactly-once through the real vehicle, the armed-debounce guard,
   and third-boot silence). Everything else the reviewer verified
   sound stands unchanged.
2. **minor — govulncheck red on the head, INHERITED from main.**
   GO-2026-6859 (net/http stdlib advisory, fixed in go1.26.11; same
   red on main @ f158f72bd). The Go version is pinned by literals
   across ci.yml (5 sites), security-scan.yml, and the e2e workflows —
   a fleet-wide toolchain roll this lane must not attempt (the
   reviewer assigned it to orchestrator/owner: waiver or a separate
   toolchain-bump PR; repo history shows dedicated toolchain commits).
   Surfaced in the PR description and the adjudication packet. Every
   other check passed on the prior head (Test full-race ~13m, Test
   -short ~15m, Lint, Frontend, ABI schema, Origin plugin, Trivy,
   Gitleaks) and must land green on the new head.

## §7 Review round 2 — the gate/stamp and bus-layer findings (GitHub AI reviewer, head 827e0a003)

The PR workflow's reviewer (mutation-verified, CHANGES_REQUESTED) found
three real weaknesses in the r1 implementation. All addressed:

1. **MAJOR — the boot reconcile bypassed the §4.2 pending-input
   gate.** `observeBusyEdgeLocked` stamped the latch BEFORE
   `settleGatesPassLocked` ran — a busy→idle flip blocked by a pending
   QUESTION left a durable stamp with the watermark trailing forever
   (no arm, no collect). A hard kill in the blocked state — exactly
   the long-lived state where pods get recycled — made the boot
   reconciliation deliver "is now idle — its turn is complete" for a
   session the design says is NOT complete: the wrong-direction
   failure this subsystem never makes. **Fix: stamp only when the
   settle is notifiable** — kind known AND the gates pass (both
   stamping sites: the live settle edge and the §7.3
   snapshot-reconciled arm). The busy clear still always persists; a
   gated settle re-settles when the turn truly ends and stamps then.
   Pinned by `TestSettleStampGatedOffByPendingInput` (live edge, incl.
   the answer→re-settle delivery) and
   `TestSnapshotReconcileStampGatedOffByPendingInput` (snapshot arm).
2. **MAJOR — the bus-path watermark advance could permanently skip an
   unconsumed settle.** `ConsumeSettleEvent` advanced to the latch's
   CURRENT stamp, not the consumed message's settle: consume(A) landing
   after a later settle B stamped T2 jumped every watermark to T2 — a
   hard kill inside B's debounce window then reconciled silent (T2 not
   after T2): B's wake-up permanently lost. **Fix: the advance keys on
   the consumed settle's OWN identity** — `publishSettle` now carries
   `settled_at` (the settle stamp) in the payload, and the consumer
   advances with that parsed value; a stamp-less payload skips the
   advance (trailing watermark → a tolerated duplicate at worst, never
   a loss). Pinned by `TestConsumeSettleEventAdvancesOnOwnStamp`
   (the A→B trace: the stale A consumption must not silence B) and
   `TestConsumeSettleEventWithoutStampSkipsAdvance`.
3. **MINOR — the bus publish→consume duplicate window.** Between
   publish and consumer delivery, a concurrent boot reconciliation
   could deliver the same settle (duplicates are tolerated by
   contract, but the exactly-once claim should hold on both layers).
   **Fix: publish success advances the watermarks to the settle's own
   stamp** — the stream entry is the durable intent on the bus layer,
   so the settle is dispositioned for reconciliation the moment it is
   on the bus (the publish-failure fallback still rides the direct
   leg's collect, which advances on its own).

Missing-case coverage added: the bus-path watermark pins above, the
multi-subscriber per-row isolation pin (`allRows()` copy semantics),
the reconcile-failure → same-boot RetryOutbox retry-once pin
(`TestReconcileMissedSettlesFailedDeliveryRidesRetryOutbox` — the
main.go boot-leg sequencing claim, now tested), and the wiring e2e
comment corrected (the armed-debounce guard is pinned by
`TestWiringE2E_DropResubscribeLatchReconciliation`, not by the
restart test whose latch starts LastDerivedBusy=false).

The govulncheck disposition is unchanged (inherited from main,
GO-2026-6859, owner/orchestrator territory — same red on main @
bb2538ff). Full `go test ./cmd/workspace-agentd/` green at this head
(374.9s); `make fmt-check` clean.

## §8 Review round 3 — the publish-stamp reborrow race and its pin (board reviewer r3, head 73e512648)

1. **MAJOR — publish-stamp-reborrow-race.** `publishSettle` re-read
   the latch stamp AFTER `debounceFired`'s unlock (Publish is I/O, the
   bus leg runs unlocked): a fold completing a full busy→settle
   episode in that window re-stamps the latch, and the in-flight
   publish then broadcast AND advanced the LATER settle's stamp — a
   hard kill inside the still-armed next debounce left
   watermark==stamp and the boot reconcile silent: that wake-up
   permanently lost (r2 finding-2's class reopened through the
   publish-side window). **Fix: the settle's identity is captured at
   ARMING time** — `armedDebounce.stamp`, set under the same mu
   critical section that writes the latch (both arming sites), and
   threaded `debounceFired → publishSettle` (payload + advance) and
   `debounceFired → collectSettlesLocked` (the direct leg's advance
   stops reading the latch too — identity over coincidence). The
   direct leg's collect was already inside one critical section
   (unreachable window) but now carries the same explicit identity.
2. **MAJOR — the publish leg was unpinned.** Zero assertion coverage
   for the publish-success advance and the settled_at payload.
   **Fixed with finding 1's pins (same region, same vehicle):**
   `TestPublishSettleCarriesStampAndAdvancesWatermarks` (a fake Bus
   driven through the real debounce fire: payload settled_at equals
   the settling stamp, the watermark advances post-publish, the outbox
   stays empty) and `TestPublishSettleUsesArmingStampNotLaterLatch`
   (the mid-publish latch re-stamp — the racing fold's durable write
   lands inside Publish — must NOT leak into the payload or the
   advance; the boot reconcile then still fires the later settle's
   wake-up). Both `-count=3` deterministic.
3. **minor — inherited CI red.** `TestPrReviewPromptCIFirstExecutionPolicy`
   (tests/ghaworkflows) failed on both Test jobs — inherited from
   main's own red (2c4f8d682 in the merge base); the owner's fix
   landed as main ccccb58017. Action: the new main tip is merged
   before this push; the green Test runs on the exact final head are
   cited in the handoff. govulncheck GO-2026-6859 remains the
   adjudicated inherited advisory (identical red on main's tip —
   waiver/toolchain-bump stays owner territory).

Full `go test ./cmd/workspace-agentd/` green at this head (375.2s);
`make fmt-check` clean.

### §8.1 Review round 4 — the inert pin, made discriminating (board reviewer r4, head f18197dbf)

The r4 reviewer verified the r3 CODE fix correct and finding 2's pin
real (mutations red), but proved finding 1's pin EMPIRICALLY INERT:
`git checkout 73e512648 -- session_notifier.go` + the new tests passed
on the pre-fix code. Root cause: the pre-fix `publishSettle` read the
latch at ENTRY — strictly before `Publish()` runs — and the re-stamp
hook fired inside `Publish()`, so the pre-fix shape never observed the
later stamp in that interleaving. A full revert of the r3 fix sailed
through green: the race was regression-invisible.

**Fix (the reviewer's option b, manual arm/fire)**: the pin now stops
the natural timer, re-arms under mu carrying the arming stamp, lands
the racing fold's durable write (the latch re-stamp to `later`)
BETWEEN the arm and a direct synchronous `debounceFired` — the exact
interleaving the pre-fix shape lost. The orphaned natural timer no-ops
(entry deleted). Verified discriminating BOTH ways this round, with
the surgical one-line reborrow mutation (signatures intact):

- MUTATED (entry-time latch read restored): RED at four assertions —
  "must never broadcast the mid-flight re-stamp", the arming-stamp
  payload, the arming-stamp advance, and the reconciled wake-up never
  arriving.
- FIXED: green, `-count=3` deterministic.

CHANGELOG's "Pinned by" bullet now enumerates the two publish-leg
pins (the r4 minor, folded into this commit).
