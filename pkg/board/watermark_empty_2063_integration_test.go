// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

// #2063 pins: the sweep's watermark capture against an EMPTY
// board_events table. max(created_at) over zero rows is NULL — scanned
// into a non-pointer time.Time it aborted the WHOLE sweep at capture
// (before any phase ran: RC1..RC4 dead, the watermark never advanced).
// The capture coalesces to the origin watermark (to_timestamp(0), the
// same seed readWatermark mints for a fresh scope): an empty event
// history IS "watermark at zero", and the two watermark paths agree
// by construction. A fresh install's first boot is exactly this state.
//
// Watermark assertions compare SQL-side (last_event_at = to_timestamp(0)):
// pgx decodes timestamptz into time.Time with varying *Location
// representations across pool connections, so Go-side DeepEqual is a
// location trap; the instant is the contract, and Postgres compares it.
package board_test

import (
	"context"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// TestPin_2063_EmptyEventsSweepCompletes (born-red): a sweep over an
// empty event history completes and leaves the watermark at the seeded
// epoch — and a second, still-empty sweep repeats cleanly (the sweep
// writes no events on an empty board, so idempotence re-exercises the
// empty path).
func (s *BoardSuite) TestPin_2063_EmptyEventsSweepCompletes() {
	// SetupTest truncated board_events; no seeds: zero events exist.
	s.Require().Equal(0, s.countEvents(""), "fixture: the event history is empty")

	s.sweep() // born-red: the NULL max() scan aborted here (issue #2063)

	s.True(s.watermarkIsAt(`to_timestamp(0)`), "empty history: the watermark stays at the origin seed")
	s.Require().Equal(0, s.countEvents(""), "no phase writes events on an empty board")

	s.sweep()
	s.True(s.watermarkIsAt(`to_timestamp(0)`), "idempotent: repeated empty sweeps stay at the origin watermark")
}

// TestPin_2063_PopulatedEventsWatermarkUnchanged (R4): with rows
// present the capture is the plain pre-sweep max(created_at) —
// identical to the pre-fix behavior (the COALESCE arm never fires when
// rows exist; r1-C3's pre-captured high-water, advanced verbatim).
func (s *BoardSuite) TestPin_2063_PopulatedEventsWatermarkUnchanged() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	s.seedTask(e, "anchor", nil, false)

	// The capture target, read PRE-sweep (r1-C3: the high-water is
	// captured before any phase runs). The fixture converges nothing
	// (unclaimed backlog task, no dependencies), so the sweep writes no
	// events — pinned, so the comparison below is the pre-capture max.
	before := s.countEvents("")
	s.sweep()
	s.Require().Equal(before, s.countEvents(""), "fixture: the sweep itself writes no events")

	var eq bool
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT last_event_at = (SELECT max(created_at) FROM board_events)
		 FROM board_sweeps WHERE scope='default'`).Scan(&eq))
	s.True(eq, "populated history: the watermark advances to the pre-captured max — behavior unchanged")
}

// watermarkIsAt compares the sweep cursor against a SQL expression
// server-side (instant equality, representation-free).
func (s *BoardSuite) watermarkIsAt(expr string) bool {
	var eq bool
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT last_event_at = (`+expr+`) FROM board_sweeps WHERE scope='default'`).Scan(&eq))
	return eq
}
