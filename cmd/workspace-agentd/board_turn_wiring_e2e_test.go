// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// board_turn_wiring_e2e_test.go — #2061/#2064 story 1: the opencode
// leg's REAL-CHAIN pin. newStateAuthority wires
// Config.BoardTurnEndProbe = boardTurnEmitterInst().facts — this test
// drives that exact production chain end to end: the spawner-minted
// registry (boardWorkerSessions), the MCP choke-point stamp
// (boardAdvanceLedgerInst().observeToolCall — the same call callMCPTool
// makes at its head), and the sessionstate authority's harness fold
// (IngestForTest IS the harness contract entry for tests). The event
// that crosses the wire must carry the seam's verdict (advanced inside
// the window, false outside it), the per-session epoch, and the raw
// harness id.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	opencode "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wiringBusyIdle folds one busy→idle pair through the harness boundary.
func wiringBusyIdle(a *sessionstate.Authority, sid string, gap time.Duration) {
	a.IngestForTest(&abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: sid, Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
	if gap > 0 {
		time.Sleep(gap)
	}
	a.IngestForTest(&abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: sid, Status: abiv1.SessionStatus_SESSION_STATUS_IDLE})
}

// wiringSubscribe opens the authority's stream, dropping the snapshot.
func wiringSubscribe(t *testing.T, a *sessionstate.Authority) <-chan *abiv1.StreamFrame {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	frames, stop, err := a.Stream(ctx)
	require.NoError(t, err)
	t.Cleanup(stop)
	select {
	case <-frames: // mandatory snapshot
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot frame never arrived")
	}
	return frames
}

// wiringCollect drains up to n events (2s deadline) — every ingested
// event frames exactly once, so counting pins presences AND absences.
func wiringCollect(t *testing.T, frames <-chan *abiv1.StreamFrame, n int) []*abiv1.Event {
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

func wiringTurnEnds(evts []*abiv1.Event) []*abiv1.Event {
	var out []*abiv1.Event
	for _, e := range evts {
		if e.GetType() == abiv1.EventType_EVENT_TYPE_BOARD_TURN_END {
			out = append(out, e)
		}
	}
	return out
}

// TestBoardTurnEndWiring_RealChainEmits: registry → choke-point stamp →
// probe → authority publication — the wired production chain on ONE
// authority, three turns: advance-in-window (advanced=true, epoch=1),
// no-fresh-stamp (advanced=false, epoch=2 — the window discipline
// through the real seam), and a user session (no event at all).
func TestBoardTurnEndWiring_RealChainEmits(t *testing.T) {
	sid := "ses_e2e_w1"
	boardWorkerSessions.record(sid) // the spawner-minted claim (memory-authoritative)

	cfg := sessionstate.Config{
		PlatformDir: t.TempDir(),
		Parser:      &opencode.ABITranslator{},
		Passwords:   []string{"pw"},
		FastCursor:  true,
		// THE wiring under test — the same function newStateAuthority
		// installs (sessionstate_wiring.go).
		BoardTurnEndProbe: boardTurnEmitterInst().facts,
	}
	a, err := sessionstate.New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })
	frames := wiringSubscribe(t, a)

	// Turn 1: busy, then the advance stamp THROUGH the real choke-point
	// entry, then the settle — advanced=true rides the event.
	a.IngestForTest(&abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: sid, Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
	boardAdvanceLedgerInst().observeToolCall("board_move_stage", map[string]any{"lsp_injected_session": sid})
	time.Sleep(2 * time.Millisecond)
	a.IngestForTest(&abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: sid, Status: abiv1.SessionStatus_SESSION_STATUS_IDLE})

	evts := wiringCollect(t, frames, 3) // busy + idle + turn-end
	got := wiringTurnEnds(evts)
	require.Len(t, got, 1, "the wired chain must publish BOARD_TURN_END, saw: %v", evts)
	assert.Equal(t, sid, got[0].GetSessionId(), "the raw harness id rides session_id (no translator wired)")
	pl := got[0].GetBoardTurnEnd()
	require.NotNil(t, pl)
	assert.True(t, pl.GetAdvanced(), "the in-window stamp must surface as advanced=true")
	assert.EqualValues(t, 1, pl.GetEpoch())
	assert.Greater(t, pl.GetTurnDurationMs(), int64(0))

	// The stamp also landed in the redirected shared tail (TestMain's
	// hermetic LLMSAFESPACES_BOARD_ADVANCES_PATH) — the e2e provably
	// used the REAL process-wide ledger, not a test double. The reader
	// resolves the same env path (same-scope posture).
	if tail := os.Getenv("LLMSAFESPACES_BOARD_ADVANCES_PATH"); tail != "" {
		_, ok := readBoardAdvanceStamps(sid)
		assert.True(t, ok, "the choke-point stamp must persist to the shared tail")
	}

	// Turn 2: no fresh stamp — turn 1's stamp sits before this window's
	// busy anchor → advanced=false, epoch=2 (the window discipline
	// through the real seam, not the stub).
	wiringBusyIdle(a, sid, 2*time.Millisecond)
	evts = wiringCollect(t, frames, 3)
	got = wiringTurnEnds(evts)
	require.Len(t, got, 1)
	assert.False(t, got[0].GetBoardTurnEnd().GetAdvanced(), "a stale stamp is a prior turn's advance")
	assert.EqualValues(t, 2, got[0].GetBoardTurnEnd().GetEpoch())

	// User arm: a session the registry never minted folds identically
	// and publishes NOTHING beyond its busy+idle.
	wiringBusyIdle(a, "ses_e2e_user", 0)
	evts = wiringCollect(t, frames, 2)
	assert.Empty(t, wiringTurnEnds(evts), "a user session must never publish BOARD_TURN_END")

	m := a.Metrics()
	assert.EqualValues(t, 2, m.BoardTurnEndsPublished)
	assert.EqualValues(t, 0, m.BoardTurnEndProbeFailures)
}
