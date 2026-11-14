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
	"testing"
	"time"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
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
