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
				s.Equal(1, s.countEvents("review_assign"), "the intent event records")
			} else {
				s.Equal(0, assigner.count(), "manual/off/auto-without-assign keep today's behavior: no dispatch")
				s.Equal(0, s.countEvents("review_assign"), "and no intent event")
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

	// Round 2: the bounced row's lease lapses; a FRESH claimant spawns —
	// its charter carries the verdict chain (iteration history).
	s.expireLease(task.ID)
	c2, err := svc.ClaimNext(ctx, "w2-"+ws, ws)
	s.Require().NoError(err)
	s.Require().Equal(task.ID, c2.TaskID)
	require.Len(s.T(), spawner.claims, 2, "two spawns: fresh + re-claim")
	second := spawner.claims[1]
	require.NotEmpty(s.T(), second.VerdictChain, "the next claimant sees prior verdicts")
	s.Equal(board.VerdictFail, second.VerdictChain[0].Verdict)
	s.Equal("rev-chain", second.VerdictChain[0].Reviewer)
	require.Len(s.T(), second.VerdictChain[0].Findings, 1)
	// And the FIRST claim carried nothing (no verdicts existed).
	require.Empty(s.T(), spawner.claims[0].VerdictChain)
}

func boolStr(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
