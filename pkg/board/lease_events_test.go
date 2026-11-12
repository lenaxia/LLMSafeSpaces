// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

// lease_events_test.go — the S4 observation-plane pins (design 0072):
// the ladder's decisions project POST-COMMIT to the lease-event sink,
// carrying their WHY (which rung renewed, what evidence wedged, why a
// claim was reclaimed) — the events whose absence during the
// 2026-10-30 incident forced transcript forensics. The nudge rung is
// deliberately absent here (the app-side nudger is the nudged
// publisher — lease_events.go's header); these pins cover everything
// the ENGINE decides.

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// recordingLeaseSink records every projected decision.
type recordingLeaseSink struct {
	mu     sync.Mutex
	events []board.LeaseEvent
	// stagesAtEmission snapshots each event's row stage AT DELIVERY —
	// the post-commit proof (a reclaimed event must see the row
	// already re-queued; a pre-commit projection would see in_progress).
	stagesAtEmission []string
	pool             *pgxpool.Pool
}

func (r *recordingLeaseSink) Sink(ctx context.Context, ev board.LeaseEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	if r.pool != nil {
		var stage string
		if err := r.pool.QueryRow(ctx, `SELECT stage FROM tasks WHERE id=$1`, ev.TaskID).Scan(&stage); err == nil {
			r.stagesAtEmission = append(r.stagesAtEmission, stage)
		} else {
			r.stagesAtEmission = append(r.stagesAtEmission, "err")
		}
	}
}

func (r *recordingLeaseSink) snapshot() ([]board.LeaseEvent, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]board.LeaseEvent, len(r.events))
	copy(out, r.events)
	stages := make([]string, len(r.stagesAtEmission))
	copy(stages, r.stagesAtEmission)
	return out, stages
}

// pinReconWithSink builds the pin reconciler with the S4 sink
// recording every projection.
func (s *BoardSuite) pinReconWithSink(lives board.AssigneeLivenessSource, nudger board.NudgeSender) (*board.Reconciler, *recordingLeaseSink) {
	rec := &recordingLeaseSink{}
	return board.NewReconciler(s.pool, s.cfg,
		board.WithLivenessSource(lives),
		board.WithNudgeSender(nudger),
		board.WithLeaseEventSink(rec.Sink),
		board.WithWorkerAborter(s.aborter.Abort),
		board.WithPRDriftSource(s.drift),
		board.WithPendingInputSource(s.pending),
		board.WithFailpoint(s.fps.fp),
	), rec
}

// TestPin_LeaseEvents_RenewalCarriesItsRung: a MOVING worker's renewal
// projects with source=movement; a busy-only renewal (the 2026-10-29
// busy-gate's silent-but-mid-turn shape) projects with source=busy_gate
// — the rung is the operator's "why was this alive" without forensics.
func (s *BoardSuite) TestPin_LeaseEvents_RenewalCarriesItsRung() {
	for _, tc := range []struct {
		name   string
		obs    board.AssigneeObservation
		source string
	}{
		{"movement", liveMoving(), board.RenewSourceMovement},
		{"busy-gate", board.AssigneeObservation{Observed: true, Exists: true, Busy: true}, board.RenewSourceBusy},
	} {
		s.Run(tc.name, func() {
			ws := s.seedWorkspace()
			p := s.seedProject(ws, 4)
			e := s.seedEpic(p, board.EpicActive)
			task := s.seedTask(e, "renew-"+tc.name, nil, false)

			s.claimOrFail(ws)
			assignee := s.reloadTask(task.ID).AssigneeSession
			s.Require().NotEmpty(assignee)

			lives := &fakeLiveness{}
			lives.set(assignee, tc.obs)
			recon, rec := s.pinReconWithSink(lives,
				func(context.Context, string, board.NudgePayload) error {
					s.T().Fatalf("this rung never nudges")
					return nil
				})
			rec.pool = s.pool

			s.expireLease(task.ID)
			s.rc1Sweep(recon)

			events, _ := rec.snapshot()
			s.Require().Len(events, 1, "exactly one renewal decision projects")
			ev := events[0]
			s.Equal(board.LeaseEventRenewed, ev.Kind)
			s.Equal(task.ID, ev.TaskID)
			s.Equal(assignee, ev.Assignee)
			s.Equal(tc.source, ev.Source, "the renewal names its rung")
			s.Equal(int32(0), ev.FailureCount)
			// A renewal never re-queues: the row is still claimed at
			// emission (post-commit read sees the extended lease).
			s.Equal(board.StageInProgress, s.rawTask(task.ID).Stage)
		})
	}
}

// TestPin_LeaseEvents_WedgeCarriesEvidenceAndPostCommitState: the
// quiet-past-sentinel wedge projects with its evidence summary and the
// sentinel it measured against — and the row is ALREADY failed when
// the sink sees it (post-commit projection, never an attempt).
func (s *BoardSuite) TestPin_LeaseEvents_WedgeCarriesEvidenceAndPostCommitState() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "wedged-worker", nil, false)

	s.claimOrFail(ws)
	assignee := s.reloadTask(task.ID).AssigneeSession
	s.Require().NotEmpty(assignee)

	lives := &fakeLiveness{}
	lives.set(assignee, liveQuiet())
	recon, rec := s.pinReconWithSink(lives, func(context.Context, string, board.NudgePayload) error { return nil })
	rec.pool = s.pool

	// Round 1: the sentinel goes out (the NUDGE rung — no engine-side
	// event by design; the app nudger is that publisher).
	s.expireLease(task.ID)
	s.rc1Sweep(recon)
	nid, _ := s.nudgeState(task.ID)
	s.Require().NotEmpty(nid, "round 1 sends the sentinel")
	events, _ := rec.snapshot()
	s.Empty(events, "the nudge rung projects nothing engine-side (the app nudger owns the nudged topic)")

	// Round 2: still quiet, sentinel positioned → the machine verdict.
	lives.set(assignee, board.AssigneeObservation{Observed: true, Exists: true, NudgeMarkKnown: true})
	s.expireLease(task.ID)
	s.rc1Sweep(recon)

	events, stages := rec.snapshot()
	s.Require().Len(events, 1, "the wedge verdict projects exactly once")
	ev := events[0]
	s.Equal(board.LeaseEventWedged, ev.Kind)
	s.Equal(nid, ev.NudgeID, "the verdict names the sentinel it measured")
	s.Equal("no session events after the sentinel", ev.Evidence)
	s.Equal(assignee, ev.Assignee)
	s.Equal(int32(1), ev.FailureCount, "the wedge counts as the attempt failure")
	s.Equal([]string{board.StageFailed}, stages,
		"post-commit proof: the sink sees the row already failed, never in-flight")
}

// TestPin_LeaseEvents_ReclaimCarriesItsReason: each reclaim path names
// why — dead evidence, a nudge that could not be attempted, and the
// no-identity wall-clock lapse.
func (s *BoardSuite) TestPin_LeaseEvents_ReclaimCarriesItsReason() {
	s.Run("dead-evidence", func() {
		ws := s.seedWorkspace()
		p := s.seedProject(ws, 4)
		e := s.seedEpic(p, board.EpicActive)
		task := s.seedTask(e, "dead-session", nil, false)

		s.claimOrFail(ws)
		assignee := s.reloadTask(task.ID).AssigneeSession
		lives := &fakeLiveness{}
		lives.set(assignee, dead())
		recon, rec := s.pinReconWithSink(lives, nil)

		s.expireLease(task.ID)
		s.rc1Sweep(recon)

		events, _ := rec.snapshot()
		s.Require().Len(events, 1)
		s.Equal(board.LeaseEventReclaimed, events[0].Kind)
		s.Equal(board.ReclaimReasonDeadEvidence, events[0].Reason)
		s.Equal(assignee, events[0].Assignee)
		s.Equal(int32(1), events[0].FailureCount)
	})

	s.Run("nudge-delivery-failed", func() {
		ws := s.seedWorkspace()
		p := s.seedProject(ws, 4)
		e := s.seedEpic(p, board.EpicActive)
		task := s.seedTask(e, "undeliverable", nil, false)

		s.claimOrFail(ws)
		assignee := s.reloadTask(task.ID).AssigneeSession
		lives := &fakeLiveness{}
		lives.set(assignee, liveQuiet())
		recon, rec := s.pinReconWithSink(lives,
			func(context.Context, string, board.NudgePayload) error {
				return errors.New("pin: delivery undecidable")
			})

		s.expireLease(task.ID)
		s.rc1Sweep(recon)

		events, _ := rec.snapshot()
		s.Require().Len(events, 1)
		s.Equal(board.LeaseEventReclaimed, events[0].Kind)
		s.Equal(board.ReclaimReasonDeliveryFailed, events[0].Reason,
			"an undecidable delivery is named — never a phantom nudge")
	})

	s.Run("no-assignee", func() {
		ws := s.seedWorkspace()
		p := s.seedProject(ws, 4)
		e := s.seedEpic(p, board.EpicActive)
		task := s.seedTask(e, "identity-less", nil, false)

		s.claimOrFail(ws)
		// Strip the identity: the wall-clock path with no one to ask.
		_, err := s.pool.Exec(context.Background(),
			`UPDATE tasks SET assignee_session=NULL WHERE id=$1`, task.ID)
		s.Require().NoError(err)

		recon, rec := s.pinReconWithSink(&fakeLiveness{}, nil)
		s.expireLease(task.ID)
		s.rc1Sweep(recon)

		events, _ := rec.snapshot()
		s.Require().Len(events, 1)
		s.Equal(board.LeaseEventReclaimed, events[0].Kind)
		s.Equal(board.ReclaimReasonNoAssignee, events[0].Reason)
		s.Empty(events[0].Assignee)
	})
}

// TestPin_LeaseEvents_HeartbeatResponseProjectsRenewal: the response
// channel's renewal — the worker answered the sentinel with the tool
// the prose demanded — projects with source=heartbeat_response at the
// heartbeat's own write (the sweep is not involved).
func (s *BoardSuite) TestPin_LeaseEvents_HeartbeatResponseProjectsRenewal() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "responsive-worker", nil, false)

	c := s.claimOrFail(ws)
	assignee := s.reloadTask(task.ID).AssigneeSession
	s.Require().NotEmpty(assignee)
	s.setNudge(task.ID, "nudge-hb")

	sink := &recordingLeaseSink{}
	svc := board.NewService(s.pool, s.cfg, board.WithLeaseEventSink(sink.Sink))
	res, err := svc.Heartbeat(context.Background(), s.agent(assignee, c.ClaimToken), task.ID, "", time.Now().UTC().Add(-time.Minute))
	s.Require().NoError(err)
	s.True(res.Extended, "a heartbeat on a nudged row responds")

	events, _ := sink.snapshot()
	s.Require().Len(events, 1, "the response projects exactly one renewal")
	s.Equal(board.LeaseEventRenewed, events[0].Kind)
	s.Equal(board.RenewSourceHeartbeat, events[0].Source)
	s.Equal(assignee, events[0].Assignee)
	s.Equal(task.ID, events[0].TaskID)
}

// TestPin_LeaseEvents_NilSinkIsInert: no sink installed — the ladder's
// behavior is byte-identical (the pre-S4 posture; the projection is
// additive, never a dependency). Renewal path spot-pin.
func (s *BoardSuite) TestPin_LeaseEvents_NilSinkIsInert() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "no-sink", nil, false)

	s.claimOrFail(ws)
	assignee := s.reloadTask(task.ID).AssigneeSession
	lives := &fakeLiveness{}
	lives.set(assignee, liveMoving())
	recon := s.pinRecon(lives, func(context.Context, string, board.NudgePayload) error {
		s.T().Fatalf("a moving worker is never nudged")
		return nil
	})

	s.expireLease(task.ID)
	s.rc1Sweep(recon)

	s.Equal(board.StageInProgress, s.rawTask(task.ID).Stage, "the renewal happened — no sink, no change")
	s.leaseFuture(task.ID)
}
