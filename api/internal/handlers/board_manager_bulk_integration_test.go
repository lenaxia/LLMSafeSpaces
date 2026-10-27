// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package handlers

// board_manager_bulk_integration_test.go — #1836 review r1's e2e gate:
// the bulk complete pinned END-TO-END on the pod seam — the REAL
// BoardService (Postgres arbitration, no fakes on the action path)
// behind the pod-identity seam (TokenReview resolve → workspace pin →
// the manager surface OPEN to any in-workspace session, identity as
// audit attribution only — the #1747 §5 belt repealed, ruling
// 2026-10-27) — the bulk mirror of TestIntegrationBoardManagerSemantics.
// Pinned: the per-item envelope through real wiring (done +
// failed(done_guard) in one 200), the foreign-workspace item rendering
// not_found per-item (the agent-surface perimeter at the HTTP tier),
// and the session-attributed audit for every landed item.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

func TestIntegrationBoardManagerMoveStageBulk(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:testpass@127.0.0.1:5433/llmsafespaces_test?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("Skipping PG integration test: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		t.Skipf("Skipping PG integration test: %v", err)
	}

	ctx := context.Background()
	_, err = pool.Exec(ctx, `
		TRUNCATE TABLE board_events, board_transitions, board_dependencies, board_sweeps, tasks, epics, projects,
			board_config_defaults, board_project_config_overrides
		CASCADE
	`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO users (id, username, email, password_hash, active, role)
		VALUES ('user-7', 'user-7', 'board1836@example.com', 'hash', true, 'user')
		ON CONFLICT (id) DO NOTHING
	`)
	require.NoError(t, err)
	// Stage-persona isolation (the #1939 flipped floor + the stage-
	// persona mechanism fire the reviewer persona on every un-
	// configured ->in_review): this suite pins bulk-manager semantics,
	// so seed the user-level explicit {} opt-out.
	_, err = pool.Exec(ctx, `
		INSERT INTO board_config_defaults (user_id, key, value)
		VALUES ('user-7', 'stagePersonas', '{}'::jsonb)
		ON CONFLICT (user_id, key) DO NOTHING
	`)
	require.NoError(t, err)
	for _, ws := range []string{wsPod, wsForeign} {
		_, err = pool.Exec(ctx, `
			INSERT INTO workspaces (id, user_id, name, namespace, created_at, updated_at)
			VALUES ($1, 'user-7', $2, 'default', now(), now())
			ON CONFLICT (id) DO NOTHING`, ws, "ws-"+ws)
		require.NoError(t, err)
	}

	cfg := board.EngineConfig{
		AttemptsCap: 3, LeaseDuration: 15 * time.Minute,
		ThrottleBackoff: time.Hour, DRRQuantum: 1, SweepInterval: time.Second,
	}
	svc := board.NewService(pool, cfg,
		board.WithWorkerSpawner(func(_ context.Context, c *board.Claim) (string, error) {
			return "sess-worker-" + c.ShortID, nil
		}),
	)
	h := &PodAutomationHandler{
		tokenReviewer: &fakeTokenReviewer{
			username: "system:serviceaccount:" + testRenameNamespace + ":workspace-" + wsPod,
		},
		lookup:            &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: wsPod, UserID: "user-7"}},
		expectedNamespace: testRenameNamespace,
		board:             svc,
		boardEngine:       svc.Config(),
		boardMgr:          svc,
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	b := r.Group("/internal/v1/automation/board")
	b.POST("/projects", h.BoardCreateProject)
	b.POST("/projects/:id/epics", h.BoardCreateEpic)
	b.POST("/epics/:id/tasks", h.BoardCreateTask)
	b.POST("/epics/:id/activate", h.BoardEpicActivate)
	b.POST("/tasks/move-stage-bulk", h.BoardManagerMoveStageBulk)

	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer whatever-the-reviewer-accepts")
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	owner := board.Actor{Kind: board.ActorUser, UserID: "user-7", Surface: board.SurfaceREST}
	const sess = "ses_orchestrator-1836"

	// Seed through the real seam: project → epic → two tasks (criteria
	// met / unmet) → activate → engine claims → worker forward writes
	// land both rows in in_review (the review-settlement fixture).
	w := call(http.MethodPost, "/internal/v1/automation/board/projects",
		fmt.Sprintf(`{"workspaceID":%q,"sessionId":%q,"name":"Bulk","workerSlots":2}`, wsPod, sess))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var proj board.Project
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &proj))

	w = call(http.MethodPost, "/internal/v1/automation/board/projects/"+proj.ID+"/epics",
		fmt.Sprintf(`{"workspaceID":%q,"sessionId":%q,"title":"E1"}`, wsPod, sess))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var epic board.Epic
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &epic))

	mkTask := func(title string, met bool) *board.Task {
		w := call(http.MethodPost, "/internal/v1/automation/board/epics/"+epic.ID+"/tasks",
			fmt.Sprintf(`{"workspaceID":%q,"sessionId":%q,"title":%q,"description":"charter","acceptanceCriteria":[{"text":"c1","met":%t}]}`,
				wsPod, sess, title, met))
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
		var task board.Task
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &task))
		return &task
	}
	met, unmet, beltProbe := mkTask("met", true), mkTask("unmet", false), mkTask("belt-probe", true)

	w = call(http.MethodPost, "/internal/v1/automation/board/epics/"+epic.ID+"/activate",
		fmt.Sprintf(`{"workspaceID":%q,"sessionId":%q}`, wsPod, sess))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	toReview := func(task *board.Task) {
		c, err := svc.TaskStart(ctx, owner, "", task.ShortID, "")
		require.NoError(t, err)
		require.False(t, c.Escalated)
		_, err = svc.MoveStage(ctx, board.Actor{Kind: board.ActorUser, UserID: "user-7",
			Surface: board.SurfaceAgent, ClaimToken: c.ClaimToken}, "",
			task.ShortID, board.StageInReview, "https://example/work", "")
		require.NoError(t, err)
	}
	toReview(met)
	toReview(unmet)

	// A live worker session exists (a claimed backlog row's assignee —
	// the belt answers from Postgres) — claim the probe through the
	// engine and keep it in flight.
	claim, err := svc.ClaimNext(ctx, "worker-1836", wsPod)
	require.NoError(t, err)
	require.Equal(t, beltProbe.ID, claim.TaskID, "the probe is the claimable row")
	liveWorker := claim.Assignee

	// --- 1: the repealed gate through REAL wiring ----------------------------------
	// The #1747 §5 manager grant is REPEALED (ruling 2026-10-27 /
	// tsk-copper-refectory-generates): a live worker's token-less bulk
	// move is SERVED — 200 with the per-item envelope. The done move
	// carries NO evidenceUrl, so the Done guard refuses the ITEM
	// (done_guard), never the caller.
	w = call(http.MethodPost, "/internal/v1/automation/board/tasks/move-stage-bulk",
		fmt.Sprintf(`{"workspaceID":%q,"sessionId":%q,"taskIds":[%q],"toStage":"done"}`, wsPod, liveWorker, met.ShortID))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var guardEnv struct {
		Results []board.BulkItemResult `json:"results"`
		Count   int                    `json:"count"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &guardEnv))
	require.Len(t, guardEnv.Results, 1)
	assert.Equal(t, 1, guardEnv.Count)
	guardItem := guardEnv.Results[0]
	assert.Equal(t, met.ShortID, guardItem.Ref)
	assert.Equal(t, board.BulkFailed, guardItem.Status, "the worker's bulk move is served; the missing evidenceUrl fails the item")
	require.NotNil(t, guardItem.Error)
	assert.Equal(t, board.GuardDone, guardItem.Error.Code, "the Done guard's own code, per item")
	assert.Equal(t, board.StageInReview, mustStage(t, pool, met.ID), "the refused item is untouched")

	// --- 2: the per-item envelope through REAL wiring ------------------------------
	// done + failed(done_guard) in ONE 200; a foreign-workspace item
	// (same owner, other workspace) renders not_found per-item.
	foreignProj, err := svc.CreateProject(ctx, owner,
		board.ProjectInput{Name: "Foreign", TargetWorkspaceID: wsForeign}, "")
	require.NoError(t, err)
	foreignEpic, err := svc.CreateEpic(ctx, owner, foreignProj.ID, "", board.EpicInput{Title: "FE"}, "")
	require.NoError(t, err)
	foreignTask, err := svc.CreateTask(ctx, owner, foreignEpic.ID, "",
		board.TaskInput{Title: "FT", Description: "charter",
			AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c1", Met: true}}}, "")
	require.NoError(t, err)

	w = call(http.MethodPost, "/internal/v1/automation/board/tasks/move-stage-bulk",
		fmt.Sprintf(`{"workspaceID":%q,"sessionId":%q,"taskIds":[%q,%q,%q],"toStage":"done","evidenceUrl":"https://example/p","requestId":"e2e-bulk-1"}`,
			wsPod, sess, met.ShortID, unmet.ShortID, foreignTask.ShortID))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var envelope struct {
		Results []board.BulkItemResult `json:"results"`
		Count   int                    `json:"count"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	require.Len(t, envelope.Results, 3)
	assert.Equal(t, 3, envelope.Count)
	byRef := map[string]board.BulkItemResult{}
	for _, item := range envelope.Results {
		byRef[item.Ref] = item
	}
	assert.Equal(t, board.BulkDone, byRef[met.ShortID].Status, "the criteria-met row completes")
	assert.Equal(t, board.BulkFailed, byRef[unmet.ShortID].Status, "partial failure per item — never all-or-nothing")
	require.NotNil(t, byRef[unmet.ShortID].Error)
	assert.Equal(t, board.GuardDone, byRef[unmet.ShortID].Error.Code, "the Done guard's own code")
	assert.Equal(t, board.BulkFailed, byRef[foreignTask.ShortID].Status)
	assert.Equal(t, board.BulkItemNotFound, byRef[foreignTask.ShortID].Error.Code,
		"the workspace binding is the perimeter: the foreign-workspace item is a plain not_found")
	assert.NotContains(t, w.Body.String(), "claim_token", "#1864: the bulk projection carries no tokens")

	// The landed item wrote its transition and attributed its audit to
	// the orchestrator session through the REAL wiring.
	var n int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM board_transitions WHERE task_id=$1 AND to_stage='done'`, met.ID).Scan(&n))
	assert.Equal(t, 1, n)
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM board_events
		WHERE item_id=$1 AND action='move_stage' AND actor->>'session_id'=$2`,
		met.ID, sess).Scan(&n))
	assert.Equal(t, 1, n, "the bulk route's audit attributes to the calling manager session")
	assert.Equal(t, board.StageInReview, mustStage(t, pool, unmet.ID), "the failed item is untouched")
	assert.Equal(t, board.StageBacklog, mustStage(t, pool, foreignTask.ID))
}

func mustStage(t *testing.T, pool *pgxpool.Pool, taskID string) string {
	t.Helper()
	var stage string
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT stage FROM tasks WHERE id=$1`, taskID).Scan(&stage))
	return stage
}
