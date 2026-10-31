// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

// stamp_events_integration_test.go — design 0072 S6 pins: every
// LANDED assignee stamp is an event on the bus sink (the stamp writes
// no board_events row — the sink is its only event surface), with the
// task/project identity the publisher needs, and a nil sink is inert
// (the S4 lease-sink posture: observability, never a dependency).

import (
	"context"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// stampRecorder collects sink events for assertions.
type stampRecorder struct {
	events []board.StampEvent
}

func (r *stampRecorder) sink(_ context.Context, ev board.StampEvent) {
	r.events = append(r.events, ev)
}

// TestPin_StampEventsSpawnAndBackfill — the two landing paths each
// project exactly one event, carrying the identity the API's
// publisher resolves scope by (TaskID + ShortID + ProjectID), the
// assignee, the origin, and the same lag the histogram observed.
func (s *BoardSuite) TestPin_StampEventsSpawnAndBackfill() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 1)
	e := s.seedEpic(p, board.EpicActive)
	s.seedTask(e, "stamp-events", nil, false)

	rec := &stampRecorder{}
	s.svc.SetStampSink(rec.sink)
	defer s.svc.SetStampSink(nil)

	c := s.claimOrFail(ws)

	// The spawn path: one event, origin=spawn, identity complete.
	s.Require().Len(rec.events, 1, "the landed spawn stamp projects exactly one event")
	ev := rec.events[0]
	s.Equal(c.TaskID, ev.TaskID)
	s.Equal(c.ShortID, ev.TaskShortID)
	s.Equal(p.ID, ev.ProjectID)
	s.Equal("sess-"+c.ShortID, ev.Assignee)
	s.Equal("spawn", ev.Origin)
	s.GreaterOrEqual(ev.LagSeconds, float64(0), "the lag rides the event (server-side, one clock domain)")

	// The lost-stamp shape back-fills: erase, then heartbeat with the
	// live token + identity — one backfill event with the evidence
	// session as assignee.
	_, err := s.pool.Exec(context.Background(),
		`UPDATE tasks SET assignee_session=NULL WHERE id=$1`, c.TaskID)
	s.Require().NoError(err)
	_, err = s.svc.Heartbeat(context.Background(),
		board.Actor{Kind: board.ActorUser, UserID: testUser, Surface: board.SurfaceAgent,
			ClaimToken: c.ClaimToken, SessionID: "sess-evidence"},
		"", c.ShortID, time.Now().UTC())
	s.Require().NoError(err)

	s.Require().Len(rec.events, 2, "the back-filled stamp projects its own event")
	ev = rec.events[1]
	s.Equal(c.TaskID, ev.TaskID)
	s.Equal("sess-evidence", ev.Assignee)
	s.Equal("backfill", ev.Origin)
	s.GreaterOrEqual(ev.LagSeconds, float64(0))

	// A landed stamp is never churned: a later heartbeat from another
	// identity neither rewrites the row NOR projects a third event.
	_, err = s.svc.Heartbeat(context.Background(),
		board.Actor{Kind: board.ActorUser, UserID: testUser, Surface: board.SurfaceAgent,
			ClaimToken: c.ClaimToken, SessionID: "sess-other"},
		"", c.ShortID, time.Now().UTC())
	s.Require().NoError(err)
	s.Len(rec.events, 2, "no new stamp, no new event")
}

// TestPin_StampSinkFailOpen — a panicking or failing sink can never
// change the stamp semantics: the row still lands, the claim still
// arbitrates (the S4 posture — the projection is never a dependency).
func (s *BoardSuite) TestPin_StampSinkFailOpen() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 1)
	e := s.seedEpic(p, board.EpicActive)
	s.seedTask(e, "stamp-failopen", nil, false)

	s.svc.SetStampSink(func(context.Context, board.StampEvent) {
		panic("sink failure must be contained by the projection contract")
	})
	defer s.svc.SetStampSink(nil)

	c := s.claimOrFail(ws)
	row := s.rawTask(c.TaskID)
	s.Require().NotNil(row.Assignee, "the stamp lands regardless of the sink")
	s.Equal("sess-"+c.ShortID, *row.Assignee)
}
