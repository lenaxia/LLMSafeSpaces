//go:build integration

package board_test

import (
	"context"
	"sync"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// persona_integration_test.go — the generalized stage-persona
// claim-then-spawn mechanism (owner directive 2026-10-24, the
// sub-orchestrator board test's claim-then-spawn-for-reviewers
// recommendation generalized to any stage + any persona). PG harness;
// skips without TEST_DATABASE_URL. BoardSuite methods (same harness
// pool, per-test service with capturing seams). Also carries the
// #1832 item 5 verdict-chain pins (absorbed from the retired
// review-assign suite).

// claimCapture records every spawn's full Claim envelope.
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

func strPtr(v string) *string { return &v }

func boolStr(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// countProjectEvents scopes the action count to ONE project (suite
// tests share one SetupTest truncate, so a global count would leak
// across projects seeded in the same test).
func (s *BoardSuite) countProjectEvents(projectID, action string) int {
	var n int
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM board_events WHERE project_id=$1 AND action=$2`, projectID, action).Scan(&n))
	return n
}

func (s *BoardSuite) setStagePersonas(svc *board.Service, p *board.Project, m map[string]board.PersonaConfig) {
	_, err := svc.SetProjectBoardConfig(context.Background(), s.owner(), p.ID, "",
		&board.BoardConfigOverride{StagePersonas: &m})
	s.Require().NoError(err)
}

// TestPersona_StageEntryClaimsThenSpawns — entering a persona-bearing
// stage CLAIMS the row (re-claim-class mint: token + lease, attempts
// UNCHANGED, provenance flag false) and spawns a fresh session whose
// charter envelope carries the configured persona.
func (s *BoardSuite) TestPersona_StageEntryClaimsThenSpawns() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "persona-entry", nil, false)

	spawner := &claimCapture{}
	svc := board.NewService(s.pool, s.cfg, board.WithWorkerSpawner(spawner.Spawn))
	s.setStagePersonas(svc, p, map[string]board.PersonaConfig{
		"in_review": {Name: "auditor", Instructions: "Verify adversarially."},
	})

	c, err := svc.ClaimNext(ctx, "w-"+ws, ws)
	s.Require().NoError(err)
	s.Require().Equal(task.ID, c.TaskID)
	workerAttempts := s.rawTask(task.ID).Attempts

	_, err = svc.MoveStage(ctx, s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)

	row := s.rawTask(task.ID)
	s.Require().NotNil(row.ClaimToken, "the persona entry mints a claim token")
	s.Require().NotNil(row.LeaseExp, "the persona entry mints a lease")
	s.Equal(workerAttempts, row.Attempts, "a persona mint is re-claim-class: attempts UNCHANGED")
	s.False(row.ClaimIncr, "provenance flag false — the throttle never rolls it back")

	// Spawn #2 (after the worker grant): the persona worker's charter.
	s.Require().Len(spawner.claims, 2, "worker grant + persona worker")
	pc := spawner.claims[1]
	require.NotNil(s.T(), pc.Persona, "the in_review spawn carries the persona")
	s.Equal("auditor", pc.Persona.Name)
	s.Equal("Verify adversarially.", pc.Persona.Instructions)
	s.Equal(board.StageInReview, pc.Stage)
	s.Equal(*row.ClaimToken, pc.ClaimToken, "the charter carries the MINTED token (claim-before-spawn)")
	s.Equal(1, s.countProjectEvents(p.ID, "persona_claim"), "the mint records its audit event")
}

// TestPersona_NoConfigByteIdenticalToday — with no persona configured
// (the shipped defaults), →in_review is byte-identical to today: no
// token, no lease, no extra spawn, no persona_claim event.
func (s *BoardSuite) TestPersona_NoConfigByteIdenticalToday() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "no-persona", nil, false)

	spawner := &claimCapture{}
	svc := board.NewService(s.pool, s.cfg, board.WithWorkerSpawner(spawner.Spawn))

	c, err := svc.ClaimNext(ctx, "w-"+ws, ws)
	s.Require().NoError(err)
	s.Require().Equal(task.ID, c.TaskID)
	_, err = svc.MoveStage(ctx, s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)

	row := s.rawTask(task.ID)
	s.Require().Nil(row.ClaimToken, "today's shape: the forward exit clears the token")
	s.Require().Nil(row.LeaseExp, "today's shape: the forward exit clears the lease")
	s.Require().Len(spawner.claims, 1, "exactly the worker grant's spawn — nothing else")
	s.Nil(spawner.claims[0].Persona, "the worker charter carries no persona")
	s.Equal(0, s.countProjectEvents(p.ID, "persona_claim"))
	s.Equal(0, s.countProjectEvents(p.ID, "review_assign"))
	// The dormant path's parallel event vocabulary is gone too: the
	// mechanism absorbed it (auto-review == a reviewer persona).
}

// TestPersona_LegacyCompositionIsReviewerPersona — the legacy config
// composition (reviewColumn=auto AND autoAssignReviewer=true, tiers
// merged per A1) keeps its meaning: in_review gets the default
// REVIEWER persona — now via claim-then-spawn (one lease, one token).
func (s *BoardSuite) TestPersona_LegacyCompositionIsReviewerPersona() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "legacy", nil, false)

	spawner := &claimCapture{}
	svc := board.NewService(s.pool, s.cfg, board.WithWorkerSpawner(spawner.Spawn))
	_, err := svc.SetUserBoardConfig(ctx, s.owner(), &board.BoardConfigOverride{
		ReviewColumn: strPtr("auto"),
	})
	s.Require().NoError(err)
	av := true
	_, err = svc.SetProjectBoardConfig(ctx, s.owner(), p.ID, "", &board.BoardConfigOverride{
		AutoAssignReviewer: &av,
	})
	s.Require().NoError(err)

	c, err := svc.ClaimNext(ctx, "w-"+ws, ws)
	s.Require().NoError(err)
	s.Require().Equal(task.ID, c.TaskID)
	_, err = svc.MoveStage(ctx, s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)

	s.Require().Len(spawner.claims, 2, "grant + reviewer")
	rc := spawner.claims[1]
	require.NotNil(s.T(), rc.Persona)
	s.Equal(board.DefaultReviewerPersona.Name, rc.Persona.Name)
	s.Equal(board.StageInReview, rc.Stage)
	s.Require().NotNil(s.rawTask(task.ID).ClaimToken, "the reviewer HOLDS a claim (one lease, one token)")

	// The non-firing legacy arms stay no-persona (matrix tail).
	for _, tc := range []struct {
		rc string
		av bool
	}{
		{"auto", false}, {"manual", true}, {"off", true},
	} {
		s.Run(tc.rc+"+"+boolStr(tc.av), func() {
			ctx := context.Background()
			ws2 := s.seedWorkspace()
			p2 := s.seedProject(ws2, 2)
			e2 := s.seedEpic(p2, board.EpicActive)
			t2 := s.seedTask(e2, "matrix-"+tc.rc, nil, false)
			sp2 := &claimCapture{}
			svc2 := board.NewService(s.pool, s.cfg, board.WithWorkerSpawner(sp2.Spawn))
			_, err := svc2.SetUserBoardConfig(ctx, s.owner(), &board.BoardConfigOverride{
				ReviewColumn: &tc.rc,
			})
			s.Require().NoError(err)
			_, err = svc2.SetProjectBoardConfig(ctx, s.owner(), p2.ID, "", &board.BoardConfigOverride{
				AutoAssignReviewer: &tc.av,
			})
			s.Require().NoError(err)
			c2, err := svc2.ClaimNext(ctx, "w2-"+ws2, ws2)
			s.Require().NoError(err)
			s.Require().Equal(t2.ID, c2.TaskID)
			_, err = svc2.MoveStage(ctx, s.agent(c2.Assignee, c2.ClaimToken), "", c2.ShortID, board.StageInReview, "", "")
			s.Require().NoError(err)
			s.Len(sp2.claims, 1, "manual/off/auto-without-assign keep today's behavior: no dispatch")
			s.Require().Nil(s.rawTask(t2.ID).ClaimToken)
			s.Equal(0, s.countProjectEvents(p2.ID, "persona_claim"))
		})
	}
}

// TestPersona_DeadWorkerLeaseLapseFreshReclaim — the required
// dead-persona-worker path: a reviewer that never turns (stillborn —
// zero heartbeats; the #1832 WS4 guard has nothing to refuse because
// nothing asks) lapses its lease; the sweep marks the #1813 failure,
// and once the backoff cools a FRESH persona worker is re-claimed
// (fresh token + lease + persona spawn). Repeated death climbs the
// failure ledger to the attempts cap → pending_input/escalated (the
// human-only exit).
func (s *BoardSuite) TestPersona_DeadWorkerLeaseLapseFreshReclaim() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "dead-reviewer", nil, false)

	spawner := &claimCapture{}
	svc := board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(spawner.Spawn),
		board.WithWorkerAborter(s.aborter.Abort),
	)
	s.setStagePersonas(svc, p, map[string]board.PersonaConfig{"in_review": {Name: "reviewer"}})

	// The reconciler needs the spawner for the re-arm's fresh spawn.
	s.recon = board.NewReconciler(s.pool, s.cfg,
		board.WithWorkerSpawner(spawner.Spawn),
		board.WithWorkerAborter(s.aborter.Abort),
		board.WithPRDriftSource(s.drift),
		board.WithPendingInputSource(s.pending),
		board.WithFailpoint(s.fps.fp),
	)

	c, err := svc.ClaimNext(ctx, "w-"+ws, ws)
	s.Require().NoError(err)
	s.Require().Equal(task.ID, c.TaskID)
	_, err = svc.MoveStage(ctx, s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)
	s.Require().Len(spawner.claims, 2, "grant + reviewer spawn #1")
	attemptsAtEntry := s.rawTask(task.ID).Attempts

	// Death #1: the reviewer never turns; its lease lapses.
	s.expireLease(task.ID)
	res := s.sweep()
	s.Equal(0, res.Reclaims, "the persona row is in_review, not in_progress — RC1's in_progress pass does not own it")
	row := s.rawTask(task.ID)
	s.Require().Nil(row.ClaimToken, "the lapse cleared the dead claim")
	s.Require().Nil(row.LeaseExp)
	s.Equal(1, s.countProjectEvents(p.ID, "reclaim"))
	s.Equal(attemptsAtEntry, row.Attempts, "persona lapses never touch attempts")

	// Backoff elapsed → the re-arm pass claims a FRESH persona worker.
	s.clearTaskBackoff(task.ID)
	s.sweep()
	row = s.rawTask(task.ID)
	s.Require().NotNil(row.ClaimToken, "the re-arm minted a fresh claim")
	s.Require().NotNil(row.LeaseExp)
	s.Require().Len(spawner.claims, 3, "a FRESH persona worker spawned")
	fresh := spawner.claims[2]
	require.NotNil(s.T(), fresh.Persona)
	s.Equal("reviewer", fresh.Persona.Name)
	s.Equal(board.StageInReview, fresh.Stage)
	s.Equal(*row.ClaimToken, fresh.ClaimToken)
	s.Equal(attemptsAtEntry, row.Attempts)

	// Death #2: lapse → failure_count=2 → backoff cools → re-arm #2
	// (a fresh live claim to kill).
	s.expireLease(task.ID)
	s.sweep()
	s.clearTaskBackoff(task.ID)
	s.sweep()
	s.Require().NotNil(s.rawTask(task.ID).ClaimToken, "re-arm #2 minted a live claim")

	// Death #3: the failure ledger climbs to the attempts cap (3) —
	// the next re-arm escalates instead of spinning.
	s.expireLease(task.ID)
	s.sweep()
	s.clearTaskBackoff(task.ID)
	s.sweep()
	row = s.rawTask(task.ID)
	s.Equal(board.StagePendingInput, row.Stage, "failure_count at the cap escalates — the human-only exit arbitrates the loop")
	require.NotNil(s.T(), row.PendingOrigin)
	s.Equal("escalated", *row.PendingOrigin)
	s.Require().Nil(row.ClaimToken)
}

// TestPersona_HeartbeatOnReviewClaim — the persona worker renews under
// the SAME token + evidence rules (liveness = new progress evidence);
// an UNCLAIMED in_review row still refuses heartbeats (today's
// semantics for no-persona rows).
func (s *BoardSuite) TestPersona_HeartbeatOnReviewClaim() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "hb", nil, false)

	spawner := &claimCapture{}
	svc := board.NewService(s.pool, s.cfg, board.WithWorkerSpawner(spawner.Spawn))
	s.setStagePersonas(svc, p, map[string]board.PersonaConfig{"in_review": {Name: "reviewer"}})

	c, err := svc.ClaimNext(ctx, "w-"+ws, ws)
	s.Require().NoError(err)
	_, err = svc.MoveStage(ctx, s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)
	row := s.rawTask(task.ID)
	reviewerToken := *row.ClaimToken

	// A heartbeat with new evidence extends the review claim.
	hb, err := svc.Heartbeat(ctx, s.agent("sess", reviewerToken), "", task.ShortID, time.Now().Add(2*time.Second))
	s.Require().NoError(err)
	s.True(hb.Extended, "the persona claim renews under the token + evidence rules")

	// A stale stamp (no new evidence) does not extend.
	hb, err = svc.Heartbeat(ctx, s.agent("sess", reviewerToken), "", task.ShortID, time.Now().Add(-time.Hour))
	s.Require().NoError(err)
	s.False(hb.Extended)

	// The WRONG token refuses.
	_, err = svc.Heartbeat(ctx, s.agent("sess", "wrong-token"), "", task.ShortID, time.Now())
	s.Require().Error(err)

	// An UNCLAIMED in_review row (no persona configured) still refuses.
	ws2 := s.seedWorkspace()
	p2 := s.seedProject(ws2, 2)
	e2 := s.seedEpic(p2, board.EpicActive)
	t2 := s.seedTask(e2, "hb-nocfg", nil, false)
	svc2 := board.NewService(s.pool, s.cfg, board.WithWorkerSpawner((&claimCapture{}).Spawn))
	c2, err := svc2.ClaimNext(ctx, "w2-"+ws2, ws2)
	s.Require().NoError(err)
	_, err = svc2.MoveStage(ctx, s.agent(c2.Assignee, c2.ClaimToken), "", c2.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)
	_, err = svc2.Heartbeat(ctx, s.agent("sess", c2.ClaimToken), "", t2.ShortID, time.Now())
	s.Require().Error(err, "today's rule: an unclaimed in_review row holds no live claim")
}

// TestPersona_ImplementerPersonaRidesGrant — an arbitrary in_progress
// persona (config-driven, nothing reviewer-specific) rides the EXISTING
// grant spawn's charter.
func (s *BoardSuite) TestPersona_ImplementerPersonaRidesGrant() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	s.seedTask(e, "impl-persona", nil, false)

	spawner := &claimCapture{}
	svc := board.NewService(s.pool, s.cfg, board.WithWorkerSpawner(spawner.Spawn))
	s.setStagePersonas(svc, p, map[string]board.PersonaConfig{"in_progress": {Name: "implementer"}})

	_, err := svc.ClaimNext(ctx, "w-"+ws, ws)
	s.Require().NoError(err)
	s.Require().Len(spawner.claims, 1)
	require.NotNil(s.T(), spawner.claims[0].Persona, "the grant's charter carries the implementer persona")
	s.Equal("implementer", spawner.claims[0].Persona.Name)
	s.Equal(board.StageInProgress, spawner.claims[0].Stage)
}

// TestPersona_BounceAfterPersonaClaim — a fail verdict on a
// persona-claimed in_review row rides the EXISTING bounce: the mint
// preserved the recorded identity (the never-token-less rule), and the
// bounced implementer's spawn re-enters in_progress (with the
// configured implementer persona, when there is one).
func (s *BoardSuite) TestPersona_BounceAfterPersonaClaim() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "bounce", nil, false)

	spawner := &claimCapture{}
	svc := board.NewService(s.pool, s.cfg, board.WithWorkerSpawner(spawner.Spawn))
	s.setStagePersonas(svc, p, map[string]board.PersonaConfig{
		"in_review":   {Name: "reviewer"},
		"in_progress": {Name: "implementer"},
	})

	c, err := svc.ClaimNext(ctx, "w-"+ws, ws)
	s.Require().NoError(err)
	_, err = svc.MoveStage(ctx, s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)
	s.Require().Len(spawner.claims, 2)

	out, v, err := svc.RecordVerdict(ctx, s.owner(), "", c.ShortID, board.VerdictInput{
		Verdict:  board.VerdictFail,
		Reviewer: "rev-persona",
		Findings: []board.VerdictFinding{{Severity: board.SeverityMajor, Pointer: "criterion 0: no evidence"}},
	}, "")
	s.Require().NoError(err)
	s.Require().True(v.Applied)
	s.Equal(board.StageInProgress, out.Stage)

	s.Require().Len(spawner.claims, 3, "the bounce's fresh implementer spawned")
	bounced := spawner.claims[2]
	s.Equal(board.StageInProgress, bounced.Stage)
	require.NotNil(s.T(), bounced.Persona)
	s.Equal("implementer", bounced.Persona.Name)
	require.NotEmpty(s.T(), bounced.VerdictChain, "the bounced implementer sees the verdict chain")

	// The persona review round's claim is consumed by the bounce.
	row := s.rawTask(task.ID)
	s.Require().NotNil(row.ClaimToken)
	s.Equal(*row.ClaimToken, bounced.ClaimToken)
}

// TestPersona_ApproveDoneOnPersonaClaim — the round-1 review's missing
// arm: an approve verdict on a persona-claimed in_review row rides the
// existing →done clear (persona claim consumed: token+lease NULL, the
// Done guard applies regardless of the claim).
func (s *BoardSuite) TestPersona_ApproveDoneOnPersonaClaim() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "approve", nil, true) // criteria pre-met for the Done guard

	spawner := &claimCapture{}
	svc := board.NewService(s.pool, s.cfg, board.WithWorkerSpawner(spawner.Spawn))
	s.setStagePersonas(svc, p, map[string]board.PersonaConfig{"in_review": {Name: "reviewer"}})

	c, err := svc.ClaimNext(ctx, "w-"+ws, ws)
	s.Require().NoError(err)
	_, err = svc.MoveStage(ctx, s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StageInReview, "https://example.org/evidence", "")
	s.Require().NoError(err)
	s.Require().NotNil(s.rawTask(task.ID).ClaimToken, "the reviewer holds the persona claim")

	out, v, err := svc.RecordVerdict(ctx, s.owner(), "", c.ShortID, board.VerdictInput{
		Verdict:     board.VerdictApprove,
		Reviewer:    "rev-approve",
		EvidenceURL: "https://example.org/evidence",
	}, "")
	s.Require().NoError(err)
	s.Require().True(v.Applied)
	s.Equal(board.StageDone, out.Stage)

	row := s.rawTask(task.ID)
	s.Require().Nil(row.ClaimToken, "the approve consumes the persona claim (token cleared)")
	s.Require().Nil(row.LeaseExp)
	s.Equal(1, row.Attempts, "one worker grant; the persona mint and the verdict never touch attempts")
}

// TestReview_1832_VerdictChainRidesNextClaim — the absorbed suite's
// verdict-chain pin (unchanged semantics: the chain rides EVERY spawn
// — the fresh claim, the fail-bounce's re-arm, the post-lapse
// re-claim).
func (s *BoardSuite) TestReview_1832_VerdictChainRidesNextClaim() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "chain", nil, false)

	spawner := &claimCapture{}
	svc := board.NewService(s.pool, s.cfg, board.WithWorkerSpawner(spawner.Spawn))

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
	// claimable set NOW — then a FRESH claimant spawns, and its charter
	// carries the verdict chain (iteration history).
	s.expireLease(task.ID)
	s.sweep()
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
