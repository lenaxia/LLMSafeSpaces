// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

// heartbeat_1847_test.go — the #1847 PR1 pins, re-pinned to the item-6
// amendment (board_heartbeat = the nudge-RESPONSE channel; the periodic
// self-report semantics died 2026-10-28). The C1 dogfood incident
// class survives the repurposing: a worker emitted a heartbeat whose
// observedProgressAt was HALLUCINATED into the future (09:05Z when the
// server clock read 01:36Z); the server accepted it and stored it as
// the progress watermark, locking the worker out of its own lease by
// its own honesty. Worker stamps are RESPONSE events now, not the
// liveness input — but a response stamp is still stored on the row, so
// the server clock stays authoritative over the FUTURE direction:
//
//   - a stamp beyond a small skew tolerance ahead of the server clock
//     is REJECTED (invalid_input) — it is an estimate, not evidence;
//   - a stamp within the tolerance is CLAMPED to the server clock (a
//     FUTURE stamp is never stored);
//   - extension itself requires a live NUDGE on the row (the response);
//     the old evidence-gate/watermark-heal pins died WITH the gate —
//     extension no longer consults the watermark, so the C1 lockout
//     class is unreachable by construction rather than healed.

import (
	"context"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// setNudge arms the row's nudge slot directly (the response arm's
// precondition — the ladder-path nudge is pinned in lease_pins_test).
func (s *BoardSuite) setNudge(taskID, nudgeID string) {
	_, err := s.pool.Exec(context.Background(),
		`UPDATE tasks SET nudge_id=$2, nudged_at=now() WHERE id=$1`, taskID, nudgeID)
	s.Require().NoError(err)
}

// TestKill_HeartbeatFutureStampRejected: a hallucinated FUTURE timestamp
// must NOT extend the lease, must NOT touch the watermark, and must NOT
// clear the nudge slot (a rejected heartbeat is not a response).
func (s *BoardSuite) TestKill_HeartbeatFutureStampRejected() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "future-stamp", nil, false)

	c := s.claimOrFail(ws)
	before := s.rawTask(task.ID)
	wmBefore := s.rawTaskWatermark(task.ID)
	s.Require().NotNil(wmBefore, "the claim stamps an initial watermark")
	s.setNudge(task.ID, "nudge-future-stamp")

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
	wmAfter := s.rawTaskWatermark(task.ID)
	s.Require().NotNil(wmAfter)
	s.True(wmBefore.Equal(*wmAfter), "a rejected heartbeat must not touch the watermark")
	nid, _ := s.nudgeState(task.ID)
	s.Equal("nudge-future-stamp", nid, "a rejected heartbeat is not a response — the nudge slot survives")
}

// TestKill_HeartbeatMildSkewClampedToServerClock: a stamp within the
// skew tolerance on a NUDGED row still responds (honest client clock
// drift), but the STORED watermark is the server clock — never the
// client's future.
func (s *BoardSuite) TestKill_HeartbeatMildSkewClampedToServerClock() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "mild-skew", nil, false)

	c := s.claimOrFail(ws)
	serverNow := time.Now().UTC()
	s.setNudge(task.ID, "nudge-mild-skew")

	mild := serverNow.Add(90 * time.Second) // within the 2m tolerance
	res, err := s.svc.Heartbeat(context.Background(), s.agent("sess", c.ClaimToken), task.ID, "", mild)
	s.Require().NoError(err)
	s.True(res.Extended, "mild clock skew is clamped, not rejected — the response still renews")

	// The stored watermark must not carry the client's future: it reads
	// back at-or-behind the server clock at observe time.
	row := s.rawTaskWatermark(task.ID)
	s.Require().NotNil(row)
	s.False(row.After(serverNow.Add(5*time.Second)),
		"stored watermark must be server-clock (clamped), got %v (server %v)", row, serverNow)
	cleared, _ := s.nudgeState(task.ID)
	s.Empty(cleared, "the response clears the nudge")
}

// TestKill_HeartbeatEvidenceGateIsDead: the #1685 evidence gate is
// GONE with the periodic discipline. A heartbeat carrying genuinely
// fresh evidence (a watermark strictly newer than the stored one) on a
// row with NO live nudge extends NOTHING — extension's only key is the
// nudge. (The standing-discipline death is also spec pin 11; this pin
// guards the 1847 file's own incident surface: a future-adjacent stamp
// plus fresh evidence must not smuggle an extension.)
func (s *BoardSuite) TestKill_HeartbeatEvidenceGateIsDead() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "gate-dead", nil, false)

	c := s.claimOrFail(ws)
	before := s.rawTask(task.ID).LeaseExp
	s.Require().NotNil(before)

	// Fresh evidence by the OLD gate's own rule: rewind the stored
	// watermark, then observe "newer" — exactly the old extends shape.
	s.poisonWatermark(task.ID, time.Now().UTC().Add(-time.Hour))
	res, err := s.svc.Heartbeat(context.Background(), s.agent("sess", c.ClaimToken), task.ID, "",
		time.Now().UTC().Add(-time.Minute))
	s.Require().NoError(err)
	s.False(res.Extended, "fresh evidence without a nudge extends nothing — the gate is dead by design")

	after := s.rawTask(task.ID).LeaseExp
	s.Require().NotNil(after)
	s.WithinDuration(*before, *after, 50*time.Millisecond, "the lease is untouched")
}

// poisonWatermark rewinds/sets the raw progress watermark (the clock
// seam for the stored-stamp side of the comparisons).
func (s *BoardSuite) poisonWatermark(taskID string, at time.Time) {
	_, err := s.pool.Exec(context.Background(),
		`UPDATE tasks SET last_progress_at=$2 WHERE id=$1`, taskID, at)
	s.Require().NoError(err)
}

// rawTaskWatermark reads the raw progress watermark column.
func (s *BoardSuite) rawTaskWatermark(taskID string) *time.Time {
	var wm *time.Time
	err := s.pool.QueryRow(context.Background(),
		`SELECT last_progress_at FROM tasks WHERE id=$1`, taskID).Scan(&wm)
	s.Require().NoError(err)
	return wm
}
