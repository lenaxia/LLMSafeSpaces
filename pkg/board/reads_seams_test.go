// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

// reads_seams_test.go — red-first pins for the #1699 engine read seams
// (ListEpics / board search / board health), the board_read projections
// design 0067 §4.2 names and Lane 2 could not surface (worklog 1133
// Blockers). RED until the seams exist.

import (
	"context"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// TestReads_ListEpics: a project's epics list with DERIVED progress per
// row (ruling R2 — Done/(total−StageCancelled); zero denominator renders
// no-progress), ordered by creation; an epic-less project renders empty.
func (s *BoardSuite) TestReads_ListEpics() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 0)
	e1 := s.seedEpic(p, board.EpicActive)
	e2 := s.seedEpic(p, board.EpicDraft)

	t1 := s.seedTask(e1, "do the thing", nil, false)
	t2 := s.seedTask(e1, "never mind", nil, false)
	s.seedTask(e1, "still queued", nil, false)
	// 1 done, 1 canceled, 1 backlog → derived 1/3 (the #1747 owner
	// ruling: Done over ALL tasks — the canceled exclusion design
	// 0067 R2 carried is superseded by PR #1763's semantics).
	if _, err := s.svc.MoveStage(ctx, s.owner(), t1.ID, "", board.StageDone, "https://example.com/proof", ""); err != nil {
		s.T().Fatalf("move to done: %v", err)
	}
	if _, err := s.svc.CancelTask(ctx, s.owner(), t2.ID, "", "superseded", ""); err != nil {
		s.T().Fatalf("cancel: %v", err)
	}

	epics, err := s.svc.ListEpics(ctx, p.ID)
	require.NoError(s.T(), err)
	require.Len(s.T(), epics, 2)

	var got1, got2 *board.Epic
	for i := range epics {
		switch epics[i].ID {
		case e1.ID:
			got1 = &epics[i]
		case e2.ID:
			got2 = &epics[i]
		}
	}
	require.NotNil(s.T(), got1, "epic 1 present")
	require.NotNil(s.T(), got2, "epic 2 present")
	assert.Equal(s.T(), board.EpicActive, got1.Status)
	assert.True(s.T(), got1.ProgressDerived, "progress is derived, never stored")
	assert.Equal(s.T(), 1, got1.ProgressDone)
	assert.Equal(s.T(), 3, got1.ProgressTotal, "denominator counts ALL tasks (#1747 ruling — PR #1763 semantics)")
	assert.False(s.T(), got2.ProgressDerived, "task-less epic renders no-progress, never 0/0")
	assert.Equal(s.T(), 0, got2.ProgressDone)
	assert.Equal(s.T(), 0, got2.ProgressTotal)

	empty := s.seedProject(s.seedWorkspace(), 0)
	list, err := s.svc.ListEpics(ctx, empty.ID)
	require.NoError(s.T(), err)
	assert.Empty(s.T(), list)
}

// TestReads_Search pins the search projection: short-ID exact
// (case-insensitive) OR title substring, scoped by tag/stage/epic/project
// filters; the authorization perimeter NEVER leaks foreign rows (results
// or suggestions); zero-hit near-misses carry 0066-shape suggestions.
func (s *BoardSuite) TestReads_Search() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 0)
	e1 := s.seedEpic(p, board.EpicActive)
	e2 := s.seedEpic(p, board.EpicActive)

	s.seedTask(e1, "Fix login bug", []string{"auth"}, false)
	s.seedTask(e2, "Fix logout bug", []string{"auth", "urgent"}, false)
	s.seedTask(e2, "Write the docs", []string{"docs"}, false)
	// Foreign user's identically-titled task: never matched, never suggested.
	foreign := s.seedForeignTask("Fix login bug")

	res, err := s.svc.SearchTasks(ctx, testUser, "login", board.SearchFilters{})
	require.NoError(s.T(), err)
	require.Len(s.T(), res.Results, 1, "title substring matches the login task")
	for _, r := range res.Results {
		assert.NotEqual(s.T(), foreign.ID, r.ID, "foreign rows never match")
		assert.NotEmpty(s.T(), r.ProjectName, "materialized display column present")
	}
	assert.Empty(s.T(), res.Suggestions, "hits carry no suggestions")

	res, err = s.svc.SearchTasks(ctx, testUser, "FIX LOGOUT", board.SearchFilters{})
	require.NoError(s.T(), err)
	assert.Len(s.T(), res.Results, 1, "title search is case-insensitive")

	// A fixture short id, exact-resolved case-insensitively.
	var byShort string
	for _, r := range res.Results {
		byShort = r.ShortID
	}
	require.NotEmpty(s.T(), byShort, "fixture produced a short id")
	res, err = s.svc.SearchTasks(ctx, testUser, byShort, board.SearchFilters{})
	require.NoError(s.T(), err)
	require.Len(s.T(), res.Results, 1)
	assert.Equal(s.T(), byShort, res.Results[0].ShortID)

	res, err = s.svc.SearchTasks(ctx, testUser, "Fix", board.SearchFilters{Tags: []string{"urgent"}})
	require.NoError(s.T(), err)
	require.Len(s.T(), res.Results, 1, "tag filter scopes the search")

	res, err = s.svc.SearchTasks(ctx, testUser, "Fix", board.SearchFilters{Epic: e1.ShortID})
	require.NoError(s.T(), err)
	require.Len(s.T(), res.Results, 1, "epic chip scopes the search")

	res, err = s.svc.SearchTasks(ctx, testUser, "bug", board.SearchFilters{Stage: board.StageBacklog})
	require.NoError(s.T(), err)
	assert.Len(s.T(), res.Results, 2, "stage filter keeps the two backlog bug rows")

	res, err = s.svc.SearchTasks(ctx, testUser, "bug", board.SearchFilters{Project: p.ShortID})
	require.NoError(s.T(), err)
	assert.Len(s.T(), res.Results, 2, "project focus scopes to the project")

	// Zero-hit near-miss: suggestions name the close short id (top-3,
	// 0066 shape), never the foreign row.
	nearMiss := mutateLastChar(byShort)
	res, err = s.svc.SearchTasks(ctx, testUser, nearMiss, board.SearchFilters{})
	require.NoError(s.T(), err)
	assert.Empty(s.T(), res.Results)
	require.NotEmpty(s.T(), res.Suggestions, "zero-hit near-miss suggests")
	assert.Contains(s.T(), res.Suggestions, byShort)

	res, err = s.svc.SearchTasks(ctx, testUser, "zzz-no-such-thing", board.SearchFilters{})
	require.NoError(s.T(), err)
	assert.Empty(s.T(), res.Results)
	assert.Empty(s.T(), res.Suggestions, "far-miss renders no suggestions")
}

// TestReads_SearchValidation: empty, blank, and over-long queries are
// input errors, not empty results; an unknown stage filter errors.
func (s *BoardSuite) TestReads_SearchValidation() {
	for _, q := range []string{"", "   ", string(make([]byte, 300))} {
		_, err := s.svc.SearchTasks(context.Background(), testUser, q, board.SearchFilters{})
		assert.Error(s.T(), err, "query %q must be an input error", q)
	}
	_, err := s.svc.SearchTasks(context.Background(), testUser, "x", board.SearchFilters{Stage: "bogus"})
	assert.Error(s.T(), err, "unknown stage filter must be an input error")
}

// TestReads_Health pins board_read(health) (§4.8's health panel): active
// leases + expiry, recent reclaims, task + project-slot cooldowns,
// pending_input by origin, and per-project slots in use.
func (s *BoardSuite) TestReads_Health() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "claimed work", nil, false)
	waiting := s.seedTask(e, "waiting on human", nil, false)

	claim := s.claimOrFail(ws)

	// A pending_input row (origin: escalated), a task cooldown, a project
	// slot cooldown, and one prior reclaim event.
	_, err := s.pool.Exec(ctx,
		`UPDATE tasks SET stage='pending_input', pending_origin='escalated' WHERE id=$1`, waiting.ID)
	require.NoError(s.T(), err)
	_, err = s.pool.Exec(ctx,
		`UPDATE tasks SET cooldown_until = now() + interval '1 hour' WHERE id=$1`, t1.ID)
	require.NoError(s.T(), err)
	_, err = s.pool.Exec(ctx,
		`UPDATE projects SET slot_cooldown_until = now() + interval '1 hour' WHERE id=$1`, p.ID)
	require.NoError(s.T(), err)
	_, err = s.pool.Exec(ctx, `
		INSERT INTO board_events (id, project_id, item_kind, item_id, action, actor, payload)
		VALUES ('reconciler:reclaim:fixture', $1, 'task', $2, 'reclaim',
		        '{"kind":"system","surface":"reconciler"}'::jsonb, '{}'::jsonb)`,
		p.ID, t1.ID)
	require.NoError(s.T(), err)

	h, err := s.svc.BoardHealth(ctx, testUser)
	require.NoError(s.T(), err)

	require.Len(s.T(), h.ActiveLeases, 1)
	assert.Equal(s.T(), claim.TaskID, h.ActiveLeases[0].TaskID)
	assert.False(s.T(), h.ActiveLeases[0].LeaseExpiresAt.IsZero())
	assert.Contains(s.T(), h.ActiveLeases[0].AssigneeSession, "sess-", "spawn-stamped assignee")
	assert.Equal(s.T(), p.ID, h.ActiveLeases[0].ProjectID)

	require.Len(s.T(), h.TaskCooldowns, 1)
	assert.Equal(s.T(), t1.ID, h.TaskCooldowns[0].TaskID)
	assert.True(s.T(), h.TaskCooldowns[0].CooldownUntil.After(time.Now()))

	require.Len(s.T(), h.ProjectCooldowns, 1)
	assert.Equal(s.T(), p.ID, h.ProjectCooldowns[0].ProjectID)

	assert.EqualValues(s.T(), 1, h.PendingInputByOrigin["escalated"])

	require.NotEmpty(s.T(), h.RecentReclaims)
	assert.Equal(s.T(), t1.ID, h.RecentReclaims[0].TaskID)

	require.Len(s.T(), h.ProjectSlots, 1)
	assert.Equal(s.T(), 2, h.ProjectSlots[0].WorkerSlots)
	assert.Equal(s.T(), 1, h.ProjectSlots[0].InUse)

	// A fresh owner's board renders zeroed health, never errors.
	empty, err := s.svc.BoardHealth(ctx, "board-health-empty-user")
	require.NoError(s.T(), err)
	assert.Empty(s.T(), empty.ActiveLeases)
	assert.Empty(s.T(), empty.RecentReclaims)
	assert.Empty(s.T(), empty.ProjectCooldowns)
	assert.Empty(s.T(), empty.TaskCooldowns)
	assert.Empty(s.T(), empty.ProjectSlots)
}

// seedForeignTask inserts another user's task with the given title,
// bypassing the service (the perimeter fixture: rows the caller must
// never see in results or suggestions).
func (s *BoardSuite) seedForeignTask(title string) *board.Task {
	ctx := context.Background()
	const otherUser = "board-foreign-user"
	_, err := s.pool.Exec(ctx,
		`INSERT INTO users (id, username, email, password_hash, active, role)
		 VALUES ($1, $1, 'board-foreign@example.com', 'hash', true, 'user')
		 ON CONFLICT (id) DO NOTHING`, otherUser)
	s.Require().NoError(err)
	ws := s.seedWorkspace()
	var tID, shortID string
	err = s.pool.QueryRow(ctx, `
		WITH p AS (
			INSERT INTO projects (id, short_id, user_id, name, target_workspace_id, worker_slots, tags, status)
			VALUES (gen_random_uuid(), 'prj-ffff0001', $1, 'foreign board', $2, 2, '{}', 'active')
			RETURNING id
		), e AS (
			INSERT INTO epics (id, project_id, short_id, title, status, priority, project_name, tags)
			VALUES (gen_random_uuid(), (SELECT id FROM p), 'epc-ffff0001', 'foreign epic', 'active', 'P2', 'foreign board', '{}')
			RETURNING id
		)
		INSERT INTO tasks (id, project_id, epic_id, short_id, title, stage, priority, project_name, epic_name, tags, acceptance_criteria)
		VALUES (gen_random_uuid(), (SELECT id FROM p), (SELECT id FROM e), 'tsk-ffff0001', $3, 'backlog', 'P2', 'foreign board', 'foreign epic', '{}', '[]')
		RETURNING id`, otherUser, ws, title).Scan(&tID)
	s.Require().NoError(err)
	s.Require().NoError(s.pool.QueryRow(ctx, `SELECT short_id FROM tasks WHERE id=$1`, tID).Scan(&shortID))
	return &board.Task{ID: tID, ShortID: shortID, Title: title}
}

// mutateLastChar swaps the last char of a short id for a near-miss with
// edit distance 1 (within the 0066 suggestion bound).
func mutateLastChar(id string) string {
	b := []byte(id)
	if b[len(b)-1] == '0' {
		b[len(b)-1] = '1'
	} else {
		b[len(b)-1] = '0'
	}
	return string(b)
}
