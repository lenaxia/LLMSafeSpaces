// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// session_notifier_reconcile_test.go — the PVC-relocation and boot-time
// missed-settle reconciliation pins (owner ruling 2026-11-14): the
// registry's default home is the platform/ PVC subPath (survives pod
// recreation), the legacy /sandbox-runtime document migrates exactly
// once, and a settle whose notification was in flight (debounce or
// outbox) at a hard kill is delivered EXACTLY once on the next boot —
// never when the per-edge delivered-watermark is current.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/lenaxia/llmsafespaces/pkg/eventbus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// ---- the store's homes, pinned (the dev_preview_headers pattern) ----

// TestSessionSubscriptionStorePathsPinned: the default registry lives
// on the PVC-backed platform/ dir (the seq cursor's durability
// envelope — pod recreation must not wipe the edges); the legacy
// /sandbox-runtime path is the one-shot migration source.
func TestSessionSubscriptionStorePathsPinned(t *testing.T) {
	assert.Equal(t, "/platform/session-subscriptions.json", defaultSessionSubscriptionsPath,
		"the registry default must live under the platform/ PVC subPath (sessionstate.DefaultPlatformDir), not the /sandbox-runtime emptyDir")
	assert.Equal(t, "/sandbox-runtime/session-subscriptions.json", legacySessionSubscriptionsPath,
		"the legacy path is the pre-PVC home — the one-shot migration source")
}

// ---- the one-shot legacy migration -----------------------------------

// TestSessionSubscriptionStoreLegacyMigrationAdoptsOnce: an absent PVC
// file plus a present legacy document adopts it — rows, latches, and
// outbox load, the new location is written, the legacy copy stays on
// disk, and a second load (new file now present) does not
// double-register.
func TestSessionSubscriptionStoreLegacyMigrationAdoptsOnce(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "sandbox-runtime", "session-subscriptions.json")
	newPath := filepath.Join(dir, "platform", "session-subscriptions.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(legacyPath), 0o750))

	// The pre-upgrade generation's durable state, written at the legacy
	// home by the OLD binary.
	settled := time.Now().UTC().Add(-time.Minute)
	legacy := subscriptionFile{
		Rows: []*SubscriptionRow{{
			Broadcaster: "ses_worker", Subscriber: "ses_manager",
			Events: []string{subscriptionEventIdle}, CreatedAt: settled.Add(-time.Hour),
		}},
		Latches: map[string]*sessionLatch{"ses_worker": {LastDerivedBusy: false, SawBusy: true,
			LastSettleAt: settled, LastSettleKind: subscriptionEventIdle}},
		Outbox:  []*pendingNotification{{Broadcaster: "ses_worker", Subscriber: "ses_manager", Event: subscriptionEventIdle, EnqueuedAt: settled}},
		Version: 1,
	}
	raw, err := json.Marshal(legacy)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(legacyPath, raw, 0o600))

	// Boot with the PVC file absent: the legacy document is adopted.
	s := newSessionSubscriptionStoreWithLegacy(newPath, legacyPath)
	require.Len(t, s.allRows(), 1)
	assert.Equal(t, "ses_manager", s.allRows()[0].Subscriber)
	assert.True(t, s.latchFor("ses_worker").SawBusy)
	assert.Equal(t, 1, s.outboxLen())
	assert.FileExists(t, newPath, "the adoption persists to the new home")
	assert.FileExists(t, legacyPath, "the legacy copy stays in place (readable, never re-read)")

	// The next boots load the NEW file only — no double registration
	// while the legacy copy remains on disk.
	again := newSessionSubscriptionStoreWithLegacy(newPath, legacyPath)
	assert.Len(t, again.allRows(), 1)
	assert.Equal(t, s.allRows(), again.allRows())
	assert.Equal(t, 1, again.outboxLen())

	// A legacy document at an UNPARSEABLE state boots empty (the
	// advisory posture) and does not adopt.
	torn := dir2(t)
	tornLegacy := filepath.Join(torn, "legacy.json")
	tornNew := filepath.Join(torn, "new.json")
	require.NoError(t, os.WriteFile(tornLegacy, raw[:len(raw)/2], 0o600))
	fromTorn := newSessionSubscriptionStoreWithLegacy(tornNew, tornLegacy)
	assert.Empty(t, fromTorn.allRows())
	assert.NoFileExists(t, tornNew, "a torn legacy document is not adopted")
}

// dir2 mints a second temp dir (the migration suite's two-document
// fixtures).
func dir2(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// ---- reload carries the watermark contract ---------------------------

// TestSessionSubscriptionStoreReloadCarriesWatermarks: edges written
// pre-restart (reload from the PVC path) are present after reload —
// rows, delivered-watermarks, and settle stamps together (the whole
// reconciliation comparison survives the round-trip).
func TestSessionSubscriptionStoreReloadCarriesWatermarks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session-subscriptions.json")
	s := newSessionSubscriptionStore(path)

	now := time.Now().UTC()
	// Both edges exist BEFORE any settle (the kind-mismatch isolation:
	// neither carries an initialization watermark).
	s.subscribe("ses_worker", "ses_manager", []string{subscriptionEventIdle}, now)
	s.subscribe("ses_worker", "ses_error_only", []string{subscriptionEventError}, now)
	settled := now.Add(time.Second)
	s.setLatch("ses_worker", sessionLatch{LastDerivedBusy: false, SawBusy: true,
		LastSettleAt: settled, LastSettleKind: subscriptionEventIdle})
	s.advanceSettleWatermarksForKind("ses_worker", subscriptionEventIdle, settled)

	reloaded := newSessionSubscriptionStore(path)
	require.Len(t, reloaded.allRows(), 2)
	rows := map[string]SubscriptionRow{}
	for _, row := range reloaded.allRows() {
		rows[row.Subscriber] = row
	}
	assert.Equal(t, settled, rows["ses_manager"].LastDeliveredSettle,
		"the delivered-watermark survives the reload")
	assert.True(t, rows["ses_error_only"].LastDeliveredSettle.IsZero(),
		"a kind-mismatched edge's watermark is untouched")
	latch := reloaded.latchFor("ses_worker")
	assert.Equal(t, settled, latch.LastSettleAt)
	assert.Equal(t, subscriptionEventIdle, latch.LastSettleKind)

	// Forward-only: an older stamp never regresses the watermark.
	s.advanceSettleWatermarksForKind("ses_worker", subscriptionEventIdle, settled.Add(-time.Hour))
	assert.Equal(t, settled, s.rowsForSubscriber("ses_manager")[0].LastDeliveredSettle)
}

// ---- the boot-time reconciliation, unit level -------------------------

// stampMissedSettle installs the durable end state of a hard kill
// inside the debounce window: the settle edge persisted (busy clear,
// saw-busy, settle stamp) while the edge's delivered-watermark still
// trails it.
func stampMissedSettle(s *sessionSubscriptionStore, broadcaster string, settled time.Time) {
	s.setLatch(broadcaster, sessionLatch{LastDerivedBusy: false, SawBusy: true,
		LastSettleAt: settled, LastSettleKind: subscriptionEventIdle})
}

// TestReconcileMissedSettlesFiresOnceWhenTrailing: watermark trails the
// settle stamp ⇒ EXACTLY one reconciled notification, watermark
// advanced to the stamp, and a second pass is silent.
func TestReconcileMissedSettlesFiresOnceWhenTrailing(t *testing.T) {
	f := newNotifierFixture(t, nil)
	subscribeForWake(t, f, "ses_worker", "ses_manager", []string{subscriptionEventIdle})
	settled := time.Now().UTC().Add(-time.Minute)
	stampMissedSettle(f.store, "ses_worker", settled)

	f.n.ReconcileMissedSettles(context.Background())
	notice := waitForNotice(t, f.deliver.ch)
	assert.Equal(t, "ses_worker", notice.Broadcaster)
	assert.Equal(t, "ses_manager", notice.Subscriber)
	assert.Equal(t, subscriptionEventIdle, notice.Event)
	assert.Equal(t, settled, notice.EnqueuedAt, "the notice carries the missed settle's stamp")
	require.Eventually(t, func() bool { return f.store.outboxLen() == 0 },
		2*time.Second, 5*time.Millisecond, "the successful reconciled delivery cleared its intent")

	// The watermark advanced to the stamp: a second pass fires nothing.
	assert.Equal(t, settled, f.store.rowsForSubscriber("ses_manager")[0].LastDeliveredSettle)
	f.n.ReconcileMissedSettles(context.Background())
	assertNoNotice(t, f.deliver.ch, 100*time.Millisecond, "a current watermark never re-fires")
}

// TestReconcileMissedSettlesSilentWhenCurrent: current watermark, a
// kind the edge does not subscribe to, and a stamp-less latch (legacy
// pre-watermark latch, or no settle ever observed) are all silent.
func TestReconcileMissedSettlesSilentWhenCurrent(t *testing.T) {
	t.Run("watermark current", func(t *testing.T) {
		f := newNotifierFixture(t, nil)
		subscribeForWake(t, f, "ses_worker", "ses_manager", []string{subscriptionEventIdle})
		settled := time.Now().UTC().Add(-time.Minute)
		stampMissedSettle(f.store, "ses_worker", settled)
		f.store.advanceSettleWatermarksForKind("ses_worker", subscriptionEventIdle, settled)

		f.n.ReconcileMissedSettles(context.Background())
		assertNoNotice(t, f.deliver.ch, 100*time.Millisecond, "a delivered settle is never re-fired")
	})

	t.Run("kind not subscribed", func(t *testing.T) {
		f := newNotifierFixture(t, nil)
		subscribeForWake(t, f, "ses_worker", "ses_error_only", []string{subscriptionEventError})
		stampMissedSettle(f.store, "ses_worker", time.Now().UTC().Add(-time.Minute))

		f.n.ReconcileMissedSettles(context.Background())
		assertNoNotice(t, f.deliver.ch, 100*time.Millisecond, "an idle settle never wakes an error-only edge")
	})

	t.Run("stampless latch", func(t *testing.T) {
		f := newNotifierFixture(t, nil)
		subscribeForWake(t, f, "ses_worker", "ses_manager", nil)
		f.store.setLatch("ses_worker", sessionLatch{LastDerivedBusy: false, SawBusy: true})

		f.n.ReconcileMissedSettles(context.Background())
		assertNoNotice(t, f.deliver.ch, 100*time.Millisecond, "a pre-watermark legacy latch carries no missed settle")
	})
}

// TestReconcileMissedSettlesFreshSubscribeDoesNotWakeOnOldSettle: an
// edge registered AFTER a settle must not be woken by it — the fresh
// row's watermark initializes to the broadcaster's current settle
// stamp.
func TestReconcileMissedSettlesFreshSubscribeDoesNotWakeOnOldSettle(t *testing.T) {
	f := newNotifierFixture(t, nil)
	settled := time.Now().UTC().Add(-time.Minute)
	stampMissedSettle(f.store, "ses_worker", settled)

	// Subscribe AFTER the settle (the session sits idle).
	subscribeForWake(t, f, "ses_worker", "ses_late_manager", []string{subscriptionEventIdle})
	assert.Equal(t, settled, f.store.rowsForSubscriber("ses_late_manager")[0].LastDeliveredSettle,
		"a fresh edge starts current")

	f.n.ReconcileMissedSettles(context.Background())
	assertNoNotice(t, f.deliver.ch, 100*time.Millisecond, "pre-subscription idleness is not this edge's to deliver")
}

// TestReconcileMissedSettlesHardKillDuringDebounce: THE window, end to
// end across a simulated generation death. Generation one observes
// busy→idle live — the settle edge persists the stamp and arms a
// debounce that NEVER fires (the long window; the quiesce cancel is
// the kill). Generation two reloads the registry from disk and
// reconciles: exactly one delivery, and only one.
func TestReconcileMissedSettlesHardKillDuringDebounce(t *testing.T) {
	path := storePath(t)

	// Generation one: a debounce longer than the test — armed, never
	// fired.
	gen1Deliver := newChanDeliverer()
	gen1 := newSessionNotifier(sessionNotifierConfig{
		Store:    newSessionSubscriptionStore(path),
		Facts:    mapSessionFacts{subtasks: map[string]bool{}, gone: map[string]bool{}},
		Debounce: time.Hour,
		Deliver:  gen1Deliver,
		Logger:   zap.NewNop(),
	})
	_, err := gen1.Subscribe("ses_worker", "ses_manager", []string{subscriptionEventIdle})
	require.NoError(t, err)
	gen1.handleFrame(statusFrame("ses_worker", abiv1.SessionStatus_SESSION_STATUS_BUSY))
	gen1.handleFrame(statusFrame("ses_worker", abiv1.SessionStatus_SESSION_STATUS_IDLE))

	// The settle edge is durable: busy cleared, saw-busy kept, the
	// settle stamped — and the debounce still armed (never fired).
	latch := gen1.store.latchFor("ses_worker")
	require.False(t, latch.LastDerivedBusy)
	require.True(t, latch.SawBusy)
	require.Equal(t, subscriptionEventIdle, latch.LastSettleKind)
	require.False(t, latch.LastSettleAt.IsZero())
	assertNoNotice(t, gen1Deliver.ch, 150*time.Millisecond, "the long debounce must not fire")
	gen1.quiesce() // the hard kill inside the debounce window

	// Generation two: fresh store (the PVC reload) + fresh engine. The
	// §7.3 snapshot machinery cannot see this episode (the latch's busy
	// edge was cleared at settle time) — the reconciliation must.
	gen2Deliver := newChanDeliverer()
	gen2 := newSessionNotifier(sessionNotifierConfig{
		Store:    newSessionSubscriptionStore(path),
		Facts:    mapSessionFacts{subtasks: map[string]bool{}, gone: map[string]bool{}},
		Debounce: 25 * time.Millisecond,
		Deliver:  gen2Deliver,
		Logger:   zap.NewNop(),
	})
	t.Cleanup(func() { quiesceNotifier(gen2) })

	// The boot ordering pin: RetryOutbox must not duplicate what
	// ReconcileMissedSettles delivers (no outbox intent exists — the
	// debounce never enqueued).
	require.Zero(t, gen2.store.outboxLen())

	gen2.ReconcileMissedSettles(context.Background())
	notice := waitForNotice(t, gen2Deliver.ch)
	assert.Equal(t, "ses_manager", notice.Subscriber)
	assert.Equal(t, subscriptionEventIdle, notice.Event)

	// Exactly once: the watermark advanced, and no second fire — from
	// the reconciliation OR a later boot.
	settled := gen2.store.latchFor("ses_worker").LastSettleAt
	require.Equal(t, settled, gen2.store.rowsForSubscriber("ses_manager")[0].LastDeliveredSettle)
	gen2.ReconcileMissedSettles(context.Background())
	assertNoNotice(t, gen2Deliver.ch, 150*time.Millisecond, "the missed settle is delivered exactly once")
	reloaded := newSessionSubscriptionStore(path)
	gen3 := newSessionNotifier(sessionNotifierConfig{
		Store: reloaded, Facts: mapSessionFacts{subtasks: map[string]bool{}, gone: map[string]bool{}},
		Debounce: 25 * time.Millisecond, Deliver: newChanDeliverer(), Logger: zap.NewNop(),
	})
	gen3.ReconcileMissedSettles(context.Background())
	assert.Equal(t, settled, reloaded.rowsForSubscriber("ses_manager")[0].LastDeliveredSettle,
		"a third boot's reconciliation stays silent (watermark durable)")
}

// ---- review round 2: the gate/stamp and bus-layer pins -----------------

// TestSettleStampGatedOffByPendingInput (review r2 finding 1): a
// busy→idle flip blocked by a pending QUESTION is NOT a notifiable
// settle — the live path must leave NO durable stamp (a hard kill in
// the blocked state must not arm the boot reconciliation to deliver
// "its turn is complete" for a session the design says is
// incomplete). The answer resolves, the next episode settles, and
// THAT settle stamps and delivers.
func TestSettleStampGatedOffByPendingInput(t *testing.T) {
	f := newNotifierFixture(t, nil)
	subscribeForWake(t, f, "ses_w", "ses_m", nil)

	f.n.handleFrame(statusFrame("ses_w", abiv1.SessionStatus_SESSION_STATUS_BUSY))
	f.n.handleFrame(evtFrame("ses_w", abiv1.EventType_EVENT_TYPE_INPUT_REQUEST, func(e *abiv1.Event) {
		e.Input = &abiv1.InputRequest{Id: "ask_1", Kind: abiv1.InputKind_INPUT_KIND_QUESTION, Question: "which?"}
	}))
	f.n.handleFrame(statusFrame("ses_w", abiv1.SessionStatus_SESSION_STATUS_IDLE))

	latch := f.store.latchFor("ses_w")
	assert.False(t, latch.LastDerivedBusy, "the busy edge cleared")
	assert.True(t, latch.SawBusy)
	assert.True(t, latch.LastSettleAt.IsZero(), "a settle gated off by a pending user input leaves NO durable stamp")
	assertNoNotice(t, f.deliver.ch, f.debounce*4, "a session blocked on a question is not complete")

	// The hard-kill-in-blocked-state boot: the gated settle stays lost
	// BY DESIGN — the boot reconciliation must stay silent.
	f.n.ReconcileMissedSettles(context.Background())
	assertNoNotice(t, f.deliver.ch, 100*time.Millisecond, "a gated settle must never be reconciled into a wake-up")

	// The turn that truly ends: answer → busy → idle stamps and
	// delivers (the watermark comparison rides the real stamp).
	f.n.handleFrame(statusFrame("ses_w", abiv1.SessionStatus_SESSION_STATUS_BUSY))
	f.n.handleFrame(evtFrame("ses_w", abiv1.EventType_EVENT_TYPE_INPUT_RESOLVED, func(e *abiv1.Event) {
		e.Input = &abiv1.InputRequest{Id: "ask_1"}
	}))
	f.n.handleFrame(statusFrame("ses_w", abiv1.SessionStatus_SESSION_STATUS_IDLE))
	notice := waitForNotice(t, f.deliver.ch)
	assert.Equal(t, subscriptionEventIdle, notice.Event)
	require.Eventually(t, func() bool {
		return !f.store.latchFor("ses_w").LastSettleAt.IsZero()
	}, 2*time.Second, 5*time.Millisecond, "the true settle stamps")
	assert.False(t, f.store.rowsForSubscriber("ses_m")[0].LastDeliveredSettle.IsZero(),
		"the delivered settle advanced the watermark")
}

// TestSnapshotReconcileStampGatedOffByPendingInput: the §7.3
// latch-vs-snapshot arm obeys the same discipline — a busy-latched
// session whose fresh snapshot is idle WITH a pending ask clears the
// busy edge but stamps nothing, notifies nobody, and reconciles
// silently at boot.
func TestSnapshotReconcileStampGatedOffByPendingInput(t *testing.T) {
	f := newNotifierFixture(t, nil)
	subscribeForWake(t, f, "ses_w", "ses_m", nil)
	f.store.setLatch("ses_w", sessionLatch{LastDerivedBusy: true, SawBusy: true})

	snap := snapSession("ses_w", false, abiv1.SessionStatus_SESSION_STATUS_IDLE)
	snap.PendingInputs = append(snap.PendingInputs, &abiv1.InputRequest{
		Id: "ask_1", Kind: abiv1.InputKind_INPUT_KIND_QUESTION})
	f.n.handleFrame(podSnapFrame(snap))

	assertNoNotice(t, f.deliver.ch, f.debounce*4, "blocked on the user is not complete")
	latch := f.store.latchFor("ses_w")
	assert.False(t, latch.LastDerivedBusy)
	assert.True(t, latch.LastSettleAt.IsZero(), "the snapshot-reconciled gated settle leaves no stamp")
	f.n.ReconcileMissedSettles(context.Background())
	assertNoNotice(t, f.deliver.ch, 100*time.Millisecond, "silent at boot too")
}

// settleMsgFor builds one bus settle message the way publishSettle
// does (topic + payload with the settle's OWN stamp).
func settleMsgFor(sid, kind, stamp string) eventbus.Message {
	return eventbus.Message{ID: "m_" + sid + "_" + kind + "_" + stamp,
		Event: eventbus.Event{
			Topic:   eventbus.SessionSettleTopic(sid, kind),
			Payload: map[string]string{"session": sid, "settle": kind, "settled_at": stamp},
		}}
}

// TestConsumeSettleEventAdvancesOnOwnStamp (review r2 finding 2): a
// late-landing bus message for settle A must advance the watermark to
// A's OWN stamp — never to the latch's CURRENT value, which by then
// may belong to a later settle B. Jumping to B's stamp would
// permanently silence B at the next boot (the exact loss this
// subsystem never makes).
func TestConsumeSettleEventAdvancesOnOwnStamp(t *testing.T) {
	f := newNotifierFixture(t, nil)
	subscribeForWake(t, f, "ses_w", "ses_m", []string{subscriptionEventIdle})

	// Settle A at T1 — published, delivered, consumed with its stamp.
	t1 := time.Now().UTC().Add(-2 * time.Minute)
	t2 := t1.Add(time.Minute)
	f.store.setLatch("ses_w", sessionLatch{LastDerivedBusy: false, SawBusy: true,
		LastSettleAt: t1, LastSettleKind: subscriptionEventIdle})
	require.NoError(t, f.n.ConsumeSettleEvent(context.Background(),
		settleMsgFor("ses_w", subscriptionEventIdle, t1.Format(time.RFC3339Nano))))
	waitForNotice(t, f.deliver.ch)
	require.Equal(t, t1, f.store.rowsForSubscriber("ses_m")[0].LastDeliveredSettle,
		"consumption advanced the watermark to the message's own stamp")

	// Settle B at T2 (stamped, its debounce armed — not yet published,
	// not yet consumed). A STALE redelivery of A lands now.
	f.store.setLatch("ses_w", sessionLatch{LastDerivedBusy: false, SawBusy: true,
		LastSettleAt: t2, LastSettleKind: subscriptionEventIdle})
	require.NoError(t, f.n.ConsumeSettleEvent(context.Background(),
		settleMsgFor("ses_w", subscriptionEventIdle, t1.Format(time.RFC3339Nano))))
	assert.Equal(t, t1, f.store.rowsForSubscriber("ses_m")[0].LastDeliveredSettle,
		"consuming the OLD message must NOT jump the watermark to the later settle's stamp")
	// The stale redelivery re-selected a (tolerated, by-contract)
	// duplicate delivery of A — selection does not consult the
	// watermark. Drain it; the pin below is B's reconciled notice.
	_ = waitForNotice(t, f.deliver.ch)

	// The hard kill inside B's debounce window: the boot reconciliation
	// must still see the watermark trailing B's stamp and deliver B.
	f.n.ReconcileMissedSettles(context.Background())
	notice := waitForNotice(t, f.deliver.ch)
	assert.Equal(t, subscriptionEventIdle, notice.Event)
	assert.Equal(t, t2, notice.EnqueuedAt, "the reconciled settle is B (stamp T2)")
	assert.Equal(t, t2, f.store.rowsForSubscriber("ses_m")[0].LastDeliveredSettle)
}

// TestConsumeSettleEventWithoutStampSkipsAdvance: a payload with no
// parsable settled_at (older publisher) advances nothing — the
// watermark stays trailing and the worst case is a tolerated
// duplicate reconciliation, never a loss.
func TestConsumeSettleEventWithoutStampSkipsAdvance(t *testing.T) {
	f := newNotifierFixture(t, nil)
	subscribeForWake(t, f, "ses_w", "ses_m", []string{subscriptionEventIdle})
	settled := time.Now().UTC().Add(-time.Minute)
	f.store.setLatch("ses_w", sessionLatch{LastDerivedBusy: false, SawBusy: true,
		LastSettleAt: settled, LastSettleKind: subscriptionEventIdle})

	msg := settleMsgFor("ses_w", subscriptionEventIdle, "")
	delete(msg.Event.Payload, "settled_at")
	require.NoError(t, f.n.ConsumeSettleEvent(context.Background(), msg))
	waitForNotice(t, f.deliver.ch) // the delivery itself still happens
	assert.True(t, f.store.rowsForSubscriber("ses_m")[0].LastDeliveredSettle.IsZero(),
		"a stamp-less payload advances no watermark")
}

// TestReconcileMissedSettlesMultipleSubscribersIsolated: two
// kind-matching edges on one broadcaster each get their own
// reconciled delivery — an earlier row's advance must not absorb its
// sibling's (allRows returns value copies; the per-row decision is
// independent).
func TestReconcileMissedSettlesMultipleSubscribersIsolated(t *testing.T) {
	f := newNotifierFixture(t, nil)
	subscribeForWake(t, f, "ses_w", "ses_m1", []string{subscriptionEventIdle})
	subscribeForWake(t, f, "ses_w", "ses_m2", []string{subscriptionEventIdle})
	settled := time.Now().UTC().Add(-time.Minute)
	stampMissedSettle(f.store, "ses_w", settled)

	f.n.ReconcileMissedSettles(context.Background())
	got := map[string]settleNotice{}
	for i := 0; i < 2; i++ {
		n := waitForNotice(t, f.deliver.ch)
		got[n.Subscriber] = n
	}
	assert.Contains(t, got, "ses_m1")
	assert.Contains(t, got, "ses_m2")
	for _, sub := range []string{"ses_m1", "ses_m2"} {
		assert.Equal(t, settled, f.store.rowsForSubscriber(sub)[0].LastDeliveredSettle,
			"%s's watermark advanced independently", sub)
	}
	f.n.ReconcileMissedSettles(context.Background())
	assertNoNotice(t, f.deliver.ch, 100*time.Millisecond, "both edges current — nothing re-fires")
}

// TestReconcileMissedSettlesFailedDeliveryRidesRetryOutbox: the
// documented boot-leg sequencing (harness-wait → ReconcileMissedSettles
// → RetryOutbox): a reconciled delivery that FAILS leaves its intent
// durable (write-before-send), and the same-boot RetryOutbox delivers
// it exactly once.
func TestReconcileMissedSettlesFailedDeliveryRidesRetryOutbox(t *testing.T) {
	f := newNotifierFixture(t, nil)
	subscribeForWake(t, f, "ses_w", "ses_m", []string{subscriptionEventIdle})
	settled := time.Now().UTC().Add(-time.Minute)
	stampMissedSettle(f.store, "ses_w", settled)
	f.deliver.failOf = func(settleNotice) bool { return true }

	f.n.ReconcileMissedSettles(context.Background())
	require.Equal(t, 1, f.store.outboxLen(), "the failed reconciled delivery left its durable intent")
	assert.Equal(t, settled, f.store.rowsForSubscriber("ses_m")[0].LastDeliveredSettle,
		"the watermark advanced regardless — the retry owns the delivery, not the comparison")

	// The harness heals; the SAME boot's RetryOutbox (the next call in
	// the boot leg's sequence) retries the intent exactly once.
	f.deliver.failOf = nil
	retriedBefore := notifyOutcome(t, "outbox_retried_delivered")
	f.n.RetryOutbox(context.Background())
	notice := waitForNotice(t, f.deliver.ch)
	assert.Equal(t, settled, notice.EnqueuedAt, "the retried intent carries the settle stamp")
	require.Eventually(t, func() bool { return f.store.outboxLen() == 0 },
		2*time.Second, 5*time.Millisecond, "the successful retry cleared the intent")
	assert.InDelta(t, 1, notifyOutcome(t, "outbox_retried_delivered")-retriedBefore, 0,
		"outbox_retried_delivered ticked exactly once")
	assertNoNotice(t, f.deliver.ch, 100*time.Millisecond, "never an unbounded ladder")
}

// ---- review round 3: the publish-leg identity pins ----------------------

// captureBus is a settlePublisher fake for the publish-leg pins: it
// records every published event and runs onPublish INSIDE Publish —
// the hook where a racing fold's durable writes land while the real
// publish does its I/O (the r3 reborrow window).
type captureBus struct {
	mu        sync.Mutex
	events    []eventbus.Event
	onPublish func()
}

func (b *captureBus) Publish(_ context.Context, ev eventbus.Event) (string, error) {
	if b.onPublish != nil {
		b.onPublish()
	}
	b.mu.Lock()
	b.events = append(b.events, ev)
	b.mu.Unlock()
	return "pub_1", nil
}

func (b *captureBus) published() []eventbus.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]eventbus.Event, len(b.events))
	copy(out, b.events)
	return out
}

// newBusLegFixture builds one notifier whose settle leg rides a fake
// Bus (the publish path under test; no consumer group).
func newBusLegFixture(t *testing.T) (*sessionNotifier, *sessionSubscriptionStore, *captureBus) {
	t.Helper()
	store := newSessionSubscriptionStore(storePath(t))
	bus := &captureBus{}
	n := newSessionNotifier(sessionNotifierConfig{
		Store:    store,
		Facts:    mapSessionFacts{subtasks: map[string]bool{}, gone: map[string]bool{}},
		Debounce: 25 * time.Millisecond,
		Deliver:  newChanDeliverer(),
		Bus:      bus,
		Logger:   zap.NewNop(),
	})
	t.Cleanup(func() { quiesceNotifier(n) })
	return n, store, bus
}

// TestPublishSettleCarriesStampAndAdvancesWatermarks (review r3
// finding 2 — the publish leg had ZERO assertion coverage): a settle
// through the bus path publishes settled_at equal to the settling
// stamp and advances the delivered-watermark to it — deleting either
// the payload field or the publish-success advance reddens this pin.
func TestPublishSettleCarriesStampAndAdvancesWatermarks(t *testing.T) {
	n, store, bus := newBusLegFixture(t)
	_, err := n.Subscribe("ses_w", "ses_m", []string{subscriptionEventIdle})
	require.NoError(t, err)

	n.handleFrame(statusFrame("ses_w", abiv1.SessionStatus_SESSION_STATUS_BUSY))
	n.handleFrame(statusFrame("ses_w", abiv1.SessionStatus_SESSION_STATUS_IDLE))
	settled := store.latchFor("ses_w").LastSettleAt
	require.False(t, settled.IsZero(), "the settle edge stamped before arming")

	require.Eventually(t, func() bool { return len(bus.published()) == 1 },
		2*time.Second, 5*time.Millisecond, "the debounce published the settle")
	ev := bus.published()[0]
	assert.Equal(t, eventbus.SessionSettleTopic("ses_w", subscriptionEventIdle), ev.Topic)
	raw, ok := ev.Payload["settled_at"]
	require.True(t, ok, "the payload carries the settle identity")
	stamp, err := time.Parse(time.RFC3339Nano, raw)
	require.NoError(t, err)
	// Instant equality (the parsed form carries no monotonic reading).
	assert.True(t, stamp.Equal(settled), "settled_at equals the settling stamp")
	require.Eventually(t, func() bool {
		return store.rowsForSubscriber("ses_m")[0].LastDeliveredSettle.Equal(settled)
	}, 2*time.Second, 5*time.Millisecond, "the publish-success advance ran")
	assert.Zero(t, store.outboxLen(), "the bus path never enqueues outbox intents")
}

// TestPublishSettleUsesArmingStampNotLaterLatch (review r3 finding 1
// — the publish-stamp reborrow race; r4: the pin rebuilt
// deterministically): the settle's identity is captured at ARMING time
// (armedDebounce.stamp, under the arming critical section) and the
// publish leg must never re-read the latch — a racing fold's durable
// write (the latch re-stamped to a LATER settle) landing between the
// arm and the publish must not leak into the payload or the
// watermark-advance. The interleaving is driven MANUALLY — stop the
// natural timer, re-arm with the arming stamp, land the re-stamp,
// then fire the debounce synchronously — so the pre-fix shape (an
// entry-time latch read inside publishSettle) observes `later` and
// the assertions go RED; the fixed shape is green. Both directions
// deterministic (r4's inert-pin finding closed).
func TestPublishSettleUsesArmingStampNotLaterLatch(t *testing.T) {
	n, store, bus := newBusLegFixture(t)
	deliver := n.cfg.Deliver.(*chanDeliverer)
	_, err := n.Subscribe("ses_w", "ses_m", []string{subscriptionEventIdle})
	require.NoError(t, err)

	// Episode one settles and publishes cleanly first (its debounce
	// fully resolves — a second busy inside the window would be §4.3's
	// legitimate cancel, not the race under test).
	n.handleFrame(statusFrame("ses_w", abiv1.SessionStatus_SESSION_STATUS_BUSY))
	n.handleFrame(statusFrame("ses_w", abiv1.SessionStatus_SESSION_STATUS_IDLE))
	require.Eventually(t, func() bool { return len(bus.published()) == 1 },
		2*time.Second, 5*time.Millisecond, "episode one published")

	// Episode two settles: the settle edge stamps arming2 and arms the
	// natural timer.
	n.handleFrame(statusFrame("ses_w", abiv1.SessionStatus_SESSION_STATUS_BUSY))
	n.handleFrame(statusFrame("ses_w", abiv1.SessionStatus_SESSION_STATUS_IDLE))
	arming2 := store.latchFor("ses_w").LastSettleAt
	require.False(t, arming2.IsZero(), "episode two stamped")
	later := arming2.Add(time.Minute)

	// The race, made deterministic (r4 fix guidance option b): stop the
	// natural timer, re-arm carrying the ARMING stamp, land the racing
	// fold's durable write (the latch re-stamp to `later`) BETWEEN the
	// arm and the fire, then fire synchronously. The orphaned natural
	// timer no-ops on its later fire (the armed entry is deleted).
	n.mu.Lock()
	n.cancelDebounceLocked("ses_w")
	n.armDebounceLocked("ses_w", subscriptionEventIdle, arming2)
	store.setLatch("ses_w", sessionLatch{LastDerivedBusy: false, SawBusy: true,
		LastSettleAt: later, LastSettleKind: subscriptionEventIdle})
	n.mu.Unlock()
	n.debounceFired("ses_w", subscriptionEventIdle)

	events := bus.published()
	require.GreaterOrEqual(t, len(events), 2, "the manual fire published")
	for i, ev := range events {
		stamp, err := time.Parse(time.RFC3339Nano, ev.Payload["settled_at"])
		require.NoError(t, err)
		assert.False(t, stamp.Equal(later),
			"publish %d must never broadcast the mid-flight re-stamp", i)
	}
	last := events[len(events)-1]
	stamp, err := time.Parse(time.RFC3339Nano, last.Payload["settled_at"])
	require.NoError(t, err)
	assert.True(t, stamp.Equal(arming2),
		"the publish broadcasts the ARMING-time stamp, not the mid-flight latch value")
	wm := store.rowsForSubscriber("ses_m")[0].LastDeliveredSettle
	assert.True(t, wm.Equal(arming2),
		"the advance carries the arming-time stamp, not the mid-flight latch value")
	assert.True(t, wm.Before(later), "the later settle is NOT absorbed")

	// The lost-wake-up regression: a hard kill now (latch=later,
	// watermark=arming2) must still reconcile the later settle's
	// wake-up — the pre-fix jump-to-later would have silenced it.
	n.ReconcileMissedSettles(context.Background())
	notice := waitForNotice(t, deliver.ch)
	assert.True(t, notice.EnqueuedAt.Equal(later), "the later settle's wake-up survives")
}
