// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate

import (
	"context"
	"testing"
	"time"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// board_turn_end_test.go — #2061/#2064 story 1: the turn-end publication
// for board claim-holder sessions. The event (EVENT_TYPE_BOARD_TURN_END,
// payload BoardTurnEnd{advanced, epoch, turn_duration_ms}) is emitted at
// ONE site only: the harness-attested event application path (Ingest /
// IngestForTest — transport truth from the harness, both in-tree
// harnesses feed it through their parsers). The audit rulings
// (2026-11-15) narrow the epic:
//
//   - EMISSION-SITE RESTRICTION (the anti-#2040 guard): reconcile /
//     settle / reseed paths (clearBusyFromEvidence,
//     rederiveStatusLocked, the reseed flush) NEVER emit — a
//     synthesized settle is not a harness-attested turn end. Born-red
//     pin below.
//   - The advance flag is the seam's answer (the wiring derives it
//     from the MCP board-tool choke point: record_progress | stage
//     move | verdict — presence reads and heartbeats never advance);
//     the authority adds the turn facts (epoch ordinal, duration).
//   - Zero liveness authority (#2042): the event type is opaque-class
//     evidence API-side (pinned in api/internal/app) and the authority
//     itself never stamps any liveness surface — it only publishes.
//
// Degradation: a nil probe (feature unwired / degraded pod) emits
// nothing — the board falls back to sweep semantics (epic R5).

// turnEndProbeStub is the injected seam's test double: it answers the
// board facts for one session's turn window and records the consult.
type turnEndProbeStub struct {
	claimHolder map[string]bool
	advanced    map[string]bool
	panics      bool
	consults    []string
	turnStarts  map[string]time.Time
}

func (p *turnEndProbeStub) probe(sessionID string, turnStart time.Time) BoardTurnEndFacts {
	p.consults = append(p.consults, sessionID)
	if p.turnStarts == nil {
		p.turnStarts = map[string]time.Time{}
	}
	p.turnStarts[sessionID] = turnStart
	if p.panics {
		panic("probe: synthetic seam failure")
	}
	return BoardTurnEndFacts{
		ClaimHolder: p.claimHolder[sessionID],
		Advanced:    p.advanced[sessionID],
	}
}

// newTurnEndAuthority builds an authority with the board turn-end probe
// wired (nil probe when stub is nil — the degraded-mode arm).
func newTurnEndAuthority(t *testing.T, stub *turnEndProbeStub) *Authority {
	t.Helper()
	cfg := Config{
		PlatformDir: t.TempDir(),
		Parser:      &reconcileNopParser{},
		Passwords:   []string{"pw"},
		FastCursor:  true,
	}
	if stub != nil {
		cfg.BoardTurnEndProbe = stub.probe
	}
	a, err := New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// runOneTurn folds a busy→idle pair through the harness boundary
// (IngestForTest IS the harness contract entry for tests — the same
// provenance as Ingest) and returns.
func runOneTurn(t *testing.T, a *Authority, sid string) {
	t.Helper()
	a.IngestForTest(&abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: sid, Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
	a.IngestForTest(&abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: sid, Status: abiv1.SessionStatus_SESSION_STATUS_IDLE})
}

// collectFrames drains up to n frames (or until a short deadline) and
// returns the events among them. Used both to FIND the turn-end
// publication and to prove its ABSENCE (every ingested event frames
// exactly once; anything beyond them would be an emission).
func collectFrames(t *testing.T, frames <-chan *abiv1.StreamFrame, n int) []*abiv1.Event {
	t.Helper()
	var out []*abiv1.Event
	deadline := time.After(2 * time.Second)
	for len(out) < n {
		select {
		case f := <-frames:
			if evt := f.GetEvent(); evt != nil {
				out = append(out, evt.GetEvent())
			}
		case <-deadline:
			return out
		}
	}
	return out
}

// eventsOfType filters collected events by type.
func eventsOfType(evts []*abiv1.Event, typ abiv1.EventType) []*abiv1.Event {
	var out []*abiv1.Event
	for _, e := range evts {
		if e.GetType() == typ {
			out = append(out, e)
		}
	}
	return out
}

// subscribeEvents opens a stream and drops the mandatory snapshot frame.
func subscribeEvents(t *testing.T, a *Authority) <-chan *abiv1.StreamFrame {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	frames, stop, err := a.Stream(ctx)
	require.NoError(t, err)
	t.Cleanup(stop)
	select {
	case <-frames: // snapshot
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot frame never arrived")
	}
	return frames
}

// TestBoardTurnEnd_ClaimHolderNoAdvanceEmits: the happy arm — a
// claim-holder's turn folds busy→idle through the harness boundary with
// no board advance observed in the window; the authority publishes
// EVENT_TYPE_BOARD_TURN_END carrying advanced=false, epoch=1, and a
// positive turn duration.
func TestBoardTurnEnd_ClaimHolderNoAdvanceEmits(t *testing.T) {
	stub := &turnEndProbeStub{claimHolder: map[string]bool{"ses_w1": true}}
	a := newTurnEndAuthority(t, stub)
	frames := subscribeEvents(t, a)

	// The turn spans a real 2ms so the duration payload is observable
	// (a sub-millisecond fold rounds to 0ms — legitimate, unassertable).
	a.IngestForTest(&abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "ses_w1", Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
	time.Sleep(2 * time.Millisecond)
	a.IngestForTest(&abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "ses_w1", Status: abiv1.SessionStatus_SESSION_STATUS_IDLE})

	evts := collectFrames(t, frames, 3) // busy + idle + turn-end
	got := eventsOfType(evts, abiv1.EventType_EVENT_TYPE_BOARD_TURN_END)
	require.Len(t, got, 1, "expected exactly one BOARD_TURN_END, got events: %v", evts)
	pl := got[0].GetBoardTurnEnd()
	require.NotNil(t, pl)
	assert.False(t, pl.GetAdvanced())
	assert.EqualValues(t, 1, pl.GetEpoch())
	assert.Greater(t, pl.GetTurnDurationMs(), int64(0))
	assert.Equal(t, "ses_w1", got[0].GetSessionId(), "the raw harness id rides session_id")
	// The probe consulted with the turn's window opening (busySince).
	require.Len(t, stub.consults, 1)
	assert.Equal(t, "ses_w1", stub.consults[0])
	assert.False(t, stub.turnStarts["ses_w1"].IsZero(), "probe must receive the turn window's start")
}

// TestBoardTurnEnd_AdvanceFlagFromProbe: the same fold with the seam
// reporting an advance-class board call in the window publishes
// advanced=true — the flag is the SEAM's verdict, derived nothing here.
func TestBoardTurnEnd_AdvanceFlagFromProbe(t *testing.T) {
	stub := &turnEndProbeStub{
		claimHolder: map[string]bool{"ses_w1": true},
		advanced:    map[string]bool{"ses_w1": true},
	}
	a := newTurnEndAuthority(t, stub)
	frames := subscribeEvents(t, a)

	runOneTurn(t, a, "ses_w1")

	evts := collectFrames(t, frames, 3)
	got := eventsOfType(evts, abiv1.EventType_EVENT_TYPE_BOARD_TURN_END)
	require.Len(t, got, 1)
	assert.True(t, got[0].GetBoardTurnEnd().GetAdvanced())
}

// TestBoardTurnEnd_UserSessionNeverEmits: R1's claim-holder filter — a
// session the seam does not vouch for (a user chat) never publishes the
// event, though its turn folds identically.
func TestBoardTurnEnd_UserSessionNeverEmits(t *testing.T) {
	stub := &turnEndProbeStub{claimHolder: map[string]bool{}}
	a := newTurnEndAuthority(t, stub)
	frames := subscribeEvents(t, a)

	runOneTurn(t, a, "ses_user")

	evts := collectFrames(t, frames, 2) // busy + idle only
	assert.Empty(t, eventsOfType(evts, abiv1.EventType_EVENT_TYPE_BOARD_TURN_END),
		"a user session must never publish BOARD_TURN_END")
	require.Len(t, stub.consults, 1, "the seam is consulted once per transition")
}

// TestBoardTurnEnd_NoTransitionNoEmit: an IDLE arriving while already
// idle (the duplicate-settle shape) is not a turn end — no consult, no
// event.
func TestBoardTurnEnd_NoTransitionNoEmit(t *testing.T) {
	stub := &turnEndProbeStub{claimHolder: map[string]bool{"ses_w1": true}}
	a := newTurnEndAuthority(t, stub)
	frames := subscribeEvents(t, a)

	a.IngestForTest(&abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "ses_w1", Status: abiv1.SessionStatus_SESSION_STATUS_IDLE})

	evts := collectFrames(t, frames, 1)
	assert.Empty(t, eventsOfType(evts, abiv1.EventType_EVENT_TYPE_BOARD_TURN_END))
	assert.Empty(t, stub.consults, "no transition means no seam consult")
}

// TestBoardTurnEnd_SyntheticSettleNeverEmits: THE born-red
// emission-site pin (audit ruling 1, the anti-#2040 guard). A wedged
// claim-holder session settled by the reconcile sweep's store evidence
// (clearBusyFromEvidence) and by the lease-window rederive
// (rederiveStatusLocked) publishes the evidence settle exactly as
// today — and NEVER a BOARD_TURN_END: a synthesized settle is not a
// harness-attested turn end and carries no due-marking authority.
func TestBoardTurnEnd_SyntheticSettleNeverEmits(t *testing.T) {
	store := &evidenceStore{states: map[string]abiv1.SessionStatus{"ses_w1": abiv1.SessionStatus_SESSION_STATUS_IDLE}}
	a := newReconcileAuthority(t, store)
	a.mu.Lock()
	a.cfg.BoardTurnEndProbe = (&turnEndProbeStub{claimHolder: map[string]bool{"ses_w1": true}}).probe
	a.mu.Unlock()
	frames := subscribeEvents(t, a)

	// Wedge busy the declared-timeout way: harness attested the busy
	// mark, then went silent (the #1722 shape).
	a.IngestForTest(&abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "ses_w1", Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stats := a.Reconcile(ctx)
	assert.GreaterOrEqual(t, stats.BusyCleared, 1, "precondition: the sweep did settle the session")

	// The sweep's settle publishes ONE frame (the #1722 publication);
	// the turn-end publication must NOT follow it.
	evts := collectFrames(t, frames, 2) // busy + evidence settle
	assert.Empty(t, eventsOfType(evts, abiv1.EventType_EVENT_TYPE_BOARD_TURN_END),
		"a synthesized settle must never publish a due-mark-eligible event")

	// The lease-window rederive arm: a SECOND session wedged busy past
	// the bound whose store seed is idle — rederiveStatusLocked settles
	// it; same verdict.
	a.mu.Lock()
	a.sessions["ses_w2"] = newSessionRecord(abiv1.SessionStatus_SESSION_STATUS_BUSY)
	a.sessions["ses_w2"].busy = true
	a.sessions["ses_w2"].busySince = time.Now().Add(-(a.leaseBound() + time.Minute))
	a.cfg.BoardTurnEndProbe = (&turnEndProbeStub{claimHolder: map[string]bool{"ses_w2": true}}).probe
	a.mu.Unlock()
	a.rederiveStatuses(map[string]SessionSeed{"ses_w2": {Status: abiv1.SessionStatus_SESSION_STATUS_IDLE}})

	evts = collectFrames(t, frames, 1) // the rederive settle
	assert.Empty(t, eventsOfType(evts, abiv1.EventType_EVENT_TYPE_BOARD_TURN_END),
		"the lease-window rederive settle must never publish BOARD_TURN_END")
}

// TestBoardTurnEnd_NilProbeDegraded: an unwired seam (degraded pod,
// feature off) emits nothing and the fold is unaffected — the board
// falls back to sweep semantics (epic R5).
func TestBoardTurnEnd_NilProbeDegraded(t *testing.T) {
	a := newTurnEndAuthority(t, nil)
	frames := subscribeEvents(t, a)

	runOneTurn(t, a, "ses_w1")

	evts := collectFrames(t, frames, 2)
	assert.Empty(t, eventsOfType(evts, abiv1.EventType_EVENT_TYPE_BOARD_TURN_END))
	m := a.Metrics()
	assert.EqualValues(t, 0, m.BoardTurnEndsPublished)
	assert.EqualValues(t, 0, m.BoardTurnEndProbeFailures)
}

// TestBoardTurnEnd_PanickingProbeContained: R4 best-effort — a seam
// that panics is contained; the harness event's fold and publication
// survive; no turn-end event; the failure is counted, never thrown into
// the harness turn.
func TestBoardTurnEnd_PanickingProbeContained(t *testing.T) {
	stub := &turnEndProbeStub{claimHolder: map[string]bool{"ses_w1": true}, panics: true}
	a := newTurnEndAuthority(t, stub)
	frames := subscribeEvents(t, a)

	require.NotPanics(t, func() { runOneTurn(t, a, "ses_w1") })

	evts := collectFrames(t, frames, 2) // busy + idle survived
	require.Len(t, evts, 2, "the harness fold must survive a panicking seam")
	assert.Empty(t, eventsOfType(evts, abiv1.EventType_EVENT_TYPE_BOARD_TURN_END))
	assert.EqualValues(t, 1, a.Metrics().BoardTurnEndProbeFailures)
	assert.EqualValues(t, 0, a.Metrics().BoardTurnEndsPublished)
}

// TestBoardTurnEnd_EpochIncrements: two consecutive turns publish
// epochs 1 then 2 — the per-session ordinal the consumer uses for
// consecutive-no-advance counting and redelivery dedupe.
func TestBoardTurnEnd_EpochIncrements(t *testing.T) {
	stub := &turnEndProbeStub{claimHolder: map[string]bool{"ses_w1": true}}
	a := newTurnEndAuthority(t, stub)
	frames := subscribeEvents(t, a)

	runOneTurn(t, a, "ses_w1")
	runOneTurn(t, a, "ses_w1")

	evts := collectFrames(t, frames, 6) // 2x (busy + idle + turn-end)
	got := eventsOfType(evts, abiv1.EventType_EVENT_TYPE_BOARD_TURN_END)
	require.Len(t, got, 2)
	assert.EqualValues(t, 1, got[0].GetBoardTurnEnd().GetEpoch())
	assert.EqualValues(t, 2, got[1].GetBoardTurnEnd().GetEpoch())
}

// TestBoardTurnEnd_ErrorTurnEmits: an EVENT_TYPE_ERROR that clears a
// busy claim-holder is a harness-attested turn end (the error turn) —
// it publishes with the same payload shape.
func TestBoardTurnEnd_ErrorTurnEmits(t *testing.T) {
	stub := &turnEndProbeStub{claimHolder: map[string]bool{"ses_w1": true}}
	a := newTurnEndAuthority(t, stub)
	frames := subscribeEvents(t, a)

	a.IngestForTest(&abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "ses_w1", Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
	a.IngestForTest(&abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_ERROR, SessionId: "ses_w1",
		Error: &abiv1.Error{Message: "harness gave up"}})

	evts := collectFrames(t, frames, 3)
	got := eventsOfType(evts, abiv1.EventType_EVENT_TYPE_BOARD_TURN_END)
	require.Len(t, got, 1)
	assert.NotNil(t, got[0].GetBoardTurnEnd())
}

// TestBoardTurnEnd_WireTranslationKeepsRawID: the #1817 emission
// translation applies to the turn-end event like every other — the wire
// session_id becomes the phrase and the RAW harness id rides
// internal_session_id (the consumer binds on the raw id).
func TestBoardTurnEnd_WireTranslationKeepsRawID(t *testing.T) {
	stub := &turnEndProbeStub{claimHolder: map[string]bool{"ses_raw1": true}}
	cfg := Config{
		PlatformDir:  t.TempDir(),
		Parser:       &reconcileNopParser{},
		Passwords:    []string{"pw"},
		FastCursor:   true,
		IDTranslator: func(harnessID string) string { return "phrase-" + harnessID },
	}
	cfg.BoardTurnEndProbe = stub.probe
	a, err := New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })
	frames := subscribeEvents(t, a)

	runOneTurn(t, a, "ses_raw1")

	evts := collectFrames(t, frames, 3)
	got := eventsOfType(evts, abiv1.EventType_EVENT_TYPE_BOARD_TURN_END)
	require.Len(t, got, 1)
	assert.Equal(t, "phrase-ses_raw1", got[0].GetSessionId())
	assert.Equal(t, "ses_raw1", got[0].GetInternalSessionId())
}

// TestBoardTurnEnd_ReseedFlushDoesNotEmit: events buffered during a
// reseed window are replayed through the flush's plain applyLocked —
// harness-provision payloads, but NOT the live harness boundary. The
// emission site is the live path only: a settle landing inside a reseed
// window degrades (the sweep backstop owns it), and the flush must not
// synthesize a turn-end publication from replayed bytes.
func TestBoardTurnEnd_ReseedFlushDoesNotEmit(t *testing.T) {
	stub := &turnEndProbeStub{claimHolder: map[string]bool{"ses_w1": true}}
	a := newTurnEndAuthority(t, stub)
	frames := subscribeEvents(t, a)

	a.mu.Lock()
	a.buffering = true
	a.mu.Unlock()
	// The raw settle arrives while buffering: it parks in the buffer.
	a.IngestForTest(&abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "ses_w1", Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
	a.mu.Lock()
	flush := a.pending
	a.pending = nil
	a.buffering = false
	// Fold the buffered payload exactly as Reseed's flush does — the
	// plain applyLocked, never the harness-boundary wrapper.
	for _, raw := range flush {
		if evt, ok, err := a.parseContained(raw); err == nil && ok {
			a.applyLocked(evt)
		}
	}
	a.mu.Unlock()

	evts := collectFrames(t, frames, 1)
	assert.Empty(t, eventsOfType(evts, abiv1.EventType_EVENT_TYPE_BOARD_TURN_END),
		"the reseed flush path must never emit BOARD_TURN_END")
}

// TestBoardTurnEnd_Metrics: the publication and probe-failure counters
// are visible on the authority's Metrics surface (the ops bridge's
// source).
func TestBoardTurnEnd_Metrics(t *testing.T) {
	stub := &turnEndProbeStub{claimHolder: map[string]bool{"ses_w1": true, "ses_w2": true}}
	a := newTurnEndAuthority(t, stub)

	runOneTurn(t, a, "ses_w1")
	runOneTurn(t, a, "ses_w2")
	runOneTurn(t, a, "ses_w1")

	m := a.Metrics()
	assert.EqualValues(t, 3, m.BoardTurnEndsPublished)
	assert.EqualValues(t, 0, m.BoardTurnEndProbeFailures)
}
