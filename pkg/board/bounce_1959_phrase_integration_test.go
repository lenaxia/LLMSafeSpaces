//go:build integration

package board_test

import (
	"context"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// bounce_1959_phrase_integration_test.go — the #1959c ENGINE-leg pins
// (review r1 on PR #1967, F1 branch (A)): the bounce path's TWO raw-keyed
// consumers — the turn-evidence pre-check (stage.go, consulted BEFORE the
// delivery seam) and the re-dispatch delivery itself — must both receive
// the RAW harness id when the stored implementer identity is the platform
// PHRASE (session_index resolves phrase→raw; opencode routes and the
// session_index session_id column are raw-keyed). Unresolved upstream,
// a phrase reads "never indexed = never turned" at the evidence arm and
// the bounce falls back to a fresh spawn WITHOUT the delivery seam ever
// running — the ordering defect this pins (born RED against the seam-only
// fix; green with the engine-side resolution).
//
// Runs in the secrets-integration workflow's kill-tests step (real
// Postgres, migrated schema). Born-RED evidence for this file is CI-side
// by construction (no local DB in the lane environment; the worklog
// records the expectation verbatim).

// recordingEvidence records every consulted id and answers from a map
// (positively zero when absent — the production boardTurnEvidence shape:
// a phrase id no registry row matches reads 0).
type recordingEvidence struct {
	mu    chan struct{}
	seen  []string
	turns map[string]int
}

func newRecordingEvidence(turns map[string]int) *recordingEvidence {
	return &recordingEvidence{mu: make(chan struct{}, 1), turns: turns}
}

func (r *recordingEvidence) SessionTurnEvidence(_ context.Context, sessionID string) (int, error) {
	r.mu <- struct{}{}
	r.seen = append(r.seen, sessionID)
	<-r.mu
	return r.turns[sessionID], nil
}

// TestBounce1959Phrase_EvidenceAndDeliveryReceiveRawID — the decisive
// engine leg: a phrase-identified implementer with a LIVE registry row
// (message_count > 0) must reach the ORIGINAL implementer's raw id — the
// evidence consult keyed on the raw id, the re-dispatch delivered to the
// raw id — and no fresh spawn may fire.
func (s *BoardSuite) TestBounce1959Phrase_EvidenceAndDeliveryReceiveRawID() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "bounce-phrase", nil, false)

	// The phrase→raw registry binding the engine must consult: the
	// ORIGINAL implementer's session, live and turned (message_count 2).
	phrase := "florid-teapot-verifies"
	rawImpl := "ses_phrase_impl_1959c"
	_, err := s.pool.Exec(ctx,
		`INSERT INTO session_index (workspace_id, session_id, short_id, message_count)
		 VALUES ($1::uuid, $2, $3, 2)`, ws, rawImpl, phrase)
	s.Require().NoError(err)
	t := s.T()
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(),
			`DELETE FROM session_index WHERE workspace_id=$1::uuid AND session_id=$2`, ws, rawImpl)
	})

	spawner := &windowSpawner{}
	redispatch := &redispatchCapture{}
	evidence := newRecordingEvidence(map[string]int{rawImpl: 2}) // phrase absent → 0 (the raw-keyed shape)
	svc := board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(spawner.Spawn),
		board.WithBounceRedispatcher(redispatch.Deliver),
		board.WithTurnEvidenceSource(evidence),
		board.WithRoleDirectory(personaRoleDirectory()),
	)
	s.setStagePersonas(svc, p, map[string]board.PersonaConfig{"in_review": {RoleID: "reviewer"}})

	_, reviewerToken := s.armRearmWindow1959(svc, spawner, task, ws)
	spawnsBefore := len(spawner.claims)

	// The stored identity takes the phrase form (the live 0.44.3
	// indictment: whatever writer placed it, the row HOLDS a phrase).
	_, err = s.pool.Exec(ctx,
		`UPDATE tasks SET implementer_session=$2 WHERE id=$1`, task.ID, phrase)
	s.Require().NoError(err)

	_, v, err := svc.RecordVerdict(ctx, s.agent("reviewer-session", reviewerToken), "", task.ShortID, board.VerdictInput{
		Verdict:  board.VerdictFail,
		Reviewer: "rev-1959c-phrase",
		Findings: []board.VerdictFinding{{Severity: board.SeverityMajor, Pointer: "criterion 0: no evidence"}},
	}, "")
	s.Require().NoError(err)
	s.Require().True(v.Applied)

	s.Require().Len(evidence.seen, 1, "the evidence consult must run exactly once")
	s.Require().Equal(rawImpl, evidence.seen[0],
		"the turn-evidence consult must receive the RAW id — a phrase reads never-turned and falls back before delivery")
	s.Require().Len(redispatch.to, 1, "the re-dispatch must fire (no fallback)")
	s.Require().Equal(rawImpl, redispatch.to[0],
		"the re-dispatch must be delivered to the ORIGINAL implementer's RAW id")
	s.Require().Len(spawner.claims, spawnsBefore, "no fresh spawn when the phrase-addressed original is live")
}

// TestBounce1959Phrase_UnresolvablePhraseFallsBack — a phrase NO live
// session holds keeps the stored ref; the evidence consult's honest zero
// (never-turned) drives the fresh-worker fallback — the degraded posture
// preserved (a dead original is a dead original, whatever form names it).
func (s *BoardSuite) TestBounce1959Phrase_UnresolvablePhraseFallsBack() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "bounce-phrase-dead", nil, false)

	spawner := &windowSpawner{}
	redispatch := &redispatchCapture{}
	evidence := newRecordingEvidence(map[string]int{})
	svc := board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(spawner.Spawn),
		board.WithBounceRedispatcher(redispatch.Deliver),
		board.WithTurnEvidenceSource(evidence),
		board.WithRoleDirectory(personaRoleDirectory()),
	)
	s.setStagePersonas(svc, p, map[string]board.PersonaConfig{"in_review": {RoleID: "reviewer"}})

	_, reviewerToken := s.armRearmWindow1959(svc, spawner, task, ws)
	spawnsBefore := len(spawner.claims)

	_, err := s.pool.Exec(ctx,
		`UPDATE tasks SET implementer_session=$2 WHERE id=$1`, task.ID, "reaped-lantern-hushes")
	s.Require().NoError(err)

	_, v, err := svc.RecordVerdict(ctx, s.agent("reviewer-session", reviewerToken), "", task.ShortID, board.VerdictInput{
		Verdict:  board.VerdictFail,
		Reviewer: "rev-1959c-dead",
		Findings: []board.VerdictFinding{{Severity: board.SeverityMajor, Pointer: "criterion 0: no evidence"}},
	}, "")
	s.Require().NoError(err)
	s.Require().True(v.Applied)

	s.Require().Empty(redispatch.to, "an unresolvable phrase is never messaged")
	s.Require().Len(spawner.claims, spawnsBefore+1, "the honest zero drives the fresh-worker fallback")
}
