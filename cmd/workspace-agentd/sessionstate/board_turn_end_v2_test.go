// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate

// board_turn_end_v2_test.go — issue #2100's transport-v2 emission pins
// (lease-evidence-core design §2.1): the claim-holder GATE in front of
// the turn-end publication is deleted under the v2 flag — the event is
// emitted for EVERY harness-observed busy→idle transition (payload:
// session, epoch, turn window, duration — no content), and the API
// consumer's assignee-match guard is the claim filter (a user chat's
// turn-end matches no claim and is inert). The registry consult that
// the #2100-A incident proved EROFS-dead stops gating anything.
//
//	P2-ungated   under Config.BoardTurnEndUngated, a session the probe
//	             would call a NON-claim-holder still publishes — and the
//	             probe is not consulted at all (the gate, not the
//	             probe's other facts, is what moved store-side).
//	P2-gated     the DEFAULT (flag off) preserves R1 verbatim: the
//	             probe's ClaimHolder=false answer suppresses the
//	             publication (the #2065 user-session pin stays green).
//	P2-window    the ungated payload carries turn_started_at — the
//	             consumer's store-side advance derivation needs an
//	             absolute window start.

import (
	"testing"
	"time"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newUngatedAuthority builds an authority with the transport-v2
// emission flag set (probe still wired — the pin asserts it goes
// unconsulted).
func newUngatedAuthority(t *testing.T, stub *turnEndProbeStub) *Authority {
	t.Helper()
	cfg := Config{
		PlatformDir:         t.TempDir(),
		Parser:              &reconcileNopParser{},
		Passwords:           []string{"pw"},
		FastCursor:          true,
		BoardTurnEndUngated: true,
	}
	if stub != nil {
		cfg.BoardTurnEndProbe = stub.probe
	}
	a, err := New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// TestPin_P2_EmissionUngated_UserSessionEmitsAndProbeUnconsulted —
// born-red on main (the gate suppresses the publication): under the v2
// flag a user-chat session's turn-end STILL publishes, the probe is
// never asked (the registry consult is deleted), and the payload marks
// advanced=false (the derivation moved store-side).
func TestPin_P2_EmissionUngated_UserSessionEmitsAndProbeUnconsulted(t *testing.T) {
	stub := &turnEndProbeStub{claimHolder: map[string]bool{}} // nobody is a claim holder
	a := newUngatedAuthority(t, stub)
	frames := subscribeEvents(t, a)

	runOneTurn(t, a, "ses_user_chat")

	evts := collectFrames(t, frames, 3)
	got := eventsOfType(evts, abiv1.EventType_EVENT_TYPE_BOARD_TURN_END)
	require.Len(t, got, 1, "the ungated emission publishes for every harness turn-end")
	pl := got[0].GetBoardTurnEnd()
	require.NotNil(t, pl)
	assert.False(t, pl.GetAdvanced(), "advanced is derived store-side now — emission carries false always")
	assert.Empty(t, stub.consults, "the claim-holder consult is DELETED under v2 (the EROFS-dead gate)")
}

// TestPin_P2_GatedDefault_PreservesR1 — the flag-off world is
// byte-identical to today: the probe's ClaimHolder=false answer
// suppresses the publication (R1 — user sessions never emit).
func TestPin_P2_GatedDefault_PreservesR1(t *testing.T) {
	stub := &turnEndProbeStub{claimHolder: map[string]bool{}}
	a := newTurnEndAuthority(t, stub)
	frames := subscribeEvents(t, a)

	runOneTurn(t, a, "ses_user_chat")

	evts := collectFrames(t, frames, 3)
	assert.Empty(t, eventsOfType(evts, abiv1.EventType_EVENT_TYPE_BOARD_TURN_END),
		"flag off: the claim-holder gate stands verbatim")
	require.Len(t, stub.consults, 1, "the probe answered the gate")
}

// TestPin_P2_PayloadCarriesTurnStartedAt — the consumer's store-side
// window anchor: the ungated payload's turn_started_at equals the
// authority's busy-mark (the absolute turn start), and the nil-probe
// shape emits too (the probe no longer gates the emission's existence).
func TestPin_P2_PayloadCarriesTurnStartedAt(t *testing.T) {
	a := newUngatedAuthority(t, nil) // probe unwired: still emits under v2
	frames := subscribeEvents(t, a)

	mark := time.Now().UTC()
	a.IngestForTest(&abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "ses_w9", Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
	a.IngestForTest(&abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "ses_w9", Status: abiv1.SessionStatus_SESSION_STATUS_IDLE})

	evts := collectFrames(t, frames, 3)
	got := eventsOfType(evts, abiv1.EventType_EVENT_TYPE_BOARD_TURN_END)
	require.Len(t, got, 1)
	pl := got[0].GetBoardTurnEnd()
	require.NotNil(t, pl)
	assert.NotZero(t, pl.GetTurnStartedAtUnixMs(), "the payload carries the absolute turn start")
	assert.WithinDuration(t, mark, time.UnixMilli(pl.GetTurnStartedAtUnixMs()), 2*time.Second,
		"turn_started_at is the busy-mark anchor")
}
