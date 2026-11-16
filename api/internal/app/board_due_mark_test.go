// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// board_due_mark_test.go — the #2061/#2065 story-2 consumer pins: the
// fold observer's filter, identity extraction, epoch watermark, and
// queue discipline; and the composite-observer zero-liveness pin (the
// consumption path never stamps the hub's worker index — #2042/R3).
// No sleep-polling: every async assertion rides a channel handoff
// from the recording sink, and the one polling pin (panic
// containment) is a bounded counter window with a hard deadline.

// recordingSink captures dispatches in order.
type recordingSink struct {
	mu   sync.Mutex
	got  []board.TurnEndDueMark
	next chan board.TurnEndDueMark // signals each dispatch (async assertions without sleep-polling)
}

func newRecordingSink() *recordingSink {
	return &recordingSink{next: make(chan board.TurnEndDueMark, 64)}
}

func (r *recordingSink) deliver(_ context.Context, e board.TurnEndDueMark) (bool, error) {
	r.mu.Lock()
	r.got = append(r.got, e)
	r.mu.Unlock()
	r.next <- e
	return true, nil
}

func (r *recordingSink) dispatched() []board.TurnEndDueMark {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]board.TurnEndDueMark, len(r.got))
	copy(out, r.got)
	return out
}

// turnEndFrame builds one BOARD_TURN_END frame (the wire shape: both
// identity keys per #1817).
func turnEndFrame(ws, internal, phrase string, epoch uint64, advanced bool) (string, *abiv1.Event) {
	return ws, &abiv1.Event{
		Type:              abiv1.EventType_EVENT_TYPE_BOARD_TURN_END,
		InternalSessionId: internal,
		SessionId:         phrase,
		BoardTurnEnd:      &abiv1.BoardTurnEnd{Advanced: advanced, Epoch: epoch, TurnDurationMs: 1000},
	}
}

func TestBoardTurnEndConsumer_FiltersForwardsAndExtracts(t *testing.T) {
	sink := newRecordingSink()
	c := newBoardTurnEndConsumer(sink.deliver, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	// Non-turn-end frames never dispatch (the hub's own stamp is the
	// composite's business — pinned separately).
	c.ObserveFrame("ws", &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_PART_DELTA, InternalSessionId: "ses_a"})
	// Typed frame without payload: unattributable.
	c.ObserveFrame("ws", &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_BOARD_TURN_END})
	// No identity keys: unattributable.
	c.ObserveFrame("ws", &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_BOARD_TURN_END,
		BoardTurnEnd: &abiv1.BoardTurnEnd{Epoch: 1}})
	// No workspace: unattributable.
	c.ObserveFrame("", &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_BOARD_TURN_END,
		BoardTurnEnd: &abiv1.BoardTurnEnd{Epoch: 1}, InternalSessionId: "ses_a"})

	c.ObserveFrame(turnEndFrame("ws", "ses_a", "phrase-a", 1, false))

	select {
	case e := <-sink.next:
		assert.Equal(t, "ws", e.WorkspaceID)
		assert.ElementsMatch(t, []string{"ses_a", "phrase-a"}, e.SessionRefs)
		assert.Equal(t, uint64(1), e.Epoch)
		assert.False(t, e.Advanced)
		assert.False(t, e.ObservedAt.IsZero(), "the observation stamp anchors the mint guard")
	case <-time.After(5 * time.Second):
		t.Fatal("the valid turn-end never dispatched")
	}
	assert.Len(t, sink.dispatched(), 1)
}

func TestBoardTurnEndConsumer_EpochWatermarkDropsStaleAndDuplicate(t *testing.T) {
	sink := newRecordingSink()
	c := newBoardTurnEndConsumer(sink.deliver, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	c.ObserveFrame(turnEndFrame("ws", "ses_a", "phrase-a", 3, false))
	<-sink.next

	// Duplicate (same epoch — at-least-once redelivery / resume-fold
	// replay) and stale (older ordinal — the pre-re-claim straggler)
	// both drop at the door.
	c.ObserveFrame(turnEndFrame("ws", "ses_a", "phrase-a", 3, false))
	c.ObserveFrame(turnEndFrame("ws", "ses_a", "phrase-a", 2, false))
	// A DIFFERENT session's epoch space is independent (per-key
	// watermark), and the advance=yes leg still forwards (telemetry).
	c.ObserveFrame(turnEndFrame("ws", "ses_b", "phrase-b", 1, true))

	select {
	case e := <-sink.next:
		assert.Equal(t, "ses_b", e.SessionRefs[0])
		assert.True(t, e.Advanced)
	case <-time.After(5 * time.Second):
		t.Fatal("the foreign-session advance never dispatched")
	}
	assert.Len(t, sink.dispatched(), 2, "duplicate + stale dropped; nothing else")
}

func TestBoardTurnEndConsumer_QueueOverflowDropsNeverBlocks(t *testing.T) {
	sink := newRecordingSink() // NO Run(): the queue fills untouched
	c := newBoardTurnEndConsumer(sink.deliver, nil)

	before := boardDueMarksDropped.Value()
	// Fill the queue (boardTurnEndQueueDepth distinct epochs — the
	// watermark admits strictly-newer ordinals) plus one more.
	for epoch := uint64(1); epoch <= boardTurnEndQueueDepth+1; epoch++ {
		c.ObserveFrame(turnEndFrame("ws", "ses_a", "phrase-a", epoch, false))
	}
	assert.Equal(t, before+1, boardDueMarksDropped.Value(),
		"exactly the over-cap event drops (counted), and the observer returns — never blocks the fold")
	assert.Len(t, sink.dispatched(), 0)
}

// TestBoardTurnEndConsumer_OverflowWhileDraining — the review-r1 race
// pin: overflow latching (the fold goroutine's CompareAndSwap) racing
// the drain goroutine's loud-once reset (Store) — the plain-bool latch
// was an unsynchronized cross-goroutine flag, reddened by -race only
// in THIS interleaving (the drain goroutine alive while the queue
// overflows). Conservation is the convergence assertion: every
// observed event is either dispatched or counted dropped — nothing
// lost, nothing duplicated.
func TestBoardTurnEndConsumer_OverflowWhileDraining(t *testing.T) {
	release := make(chan struct{})
	// Sized for the FULL wave count so the send belt below never fires
	// (a lost dispatch would break conservation — the collector drains
	// continuously; the buffer only absorbs bursts between iterations).
	dispatched := make(chan board.TurnEndDueMark, boardTurnEndQueueDepth+320)
	sink := func(_ context.Context, e board.TurnEndDueMark) (bool, error) {
		<-release // the FIRST dispatch holds the drain: the queue fills behind it
		dispatched <- e
		return true, nil
	}
	c := newBoardTurnEndConsumer(sink, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	before := boardDueMarksDropped.Value()
	// Epoch 1 occupies the drain (blocked in the sink); epochs 2..N
	// fill the queue past capacity — overflow WHILE Run is live.
	const firstWave = uint64(boardTurnEndQueueDepth + 2)
	for epoch := uint64(1); epoch <= firstWave; epoch++ {
		c.ObserveFrame(turnEndFrame("ws", "ses_a", "phrase-a", epoch, false))
	}
	dropsPhase1 := boardDueMarksDropped.Value() - before
	require.GreaterOrEqual(t, dropsPhase1, int64(1),
		"overflow dropped while the drain goroutine was live — counted, never blocked")

	// Open the drain and feed a SECOND wave concurrently: Store(false)
	// (per pickup) races CompareAndSwap (per overflow) — the exact
	// interleaving the plain bool lost.
	go func() {
		for epoch := firstWave + 1; epoch <= firstWave+300; epoch++ {
			c.ObserveFrame(turnEndFrame("ws", "ses_a", "phrase-a", epoch, false))
		}
	}()
	close(release)

	// Convergence by quiescence (bounded): collect dispatches until
	// the channel is quiet, then assert conservation.
	var got int64
	quiet := time.NewTimer(2 * time.Second)
	defer quiet.Stop()
	for {
		select {
		case <-dispatched:
			got++
			quiet.Reset(2 * time.Second)
		case <-quiet.C:
			quiet.Reset(2 * time.Second)
			// One more non-blocking sweep, then settle.
			select {
			case <-dispatched:
				got++
				continue
			default:
			}
			goto settled
		}
	}
settled:
	totalObserved := int64(firstWave + 300)
	totalDropped := boardDueMarksDropped.Value() - before
	assert.Equal(t, totalObserved, got+totalDropped,
		"conservation: every observed event dispatched or counted dropped (got=%d dropped=%d)", got, totalDropped)
}

func TestBoardTurnEndConsumer_RunContainsSinkPanic(t *testing.T) {
	sink := func(context.Context, board.TurnEndDueMark) (bool, error) {
		panic("sink exploded")
	}
	c := newBoardTurnEndConsumer(sink, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	before := boardDueMarksDropped.Value()
	c.ObserveFrame(turnEndFrame("ws", "ses_a", "phrase-a", 1, false))

	// The panic is contained (counted as a dropped dispatch); the
	// drain goroutine survives for the next event. Bounded counter
	// window with a hard deadline — the one legitimate poll.
	deadline := time.After(5 * time.Second)
	for boardDueMarksDropped.Value() == before {
		select {
		case <-deadline:
			t.Fatal("the contained panic was never counted")
		case <-time.After(time.Millisecond):
		}
	}
}

// TestBoardTurnEndConsumption_NeverStampsWorkerIndex — the R3 pin for
// the CONSUMPTION path (#2042 zero-liveness contract, extended): the
// composite frame observer (hub stamp first, consumer second — the
// production wiring) routes a turn-end through both legs; the hub's
// WORKER index stays unstamped under both identity keys (the event is
// opaque-class, story #2064's allowlist pin) while the due-mark
// dispatch fires — the event's ONLY consumer signal.
func TestBoardTurnEndConsumption_NeverStampsWorkerIndex(t *testing.T) {
	hub := newBoardLivenessHub(nil)
	sink := newRecordingSink()
	c := newBoardTurnEndConsumer(sink.deliver, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	composite := chainFrameObservers(hub.ObserveFrame, c.ObserveFrame)
	ws, evt := turnEndFrame("ws", "ses_stale", "phrase-stale", 1, false)
	composite(ws, evt)

	select {
	case <-sink.next:
	case <-time.After(5 * time.Second):
		t.Fatal("the dispatch never fired")
	}
	hub.mu.Lock()
	_, workerRaw := hub.framesWorker["ses_stale"]
	_, workerPhrase := hub.framesWorker["phrase-stale"]
	opaqueRaw := hub.framesOpaque["ses_stale"]
	hub.mu.Unlock()
	assert.False(t, workerRaw, "the turn-end event never stamps the worker index (raw key)")
	assert.False(t, workerPhrase, "the turn-end event never stamps the worker index (phrase key)")
	assert.Positive(t, opaqueRaw, "the hub still sees it as telemetry (opaque class — zero renewal authority)")
	require.Len(t, sink.dispatched(), 1)
}
