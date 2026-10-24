//go:build integration

package board_test

import (
	"context"
	"sync"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// #1832 item 5 (2/2) — config-gated review auto-assign + the verdict
// chain riding the next claimant's charter (PG harness; skips without
// TEST_DATABASE_URL). These are BoardSuite methods (same harness pool,
// per-test service with capturing seams).

type captureAssigner struct {
	mu    sync.Mutex
	calls []string
}

func (c *captureAssigner) Assign(_ context.Context, t *board.Task, _ *board.Project) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, t.ShortID)
	return nil
}

func (c *captureAssigner) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

type claimCapture struct {
	mu     sync.Mutex
	claims []*board.Claim
}

func (c *claimCapture) Spawn(_ context.Context, cl *board.Claim) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.claims = append(c.claims, cl)
	return "sess-" + cl.ShortID, nil
}

func (s *BoardSuite) TestReview_1832_AutoAssignGatingMatrix() {
	cases := []struct {
		reviewColumn string
		autoAssign   bool
		wantAssign   bool
	}{
		{"auto", true, true},   // the ONLY firing combination
		{"auto", false, false}, // auto column, manual assignment
		{"manual", true, false},
		{"off", true, false},
		{"manual", false, false}, // today's default, exactly
	}
	for _, tc := range cases {
		s.Run(tc.reviewColumn+"+"+boolStr(tc.autoAssign), func() {
			ctx := context.Background()
			ws := s.seedWorkspace()
			p := s.seedProject(ws, 2)
			e := s.seedEpic(p, board.EpicActive)
			task := s.seedTask(e, "gate-"+tc.reviewColumn, nil, false)

			spawner := &claimCapture{}
			assigner := &captureAssigner{}
			svc := board.NewService(s.pool, s.cfg,
				board.WithWorkerSpawner(spawner.Spawn),
				board.WithReviewAssigner(assigner.Assign),
			)
			// Config: user defaults carry reviewColumn (user tier); the
			// project override carries autoAssignReviewer (project tier)
			// — both merge tiers exercised per A1 semantics.
			_, err := svc.SetUserBoardConfig(ctx, s.owner(), &board.BoardConfigOverride{
				ReviewColumn: &tc.reviewColumn,
			})
			s.Require().NoError(err)
			av := tc.autoAssign
			_, err = svc.SetProjectBoardConfig(ctx, s.owner(), p.ID, "", &board.BoardConfigOverride{
				AutoAssignReviewer: &av,
			})
			s.Require().NoError(err)

			c, err := svc.ClaimNext(ctx, "w-"+ws, ws)
			s.Require().NoError(err)
			s.Require().Equal(task.ID, c.TaskID)
			_, err = svc.MoveStage(ctx, s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StageInReview, "", "")
			s.Require().NoError(err)

			if tc.wantAssign {
				s.Equal(1, assigner.count(), "auto+true dispatches exactly one reviewer, directly (never queued)")
				s.Equal(1, s.countProjectEvents(p.ID, "review_assign"), "the intent event records (project-scoped)")
			} else {
				s.Equal(0, assigner.count(), "manual/off/auto-without-assign keep today's behavior: no dispatch")
				s.Equal(0, s.countProjectEvents(p.ID, "review_assign"), "and no intent event (project-scoped)")
			}
		})
	}
}

func (s *BoardSuite) TestReview_1832_VerdictChainRidesNextClaim() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "chain", nil, false)

	spawner := &claimCapture{}
	svc := board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(spawner.Spawn),
		board.WithReviewAssigner((&captureAssigner{}).Assign),
	)

	// Round 1: fresh claim — no verdicts yet.
	c1, err := svc.ClaimNext(ctx, "w1-"+ws, ws)
	s.Require().NoError(err)
	s.Require().Equal(task.ID, c1.TaskID)
	_, err = svc.MoveStage(ctx, s.agent(c1.Assignee, c1.ClaimToken), "", c1.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)

	// Fail verdict: findings land in the chain; the task bounces
	// in_progress (attempts unchanged — B1's pin).
	out, v, err := svc.RecordVerdict(ctx, s.owner(), "", c1.ShortID, board.VerdictInput{
		Verdict:  board.VerdictFail,
		Reviewer: "rev-chain",
		Findings: []board.VerdictFinding{{Severity: board.SeverityMajor, Pointer: "criterion 0: no evidence"}},
	}, "")
	s.Require().NoError(err)
	s.Require().True(v.Applied)
	s.Equal(board.StageInProgress, out.Stage)

	// Round 2: the bounced row's lease lapses; the sweep's #1813 path
	// returns it to backlog with a failure backoff (a lapsed lease IS a
	// failure event); the cooldown is cleared so the row re-enters the
	// claimable set NOW (the sweep's auto-return clock is not what this
	// row pins) — then a FRESH claimant spawns, and its charter carries
	// the verdict chain (iteration history).
	s.expireLease(task.ID)
	s.sweep()
	// Insurance: the lapse round's failure accounting must not tip the
	// row into escalation (this row pins CHAIN propagation, not the
	// attempts cap) — clear the backoff AND the counter.
	_, err = s.pool.Exec(ctx, `UPDATE tasks SET cooldown_until=NULL, attempts=0, last_attempt_failed=false WHERE id=$1`, task.ID)
	s.Require().NoError(err)
	c2, err := svc.ClaimNext(ctx, "w2-"+ws, ws)
	s.Require().NoError(err)
	s.Require().Equal(task.ID, c2.TaskID)
	// THREE spawns: the fresh claim, the fail-bounce's re-arm (a new
	// claim minted for the recorded assignee — spawn #2), and the
	// post-lapse re-claim (the fresh claimant — spawn #3, whose charter
	// carries the chain).
	require.Len(s.T(), spawner.claims, 3, "spawns: fresh + bounce re-arm + re-claim")
	third := spawner.claims[2]
	require.NotEmpty(s.T(), third.VerdictChain, "the next claimant sees prior verdicts")
	s.Equal(board.VerdictFail, third.VerdictChain[0].Verdict)
	s.Equal("rev-chain", third.VerdictChain[0].Reviewer)
	require.Len(s.T(), third.VerdictChain[0].Findings, 1)
	// And the FIRST claim carried nothing (no verdicts existed); the
	// bounce re-arm's charter carries the chain too (the bounced
	// assignee is the first to see the findings).
	require.Empty(s.T(), spawner.claims[0].VerdictChain)
	require.NotEmpty(s.T(), spawner.claims[1].VerdictChain, "the bounce re-arm's charter embeds the verdict chain")
}

// TestReview_AutoReviewDefaultOn — the owner ruling of 2026-10-24
// ("flip defaults for review to on"): the CODE defaults resolve
// reviewColumn=auto + autoAssignReviewer=true, so a FRESHLY-CREATED
// board project (no board_config_defaults row for the owner, no
// board_project_config_overrides row for the project) dispatches the
// reviewer on →in_review with NO configuration writes at all. RED on
// the pre-flip defaults (manual/false → no dispatch).
func (s *BoardSuite) TestReview_AutoReviewDefaultOn() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "default-on", nil, false)

	spawner := &claimCapture{}
	assigner := &captureAssigner{}
	svc := board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(spawner.Spawn),
		board.WithReviewAssigner(assigner.Assign),
	)

	// Effective config, straight read: the fresh rows must render the
	// flipped defaults (the same merge the gate consumes).
	pc, err := svc.GetProjectBoardConfig(ctx, s.owner(), p.ID, "")
	s.Require().NoError(err)
	s.Equal(board.ReviewColumnAuto, pc.Effective.ReviewColumn,
		"fresh project: reviewColumn inherits the flipped code default")
	s.True(pc.Effective.AutoAssignReviewer,
		"fresh project: autoAssignReviewer inherits the flipped code default")

	c, err := svc.ClaimNext(ctx, "w-"+ws, ws)
	s.Require().NoError(err)
	s.Require().Equal(task.ID, c.TaskID)
	_, err = svc.MoveStage(ctx, s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)

	s.Equal(1, assigner.count(), "fresh project, zero config rows: the flipped defaults dispatch the reviewer")
	s.Equal(1, s.countProjectEvents(p.ID, "review_assign"), "the intent event records")
}

// TestReview_ExplicitOptOutSurvivesDefaultFlip — the back-compat half
// of the 2026-10-24 ruling: flipping a DEFAULT must never override a
// DELIBERATE stored opt-out. An explicit off at EITHER tier (user
// defaults or project overrides) keeps auto-review off after the flip,
// and clearing the opt-out (REPLACE `{}`) re-enables the new default —
// the opt-out lives in the stored map, not a sticky flag.
func (s *BoardSuite) TestReview_ExplicitOptOutSurvivesDefaultFlip() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	spawner := &claimCapture{}
	assigner := &captureAssigner{}
	svc := board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(spawner.Spawn),
		board.WithReviewAssigner(assigner.Assign),
	)

	// drive seeds one more task in the GIVEN project, claims it, and
	// moves it to in_review; returns whether the reviewer dispatched
	// for THAT project (the claim is workspace-scoped, but the only
	// claimable task is the one just seeded — earlier tasks are past
	// backlog).
	drive := func(ctx context.Context, p *board.Project, name string) bool {
		e := s.seedEpic(p, board.EpicActive)    //nolint:contextcheck // grandfathered harness helper (ctx-less by design, like the gating matrix's seed sites)
		task := s.seedTask(e, name, nil, false) //nolint:contextcheck // same grandfathered harness helper seam
		c, err := svc.ClaimNext(ctx, "w-"+ws+"-"+name, ws)
		s.Require().NoError(err)
		s.Require().Equal(task.ID, c.TaskID, "one claimable task at a time: the claim is deterministic")
		_, err = svc.MoveStage(ctx, s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StageInReview, "", "")
		s.Require().NoError(err)
		return s.countProjectEvents(p.ID, "review_assign") > 0 //nolint:contextcheck // same grandfathered harness helper seam (self-scoped query ctx)
	}

	// Project tier, key 1: explicit reviewColumn=manual opts the project
	// out even though autoAssignReviewer re-inherits (now: true).
	pOff := s.seedProject(ws, 2)
	manual := board.ReviewColumnManual
	_, err := svc.SetProjectBoardConfig(ctx, s.owner(), pOff.ID, "", &board.BoardConfigOverride{ReviewColumn: &manual})
	s.Require().NoError(err)
	s.False(drive(ctx, pOff, "optout-proj-manual"), "explicit project reviewColumn=manual stays off after the default flip")

	// Clearing the opt-out re-enables the flipped default (REPLACE
	// semantics: absent = inherit, never sticky).
	_, err = svc.SetProjectBoardConfig(ctx, s.owner(), pOff.ID, "", &board.BoardConfigOverride{})
	s.Require().NoError(err)
	s.True(drive(ctx, pOff, "optout-proj-cleared"), "cleared override re-inherits the flipped on-default")

	// Project tier, key 2: explicit autoAssignReviewer=false opts the
	// project out even though reviewColumn re-inherits (now: auto).
	pNoAssign := s.seedProject(ws, 2)
	noAssign := false
	_, err = svc.SetProjectBoardConfig(ctx, s.owner(), pNoAssign.ID, "", &board.BoardConfigOverride{AutoAssignReviewer: &noAssign})
	s.Require().NoError(err)
	s.False(drive(ctx, pNoAssign, "optout-proj-noassign"), "explicit project autoAssignReviewer=false stays off after the default flip")

	// User tier: explicit user-default reviewColumn=off opts EVERY
	// inheriting project out; restoring it (replace with {}) re-enables.
	pUser := s.seedProject(ws, 2)
	off := board.ReviewColumnOff
	_, err = svc.SetUserBoardConfig(ctx, s.owner(), &board.BoardConfigOverride{ReviewColumn: &off})
	s.Require().NoError(err)
	s.False(drive(ctx, pUser, "optout-user-off"), "explicit user reviewColumn=off stays off after the default flip")
	_, err = svc.SetUserBoardConfig(ctx, s.owner(), &board.BoardConfigOverride{})
	s.Require().NoError(err)
	s.True(drive(ctx, pUser, "optout-user-cleared"), "cleared user map re-inherits the flipped on-default")
}

// TestReview_GateFailsClosedOnUserMapDrift — the dispatch gate is the
// flip's fail-closed counterpart (r1 review finding): a drifted row in
// the owner's board_config_defaults map (known key, malformed value)
// must surface as NO dispatch — the flipped on-defaults never ride a
// corrupt row into enabling reviewer dispatch. Same construction as
// TestBoardConfig_FailClosedOnDrift's user-map arm; this pins the
// GATE's behavior (warn + return, no event), not just the read seam.
func (s *BoardSuite) TestReview_GateFailsClosedOnUserMapDrift() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "gate-drift", nil, false)

	spawner := &claimCapture{}
	assigner := &captureAssigner{}
	svc := board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(spawner.Spawn),
		board.WithReviewAssigner(assigner.Assign),
	)

	// Drift: the exact TestBoardConfig_FailClosedOnDrift construction —
	// a known key with a malformed value, landed via raw SQL.
	_, err := s.pool.Exec(ctx,
		`INSERT INTO board_config_defaults (user_id, key, value) VALUES ($1, 'autoAssignReviewer', '"not-a-bool"')`, testUser)
	s.Require().NoError(err)

	c, err := svc.ClaimNext(ctx, "w-"+ws, ws)
	s.Require().NoError(err)
	s.Require().Equal(task.ID, c.TaskID)
	_, err = svc.MoveStage(ctx, s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)

	s.Equal(0, assigner.count(), "drifted user map: the gate fails closed — no dispatch under the flipped defaults")
	s.Equal(0, s.countProjectEvents(p.ID, "review_assign"), "and no intent event (the manual path remains)")
}

func boolStr(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// countProjectEvents scopes the action count to ONE project — the
// gating matrix runs all five combos under a single SetupTest (one
// truncate), so a global count would leak the firing combo's event
// into every later combo's zero-assertion.
func (s *BoardSuite) countProjectEvents(projectID, action string) int {
	var n int
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM board_events WHERE project_id=$1 AND action=$2`, projectID, action).Scan(&n))
	return n
}
