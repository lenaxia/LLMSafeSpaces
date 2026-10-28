// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// #1832 WS4, re-pinned to the item-6 amendment: the stillborn CLASS
// (a spawn that mints a session but produces ZERO turns) is no longer
// policed at the heartbeat extension — board_heartbeat is the
// nudge-RESPONSE channel, and a zero-turn session cannot call tools,
// so it can never answer a sentinel: the WEDGE VERDICT owns corpses
// (a nudge, a full window of silence on both channels, failed). The
// turn-evidence seam survives with a different consumer — the #1959c
// bounce re-dispatch refuses to re-dispatch into a corpse
// (redispatchBounce, pinned in bounce_1959_*_test.go).
//
// What THIS file pins: the response channel is TURN-BLIND — the seam's
// answers (zero turns, live turns, erroring) never gate a nudge
// response. The discriminator between a corpse and a live reviewer is
// the ladder, not the extension.

// fakeTurnSource is the turn-evidence seam fixture.
type fakeTurnSource struct {
	counts map[string]int
	err    error
}

func (f *fakeTurnSource) SessionTurnEvidence(_ context.Context, sessionID string) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.counts[sessionID], nil
}

// stillbornSvc builds a service with the turn-evidence seam installed
// (the shared suite service has none — its nil-seam behavior is pinned
// separately).
func (s *BoardSuite) stillbornSvc(ts *fakeTurnSource) *board.Service {
	return board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(s.spawner.Spawn),
		board.WithTurnEvidenceSource(ts),
	)
}

func TestStillbornEvidence_SeamSemantics(t *testing.T) {
	// The seam is a first-class option (the bounce re-dispatch's
	// consumer); nil (the default) keeps today's behavior.
	require.NotNil(t, board.WithTurnEvidenceSource(&fakeTurnSource{}), "the option installs")
}

func (s *BoardSuite) TestStillborn_ResponseChannelIsTurnBlind() {
	for _, tc := range []struct {
		name  string
		ts    *fakeTurnSource
		brief string
	}{
		{"zero-turn assignee", &fakeTurnSource{counts: map[string]int{}},
			"a corpse cannot call tools — but the response, when it arrives, is honored"},
		{"live assignee", &fakeTurnSource{counts: map[string]int{"sess": 5}},
			"a working session's response extends"},
		{"seam erroring", &fakeTurnSource{err: errTurnSourceBlip},
			"a transient seam blip never gates the response channel"},
	} {
		s.Run(tc.name, func() {
			ws := s.seedWorkspace()
			p := s.seedProject(ws, 2)
			e := s.seedEpic(p, board.EpicActive)
			task := s.seedTask(e, "turn-blind", nil, false)

			svc := s.stillbornSvc(tc.ts)
			c, err := svc.ClaimNext(context.Background(), "worker-"+ws, ws)
			s.Require().NoError(err)
			s.Require().NotNil(c.Task)

			// No nudge: advisory only, whatever the turn evidence says.
			res, err := svc.Heartbeat(context.Background(), s.agent(c.Assignee, c.ClaimToken), "", c.ShortID,
				time.Now().UTC())
			s.Require().NoError(err)
			require.False(s.T(), res.Extended, "no nudge → no extension (turn evidence is irrelevant)")

			// The nudge response: honored on every seam answer.
			s.setNudge(task.ID, "nudge-turn-blind")
			res, err = svc.Heartbeat(context.Background(), s.agent(c.Assignee, c.ClaimToken), "", c.ShortID,
				time.Now().UTC())
			s.Require().NoError(err)
			require.True(s.T(), res.Extended, tc.brief)
			require.Empty(s.T(), res.Reason)
		})
	}
}

func (s *BoardSuite) TestStillborn_WedgeVerdictOwnsCorpses() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "corpse", nil, false)

	// The stillborn shape: a session at ZERO turns holds the claim.
	svc := s.stillbornSvc(&fakeTurnSource{counts: map[string]int{}})
	c, err := svc.ClaimNext(context.Background(), "worker-"+ws, ws)
	s.Require().NoError(err)
	s.Require().NotNil(c.Task)
	assignee := s.reloadTask(task.ID).AssigneeSession
	s.Require().NotEmpty(assignee)

	// The ladder asks; the corpse cannot answer (no turn can start, no
	// tool can be called); the silence on both channels wedges.
	lives := &fakeLiveness{}
	lives.set(assignee, liveQuiet())
	nudger := &fakeNudger{}
	recon := s.pinRecon(lives, nudger.Send)

	s.expireLease(task.ID)
	s.rc1Sweep(recon)
	s.Equal(1, nudger.count(), "the sentinel was delivered")

	s.expireLease(task.ID)
	s.rc1Sweep(recon)

	row := s.rawTask(task.ID)
	s.Equal(board.StageFailed, row.Stage, "a stillborn's silence is the wedge verdict — the class's new owner")
	s.Nil(row.ClaimToken, "the corpse's claim is cleared")
	s.Equal(1, s.countEvents("wedge"))
}

// errTurnSourceBlip is the transient-failure fixture.
var errTurnSourceBlip = &turnSourceError{}

type turnSourceError struct{}

func (*turnSourceError) Error() string { return "session index blip" }
