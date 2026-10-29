// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// session_notifier_store_test.go — session idle-subscriptions v1 §4.1:
// the durable registry's own pins. Round-trip through the real file
// (the shortids-replica pattern, plus the fsync the outbox contract
// adds), crash-safety (atomic-or-absent: a torn file boots empty, a
// completed write never leaves tmp residue), the pure graph predicates
// the guards consume (cycle DFS, cap), and the outbox lifecycle.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateSubscriptionEvents(t *testing.T) {
	t.Run("empty defaults to both kinds", func(t *testing.T) {
		evs, err := validateSubscriptionEvents(nil)
		require.NoError(t, err)
		assert.Equal(t, []string{subscriptionEventIdle, subscriptionEventError}, evs)
	})

	t.Run("dedupes in first-seen order", func(t *testing.T) {
		evs, err := validateSubscriptionEvents([]string{"error", "idle", "error"})
		require.NoError(t, err)
		assert.Equal(t, []string{"error", "idle"}, evs)
	})

	t.Run("closed vocabulary", func(t *testing.T) {
		_, err := validateSubscriptionEvents([]string{"idle", "busy"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported subscription event")
	})
}

func TestSessionSubscriptionStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session-subscriptions.json")
	s := newSessionSubscriptionStore(path)

	now := time.Now().UTC()
	row := s.subscribe("ses_broadcaster", "ses_subscriber", []string{subscriptionEventIdle}, now)
	require.NotNil(t, row)
	s.subscribe("ses_other", "ses_subscriber", []string{subscriptionEventIdle, subscriptionEventError}, now)
	s.setLatch("ses_broadcaster", sessionLatch{LastDerivedBusy: true, SawBusy: true})
	s.setLatch("ses_other", sessionLatch{LastDerivedBusy: false, SawBusy: true})
	s.enqueue(pendingNotification{Broadcaster: "ses_broadcaster", Subscriber: "ses_subscriber",
		Event: subscriptionEventIdle, EnqueuedAt: now})

	// Reload from disk: rows, latches, and outbox survive together (one
	// atomic document — the whole point of the single-file shape).
	reloaded := newSessionSubscriptionStore(path)
	assert.Equal(t, s.allRows(), reloaded.allRows())
	assert.Equal(t, 2, len(reloaded.allRows()))
	assert.Equal(t, sessionLatch{LastDerivedBusy: true, SawBusy: true}, reloaded.latchFor("ses_broadcaster"))
	assert.Equal(t, sessionLatch{LastDerivedBusy: false, SawBusy: true}, reloaded.latchFor("ses_other"))
	assert.Equal(t, sessionLatch{}, reloaded.latchFor("ses_unknown"))
	assert.Equal(t, 1, reloaded.outboxLen())

	// Unsubscribe persists too.
	require.True(t, reloaded.unsubscribe("ses_broadcaster", "ses_subscriber"))
	again := newSessionSubscriptionStore(path)
	assert.Equal(t, 1, len(again.allRows()))
	assert.Zero(t, again.rowsForBroadcaster("ses_broadcaster"))
}

func TestSessionSubscriptionStoreCrashSafety(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session-subscriptions.json")

	s := newSessionSubscriptionStore(path)
	s.subscribe("ses_a", "ses_b", []string{subscriptionEventIdle}, time.Now())

	// Atomic-or-absent: a completed write leaves no tmp residue, the
	// file parses, and the mode is the private one.
	assert.NoFileExists(t, path+".tmp")
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var doc subscriptionFile
	require.NoError(t, json.Unmarshal(raw, &doc))
	assert.Equal(t, 1, len(doc.Rows))

	// A crash-torn file (truncated mid-document) boots EMPTY — the
	// only readable states are complete ones; subscriptions
	// re-register, the failure mode is a lost wake-up, never a wrong
	// one.
	torn := append([]byte{}, raw[:len(raw)/2]...)
	require.NoError(t, os.WriteFile(path, torn, 0o600))
	fromTorn := newSessionSubscriptionStore(path)
	assert.Empty(t, fromTorn.allRows())
	assert.Zero(t, fromTorn.outboxLen())

	// An absent file boots empty (fresh pod).
	fromAbsent := newSessionSubscriptionStore(filepath.Join(dir, "missing.json"))
	assert.Empty(t, fromAbsent.allRows())
}

func TestSessionSubscriptionStoreLifecycleOps(t *testing.T) {
	s := newSessionSubscriptionStore(filepath.Join(t.TempDir(), "session-subscriptions.json"))

	// Idempotent re-subscribe updates the event set in place.
	s.subscribe("ses_b", "ses_s1", []string{subscriptionEventIdle}, time.Now())
	row := s.subscribe("ses_b", "ses_s1", []string{subscriptionEventIdle, subscriptionEventError}, time.Now())
	require.NotNil(t, row)
	assert.Equal(t, []string{subscriptionEventIdle, subscriptionEventError}, s.allRows()[0].Events)
	assert.Equal(t, 1, s.subscriberCount("ses_b"))

	// Absent-edge unsubscribe is a clean false (§5 idempotence).
	assert.False(t, s.unsubscribe("ses_b", "ses_nobody"))

	// Outbox: enqueue → dismiss matching, keep non-matching.
	p1 := pendingNotification{Broadcaster: "ses_b", Subscriber: "ses_s1", Event: subscriptionEventIdle, EnqueuedAt: time.Now()}
	p2 := pendingNotification{Broadcaster: "ses_b", Subscriber: "ses_s2", Event: subscriptionEventIdle, EnqueuedAt: time.Now()}
	s.enqueue(p1)
	s.enqueue(p2)
	assert.Equal(t, 2, s.outboxLen())
	assert.True(t, s.dismiss(p1))
	assert.False(t, s.dismiss(p1)) // second dismiss of the same intent: nothing left
	assert.Equal(t, 1, s.outboxLen())

	// rows()/rowsFor* return private copies.
	got := s.rowsForSubscriber("ses_s1")
	require.Equal(t, 1, len(got))
	got[0].Events = nil
	assert.NotNil(t, s.allRows()[0].Events)
}

func TestSessionSubscriptionStoreWouldCreateCycle(t *testing.T) {
	s := newSessionSubscriptionStore(filepath.Join(t.TempDir(), "session-subscriptions.json"))
	now := time.Now()

	// Wake flow is broadcaster → subscriber: B watches A (edge A→B),
	// C watches B (edge B→C).
	s.subscribe("ses_a", "ses_b", nil, now)
	s.subscribe("ses_b", "ses_c", nil, now)

	// Safe: D watches A — no path back.
	assert.False(t, s.wouldCreateCycle("ses_a", "ses_d"))
	// Safe: C watches A — the wake from A reaches C but stops there.
	assert.False(t, s.wouldCreateCycle("ses_a", "ses_c"))

	// Mutual A↔B: A watching B closes the loop (B's settle wakes A,
	// A's settle wakes B, forever).
	assert.True(t, s.wouldCreateCycle("ses_b", "ses_a"))
	// The longer cycle: A watching C (C → … → B → … closes on A).
	assert.True(t, s.wouldCreateCycle("ses_c", "ses_a"))
	// The trivial self edge is a cycle by definition (refused earlier
	// with its own sentinel).
	assert.True(t, s.wouldCreateCycle("ses_a", "ses_a"))
}

func TestSessionSubscriptionStoreSubscriberCapData(t *testing.T) {
	s := newSessionSubscriptionStore(filepath.Join(t.TempDir(), "session-subscriptions.json"))
	now := time.Now()
	for i := 0; i < maxSubscribersPerBroadcaster; i++ {
		s.subscribe("ses_broadcaster", fmt.Sprintf("ses_sub_%02d", i), nil, now)
	}
	assert.Equal(t, maxSubscribersPerBroadcaster, s.subscriberCount("ses_broadcaster"))
}

func TestSessionSubscriptionStorePruneSessions(t *testing.T) {
	s := newSessionSubscriptionStore(filepath.Join(t.TempDir(), "session-subscriptions.json"))
	now := time.Now()
	s.subscribe("ses_gone_b", "ses_s1", nil, now)
	s.subscribe("ses_b2", "ses_gone_s", nil, now)
	s.subscribe("ses_b2", "ses_s2", nil, now)
	s.setLatch("ses_gone_b", sessionLatch{LastDerivedBusy: true, SawBusy: true})
	s.setLatch("ses_b2", sessionLatch{LastDerivedBusy: false, SawBusy: true})
	s.enqueue(pendingNotification{Broadcaster: "ses_gone_b", Subscriber: "ses_s1",
		Event: subscriptionEventIdle, EnqueuedAt: now})
	s.enqueue(pendingNotification{Broadcaster: "ses_b2", Subscriber: "ses_gone_s",
		Event: subscriptionEventIdle, EnqueuedAt: now})

	gone := map[string]bool{"ses_gone_b": true, "ses_gone_s": true}
	dropped := s.pruneSessions(func(id string) bool { return gone[id] })
	assert.Equal(t, 2, dropped)

	remaining := s.allRows()
	require.Equal(t, 1, len(remaining))
	assert.Equal(t, "ses_b2", remaining[0].Broadcaster)
	assert.Equal(t, "ses_s2", remaining[0].Subscriber)
	assert.Equal(t, sessionLatch{}, s.latchFor("ses_gone_b"))
	assert.Equal(t, sessionLatch{LastDerivedBusy: false, SawBusy: true}, s.latchFor("ses_b2"))
	assert.Zero(t, s.outboxLen(), "both intents touch a gone session")

	// Nil-safe / nil predicate.
	assert.Equal(t, 0, s.pruneSessions(nil))
}

func TestSessionSubscriptionStoreNilSafe(t *testing.T) {
	var s *sessionSubscriptionStore
	assert.Nil(t, s.allRows())
	assert.False(t, s.hasRow("a", "b"))
	assert.Zero(t, s.subscriberCount("a"))
	// A nil registry refuses every edge — the safe direction for the
	// cycle guard (Subscribe refuses earlier on the missing store).
	assert.True(t, s.wouldCreateCycle("a", "b"))
	assert.Equal(t, sessionLatch{}, s.latchFor("a"))
	assert.False(t, s.unsubscribe("a", "b"))
}
