// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package outbox

// Stress tests (#987): the delivery state machine under sustained
// ambiguity, multi-replica workers, and concurrent accepts. The
// invariant that matters: every accepted entry is SENT exactly once,
// completes exactly once, and never duplicates — even when every single
// send outcome is ambiguous (times out mid-turn) and two worker
// replicas race the same sessions.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stormHarness models the agent: sends persist BEFORE a turn that
// outlives the delivery timeout (opencode's contract), so verification
// against the transcript eventually confirms every send.
type stormHarness struct {
	mu         sync.Mutex
	transcript map[string]int // text → persisted count
	sends      map[string]int // text → POST count (THE duplicate metric)
}

func newStormHarness() *stormHarness {
	return &stormHarness{
		transcript: map[string]int{},
		sends:      map[string]int{},
	}
}

func (h *stormHarness) deliver(_ context.Context, _, ses string, e Entry) error {
	h.mu.Lock()
	h.sends[e.Text]++
	h.transcript[e.Text]++ // persist-before-turn
	h.mu.Unlock()
	time.Sleep(20 * time.Millisecond) // outlives the shrunk DeliveryTimeout
	return Ambiguous(errors.New("context deadline exceeded"))
}

func (h *stormHarness) verify(_ context.Context, _, _ string, e Entry) Verdict {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.transcript[e.Text] > 0 {
		return VerdictDelivered
	}
	return VerdictInconclusive
}

func shrinkStormTimers(t *testing.T) {
	t.Helper()
	orig := [8]time.Duration{
		DeliveryTimeout, VerifyDelay, VerifyBackoff, MaxVerifyBackoff,
		RetryBackoff, MaxBackoff, LockTTL, outboxTickForStress(),
	}
	DeliveryTimeout = 10 * time.Millisecond
	VerifyDelay = 2 * time.Millisecond
	VerifyBackoff = time.Millisecond
	MaxVerifyBackoff = time.Millisecond
	RetryBackoff = time.Millisecond
	MaxBackoff = time.Millisecond
	// Must exceed not just DeliveryTimeout but the WHOLE deliverOne
	// critical section under scheduler noise — staging + deliver +
	// bookkeeping + (same lock hold) verify. 50ms was enough on quiet
	// dev machines but CI runners routinely stall goroutines past it:
	// the lock expires mid-iteration, the second replica's worker
	// acquires it, and the same entry double-fires OnDelivered (observed
	// on the 1.18.15 bump PR's race-detector run). 50× the delivery
	// bound mirrors production's generous LockTTL/DeliveryTimeout ratio.
	LockTTL = 500 * time.Millisecond
	t.Cleanup(func() {
		DeliveryTimeout, VerifyDelay, VerifyBackoff, MaxVerifyBackoff = orig[0], orig[1], orig[2], orig[3]
		RetryBackoff, MaxBackoff, LockTTL = orig[4], orig[5], orig[6]
	})
}

func outboxTickForStress() time.Duration { return 3 * time.Millisecond }

// TestStress_AmbiguityStormMultiReplica: two Run loops (two API
// replicas sharing Valkey), 12 sessions, a producer accepting entries
// throughout, every send ambiguous, every verify eventually confirming.
// Invariants after drain:
//   - every accepted text was POSTed exactly ONCE
//   - OnDelivered fired exactly once per entry
//   - the outbox is empty (nothing parked, nothing lost)
func TestStress_AmbiguityStormMultiReplica(t *testing.T) {
	shrinkStormTimers(t)
	s, _ := newTestService(t)
	h := newStormHarness()
	s.SetVerifier(h.verify)
	var delivered sync.Map // text → count
	s.SetOnDelivered(func(_, _ string, e Entry) {
		v, _ := delivered.LoadOrStore(e.Text, new(int32))
		atomic.AddInt32(v.(*int32), 1)
	})

	const sessions = 12
	const perSession = 6
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	t.Cleanup(func() { // runs before the timer-restore cleanup (LIFO)
		cancel()
		wg.Wait()
	})
	for r := 0; r < 2; r++ { // two replicas
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Run(ctx, h.deliver, 3*time.Millisecond)
		}()
	}

	// Producer: accept entries across sessions while delivery storms.
	total := 0
	for i := 0; i < perSession; i++ {
		for ses := 0; ses < sessions; ses++ {
			text := fmt.Sprintf("storm-%d-%d", ses, i)
			if _, err := s.Accept(ctx, "ws-storm", fmt.Sprintf("ses-%d", ses), "u-1", "cm-"+text, text, nil); err != nil {
				t.Fatalf("accept %s: %v", text, err)
			}
			total++
		}
		time.Sleep(2 * time.Millisecond)
	}

	requireEventually(t, func() bool {
		entries := 0
		for ses := 0; ses < sessions; ses++ {
			list, err := s.List(ctx, "ws-storm", fmt.Sprintf("ses-%d", ses))
			if err != nil {
				return false
			}
			entries += len(list)
		}
		return entries == 0
	}, 15*time.Second, "all entries must drain via verification")

	cancel()
	wg.Wait()

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.sends) != total {
		t.Fatalf("sent %d distinct texts, expected %d", len(h.sends), total)
	}
	for text, n := range h.sends {
		if n != 1 {
			t.Fatalf("text %s was POSTed %d times — duplicate under full ambiguity + multi-replica", text, n)
		}
	}
	dupHooks := 0
	delivered.Range(func(k, v any) bool {
		if atomic.LoadInt32(v.(*int32)) != 1 {
			dupHooks++
			t.Errorf("OnDelivered fired %d times for %v", atomic.LoadInt32(v.(*int32)), k)
		}
		return true
	})
	if dupHooks > 0 {
		t.FailNow()
	}
}

// TestStress_CrashWindowsRecoverUnderLoad: entries seeded into both
// main and staging (the crash window), mixed with clean pending entries
// and error-parked ones; Recover + workers must lose nothing, deliver
// each main-list entry exactly once, and never double-drive staged IDs.
func TestStress_CrashWindowsRecoverUnderLoad(t *testing.T) {
	shrinkStormTimers(t)
	s, _ := newTestService(t)
	h := newStormHarness()
	s.SetVerifier(h.verify)

	ctx := context.Background()
	const pairs = 40
	for i := 0; i < pairs; i++ {
		ses := fmt.Sprintf("ses-c-%d", i%5)
		e, err := s.Accept(ctx, "ws-crash", ses, "u-1", fmt.Sprintf("cm-c-%d", i), fmt.Sprintf("crash-%d", i), nil)
		if err != nil {
			t.Fatalf("accept: %v", err)
		}
		b := string(mustMarshal(*e))
		if i%2 == 0 {
			// Stage-out crash window: staging holds the entry AND main
			// still has it (crash between RPUSH and LREM). Main wins;
			// the worker re-sends (ambiguous) and verifies.
			s.client.RPush(ctx, dKey("ws-crash", ses), b)
		} else {
			// Staged-only window (crash after LREM): the send had
			// already persisted at the agent before the API died —
			// seed the transcript so verification confirms it.
			s.client.LRem(ctx, qKey("ws-crash", ses), 1, b)
			s.client.RPush(ctx, dKey("ws-crash", ses), b)
			h.mu.Lock()
			h.transcript[e.Text]++
			h.mu.Unlock()
		}
	}

	n := s.Recover(ctx)
	if n != pairs/2 {
		t.Fatalf("Recover requeued %d, expected %d (odd entries were staged-only)", n, pairs/2)
	}

	runCtx, cancel := context.WithCancel(ctx)
	var runWG sync.WaitGroup
	t.Cleanup(func() { // runs before the timer-restore cleanup (LIFO)
		cancel()
		runWG.Wait()
	})
	runWG.Add(1)
	go func() {
		defer runWG.Done()
		s.Run(runCtx, h.deliver, 2*time.Millisecond)
	}()

	requireEventually(t, func() bool {
		entries := 0
		for i := 0; i < 5; i++ {
			list, err := s.List(runCtx, "ws-crash", fmt.Sprintf("ses-c-%d", i))
			if err != nil {
				return false
			}
			entries += len(list)
		}
		return entries == 0
	}, 15*time.Second, "all crash-window entries must drain")

	h.mu.Lock()
	defer h.mu.Unlock()
	// Even entries (main+staging) re-send once via the worker; odd
	// entries (staged-only, transcript pre-seeded) complete via
	// verification with ZERO re-sends — the anti-duplicate property.
	if len(h.sends) != pairs/2 {
		t.Fatalf("sent %d distinct texts, expected %d (staged-only entries must NOT re-send)", len(h.sends), pairs/2)
	}
	for text, n := range h.sends {
		if n != 1 {
			t.Fatalf("text %s POSTed %d times after crash-window recovery", text, n)
		}
	}
}

// TestStress_ConcurrentAcceptDedupeAndCap: concurrent accepts of the
// same clientMessageID collapse to ONE entry; distinct cmids respect
// the cap exactly.
func TestStress_ConcurrentAcceptDedupeAndCap(t *testing.T) {
	origCap := Cap
	Cap = 25
	t.Cleanup(func() { Cap = origCap })
	s, _ := newTestService(t)
	ctx := context.Background()

	const racers = 50
	var wg sync.WaitGroup
	var accepted, duplicated atomic.Int32
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var dup *Duplicate
			_, err := s.Accept(ctx, "ws-race", "ses-race", "u-1", "cm-same", "same text", nil)
			switch {
			case err == nil:
				accepted.Add(1)
			case errors.As(err, &dup):
				duplicated.Add(1)
			case errors.Is(err, ErrCapped):
			default:
				t.Errorf("unexpected accept error: %v", err)
			}
		}()
	}
	wg.Wait()

	entries, err := s.List(ctx, "ws-race", "ses-race")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("concurrent same-cmid accepts produced %d entries, want 1", len(entries))
	}
	if accepted.Load() != 1 {
		t.Fatalf("accepted=%d duplicated=%d, want exactly one winner", accepted.Load(), duplicated.Load())
	}

	// Fill to cap with distinct cmids concurrently; overflow must 429-class.
	var capped atomic.Int32
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.Accept(ctx, "ws-race", "ses-race", "u-1", fmt.Sprintf("cm-d-%d", i), "distinct", nil); err == ErrCapped {
				capped.Add(1)
			}
		}(i)
	}
	wg.Wait()
	entries, _ = s.List(ctx, "ws-race", "ses-race")
	if len(entries) != Cap {
		t.Fatalf("queue length %d after flood, want cap %d", len(entries), Cap)
	}
}

func requireEventually(t *testing.T, cond func() bool, timeout time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timeout: " + msg)
}
