// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"expvar"
	"fmt"
	"sync"
	"time"

	"github.com/lenaxia/llmsafespaces/api/internal/logger"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// board_due_mark.go — #2061/#2065 story 2: the production consumer of
// EVENT_TYPE_BOARD_TURN_END on the usagestream fold. The frame observer
// (installed beside boardLivenessHub.ObserveFrame — the composite rides
// the SAME SetFrameObserver seam, #0069's per-frame hook) filters the
// turn-end type, applies the consumer-side epoch watermark, and
// dispatches ONE guarded due-mark write to the board engine
// (Service.ConsumeTurnEnd — due_mark.go's authority surface).
//
// Zero liveness authority (#2042, R3): this consumer NEVER touches the
// liveness hub — no worker-index stamp, no frame counter, no nudge
// mark. The event stays opaque-class in the hub's own allowlist
// (workerActivityStampsEvidence does not name it, pinned by the #2064
// suite); the ONLY consumer signal is the due-mark. Pinned in
// board_due_mark_test.go: a turn-end routed through the composite
// observer leaves the hub's worker index unstamped while the due-mark
// dispatch fires.
//
// Non-blocking by contract (the OnFrame discipline): the observer is a
// map compare under one mutex plus a buffered enqueue; the DB write
// happens on the drain goroutine (Run). Overflow drops — loudly, via
// the board_due_marks_dropped expvar — and NEVER blocks the fold: a
// dropped event is the R5-safe direction (the sweep backstop owns the
// row; the event path is an accelerator, never a dependency).
//
// Epoch watermark (R4, the consumer half of the epoch-mismatch drop):
// per (workspace, canonical session key), only events with an epoch
// STRICTLY NEWER than the last dispatched one pass. That collapses
// duplicates (at-least-once redelivery — the resume fold replays) and
// drops stale ordinals (an event from before a re-claim). Restart
// semantics: the emitter's opencode-leg epoch counter is process-local
// (story #2064), so an agentd restart mid-session resets epochs to 1
// and the watermark then drops that session's events until its ordinal
// climbs past the pre-restart high — a BOUNDED miss in the safe
// direction (the sweep backstop owns those rows), never a wrong
// due-mark. The map is FIFO-bounded (boardTurnEndEpochEntries): an
// evicted session re-baselines on its next event — one potential
// redundant due-mark, idempotent by the write's own already-due
// predicate.

// boardDueMarksDropped counts events the consumer dropped at the queue
// (overflow — the fold must never block). The expvar posture of
// boardLivenessDegraded: process-visible on /debug/vars.
var boardDueMarksDropped expvar.Int

func init() { expvar.Publish("board_due_marks_dropped", &boardDueMarksDropped) }

const (
	// boardTurnEndQueueDepth bounds the dispatch queue: turn-ends are
	// per-claim-holder settle events (rare by construction — one per
	// worker turn); the depth covers a burst of simultaneous settles.
	boardTurnEndQueueDepth = 256
	// boardTurnEndEpochEntries bounds the watermark map (the hub's
	// boardLivenessMaxEntries posture — process-local maps never grow
	// unbounded). FIFO eviction; see the file header for the safety
	// argument.
	boardTurnEndEpochEntries = 8192
)

// boardTurnEndSink receives the dispatch (production:
// (*board.Service).ConsumeTurnEnd; tests fake it).
type boardTurnEndSink func(ctx context.Context, e board.TurnEndDueMark) (bool, error)

// boardTurnEndConsumer is the fold observer + drain pair.
type boardTurnEndConsumer struct {
	sink   boardTurnEndSink
	logger *logger.Logger
	ch     chan board.TurnEndDueMark

	mu      sync.Mutex
	epochs  map[string]uint64 // canonical session key → last dispatched epoch
	evict   []string          // FIFO basis for the cap
	dropped bool              // overflow latched for one warn (loud-once posture)
}

func newBoardTurnEndConsumer(sink boardTurnEndSink, logger *logger.Logger) *boardTurnEndConsumer {
	// logger may be nil only in tests that never exercise the warn
	// paths; production always passes the app logger. A nil logger is
	// tolerated by never being dereferenced outside the failure paths
	// (the drop counters carry the signal regardless).
	return &boardTurnEndConsumer{
		sink:   sink,
		logger: logger,
		ch:     make(chan board.TurnEndDueMark, boardTurnEndQueueDepth),
		epochs: map[string]uint64{},
	}
}

// ObserveFrame is the usagestream OnFrame observer (the composite's
// second leg). Filter → identity → watermark → enqueue. Cheap by
// contract; it must never block or panic the fold goroutine.
func (c *boardTurnEndConsumer) ObserveFrame(workspaceID string, evt *abiv1.Event) {
	if evt.GetType() != abiv1.EventType_EVENT_TYPE_BOARD_TURN_END {
		return
	}
	payload := evt.GetBoardTurnEnd()
	if payload == nil {
		return // typed frame without payload: unattributable, never a due-mark
	}
	// #1817 identity: the wire id is the phrase, the harness id rides
	// internal — the due-mark matches the row's assignee against EVERY
	// key (the row may hold either form).
	refs := make([]string, 0, 2)
	for _, key := range [...]string{evt.GetInternalSessionId(), evt.GetSessionId()} {
		if key != "" {
			refs = append(refs, key)
		}
	}
	if len(refs) == 0 || workspaceID == "" {
		return
	}
	canonical := refs[0] // internal harness id when present, else the wire id
	key := workspaceID + "|" + canonical

	// The epoch watermark (duplicate / stale-ordinal drop). Strictly
	// newer only — see the file header for the restart semantics.
	c.mu.Lock()
	if last, ok := c.epochs[key]; ok && payload.GetEpoch() <= last {
		c.mu.Unlock()
		return
	}
	c.epochs[key] = payload.GetEpoch()
	c.evict = append(c.evict, key)
	if len(c.evict) > boardTurnEndEpochEntries {
		delete(c.epochs, c.evict[0])
		c.evict = c.evict[1:]
	}
	c.mu.Unlock()

	e := board.TurnEndDueMark{
		WorkspaceID: workspaceID,
		SessionRefs: refs,
		Epoch:       payload.GetEpoch(),
		Advanced:    payload.GetAdvanced(),
		ObservedAt:  time.Now().UTC(),
	}
	select {
	case c.ch <- e:
	default:
		// Overflow: drop (the sweep owns the row — R5's safe
		// direction), count, and warn once until the queue drains.
		boardDueMarksDropped.Add(1)
		if !c.dropped {
			c.dropped = true
			if c.logger != nil {
				c.logger.Warn("board turn-end consumer: dispatch queue overflow — events dropped (the sweep backstop owns the rows)", "workspace", workspaceID)
			}
		}
	}
}

// Run drains the dispatch queue until the context ends. One worker:
// per-session order is preserved (the channel is FIFO, and the
// watermark already collapsed duplicates upstream). A panic in the
// sink is contained and counted — never the process.
func (c *boardTurnEndConsumer) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-c.ch:
			c.dropped = false // queue has headroom again (loud-once reset)
			func() {
				defer func() {
					if r := recover(); r != nil {
						boardDueMarksDropped.Add(1)
						if c.logger != nil {
							c.logger.Error("board turn-end consumer: sink panic contained — dispatch lost (sweep semantics)",
								fmt.Errorf("%v", r), "workspace", e.WorkspaceID)
						}
					}
				}()
				if _, err := c.sink(ctx, e); err != nil {
					// A failed write is a missed acceleration only —
					// the sweep converges the row. Log at debug-adjacent
					// volume: transient pool pressure is routine.
					if c.logger != nil {
						c.logger.Warn("board turn-end consumer: due-mark write failed (sweep backstop owns the row)", "workspace", e.WorkspaceID, "error", err)
					}
				}
			}()
		}
	}
}

// chainFrameObservers composes fold observers into one (the
// SetFrameObserver seam takes a single function; the board wires the
// liveness hub's stamp and the turn-end consumer side by side, call
// order preserved — the hub stamps FIRST, exactly as today, so the
// composite is behavior-identical for every frame the consumer
// ignores).
func chainFrameObservers(fns ...func(workspaceID string, evt *abiv1.Event)) func(workspaceID string, evt *abiv1.Event) {
	return func(workspaceID string, evt *abiv1.Event) {
		for _, fn := range fns {
			if fn != nil {
				fn(workspaceID, evt)
			}
		}
	}
}
