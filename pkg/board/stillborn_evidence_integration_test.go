// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// #1832 WS4 — stillborn-spawn claim hygiene (the NARROW evidence fix;
// the platform-derived liveness REWORK is #1847's, another lane). A
// spawn that mints a session but produces ZERO turns consumed a claim,
// and a lease-keeper renewed the corpse because heartbeat extension
// trusted the evidence TIMESTAMP alone. The fix: extension requires
// ACTUAL TURN EVIDENCE — when the installed source positively reports
// the assignee session at zero messages/outputs, the heartbeat does
// NOT extend (the wall clock reclaims the stillborn's lease). A seam
// ERROR keeps today's behavior (fail-open — transient infra must not
// kill live workers' renewals; the rework owns the stronger posture).

// fakeTurnSource is the turn-evidence seam fixture.
type fakeTurnSource struct {
	counts map[string]int
	err    error
}

func (f *fakeTurnSource) SessionTurnEvidence(_ context.Context, sessionID string) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.counts[sessionID], nil
}

// stillbornSvc builds a service with the turn-evidence seam installed
// (the shared suite service has none — its nil-seam behavior is pinned
// separately).
func (s *BoardSuite) stillbornSvc(ts *fakeTurnSource) *board.Service {
	return board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(s.spawner.Spawn),
		board.WithTurnEvidenceSource(ts),
	)
}

func TestStillbornEvidence_SeamSemantics(t *testing.T) {
	// The seam is a first-class option: installed services consult it
	// on every extension; nil (the default) keeps today's behavior.
	require.NotNil(t, board.WithTurnEvidenceSource(&fakeTurnSource{}), "the option installs")
}

func (s *BoardSuite) TestStillborn_ZeroTurnEvidenceRefusesExtension() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "stillborn", nil, false)

	ts := &fakeTurnSource{counts: map[string]int{}}
	svc := s.stillbornSvc(ts)
	c, err := svc.ClaimNext(context.Background(), "worker-"+ws, ws)
	s.Require().NoError(err)
	s.Require().NotNil(c.Task)
	s.Require().NotEmpty(c.Assignee, "the spawn stamp recorded the session identity")

	// The stillborn: the minted session produced ZERO messages.
	ts.counts[c.Assignee] = 0
	before := s.rawTask(task.ID)
	require.NotNil(s.T(), before.ClaimToken)

	// A heartbeat carrying FRESH evidence timestamps (exactly what a
	// lease-keeper reading spawn-moment updated_at would send) must NOT
	// extend a zero-turn assignee's lease.
	res, err := svc.Heartbeat(context.Background(), s.agent(c.Assignee, c.ClaimToken), "", c.ShortID,
		time.Now().UTC().Add(time.Minute))
	s.Require().NoError(err)
	require.False(s.T(), res.Extended, "stillborn: no turn evidence, no extension — updated_at alone is not liveness")
	require.Contains(s.T(), res.Reason, "turn evidence",
		"the refusal names the evidence class (operational debuggability)")

	// The stored watermark is untouched — the corpse consumed nothing.
	after := s.rawTask(task.ID)
	require.Equal(s.T(), before.ClaimToken, after.ClaimToken)
	require.False(s.T(), after.LeaseExp.After(*before.LeaseExp),
		"the lease was NOT renewed")
}

func (s *BoardSuite) TestStillborn_LiveSessionExtends() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	s.seedTask(e, "live", nil, false)

	ts := &fakeTurnSource{counts: map[string]int{}}
	svc := s.stillbornSvc(ts)
	c, err := svc.ClaimNext(context.Background(), "worker-"+ws, ws)
	s.Require().NoError(err)
	s.Require().NotNil(c.Task)

	ts.counts[c.Assignee] = 5 // a working session: five messages
	res, err := svc.Heartbeat(context.Background(), s.agent(c.Assignee, c.ClaimToken), "", c.ShortID,
		time.Now().UTC().Add(time.Minute))
	s.Require().NoError(err)
	require.True(s.T(), res.Extended, "real turn evidence + a moved watermark extends")
	require.Empty(s.T(), res.Reason)
}

func (s *BoardSuite) TestStillborn_SeamErrorFailsOpen() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	s.seedTask(e, "blip", nil, false)

	ts := &fakeTurnSource{err: errTurnSourceBlip}
	svc := s.stillbornSvc(ts)
	c, err := svc.ClaimNext(context.Background(), "worker-"+ws, ws)
	s.Require().NoError(err)
	s.Require().NotNil(c.Task)

	// A transient seam error must NOT turn into a lease kill for a
	// live worker: the heartbeat falls back to the timestamp rule
	// (fail-open here is deliberate — #1847's rework owns the stronger
	// posture; this lane's guard acts only on POSITIVE zero evidence).
	res, err := svc.Heartbeat(context.Background(), s.agent(c.Assignee, c.ClaimToken), "", c.ShortID,
		time.Now().UTC().Add(time.Minute))
	s.Require().NoError(err)
	require.True(s.T(), res.Extended, "seam error: the evidence-timestamp rule still extends")
}

func (s *BoardSuite) TestStillborn_NilSeamKeepsTodaySemantics() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	s.seedTask(e, "nilseam", nil, false)

	// The shared suite service installs NO seam: a fresh watermark
	// extends exactly as before (the guard is inert without a source).
	c, err := s.svc.ClaimNext(context.Background(), "worker-"+ws, ws)
	s.Require().NoError(err)
	s.Require().NotNil(c.Task)
	res, err := s.svc.Heartbeat(context.Background(), s.agent(c.Assignee, c.ClaimToken), "", c.ShortID,
		time.Now().UTC().Add(time.Minute))
	s.Require().NoError(err)
	require.True(s.T(), res.Extended)
	require.Empty(s.T(), res.Reason)
}

// errTurnSourceBlip is the transient-failure fixture.
var errTurnSourceBlip = &turnSourceError{}

type turnSourceError struct{}

func (*turnSourceError) Error() string { return "session index blip" }
