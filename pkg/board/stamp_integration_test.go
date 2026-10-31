// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

// stamp_integration_test.go — RC-B S1 pins: the assignee stamp's retry,
// back-fill, and observability (the 2026-10-30 incident's lost-stamp
// blindness, pinned so it cannot return silently).

import (
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// stampMetricCount reads a histogram observation's count (the pinned
// "emitted and visible" assertions).
func stampMetricCount(o prometheus.Observer) uint64 {
	h, ok := o.(prometheus.Histogram)
	if !ok {
		return 0
	}
	var m dto.Metric
	if err := h.Write(&m); err != nil {
		return 0
	}
	return m.GetHistogram().GetSampleCount()
}

// TestPin_StampRetryLandsUnderTransientFailure — build item 1: a
// transient failure of the stamp write (here: the fault seam failing
// the first two attempts, standing in for a flapping DB) must not lose
// the stamp — the retry loop lands it, every failed attempt counts in
// the failure metric, and the landed stamp emits stamp_lag.
func (s *BoardSuite) TestPin_StampRetryLandsUnderTransientFailure() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 1)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "stamp-retry", nil, false)

	// The transient: the first two stamp-write attempts fail, the
	// third lands. The sync spawner (the harness default) runs the
	// stamp inline in the claim.
	var n int32
	s.fps.armAtFn("stamp.write", func() error {
		n++
		if n <= 2 {
			return errors.New("injected transient: db is flapping")
		}
		return nil
	})
	defer s.fps.disarm("stamp.write")

	failuresBefore := promtestutil.ToFloat64(board.StampWriteFailures().WithLabelValues("spawn"))
	spawnLag := board.StampLag().WithLabelValues("spawn")
	spawnObserved := stampMetricCount(spawnLag)

	c := s.claimOrFail(ws)
	_ = task

	// The retry landed the stamp: the observation plane can see the claim.
	row := s.rawTask(c.TaskID)
	s.Require().NotNil(row.Assignee, "the retried stamp write must land the assignee identity")
	s.Equal("sess-"+c.ShortID, *row.Assignee, "the stamped identity is the spawned session")
	// The guard held: the claim token still arbitrates the row.
	s.Require().NotNil(row.ClaimToken)
	s.Equal(c.ClaimToken, *row.ClaimToken)

	// Every failed attempt is visible in the failure counter (+2), and
	// the landed stamp emitted stamp_lag (+1 observation).
	s.Equal(float64(2), promtestutil.ToFloat64(board.StampWriteFailures().WithLabelValues("spawn"))-failuresBefore,
		"two injected failures must count in the stamp-write failure metric")
	s.Equal(spawnObserved+1, stampMetricCount(spawnLag), "the landed stamp must emit stamp_lag (origin=spawn)")
}

// TestPin_StampBackfillOnClaimEvidence — build item 2, the pinned
// back-fill: a claim whose stamp was lost (write-once semantics dropped
// it; retries never got a chance) is RE-STAMPED when subsequent
// evidence of the claim arrives — a worker-turn heartbeat carrying the
// live claim token and the calling session's identity. The wrong token
// never back-fills; an existing stamp is never overwritten.
func (s *BoardSuite) TestPin_StampBackfillOnClaimEvidence() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 1)
	e := s.seedEpic(p, board.EpicActive)
	s.seedTask(e, "stamp-backfill", nil, false)

	c := s.claimOrFail(ws)
	// The stamp landed at spawn (healthy path); simulate the LOST write
	// by erasing it — the row is now observationally blind exactly like
	// the incident's orphans.
	_, err := s.pool.Exec(context.Background(),
		`UPDATE tasks SET assignee_session=NULL WHERE id=$1`, c.TaskID)
	s.Require().NoError(err)
	s.Nil(s.rawTask(c.TaskID).Assignee, "fixture: the stamp is gone (the lost-write shape)")

	backLag := board.StampLag().WithLabelValues("backfill")
	backfillObserved := stampMetricCount(backLag)

	// A heartbeat on the WRONG token is not evidence of THIS claim: no
	// back-fill (the claim-token guard refuses the call outright).
	_, err = s.svc.Heartbeat(context.Background(),
		board.Actor{Kind: board.ActorUser, UserID: testUser, Surface: board.SurfaceAgent,
			ClaimToken: "00000000-0000-0000-0000-000000000000", SessionID: "sess-imposter"},
		"", c.ShortID, time.Now().UTC())
	s.Require().Error(err, "a wrong-token heartbeat refuses (the claim-token guard)")
	s.Nil(s.rawTask(c.TaskID).Assignee, "no token, no evidence: the stamp is still missing")

	// The next worker-turn observation — the live token + the calling
	// session's identity — re-stamps the claim.
	res, err := s.svc.Heartbeat(context.Background(),
		board.Actor{Kind: board.ActorUser, UserID: testUser, Surface: board.SurfaceAgent,
			ClaimToken: c.ClaimToken, SessionID: "sess-recovered"},
		"", c.ShortID, time.Now().UTC())
	s.Require().NoError(err, "the heartbeat itself succeeds (the back-fill is best-effort, never a hostage)")
	_ = res

	row := s.rawTask(c.TaskID)
	s.Require().NotNil(row.Assignee, "the back-fill re-stamped the claim")
	s.Equal("sess-recovered", *row.Assignee, "the stamped identity is the evidence-carrying session")
	s.Equal(backfillObserved+1, stampMetricCount(backLag), "the back-filled stamp emits stamp_lag (origin=backfill)")

	// An existing stamp is NEVER overwritten: a later heartbeat from a
	// different identity keeps the recorded assignee.
	_, err = s.svc.Heartbeat(context.Background(),
		board.Actor{Kind: board.ActorUser, UserID: testUser, Surface: board.SurfaceAgent,
			ClaimToken: c.ClaimToken, SessionID: "sess-other"},
		"", c.ShortID, time.Now().UTC())
	s.Require().NoError(err)
	row = s.rawTask(c.TaskID)
	s.Require().NotNil(row.Assignee)
	s.Equal("sess-recovered", *row.Assignee, "the back-fill fills the gap; it never churns a landed identity")
}

// TestPin_StampExhaustionRunsUnstampedUntilBackfill — the eventually-
// true end-to-end pin: a stamp write that exhausts every retry on a
// STILL-LIVE claim leaves the worker RUNNING (aborting a live worker
// over a stamp it never asked to lose would replay the incident's
// kill), and the very next claim evidence heals the row.
func (s *BoardSuite) TestPin_StampExhaustionRunsUnstampedUntilBackfill() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 1)
	e := s.seedEpic(p, board.EpicActive)
	s.seedTask(e, "stamp-exhaustion", nil, false)

	// Every stamp-write attempt fails this episode.
	s.fps.armAt("stamp.write", errors.New("injected outage: db down"))
	defer s.fps.disarm("stamp.write")

	c := s.claimOrFail(ws)

	row := s.rawTask(c.TaskID)
	s.Nil(row.Assignee, "the stamp never landed (all retries failed)")
	s.Equal(0, s.aborter.count(), "a live claim's worker is never aborted over a lost stamp — back-fill owns recovery")
	s.Require().NotNil(row.ClaimToken, "the claim itself is untouched by the stamp loss")
	s.Equal(c.ClaimToken, *row.ClaimToken)

	// The outage heals; the worker's next turn observation back-fills.
	_, err := s.svc.Heartbeat(context.Background(),
		board.Actor{Kind: board.ActorUser, UserID: testUser, Surface: board.SurfaceAgent,
			ClaimToken: c.ClaimToken, SessionID: "sess-healed"},
		"", c.ShortID, time.Now().UTC())
	s.Require().NoError(err)
	row = s.rawTask(c.TaskID)
	s.Require().NotNil(row.Assignee, "the claim is observable again (eventually-true, never write-once)")
	s.Equal("sess-healed", *row.Assignee)
}
