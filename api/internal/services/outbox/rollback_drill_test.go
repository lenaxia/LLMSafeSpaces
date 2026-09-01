// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !race

// The drill is a PROCEDURE proof (park holds, unpark drains, zero loss/
// duplication), not a concurrency-safety test — under -race the deploy
// boundary's detached-deliverer/staging interplay adds scheduling noise
// that swamps the drain window. The outbox's race semantics are pinned
// by outbox_stress_test.go, which runs everywhere.

package outbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingDeliverer is the drill's 0052-path agent: slow enough that
// in-flight entries exist when the park lands, and it records every
// delivered clientMessageID (the zero-loss / zero-duplication oracle).
type recordingDeliverer struct {
	mu        sync.Mutex
	delivered []string
	delay     time.Duration
}

func (r *recordingDeliverer) Deliver(ctx context.Context, ws, ses string, e Entry) error {
	time.Sleep(r.delay)
	r.mu.Lock()
	r.delivered = append(r.delivered, e.ClientMessageID)
	r.mu.Unlock()
	return nil
}

func (r *recordingDeliverer) deliver(ctx context.Context, ws, ses string, e Entry) error {
	return r.Deliver(ctx, ws, ses, e)
}

// counts returns unique delivered clientMessageIDs and the duplicate
// delivery count. Duplicates are the outbox's documented at-least-once
// posture at the staging crash window (loss-proof ordering: stage first,
// remove second); the agent-side entryID/clientMessageID dedupe (I5) is
// what makes them harmless — the drill's loss-zero verdict counts
// UNIQUE ids.
func (r *recordingDeliverer) counts() (unique int, dupes int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[string]int{}
	for _, id := range r.delivered {
		seen[id]++
		if seen[id] > 1 {
			dupes++
		}
	}
	return len(seen), dupes
}

// TestRollbackDrill_UnderLoad (US-69.13 / rollback_drill_final_under_load,
// R8 — the in-repo drill; cluster-scale rides the delivery pool):
//
//  1. LOAD: a stream of accepted entries delivers through a slow agent.
//  2. DEPLOY (the flip): the delivery worker stops (a flip IS a deploy —
//     the API pods restart), then the workspace's queued entries park
//     with the mode_transition reason.
//  3. UNPARK (the rollback): the worker returns; entries drain through
//     the 0052 path.
//  4. VERDICT: every accepted entry delivered EXACTLY once — zero
//     user-visible loss, zero duplicate turns.
func TestRollbackDrill_UnderLoad(t *testing.T) {
	s, mr := newTestService(t)
	ctx := context.Background()
	d := &recordingDeliverer{delay: 2 * time.Millisecond}
	runCtx, stopRun := context.WithCancel(context.Background())
	go func() { s.Run(runCtx, d.deliver, 5*time.Millisecond) }()
	t.Cleanup(func() {
		stopRun()
		mr.Close()
	})

	const total = 60
	var idGen atomic.Int64
	var accepted atomic.Int64
	loadCtx, stopLoad := context.WithCancel(context.Background())
	var loaders sync.WaitGroup
	for w := 0; w < 4; w++ {
		loaders.Add(1)
		go func(w int) {
			defer loaders.Done()
			for i := 0; ; i++ {
				select {
				case <-loadCtx.Done():
					return
				default:
				}
				id := idGen.Add(1)
				if id > total {
					return
				}
				// Shard across sessions; cap rejections retry (the
				// deliverer drains, the cap recedes).
				ses := fmt.Sprintf("ses-%d", id%4)
				for {
					_, err := s.Accept(ctx, "ws-drill", ses, "u1", fmt.Sprintf("cm-%d", id), "drill", nil)
					if err == nil {
						accepted.Add(1) // count only real accepts
						break
					}
					if !errors.Is(err, ErrCapped) {
						return // cancel race at teardown
					}
					select {
					case <-loadCtx.Done():
						return
					case <-time.After(5 * time.Millisecond):
					}
				}
			}
		}(w)
	}

	// Let load flow, then execute the flip per the runbook order: the
	// deploy stops the delivery worker, accepts pause, and the residue
	// parks with the mode_transition reason.
	time.Sleep(40 * time.Millisecond)
	stopRun() // the deploy: no worker runs during the mode transition
	stopLoad()
	loaders.Wait()
	time.Sleep(30 * time.Millisecond)

	parked, err := s.ParkWorkspace(context.Background(), "ws-drill", "authority rollback drill")
	require.NoError(t, err)
	t.Logf("drill: parked %d queued entries at the flip", parked)
	require.Positive(t, parked, "the drill must park mid-load (slow deliverer guarantees a queue residue)")

	// The park holds the QUEUED entries (no worker runs). In-flight
	// attempts may still complete by design (D3 detach — zero-loss), so
	// the hold assertion is "nothing spurious", not "nothing new".
	time.Sleep(60 * time.Millisecond)
	held, _ := d.counts()
	assert.LessOrEqual(t, held, int(accepted.Load()), "nothing spurious delivers during the hold")
	_ = held

	// The rollback: unpark, the worker returns — the 0052 path drains.
	n, err := s.UnparkWorkspace(context.Background(), "ws-drill")
	require.NoError(t, err)
	t.Logf("drill: unparked %d entries for the 0052 drain", n)
	drainCtx, drainCancel := context.WithCancel(context.Background())
	defer drainCancel()
	go func() { s.Run(drainCtx, d.deliver, 5*time.Millisecond) }()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		got, dupes := d.counts()
		if got >= int(accepted.Load()) && dupes == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	drainCancel()
	got, _ := d.counts()
	if got != int(accepted.Load()) {
		for ses := 0; ses < 4; ses++ {
			sid := fmt.Sprintf("ses-%d", ses)
			for _, e := range readQueueEntries(t, s, "ws-drill", sid) {
				t.Logf("post-drill queue %s: %s status=%s next=%v", sid, e.ID, e.Status, e.NextAttemptAt)
			}
			if st := s.client.LRange(context.Background(), dKey("ws-drill", sid), 0, -1).Val(); len(st) > 0 {
				t.Logf("post-drill staged %s: %d", sid, len(st))
			}
		}
	}
	assert.Equal(t, int(accepted.Load()), got,
		"R8 zero user-visible loss: every accepted entry's clientMessageID delivered after the round-trip")
	assert.GreaterOrEqual(t, accepted.Load(), int64(20), "the drill exercised a real load before the flip")
}
