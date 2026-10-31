// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

import (
	"context"
	"encoding/json"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// S5 — the DEPOSED directive: first-class salvage handoff for lapsed
// attempts. The 2026-10-30 incident (glassy-shovel / orient-lantern)
// cost 4 worker sessions and 3 claims because the pushed work (branch,
// PR, last known state) lived nowhere on the row — recovery was the
// orchestrator reading transcripts and hand-writing a steering note.
// These pins hold the three charter guarantees: the pointer lands on
// the row, the next claim's charter embeds it, and a live healthy claim
// can never be deposed.

// deposePtr is the incident-shaped salvage pointer (the orient-lantern
// salvage: branch + head + what's on it).
func deposePtr() board.SalvagePointer {
	return board.SalvagePointer{
		Branch: "feat/sprint-board-story-vocabulary",
		PR:     "1997",
		Head:   "65e4d9dcd",
		State:  "full rename sweep pushed; worklog 1237, CHANGELOG, COORDINATE row; CI green on head",
		Note:   "verify against the acceptance criteria, then open the PR and iterate with the reviewer — do not redo the sweep",
	}
}

func (s *BoardSuite) salvageOnRow(taskID string) *board.SalvagePointer {
	var raw []byte
	err := s.pool.QueryRow(context.Background(), `SELECT salvage FROM tasks WHERE id=$1`, taskID).Scan(&raw)
	s.Require().NoError(err)
	if len(raw) == 0 {
		return nil
	}
	var p board.SalvagePointer
	s.Require().NoError(json.Unmarshal(raw, &p))
	return &p
}

// KILL-PIN S5-a: deposing a LAPSED in_progress claim records the
// salvage pointer on the row, requeues exactly as the reclaim would
// (failure overlay + backoff, attempts untouched), delivers the DEPOSED
// directive to the stale session, and writes exactly one audited depose
// event whose payload carries the pointer. A requestId replay adds
// nothing.
func (s *BoardSuite) TestDepose_LapsedClaimRecordsSalvageAndRequeues() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	t := s.seedTask(e, "salvageable", nil, false)

	msg := &fakeMessenger{}
	lives := &fakeLiveness{}
	s.svc = s.newDeposeService(msg, lives)

	c := s.claimOrFail(ws)
	require.Equal(s.T(), t.ID, c.TaskID)
	assignee := s.rawTask(t.ID).Assignee
	s.Require().NotNil(assignee)
	s.Require().NotEmpty(*assignee, "the fake spawner stamps the assignee at claim")

	// Lap the claim, keep the session observably DEAD (no movement, no
	// busy, no post-sentinel reaction): the honest wall-clock shape.
	s.expireLease(t.ID)
	lives.set(*assignee, board.AssigneeObservation{Observed: true, Exists: false})

	out, err := s.svc.DeposeTask(ctx, s.agentManager(ws, "orchestrator-depose"), "", t.ShortID, deposePtr(), "req-depose-1")
	s.Require().NoError(err)
	require.Equal(s.T(), board.StageBacklog, out.Stage, "the lapsed claim was deposed and requeued")

	// The pointer on the row — the criterion as written: branch/PR/state.
	got := s.salvageOnRow(t.ID)
	require.NotNil(s.T(), got, "the salvage pointer must live on the row")
	require.Equal(s.T(), deposePtr(), *got)
	require.NotNil(s.T(), out.Salvage, "the read edge carries the pointer")
	require.Equal(s.T(), "1997", out.Salvage.PR)

	// The reclaim-parity overlay: claim cleared, failure counted once,
	// attempts left for the next claim, cooldown parked the row.
	row := s.rawTask(t.ID)
	require.Nil(s.T(), row.ClaimToken)
	require.Nil(s.T(), row.Assignee, "the deposed session no longer holds the row")
	var failures int
	s.Require().NoError(s.pool.QueryRow(ctx, `SELECT failure_count FROM tasks WHERE id=$1`, t.ID).Scan(&failures))
	require.Equal(s.T(), 1, failures, "depose counts the lapse exactly as the reclaim would")
	require.Equal(s.T(), 1, row.Attempts, "attempts untouched — the increment belongs to the next claim")
	require.Equal(s.T(), 1, s.countTransitions(t.ID, board.StageBacklog), "one in_progress->backlog transition")

	// The DEPOSED directive landed on the stale session (the
	// HALT/RESUME transport — turn-boundary, not an abort).
	require.Len(s.T(), msg.calls, 1)
	require.Contains(s.T(), msg.calls[0], board.DirectiveDeposed)
	require.Contains(s.T(), msg.calls[0], *assignee)
	require.Equal(s.T(), 0, s.aborter.count(), "depose NEVER aborts the session")

	// Audited: exactly one depose event, payload carrying the pointer.
	require.Equal(s.T(), 1, s.countEvents("depose"))
	var payload []byte
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT payload FROM board_events WHERE project_id=$1 AND action='depose'`, p.ID).Scan(&payload))
	var ev struct {
		Salvage        board.SalvagePointer `json:"salvage"`
		DeposedSession string               `json:"deposed_session"`
		FromStage      string               `json:"from_stage"`
	}
	s.Require().NoError(json.Unmarshal(payload, &ev))
	require.Equal(s.T(), deposePtr(), ev.Salvage, "the audit event embeds the recorded pointer")
	require.Equal(s.T(), *assignee, ev.DeposedSession)
	require.Equal(s.T(), board.StageInProgress, ev.FromStage)

	// The replay (same requestId) writes nothing new.
	_, err = s.svc.DeposeTask(ctx, s.agentManager(ws, "orchestrator-depose"), "", t.ShortID, deposePtr(), "req-depose-1")
	s.Require().NoError(err, "a replayed requestId returns the original outcome, never an error")
	require.Equal(s.T(), 1, s.countEvents("depose"), "the replay added no second event")
	require.Equal(s.T(), 1, s.countTransitions(t.ID, board.StageBacklog))
	require.Len(s.T(), msg.calls, 1, "the replay delivered no second directive")
}

// KILL-PIN S5-b: the next claim's charter envelope embeds the salvage
// note automatically — inherit, don't restart. The fresh-grant path
// carries Claim.Salvage from the row; the claim's Task read carries it
// too; and after the pointer is CONSUMED by reality (a new depose
// overwrites it latest-only), the newest truth is what embeds.
func (s *BoardSuite) TestDepose_NextClaimCharterEmbedsSalvage() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	t := s.seedTask(e, "handoff", nil, false)

	msg := &fakeMessenger{}
	s.svc = s.newDeposeService(msg, &fakeLiveness{})

	c := s.claimOrFail(ws)
	require.Equal(s.T(), t.ID, c.TaskID)
	require.Nil(s.T(), c.Salvage, "a pointer-less claim embeds nothing (the pre-depose shape)")

	s.expireLease(t.ID)
	_, err := s.svc.DeposeTask(ctx, s.agentManager(ws, "orchestrator-depose"), "", t.ShortID, deposePtr(), "")
	s.Require().NoError(err)
	s.clearTaskBackoff(t.ID)

	c2, err := s.svc.ClaimNext(ctx, "worker-"+ws, ws)
	s.Require().NoError(err)
	require.Equal(s.T(), t.ID, c2.TaskID)
	require.NotNil(s.T(), c2.Salvage, "the fresh claim inherits the salvage pointer")
	require.Equal(s.T(), deposePtr(), *c2.Salvage)
	require.Equal(s.T(), "feat/sprint-board-story-vocabulary", c2.Salvage.Branch)
	require.Equal(s.T(), "1997", c2.Salvage.PR)
	require.NotNil(s.T(), c2.Task, "the claim's task read is loaded (the spawn path's payload)")
	require.NotNil(s.T(), c2.Task.Salvage, "the row read carries the pointer (board_read parity)")

	// Latest-only: a SECOND depose (this claim lapses too) overwrites
	// the pointer with the newer truth, and the third claim embeds THAT.
	s.expireLease(t.ID)
	_, err = s.svc.DeposeTask(ctx, s.agentManager(ws, "orchestrator-depose"), "", t.ShortID,
		board.SalvagePointer{Branch: "feat/sprint-board-story-vocabulary", Head: "65e4d9dcd",
			State: "plus the review-fix commit d2bc566a9 (worklog NNNN_ sentinel, e2e tier)"}, "")
	s.Require().NoError(err)
	s.clearTaskBackoff(t.ID)
	c3, err := s.svc.ClaimNext(ctx, "worker-"+ws, ws)
	s.Require().NoError(err)
	require.Equal(s.T(), t.ID, c3.TaskID)
	require.NotNil(s.T(), c3.Salvage)
	require.Equal(s.T(), "65e4d9dcd", c3.Salvage.Head)
	require.NotEqual(s.T(), deposePtr(), *c3.Salvage, "the newest pointer wins (latest-only)")
}

// KILL-PIN S5-c (the pinned guard): a LIVE healthy claim refuses
// deposition. Unexpired lease → GuardClaimLive, nothing written, no
// event, no directive. Life evidence on an EXPIRED lease refuses too:
// observation movement / mid-turn busy, and the post-nudge heartbeat.
func (s *BoardSuite) TestDepose_LiveClaimRefuses() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	t := s.seedTask(e, "alive", nil, false)

	msg := &fakeMessenger{}
	lives := &fakeLiveness{}
	s.svc = s.newDeposeService(msg, lives)

	c := s.claimOrFail(ws)
	require.Equal(s.T(), t.ID, c.TaskID)
	assignee := *s.rawTask(t.ID).Assignee

	// Arm 1 — unexpired lease: flat refusal.
	_, err := s.svc.DeposeTask(ctx, s.agentManager(ws, "orchestrator-depose"), "", t.ShortID, deposePtr(), "")
	var guard *board.GuardError
	require.ErrorAs(s.T(), err, &guard)
	require.Equal(s.T(), board.GuardClaimLive, guard.Code)
	require.Equal(s.T(), board.StageInProgress, s.rawTask(t.ID).Stage, "nothing was written")
	require.Nil(s.T(), s.salvageOnRow(t.ID))
	require.Equal(s.T(), 0, s.countEvents("depose"))
	require.Empty(s.T(), msg.calls)

	// Arm 2 — expired lease, but the observation seam sees MOVEMENT:
	// the ladder would renew this worker; depose refuses.
	s.expireLease(t.ID)
	lives.set(assignee, board.AssigneeObservation{Observed: true, Exists: true, MovedSinceLastSweep: true})
	_, err = s.svc.DeposeTask(ctx, s.agentManager(ws, "orchestrator-depose"), "", t.ShortID, deposePtr(), "")
	require.ErrorAs(s.T(), err, &guard)
	require.Equal(s.T(), board.GuardClaimLive, guard.Code)
	require.GreaterOrEqual(s.T(), lives.askedFor(assignee), 1, "the guard consulted the observation seam")
	require.Nil(s.T(), s.salvageOnRow(t.ID), "refused deposes write no pointer")

	// Arm 3 — expired lease, mid-turn busy (the 2026-10-29 busy-gate
	// lesson: a silent-but-busy turn is alive): refuses.
	lives.set(assignee, board.AssigneeObservation{Observed: true, Exists: true, Busy: true})
	_, err = s.svc.DeposeTask(ctx, s.agentManager(ws, "orchestrator-depose"), "", t.ShortID, deposePtr(), "")
	require.ErrorAs(s.T(), err, &guard)
	require.Equal(s.T(), board.GuardClaimLive, guard.Code)

	// Arm 4 — the post-nudge heartbeat (the response channel): a
	// heartbeat later than the sentinel is the strong alive signal,
	// whatever the observation seam says (here: dead-evidence).
	_, execErr := s.pool.Exec(ctx, `
		UPDATE tasks SET nudge_id='n-1', nudged_at=now()-interval '2 minutes',
			last_heartbeat_at=now()-interval '1 minute' WHERE id=$1`, t.ID)
	s.Require().NoError(execErr)
	lives.set(assignee, board.AssigneeObservation{Observed: true, Exists: false})
	_, err = s.svc.DeposeTask(ctx, s.agentManager(ws, "orchestrator-depose"), "", t.ShortID, deposePtr(), "")
	require.ErrorAs(s.T(), err, &guard)
	require.Equal(s.T(), board.GuardClaimLive, guard.Code)
	require.Equal(s.T(), board.StageInProgress, s.rawTask(t.ID).Stage, "still never deposed")
	require.Equal(s.T(), 0, s.countEvents("depose"))

	// Arm 5 — degradable honesty: quiet + expired + NO observation
	// source deposes on the wall-clock lease alone (the S1 trust
	// chain: the stamp/lapse evidence is the authority). The nudge/
	// heartbeat slots clear first — Arm 4's heartbeat evidence is
	// legitimately still refusing.
	_, execErr = s.pool.Exec(ctx,
		`UPDATE tasks SET nudge_id=NULL, nudged_at=NULL, last_heartbeat_at=NULL WHERE id=$1`, t.ID)
	s.Require().NoError(execErr)
	s.svc = s.newDeposeService(msg, nil)
	out, err := s.svc.DeposeTask(ctx, s.agentManager(ws, "orchestrator-depose"), "", t.ShortID, deposePtr(), "")
	s.Require().NoError(err)
	require.Equal(s.T(), board.StageBacklog, out.Stage)
	require.NotNil(s.T(), s.salvageOnRow(t.ID))
}

// KILL-PIN S5-d: the incident's actual shape — the orchestrator
// notices AFTER the reclaim already requeued the row. A backlog row
// takes the pointer alone: no stage change, no second failure count,
// no transition, one audited event, no directive (the assignee is
// gone).
func (s *BoardSuite) TestDepose_BacklogRowRecordsPointerOnly() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	t := s.seedTask(e, "post-reclaim", nil, false)

	msg := &fakeMessenger{}
	s.svc = s.newDeposeService(msg, &fakeLiveness{})

	c := s.claimOrFail(ws)
	require.Equal(s.T(), t.ID, c.TaskID)
	s.expireLease(t.ID)
	s.sweep() // the rc1 reclaim: backlog + failure_count 1 + cooldown
	require.Equal(s.T(), board.StageBacklog, s.rawTask(t.ID).Stage)

	out, err := s.svc.DeposeTask(ctx, s.agentManager(ws, "orchestrator-depose"), "", t.ShortID, deposePtr(), "req-depose-2")
	s.Require().NoError(err)
	require.Equal(s.T(), board.StageBacklog, out.Stage)
	require.Equal(s.T(), deposePtr(), *s.salvageOnRow(t.ID))

	var failures int
	s.Require().NoError(s.pool.QueryRow(ctx, `SELECT failure_count FROM tasks WHERE id=$1`, t.ID).Scan(&failures))
	require.Equal(s.T(), 1, failures, "the backlog depose counts no second failure")
	require.Equal(s.T(), 1, s.countTransitions(t.ID, board.StageBacklog),
		"the ONLY backlog transition is the reclaim's — the depose moved nothing")
	require.Equal(s.T(), 1, s.countEvents("depose"))
	require.Empty(s.T(), msg.calls, "no assignee on the row — the directive messages nobody")

	// And the pointer still reaches the next claim (cooldown cleared).
	s.clearTaskBackoff(t.ID)
	c2, err := s.svc.ClaimNext(ctx, "worker-"+ws, ws)
	s.Require().NoError(err)
	require.Equal(s.T(), t.ID, c2.TaskID)
	require.NotNil(s.T(), c2.Salvage, "the post-reclaim pointer embeds in the next claim too")
}

// KILL-PIN S5-e: the enumerated refusals — done is immutable, failed
// takes re-arm, in_review belongs to the persona pass, an empty
// pointer is invalid input, and the WORKER-grade actor never deposes
// (manager route only).
func (s *BoardSuite) TestDepose_EnumeratedRefusals() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)

	msg := &fakeMessenger{}
	s.svc = s.newDeposeService(msg, &fakeLiveness{})

	// Seed every row UP FRONT (finishClaim completes an epic whose
	// stories are all done — #1948 — and further creates refuse).
	done := s.seedTask(e, "done-row", nil, false)
	failed := s.seedTask(e, "failed-row", nil, false)
	review := s.seedTask(e, "review-row", nil, false)
	backlog := s.seedTask(e, "plain", nil, false)

	// done is immutable.
	c := s.claimOrFail(ws)
	require.Equal(s.T(), done.ID, c.TaskID)
	s.finishClaim(c)
	_, err := s.svc.DeposeTask(ctx, s.agentManager(ws, "orchestrator-depose"), "", done.ShortID, deposePtr(), "")
	var guard *board.GuardError
	require.ErrorAs(s.T(), err, &guard)
	require.Equal(s.T(), board.GuardDone, guard.Code)

	// failed takes re-arm.
	_, execErr := s.pool.Exec(ctx,
		`UPDATE tasks SET stage='failed', pending_origin='escalated' WHERE id=$1`, failed.ID)
	s.Require().NoError(execErr)
	_, err = s.svc.DeposeTask(ctx, s.agentManager(ws, "orchestrator-depose"), "", failed.ShortID, deposePtr(), "")
	require.ErrorAs(s.T(), err, &guard)
	require.Equal(s.T(), board.GuardStage, guard.Code)

	// in_review belongs to the persona pass.
	_, execErr = s.pool.Exec(ctx,
		`UPDATE tasks SET stage='in_review', stage_entered_at=now() WHERE id=$1`, review.ID)
	s.Require().NoError(execErr)
	_, err = s.svc.DeposeTask(ctx, s.agentManager(ws, "orchestrator-depose"), "", review.ShortID, deposePtr(), "")
	require.ErrorAs(s.T(), err, &guard)
	require.Equal(s.T(), board.GuardStage, guard.Code)

	// An all-empty pointer is invalid input (the audit event must
	// record SOMETHING for the handoff to exist).
	_, err = s.svc.DeposeTask(ctx, s.agentManager(ws, "orchestrator-depose"), "", backlog.ShortID, board.SalvagePointer{}, "")
	require.ErrorAs(s.T(), err, &guard)
	require.Equal(s.T(), board.GuardInput, guard.Code)
	require.Nil(s.T(), s.salvageOnRow(backlog.ID))

	// The foreign owner never deposes (R7 ownership; the agent seam's
	// workspace binding is the same class — resolution scopes first).
	_, err = s.svc.DeposeTask(ctx, board.Actor{Kind: board.ActorUser, UserID: "user-other", Surface: board.SurfaceAgentManager, WorkspaceID: ws}, "", backlog.ShortID, deposePtr(), "")
	require.ErrorAs(s.T(), err, &guard)
	require.Equal(s.T(), board.GuardAuthz, guard.Code)
}

// newDeposeService builds the depose-test service: spawner (assignee
// stamping at claim), messenger (directive capture), and a liveness
// source (nil = the degradable no-evidence rung).
func (s *BoardSuite) newDeposeService(msg *fakeMessenger, lives board.AssigneeLivenessSource) *board.Service {
	opts := []board.Option{
		board.WithWorkerSpawner(s.spawner.Spawn),
		board.WithWorkerAborter(s.aborter.Abort),
		board.WithWorkerMessenger(msg.Deliver),
		board.WithFailpoint(s.fps.fp),
		board.WithRoleDirectory(personaRoleDirectory()),
	}
	if lives != nil {
		opts = append(opts, board.WithLivenessSource(lives))
	}
	return board.NewService(s.pool, s.cfg, opts...)
}
