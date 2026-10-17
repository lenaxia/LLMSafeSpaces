// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

// Engine kill-tests (issue #1698, design 0067 §4.4 + §6 "Engine" pins).
// Postgres is the only arbiter: every race here runs real concurrent
// transactions against real rows.
package board_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// KILL-TEST 1 (§6 invariant 4 companion; engine pin "concurrent-claim
// race"): N concurrent dispatch-path claims × 1 ready task → exactly one
// winner, one spawn (ruling R9), attempts=1. Double dispatch is impossible
// by construction — this test kills any impl that checks-then-writes.
func (s *BoardSuite) TestKill_ConcurrentClaimsSingleWinner() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 8)
	e := s.seedEpic(p, board.EpicActive)
	s.seedTask(e, "only-one", nil, false)

	const racers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	var grants []*board.Claim
	var noCandidate, escalated int

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := s.svc.ClaimNext(context.Background(), "racer", ws)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				s.T().Errorf("unexpected claim error: %v", err)
			case c.Escalated:
				escalated++
			case c.Task != nil:
				grants = append(grants, c)
			default:
				noCandidate++ // a clean empty is a normal outcome
			}
		}()
	}
	wg.Wait()

	s.Equal(1, len(grants), "exactly one racer wins the single task")
	s.Equal(racers-1, noCandidate, "everyone else gets a clean empty")
	s.Equal(0, escalated)
	s.Equal(1, s.spawner.count(), "exactly one fresh-worker spawn per granted claim (R9)")
	row := s.rawTask(grants[0].TaskID)
	s.Equal(board.StageInProgress, row.Stage)
	s.Equal(1, row.Attempts, "the claim is the only attempts increment site")
	s.NotNil(row.ClaimToken)
	s.Equal(grants[0].ClaimToken, *row.ClaimToken, "row carries the winner's token")
	s.Equal(1, s.countEvents("claim"))
}

// KILL-TEST 2 (engine pin "worker_slots cap race"): a project AT capacity
// (2 in_progress = worker_slots) — N+1 concurrent claimers all get clean
// empties and the in-flight count NEVER exceeds worker_slots (the p CTE's
// row lock serializes the count read; SKIP LOCKED makes racers on a locked
// project return empty rather than block).
func (s *BoardSuite) TestKill_WorkerSlotsCapRace() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2) // 2 slots
	e := s.seedEpic(p, board.EpicActive)
	for i := 0; i < 5; i++ {
		s.seedTask(e, fmt.Sprintf("t%d", i), nil, false)
	}
	// Fill both slots first (the pin's "full project" precondition).
	c1 := s.claimOrFail(ws)
	c2 := s.claimOrFail(ws)
	s.Equal(2, s.inProgressCount(p.ID))

	const racers = 6
	var wg sync.WaitGroup
	var mu sync.Mutex
	grants := 0
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := s.svc.ClaimNext(context.Background(), "racer", ws)
			mu.Lock()
			defer mu.Unlock()
			if err == nil && c.Task != nil && !c.Escalated {
				grants++
			}
		}()
	}
	wg.Wait()

	s.Equal(0, grants, "the surplus claimers on a full project get empty")
	s.Equal(2, s.inProgressCount(p.ID), "in-flight never exceeds worker_slots")
	s.Equal(2, s.spawner.count(), "no third spawn while both slots are held")
	_ = c1
	_ = c2
}

// KILL-TEST 3 (engine pin "escalation-at-claim"): pre-increment
// attempts >= N withholds the grant — pending_input/escalated, NO token, NO
// lease, NO spawn, attempts NOT incremented (fires on claim N+1, after N
// grants). Then the worker claims the next candidate normally.
func (s *BoardSuite) TestKill_EscalationAtClaimNPlusOne() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	tired := s.seedTask(e, "tired", nil, false) // will be driven to attempts=3
	fresh := s.seedTask(e, "fresh", nil, false)

	// 3 grants, each lapsing and reclaiming (RC1 never increments).
	for i := 0; i < s.cfg.AttemptsCap; i++ {
		c := s.claimOrFail(ws)
		s.Equal(tired.ID, c.TaskID, "priority ties break by created_at (tired first)")
		s.Equal(i+1, s.rawTask(tired.ID).Attempts)
		s.expireLease(tired.ID)
		res := s.sweep()
		s.Equal(1, res.Reclaims)
	}
	s.Equal(3, s.rawTask(tired.ID).Attempts)
	spawnsBeforeEscalation := s.spawner.count()

	// Claim N+1: the grant is withheld; escalation is returned to the caller.
	c, err := s.svc.ClaimNext(context.Background(), "w", ws)
	s.Require().NoError(err)
	s.Require().NotNil(c.Task)
	s.True(c.Escalated, "claim N+1 escalates instead of granting")
	s.Equal(tired.ID, c.TaskID)
	s.Empty(c.ClaimToken, "escalation mints nothing (grant withheld)")

	row := s.rawTask(tired.ID)
	s.Equal(board.StagePendingInput, row.Stage)
	s.Require().NotNil(row.PendingOrigin)
	s.Equal(board.PendingOriginEscalated, *row.PendingOrigin)
	s.Nil(row.ClaimToken)
	s.Nil(row.LeaseExp)
	s.Nil(row.Assignee)
	s.Equal(3, row.Attempts, "escalation is NOT an increment")
	s.Equal(spawnsBeforeEscalation, s.spawner.count(), "no worker spawned for a withheld grant")
	s.Equal(1, s.countEvents("escalate"))

	// The worker claims again → the next candidate grants normally.
	c2 := s.claimOrFail(ws)
	s.Equal(fresh.ID, c2.TaskID)
	s.Equal(1, s.rawTask(fresh.ID).Attempts)
	s.Equal(spawnsBeforeEscalation+1, s.spawner.count(), "the withheld grant spawned nothing; the fresh grant spawned one")
	// The escalated row is no longer claimable (stage predicate).
	c3, err := s.svc.ClaimNext(context.Background(), "w", ws)
	s.Require().NoError(err)
	s.Nil(c3.Task, "pending_input rows never enter the candidate set")
}

// KILL-TEST 4 (engine pin "caller-workspace binding"): workspace A's worker
// cannot claim project B targeting workspace B — structurally (the p CTE
// predicate), 403 on task_start's explicit form.
func (s *BoardSuite) TestKill_WorkspaceBinding() {
	wsA := s.seedWorkspace()
	wsB := s.seedWorkspace()
	pB := s.seedProject(wsB, 2)
	eB := s.seedEpic(pB, board.EpicActive)
	task := s.seedTask(eB, "b-only", nil, false)

	c, err := s.svc.ClaimNext(context.Background(), "worker-in-a", wsA)
	s.Require().NoError(err)
	s.Nil(c.Task, "no candidate in A's scope: B's project is invisible, not an error")
	s.Equal(0, s.spawner.count())

	_, err = s.svc.TaskStart(context.Background(), s.owner(), task.ID, "", wsA, "")
	s.Require().Error(err)
	var ge *board.GuardError
	s.Require().ErrorAs(err, &ge)
	s.Equal(board.GuardWorkspaceBinding, ge.Code, "task_start guards the same binding (403)")

	// The B-side claim works, and the granted token binds the identity.
	c2 := s.claimOrFail(wsB)
	s.Equal(task.ID, c2.TaskID)
}

// KILL-TEST 5 (engine pin "throttle requeue rolls back that claim's
// increment"): claim-class token (flag=true) + 429 → net attempts unchanged,
// both cooldowns set, assignee retired, next claim blocked until cooldown
// clears.
func (s *BoardSuite) TestKill_ThrottleRollbackIncrementingGrant() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "throttled", nil, false)

	c := s.claimOrFail(ws)
	s.Equal(1, s.rawTask(task.ID).Attempts)

	_, err := s.svc.ReportThrottle(context.Background(), s.agent("sess", c.ClaimToken), task.ID, "", time.Minute, "")
	s.Require().NoError(err)

	row := s.rawTask(task.ID)
	s.Equal(board.StageBacklog, row.Stage, "429 requeues without attempt penalty")
	s.Equal(0, row.Attempts, "net attempts unchanged — the increment rolled back (flag was true)")
	s.Nil(row.Assignee, "worker identity retires with the claim")
	s.Nil(row.ClaimToken)
	s.False(row.ClaimIncr)
	var taskCooled bool
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT cooldown_until IS NOT NULL AND cooldown_until > now() FROM tasks WHERE id=$1`, task.ID).Scan(&taskCooled))
	s.True(taskCooled, "task cooldown_until is set in the future")

	var slotCool bool
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT slot_cooldown_until > now() FROM projects WHERE id=$1`, p.ID).Scan(&slotCool))
	s.True(slotCool, "project slot cooldown pauses NEW claims into the project")

	// New claims into the project are paused while the slot cooldown runs.
	c2, err := s.svc.ClaimNext(context.Background(), "w", ws)
	s.Require().NoError(err)
	s.Nil(c2.Task, "cooldown excludes the task AND the project from the candidate set")

	// Cooldown clears → claimable again, and the grant re-increments.
	s.clearCooldowns(p, task.ID)
	c3 := s.claimOrFail(ws)
	s.Equal(task.ID, c3.TaskID)
	s.Equal(1, s.rawTask(task.ID).Attempts)
}

// KILL-TEST 6 (engine pin "bounce-throttle fixture"): re-claim-class token
// (flag=false — the review bounce) + 429 → requeue + cooldown with attempts
// UNCHANGED — never undercounted, never negative.
func (s *BoardSuite) TestKill_ThrottleBounceTokenNoDecrement() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "bounce-throttle", nil, false)

	// Grant 1 (incrementing), advance to in_review, bounce (re-claim, flag=false).
	c1 := s.claimOrFail(ws)
	_, err := s.svc.MoveStage(context.Background(), s.agent("sess", c1.ClaimToken), task.ID, "", board.StageInReview, "", "")
	s.Require().NoError(err)
	s.Equal(1, s.rawTask(task.ID).Attempts)

	c2, err := s.svc.MoveStage(context.Background(), s.owner(), task.ID, "", board.StageInProgress, "", "")
	s.Require().NoError(err)
	row := s.rawTask(task.ID)
	s.Equal(1, row.Attempts, "the bounce mints without incrementing")
	s.False(row.ClaimIncr, "re-claim-class token carries flag=false")
	bounceToken := s.reloadTask(task.ID).ClaimToken
	s.NotEmpty(bounceToken)
	_ = c2

	// 429 on the bounce token: no decrement (would undercount grant 1).
	_, err = s.svc.ReportThrottle(context.Background(), s.agent("sess", bounceToken), task.ID, "", time.Minute, "")
	s.Require().NoError(err)
	row = s.rawTask(task.ID)
	s.Equal(1, row.Attempts, "attempts UNCHANGED — the refused re-claim was never counted")
	s.Equal(board.StageBacklog, row.Stage)
	s.False(row.ClaimIncr)
}

// KILL-TEST 7 (engine pin "throttle guard"): a pending_input row's
// still-held token CANNOT report_throttle (a worker cannot self-exit
// pending_input through the 429 path).
func (s *BoardSuite) TestKill_ThrottlePendingInputTokenRejected() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "pending", nil, false)

	c := s.claimOrFail(ws)
	// Worker-origin pending_input: the token is retained (the worker still
	// holds the work; the lease clock is suspended by the stage predicate).
	_, err := s.svc.MoveStage(context.Background(), s.agent("sess", c.ClaimToken), task.ID, "", board.StagePendingInput, "", "")
	s.Require().NoError(err)
	row := s.rawTask(task.ID)
	s.NotNil(row.ClaimToken, "pending_input keeps its token")
	s.Equal(c.ClaimToken, *row.ClaimToken)

	_, err = s.svc.ReportThrottle(context.Background(), s.agent("sess", c.ClaimToken), task.ID, "", time.Minute, "")
	s.Require().Error(err)
	var ge *board.GuardError
	s.Require().ErrorAs(err, &ge)
	s.Equal(board.GuardClaimToken, ge.Code, "stage guard rejects: a throttle concerns a live claim")

	row2 := s.rawTask(task.ID)
	s.Equal(board.StagePendingInput, row2.Stage, "no self-exit")
	s.Equal(1, row2.Attempts)
	s.Equal(c.ClaimToken, *row2.ClaimToken, "token untouched")
}

// KILL-TEST 8 (engine pin "heartbeat evidence gate", #1685): a heartbeat
// with a moved watermark extends the lease; a silent heartbeat does not.
// Elapsed time is not liveness.
func (s *BoardSuite) TestKill_HeartbeatEvidenceGate() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "beat", nil, false)

	c := s.claimOrFail(ws)
	before := s.rawTask(task.ID).LeaseExp

	// Silent heartbeat: watermark did not move → no extension.
	res, err := s.svc.Heartbeat(context.Background(), s.agent("sess", c.ClaimToken), task.ID, "",
		time.Now().Add(-time.Hour))
	s.Require().NoError(err)
	s.False(res.Extended, "a heartbeat with no progress evidence does not extend")
	silent := s.rawTask(task.ID).LeaseExp
	s.Require().NotNil(silent)
	s.WithinDuration(*before, *silent, 50*time.Millisecond)

	// Evidence-bearing heartbeat: watermark moved → lease re-armed.
	observed := time.Now().Add(time.Second)
	res, err = s.svc.Heartbeat(context.Background(), s.agent("sess", c.ClaimToken), task.ID, "", observed)
	s.Require().NoError(err)
	s.True(res.Extended)
	after := s.rawTask(task.ID).LeaseExp
	s.Require().NotNil(after)
	s.True(after.After(*before), "lease re-armed past the pre-heartbeat expiry")
	s.Equal(1, s.countEvents("heartbeat"))

	// Wrong token: the in-flight guard fails before anything moves.
	_, err = s.svc.Heartbeat(context.Background(), s.agent("sess", "00000000-0000-0000-0000-000000000000"), task.ID, "", time.Now())
	s.Require().Error(err)
	var ge *board.GuardError
	s.Require().ErrorAs(err, &ge)
	s.Equal(board.GuardClaimToken, ge.Code)
}

// KILL-TEST 9 (engine pin "lease lifecycle totality"): every write landing
// a row in in_progress sets/re-arms the lease in the same statement; every
// forward exit clears token+lease; the RC1 NULL-belt reclaims a NULL lease.
func (s *BoardSuite) TestKill_LeaseLifecycleTotality() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "life", nil, false)

	// claim: lease set.
	c := s.claimOrFail(ws)
	s.NotNil(s.rawTask(task.ID).LeaseExp)

	// forward exit in_review: token+lease cleared, identity retained.
	_, err := s.svc.MoveStage(context.Background(), s.agent("sess", c.ClaimToken), task.ID, "", board.StageInReview, "", "")
	s.Require().NoError(err)
	row := s.rawTask(task.ID)
	s.Nil(row.ClaimToken)
	s.Nil(row.LeaseExp)
	s.NotNil(row.Assignee, "in_review retains the identity (the bounce/RC2 consumer)")

	// re-claim-class write (bounce): lease re-armed, flag=false.
	_, err = s.svc.MoveStage(context.Background(), s.owner(), task.ID, "", board.StageInProgress, "", "")
	s.Require().NoError(err)
	row = s.rawTask(task.ID)
	s.NotNil(row.ClaimToken)
	s.NotNil(row.LeaseExp, "the bounce re-arms the lease in the same statement")
	s.False(row.ClaimIncr)

	// forward exit done: token+lease cleared.
	markCriteria := s.reloadTask(task.ID)
	markCriteria.AcceptanceCriteria = []board.AcceptanceCriterion{{Text: "c1", Met: true}, {Text: "c2", Met: true}}
	updated, err := s.svc.UpdateTask(context.Background(), s.owner(), task.ID, "",
		board.TaskInput{Title: markCriteria.Title, AcceptanceCriteria: markCriteria.AcceptanceCriteria}, "")
	s.Require().NoError(err)
	_ = updated
	// The bounced worker still holds the work: its (fresh) token drives
	// the forward exit. A manager drag here would snap back (in-flight).
	bounceToken := s.reloadTask(task.ID).ClaimToken
	_, err = s.svc.MoveStage(context.Background(), s.agent("sess", bounceToken), task.ID, "", board.StageDone, "https://evidence.example/x", "")
	s.Require().NoError(err)
	row = s.rawTask(task.ID)
	s.Equal(board.StageDone, row.Stage)
	s.Nil(row.ClaimToken)
	s.Nil(row.LeaseExp)

	// RC1 NULL-belt: a corrupted in_progress row (NULL lease) is still
	// reclaimed — never permanently unreclaimable. (Fresh epic: the main
	// flow's lone-task done completed the first epic — RC4.)
	beltEpic := s.seedEpic(p, board.EpicActive)
	belt := s.seedTask(beltEpic, "belt", nil, false)
	c2 := s.claimOrFail(ws)
	_ = c2
	_, err = s.pool.Exec(context.Background(),
		`UPDATE tasks SET lease_expires_at=NULL WHERE id=$1`, belt.ID)
	s.Require().NoError(err)
	res := s.sweep()
	s.Equal(1, res.Reclaims)
	s.Equal(board.StageBacklog, s.rawTask(belt.ID).Stage)
}

// KILL-TEST 10 (ruling R9 pin): every granted claim spawns exactly one
// fresh-charter session; a spawn FAILURE never revokes the claim — the
// lease-lapse path heals it (self-healing by convergence).
func (s *BoardSuite) TestKill_SpawnFailureHealsByLeaseLapse() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "spawnless", nil, false)

	s.spawner.failWith(errors.New("spawn backend down"))
	s.claimOrFail(ws)
	s.Equal(1, s.spawner.count(), "the spawn was attempted exactly once")
	row := s.rawTask(task.ID)
	s.Equal(board.StageInProgress, row.Stage, "the claim stands: spawn failure is not a claim failure")
	s.NotNil(row.ClaimToken)

	// The claim lapses; the reconciler reclaims; the next claim re-spawns.
	s.expireLease(task.ID)
	res := s.sweep()
	s.Equal(1, res.Reclaims)
	s.spawner.failWith(nil)
	c2 := s.claimOrFail(ws)
	s.Equal(task.ID, c2.TaskID)
	s.Equal(2, s.spawner.count())
}

// KILL-TEST 11 (§6 invariant 1, arm 1 — kill worker mid-lease): claim →
// kill the worker (abort recorded by the claim-then-spawn lifecycle) → stop
// heartbeats → advance past lease_expires_at → sweep → reclaimed to backlog
// with attempts UNCHANGED; re-claim increments; old token now fails.
func (s *BoardSuite) TestKill_WorkerDiedMidLeaseTaskReclaims() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "orphan", nil, false)

	c := s.claimOrFail(ws)
	oldToken := c.ClaimToken
	s.Equal(1, s.rawTask(task.ID).Attempts)

	// Worker dies silently: no more heartbeats, lease runs out.
	s.expireLease(task.ID)
	res := s.sweep()
	s.Equal(1, res.Reclaims)

	row := s.rawTask(task.ID)
	s.Equal(board.StageBacklog, row.Stage)
	s.Nil(row.ClaimToken)
	s.Nil(row.Assignee)
	s.Equal(1, row.Attempts, "reclaim NEVER increments (the increment belongs to the next claim)")
	s.Equal(1, s.countTransitions(task.ID, board.StageBacklog))

	// The next claim increments and grants.
	c2 := s.claimOrFail(ws)
	s.Equal(task.ID, c2.TaskID)
	s.Equal(2, s.rawTask(task.ID).Attempts)

	// The old token is dead: in-flight writes fail the guard even though
	// the row is claimed again (by another worker).
	_, err := s.svc.MoveStage(context.Background(), s.agent("dead", oldToken), task.ID, "", board.StageInReview, "", "")
	s.Require().Error(err)
	var ge *board.GuardError
	s.Require().ErrorAs(err, &ge)
	s.Equal(board.GuardClaimToken, ge.Code, "a stale worker fails the guard before authz")
}
