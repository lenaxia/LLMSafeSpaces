// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

// event_publisher_test.go — red-first pins for the §4.6 board.event
// SSE publisher seam (#1699 gap 4): board events fan out post-commit to
// an installed EventPublisher, on BOTH write paths — the user-action
// eventWriter and the engine/reconciler insertEventTx sites. RED until
// the call sites publish.

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// eventCapture records published events (publishCommitted is
// synchronous; the mutex is the belt for -race).
type eventCapture struct {
	mu     sync.Mutex
	events []board.Event
}

func (c *eventCapture) record(evt board.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, evt)
}

func (c *eventCapture) snapshot() []board.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]board.Event, len(c.events))
	copy(out, c.events)
	return out
}

func (c *eventCapture) actions() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	acts := make([]string, 0, len(c.events))
	for _, e := range c.events {
		acts = append(acts, e.Action)
	}
	return acts
}

// TestPublisher_UserActionsPersistThenPublish: a user mutation commits
// and THEN publishes exactly one event carrying the board_events row
// (action, ids, actor, payload, created_at — the §4.6 payload shape).
func (s *BoardSuite) TestPublisher_UserActionsPersistThenPublish() {
	ctx := context.Background()
	cap := &eventCapture{}
	s.svc.SetEventPublisher(cap.record)

	ws := s.seedWorkspace()
	p := s.seedProject(ws, 0)
	e := s.seedEpic(p, board.EpicActive)

	before := len(cap.snapshot())
	created, err := s.svc.CreateTask(ctx, s.owner(), e.ID, "",
		board.TaskInput{Title: "published work", Tags: []string{}}, "pub-task-1")
	require.NoError(s.T(), err)

	events := cap.snapshot()
	require.Len(s.T(), events, before+1, "exactly one publish per committed action")
	evt := events[len(events)-1]
	assert.Equal(s.T(), "create_task", evt.Action)
	assert.Equal(s.T(), p.ID, evt.ProjectID)
	assert.Equal(s.T(), "task", evt.ItemKind)
	assert.Equal(s.T(), created.ID, evt.ItemID)
	assert.False(s.T(), evt.CreatedAt.IsZero(), "created_at rides the row")
	assert.NotEmpty(s.T(), evt.ID, "the board_events id (requestId key) rides the row")
	var actor map[string]string
	require.NoError(s.T(), json.Unmarshal(evt.Actor, &actor))
	assert.Equal(s.T(), board.ActorUser, actor["kind"])
	assert.Equal(s.T(), testUser, actor["user_id"])
	assert.NotEmpty(s.T(), evt.Payload)
}

// TestPublisher_NoPublishOnAbort: a fault between effect and marker
// aborts effect AND publish together — the SSE projection never sees a
// state the database does not hold (§4.6 i, invariant-4 seam). The
// fixture seeds BEFORE the publisher installs so the assertion sees
// only the aborted action's would-be event.
func (s *BoardSuite) TestPublisher_NoPublishOnAbort() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 0)
	e := s.seedEpic(p, board.EpicActive)

	cap := &eventCapture{}
	s.svc.SetEventPublisher(cap.record)
	s.fps.armAt("create_task.effect", assertErr("publisher abort fixture"))

	_, err := s.svc.CreateTask(context.Background(), s.owner(), e.ID, "",
		board.TaskInput{Title: "aborted work", Tags: []string{}}, "")
	require.Error(s.T(), err)
	assert.Empty(s.T(), cap.snapshot(), "an aborted transaction publishes nothing")
} // TestPublisher_NoRepublishOnReplay: a replayed requestId renders the
// original outcome (200-with-original) and does NOT re-publish — the
// dedupe scope is the same (project_id, id) uniqueness the event row
// enforces.
func (s *BoardSuite) TestPublisher_NoRepublishOnReplay() {
	ctx := context.Background()
	cap := &eventCapture{}
	s.svc.SetEventPublisher(cap.record)

	ws := s.seedWorkspace()
	p := s.seedProject(ws, 0)

	const key = "pub-replay-1"
	_, err := s.svc.ArchiveProject(ctx, s.owner(), p.ID, key)
	require.NoError(s.T(), err)
	_, err = s.svc.UnarchiveProject(ctx, s.owner(), p.ID, "pub-replay-2")
	require.NoError(s.T(), err)

	// Replay the archive with the SAME requestId: DuplicateError →
	// 200-with-original at the projections; still exactly ONE
	// archive_project event published.
	_, err = s.svc.ArchiveProject(ctx, s.owner(), p.ID, key)
	var dup *board.DuplicateError
	require.ErrorAs(s.T(), err, &dup)
	assert.Equal(s.T(), key, dup.EventID)

	var archives int
	for _, evt := range cap.snapshot() {
		if evt.Action == "archive_project" {
			archives++
		}
	}
	assert.Equal(s.T(), 1, archives, "a replayed requestId re-publishes nothing")
}

// TestPublisher_EngineAndReconcilerEventsPublish: the non-eventWriter
// write paths publish too — a granted claim (task_start/claim) and a
// reconciler reclaim (the UI's §4.6 live-move feed depends on exactly
// this class).
func (s *BoardSuite) TestPublisher_EngineAndReconcilerEventsPublish() {
	cap := &eventCapture{}
	s.svc.SetEventPublisher(cap.record)
	s.recon.SetEventPublisher(cap.record)

	ws := s.seedWorkspace()
	p := s.seedProject(ws, 0)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "engine path", nil, false)

	claim := s.claimOrFail(ws)
	require.Equal(s.T(), task.ID, claim.TaskID)
	assert.Contains(s.T(), cap.actions(), "claim", "the engine's claim event publishes")

	// Lapse the lease; the reconciler's RC1 reclaim publishes.
	s.expireLease(task.ID)
	res := s.sweep()
	require.EqualValues(s.T(), 1, res.Reclaims)
	assert.Contains(s.T(), cap.actions(), "reclaim", "the reconciler's reclaim event publishes")

	for _, evt := range cap.snapshot() {
		if evt.Action == "reclaim" {
			assert.Equal(s.T(), board.ActorSystem, jsonActorKind(s.T(), evt.Actor))
			return
		}
	}
}

// TestPublisher_NilPublisherWritesSucceed: no installed publisher is a
// no-op (engine dark / broker absent) — every action still commits.
// (The entire pre-existing suite already runs without a publisher; this
// pins it explicitly beside the publishing arms.)
func (s *BoardSuite) TestPublisher_NilPublisherWritesSucceed() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 0)
	e := s.seedEpic(p, board.EpicActive)
	_, err := s.svc.CreateTask(ctx, s.owner(), e.ID, "", board.TaskInput{Title: "dark write", Tags: []string{}}, "")
	require.NoError(s.T(), err)
	_, err = s.svc.ArchiveProject(ctx, s.owner(), p.ID, "dark-1")
	require.NoError(s.T(), err)
}

// TestPublisher_LifecycleRequestIdReplayProtection: the six lifecycle
// actions thread requestId into their event writers (the #1728-flagged
// engine follow-up): a replay of archive_project renders the original
// outcome instead of writing a second event row.
func (s *BoardSuite) TestPublisher_LifecycleRequestIdReplayProtection() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 0)
	e := s.seedEpic(p, board.EpicDraft)

	const key = "lifecycle-replay-1"
	before := s.countEvents("epic_activate")
	_, err := s.svc.EpicActivate(ctx, s.owner(), e.ID, "", key)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), before+1, s.countEvents("epic_activate"), "first lifecycle write lands its event row")
	_, err = s.svc.EpicActivate(ctx, s.owner(), e.ID, "", key)
	var dup *board.DuplicateError
	require.ErrorAs(s.T(), err, &dup, "replayed lifecycle requestId renders 200-with-original, never a second transition")
	assert.Equal(s.T(), before+1, s.countEvents("epic_activate"), "exactly one epic_activate event row")

	_, err = s.svc.ArchiveProject(ctx, s.owner(), p.ID, "lifecycle-replay-2")
	require.NoError(s.T(), err)
	beforeProj := s.countEvents("archive_project")
	_, err = s.svc.ArchiveProject(ctx, s.owner(), p.ID, "lifecycle-replay-2")
	require.ErrorAs(s.T(), err, &dup)
	assert.Equal(s.T(), beforeProj, s.countEvents("archive_project"), "exactly one archive_project event row")
}

type assertErr string

func (e assertErr) Error() string { return string(e) }

func jsonActorKind(t interface{ Helper() }, raw json.RawMessage) string {
	t.Helper()
	var actor map[string]string
	if err := json.Unmarshal(raw, &actor); err != nil {
		return ""
	}
	return actor["kind"]
}
