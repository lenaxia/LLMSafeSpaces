// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package handlers

// board_manager_semantics_integration_test.go — #1747 r2 (the review's
// integration-gap fix): the manager seam pinned END-TO-END — the REAL
// BoardService (Postgres arbitration, no fakes on the action path)
// behind the REAL pod-identity seam (resolve: TokenReview → SA
// principal → workspace pin → delegated owner) — the manager-surface
// mirror of TestIntegrationBoardWorkerSemantics. Pinned:
//
//   - a manager route creates real rows (project → epic → task) and
//     the audit trail attributes every row to the creating SESSION
//     (board_events.actor.session_id lands through the real wiring —
//     the actorViewOf embedding the fake tier could not observe)
//   - the workspace binding: a foreign-workspace project refuses with
//     the 403 class through the real guards
//   - the grant belt against a REAL live assignee: IsWorkerSession
//     answers from Postgres (a claimed row's assignee is refused 403;
//     the omission arm is pinned handler-side)
//
// Runs against TEST_DATABASE_URL (skips when unreachable); the schema
// is expected to exist (migrations applied by the CI job).

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

func TestIntegrationBoardManagerSemantics(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:testpass@localhost:5433/llmsafespaces_test?sslmode=disable"
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
		VALUES ('user-7', 'user-7', 'board1747@example.com', 'hash', true, 'user')
		ON CONFLICT (id) DO NOTHING
	`)
	require.NoError(t, err, "users row must exist (FK target)")
	_, err = pool.Exec(ctx, `
		INSERT INTO workspaces (id, user_id, name, namespace, created_at, updated_at)
		VALUES ($1, 'user-7', 'ws-pod', 'default', now(), now())
		ON CONFLICT (id) DO NOTHING
	`, wsPod)
	require.NoError(t, err)

	cfg := board.EngineConfig{
		AttemptsCap:     3,
		LeaseDuration:   15 * time.Minute,
		ThrottleBackoff: time.Hour,
		DRRQuantum:      1,
		SweepInterval:   time.Second,
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
	b.POST("/tasks/:id/pause", h.BoardPauseTask)

	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer whatever-the-reviewer-accepts")
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	// --- 1: the collaboration-to-execution loop on real rows ----------------------
	// A collaborator session creates a project, an epic, a task — and
	// every row's audit event carries the CREATING SESSION's identity.
	const sess = "ses_collab-author-1747"
	w := call(http.MethodPost, "/internal/v1/automation/board/projects",
		fmt.Sprintf(`{"workspaceID":%q,"sessionId":%q,"name":"Loop","workerSlots":2}`, wsPod, sess))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var proj board.Project
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &proj))

	w = call(http.MethodPost, "/internal/v1/automation/board/projects/"+proj.ID+"/epics",
		fmt.Sprintf(`{"workspaceID":%q,"sessionId":%q,"title":"E1","outcome":"ship"}`, wsPod, sess))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var epic board.Epic
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &epic))

	w = call(http.MethodPost, "/internal/v1/automation/board/epics/"+epic.ID+"/tasks",
		fmt.Sprintf(`{"workspaceID":%q,"sessionId":%q,"title":"T1","description":"charter"}`, wsPod, sess))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var task board.Task
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &task))
	assert.Equal(t, board.StageBacklog, task.Stage)

	// Session attribution landed in the AUDIT TRAIL through the real
	// wiring (actorViewOf's embedding — the gap the review flagged).
	var n int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM board_events
		WHERE project_id = $1 AND actor->>'session_id' = $2`,
		proj.ID, sess).Scan(&n))
	assert.Equal(t, 3, n, "create_project + create_epic + create_task all attribute to the creating session")
	// The surface marker rides the same actor object.
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM board_events
		WHERE project_id = $1 AND actor->>'surface' = 'agent'`,
		proj.ID).Scan(&n))
	assert.Equal(t, 3, n)

	// --- 2: the workspace binding on real guards -----------------------------------
	// A foreign-workspace project (same owner) refuses the agent surface.
	foreign, err := svc.CreateProject(ctx, board.Actor{Kind: board.ActorUser, UserID: "user-7", Surface: board.SurfaceREST},
		board.ProjectInput{Name: "Foreign", TargetWorkspaceID: wsForeign}, "")
	require.NoError(t, err)
	_, _ = svc.CreateEpic(ctx, board.Actor{Kind: board.ActorUser, UserID: "user-7", Surface: board.SurfaceREST},
		foreign.ID, "", board.EpicInput{Title: "FE"}, "")
	w = call(http.MethodPost, "/internal/v1/automation/board/projects/"+foreign.ID+"/epics",
		fmt.Sprintf(`{"workspaceID":%q,"sessionId":%q,"title":"nope"}`, wsPod, sess))
	assert.Equal(t, http.StatusForbidden, w.Code, "requireManagerAction: the pod's workspace is not the project's target")
	assert.Contains(t, w.Body.String(), board.GuardWorkspaceBinding)

	// --- 3: the grant belt against a REAL live assignee ----------------------------
	// Claim the task through the engine (a spawned worker exists),
	// then attempt a manager action AS that worker's session: the belt
	// answers from Postgres and refuses.
	claim, err := svc.ClaimNext(ctx, "worker-integration", wsPod)
	require.NoError(t, err)
	require.False(t, claim.Escalated)
	require.Equal(t, task.ID, claim.TaskID)
	live, err := svc.IsWorkerSession(ctx, claim.Assignee)
	require.NoError(t, err)
	require.True(t, live, "the stamped assignee is a live worker")

	w = call(http.MethodPost, "/internal/v1/automation/board/epics/"+epic.ID+"/tasks",
		fmt.Sprintf(`{"workspaceID":%q,"sessionId":%q,"title":"injection"}`, wsPod, claim.Assignee))
	assert.Equal(t, http.StatusForbidden, w.Code, "the belt refuses a live worker session (real IsWorkerSession)")
	assert.Contains(t, w.Body.String(), board.GuardManagerSurface)

	// Pause the claimed task via the manager route (the collaborator
	// still can — pause is a manager action, not worker-scoped) and
	// verify the flag lands on the real row.
	w = call(http.MethodPost, "/internal/v1/automation/board/tasks/"+task.ID+"/pause",
		fmt.Sprintf(`{"workspaceID":%q,"sessionId":%q}`, wsPod, sess))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var paused board.Task
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &paused))
	assert.True(t, paused.Paused)
}
