// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

// heartbeat_1847_test.go — the #1847 PR1 hotfix pins (red-first): the
// C1 dogfood incident class. A worker emitted a heartbeat whose
// observedProgressAt was HALLUCINATED into the future (09:05Z when the
// server clock read 01:36Z); the server accepted it, stored it as the
// progress watermark, and every subsequent honest heartbeat failed the
// evidence gate (observed.After(stored) can never hold against a future
// watermark) — the worker was locked out of its own lease by its own
// honesty. Zero worker deaths among the night's lapsed claims; this was
// pure input-trust failure.
//
// The guard: server-clock authority over client-observed stamps.
//   - a stamp beyond a small skew tolerance ahead of the server clock is
//     REJECTED (invalid_input) — it is not evidence, it is an estimate;
//   - a stamp within the tolerance is clamped to the server clock (all
//     stored watermark stamps are server-clock — the #1847 direction);
//   - a watermark already poisoned by the pre-fix bug self-heals: a
//     stored FUTURE watermark reads as stale, so an honest present-t
//     observation extends again.

import (
	"context"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// poisonWatermark simulates the pre-fix C1 incident's persisted damage:
// a future stamp stored as the progress watermark.
func (s *BoardSuite) poisonWatermark(taskID string, at time.Time) {
	_, err := s.pool.Exec(context.Background(),
		`UPDATE tasks SET last_progress_at=$2 WHERE id=$1`, taskID, at)
	s.Require().NoError(err)
}

// TestKill_HeartbeatFutureStampRejected: a hallucinated FUTURE timestamp
// must NOT extend the lease and must NOT touch the watermark.
func (s *BoardSuite) TestKill_HeartbeatFutureStampRejected() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "future-stamp", nil, false)

	c := s.claimOrFail(ws)
	before := s.rawTask(task.ID)

	hallucinated := time.Now().UTC().Add(8 * time.Hour)
	res, err := s.svc.Heartbeat(context.Background(), s.agent("sess", c.ClaimToken), task.ID, "", hallucinated)
	s.Require().Error(err, "a future-stamped heartbeat must be rejected, not accepted")
	var ge *board.GuardError
	s.Require().ErrorAs(err, &ge)
	s.Equal(board.GuardInput, ge.Code)
	s.Equal(board.HeartbeatResult{}, res, "a rejected heartbeat returns no result")

	after := s.rawTask(task.ID)
	s.WithinDuration(*before.LeaseExp, *after.LeaseExp, 50*time.Millisecond,
		"a future-stamped heartbeat must not extend the lease")
	s.Equal(0, s.countEvents("heartbeat"), "a rejected heartbeat writes no audit event")
}

// TestKill_HeartbeatMildSkewClampedToServerClock: a stamp within the
// skew tolerance still extends (honest client clock drift), but the
// STORED watermark is the server clock — never the client's future.
func (s *BoardSuite) TestKill_HeartbeatMildSkewClampedToServerClock() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "mild-skew", nil, false)

	c := s.claimOrFail(ws)
	serverNow := time.Now().UTC()

	mild := serverNow.Add(90 * time.Second) // within the 2m tolerance
	res, err := s.svc.Heartbeat(context.Background(), s.agent("sess", c.ClaimToken), task.ID, "", mild)
	s.Require().NoError(err)
	s.True(res.Extended, "mild clock skew is clamped, not rejected — the heartbeat still extends")

	// The stored watermark must not carry the client's future: it reads
	// back at-or-behind the server clock at observe time.
	row := s.rawTaskWatermark(task.ID)
	s.Require().NotNil(row)
	s.False(row.After(serverNow.Add(5*time.Second)),
		"stored watermark must be server-clock (clamped), got %v (server %v)", row, serverNow)
}

// TestKill_HeartbeatPoisonedWatermarkSelfHeals: the C1 lockout — a
// watermark poisoned FUTURE (the pre-fix bug's persisted damage) must
// not hold the honest worker out: a present-t observation extends again.
func (s *BoardSuite) TestKill_HeartbeatPoisonedWatermarkSelfHeals() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "poisoned", nil, false)

	c := s.claimOrFail(ws)
	// Persist the incident: watermark 8h in the future, lease about to
	// lapse — the state C1's worker was actually in.
	s.poisonWatermark(task.ID, time.Now().UTC().Add(8*time.Hour))
	s.expireLease(task.ID)

	res, err := s.svc.Heartbeat(context.Background(), s.agent("sess", c.ClaimToken), task.ID, "", time.Now().UTC())
	s.Require().NoError(err, "an honest heartbeat must extend against a poisoned (future) watermark")
	s.True(res.Extended, "the self-heal: a stored future watermark reads as stale")

	row := s.rawTaskWatermark(task.ID)
	s.Require().NotNil(row)
	s.False(row.After(time.Now().UTC().Add(5*time.Second)),
		"the heal REPLACES the future watermark with the server-clock stamp")
}

// rawTaskWatermark reads the raw progress watermark column.
func (s *BoardSuite) rawTaskWatermark(taskID string) *time.Time {
	var wm *time.Time
	err := s.pool.QueryRow(context.Background(),
		`SELECT last_progress_at FROM tasks WHERE id=$1`, taskID).Scan(&wm)
	s.Require().NoError(err)
	return wm
}
