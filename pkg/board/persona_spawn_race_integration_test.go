//go:build integration

package board_test

import (
	"context"
	"sync"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// persona_spawn_race_integration_test.go — issue #1947 regression pins.
// The v0.43.0 live probe (project fixes-verification-043, task
// tsk-chalky-oxbow-stencils) surfaced three symptoms against the #1941
// stage-persona claim-then-spawn machinery, plus the follow-up heartbeat
// finding (issue comment, 2026-10-25T03:49Z). These pins hold the
// machinery to its designed behavior under the PRODUCTION spawn shape —
// the charter dispatch (newBoardSpawner) blocks for the spawned
// session's ENTIRE FIRST TURN, so a freshly-granted task sits claimed
// but UNSTAMPED (assignee NULL) for the whole first turn, and every
// stage-persona mint opens a mint→stamp window.
//
// Per-symptom pins (the probe-1-vs-probe-2 differential is the
// discriminator: probe 1's worker self-moved at T+30s — after its stamp
// landed; probe 2's move hit at T+1s — mid-flight):
//
//	S1  flip-without-action — no MoveStage ⇒ no in_review entry, no
//	    second spawn, through a full sweep + dispatch drive, while the
//	    grant's async spawn is parked mid-dispatch (the engine has NO
//	    mechanical in_progress→in_review writer; the only flip path is
//	    the token-guarded MoveStage).
//	S2  duplicate-spawn containment — the T+1s worker move (probe 2's
//	    shape): the flip mints the reviewer claim, exactly ONE
//	    reviewer-persona spawn fires on in_review entry, the parked
//	    implementer spawn's late stamp LOSES the race and is aborted
//	    (the double-execution guard), and no third spawn exists.
//	S5  heartbeat staleness (the 03:49Z finding) — between the re-arm
//	    mint and the fresh reviewer's stamp, a heartbeat carrying the
//	    FRESH claim token must not be stillborn-refused against the
//	    DEAD prior assignee the lapsed claim left behind (rc1 clears
//	    assignee on in_progress reclaims; the persona lapse pass must
//	    match that discipline).
//
// PG harness; skips without TEST_DATABASE_URL. BoardSuite methods.

// gatedSpawner simulates the production charter dispatch: every call
// records its Claim at ENTRY (arrival order = dispatch order) and then
// parks on the per-call gate when one is armed — the spawn-stamp lands
// only when the spawner returns, exactly like newBoardSpawner whose
// Execute spans the spawned session's first turn.
type gatedSpawner struct {
	mu     sync.Mutex
	claims []*board.Claim
	gates  map[int]chan struct{}
}

func newGatedSpawner(armed ...int) *gatedSpawner {
	g := &gatedSpawner{gates: map[int]chan struct{}{}}
	for _, i := range armed {
		g.gates[i] = make(chan struct{})
	}
	return g
}

func (g *gatedSpawner) Spawn(_ context.Context, c *board.Claim) (string, error) {
	g.mu.Lock()
	idx := len(g.claims)
	g.claims = append(g.claims, c)
	gate := g.gates[idx]
	g.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-time.After(30 * time.Second): // harness belt: a wedged gate fails the test, not the CI runner
			return "sess-gate-timeout", nil
		}
	}
	return "sess-" + string(rune('0'+idx)), nil
}

func (g *gatedSpawner) release(idx int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if ch, ok := g.gates[idx]; ok {
		close(ch)
	}
}

func (g *gatedSpawner) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.claims)
}

func (g *gatedSpawner) claimAt(idx int) *board.Claim {
	g.mu.Lock()
	defer g.mu.Unlock()
	if idx < 0 || idx >= len(g.claims) {
		return nil
	}
	return g.claims[idx]
}

// turnEvidenceStub is the #1832 WS4 stillborn guard's controllable
// source: per-session counts, positively zero when absent from the map.
type turnEvidenceStub struct {
	mu    sync.Mutex
	turns map[string]int
}

func (t *turnEvidenceStub) SessionTurnEvidence(_ context.Context, sessionID string) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.turns[sessionID], nil
}

// waitUntil polls cond until it holds or the belt expires (async spawn
// goroutines land on their own schedule).
func (s *BoardSuite) waitUntil(cond func() bool) {
	s.T().Helper()
	for i := 0; i < 200 && !cond(); i++ {
		time.Sleep(25 * time.Millisecond)
	}
	s.Require().True(cond(), "condition never held")
}

// TestPersonaSpawnRace_S1_NoMechanicalFlipWithoutWorkerMove — symptom 1
// (the "mechanical flip"): with the grant's async spawn parked
// mid-dispatch (the production first-turn latency), a full reconciler
// sweep plus a dispatch drive must NOT move the row, spawn a second
// session, or record a persona mint. The engine owns no
// in_progress→in_review writer; if one appears, this pin goes red.
func (s *BoardSuite) TestPersonaSpawnRace_S1_NoMechanicalFlipWithoutWorkerMove() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "s1-no-flip", nil, false)

	spawner := newGatedSpawner(0) // the implementer's dispatch parks (first turn in flight)
	svc := board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(spawner.Spawn),
		board.WithWorkerAborter(s.aborter.Abort),
		board.WithAsyncSpawn(),
		board.WithDriveQueue(nil),
		board.WithRoleDirectory(personaRoleDirectory()),
	)
	s.recon = board.NewReconciler(s.pool, s.cfg,
		board.WithWorkerSpawner(spawner.Spawn),
		board.WithWorkerAborter(s.aborter.Abort),
		board.WithPRDriftSource(s.drift),
		board.WithPendingInputSource(s.pending),
		board.WithFailpoint(s.fps.fp),
		board.WithAsyncSpawn(),
		board.WithRoleDirectory(personaRoleDirectory()),
	)

	c, err := svc.ClaimNext(ctx, "w-"+ws, ws)
	s.Require().NoError(err)
	s.Require().Equal(task.ID, c.TaskID)
	s.waitUntil(func() bool { return spawner.count() == 1 })

	// Everything the engine runs on cadence, while the implementer's
	// spawn is mid-dispatch: a full sweep + an explicit project drive.
	s.sweep()
	driven, err := svc.DriveProject(ctx, p.ID)
	s.Require().NoError(err)
	s.Equal(0, driven, "nothing left to grant — the row is already claimed")

	row := s.rawTask(task.ID)
	s.Equal(board.StageInProgress, row.Stage, "no mechanical flip: only a token-guarded MoveStage exits in_progress")
	s.Require().NotNil(row.ClaimToken)
	s.Equal(1, spawner.count(), "exactly ONE spawn — no duplicate task-charter dispatch")
	s.Equal(0, s.countProjectEvents(p.ID, "persona_claim"), "no persona mint without an in_review entry")
	s.Equal(0, s.countTransitions(task.ID, board.StageInReview))

	// The dispatch completes (first turn ends): the stamp lands, the row
	// stays put.
	spawner.release(0)
	s.waitUntil(func() bool {
		r := s.rawTask(task.ID)
		return r.Assignee != nil && *r.Assignee == "sess-0"
	})
	s.Equal(board.StageInProgress, s.rawTask(task.ID).Stage)
	s.Equal(1, spawner.count())
}

// TestPersonaSpawnRace_S2_ImmediateMoveRacingInFlightSpawn — probe 2's
// differential shape: the worker's →in_review MoveStage lands at "T+1s"
// while the grant's async spawn is still parked mid-dispatch. The engine
// must (a) flip + mint through the ONE token-guarded path, (b) spawn
// exactly ONE reviewer-persona session on in_review entry, (c) abort the
// parked implementer's late stamp (the double-execution guard — two
// sessions must never both hold a live claim), and (d) spawn nothing
// else.
func (s *BoardSuite) TestPersonaSpawnRace_S2_ImmediateMoveRacingInFlightSpawn() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "s2-immediate", nil, false)

	// The #1939 shipped default posture: zero stored config rows —
	// reviewColumn=auto + autoAssignReviewer=true render the default
	// reviewer persona on in_review entry.
	_, err := s.pool.Exec(ctx, `DELETE FROM board_config_defaults WHERE user_id=$1`, testUser)
	s.Require().NoError(err)

	spawner := newGatedSpawner(0) // implementer dispatch parks; the reviewer's (call 1) is ungated
	aborter := &fakeAborter{}
	svc := board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(spawner.Spawn),
		board.WithWorkerAborter(aborter.Abort),
		board.WithAsyncSpawn(),
		board.WithRoleDirectory(personaRoleDirectory()),
	)

	c, err := svc.ClaimNext(ctx, "w-"+ws, ws)
	s.Require().NoError(err)
	s.Require().Equal(task.ID, c.TaskID)
	workerAttempts := s.rawTask(task.ID).Attempts
	s.waitUntil(func() bool { return spawner.count() == 1 })
	s.Require().Nil(s.rawTask(task.ID).Assignee, "pre-fix production shape: the stamp has not landed (first turn in flight)")

	// T+1s: the immediate worker move — the ONLY legal flip path (the
	// live claim token from the charter).
	_, err = svc.MoveStage(ctx, s.agent("", c.ClaimToken), "", c.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)

	row := s.rawTask(task.ID)
	s.Equal(board.StageInReview, row.Stage)
	s.Require().NotNil(row.ClaimToken, "the persona entry minted the reviewer claim")
	s.Require().NotNil(row.LeaseExp)
	s.Equal(workerAttempts, row.Attempts, "a persona mint is re-claim-class")

	// The reviewer-persona spawn fired on in_review entry (post-commit
	// nudge), took the minted token, and stamped.
	s.waitUntil(func() bool { return spawner.count() == 2 })
	s.waitUntil(func() bool {
		r := s.rawTask(task.ID)
		return r.Assignee != nil && *r.Assignee == "sess-1"
	})
	rev := spawner.claimAt(1)
	s.Require().NotNil(rev.Persona, "the in_review entry's spawn carries the reviewer persona")
	s.Equal(reviewerRole.ID, rev.Persona.RoleID)
	s.Equal(board.StageInReview, rev.Stage)
	s.Equal(*row.ClaimToken, rev.ClaimToken, "claim-before-spawn: the charter carries the MINTED token")

	// The implementer's dispatch completes late: its stamp must LOSE
	// (token consumed by the mint) and the session must be aborted.
	spawner.release(0)
	s.waitUntil(func() bool { return len(aborter.callsSlice()) == 1 })
	s.Equal(task.ID+"/sess-0", aborter.callsSlice()[0], "the late implementer stamp is a dead charter — aborted")
	s.Equal(2, spawner.count(), "exactly two spawns: implementer + reviewer — no duplicate task-charter dispatch")
	s.Equal(1, s.countProjectEvents(p.ID, "persona_claim"))
}

// TestPersonaSpawnRace_S5_HeartbeatNotStillbornOnRearmWindow — the live
// 03:49Z finding: after the reviewer's lease lapses and the re-arm pass
// mints a FRESH claim (worker #3), a heartbeat carrying the fresh token
// must not consult the DEAD prior assignee the lapsed claim left
// recorded. rc1's in_progress reclaims clear assignee on lapse; the
// persona lapse pass must match that discipline — the stillborn guard
// reads the row's assignee, so a corpse assignee poisons every renewal
// of the fresh claimant (the observed infinite churn).
func (s *BoardSuite) TestPersonaSpawnRace_S5_HeartbeatNotStillbornOnRearmWindow() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "s5-stale-assignee", nil, false)

	spawner := newGatedSpawner(1, 2) // call 0 (implementer) free; the reviewer dispatches (calls 1 and 2) park — the mint→stamp windows stay open
	evidence := &turnEvidenceStub{turns: map[string]int{}}
	svc := board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(spawner.Spawn),
		board.WithAsyncSpawn(),
		board.WithTurnEvidenceSource(evidence),
		board.WithRoleDirectory(personaRoleDirectory()),
	)
	s.setStagePersonas(svc, p, map[string]board.PersonaConfig{"in_review": {RoleID: "reviewer"}})

	c, err := svc.ClaimNext(ctx, "w-"+ws, ws)
	s.Require().NoError(err)
	s.Require().Equal(task.ID, c.TaskID)
	s.waitUntil(func() bool {
		r := s.rawTask(task.ID)
		return r.Assignee != nil && *r.Assignee == "sess-0"
	})

	// The worker moves to in_review: the mint lands, the reviewer's
	// dispatch parks (first turn in flight) — the mint→stamp window.
	_, err = svc.MoveStage(ctx, s.agent("", c.ClaimToken), "", c.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)
	s.waitUntil(func() bool { return spawner.count() == 2 })
	minted := s.rawTask(task.ID)
	s.Require().NotNil(minted.ClaimToken)

	// The reviewer never turns; its lease lapses. #1813 accounting marks
	// the failure and the lapse must ALSO clear the dead claim's
	// recorded identity — rc1 parity (the in_progress reclaim clears
	// assignee_session; the persona lapse pass is its in_review twin).
	s.expireLease(task.ID)
	s.recon = board.NewReconciler(s.pool, s.cfg,
		board.WithWorkerSpawner(spawner.Spawn),
		board.WithWorkerAborter(s.aborter.Abort),
		board.WithPRDriftSource(s.drift),
		board.WithPendingInputSource(s.pending),
		board.WithFailpoint(s.fps.fp),
		board.WithAsyncSpawn(),
		board.WithRoleDirectory(personaRoleDirectory()),
	)
	s.sweep()
	lapsed := s.rawTask(task.ID)
	s.Require().Nil(lapsed.ClaimToken, "the lapse cleared the dead claim")

	// The re-arm mints the fresh claim (worker #3) whose dispatch parks:
	// the heartbeat-staleness window of the live finding.
	s.clearTaskBackoff(task.ID)
	s.sweep()
	s.waitUntil(func() bool { return spawner.count() == 3 })
	fresh := s.rawTask(task.ID)
	s.Require().NotNil(fresh.ClaimToken, "the re-arm minted the fresh persona claim")
	freshToken := *fresh.ClaimToken

	// The stale corpse is positively zero-turn at the evidence source —
	// exactly the live rejection shape. Item 6: the response channel is
	// TURN-BLIND and prior-episode-blind — the fresh claimant's matching
	// token is ACCEPTED (the arrival records; a dead prior assignee
	// poisons nothing), but without a nudge nothing extends.
	_, err = s.pool.Exec(ctx, `UPDATE tasks SET last_progress_at = now() - interval '1 second' WHERE id=$1`, task.ID)
	s.Require().NoError(err)
	hb, err := svc.Heartbeat(ctx, s.agent("", freshToken), "", task.ShortID, time.Now().Add(2*time.Second))
	s.Require().NoError(err)
	s.Require().False(hb.Extended, "advisory: no nudge on the row — the prior episode's zero-turn evidence is irrelevant (the wedge verdict owns corpses)")

	// The WS4 guard's heartbeat arm is gone entirely: once the fresh
	// reviewer STAMPS, its own zero-turn evidence refuses NOTHING — the
	// discriminator between corpse and live reviewer is the ladder.
	spawner.release(1)
	spawner.release(2)
	s.waitUntil(func() bool {
		r := s.rawTask(task.ID)
		return r.Assignee != nil && *r.Assignee == "sess-2"
	})
	evidence.mu.Lock()
	evidence.turns["sess-2"] = 0
	evidence.mu.Unlock()
	s.setNudge(task.ID, "nudge-race")
	hb, err = svc.Heartbeat(ctx, s.agent("", freshToken), "", task.ShortID, time.Now().Add(4*time.Second))
	s.Require().NoError(err)
	s.Require().True(hb.Extended, "the response channel is turn-blind: a nudged row's response renews")
}

// TestPersonaSpawnRace_S6_VerdictChannelHoldsReviewersToken — symptom 3's
// channel half (the live probe's finding B): the reviewer persona the
// engine spawns is a WORKER session — its ONE authorized write is the
// verdict, keyed to the persona claim it holds. An agent-surface
// RecordVerdict requires the in_review row's LIVE claim token (the
// reviewer's own); a wrong or missing token refuses, and the owner/REST
// surface keeps today's semantics.
func (s *BoardSuite) TestPersonaSpawnRace_S6_VerdictChannelHoldsReviewersToken() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "s6-verdict-channel", nil, false)

	spawner := &claimCapture{}
	svc := board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(spawner.Spawn),
		board.WithRoleDirectory(personaRoleDirectory()))
	s.setStagePersonas(svc, p, map[string]board.PersonaConfig{"in_review": {RoleID: "reviewer"}})

	c, err := svc.ClaimNext(ctx, "w-"+ws, ws)
	s.Require().NoError(err)
	_, err = svc.MoveStage(ctx, s.agent("", c.ClaimToken), "", c.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)
	row := s.rawTask(task.ID)
	s.Require().NotNil(row.ClaimToken)
	reviewerToken := *row.ClaimToken

	// The spawned reviewer's surface: agent actor holding its claim
	// token — the fail verdict applies (the bounce).
	out, v, err := svc.RecordVerdict(ctx, s.agent("", reviewerToken), "", c.ShortID, board.VerdictInput{
		Verdict:  board.VerdictFail,
		Reviewer: "spawned-reviewer",
		Findings: []board.VerdictFinding{{Severity: board.SeverityMajor, Pointer: "criterion 0: no evidence"}},
	}, "")
	s.Require().NoError(err)
	s.Require().True(v.Applied, "the reviewer's token-bearing verdict applies")
	s.Equal(board.StageInProgress, out.Stage)
	_ = out

	// Re-enter in_review for the guard's negative arms.
	_, err = svc.MoveStage(ctx, s.agent("", row2Token(s, task)), "", c.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)
	fresh := s.rawTask(task.ID)
	s.Require().NotNil(fresh.ClaimToken)

	_, _, err = svc.RecordVerdict(ctx, s.agent("", "wrong-token"), "", c.ShortID, board.VerdictInput{
		Verdict: board.VerdictFail, Reviewer: "impostor",
		Findings: []board.VerdictFinding{{Severity: board.SeverityMinor, Pointer: "x"}},
	}, "")
	s.Require().Error(err, "a wrong token never records")

	_, _, err = svc.RecordVerdict(ctx, s.agent("", ""), "", c.ShortID, board.VerdictInput{
		Verdict: board.VerdictFail, Reviewer: "tokenless",
		Findings: []board.VerdictFinding{{Severity: board.SeverityMinor, Pointer: "x"}},
	}, "")
	s.Require().Error(err, "an agent-surface verdict without a token refuses (the claim-token fence — the real in-flight protection)")
}

// row2Token fetches the row's current claim token (the bounce's mint).
func row2Token(s *BoardSuite, task *board.Task) string {
	row := s.rawTask(task.ID)
	s.Require().NotNil(row.ClaimToken)
	return *row.ClaimToken
}
