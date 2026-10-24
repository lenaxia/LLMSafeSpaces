// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package handlers

// board_manager_delete_bulk_integration_test.go — #1836 sibling's e2e
// gate: the bulk DELETE pinned END-TO-END on the pod seam — the REAL
// BoardService (Postgres arbitration, no fakes on the action path)
// behind the REAL pod-identity belt (TokenReview resolve → workspace
// pin → resolveManager: fail-closed identity, the live-assignee worker
// refusal answered from Postgres). Pinned: the per-item envelope
// through real wiring (done + not_found in one 200 — the absence ≡
// miss ≡ foreign triple-equality), the real belt's worker refusal on
// the bulk delete route, the in-flight item's post-commit worker abort,
// and the session-attributed audit (delete_task event with the
// final-state snapshot) for every landed item.

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

func TestIntegrationBoardManagerDeleteTasksBulk(t *testing.T) {
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
		TRUNCATE TABLE board_events, board_transitions, board_dependencies, board_sweeps, tasks, epics, projects
		CASCADE
	`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO users (id, username, email, password_hash, active, role)
		VALUES ('user-7', 'user-7', 'board1836del@example.com', 'hash', true, 'user')
		ON CONFLICT (id) DO NOTHING
	`)
	require.NoError(t, err)
	for _, ws := range []string{wsPod, wsForeign} {
		_, err = pool.Exec(ctx, `
			INSERT INTO workspaces (id, user_id, name, namespace, created_at, updated_at)
			VALUES ($1, 'user-7', $2, 'default', now(), now())
			ON CONFLICT (id) DO NOTHING`, ws, "ws-"+ws)
		require.NoError(t, err)
	}

	aborts := map[string]string{} // taskID → assignee (post-commit abort spy)
	svc := board.NewService(pool, board.EngineConfig{
		AttemptsCap: 3, LeaseDuration: 15 * time.Minute,
		ThrottleBackoff: time.Hour, DRRQuantum: 1, SweepInterval: time.Second,
	},
		board.WithWorkerSpawner(func(_ context.Context, c *board.Claim) (string, error) {
			return "sess-worker-" + c.ShortID, nil
		}),
		board.WithWorkerAborter(func(_ context.Context, taskID, assignee string) error {
			aborts[taskID] = assignee
			return nil
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
	b.POST("/tasks/delete-bulk", h.BoardManagerDeleteTasksBulk)

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
	const sess = "ses_orchestrator-1836del"

	// Seed through the real seam: project → epic → tasks → activate.
	w := call(http.MethodPost, "/internal/v1/automation/board/projects",
		fmt.Sprintf(`{"workspaceID":%q,"sessionId":%q,"name":"BulkDel","workerSlots":2}`, wsPod, sess))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var proj board.Project
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &proj))

	w = call(http.MethodPost, "/internal/v1/automation/board/projects/"+proj.ID+"/epics",
		fmt.Sprintf(`{"workspaceID":%q,"sessionId":%q,"title":"E1"}`, wsPod, sess))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var epic board.Epic
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &epic))

	mkTask := func(title string) *board.Task {
		w := call(http.MethodPost, "/internal/v1/automation/board/epics/"+epic.ID+"/tasks",
			fmt.Sprintf(`{"workspaceID":%q,"sessionId":%q,"title":%q,"description":"charter","acceptanceCriteria":[{"text":"c1","met":true}]}`,
				wsPod, sess, title))
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
		var task board.Task
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &task))
		return &task
	}
	idle := mkTask("idle")
	beltProbe := mkTask("belt-probe")

	w = call(http.MethodPost, "/internal/v1/automation/board/epics/"+epic.ID+"/activate",
		fmt.Sprintf(`{"workspaceID":%q,"sessionId":%q}`, wsPod, sess))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// A live worker session exists (a claimed backlog row's assignee —
	// the belt answers from Postgres) — claim the probe through the
	// engine and keep it in flight.
	claim, err := svc.ClaimNext(ctx, "worker-1836del", wsPod)
	require.NoError(t, err)
	require.Equal(t, beltProbe.ID, claim.TaskID, "the probe is the claimable row")
	liveWorker := claim.Assignee

	// --- 1: the belt's worker refusal through REAL wiring --------------------------
	w = call(http.MethodPost, "/internal/v1/automation/board/tasks/delete-bulk",
		fmt.Sprintf(`{"workspaceID":%q,"sessionId":%q,"taskIds":[%q]}`, wsPod, liveWorker, idle.ShortID))
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), board.GuardManagerSurface)
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE id=$1`, idle.ID).Scan(&n))
	assert.Equal(t, 1, n, "the refused batch deleted NOTHING")

	// --- 2: the per-item envelope through REAL wiring ------------------------------
	// done + not_found (a miss) in ONE 200; a foreign-workspace item
	// (same owner, other workspace) renders the same not_found.
	foreignProj, err := svc.CreateProject(ctx, owner,
		board.ProjectInput{Name: "Foreign", TargetWorkspaceID: wsForeign}, "")
	require.NoError(t, err)
	foreignEpic, err := svc.CreateEpic(ctx, owner, foreignProj.ID, "", board.EpicInput{Title: "FE"}, "")
	require.NoError(t, err)
	foreignTask, err := svc.CreateTask(ctx, owner, foreignEpic.ID, "",
		board.TaskInput{Title: "FT", Description: "charter",
			AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c1", Met: true}}}, "")
	require.NoError(t, err)

	w = call(http.MethodPost, "/internal/v1/automation/board/tasks/delete-bulk",
		fmt.Sprintf(`{"workspaceID":%q,"sessionId":%q,"taskIds":[%q,%q,%q],"requestId":"e2e-del-1"}`,
			wsPod, sess, idle.ShortID, "tsk-nope-does-not-exist", foreignTask.ShortID))
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
	assert.Equal(t, board.BulkDone, byRef[idle.ShortID].Status, "the idle row deletes")
	missItem := byRef["tsk-nope-does-not-exist"]
	assert.Equal(t, safeCode(missItem), board.BulkItemNotFound, "a miss is its own not_found item")
	foreignItem := byRef[foreignTask.ShortID]
	assert.Equal(t, safeCode(foreignItem), board.BulkItemNotFound,
		"the workspace binding is the perimeter: the foreign-workspace item is a plain not_found")
	// Refs echo verbatim (differ by construction); the never-disclose
	// property: each class's error renders EXACTLY what a NotFoundError
	// for the caller's OWN ref renders — same code, same detail
	// template, zero class information.
	require.NotNil(t, missItem.Error)
	require.NotNil(t, foreignItem.Error)
	assert.Equal(t, missItem.Error.Code, foreignItem.Error.Code)
	assert.Equal(t, (&board.NotFoundError{Item: "task", Identifier: foreignTask.ShortID}).Error(),
		foreignItem.Error.Detail, "miss ≡ foreign: the same NotFoundError render per ref (never-disclose)")
	assert.NotContains(t, w.Body.String(), "claim_token", "#1864: the bulk projection carries no tokens")

	// The landed item's audit: delete_task event attributed to the
	// orchestrator session, carrying the final-state snapshot.
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM board_events
		WHERE item_id=$1 AND action='delete_task' AND actor->>'session_id'=$2
		  AND payload ? 'snapshot'`,
		idle.ID, sess).Scan(&n))
	assert.Equal(t, 1, n, "the bulk route's audit attributes to the calling manager session, snapshot embedded")
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE id=$1`, idle.ID).Scan(&n))
	assert.Zero(t, n, "the landed item's row is gone")
	assert.Equal(t, board.StageBacklog, mustStage(t, pool, foreignTask.ID), "the foreign row is untouched")

	// --- 3: the in-flight item deletes + aborts post-commit ------------------------
	inFlight := mkTask("in-flight")
	c2, err := svc.TaskStart(ctx, owner, "", inFlight.ShortID, "")
	require.NoError(t, err)

	w = call(http.MethodPost, "/internal/v1/automation/board/tasks/delete-bulk",
		fmt.Sprintf(`{"workspaceID":%q,"sessionId":%q,"taskIds":[%q],"reconnect":false}`, wsPod, sess, inFlight.ShortID))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	require.Len(t, envelope.Results, 1)
	assert.Equal(t, board.BulkDone, envelope.Results[0].Status)
	assignee, ok := aborts[inFlight.ID]
	assert.True(t, ok, "the in-flight item's worker is aborted post-commit")
	assert.Equal(t, c2.Assignee, assignee)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE id=$1`, inFlight.ID).Scan(&n))
	assert.Zero(t, n, "the row is REALLY gone before the abort is observable as cleanup")

	// --- 4: the re-submit is safe ---------------------------------------------------
	w = call(http.MethodPost, "/internal/v1/automation/board/tasks/delete-bulk",
		fmt.Sprintf(`{"workspaceID":%q,"sessionId":%q,"taskIds":[%q],"requestId":"e2e-del-1"}`, wsPod, sess, idle.ShortID))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String(), "re-submitting the batch never errors")
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	require.Len(t, envelope.Results, 1)
	assert.Equal(t, board.BulkFailed, envelope.Results[0].Status)
	assert.Equal(t, board.BulkItemNotFound, safeCode(envelope.Results[0]),
		"absence renders not_found — the truthful, never-disclose retry rendering")
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM board_events WHERE action='delete_task' AND item_id=$1`, idle.ID).Scan(&n))
	assert.Equal(t, 1, n, "no duplicate delete event on re-submit")
}

// safeCode dereferences an item's error code (nil-safe for readable
// assertion failures).
func safeCode(item board.BulkItemResult) string {
	if item.Error == nil {
		return ""
	}
	return item.Error.Code
}
