// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

// phrase_read_integration_test.go — #1922 red-first pins: the board's
// public read edge serves the #1756 PHRASE for assignee/actor session
// ids (read-time translation; the stored rows stay canonical), with the
// #1817/#1821 degraded posture (raw + deprecation flag) while the
// design-0066 registration seam has not yet observed the session.
//
// Born-RED on pristine main: every warm leg observes the raw harness id
// (scanTask copies t.assignee_session verbatim — the leak), so the
// phrase assertions fail until the read-edge translation lands.

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// jsonOf renders a row as its wire form (the pins assert contract
// fields — e.g. assignee_session_deprecated — through the JSON so the
// file compiles against pristine main and fails ASSERTIONS, not the
// build, in the born-red run).
func jsonOf(s *BoardSuite, v any) string {
	b, err := json.Marshal(v)
	s.Require().NoError(err)
	return string(b)
}

// deprecatedFlag renders the row's wire form ONLY when it carries the
// deprecation marker ("" when absent).
func deprecatedFlag(s *BoardSuite, v any) string {
	for _, seg := range strings.Split(jsonOf(s, v), ",") {
		if strings.Contains(seg, "assignee_session_deprecated") {
			return seg
		}
	}
	return ""
}

// seedPhraseRow registers a session_index row (the design-0066 warm
// posture: the proxy observed the session and the phrase was minted).
func (s *BoardSuite) seedPhraseRow(wsID, sessionID, phrase string) {
	_, err := s.pool.Exec(context.Background(), `
		INSERT INTO session_index (workspace_id, session_id, short_id, title, message_count)
		VALUES ($1, $2, $3, 'phrase pin', 1)
		ON CONFLICT (workspace_id, session_id) DO UPDATE SET short_id = EXCLUDED.short_id`,
		wsID, sessionID, phrase)
	s.Require().NoError(err)
}

// claimWithAssignee claims the next task and stamps a canonical harness
// id as the assignee (the production spawn stamp, engine.go:487).
func (s *BoardSuite) claimWithAssignee(wsID string) (*board.Claim, string) {
	c := s.claimOrFail(wsID)
	raw := "ses_" + uuid.NewString()
	_, err := s.pool.Exec(context.Background(),
		`UPDATE tasks SET assignee_session=$1 WHERE id=$2`, raw, c.TaskID)
	s.Require().NoError(err)
	return c, raw
}

// managerActorOn builds the manager-grade actor — any in-workspace
// session post-repeal (ruling 2026-10-27 /
// tsk-copper-refectory-generates); the harness-attested SessionID
// attributes the audit row.
func (s *BoardSuite) managerActorOn(wsID, sessionID string) board.Actor {
	return board.Actor{Kind: board.ActorUser, UserID: testUser,
		Surface: board.SurfaceAgentManager, WorkspaceID: wsID, SessionID: sessionID}
}

// TestReads_AssigneePhraseTranslation pins the WARM posture: with the
// session_index row present (phrase minted), every task-bearing public
// read serves the phrase — never the raw harness id — without the
// deprecation flag, and the STORED row stays canonical.
func (s *BoardSuite) TestReads_AssigneePhraseTranslation() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "phrase pin work", nil, true)

	claim, raw := s.claimWithAssignee(ws)
	const phrase = "amber-falcon-glides"
	s.seedPhraseRow(ws, raw, phrase)
	s.seedPhraseRowCleanup(ws)

	// The drawer's read.
	got, err := s.svc.GetTask(ctx, t1.ID, "")
	require.NoError(s.T(), err)
	assert.Equal(s.T(), phrase, got.AssigneeSession, "GetTask serves the phrase")
	assert.Empty(s.T(), deprecatedFlag(s, got), "warm row carries no deprecation flag")
	assert.NotContains(s.T(), got.AssigneeSession, "ses_", "the raw harness id never leaves the read edge")

	// The project detail listing.
	tasks, err := s.svc.ListTasks(ctx, p.ID, "", "")
	require.NoError(s.T(), err)
	require.Len(s.T(), tasks, 1)
	assert.Equal(s.T(), phrase, tasks[0].AssigneeSession, "ListTasks serves the phrase")
	assert.Empty(s.T(), deprecatedFlag(s, tasks[0]))

	// Board-wide search.
	res, err := s.svc.SearchTasks(ctx, testUser, "phrase pin", board.SearchFilters{})
	require.NoError(s.T(), err)
	require.Len(s.T(), res.Results, 1)
	assert.Equal(s.T(), phrase, res.Results[0].AssigneeSession, "SearchTasks serves the phrase")

	// Health leases — both perimeters.
	h, err := s.svc.BoardHealth(ctx, testUser)
	require.NoError(s.T(), err)
	require.Len(s.T(), h.ActiveLeases, 1)
	assert.Equal(s.T(), phrase, h.ActiveLeases[0].AssigneeSession, "BoardHealth lease serves the phrase")
	assert.Empty(s.T(), deprecatedFlag(s, h.ActiveLeases[0]))

	hw, err := s.svc.BoardHealthInWorkspace(ctx, ws)
	require.NoError(s.T(), err)
	require.Len(s.T(), hw.ActiveLeases, 1)
	assert.Equal(s.T(), phrase, hw.ActiveLeases[0].AssigneeSession, "BoardHealthInWorkspace lease serves the phrase")

	// The agent-surface projections (manager board_list / worker
	// board_read ride these).
	wsTasks, _, err := s.svc.ListTasksInWorkspace(ctx, ws, p.ID, "", "", "")
	require.NoError(s.T(), err)
	require.Len(s.T(), wsTasks, 1)
	assert.Equal(s.T(), phrase, wsTasks[0].AssigneeSession, "ListTasksInWorkspace serves the phrase")

	wt, err := s.svc.GetTaskInWorkspace(ctx, ws, t1.ID, "")
	require.NoError(s.T(), err)
	assert.Equal(s.T(), phrase, wt.AssigneeSession, "GetTaskInWorkspace serves the phrase")

	// Write echoes (the filing's stage-move echo class): the worker's
	// token-guarded in_progress→in_review move returns the task row.
	worker := board.Actor{Kind: board.ActorUser, UserID: testUser, Surface: board.SurfaceAgent,
		ClaimToken: claim.ClaimToken, WorkspaceID: ws}
	moved, err := s.svc.MoveStage(ctx, worker, t1.ID, "", board.StageInReview, "https://example.test/evidence", "")
	require.NoError(s.T(), err)
	assert.Equal(s.T(), phrase, moved.AssigneeSession, "MoveStage echo serves the phrase")

	// Storage stays canonical: the stored row was NEVER rewritten.
	stored := s.rawTask(t1.ID)
	require.NotNil(s.T(), stored.Assignee)
	assert.Equal(s.T(), raw, *stored.Assignee, "the stored assignee_session stays the canonical harness id (read-time translation only)")
}

// seedPhraseRowCleanup keeps the suite's session_index seed isolated to
// this workspace (the harness truncates board tables only).
func (s *BoardSuite) seedPhraseRowCleanup(wsID string) {
	s.T().Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(),
			`DELETE FROM session_index WHERE workspace_id=$1`, wsID)
	})
}

// TestReads_AssigneePhraseColdDegrade pins the degraded posture while
// the registration seam has not observed the session (design 0066 cold
// window): the raw harness id passes through (never worse) carrying the
// deprecation flag — the #1817/#1821 SSE precedent's shape — and the
// row CONVERGES to the phrase on the very next read once minted.
func (s *BoardSuite) TestReads_AssigneePhraseColdDegrade() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "cold degrade pin", nil, true)

	claim, raw := s.claimWithAssignee(ws)
	s.seedPhraseRowCleanup(ws)
	require.NotEmpty(s.T(), claim.ClaimToken)

	got, err := s.svc.GetTask(ctx, t1.ID, "")
	require.NoError(s.T(), err)
	assert.Equal(s.T(), raw, got.AssigneeSession, "cold row degrades to the raw harness id (never worse)")
	assert.Contains(s.T(), deprecatedFlag(s, got), `"assignee_session_deprecated":true`, "the degraded raw form carries the deprecation flag")

	h, err := s.svc.BoardHealth(ctx, testUser)
	require.NoError(s.T(), err)
	require.Len(s.T(), h.ActiveLeases, 1)
	assert.Equal(s.T(), raw, h.ActiveLeases[0].AssigneeSession)
	assert.Contains(s.T(), deprecatedFlag(s, h.ActiveLeases[0]), `"assignee_session_deprecated":true`, "health lease degradation is flagged too")

	// Convergence: once the seam registers the session, the next read
	// serves the phrase and clears the flag.
	s.seedPhraseRow(ws, raw, "quiet-harbor-drifts")
	got, err = s.svc.GetTask(ctx, t1.ID, "")
	require.NoError(s.T(), err)
	assert.Equal(s.T(), "quiet-harbor-drifts", got.AssigneeSession, "the read converges to the phrase once minted")
	assert.Empty(s.T(), deprecatedFlag(s, got))
}

// TestReads_ActorSessionPhrase pins surface 2: board_events.actor's
// session_id is translated at READ time (the stored audit row is
// append-only, never rewritten) — phrase when the session is known, raw
// + session_id_deprecated flag when cold (the #1821
// markDeprecatedSessionID shape).
func (s *BoardSuite) TestReads_ActorSessionPhrase() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "actor phrase pin", nil, true)
	s.seedPhraseRowCleanup(ws)

	worker := func(token string) board.Actor {
		return board.Actor{Kind: board.ActorUser, UserID: testUser, Surface: board.SurfaceAgent,
			ClaimToken: token, WorkspaceID: ws}
	}

	// Warm manager exit: the actor's session_id is known to the index.
	c1, _ := s.claimWithAssignee(ws)
	warmRaw := "ses_" + uuid.NewString()
	s.seedPhraseRow(ws, warmRaw, "brave-osprey-lands")
	_, err := s.svc.MoveStage(ctx, worker(c1.ClaimToken), t1.ID, "", board.StagePendingInput, "", "")
	require.NoError(s.T(), err)
	_, err = s.svc.MoveStage(ctx, s.managerActorOn(ws, warmRaw), t1.ID, "", board.StageBacklog, "", "")
	require.NoError(s.T(), err)

	// Cold manager exit: no index row for this session.
	_, err = s.pool.Exec(ctx, `UPDATE tasks SET stage='backlog', claim_token=NULL, lease_expires_at=NULL, assignee_session=NULL, attempts=0 WHERE id=$1`, t1.ID)
	require.NoError(s.T(), err)
	s.clearCooldowns(p, t1.ID)
	c2 := s.claimOrFail(ws)
	coldRaw := "ses_" + uuid.NewString()
	_, err = s.pool.Exec(ctx, `UPDATE tasks SET assignee_session=$1 WHERE id=$2`, coldRaw, t1.ID)
	require.NoError(s.T(), err)
	_, err = s.svc.MoveStage(ctx, worker(c2.ClaimToken), t1.ID, "", board.StagePendingInput, "", "")
	require.NoError(s.T(), err)
	_, err = s.svc.MoveStage(ctx, s.managerActorOn(ws, coldRaw), t1.ID, "", board.StageBacklog, "", "")
	require.NoError(s.T(), err)

	evs, err := s.svc.TaskEvents(ctx, t1.ID, "", 50)
	require.NoError(s.T(), err)

	var warmSeen, coldSeen bool
	for _, ev := range evs {
		var actor struct {
			SessionID           string `json:"session_id"`
			SessionIDDeprecated string `json:"session_id_deprecated"`
		}
		require.NoError(s.T(), json.Unmarshal(ev.Actor, &actor), "actor stays a JSON object")
		switch actor.SessionID {
		case "brave-osprey-lands":
			warmSeen = true
			assert.Empty(s.T(), actor.SessionIDDeprecated, "warm actor carries no deprecation flag")
		case warmRaw:
			s.T().Errorf("TaskEvents served the RAW harness id %q for a warm session (actor translation missing)", warmRaw)
		case coldRaw:
			coldSeen = true
			assert.Equal(s.T(), "true", actor.SessionIDDeprecated, "cold actor carries session_id_deprecated (the #1821 degraded shape)")
		}
	}
	assert.True(s.T(), warmSeen, "the warm manager exit's actor carries the phrase")
	assert.True(s.T(), coldSeen, "the cold manager exit's actor carries the flagged raw id")

	// Storage immutability: the stored audit rows still hold the CANONICAL
	// actor (append-only evidence is never rewritten).
	var storedCount int
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`SELECT count(*) FROM board_events WHERE item_id=$1 AND actor->>'session_id' IN ($2, $3)`,
		t1.ID, warmRaw, coldRaw).Scan(&storedCount))
	assert.EqualValues(s.T(), 2, storedCount, "the stored board_events.actor rows keep the canonical harness ids")
}

// TestReads_ReplayEchoPhraseTranslation pins the round-2 findings: a
// replayed requestId renders the ORIGINAL task's CURRENT row — claimed
// and spawn-stamped since — so the replay echo (CreateTask's outcome-id
// path, RearmTask's tail reload) must translate exactly like a fresh
// read. Born-RED on 850475f7 (both echoes served the raw id unflagged).
func (s *BoardSuite) TestReads_ReplayEchoPhraseTranslation() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	s.seedPhraseRowCleanup(ws)

	// --- CreateTask replay: original outcome's id → current row -----
	const createReq = "fix-1922-replay-create"
	in := board.TaskInput{Title: "replay create pin", Description: "replay-echo phrase pin charter: reproduce then fix, per lane protocol.", Tags: []string{},
		AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c1", Met: true}}}
	created, err := s.svc.CreateTask(ctx, s.owner(), e.ID, "", in, createReq)
	require.NoError(s.T(), err)

	claim, err := s.svc.ClaimNext(ctx, "replay-worker", ws)
	require.NoError(s.T(), err)
	require.Equal(s.T(), created.ID, claim.TaskID, "the created task is the claim candidate")
	raw := "ses_" + uuid.NewString()
	_, err = s.pool.Exec(ctx, `UPDATE tasks SET assignee_session=$1 WHERE id=$2`, raw, created.ID)
	require.NoError(s.T(), err)
	s.seedPhraseRow(ws, raw, "amber-falcon-glides")

	echo, err := s.svc.CreateTask(ctx, s.owner(), e.ID, "", in, createReq)
	require.NoError(s.T(), err, "the replay renders 200-with-original, never an error")
	assert.Equal(s.T(), created.ID, echo.ID, "the replay resolves the ORIGINAL task's row")
	assert.Equal(s.T(), "amber-falcon-glides", echo.AssigneeSession,
		"the replay echo translates the (now claimed+stamped) original row")
	assert.Empty(s.T(), deprecatedFlag(s, echo), "warm row — no deprecation flag")

	// --- RearmTask replay: tail reload of the re-claimed row ---------
	t2 := s.seedTask(e, "rearm replay pin", nil, true)
	_, err = s.pool.Exec(ctx,
		`UPDATE tasks SET stage='pending_input', pending_origin='escalated' WHERE id=$1`, t2.ID)
	require.NoError(s.T(), err)
	const rearmReq = "fix-1922-replay-rearm"
	_, err = s.svc.RearmTask(ctx, s.owner(), t2.ID, "", rearmReq)
	require.NoError(s.T(), err)

	c2 := s.claimOrFail(ws)
	require.Equal(s.T(), t2.ID, c2.TaskID, "the re-pooled task is the next candidate")
	raw2 := "ses_" + uuid.NewString()
	_, err = s.pool.Exec(ctx, `UPDATE tasks SET assignee_session=$1 WHERE id=$2`, raw2, t2.ID)
	require.NoError(s.T(), err)
	s.seedPhraseRow(ws, raw2, "brave-osprey-lands")

	echo2, err := s.svc.RearmTask(ctx, s.owner(), t2.ID, "", rearmReq)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), "brave-osprey-lands", echo2.AssigneeSession,
		"the rearm replay echo translates the re-claimed row's current state")
	assert.Empty(s.T(), deprecatedFlag(s, echo2))
}

// TestReads_TaskStartEchoPhrase pins the TaskStart echo's embedded task
// row: the copy resolves AFTER the spawn launch, so a (sync- or
// fast-async) spawner's stamp is already on the row — the echo must
// translate it. Born-RED on 850475f7 (the sync-spawn path determin-
// istically exposed the raw stamp). The Claim.Assignee field itself is
// the documented probabilistic window (see the PR body).
func (s *BoardSuite) TestReads_TaskStartEchoPhrase() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "task start echo pin", nil, true)
	s.seedPhraseRowCleanup(ws)

	// The suite's fake spawner returns "sess-" + ShortID — warm it
	// BEFORE the start so the stamp resolves to a phrase.
	spawnID := "sess-" + t1.ShortID
	s.seedPhraseRow(ws, spawnID, "quiet-harbor-drifts")

	c, err := s.svc.TaskStart(ctx, s.owner(), t1.ID, "", "")
	require.NoError(s.T(), err)
	require.False(s.T(), c.Escalated)
	require.NotNil(s.T(), c.Task, "the grant embeds the task row")
	assert.Equal(s.T(), "quiet-harbor-drifts", c.Task.AssigneeSession,
		"the TaskStart echo's task copy carries the phrase")
	assert.Empty(s.T(), deprecatedFlag(s, c.Task))

	// Storage stays canonical.
	stored := s.rawTask(t1.ID)
	require.NotNil(s.T(), stored.Assignee)
	assert.Equal(s.T(), spawnID, *stored.Assignee, "the stored stamp stays canonical")
}
