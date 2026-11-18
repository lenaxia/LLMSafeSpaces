// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// Salvage-prefer terminal (§5.2-§5.3, the wait-salvage-terminal story):
// at BOTH worker-claim exhaustion sites (the dispatch claim's escalate
// arm and task_start's), differing salvage facts depose-with-salvage —
// the S5 pointer, backlog behind the failure backoff, attempts=0 (the
// rearm posture), one audited salvage_depose event — INSTEAD of
// stage='failed'. Nothing salvageable, an inert source, or facts
// identical to the recorded pointer (the loop bound) keep today's
// failed terminal byte-identical. The scoped-out third site (the
// reconciler's persona claim-time escalation) never probes, pinned.

// fakeSalvageSource scripts the seam's answer and records the probes.
type fakeSalvageSource struct {
	mu      sync.Mutex
	signals *board.SalvageSignals
	asked   int
}

func (f *fakeSalvageSource) SalvageFor(_ context.Context, _ *board.Task) *board.SalvageSignals {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked++
	return f.signals
}

func (f *fakeSalvageSource) probeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asked
}

func (f *fakeSalvageSource) set(s *board.SalvageSignals) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signals = s
}

// newSalvageService builds the salvage-test service (the depose-test
// builder's shape + the probe seam).
func (s *BoardSuite) newSalvageService(msg *fakeMessenger, src board.SalvageSource) *board.Service {
	opts := []board.Option{
		board.WithWorkerSpawner(s.spawner.Spawn),
		board.WithWorkerAborter(s.aborter.Abort),
		board.WithWorkerMessenger(msg.Deliver),
		board.WithFailpoint(s.fps.fp),
		board.WithRoleDirectory(personaRoleDirectory()),
	}
	if src != nil {
		opts = append(opts, board.WithSalvageSource(src))
	}
	return board.NewService(s.pool, s.cfg, opts...)
}

// salvage2064 is the #2064-shape answer: an open PR from a lane branch.
func salvage2064() *board.SalvageSignals {
	return &board.SalvageSignals{Branch: "board/wait-salvage", Head: "deadbeef2064", OpenPR: "2064"}
}

// setAttempts parks the row at the attempts cap (the exhaustion
// precondition) without driving three real claim lapses.
func (s *BoardSuite) setAttempts(taskID string, n int) {
	_, err := s.pool.Exec(context.Background(), `UPDATE tasks SET attempts=$2 WHERE id=$1`, taskID, n)
	s.Require().NoError(err)
}

// cooldownFuture reports whether the row's cooldown still gates claims.
func (s *BoardSuite) cooldownFuture(taskID string) bool {
	var future bool
	err := s.pool.QueryRow(context.Background(),
		`SELECT cooldown_until > now() FROM tasks WHERE id=$1`, taskID).Scan(&future)
	s.Require().NoError(err)
	return future
}

// seedSalvage pre-records a pointer on the row (the loop-bound pin's
// precondition — a prior depose's record).
func (s *BoardSuite) seedSalvage(taskID string, p board.SalvagePointer) {
	raw, err := json.Marshal(p)
	s.Require().NoError(err)
	_, err = s.pool.Exec(context.Background(), `UPDATE tasks SET salvage=$2 WHERE id=$1`, taskID, raw)
	s.Require().NoError(err)
}

// assertFailedTerminal pins TODAY's exhaustion terminal byte-shape —
// the shared arm of the nothing/inert/loop-bound/persona pins.
func (s *BoardSuite) assertFailedTerminal(taskID string, attemptsAtCap int) {
	row := s.rawTask(taskID)
	require.Equal(s.T(), board.StageFailed, row.Stage, "000050: exhaustion lands the failure terminus")
	require.NotNil(s.T(), row.PendingOrigin)
	require.Equal(s.T(), board.PendingOriginEscalated, *row.PendingOrigin)
	require.False(s.T(), row.PendingInput)
	require.Equal(s.T(), attemptsAtCap, row.Attempts, "attempts stay at the cap (the exhaustion signal)")
	require.Equal(s.T(), 1, s.countEvents("escalate"), "exactly one escalate event (today's shape)")
	require.Equal(s.T(), 0, s.countEvents("salvage_depose"), "no salvage depose happened")
}

// KILL-PIN §5.3-a (the #2064 shape, BOTH sites): attempts at cap + an
// open PR from the lane ⇒ depose-with-salvage — pointer recorded,
// backlog behind the failure backoff, attempts 0 (the rearm posture),
// overlay cleared, one audited salvage_depose event whose payload
// carries the facts and attempts_at_cap — NEVER stage='failed'. The
// next claimant's charter inherits the pointer (rearm inheritance, the
// existing claimSalvage machinery), and a NEW head on a later
// exhaustion deposes again while the DEPOSED directive rides the
// existing sender seam to a surviving implementer identity.
func (s *BoardSuite) TestPin_ExhaustionWithOpenPR_SalvageDeposes() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)

	// --- the dispatch claim site (claimOnce's escalate arm) ----------
	t1 := s.seedTask(e, "claim-path-2064", nil, false)
	s.setAttempts(t1.ID, s.cfg.AttemptsCap)

	msg := &fakeMessenger{}
	src := &fakeSalvageSource{signals: salvage2064()}
	s.svc = s.newSalvageService(msg, src)

	c, err := s.svc.ClaimNext(ctx, "worker-"+ws, ws)
	s.Require().NoError(err)
	require.Equal(s.T(), t1.ID, c.TaskID)
	require.True(s.T(), c.Escalated, "the grant was withheld — the depose is the escalate arm's outcome")
	require.Equal(s.T(), board.StageBacklog, s.rawTask(t1.ID).Stage, "NEVER stage='failed' — the row re-pooled with its salvage")

	// The rearm posture + pointer: attempts 0, overlay cleared,
	// pending_origin NULL, cooldown gating, S5 pointer recorded.
	row := s.rawTask(t1.ID)
	require.Equal(s.T(), 0, row.Attempts, "attempts reset — the rearm posture")
	require.Nil(s.T(), row.PendingOrigin)
	require.False(s.T(), row.PendingInput)
	require.Nil(s.T(), row.ClaimToken, "no claim was minted")
	require.True(s.T(), s.cooldownFuture(t1.ID), "backlog BEHIND the failure backoff cooldown")
	var failures int
	var lastFailed bool
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT failure_count, last_attempt_failed FROM tasks WHERE id=$1`, t1.ID).Scan(&failures, &lastFailed))
	require.Equal(s.T(), 0, failures, "the depose is not a failure (rearm clears the overlay)")
	require.False(s.T(), lastFailed)

	got := s.salvageOnRow(t1.ID)
	require.NotNil(s.T(), got, "the S5 pointer lives on the row")
	require.Equal(s.T(), "board/wait-salvage", got.Branch)
	require.Equal(s.T(), "deadbeef2064", got.Head)
	require.Equal(s.T(), "2064", got.PR)
	require.Contains(s.T(), got.State, "exhaustion-salvage")

	// Audited: exactly one salvage_depose event, payload carrying the
	// facts + attempts_at_cap; NO escalate event on this row's path.
	require.Equal(s.T(), 1, s.countEvents("salvage_depose"))
	require.Equal(s.T(), 0, s.countEvents("escalate"))
	var payload []byte
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT payload FROM board_events WHERE action='salvage_depose'`).Scan(&payload))
	var ev struct {
		Salvage        board.SalvagePointer `json:"salvage"`
		Facts          board.SalvageSignals `json:"facts"`
		AttemptsAtCap  int                  `json:"attempts_at_cap"`
		DeposedSession string               `json:"deposed_session"`
		FromStage      string               `json:"from_stage"`
	}
	s.Require().NoError(json.Unmarshal(payload, &ev))
	require.Equal(s.T(), *salvage2064(), ev.Facts, "the audit event embeds the probe's facts")
	require.Equal(s.T(), s.cfg.AttemptsCap, ev.AttemptsAtCap)
	require.Equal(s.T(), board.StageBacklog, ev.FromStage)
	require.Equal(s.T(), *got, ev.Salvage, "the audit event embeds the recorded pointer")
	require.Equal(s.T(), 1, s.probeHit(t1.ID), "the seam was consulted exactly once")

	// The DEPOSED directive: the exhaustion row is identity-less in the
	// common path (the reclaims cleared it) — nothing is delivered.
	require.Empty(s.T(), msg.calls)

	// Rearm inheritance (the existing machinery, unchanged): after the
	// cooldown, the next claim's charter envelope embeds the pointer.
	s.clearTaskBackoff(t1.ID)
	c2 := s.claimOrFail(ws)
	require.Equal(s.T(), t1.ID, c2.TaskID)
	require.NotNil(s.T(), c2.Salvage, "the post-exhaustion claim inherits the salvage pointer")
	require.Equal(s.T(), "2064", c2.Salvage.PR)
	require.Equal(s.T(), 1, s.rawTask(t1.ID).Attempts, "the inherited claim is a fresh attempt (attempts from 0)")

	// A SURVIVING implementer identity gets the directive: a later
	// exhaustion (new cap reached) with a NEW head deposes again — one
	// round per real change — and this time a stale implementer is on
	// the row to notify.
	s.clearTaskBackoff(t1.ID)
	_, err = s.pool.Exec(ctx, `UPDATE tasks SET stage='backlog', attempts=$2, implementer_session='imp-stale-2064' WHERE id=$1`,
		t1.ID, s.cfg.AttemptsCap)
	s.Require().NoError(err)
	src.set(&board.SalvageSignals{Branch: "board/wait-salvage", Head: "cafebabe2100", OpenPR: "2064"})
	c3, err := s.svc.ClaimNext(ctx, "worker2-"+ws, ws)
	s.Require().NoError(err)
	require.True(s.T(), c3.Escalated)
	require.Equal(s.T(), board.StageBacklog, s.rawTask(t1.ID).Stage, "a new head deposed again — never failed")
	require.Equal(s.T(), 2, s.countEvents("salvage_depose"), "the second round audited its own episode")
	require.Len(s.T(), msg.calls, 1, "the DEPOSED directive rode the existing sender seam")
	require.Contains(s.T(), msg.calls[0], board.DirectiveDeposed)
	require.Contains(s.T(), msg.calls[0], "imp-stale-2064")
	require.Equal(s.T(), "cafebabe2100", s.salvageOnRow(t1.ID).Head, "latest-only pointer truth")

	// --- the manual dispatch site (TaskStart's escalate arm) ----------
	t2 := s.seedTask(e, "task-start-path-2064", nil, false)
	s.setAttempts(t2.ID, s.cfg.AttemptsCap)
	s.clearTaskBackoff(t1.ID) // keep t1's cooldown out of t2's claim path

	cs, err := s.svc.TaskStart(ctx, s.owner(), t2.ID, "", "")
	s.Require().NoError(err)
	require.True(s.T(), cs.Escalated)
	require.Equal(s.T(), board.StageBacklog, s.rawTask(t2.ID).Stage, "task_start's arm deposes too — NEVER failed")
	require.Equal(s.T(), 0, s.rawTask(t2.ID).Attempts)
	got2 := s.salvageOnRow(t2.ID)
	require.NotNil(s.T(), got2)
	require.Equal(s.T(), "2064", got2.PR)
	require.True(s.T(), s.cooldownFuture(t2.ID))
	require.Equal(s.T(), 3, s.countEvents("salvage_depose"), "both sites audit their episodes")
	require.Equal(s.T(), 0, s.countEvents("escalate"))
}

// KILL-PIN §5.3-b: exhaustion with NOTHING salvageable (the source
// answers nil) keeps today's failed terminal — the probe is consulted
// and the ordinary path runs byte-identically.
func (s *BoardSuite) TestPin_ExhaustionWithNothing_StaysFailed() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "nothing-left", nil, false)
	s.setAttempts(t1.ID, s.cfg.AttemptsCap)

	src := &fakeSalvageSource{signals: nil}
	s.svc = s.newSalvageService(&fakeMessenger{}, src)

	c, err := s.svc.ClaimNext(ctx, "worker-"+ws, ws)
	s.Require().NoError(err)
	require.True(s.T(), c.Escalated)
	s.assertFailedTerminal(t1.ID, s.cfg.AttemptsCap)
	require.Nil(s.T(), s.salvageOnRow(t1.ID), "no pointer without salvage")
	require.Equal(s.T(), 1, src.probeCount(), "the seam WAS consulted — it answered nothing")

	// The task_start arm's nothing-shape too.
	t2 := s.seedTask(e, "nothing-start", nil, false)
	s.setAttempts(t2.ID, s.cfg.AttemptsCap)
	cs, err := s.svc.TaskStart(ctx, s.owner(), t2.ID, "", "")
	s.Require().NoError(err)
	require.True(s.T(), cs.Escalated)
	require.Equal(s.T(), board.StageFailed, s.rawTask(t2.ID).Stage)
	require.Nil(s.T(), s.salvageOnRow(t2.ID))
}

// KILL-PIN §5.3-c (the loop bound): signals IDENTICAL to the row's
// recorded pointer are NOT new salvage — the ordinary failed terminal,
// the pointer left untouched, no second depose.
func (s *BoardSuite) TestPin_SalvageSamePointer_Terminal() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "same-facts", nil, false)
	s.setAttempts(t1.ID, s.cfg.AttemptsCap)
	prior := board.SalvagePointer{Branch: "board/wait-salvage", Head: "deadbeef2064", PR: "2064",
		State: "prior round's record"}
	s.seedSalvage(t1.ID, prior)

	src := &fakeSalvageSource{signals: salvage2064()} // facts identical to prior
	s.svc = s.newSalvageService(&fakeMessenger{}, src)

	c, err := s.svc.ClaimNext(ctx, "worker-"+ws, ws)
	s.Require().NoError(err)
	require.True(s.T(), c.Escalated)
	s.assertFailedTerminal(t1.ID, s.cfg.AttemptsCap)
	require.Equal(s.T(), prior, *s.salvageOnRow(t1.ID), "the recorded pointer is left byte-identical")
	require.Equal(s.T(), 1, src.probeCount(), "the probe ran; the loop bound refused the depose")
}

// KILL-PIN §5.3-d (the inert contract): NO salvage source installed ⇒
// the exhaustion terminal is byte-identical to today — same row shape,
// same escalate event, no probe surface at all.
func (s *BoardSuite) TestPin_NilSalvageSource_ByteIdentical() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "inert-source", nil, false)
	s.setAttempts(t1.ID, s.cfg.AttemptsCap)

	// The harness default service carries NO salvage source (and no
	// messenger): the pre-feature shape exactly.
	c, err := s.svc.ClaimNext(ctx, "worker-"+ws, ws)
	s.Require().NoError(err)
	require.True(s.T(), c.Escalated)
	s.assertFailedTerminal(t1.ID, s.cfg.AttemptsCap)
	require.Nil(s.T(), s.salvageOnRow(t1.ID))

	var exhausted string
	rows, qerr := s.pool.Query(ctx,
		`SELECT payload->>'exhausted' FROM board_events WHERE action='escalate' AND item_id=$1`, t1.ID)
	s.Require().NoError(qerr)
	for rows.Next() {
		s.Require().NoError(rows.Scan(&exhausted))
	}
	rows.Close()
	require.Equal(s.T(), "true", exhausted, "the escalate event payload carries exhausted=true (today's shape)")
}

// KILL-PIN §5.3-e (the scoped-out third site): the reconciler's persona
// claim-time escalation — failure_count at the cap on an unclaimed
// in_review row — lands the ordinary failed terminal byte-identically
// with a salvage source INSTALLED that would answer signals: reviewer
// rows carry no lane worktree, the pass never probes.
func (s *BoardSuite) TestPin_PersonaEscalation_Unchanged() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "reviewer-exhausted", nil, false)

	spawner := &claimCapture{}
	src := &fakeSalvageSource{signals: salvage2064()}
	svc := board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(spawner.Spawn),
		board.WithWorkerAborter(s.aborter.Abort),
		board.WithSalvageSource(src),
		board.WithRoleDirectory(personaRoleDirectory()),
	)
	s.setStagePersonas(svc, p, map[string]board.PersonaConfig{"in_review": {RoleID: "reviewer"}})
	s.recon = board.NewReconciler(s.pool, s.cfg,
		board.WithWorkerSpawner(spawner.Spawn),
		board.WithWorkerAborter(s.aborter.Abort),
		board.WithPRDriftSource(s.drift),
		board.WithPendingInputSource(s.pending),
		board.WithFailpoint(s.fps.fp),
		board.WithSalvageSource(src),
		board.WithRoleDirectory(personaRoleDirectory()),
	)

	c, err := svc.ClaimNext(ctx, "w-"+ws, ws)
	s.Require().NoError(err)
	s.Require().Equal(task.ID, c.TaskID)
	_, err = svc.MoveStage(ctx, s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)
	s.Require().Len(spawner.claims, 2, "grant + reviewer spawn #1")

	// Three reviewer deaths drive the failure ledger to the cap; the
	// persona pass's claim-time arm escalates (the recipe from
	// TestPersona_DeadWorkerLeaseLapseFreshReclaim).
	for i := 0; i < 3; i++ {
		s.expireLease(task.ID)
		s.sweep()
		s.clearTaskBackoff(task.ID)
		s.sweep()
	}
	row := s.rawTask(task.ID)
	require.Equal(s.T(), board.StageFailed, row.Stage, "the persona escalation stays byte-identical — failed terminus")
	require.NotNil(s.T(), row.PendingOrigin)
	require.Equal(s.T(), board.PendingOriginEscalated, *row.PendingOrigin)
	require.Nil(s.T(), s.salvageOnRow(task.ID), "no pointer, no probe")
	require.Equal(s.T(), 0, src.probeCount(), "the persona exhaustion NEVER consults the salvage seam")
	require.Equal(s.T(), 0, s.countEvents("salvage_depose"))
}

// probeHit reads the engine's own probe audit trail count for a task
// (the events ARE the per-task probe record: one salvage_depose per
// round; the fake's counter cross-checks the consult itself).
func (s *BoardSuite) probeHit(taskID string) int {
	var n int
	err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM board_events WHERE item_id=$1 AND action='salvage_depose'`, taskID).Scan(&n)
	s.Require().NoError(err)
	return n
}
