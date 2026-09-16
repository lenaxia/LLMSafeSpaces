// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package outbox

// Recover race pin (epic-71 flake-storm-outbox): Recover snapshot the
// staging list BEFORE acquiring the session lock, then requeued from
// that stale snapshot once the lock was finally acquired. An entry that
// COMPLETED in between (restored to main, verified, claimed — OnDelivered
// fired once, both lists clean) was no longer in main at requeue time,
// passed the inMain ID-dedupe, and was pushed back as a verifying zombie:
// a later pass verified and claimed it again — OnDelivered fired twice
// for exactly one send. This is the 2026-09-16 main-CI
// TestStress_AmbiguityStormMultiReplica failure (run 35038952063), and a
// live production boot race (every replica start runs Recover while
// peer replicas deliver).

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// shrinkRecoverTimers keeps the delivery/verify cycles tight. LockTTL is
// irrelevant here (no expiry is involved); what matters is
// sweepLockRetryEvery — Recover's lock retry cadence — held LONG enough
// that the test's synchronous steps land deterministically inside one
// retry sleep.
func shrinkRecoverTimers(t *testing.T) {
	t.Helper()
	orig := [8]time.Duration{
		DeliveryTimeout, VerifyDelay, VerifyBackoff, MaxVerifyBackoff,
		RetryBackoff, MaxBackoff, LockTTL, sweepLockRetryEvery,
	}
	DeliveryTimeout = 10 * time.Millisecond
	VerifyDelay = time.Millisecond
	VerifyBackoff = time.Millisecond
	MaxVerifyBackoff = time.Millisecond
	RetryBackoff = time.Millisecond
	MaxBackoff = time.Millisecond
	sweepLockRetryEvery = 200 * time.Millisecond
	t.Cleanup(func() {
		DeliveryTimeout, VerifyDelay, VerifyBackoff, MaxVerifyBackoff = orig[0], orig[1], orig[2], orig[3]
		RetryBackoff, MaxBackoff, LockTTL = orig[4], orig[5], orig[6]
		sweepLockRetryEvery = orig[7]
	})
}

// TestRecover_UnderLockSnapshotNeverResurrectsCompletedEntry: W1 stages
// and sends an entry (blocked mid-deliver, holding the session lock);
// Recover reads the staging snapshot and parks on the lock; the entry
// then completes normally (restore → verify → claim → one fire); Recover
// finally acquires and must NOT requeue from its pre-lock snapshot —
// requeueing resurrects the completed entry as a verifying zombie and
// fires OnDelivered a second time for one send.
func TestRecover_UnderLockSnapshotNeverResurrectsCompletedEntry(t *testing.T) {
	shrinkRecoverTimers(t)
	s, _ := newTestService(t)

	var sends, fired sync.Map // text → *int32
	bump := func(m *sync.Map, text string) {
		v, _ := m.LoadOrStore(text, new(int32))
		atomic.AddInt32(v.(*int32), 1)
	}
	count := func(m *sync.Map, text string) int32 {
		v, _ := m.LoadOrStore(text, new(int32))
		return atomic.LoadInt32(v.(*int32))
	}

	entered, release := make(chan struct{}), make(chan struct{})
	deliver := func(_ context.Context, _, _ string, e Entry) error {
		bump(&sends, e.Text) // persist-before-turn
		close(entered)
		<-release
		return Ambiguous(errors.New("context deadline exceeded"))
	}
	s.SetVerifier(func(_ context.Context, _, _ string, _ Entry) Verdict {
		return VerdictDelivered // the transcript has it after the send
	})
	s.SetOnDelivered(func(_, _ string, e Entry) { bump(&fired, e.Text) })

	ctx := context.Background()
	const text = "recover-race"
	if _, err := s.Accept(ctx, "ws-rr", "ses-rr", "u-1", "cm-rr", text, nil); err != nil {
		t.Fatalf("accept: %v", err)
	}

	// W1: stage + send, then block mid-deliver holding the session lock.
	w1Done := make(chan bool, 1)
	go func() { w1Done <- s.DeliverOnce(ctx, "ws-rr", "ses-rr", deliver) }()
	<-entered

	// Recover: reads the staging snapshot (entry visible mid-delivery),
	// fails its first lock acquire, and sleeps one (long) retry period.
	recDone := make(chan int, 1)
	go func() { recDone <- s.Recover(ctx) }()
	time.Sleep(50 * time.Millisecond) // Recover is now parked in its retry sleep

	// The entry completes while Recover sleeps: restore → verify →
	// claim → OnDelivered fires exactly once.
	close(release)
	if !<-w1Done {
		t.Fatal("W1 pass must process the entry")
	}
	if !s.DeliverOnce(ctx, "ws-rr", "ses-rr", func(_ context.Context, _, _ string, e Entry) error {
		return errors.New("unreached: entry is verifying by now")
	}) {
		t.Fatal("verify pass must complete the entry")
	}
	if got := count(&fired, text); got != 1 {
		t.Fatalf("entry fired %d times at completion, want 1", got)
	}
	if entries, _ := s.List(ctx, "ws-rr", "ses-rr"); len(entries) != 0 {
		t.Fatalf("outbox must be empty after completion, has %d", len(entries))
	}

	// Recover wakes, acquires the now-free lock, and makes its requeue
	// decision against the stale pre-lock snapshot.
	<-recDone

	// Drain any zombie: drive passes until the outbox empties. A
	// resurrected zombie re-verifies (transcript confirms) and claims —
	// firing OnDelivered a second time for the single send.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if entries, err := s.List(ctx, "ws-rr", "ses-rr"); err == nil && len(entries) == 0 {
			break
		}
		s.DeliverOnce(ctx, "ws-rr", "ses-rr", deliver)
		time.Sleep(time.Millisecond)
	}
	if entries, _ := s.List(ctx, "ws-rr", "ses-rr"); len(entries) != 0 {
		t.Fatalf("outbox did not drain: %d entries remain", len(entries))
	}

	if got := count(&sends, text); got != 1 {
		t.Fatalf("entry sent %d times, want 1", got)
	}
	if got := count(&fired, text); got != 1 {
		t.Fatalf("OnDelivered fired %d times for one send, want 1 — Recover requeued a completed entry from its pre-lock snapshot", got)
	}
}
