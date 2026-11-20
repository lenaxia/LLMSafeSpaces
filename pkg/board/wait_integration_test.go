// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

// wait_integration_test.go — the typed-wait verb's board-level pins
// (claim-wait lifecycle, low-level design §1-§3; migration 000060):
//
//   - USER waits are today's flag-op, byte-identically (row shape +
//     the pending_input_flag event; the ladder freezes), plus dress.
//   - EVENT waits park the claim: pending_input stays FALSE, the row
//     stays in_progress with token+identity, the condition rides the
//     wait_declared event, and the row is visible on the GetTask wire
//     and board_health's waiting section.
//   - The verb matrix's refusals: no token, wrong stage, bad grammar,
//     user-with-condition, event-without-reason, placeholder reason,
//     over-cap reason, and the ONE-active-event-wait cap.
//   - Advances clear: forward stage moves and the claim mints both
//     grow wait_on=NULL, wait_reason=NULL (the episode discipline).
//   - The LADDER BOUNDARY, pinned both ways (§3, not re-derived): an
//     event-wait is a FULL ladder citizen (lapsed lease => nudged);
//     a user-wait freezes the ladder (never a victim).

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// waitState reads the row's stored wait columns directly (raw truth,
// not the scanTask read edge — the read edge has its own pin below).
func (s *BoardSuite) waitState(taskID string) (waitOn, waitReason string) {
	var won, wr *string
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT wait_on, wait_reason FROM tasks WHERE id=$1`, taskID).Scan(&won, &wr))
	if won != nil {
		waitOn = *won
	}
	if wr != nil {
		waitReason = *wr
	}
	return waitOn, waitReason
}

// testRepo is the declaration-time repo identity the suite's
// pr-anchored conditions carry (the broker compiles its sensor from
// the payload's repo — never guessing).
const testRepo = "lenaxia/llmsafespaces"

// declareEvent is the verb's happy path: a well-formed event-wait
// declaration on the claimant's own row (GitHub-anchored conditions
// carry the test repo identity).
func (s *BoardSuite) declareEvent(t *testing.T, c *board.Claim, reason, condition string) *board.Task {
	repo := ""
	if cond, err := board.ParseWaitCondition(condition); err == nil && board.ConditionNeedsRepo(cond.Primitive) {
		repo = testRepo
	}
	out, err := s.svc.DeclareWait(context.Background(), s.agent(c.Assignee, c.ClaimToken),
		"", c.ShortID, board.WaitOnEvent, reason, condition, repo, "")
	require.NoError(t, err)
	return out
}

// --- USER waits: byte-identical flag-op, plus dress -----------------------

func (s *BoardSuite) TestDeclareWait_User_ByteIdenticalFlagOp() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "verb-user", nil, false)
	t2 := s.seedTask(e, "alias-user", nil, false)

	c1 := s.claimOrFail(ws)
	s.Require().Equal(t1.ID, c1.TaskID)
	// The VERB.
	verbOut, err := s.svc.DeclareWait(context.Background(), s.agent(c1.Assignee, c1.ClaimToken),
		"", c1.ShortID, board.WaitOnUser, "owner decision: runtime bump scope", "", "", "")
	s.Require().NoError(err)
	require.Equal(s.T(), board.StageInProgress, verbOut.Stage, "000050: the flag rides the real stage")

	// The ALIAS (move_stage toStage=pending_input) on a second row.
	c2 := s.claimOrFail(ws)
	s.Require().Equal(t2.ID, c2.TaskID)
	_, err = s.svc.MoveStage(context.Background(), s.agent(c2.Assignee, c2.ClaimToken),
		"", c2.ShortID, board.StagePendingInput, "", "")
	s.Require().NoError(err)

	for _, tc := range []struct {
		name string
		id   string
	}{
		{"verb", t1.ID},
		{"alias", t2.ID},
	} {
		row := s.rawTask(tc.id)
		s.True(row.PendingInput, "%s: pending_input=true (today's flag-op)", tc.name)
		require.NotNil(s.T(), row.PendingOrigin)
		s.Equal("worker", *row.PendingOrigin, "%s: pending_origin='worker'", tc.name)
		won, _ := s.waitState(tc.id)
		s.Equal(board.WaitOnUser, won, "%s: typed dress wait_on='user'", tc.name)
		// The flag-op's own audit event, per row (the deterministic
		// engine:flag:<task>:<attempts> id — the verb's write is
		// byte-identical to the alias's).
		var flagEvents int
		s.Require().NoError(s.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM board_events WHERE action='pending_input_flag' AND item_id=$1`, tc.id).Scan(&flagEvents))
		s.Equal(1, flagEvents, "%s: exactly the flag-op's own audit event", tc.name)
	}

	// The verb's stated reason lands; the alias leaves it NULL.
	_, verbReason := s.waitState(t1.ID)
	s.Equal("owner decision: runtime bump scope", verbReason)
	_, aliasReason := s.waitState(t2.ID)
	s.Equal("", aliasReason)

	// Byte-identical semantics: the token is retained, the ladder
	// frozen (the pin below), and the exit is the manager's.
	c1After := s.reloadTask(t1.ID)
	require.NotEmpty(s.T(), c1After.ClaimToken, "the flag keeps its token (clock suspended, not ended)")
	s.Equal(c1.ClaimToken, c1After.ClaimToken)
}

// --- EVENT waits: the new state -------------------------------------------

func (s *BoardSuite) TestDeclareWait_Event_ParksClaim_VisibleEverywhere() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "event-wait", nil, false)

	c := s.claimOrFail(ws)
	s.Require().Equal(t1.ID, c.TaskID)
	out := s.declareEvent(s.T(), c, "PR 2083 merge", "pr_merged(2083)")

	// The row: in_progress, flag FALSE, wait typed, claim intact.
	require.Equal(s.T(), board.StageInProgress, out.Stage)
	row := s.rawTask(t1.ID)
	s.False(row.PendingInput, "an event-wait is NOT a pending_input")
	s.Nil(row.PendingOrigin)
	won, wr := s.waitState(t1.ID)
	s.Equal(board.WaitOnEvent, won)
	s.Equal("PR 2083 merge", wr)
	require.NotNil(s.T(), row.ClaimToken)
	s.Equal(c.ClaimToken, *row.ClaimToken, "the claim persists — the worker anchored it")
	require.NotNil(s.T(), row.Assignee)

	// The wire (GetTask): camelCase fields + the composed prose.
	wire, err := s.svc.GetTask(context.Background(), "", t1.ID)
	s.Require().NoError(err)
	s.Equal(board.WaitOnEvent, wire.WaitOn)
	s.Equal("PR 2083 merge", wire.WaitReason)
	s.Equal("PR 2083 merge", wire.WaitingOnTyped)
	s.False(wire.PendingInput)

	// The audit trail: the wait_declared event carries the CONDITION
	// (data, never code) and the deterministic wait: prefix.
	s.Equal(1, s.countEvents("wait_declared"))
	var condText string
	s.Require().NoError(s.pool.QueryRow(context.Background(), `
		SELECT payload->'condition'->>'text' FROM board_events
		WHERE action='wait_declared' AND item_id=$1`, t1.ID).Scan(&condText))
	s.Equal("pr_merged(2083)", condText)
	var evtID string
	s.Require().NoError(s.pool.QueryRow(context.Background(), `
		SELECT id FROM board_events WHERE action='wait_declared' AND item_id=$1`, t1.ID).Scan(&evtID))
	require.Contains(s.T(), evtID, "wait:"+t1.ID+":"+c.ClaimToken+":",
		"the deterministic id shape wait:<taskID>:<claimToken>:<nanos>")

	// board_health's waiting section: the facts beside the leases.
	h, err := s.svc.BoardHealth(context.Background(), testUser)
	s.Require().NoError(err)
	require.Len(s.T(), h.Waiting, 1)
	s.Equal(t1.ShortID, h.Waiting[0].ShortID)
	s.Equal(board.WaitOnEvent, h.Waiting[0].WaitOn)
	s.Equal("PR 2083 merge", h.Waiting[0].WaitReason)
	s.False(h.Waiting[0].Since.IsZero(), "since = the declaration event's timestamp")
}

func (s *BoardSuite) TestDeclareWait_UserWait_HealthSectionCoalesceRead() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "preverb-row", nil, false)

	c := s.claimOrFail(ws)
	s.Require().Equal(t1.ID, c.TaskID)
	// Simulate a PRE-MIGGRATION flagged row: flag set, wait_on NULL
	// (exactly what 000059-era rows look like after 000060 lands —
	// no backfill).
	_, err := s.pool.Exec(context.Background(),
		`UPDATE tasks SET pending_input=true, pending_origin='worker', wait_on=NULL, wait_reason=NULL WHERE id=$1`, t1.ID)
	s.Require().NoError(err)

	// The read rule: waitOn renders 'user' by COALESCE.
	wire, err := s.svc.GetTask(context.Background(), "", t1.ID)
	s.Require().NoError(err)
	s.Equal(board.WaitOnUser, wire.WaitOn)
	s.Equal("user", wire.WaitingOnTyped)
	s.True(wire.PendingInput)

	h, err := s.svc.BoardHealth(context.Background(), testUser)
	s.Require().NoError(err)
	require.Len(s.T(), h.Waiting, 1)
	s.Equal(board.WaitOnUser, h.Waiting[0].WaitOn)
}

// TestDeclareWait_WorkspaceHealth_WaitingSection pins the WORKSPACE
// health surface's waiting section (round-1 review finding: the
// nil-vs-empty wire split) — the same projection BoardHealth serves,
// workspace-scoped. Both invariants: [] NOT null when nothing waits,
// and both wait types render with their facts.
func (s *BoardSuite) TestDeclareWait_WorkspaceHealth_WaitingSection() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "ws-waiting", nil, false)

	// BEFORE any wait: the section is NON-NIL EMPTY — "waiting": [] on
	// the wire, never null (the split the owner surface never had).
	h0, err := s.svc.BoardHealthInWorkspace(ctx, ws)
	s.Require().NoError(err)
	require.NotNil(s.T(), h0.Waiting, "the workspace surface emits [] — never null — when nothing waits")
	require.Empty(s.T(), h0.Waiting)

	c := s.claimOrFail(ws)
	s.Require().Equal(t1.ID, c.TaskID)
	s.declareEvent(s.T(), c, "PR 2083 merge", "pr_merged(2083)")

	t2 := s.seedTask(e, "ws-user-wait", nil, false)
	c2 := s.claimOrFail(ws)
	s.Require().Equal(t2.ID, c2.TaskID)
	_, err = s.svc.DeclareWait(ctx, s.agent(c2.Assignee, c2.ClaimToken), "", c2.ShortID,
		board.WaitOnUser, "owner decision: runtime bump scope", "", "", "")
	s.Require().NoError(err)

	h, err := s.svc.BoardHealthInWorkspace(ctx, ws)
	s.Require().NoError(err)
	require.Len(s.T(), h.Waiting, 2)
	byShort := map[string]board.HealthWait{}
	for _, w := range h.Waiting {
		byShort[w.ShortID] = w
	}
	ew, ok := byShort[t1.ShortID]
	s.Require().True(ok, "the event-wait renders on the workspace surface")
	s.Equal(board.WaitOnEvent, ew.WaitOn)
	s.Equal("PR 2083 merge", ew.WaitReason)
	s.False(ew.Since.IsZero(), "since rides the wait_declared event's timestamp")
	uw, ok := byShort[t2.ShortID]
	s.Require().True(ok, "the user-wait renders on the workspace surface")
	s.Equal(board.WaitOnUser, uw.WaitOn)
	s.Equal("owner decision: runtime bump scope", uw.WaitReason)

	// Workspace scope: a sibling workspace sees its own empty section —
	// non-nil, empty, and no cross-workspace wait disclosure.
	other := s.seedWorkspace()
	h2, err := s.svc.BoardHealthInWorkspace(ctx, other)
	s.Require().NoError(err)
	require.NotNil(s.T(), h2.Waiting)
	require.Empty(s.T(), h2.Waiting, "no cross-workspace wait disclosure")
}

// --- The verb matrix's refusals -------------------------------------------

func (s *BoardSuite) TestDeclareWait_RefusalMatrix() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "refusals", nil, false)

	c := s.claimOrFail(ws)
	s.Require().Equal(t1.ID, c.TaskID)
	act := s.agent(c.Assignee, c.ClaimToken)

	guard := func(fn func() error) *board.GuardError {
		err := fn()
		require.Error(s.T(), err)
		var g *board.GuardError
		require.ErrorAs(s.T(), err, &g)
		return g
	}

	// No token.
	g := guard(func() error {
		_, err := s.svc.DeclareWait(context.Background(), s.agent(c.Assignee, ""),
			"", c.ShortID, board.WaitOnEvent, "PR 2083 merge", "pr_merged(2083)", testRepo, "")
		return err
	})
	s.Equal(board.GuardClaimToken, g.Code)

	// Wrong token.
	g = guard(func() error {
		_, err := s.svc.DeclareWait(context.Background(), s.agent(c.Assignee, "00000000-0000-0000-0000-000000000000"),
			"", c.ShortID, board.WaitOnEvent, "PR 2083 merge", "pr_merged(2083)", testRepo, "")
		return err
	})
	s.Equal(board.GuardClaimToken, g.Code)

	// Bad grammar (the closed vocabulary, refused loudly).
	g = guard(func() error {
		_, err := s.svc.DeclareWait(context.Background(), act, "", c.ShortID, board.WaitOnEvent, "waiting", "deploy_finished(9)", "", "")
		return err
	})
	s.Equal(board.GuardInput, g.Code)
	require.Contains(s.T(), g.Detail, "pr_merged")

	// User-with-condition: nothing executable crosses the surface.
	g = guard(func() error {
		_, err := s.svc.DeclareWait(context.Background(), act, "", c.ShortID, board.WaitOnUser, "owner call", "pr_merged(2083)", "", "")
		return err
	})
	s.Equal(board.GuardInput, g.Code)

	// Event-without-reason.
	g = guard(func() error {
		_, err := s.svc.DeclareWait(context.Background(), act, "", c.ShortID, board.WaitOnEvent, "", "pr_merged(2083)", testRepo, "")
		return err
	})
	s.Equal(board.GuardInput, g.Code)

	// Placeholder reason (the #1820 emission-corruption class).
	g = guard(func() error {
		_, err := s.svc.DeclareWait(context.Background(), act, "", c.ShortID, board.WaitOnEvent, "TODO", "pr_merged(2083)", testRepo, "")
		return err
	})
	s.Equal(board.GuardTextRejected, g.Code)

	// Over-cap reason.
	g = guard(func() error {
		_, err := s.svc.DeclareWait(context.Background(), act, "", c.ShortID, board.WaitOnEvent,
			string(make([]byte, board.MaxWaitReasonBytes+1)), "pr_merged(2083)", testRepo, "")
		return err
	})
	s.Equal(board.GuardInput, g.Code)

	// Unknown waitOn vocabulary.
	g = guard(func() error {
		_, err := s.svc.DeclareWait(context.Background(), act, "", c.ShortID, "divine_intervention", "reason", "", "", "")
		return err
	})
	s.Equal(board.GuardInput, g.Code)

	// Nothing landed: the row never waited through the refusals.
	won, _ := s.waitState(t1.ID)
	s.Equal("", won)
	s.Equal(0, s.countEvents("wait_declared"))
	s.False(s.rawTask(t1.ID).PendingInput)

	// Wrong stage: the verb enters from LIVE WORK only — an unclaimed
	// backlog row refuses on the stage guard (the design's "stage
	// machine: only in_progress").
	t2 := s.seedTask(e, "parked", nil, false)
	g = guard(func() error {
		_, err := s.svc.DeclareWait(context.Background(), s.owner(), "", t2.ShortID, board.WaitOnUser, "why", "", "", "")
		return err
	})
	s.Equal(board.GuardStage, g.Code)
}

func (s *BoardSuite) TestDeclareWait_SecondEventWaitRefuses() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "one-cap", nil, false)

	c := s.claimOrFail(ws)
	s.Require().Equal(t1.ID, c.TaskID)
	s.declareEvent(s.T(), c, "PR 2083 merge", "pr_merged(2083)")

	_, err := s.svc.DeclareWait(context.Background(), s.agent(c.Assignee, c.ClaimToken),
		"", c.ShortID, board.WaitOnEvent, "CI on the same PR", "ci_green(2083)", testRepo, "")
	require.Error(s.T(), err)
	var g *board.GuardError
	require.ErrorAs(s.T(), err, &g)
	s.Equal(board.GuardWaitDeclared, g.Code)
	s.Equal(1, s.countEvents("wait_declared"), "no second declaration landed")
}

// --- Conversions -----------------------------------------------------------

func (s *BoardSuite) TestDeclareWait_Conversions() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "to-event", nil, false)
	t2 := s.seedTask(e, "to-user", nil, false)

	// user -> event: the flag clears, wait_on='event', reason replaced.
	c1 := s.claimOrFail(ws)
	s.Require().Equal(t1.ID, c1.TaskID)
	_, err := s.svc.DeclareWait(context.Background(), s.agent(c1.Assignee, c1.ClaimToken),
		"", c1.ShortID, board.WaitOnUser, "owner call", "", "", "")
	s.Require().NoError(err)
	s.True(s.rawTask(t1.ID).PendingInput)
	// Clear the harness's pending-source answer so the coupling pass
	// leaves the row alone (the conversion under test is the VERB's).
	out := s.declareEvent(s.T(), c1, "PR 2090 merge", "pr_merged(2090)")
	require.Equal(s.T(), board.StageInProgress, out.Stage)
	row := s.rawTask(t1.ID)
	s.False(row.PendingInput, "the user-wait converted: the flag cleared")
	s.Nil(row.PendingOrigin)
	won, wr := s.waitState(t1.ID)
	s.Equal(board.WaitOnEvent, won)
	s.Equal("PR 2090 merge", wr)
	s.Equal(1, s.countEvents("wait_cleared"), "the conversion audits wait_cleared")

	// event -> user: the flag wins, wait_on='user'.
	c2 := s.claimOrFail(ws)
	s.Require().Equal(t2.ID, c2.TaskID)
	s.declareEvent(s.T(), c2, "PR 2083 merge", "pr_merged(2083)")
	_, err = s.svc.DeclareWait(context.Background(), s.agent(c2.Assignee, c2.ClaimToken),
		"", c2.ShortID, board.WaitOnUser, "owner decision after all", "", "", "")
	s.Require().NoError(err)
	row2 := s.rawTask(t2.ID)
	s.True(row2.PendingInput)
	require.NotNil(s.T(), row2.PendingOrigin)
	s.Equal("worker", *row2.PendingOrigin)
	won2, wr2 := s.waitState(t2.ID)
	s.Equal(board.WaitOnUser, won2)
	s.Equal("owner decision after all", wr2)
}

// --- Advances clear --------------------------------------------------------

func (s *BoardSuite) TestForwardMoveClearsWait() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "advance", nil, true) // criteria met: done-able

	c := s.claimOrFail(ws)
	s.Require().Equal(t1.ID, c.TaskID)
	s.declareEvent(s.T(), c, "PR 2083 merge", "pr_merged(2083)")

	// A worker-anchored forward exit clears the wait.
	_, err := s.svc.MoveStage(context.Background(), s.agent(c.Assignee, c.ClaimToken),
		"", c.ShortID, board.StageInReview, "https://example.com/pr/2083", "")
	s.Require().NoError(err)
	won, wr := s.waitState(t1.ID)
	s.Equal("", won, "the forward move cleared the wait")
	s.Equal("", wr)
	s.False(s.rawTask(t1.ID).PendingInput)

	// The done exit clears too (from the reviewer side: owner drag).
	_, err = s.svc.MoveStage(context.Background(), s.owner(), "", c.ShortID,
		board.StageDone, "https://example.com/merged", "")
	s.Require().NoError(err)
	won, _ = s.waitState(t1.ID)
	s.Equal("", won)
}

func (s *BoardSuite) TestMintClearsWait() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "mint-taskstart", nil, false)
	t2 := s.seedTask(e, "mint-claimnext", nil, false)

	// TaskStart's mint (engine.go task_start path): a backlog row that
	// somehow still carries a wait (defense-in-depth — the exits
	// clear, so this is only reachable by drift) has it cleared by the
	// mint's slot-reset.
	_, err := s.pool.Exec(context.Background(),
		`UPDATE tasks SET wait_on='event', wait_reason='stale' WHERE id=$1`, t1.ID)
	s.Require().NoError(err)
	_, err = s.svc.TaskStart(context.Background(), s.owner(), "", t1.ShortID, "")
	s.Require().NoError(err)
	won, wr := s.waitState(t1.ID)
	s.Equal("", won, "the task_start mint cleared the wait")
	s.Equal("", wr)

	// ClaimNext's mint (engine.go claim path), same posture.
	_, err = s.pool.Exec(context.Background(),
		`UPDATE tasks SET stage='backlog', claim_token=NULL, lease_expires_at=NULL, wait_on='event', wait_reason='stale' WHERE id=$1`, t2.ID)
	s.Require().NoError(err)
	s.Require().NoError(s.pool.Ping(context.Background()))
	c, err := s.svc.ClaimNext(context.Background(), "worker-"+ws, ws)
	s.Require().NoError(err)
	if c.TaskID == t2.ID {
		won, wr := s.waitState(t2.ID)
		s.Equal("", won, "the claim mint cleared the wait")
		s.Equal("", wr)
	} else {
		// The DRR round served another candidate first — the mint pin
		// holds per-row; force this row's readiness and retry.
		for i := 0; i < 4 && c.TaskID != t2.ID; i++ {
			c, err = s.svc.ClaimNext(context.Background(), "worker-"+ws, ws)
			s.Require().NoError(err)
		}
		s.Require().Equal(t2.ID, c.TaskID, "the seeded row must come up for the mint pin to run")
		won, wr := s.waitState(t2.ID)
		s.Equal("", won)
		s.Equal("", wr)
	}
}

// --- The ladder boundary, pinned both ways (§3 — NOT re-derived) ----------

func (s *BoardSuite) TestPin_EventWait_StaysLadderCitizen() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "ladder-citizen", nil, false)

	c := s.claimOrFail(ws)
	s.Require().Equal(t1.ID, c.TaskID)
	assignee := s.reloadTask(t1.ID).AssigneeSession
	s.Require().NotEmpty(assignee)
	s.declareEvent(s.T(), c, "PR 2083 merge", "pr_merged(2083)")

	// The event-waiting row's lease lapses; the assignee is quiet.
	lives := &fakeLiveness{}
	lives.set(assignee, liveQuiet())
	var nudged int
	recon := s.pinRecon(lives, func(context.Context, string, board.NudgePayload) error {
		nudged++
		return nil
	})
	s.expireLease(t1.ID)
	s.rc1Sweep(recon)

	s.Equal(1, nudged, "an event-wait with a lapsed lease is NUDGED — a full ladder citizen")
	nid, _ := s.nudgeState(t1.ID)
	s.NotEmpty(nid, "the nudge slot is armed on the row")
	row := s.rawTask(t1.ID)
	s.Equal(board.StageInProgress, row.Stage, "the wait parks the WORK, never the ladder")
	require.NotNil(s.T(), row.ClaimToken)
	s.Equal(c.ClaimToken, *row.ClaimToken)
	won, _ := s.waitState(t1.ID)
	s.Equal(board.WaitOnEvent, won, "the nudge does not clear the wait — only an advance does")
}

func (s *BoardSuite) TestPin_UserWait_FreezesLadder_Unchanged() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "frozen", nil, false)

	c := s.claimOrFail(ws)
	s.Require().Equal(t1.ID, c.TaskID)
	assignee := s.reloadTask(t1.ID).AssigneeSession
	s.Require().NotEmpty(assignee)
	_, err := s.svc.DeclareWait(context.Background(), s.agent(assignee, c.ClaimToken),
		"", c.ShortID, board.WaitOnUser, "owner decision: cut scope", "", "", "")
	s.Require().NoError(err)

	lives := &fakeLiveness{}
	lives.set(assignee, liveQuiet())
	var nudged int
	recon := s.pinRecon(lives, func(context.Context, string, board.NudgePayload) error {
		nudged++
		return nil
	})
	s.expireLease(t1.ID)
	s.rc1Sweep(recon)

	s.Equal(0, nudged, "a user-wait NEVER feeds the victims scan — the ladder is frozen")
	nid, _ := s.nudgeState(t1.ID)
	s.Empty(nid)
	s.Equal(0, s.countEvents("reclaim"))
	row := s.rawTask(t1.ID)
	s.Equal(board.StageInProgress, row.Stage)
	s.True(row.PendingInput, "the flag is the freeze — unchanged")
	won, wr := s.waitState(t1.ID)
	s.Equal(board.WaitOnUser, won, "the freeze is untouched by the sweep")
	s.Equal("owner decision: cut scope", wr)
}

// --- The 000050 exits' interplay ------------------------------------------

func (s *BoardSuite) TestPendingExits_ClearTheUserWait() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "exits", nil, false)

	c := s.claimOrFail(ws)
	s.Require().Equal(t1.ID, c.TaskID)
	_, err := s.svc.DeclareWait(context.Background(), s.agent(c.Assignee, c.ClaimToken),
		"", c.ShortID, board.WaitOnUser, "owner decision", "", "", "")
	s.Require().NoError(err)
	s.True(s.rawTask(t1.ID).PendingInput)

	// The manager re-queue exit (no identity: backlog branch) clears
	// the wait with the move.
	_, err = s.svc.MoveStage(context.Background(), s.agentManager(ws, "orchestrator-1"),
		"", c.ShortID, board.StageBacklog, "", "")
	s.Require().NoError(err)
	won, wr := s.waitState(t1.ID)
	s.Equal("", won, "the human exit clears the wait")
	s.Equal("", wr)
	s.False(s.rawTask(t1.ID).PendingInput)
}

// --- Sweep hygiene: the coupling pass leaves event-waits alone ------------

func (s *BoardSuite) TestPendingCoupling_IgnoresEventWaits() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "coupling", nil, false)

	c := s.claimOrFail(ws)
	s.Require().Equal(t1.ID, c.TaskID)
	s.declareEvent(s.T(), c, "PR 2083 merge", "pr_merged(2083)")

	// A full sweep (not just rc1): the pendingCoupling pass keys on
	// the FLAG — an event-wait must never be staged or resumed by it.
	res := s.sweep()
	s.Equal(0, res.PendingStaged, "an event-wait is not a pending_input to stage")
	s.Equal(0, res.Resumes, "an event-wait is not a pending_input to resume")
	row := s.rawTask(t1.ID)
	s.False(row.PendingInput)
	won, wr := s.waitState(t1.ID)
	s.Equal(board.WaitOnEvent, won)
	s.Equal("PR 2083 merge", wr)
}

// --- Determinism / idempotency --------------------------------------------

func (s *BoardSuite) TestDeclareWait_RequestIdReplayIsOriginal() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "replay", nil, false)

	c := s.claimOrFail(ws)
	s.Require().Equal(t1.ID, c.TaskID)
	act := s.agent(c.Assignee, c.ClaimToken)
	first, err := s.svc.DeclareWait(context.Background(), act, "", c.ShortID,
		board.WaitOnEvent, "PR 2083 merge", "pr_merged(2083)", testRepo, "req-wait-1")
	s.Require().NoError(err)
	second, err := s.svc.DeclareWait(context.Background(), act, "", c.ShortID,
		board.WaitOnEvent, "PR 2083 merge", "pr_merged(2083)", testRepo, "req-wait-1")
	s.Require().NoError(err, "a replayed requestId is 200-with-original, never an error")
	s.Equal(first.ID, second.ID)
	s.Equal(1, s.countEvents("wait_declared"), "the replay wrote no second declaration")
}

// --- The declaration-time repo identity (the patched charter) --------------

func (s *BoardSuite) TestDeclareWait_RepoGrammar() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "repo-grammar", nil, false)

	c := s.claimOrFail(ws)
	s.Require().Equal(t1.ID, c.TaskID)
	act := s.agent(c.Assignee, c.ClaimToken)

	guard := func(fn func() error) *board.GuardError {
		err := fn()
		require.Error(s.T(), err)
		var g *board.GuardError
		require.ErrorAs(s.T(), err, &g)
		return g
	}

	// pr-anchored without repo: the broker never guesses — refuse.
	g := guard(func() error {
		_, err := s.svc.DeclareWait(context.Background(), act, "", c.ShortID,
			board.WaitOnEvent, "PR 2083 merge", "pr_merged(2083)", "", "")
		return err
	})
	s.Equal(board.GuardInput, g.Code)
	require.Contains(s.T(), g.Detail, "owner/repo")

	// Malformed repo shapes.
	for _, bad := range []string{"no-slash", "a/b/c", "/repo", "owner/", "owner/repo extra"} {
		g = guard(func() error {
			_, err := s.svc.DeclareWait(context.Background(), act, "", c.ShortID,
				board.WaitOnEvent, "PR 2083 merge", "pr_merged(2083)", bad, "")
			return err
		})
		s.Equal(board.GuardInput, g.Code)
	}

	// story_done carries no repo (it reads the platform's own board).
	g = guard(func() error {
		_, err := s.svc.DeclareWait(context.Background(), act, "", c.ShortID,
			board.WaitOnEvent, "story done", "story_done(tsk-ebony-javelin-tramps)", testRepo, "")
		return err
	})
	s.Equal(board.GuardInput, g.Code)

	// The user arm takes no repo either.
	g = guard(func() error {
		_, err := s.svc.DeclareWait(context.Background(), act, "", c.ShortID,
			board.WaitOnUser, "owner call", "", testRepo, "")
		return err
	})
	s.Equal(board.GuardInput, g.Code)
	s.Equal(0, s.countEvents("wait_declared"), "no declaration landed through the refusals")

	// The happy path's payload carries the repo (plus the facts the
	// broker's materialize pass keys on).
	s.declareEvent(s.T(), c, "PR 2083 merge", "pr_merged(2083)")
	var repo string
	s.Require().NoError(s.pool.QueryRow(context.Background(), `
		SELECT payload->>'repo' FROM board_events
		WHERE action='wait_declared' AND item_id=$1`, t1.ID).Scan(&repo))
	s.Equal(testRepo, repo)
	var assignee, shortID, claimTok string
	s.Require().NoError(s.pool.QueryRow(context.Background(), `
		SELECT payload->>'assignee', payload->>'short_id', payload->>'claim_token' FROM board_events
		WHERE action='wait_declared' AND item_id=$1`, t1.ID).Scan(&assignee, &shortID, &claimTok))
	s.Equal(c.Assignee, assignee)
	s.Equal(t1.ShortID, shortID)
	s.Equal(c.ClaimToken, claimTok)
}

// Compile-time: the suite's time import stays used (Since assertions
// above rely on time.Time zero-checks via h.Waiting[0].Since.IsZero).
var _ = time.Time{}
