//go:build integration

package board_test

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// bounce_1959_integration_test.go — the verdict-driven bounce refusal
// (issue #1959, live evidence 2026-10-25 23:26–00:01Z on
// tsk-orient-songbird-wiggles): a fail verdict on an in_review row
// holding a live reviewer-persona claim returned
// applied:false, transition:"refused:stage_transition" while the row's
// assignee_session was NULL — the re-arm mint→stamp window. The
// reviewer persona's mint (personaClaimSQL) preserves the identity
// column as it stands; after the #1947 lapse-clear the column is NULL,
// and it stays NULL until the re-armed reviewer's ASYNC spawn-stamp
// lands (~90s live: drafty-persimmon-generates spawned 23:45:11,
// stamped 23:46:37 — its verdict fired 23:46:05, 32s INSIDE the
// window). The bounce guard read cur.AssigneeSession == "" and
// refused; a prompt reviewer ALWAYS beats its own stamp, so the
// bounce never fires.

// windowSpawner stamps the IMPLEMENTER grant normally but returns ""
// for every reviewer-persona spawn — the live mint→stamp window: the
// persona claim is minted and the reviewer session exists, but its
// assignee stamp has not landed on the row. Non-persona spawns get
// UNIQUE per-spawn ids (the fixture must distinguish the original
// implementer's session from any fallback fresh-worker spawn).
type windowSpawner struct {
	mu     sync.Mutex
	claims []*board.Claim
	n      int
}

func (w *windowSpawner) Spawn(_ context.Context, c *board.Claim) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	cp := *c
	w.claims = append(w.claims, &cp)
	if c.Persona != nil {
		return "", nil // the reviewer's spawn-stamp never lands (the window)
	}
	w.n++
	return "sess-impl-" + c.ShortID + "-" + /* #1959 unique */ fmt.Sprint(w.n), nil
}

// armRearmWindow1959 drives a task into the LIVE refusal state: an
// in_review row holding a LIVE re-armed reviewer-persona claim whose
// assignee stamp has not landed (assignee_session NULL). This is the
// drafty-persimmon-generates shape — lapse (clears the identity,
// #1947) → re-arm mint (identity stays NULL) → verdict before the
// fresh reviewer's async stamp.
func (s *BoardSuite) armRearmWindow1959(svc *board.Service, spawner *windowSpawner, task *board.Task, ws string) (implementer, reviewerToken string) {
	ctx := context.Background()
	// The reconciler drives the lapse + re-arm (its own spawn seam rides
	// the same window spawner: the re-armed reviewer never stamps).
	s.recon = board.NewReconciler(s.pool, s.cfg,
		board.WithWorkerSpawner(spawner.Spawn),
		board.WithWorkerAborter(s.aborter.Abort),
		board.WithPRDriftSource(s.drift),
		board.WithPendingInputSource(s.pending),
		board.WithFailpoint(s.fps.fp),
	)

	c, err := svc.ClaimNext(ctx, "w-"+ws, ws)
	s.Require().NoError(err)
	implementer = c.Assignee
	s.Require().NotEmpty(implementer, "the implementer grant is stamped")

	// Forward exit → in_review: the reviewer persona mint (spawn #2,
	// unstamped — its lease will lapse).
	_, err = svc.MoveStage(ctx, s.agent(implementer, c.ClaimToken), "", c.ShortID, board.StageInReview, "file:///ws/bounce-final.txt", "")
	s.Require().NoError(err)

	// The reviewer dies still in the window (no stamp, no heartbeat):
	// the #1947 lapse pass clears claim AND assignee identity, parks
	// the row behind backoff.
	s.expireLease(task.ID)
	s.sweep()
	s.Require().Nil(s.rawTask(task.ID).Assignee, "the lapse cleared the dead reviewer's identity")

	// Re-arm: a FRESH persona claim is minted (identity column stays
	// NULL — the mint preserves it as it stands) and the fresh reviewer
	// spawns unstamped. The row now sits exactly where the live probe
	// sat: live reviewer claim, verdict imminent, assignee NULL.
	s.clearTaskBackoff(task.ID)
	s.sweep()
	row := s.rawTask(task.ID)
	s.Require().NotNil(row.ClaimToken, "the re-arm minted a fresh reviewer claim")
	s.Require().Nil(row.Assignee, "the re-arm mint→stamp window: the fresh reviewer's stamp has not landed")
	return implementer, *row.ClaimToken
}

// TestBounce1959_VerdictFiresInsideMintStampWindow — the core defect,
// born RED: a fail verdict on a re-armed reviewer-claimed in_review row
// whose assignee stamp is still in flight must FIRE the bounce
// (in_review→in_progress, fresh claim minted for the ORIGINAL
// implementer, attempts unchanged). Today it returns the live refusal
// (applied:false, refused:stage_transition) — the exact tool evidence
// from cloven-loggia-snorts and drafty-persimmon-generates.
func (s *BoardSuite) TestBounce1959_VerdictFiresInsideMintStampWindow() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "bounce-window", nil, false)

	spawner := &windowSpawner{}
	svc := board.NewService(s.pool, s.cfg, board.WithWorkerSpawner(spawner.Spawn))
	s.setStagePersonas(svc, p, map[string]board.PersonaConfig{"in_review": {Name: "reviewer"}})

	implementer, reviewerToken := s.armRearmWindow1959(svc, spawner, task, ws)
	_ = implementer // the no-seam arm below exercises the fresh-worker fallback, not the re-dispatch target
	attempts := s.rawTask(task.ID).Attempts

	// The reviewer's fail verdict (live shape: agent surface, the row's
	// live claim token) — inside the window.
	out, v, err := svc.RecordVerdict(ctx, s.agent("reviewer-session", reviewerToken), "", task.ShortID, board.VerdictInput{
		Verdict:  board.VerdictFail,
		Reviewer: "reviewer",
		Findings: []board.VerdictFinding{{
			Severity:       board.SeverityBlocker,
			Pointer:        "/workspace/bounce-final.txt:1 contains 'TOKEN-INITIAL'; the criterion requires exactly 'TOKEN-FINAL'",
			CriterionIndex: intPtr1959(0),
		}},
	}, "")
	s.Require().NoError(err)

	// #1959 fix shape: the bounce FIRES (today, born RED: the live
	// refusal was applied:false, transition "refused:stage_transition").
	s.Require().True(v.Applied, "the fail verdict must fire the bounce (live refusal: applied:false transition=%q)", v.Transition)
	s.Require().Equal(board.StageInProgress, out.Stage)

	bounced := s.rawTask(task.ID)
	s.Require().NotNil(bounced.ClaimToken, "a fresh claim was minted")
	s.Require().NotEqual(reviewerToken, *bounced.ClaimToken, "the reviewer's token died with its verdict")
	s.Require().NotNil(bounced.Assignee, "the bounced row records a live identity (never a token-less in_progress row)")
	s.Require().Equal(attempts, bounced.Attempts, "a bounce is an iteration inside the verdict chain, never an attempt")
}

// TestBounce1959_ManualBounceInsideMintStampWindow — the manual
// in_review→in_progress move with a valid reviewer claim token refused
// with the same guard live (cloven-loggia-snorts 23:27:50Z: 409
// "bounce requires a recorded assignee"); the same guard arms both
// paths, and both must fire inside the window.
func (s *BoardSuite) TestBounce1959_ManualBounceInsideMintStampWindow() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "bounce-manual", nil, false)

	spawner := &windowSpawner{}
	svc := board.NewService(s.pool, s.cfg, board.WithWorkerSpawner(spawner.Spawn))
	s.setStagePersonas(svc, p, map[string]board.PersonaConfig{"in_review": {Name: "reviewer"}})

	implementer, reviewerToken := s.armRearmWindow1959(svc, spawner, task, ws)
	_ = implementer // the no-seam arm below exercises the fresh-worker fallback, not the re-dispatch target

	out, err := svc.MoveStage(ctx, s.agent("reviewer-session", reviewerToken), "", task.ShortID, board.StageInProgress, "", "")
	s.Require().NoError(err, "the manual review bounce must fire inside the window (live: 409 'bounce requires a recorded assignee')")
	s.Require().Equal(board.StageInProgress, out.Stage)
	bounced := s.rawTask(task.ID)
	s.Require().NotNil(bounced.Assignee, "the manual bounce records a live identity too (no redispatch seam here: the fresh-worker fallback stamps)")
}

// redispatchCapture records the #1959 re-dispatch deliveries (the
// existing-session seam) and optionally fails them.
type redispatchCapture struct {
	mu       sync.Mutex
	to       []string
	texts    []string
	failWith error
}

func (r *redispatchCapture) Deliver(_ context.Context, sessionID, text, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.to = append(r.to, sessionID)
	r.texts = append(r.texts, text)
	if r.failWith != nil {
		return r.failWith
	}
	return nil
}

// turnsEvidence is a deterministic TurnEvidenceSource for the
// never-turned fallback pin.
type turnsEvidence struct{ turns int }

func (t turnsEvidence) SessionTurnEvidence(context.Context, string) (int, error) { return t.turns, nil }

// TestBounce1959_RedispatchReachesOriginalImplementer — the ruling's
// bounce-target semantics: the re-dispatch turn lands in the ORIGINAL
// implementer's session (the assignee that moved the row to
// in_review), carrying the fresh bounce claim token AND the embedded
// verdict chain (findings as iteration history). No fresh spawn fires.
func (s *BoardSuite) TestBounce1959_RedispatchReachesOriginalImplementer() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "bounce-redispatch", nil, false)

	spawner := &windowSpawner{}
	redispatch := &redispatchCapture{}
	svc := board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(spawner.Spawn),
		board.WithBounceRedispatcher(redispatch.Deliver),
		board.WithTurnEvidenceSource(turnsEvidence{turns: 3}),
	)
	s.setStagePersonas(svc, p, map[string]board.PersonaConfig{"in_review": {Name: "reviewer"}})

	implementer, reviewerToken := s.armRearmWindow1959(svc, spawner, task, ws)
	spawnsBefore := len(spawner.claims)

	_, v, err := svc.RecordVerdict(ctx, s.agent("reviewer-session", reviewerToken), "", task.ShortID, board.VerdictInput{
		Verdict:  board.VerdictFail,
		Reviewer: "rev-1959",
		Findings: []board.VerdictFinding{{Severity: board.SeverityBlocker, Pointer: "/workspace/bounce-final.txt:1 contains 'TOKEN-INITIAL'", CriterionIndex: intPtr1959(0)}},
	}, "")
	s.Require().NoError(err)
	s.Require().True(v.Applied)

	s.Require().Len(redispatch.to, 1, "exactly one re-dispatch delivery")
	s.Require().Equal(implementer, redispatch.to[0], "the re-dispatch reaches the ORIGINAL implementer's session")
	freshToken := s.rawTask(task.ID).ClaimToken
	s.Require().NotNil(freshToken)
	s.Require().Contains(redispatch.texts[0], *freshToken, "the re-dispatch carries the FRESH bounce claim token")
	s.Require().Contains(redispatch.texts[0], "TOKEN-INITIAL", "the re-dispatch embeds the verdict chain's findings")
	s.Require().Contains(redispatch.texts[0], task.ShortID, "the re-dispatch identifies the task")
	// The mint stamps the implementer IN the statement (never a
	// token-less in_progress row — and never the fallback's fresh id,
	// which would mean the re-dispatch arm silently degraded).
	stamped := s.rawTask(task.ID).Assignee
	s.Require().NotNil(stamped)
	s.Require().Equal(implementer, *stamped, "the bounce mint stamps the ORIGINAL implementer as the row's identity")
	s.Require().Len(spawner.claims, spawnsBefore, "no fresh spawn when the original implementer is live")
}

// TestBounce1959_FallbackOnDeadOriginal — the ruling's fallback: a
// fresh-implementer spawn fires ONLY when the original session is
// gone/unresponsive. Two arms pinned: never-turned (zero turn
// evidence) and delivery failure (the seam errors — archived/reaped).
func (s *BoardSuite) TestBounce1959_FallbackOnDeadOriginal() {
	ctx := context.Background()

	// Arm 1: never-turned original — the re-dispatch is skipped, a
	// fresh worker spawns for the minted claim.
	s.Run("never-turned", func() {
		ws := s.seedWorkspace()
		p := s.seedProject(ws, 2)
		e := s.seedEpic(p, board.EpicActive)
		task := s.seedTask(e, "bounce-dead1", nil, false)

		spawner := &windowSpawner{}
		redispatch := &redispatchCapture{}
		svc := board.NewService(s.pool, s.cfg,
			board.WithWorkerSpawner(spawner.Spawn),
			board.WithBounceRedispatcher(redispatch.Deliver),
			board.WithTurnEvidenceSource(turnsEvidence{turns: 0}), // never-turned
		)
		s.setStagePersonas(svc, p, map[string]board.PersonaConfig{"in_review": {Name: "reviewer"}})
		_, reviewerToken := s.armRearmWindow1959(svc, spawner, task, ws)
		spawnsBefore := len(spawner.claims)

		_, v, err := svc.RecordVerdict(ctx, s.agent("reviewer-session", reviewerToken), "", task.ShortID, board.VerdictInput{
			Verdict: board.VerdictFail, Reviewer: "rev-dead1",
			Findings: []board.VerdictFinding{{Severity: board.SeverityMajor, Pointer: "criterion 0: no evidence"}},
		}, "")
		s.Require().NoError(err)
		s.Require().True(v.Applied)
		s.Require().Empty(redispatch.to, "a never-turned original is never messaged")
		s.Require().Len(spawner.claims, spawnsBefore+1, "the fallback spawns a FRESH implementer")
		fresh := spawner.claims[len(spawner.claims)-1]
		s.Require().Nil(fresh.Persona, "the fallback is the implementer class (no reviewer persona)")
		s.Require().Equal(board.StageInProgress, fresh.Stage)
		rowAfter := s.rawTask(task.ID)
		s.Require().NotNil(rowAfter.ClaimToken)
		s.Require().Equal(*rowAfter.ClaimToken, fresh.ClaimToken, "the fresh worker receives the bounce's minted token")
	})

	// Arm 2: delivery failure (archived/reaped) — the fallback spawns.
	s.Run("delivery-failure", func() {
		ws := s.seedWorkspace()
		p := s.seedProject(ws, 2)
		e := s.seedEpic(p, board.EpicActive)
		task := s.seedTask(e, "bounce-dead2", nil, false)

		spawner := &windowSpawner{}
		redispatch := &redispatchCapture{failWith: errors.New("session gone")}
		svc := board.NewService(s.pool, s.cfg,
			board.WithWorkerSpawner(spawner.Spawn),
			board.WithBounceRedispatcher(redispatch.Deliver),
			board.WithTurnEvidenceSource(turnsEvidence{turns: 3}),
		)
		s.setStagePersonas(svc, p, map[string]board.PersonaConfig{"in_review": {Name: "reviewer"}})
		_, reviewerToken := s.armRearmWindow1959(svc, spawner, task, ws)
		spawnsBefore := len(spawner.claims)

		_, v, err := svc.RecordVerdict(ctx, s.agent("reviewer-session", reviewerToken), "", task.ShortID, board.VerdictInput{
			Verdict: board.VerdictFail, Reviewer: "rev-dead2",
			Findings: []board.VerdictFinding{{Severity: board.SeverityMajor, Pointer: "criterion 0: no evidence"}},
		}, "")
		s.Require().NoError(err)
		s.Require().True(v.Applied)
		s.Require().Len(redispatch.to, 1, "delivery was attempted")
		s.Require().Len(spawner.claims, spawnsBefore+1, "the failed delivery falls back to a FRESH implementer spawn")
	})
}

// TestBounce1959_NoIdentityFallsBackToFreshSpawn — a bounce with NO
// recorded implementer (pre-migration shape: identity never captured)
// no longer refuses (today's guard): it mints for a FRESH implementer.
func (s *BoardSuite) TestBounce1959_NoIdentityFallsBackToFreshSpawn() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "bounce-noid", nil, false)

	spawner := &windowSpawner{}
	svc := board.NewService(s.pool, s.cfg, board.WithWorkerSpawner(spawner.Spawn))
	s.setStagePersonas(svc, p, map[string]board.PersonaConfig{"in_review": {Name: "reviewer"}})

	// Simulate the pre-migration row: in_review, live persona claim,
	// BOTH identity columns empty (the migration backfill missed it —
	// e.g. the persona claim was minted before the column existed).
	_, reviewerToken := s.armRearmWindow1959(svc, spawner, task, ws)
	_, err := s.pool.Exec(ctx,
		`UPDATE tasks SET implementer_session=NULL, assignee_session=NULL WHERE id=$1`, task.ID)
	s.Require().NoError(err)
	spawnsBefore := len(spawner.claims)

	out, v, err := svc.RecordVerdict(ctx, s.agent("reviewer-session", reviewerToken), "", task.ShortID, board.VerdictInput{
		Verdict: board.VerdictFail, Reviewer: "rev-noid",
		Findings: []board.VerdictFinding{{Severity: board.SeverityMajor, Pointer: "criterion 0: no evidence"}},
	}, "")
	s.Require().NoError(err)
	s.Require().True(v.Applied, "no-identity bounces fall back to a fresh spawn — never the live refusal")
	s.Require().Equal(board.StageInProgress, out.Stage)
	s.Require().Len(spawner.claims, spawnsBefore+1, "the fallback spawns a FRESH implementer")
}

func intPtr1959(i int) *int { return &i }
