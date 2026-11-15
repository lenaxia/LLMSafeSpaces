// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

// directive_delivery_test.go — #2046 directive delivery discipline:
// every halt/resume/deposed cascade writes an auditable board_events
// row (action "directive") carrying the delivery outcome, the fire-and-
// forget `_ =` drops are gone, and pause/resume NEVER fails on an
// unreachable worker. Born RED against the plain-string messenger seam
// (no audit rows, dropped outcomes).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// directiveCall is one recorded messenger delivery: the STRUCTURED
// directive (#2046 — kind + task + the audit id that keys its event).
type directiveCall struct {
	session string
	d       board.Directive
}

// recordingDirectiveMessenger records structured directives; err, when
// set, is returned for every delivery (the unreachable-worker path).
// Mutex-guarded: cascades deliver with bounded concurrency.
type recordingDirectiveMessenger struct {
	mu    sync.Mutex
	calls []directiveCall
	err   error
}

func (f *recordingDirectiveMessenger) Deliver(_ context.Context, sessionID string, d board.Directive) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, directiveCall{session: sessionID, d: d})
	return f.err
}

// directiveEventRow reads one directive audit event's payload.
type directiveEventRow struct {
	ID      string `db:"id"`
	ItemID  string `db:"item_id"`
	Action  string `db:"action"`
	Payload []byte `db:"payload"`
}

func (s *BoardSuite) directiveEvents(taskID string) []directiveEventRow {
	rows, err := s.pool.Query(context.Background(),
		`SELECT id, item_id, action, payload FROM board_events
		 WHERE action='directive' AND item_id=$1 ORDER BY created_at, id`, taskID)
	s.Require().NoError(err)
	defer rows.Close()
	var out []directiveEventRow
	for rows.Next() {
		var r directiveEventRow
		s.Require().NoError(rows.Scan(&r.ID, &r.ItemID, &r.Action, &r.Payload))
		out = append(out, r)
	}
	s.Require().NoError(rows.Err())
	return out
}

// KILL-TEST 2046-a (task pause/resume): the halt and the resume each
// deliver ONE structured directive and write ONE audit event carrying
// the delivery outcome; a FAILING messenger WARNs and records outcome
// "failed" — and the pause/resume itself still succeeds (the flags are
// the authority; delivery is best-effort, never a blocker).
func (s *BoardSuite) TestKill_2046_TaskDirectiveAuditTrail() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "directed", nil, false)
	msg := &recordingDirectiveMessenger{}
	s.svc = board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(s.spawner.Spawn),
		board.WithWorkerAborter(s.aborter.Abort),
		board.WithWorkerMessenger(msg.Deliver),
	)

	c := s.claimOrFail(ws)
	s.Require().Equal(t1.ID, c.TaskID)
	require.Len(s.T(), msg.calls, 0, "claims deliver no directives")

	// Pause in-flight: halt delivered + audited as dispatched.
	_, err := s.svc.PauseTask(context.Background(), s.owner(), t1.ID, "", "")
	s.Require().NoError(err)
	require.Len(s.T(), msg.calls, 1)
	s.Equal("sess-"+t1.ShortID, msg.calls[0].session)
	s.Equal(board.DirectiveHalt, msg.calls[0].d.Kind)
	s.Equal(t1.ShortID, msg.calls[0].d.TaskShortID)
	s.NotEmpty(msg.calls[0].d.AuditID, "the directive carries the audit id that keys its event")

	events := s.directiveEvents(t1.ID)
	require.Len(s.T(), events, 1, "the halt cascade writes exactly one audit event")
	var pl map[string]any
	s.Require().NoError(json.Unmarshal(events[0].Payload, &pl))
	s.Equal(board.DirectiveHalt, pl["directive"])
	s.Equal(t1.ShortID, pl["short_id"])
	s.Equal("sess-"+t1.ShortID, pl["session"])
	s.Equal("dispatched", pl["outcome"], "a nil messenger return is a dispatch")
	s.Equal(fmt.Sprintf("directive:%s:%s", t1.ID, msg.calls[0].d.AuditID), events[0].ID,
		"the event id is sentinel-keyed on task + audit id")

	// Resume in-flight: resume delivered + audited.
	_, err = s.svc.ResumeTask(context.Background(), s.owner(), t1.ID, "", "")
	s.Require().NoError(err)
	require.Len(s.T(), msg.calls, 2)
	s.Equal(board.DirectiveResume, msg.calls[1].d.Kind)
	events = s.directiveEvents(t1.ID)
	require.Len(s.T(), events, 2)
	s.Require().NoError(json.Unmarshal(events[1].Payload, &pl))
	s.Equal(board.DirectiveResume, pl["directive"])
	s.Equal("dispatched", pl["outcome"])
	require.NotEqual(s.T(), events[0].ID, events[1].ID, "each delivery keys its own event")
}

// KILL-TEST 2046-b (delivery failure): an unreachable worker (messenger
// error) must WARN — never fail the pause — and the audit event carries
// outcome "failed" with the error surfaced in the payload.
func (s *BoardSuite) TestKill_2046_DirectiveFailureWarnsAndAudits() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "unreachable", nil, false)
	msg := &recordingDirectiveMessenger{err: errors.New("v1 send 502: pod activating")}
	s.svc = board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(s.spawner.Spawn),
		board.WithWorkerAborter(s.aborter.Abort),
		board.WithWorkerMessenger(msg.Deliver),
	)

	c := s.claimOrFail(ws)
	s.Require().Equal(t1.ID, c.TaskID)

	out, err := s.svc.PauseTask(context.Background(), s.owner(), t1.ID, "", "")
	s.Require().NoError(err, "pause NEVER fails on an unreachable worker")
	s.True(out.Paused, "the flag is the authority")
	require.Len(s.T(), msg.calls, 1)

	events := s.directiveEvents(t1.ID)
	require.Len(s.T(), events, 1)
	var pl map[string]any
	s.Require().NoError(json.Unmarshal(events[0].Payload, &pl))
	s.Equal("failed", pl["outcome"])
	s.Contains(pl["error"], "502", "the failure surfaces in the event payload")
}

// KILL-TEST 2046-c (project cascade): the project lever cascades to
// EVERY in-flight worker with one audit event per delivery, and the
// resume cascade keys its own fresh events.
func (s *BoardSuite) TestKill_2046_ProjectCascadeAudit() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "one", nil, false)
	t2 := s.seedTask(e, "two", nil, false)
	msg := &recordingDirectiveMessenger{}
	s.svc = board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(s.spawner.Spawn),
		board.WithWorkerAborter(s.aborter.Abort),
		board.WithWorkerMessenger(msg.Deliver),
	)

	c1 := s.claimOrFail(ws)
	c2 := s.claimOrFail(ws)
	s.Require().Equal(t1.ID, c1.TaskID)
	s.Require().Equal(t2.ID, c2.TaskID)

	_, err := s.svc.PauseProject(context.Background(), s.owner(), p.ID, "", "")
	s.Require().NoError(err)
	require.Len(s.T(), msg.calls, 2, "the halt cascades to both in-flight workers")
	s.Equal(2, s.countEvents("directive"), "one audit event per cascaded delivery")

	_, err = s.svc.ResumeProject(context.Background(), s.owner(), p.ID, "", "")
	s.Require().NoError(err)
	require.Len(s.T(), msg.calls, 4)
	s.Equal(4, s.countEvents("directive"), "the resume cascade writes its own events")

	// Every event id is unique (fresh audit id per delivery) and
	// sentinel-keyed on its task.
	for _, ev := range s.directiveEvents(t1.ID) {
		s.Require().Contains(ev.ID, "directive:"+t1.ID+":")
	}
	require.Len(s.T(), s.directiveEvents(t1.ID), 2)
	require.Len(s.T(), s.directiveEvents(t2.ID), 2)
}

// KILL-TEST 2046-d (epic cascade + deposed): the epic pause lever
// audits like the project lever; the DEPOSED directive (the S5
// salvage-handoff notice) rides the same audited seam.
func (s *BoardSuite) TestKill_2046_EpicAndDeposedAudit() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "epic-owned", nil, false)
	msg := &recordingDirectiveMessenger{}
	s.svc = board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(s.spawner.Spawn),
		board.WithWorkerAborter(s.aborter.Abort),
		board.WithWorkerMessenger(msg.Deliver),
	)

	c := s.claimOrFail(ws)
	s.Require().Equal(t1.ID, c.TaskID)

	_, err := s.svc.EpicPause(context.Background(), s.owner(), e.ID, "", "")
	s.Require().NoError(err)
	require.Len(s.T(), msg.calls, 1, "the epic halt cascades to the in-flight worker")
	s.Equal(board.DirectiveHalt, msg.calls[0].d.Kind)
	require.Len(s.T(), s.directiveEvents(t1.ID), 1, "the epic cascade audits per delivery")

	// Depose the lapsed-claim-shaped row (fixture: expire the lease —
	// the live-lease guard refuses otherwise; no lives seam is wired,
	// so the wall-clock lapse is the evidence): the stale session gets
	// the DEPOSED directive, audited on the same seam.
	_, err = s.pool.Exec(context.Background(),
		`UPDATE tasks SET lease_expires_at = now() - interval '10 minutes' WHERE id=$1`, t1.ID)
	s.Require().NoError(err)
	_, err = s.svc.DeposeTask(context.Background(), s.owner(), t1.ID, "",
		board.SalvagePointer{Branch: "feat/x", State: "mid-work"}, "")
	s.Require().NoError(err)
	var sawDeposed bool
	for _, call := range msg.calls {
		if call.d.Kind == board.DirectiveDeposed {
			sawDeposed = true
			s.Equal("sess-"+t1.ShortID, call.session)
		}
	}
	s.Require().True(sawDeposed, "the deposed session receives the DEPOSED directive")
	var deposedRows int
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM board_events WHERE action='directive'
		 AND payload->>'directive'='deposed' AND item_id=$1`, t1.ID).Scan(&deposedRows))
	s.Equal(1, deposedRows, "the deposed delivery writes its audit event")
}

// KILL-TEST 2046-e (idempotent event ids): the sentinel-keyed event id
// is inert under replay — a re-run of the same delivery's write is a
// no-op, mirroring the nudge event's idempotency.
func (s *BoardSuite) TestKill_2046_DirectiveEventIdempotent() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "idem", nil, false)
	msg := &recordingDirectiveMessenger{}
	s.svc = board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(s.spawner.Spawn),
		board.WithWorkerAborter(s.aborter.Abort),
		board.WithWorkerMessenger(msg.Deliver),
	)

	c := s.claimOrFail(ws)
	s.Require().Equal(t1.ID, c.TaskID)
	_, err := s.svc.PauseTask(context.Background(), s.owner(), t1.ID, "", "")
	s.Require().NoError(err)
	require.Len(s.T(), msg.calls, 1)

	// Re-pausing an already-paused row is a no-op (no second cascade —
	// the existing idempotence), and the audit id from the first
	// delivery replays inertly: re-writing the same event id must not
	// double-row (the helper's ON CONFLICT contract).
	_, err = s.svc.PauseTask(context.Background(), s.owner(), t1.ID, "", "")
	s.Require().NoError(err)
	require.Len(s.T(), msg.calls, 1, "already-paused short-circuits (no re-delivery)")
	require.Len(s.T(), s.directiveEvents(t1.ID), 1)
}
