// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

// due_mark_2065_integration_test.go — #2061/#2065 story 2 pins: the
// event-driven due-mark. The measured incident shape (worker's turn
// ends stop at 19:51, first nudge 21:38 — 1h47m) closes to under one
// sweep: a turn-end(no-advance) event marks the claim's lease due, and
// the NEXT sweep runs the EXISTING quiet arms. Every pin asserts state
// DELTAS, never wall-clock.
//
//	2065-A  the incident shape, delta-pinned: a live-lease row survives
//	        a sweep untouched (the pre-event delta leg — today's
//	        behavior); ONE event makes it due; ONE sweep then arms the
//	        nudge. No lease expiry is waited out anywhere.
//	2065-B  advance=yes is telemetry-only: lease untouched, due-mark
//	        counter untouched, turn_advances +1.
//	2065-C  guards: pending_input and paused rows are never due-marked.
//	2065-D  idempotence + episode ownership: an armed nudge slot or an
//	        already-due lease means the sweep owns the episode — the
//	        event is a no-op, the counter does not move (the
//	        cross-replica collision collapses the same way: the loser's
//	        predicate sees the winner's write).
//	2065-E  foreign identity: an event matching no live claim (user
//	        chat, other workspace) is a silent no-op.
//	2065-F  the row-side epoch guard: an event observed BEFORE the
//	        row's claim mint (a straggler from the previous epoch of a
//	        re-claimed row) is dropped.
//	2065-G  the sweep's own entry count (trigger=sweep_lapse) and the
//	        R5 degraded pin: with zero events flowing, a naturally
//	        lapsed row processes through the sweep exactly as today —
//	        the counter is the ONLY sweep-side change.

package board_test

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// armLiveLease re-arms the row's lease far into the future — the honest
// live-claim shape. The suite's 200ms LeaseDuration exists for expiry
// tests; these pins need a lease ONLY the event can due (the pre-event
// sweep leg asserts today's no-touch behavior on the very same row).
func (s *BoardSuite) armLiveLease(taskID string) {
	_, err := s.pool.Exec(context.Background(),
		`UPDATE tasks SET lease_expires_at = now() + interval '1 hour' WHERE id=$1`, taskID)
	s.Require().NoError(err)
}

// seedClaimedTask seeds one claimable task in a fresh workspace and
// claims it — the standard lane shape (the lease_pins boilerplate).
func (s *BoardSuite) seedClaimedTask(title string) (ws string, c *board.Claim) {
	ws = s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	_ = s.seedTask(e, title, nil, false)
	c = s.claimOrFail(ws)
	return ws, c
}

// dueMarks reads one trigger label's current value (the delta-pair
// basis; prometheus counters are process-global, the suite is
// sequential).
func dueMarks(trigger string) float64 {
	return testutil.ToFloat64(board.DueMarks().WithLabelValues(trigger))
}

func (s *BoardSuite) TestPin_2065A_TurnEndNoAdvanceNudgesWithinOneSweep() {
	ws, c := s.seedClaimedTask("due-mark")
	s.armLiveLease(c.TaskID)
	assignee := s.reloadTask(c.TaskID).AssigneeSession // the spawner's stamp

	lives := &fakeLiveness{}
	lives.set(assignee, liveQuiet())
	nudger := &fakeNudger{}
	recon := s.pinRecon(lives, nudger.Send)

	// DELTA leg 1 — pre-event (today's behavior, the incident's cost):
	// a live-lease row is not a victim; one sweep changes nothing.
	s.rc1Sweep(recon)
	s.Equal(0, nudger.count(), "a live lease must not be swept")
	id, at := s.nudgeState(c.TaskID)
	s.Empty(id)
	s.Nil(at)

	// The event: attested turn-end, no board advance.
	before := dueMarks(board.DueMarkTriggerEvent)
	marked, err := s.svc.ConsumeTurnEnd(context.Background(), board.TurnEndDueMark{
		WorkspaceID: ws,
		SessionRefs: []string{assignee, "phrase-" + assignee},
		Epoch:       1,
	})
	s.Require().NoError(err)
	s.True(marked, "the no-advance event must due-mark the live claim")
	require.InDelta(s.T(), before+1, dueMarks(board.DueMarkTriggerEvent), 0.0001,
		"trigger=event counts the due-mark write")

	// The lease is due NOW — the delta the incident waited 1h47m for.
	var lease time.Time
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT lease_expires_at FROM tasks WHERE id=$1`, c.TaskID).Scan(&lease))
	s.False(lease.After(time.Now().Add(time.Second)),
		"the due-mark writes lease_expires_at=now() — the next sweep owns the row")

	// DELTA leg 2 — post-event: ONE sweep arms the nudge (the quiet
	// arms engaged; the sweep_lapse count registers the convergence).
	beforeLapse := dueMarks(board.DueMarkTriggerSweepLapse)
	s.rc1Sweep(recon)
	s.Equal(1, nudger.count(), "one sweep after the event: the sentinel is out")
	nudgeID, nudgedAt := s.nudgeState(c.TaskID)
	s.NotEmpty(nudgeID)
	s.NotNil(nudgedAt)
	require.InDelta(s.T(), beforeLapse+1, dueMarks(board.DueMarkTriggerSweepLapse), 0.0001,
		"the event-marked row converges into the sweep's entry count one cycle later")
}

func (s *BoardSuite) TestPin_2065B_AdvanceYesIsTelemetryOnly() {
	ws, c := s.seedClaimedTask("due-mark")
	s.armLiveLease(c.TaskID)

	var leaseBefore time.Time
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT lease_expires_at FROM tasks WHERE id=$1`, c.TaskID).Scan(&leaseBefore))

	beforeEvent := dueMarks(board.DueMarkTriggerEvent)
	beforeAdv := testutil.ToFloat64(board.TurnAdvancesCounter())
	marked, err := s.svc.ConsumeTurnEnd(context.Background(), board.TurnEndDueMark{
		WorkspaceID: ws,
		SessionRefs: []string{"sess-" + c.ShortID},
		Epoch:       1,
		Advanced:    true,
	})
	s.Require().NoError(err)
	s.False(marked, "advance=yes never due-marks (R2)")

	var leaseAfter time.Time
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT lease_expires_at FROM tasks WHERE id=$1`, c.TaskID).Scan(&leaseAfter))
	s.Equal(leaseBefore.UnixNano(), leaseAfter.UnixNano(), "the advancing leg leaves the lease byte-identical")
	require.InDelta(s.T(), beforeEvent, dueMarks(board.DueMarkTriggerEvent), 0.0001,
		"the due-mark counter does not move for advance=yes")
	require.InDelta(s.T(), beforeAdv+1, testutil.ToFloat64(board.TurnAdvancesCounter()), 0.0001,
		"the advance telemetry counter records the turn")
}

func (s *BoardSuite) TestPin_2065C_PendingInputAndPausedGuards() {
	for _, guard := range []struct {
		name string
		sql  string
	}{
		{"pending_input", `UPDATE tasks SET pending_input=true WHERE id=$1`},
		{"paused", `UPDATE tasks SET paused=true WHERE id=$1`},
	} {
		s.Run(guard.name, func() {
			ws, c := s.seedClaimedTask("guarded")
			s.armLiveLease(c.TaskID)
			_, err := s.pool.Exec(context.Background(), guard.sql, c.TaskID)
			s.Require().NoError(err)

			before := dueMarks(board.DueMarkTriggerEvent)
			marked, err := s.svc.ConsumeTurnEnd(context.Background(), board.TurnEndDueMark{
				WorkspaceID: ws,
				SessionRefs: []string{"sess-" + c.ShortID},
				Epoch:       1,
			})
			s.Require().NoError(err)
			s.False(marked, "a %s row is never due-marked by an event (R4)", guard.name)

			var lease time.Time
			s.Require().NoError(s.pool.QueryRow(context.Background(),
				`SELECT lease_expires_at FROM tasks WHERE id=$1`, c.TaskID).Scan(&lease))
			s.True(lease.After(time.Now()), "the guarded row's lease is untouched")
			require.InDelta(s.T(), before, dueMarks(board.DueMarkTriggerEvent), 0.0001)
		})
	}
}

func (s *BoardSuite) TestPin_2065D_ArmedEpisodeAndIdempotence() {
	ws, c := s.seedClaimedTask("due-mark")
	s.armLiveLease(c.TaskID)

	// An armed nudge slot: the sweep's quiet arms OWN the episode.
	_, err := s.pool.Exec(context.Background(),
		`UPDATE tasks SET nudge_id='sentinel-live', nudged_at=now() WHERE id=$1`, c.TaskID)
	s.Require().NoError(err)
	before := dueMarks(board.DueMarkTriggerEvent)
	marked, err := s.svc.ConsumeTurnEnd(context.Background(), board.TurnEndDueMark{
		WorkspaceID: ws, SessionRefs: []string{"sess-" + c.ShortID}, Epoch: 1,
	})
	s.Require().NoError(err)
	s.False(marked, "a positioned sentinel owns the episode — the event re-marks nothing")
	require.InDelta(s.T(), before, dueMarks(board.DueMarkTriggerEvent), 0.0001)

	// The already-due shape (and the cross-replica duplicate): a second
	// event for the same turn is a silent no-op — one due-mark per
	// episode, whichever replica loses the write race.
	_, err = s.pool.Exec(context.Background(),
		`UPDATE tasks SET nudge_id=NULL, nudged_at=NULL, lease_expires_at = now() - interval '1 second' WHERE id=$1`, c.TaskID)
	s.Require().NoError(err)
	marked, err = s.svc.ConsumeTurnEnd(context.Background(), board.TurnEndDueMark{
		WorkspaceID: ws, SessionRefs: []string{"sess-" + c.ShortID}, Epoch: 1,
	})
	s.Require().NoError(err)
	s.False(marked, "an already-due row is the sweep's episode (idempotent no-op)")
	require.InDelta(s.T(), before, dueMarks(board.DueMarkTriggerEvent), 0.0001,
		"duplicates never double-count")
}

func (s *BoardSuite) TestPin_2065E_ForeignIdentityIsSilentNoOp() {
	ws, c := s.seedClaimedTask("due-mark") // a live claim exists — the event must still miss on identity
	s.armLiveLease(c.TaskID)

	before := dueMarks(board.DueMarkTriggerEvent)
	// A user-chat session (should one ever emit): no claimed row matches.
	marked, err := s.svc.ConsumeTurnEnd(context.Background(), board.TurnEndDueMark{
		WorkspaceID: ws, SessionRefs: []string{"ses_user_chat_session"}, Epoch: 1,
	})
	s.Require().NoError(err)
	s.False(marked)
	// The true assignee under a FOREIGN workspace: the project join scopes it out.
	marked, err = s.svc.ConsumeTurnEnd(context.Background(), board.TurnEndDueMark{
		WorkspaceID: "00000000-0000-0000-0000-000000000000",
		SessionRefs: []string{"sess-" + c.ShortID},
		Epoch:       1,
	})
	s.Require().NoError(err)
	s.False(marked)
	// Unattributable (no identity keys): never a due-mark.
	marked, err = s.svc.ConsumeTurnEnd(context.Background(), board.TurnEndDueMark{
		WorkspaceID: ws, Epoch: 1,
	})
	s.Require().NoError(err)
	s.False(marked)
	require.InDelta(s.T(), before, dueMarks(board.DueMarkTriggerEvent), 0.0001)

	var lease time.Time
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT lease_expires_at FROM tasks WHERE id=$1`, c.TaskID).Scan(&lease))
	s.True(lease.After(time.Now()), "foreign events touch nothing")
}

func (s *BoardSuite) TestPin_2065F_RowEpochGuardDropsPreMintEvents() {
	ws, c := s.seedClaimedTask("due-mark")
	s.armLiveLease(c.TaskID)

	// The row re-claimed a moment AFTER the event's observation: the
	// row's claim epoch (stage_entered_at, written by every claim-path
	// mint) is ahead of the event — a straggler from the PREVIOUS
	// epoch, dropped (the audit ruling's epoch-mismatch guard, row
	// side; the consumer-side watermark is pinned in api/internal/app).
	_, err := s.pool.Exec(context.Background(),
		`UPDATE tasks SET stage_entered_at = now() + interval '1 hour' WHERE id=$1`, c.TaskID)
	s.Require().NoError(err)

	before := dueMarks(board.DueMarkTriggerEvent)
	marked, err := s.svc.ConsumeTurnEnd(context.Background(), board.TurnEndDueMark{
		WorkspaceID: ws,
		SessionRefs: []string{"sess-" + c.ShortID},
		Epoch:       7,
	})
	s.Require().NoError(err)
	s.False(marked, "an event observed before the row's claim mint cannot due-mark the fresh epoch")
	require.InDelta(s.T(), before, dueMarks(board.DueMarkTriggerEvent), 0.0001)
}

func (s *BoardSuite) TestPin_2065G_SweepLapseCountsWhenStreamIsSilent() {
	// R5: with the event stream degraded (zero events), a naturally
	// lapsed row processes through the sweep exactly as today — the
	// sweep_lapse counter is the only observable difference.
	_, c := s.seedClaimedTask("silent-stream")
	s.expireLease(c.TaskID)

	assignee := s.reloadTask(c.TaskID).AssigneeSession
	lives := &fakeLiveness{}
	lives.set(assignee, liveQuiet())
	nudger := &fakeNudger{}
	recon := s.pinRecon(lives, nudger.Send)

	beforeLapse := dueMarks(board.DueMarkTriggerSweepLapse)
	beforeEvent := dueMarks(board.DueMarkTriggerEvent)
	s.rc1Sweep(recon)
	require.InDelta(s.T(), beforeLapse+1, dueMarks(board.DueMarkTriggerSweepLapse), 0.0001,
		"the sweep's own entry count moves for the natural lapse")
	require.InDelta(s.T(), beforeEvent, dueMarks(board.DueMarkTriggerEvent), 0.0001,
		"no events flowing: the event trigger never moves")
	s.Equal(1, nudger.count(), "the ordinary quiet arms ran — today's behavior, unchanged")
}
