// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

// observation_integration_test.go — the S7 audit-visibility pins (the
// 2026-10-30 incident remediation): every observation-plane action
// lands ONE row in the per-task trail, the recent window is ONE READ
// away on both health surfaces, the retention boundary is enforced,
// and the whole plane is FAIL-OPEN (a dead trail never breaks the
// board). The incident's unanswerable questions — were nudges sent?
// did lapse verdicts fire? who reclaimed? — become a SELECT.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// trail reads one task's retained window through the service read
// (the same projection future surfaces consume).
func (s *BoardSuite) trail(taskID string) []board.ObservationRow {
	rows, err := s.svc.TaskObservationTrail(context.Background(), taskID, 0)
	s.Require().NoError(err)
	return rows
}

// rawTrailCount counts rows directly (the prune pin's accounting).
func (s *BoardSuite) rawTrailCount(taskID string) int {
	var n int
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM task_observation_events WHERE task_id=$1`, taskID).Scan(&n))
	return n
}

// TestPin_ObservationTrail_StampLandsWithLag: the claim's assignee
// stamp — the observation plane's KEY — lands on the trail with its
// lag (S1's stamp_lag as a per-task fact, not just a histogram
// bucket). The incident's anonymous orphans are visible HERE first.
func (s *BoardSuite) TestPin_ObservationTrail_StampLandsWithLag() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "stamped-worker", nil, false)

	s.claimOrFail(ws)

	rows := s.trail(task.ID)
	s.Require().Len(rows, 1, "the spawn stamp lands exactly one trail row")
	r := rows[0]
	s.Equal(board.ObservationStamped, r.Kind)
	s.Equal("spawn", fmt.Sprint(r.Detail["origin"]))
	lag, ok := r.Detail["lag_seconds"].(float64)
	s.Require().True(ok, "lag_seconds is a number: %v", r.Detail)
	s.GreaterOrEqual(lag, float64(0))
	s.False(r.At.IsZero())
}

// TestPin_ObservationTrail_NudgeCarriesRung: a delivered sentinel
// lands kind=nudged with its id and WHICH sentinel (first_sentinel vs
// re_anchor — the ordinal question was unanswerable, the duplicate is
// benign). "Were nudges sent?" is the trail's first row.
func (s *BoardSuite) TestPin_ObservationTrail_NudgeCarriesRung() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "nudged-worker", nil, false)

	s.claimOrFail(ws)
	assignee := s.reloadTask(task.ID).AssigneeSession
	s.Require().NotEmpty(assignee)

	lives := &fakeLiveness{}
	lives.set(assignee, liveQuiet())
	recon := s.pinRecon(lives, func(context.Context, string, board.NudgePayload) error { return nil })

	// Round 1: quiet, no sentinel yet → the FIRST sentinel goes out.
	s.expireLease(task.ID)
	s.rc1Sweep(recon)
	nid, _ := s.nudgeState(task.ID)
	s.Require().NotEmpty(nid)

	rows := s.trail(task.ID)
	s.Require().Len(rows, 2, "stamp + nudge")
	n := rows[0] // newest-first: the nudge
	s.Equal(board.ObservationNudged, n.Kind)
	s.Equal(nid, fmt.Sprint(n.Detail["nudge_id"]))
	s.Equal(board.NudgeRungFirst, fmt.Sprint(n.Detail["rung"]))
	s.Equal(assignee, n.Session)

	// Round 2: still quiet, but THIS source never saw the mark — the
	// ordinal question is unanswerable, so the ladder re-anchors with
	// a FRESH sentinel (latest-only makes the duplicate benign). The
	// trail names the rung honestly.
	lives.set(assignee, board.AssigneeObservation{Observed: true, Exists: true, NudgeMarkKnown: false})
	s.expireLease(task.ID)
	s.rc1Sweep(recon)
	nid2, _ := s.nudgeState(task.ID)
	s.Require().NotEmpty(nid2)
	s.NotEqual(nid, nid2, "the re-anchor mints a fresh sentinel")

	rows = s.trail(task.ID)
	s.Require().Len(rows, 3, "stamp + nudge + re-anchor")
	n2 := rows[0]
	s.Equal(board.ObservationNudged, n2.Kind)
	s.Equal(nid2, fmt.Sprint(n2.Detail["nudge_id"]))
	s.Equal(board.NudgeRungReAnchor, fmt.Sprint(n2.Detail["rung"]),
		"the second sentinel names its rung — an unmeasured ordinal question re-anchors, never guesses")
}

// TestPin_ObservationTrail_LadderDecisionsLand: the ladder's three
// engine decisions — renewal (with its rung), lapse verdict (with its
// evidence), reclaim (with its reason) — each land their trail row
// through the same post-commit seam the bus rides.
func (s *BoardSuite) TestPin_ObservationTrail_LadderDecisionsLand() {
	s.Run("renewal", func() {
		ws := s.seedWorkspace()
		p := s.seedProject(ws, 4)
		e := s.seedEpic(p, board.EpicActive)
		task := s.seedTask(e, "renewing-worker", nil, false)

		s.claimOrFail(ws)
		assignee := s.reloadTask(task.ID).AssigneeSession
		lives := &fakeLiveness{}
		lives.set(assignee, liveMoving())
		recon := s.pinRecon(lives, nil)

		s.expireLease(task.ID)
		s.rc1Sweep(recon)

		rows := s.trail(task.ID)
		s.Require().Len(rows, 2, "stamp + renewal")
		s.Equal(board.ObservationRenewed, rows[0].Kind)
		s.Equal(board.RenewSourceMovement, fmt.Sprint(rows[0].Detail["source"]),
			"the renewal names which rung answered alive")
		s.Equal(board.StageInProgress, s.rawTask(task.ID).Stage)
	})

	s.Run("lapse-verdict", func() {
		ws := s.seedWorkspace()
		p := s.seedProject(ws, 4)
		e := s.seedEpic(p, board.EpicActive)
		task := s.seedTask(e, "wedged-worker", nil, false)

		s.claimOrFail(ws)
		assignee := s.reloadTask(task.ID).AssigneeSession
		lives := &fakeLiveness{}
		lives.set(assignee, liveQuiet())
		recon := s.pinRecon(lives, func(context.Context, string, board.NudgePayload) error { return nil })

		s.expireLease(task.ID)
		s.rc1Sweep(recon)
		nid, _ := s.nudgeState(task.ID)
		s.Require().NotEmpty(nid)

		lives.set(assignee, board.AssigneeObservation{Observed: true, Exists: true, NudgeMarkKnown: true})
		s.expireLease(task.ID)
		s.rc1Sweep(recon)

		rows := s.trail(task.ID)
		s.Require().Len(rows, 3, "stamp + nudge + verdict")
		v := rows[0]
		s.Equal(board.ObservationLapseVerdict, v.Kind)
		s.Equal("no session events after the sentinel", fmt.Sprint(v.Detail["evidence"]),
			"the machine verdict carries its evidence")
		s.Equal(nid, fmt.Sprint(v.Detail["nudge_id"]))
		fc, ok := v.Detail["failure_count"].(float64)
		s.Require().True(ok)
		s.EqualValues(1, fc)
	})

	s.Run("reclaim", func() {
		ws := s.seedWorkspace()
		p := s.seedProject(ws, 4)
		e := s.seedEpic(p, board.EpicActive)
		task := s.seedTask(e, "dead-session", nil, false)

		s.claimOrFail(ws)
		assignee := s.reloadTask(task.ID).AssigneeSession
		lives := &fakeLiveness{}
		lives.set(assignee, dead())
		recon := s.pinRecon(lives, nil)

		s.expireLease(task.ID)
		s.rc1Sweep(recon)

		rows := s.trail(task.ID)
		s.Require().Len(rows, 2, "stamp + reclaim")
		s.Equal(board.ObservationReclaimed, rows[0].Kind)
		s.Equal(board.ReclaimReasonDeadEvidence, fmt.Sprint(rows[0].Detail["reason"]))
	})
}

// TestPin_ObservationTrail_DeposeCarriesSalvagePointer: S5's
// deposition lands kind=deposed with the salvage pointer, the stage it
// came from, and whether the row was re-queued — the pointer WITH its
// context, one row.
func (s *BoardSuite) TestPin_ObservationTrail_DeposeCarriesSalvagePointer() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "deposed-worker", nil, false)

	s.claimOrFail(ws)
	assignee := s.reloadTask(task.ID).AssigneeSession
	s.Require().NotEmpty(assignee)
	s.expireLease(task.ID)

	_, err := s.svc.DeposeTask(context.Background(), s.owner(), task.ID, "", board.SalvagePointer{
		Branch: "s7-pin", Head: "deadbee", State: "mid-pin", Note: "trail pin",
	}, "")
	s.Require().NoError(err)

	rows := s.trail(task.ID)
	s.Require().Len(rows, 2, "stamp + deposed")
	d := rows[0]
	s.Equal(board.ObservationDeposed, d.Kind)
	s.Equal(assignee, d.Session)
	s.Equal(board.StageInProgress, fmt.Sprint(d.Detail["from_stage"]))
	s.Equal(true, d.Detail["requeued"])
	// The salvage pointer rides as its object verbatim.
	raw, err := json.Marshal(d.Detail["salvage"])
	s.Require().NoError(err)
	var ptr board.SalvagePointer
	s.Require().NoError(json.Unmarshal(raw, &ptr))
	s.Equal("s7-pin", ptr.Branch)
	s.Equal("deadbee", ptr.Head)
}

// TestPin_Health_ObservationWindow: the recent window is ONE READ
// away on BOTH health surfaces (the owner's and the agent surface's
// board_health) — newest first, short_id attached, kinds and details
// verbatim, sessions degrading raw+flagged when the index never saw
// them, and the retention boundary self-documented on every read.
func (s *BoardSuite) TestPin_Health_ObservationWindow() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "window-worker", nil, false)

	c := s.claimOrFail(ws)
	assignee := s.reloadTask(task.ID).AssigneeSession
	lives := &fakeLiveness{}
	lives.set(assignee, liveQuiet())
	recon := s.pinRecon(lives, func(context.Context, string, board.NudgePayload) error { return nil })
	s.expireLease(task.ID)
	s.rc1Sweep(recon)

	h, err := s.svc.BoardHealth(ctx, testUser)
	s.Require().NoError(err)
	s.Require().NotEmpty(h.ObservationEvents)
	s.Equal(board.ObservationKeepPerTask, h.ObservationRetention.PerTaskEvents)
	s.Equal(board.ObservationTTL.String(), h.ObservationRetention.TTL)

	// Newest first: nudge over stamp; the short id rides; the raw
	// harness session degrades flagged (the index never saw it).
	s.Equal(board.ObservationNudged, h.ObservationEvents[0].Kind)
	s.Equal(task.ShortID, h.ObservationEvents[0].ShortID)
	s.Equal(task.ID, h.ObservationEvents[0].TaskID)
	s.True(h.ObservationEvents[0].SessionDeprecated, "an unindexed session renders raw + flagged")

	hw, err := s.svc.BoardHealthInWorkspace(ctx, ws)
	s.Require().NoError(err)
	s.Require().NotEmpty(hw.ObservationEvents)
	s.Equal(h.ObservationEvents[0].Kind, hw.ObservationEvents[0].Kind)
	s.Equal(h.ObservationEvents[0].TaskID, hw.ObservationEvents[0].TaskID)

	// The window is scoped: a foreign workspace's boards stay invisible.
	ws2 := s.seedWorkspace()
	hw2, err := s.svc.BoardHealthInWorkspace(ctx, ws2)
	s.Require().NoError(err)
	s.Empty(hw2.ObservationEvents)
	_ = c
}

// TestPin_ObservationTrail_RetentionPrune: the boundary is ENFORCED —
// per task the newest ObservationKeepPerTask rows stay, the TTL arm
// ages out even under the cap, and the permanent record (board_events)
// is untouched.
func (s *BoardSuite) TestPin_ObservationTrail_RetentionPrune() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "chatty-worker", nil, false)

	// Seed past the cap: ObservationKeepPerTask + 7 rows, the OLDEST
	// backdated past the TTL (it must age out regardless of count).
	seed := func(n int, age time.Duration) {
		for i := 0; i < n; i++ {
			_, err := s.pool.Exec(ctx, `
				INSERT INTO task_observation_events (task_id, kind, session, detail, created_at)
				VALUES ($1, 'renewed', 'sess', '{"source":"movement"}'::jsonb, now() - make_interval(secs=>$2))`,
				task.ID, age.Seconds()+float64(i))
			s.Require().NoError(err)
		}
	}
	seed(1, board.ObservationTTL+time.Hour) // aged past the TTL
	seed(board.ObservationKeepPerTask+6, time.Minute)

	s.svc.PruneObservationTrail(ctx)

	n := s.rawTrailCount(task.ID)
	s.Equal(board.ObservationKeepPerTask, n,
		"the cap keeps exactly the newest %d rows (the aged row plus the overflow prune)", board.ObservationKeepPerTask)

	// The permanent record stays: board_events still carries its rows.
	var evCount int
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT count(*) FROM board_events WHERE item_id=$1`, task.ID).Scan(&evCount))
	s.Greater(evCount, 0, "the idempotency ledger is never pruned by the trail's boundary")

	// The newest rows are the survivors (the read's newest-first head
	// is a fresh row, and the read never exceeds the cap).
	rows := s.trail(task.ID)
	s.Require().Len(rows, board.ObservationKeepPerTask)
}

// TestPin_ObservationTrail_FailOpenDeadTrail: a DEAD trail changes no
// board state — with the table dropped, the ladder still renews (and
// the failure counter moves); the trail is observability, never a
// dependency. The incident's remediation must not introduce a new
// single point of failure.
func (s *BoardSuite) TestPin_ObservationTrail_FailOpenDeadTrail() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "failopen-worker", nil, false)

	s.claimOrFail(ws)
	assignee := s.reloadTask(task.ID).AssigneeSession

	// Drop the trail (the harshest failure: the writes have nowhere to
	// land). Restored before the test ends — SetupTest's TRUNCATE
	// needs it.
	_, err := s.pool.Exec(context.Background(), `DROP TABLE task_observation_events`)
	s.Require().NoError(err)
	defer func() {
		_, rerr := s.pool.Exec(context.Background(), `
			CREATE TABLE IF NOT EXISTS task_observation_events (
				id BIGSERIAL PRIMARY KEY,
				task_id UUID NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
				kind TEXT NOT NULL,
				session TEXT NOT NULL DEFAULT '',
				detail JSONB NOT NULL DEFAULT '{}'::jsonb,
				created_at TIMESTAMPTZ NOT NULL DEFAULT now()
			);
			CREATE INDEX IF NOT EXISTS idx_task_observation_events_task
				ON task_observation_events (task_id, id DESC);
			CREATE INDEX IF NOT EXISTS idx_task_observation_events_created
				ON task_observation_events (created_at);`)
		s.Require().NoError(rerr)
	}()

	before := testutil.ToFloat64(board.ObservationFailuresMetric().WithLabelValues(board.ObservationRenewed))
	lives := &fakeLiveness{}
	lives.set(assignee, liveMoving())
	recon := s.pinRecon(lives, nil)
	s.expireLease(task.ID)
	s.rc1Sweep(recon) // must NOT error: the sweep's own writes are unaffected

	s.Equal(board.StageInProgress, s.rawTask(task.ID).Stage, "the renewal landed — the board never depended on the trail")
	tr := s.rawTask(task.ID)
	s.Require().NotNil(tr.LeaseExp, "the lease extended despite the dead trail")
	after := testutil.ToFloat64(board.ObservationFailuresMetric().WithLabelValues(board.ObservationRenewed))
	s.Greater(after, before, "the dropped row is counted, never swallowed")
}
