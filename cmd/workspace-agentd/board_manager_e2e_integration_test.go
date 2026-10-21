// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

// board_manager_e2e_integration_test.go — #1747 review round 8 ("E2E
// minimal tier"): the two halves of the manager chain joined in ONE
// test. The REAL chain — the plugin-injection convention
// (lsp_injected_session over a conflicting model-declared copy) →
// agentd's real dispatchBoardManagerTool → the real loopback HTTP
// client → the pod seam over real HTTP → the real BoardService over
// Postgres (audit trail, workspace binding, grant belt all engine
// arbitration). Pinned: injection wins into the audit actor;
// identity-less dispatch fails closed pre-wire (no seam call); the
// engine belt refuses a live claimed worker (403 manager_surface_only);
// the cross-workspace binding refuses a foreign project (403
// workspace_binding).
//
// The seam's identity middleware (resolve/resolveManager: TokenReview →
// SA principal → workspace pin → owner → session gate) is a CONTRACT
// MIRROR, not the real handler: Go's internal-package rule prevents
// cmd/workspace-agentd from importing api/internal/handlers (the same
// boundary documented in controller/internal/workspace/org_status_e2e_test.go).
// The real handler's own behavior is pinned in
// api/internal/handlers/board_manager_semantics_integration_test.go; this
// test proves the agentd arm of the contract against the same engine
// guards. Everything on the action path is real: dispatch, the loopback
// wire form, HTTP, the engine (Postgres), IsWorkerSession, and the
// audit actor. The only doubles sit at the identity boundary (fake
// clientset TokenReview, workspace lookup) plus the recording spawner.
// Runs against TEST_DATABASE_URL (skips when unreachable); the schema
// is expected to exist (CI applies migrations).

package main

import (
	"context"
	"encoding/json"
	"io"
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

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	"github.com/lenaxia/llmsafespaces/pkg/board"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

type boardE2ELookup struct{}

func (boardE2ELookup) GetWorkspace(_ context.Context, workspaceID string) (*types.WorkspaceMetadata, error) {
	return &types.WorkspaceMetadata{ID: workspaceID, UserID: "user-7"}, nil
}

func boardE2EParseSAPrincipal(username string) (namespace, workspaceID string, ok bool) {
	rest, found := strings.CutPrefix(username, "system:serviceaccount:")
	if !found {
		return "", "", false
	}
	namespace, saName, found := strings.Cut(rest, ":")
	if !found {
		return "", "", false
	}
	wsID, found := strings.CutPrefix(saName, "workspace-")
	if !found {
		return "", "", false
	}
	return namespace, wsID, true
}

type boardE2ESeam struct {
	cs         *k8sfake.Clientset
	svc        *board.Service
	expectedNS string
}

func (s *boardE2ESeam) actor(ownerID, workspaceID, sessionID string) board.Actor {
	return board.Actor{Kind: board.ActorUser, UserID: ownerID, Surface: board.SurfaceAgent, WorkspaceID: workspaceID, SessionID: sessionID}
}

func (s *boardE2ESeam) fail(c *gin.Context, status int, code, msg string) {
	c.AbortWithStatusJSON(status, gin.H{"error": msg, "code": code})
}

func (s *boardE2ESeam) resolve(c *gin.Context) (fields map[string]json.RawMessage, workspaceID, ownerID, sessionID string, ok bool) {
	token := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
	if token == "" {
		s.fail(c, http.StatusUnauthorized, "", "missing authorization")
		return nil, "", "", "", false
	}
	tr, err := s.cs.AuthenticationV1().TokenReviews().Create(c.Request.Context(), &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{Token: token},
	}, metav1.CreateOptions{})
	if err != nil || !tr.Status.Authenticated {
		s.fail(c, http.StatusUnauthorized, "", "token not authenticated")
		return nil, "", "", "", false
	}
	saNamespace, saWorkspaceID, principalOK := boardE2EParseSAPrincipal(tr.Status.User.Username)

	raw, rerr := io.ReadAll(c.Request.Body)
	if rerr != nil {
		raw = nil
	}
	fields = map[string]json.RawMessage{}
	if len(raw) > 0 && json.Unmarshal(raw, &fields) != nil {
		fields = map[string]json.RawMessage{}
	}
	if v, has := fields["workspaceID"]; has {
		_ = json.Unmarshal(v, &workspaceID)
	}
	if saNamespace != s.expectedNS || saWorkspaceID != workspaceID || !principalOK {
		s.fail(c, http.StatusForbidden, "", "workspace identity mismatch")
		return nil, "", "", "", false
	}
	ws, lerr := boardE2ELookup{}.GetWorkspace(c.Request.Context(), workspaceID)
	if lerr != nil || ws == nil {
		s.fail(c, http.StatusInternalServerError, "", "workspace lookup failed")
		return nil, "", "", "", false
	}
	if v, has := fields["sessionId"]; has {
		_ = json.Unmarshal(v, &sessionID)
	}
	if sessionID == "" {
		s.fail(c, http.StatusForbidden, board.GuardManagerSurface,
			"board manager routes require the calling session's identity (sessionId) — spawned workers and unidentified callers hold no manager grant (ruling #1747 §5)")
		return nil, "", "", "", false
	}
	worker, werr := s.svc.IsWorkerSession(c.Request.Context(), sessionID)
	if werr != nil {
		s.fail(c, http.StatusServiceUnavailable, board.GuardManagerSurface, "board manager grant check failed — refusing fail-open (ruling #1747 §5)")
		return nil, "", "", "", false
	}
	if worker {
		s.fail(c, http.StatusForbidden, board.GuardManagerSurface,
			"board manager tools attach to user-collaborator sessions, not spawned workers (ruling #1747 §5)")
		return nil, "", "", "", false
	}
	return fields, workspaceID, ws.UserID, sessionID, true
}

func (s *boardE2ESeam) engineError(c *gin.Context, err error) {
	switch e := err.(type) {
	case *board.NotFoundError:
		c.JSON(http.StatusNotFound, gin.H{"error": e.Error(), "suggestions": e.Suggestions})
	case *board.GuardError:
		if e.Code == board.GuardWorkspaceBinding || e.Code == board.GuardAuthz {
			c.JSON(http.StatusForbidden, gin.H{"error": e.Error(), "code": e.Code})
			return
		}
		c.JSON(http.StatusConflict, gin.H{"error": e.Error(), "code": e.Code})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

func (s *boardE2ESeam) createProject(c *gin.Context) {
	fields, ws, owner, session, ok := s.resolve(c)
	if !ok {
		return
	}
	var req struct {
		Name        string `json:"name"`
		WorkerSlots int    `json:"workerSlots"`
	}
	_ = json.Unmarshal(boardE2EFieldsJSON(fields), &req)
	p, err := s.svc.CreateProject(c.Request.Context(), s.actor(owner, ws, session),
		board.ProjectInput{Name: req.Name, TargetWorkspaceID: ws, WorkerSlots: req.WorkerSlots}, "")
	if err != nil {
		s.engineError(c, err)
		return
	}
	c.JSON(http.StatusCreated, p)
}

func (s *boardE2ESeam) createEpic(c *gin.Context) {
	fields, ws, owner, session, ok := s.resolve(c)
	if !ok {
		return
	}
	var req struct {
		Title string `json:"title"`
	}
	_ = json.Unmarshal(boardE2EFieldsJSON(fields), &req)
	e, err := s.svc.CreateEpic(c.Request.Context(), s.actor(owner, ws, session), "", c.Param("id"),
		board.EpicInput{Title: req.Title}, "")
	if err != nil {
		s.engineError(c, err)
		return
	}
	c.JSON(http.StatusCreated, e)
}

func (s *boardE2ESeam) createTask(c *gin.Context) {
	fields, ws, owner, session, ok := s.resolve(c)
	if !ok {
		return
	}
	var req struct {
		Title       string `json:"title"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal(boardE2EFieldsJSON(fields), &req)
	t, err := s.svc.CreateTask(c.Request.Context(), s.actor(owner, ws, session), "", c.Param("id"),
		board.TaskInput{Title: req.Title, Description: req.Description}, "")
	if err != nil {
		s.engineError(c, err)
		return
	}
	c.JSON(http.StatusCreated, t)
}

func (s *boardE2ESeam) epicActivate(c *gin.Context) {
	_, ws, owner, session, ok := s.resolve(c)
	if !ok {
		return
	}
	e, err := s.svc.EpicActivate(c.Request.Context(), s.actor(owner, ws, session), "", c.Param("id"), false, "")
	if err != nil {
		s.engineError(c, err)
		return
	}
	c.JSON(http.StatusOK, e)
}

func boardE2EFieldsJSON(fields map[string]json.RawMessage) []byte {
	b, _ := json.Marshal(fields)
	return b
}

func boardE2EEventCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM board_events`).Scan(&n))
	return n
}

func TestIntegrationBoardManagerE2E(t *testing.T) {
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
	t.Cleanup(pool.Close)

	ctx := context.Background()
	const (
		expectedNS = "llmsafespaces-e2e"
		wsPod      = "11111111-1111-1111-1111-111111111101"
		wsForeign  = "22222222-2222-2222-2222-222222222202"
		mgrSession = "ses_plugin_injected_mgr"
	)
	_, err = pool.Exec(ctx, `
		TRUNCATE TABLE board_events, board_transitions, board_dependencies, board_sweeps, tasks, epics, projects
		CASCADE
	`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO users (id, username, email, password_hash, active, role)
		VALUES ('user-7', 'user-7', 'board1747e2e@example.com', 'hash', true, 'user')
		ON CONFLICT (id) DO NOTHING
	`)
	require.NoError(t, err, "users row must exist (FK target)")
	for _, ws := range []string{wsPod, wsForeign} {
		_, err = pool.Exec(ctx, `
			INSERT INTO workspaces (id, user_id, name, namespace, created_at, updated_at)
			VALUES ($1, 'user-7', 'ws-e2e', 'default', now(), now())
			ON CONFLICT (id) DO NOTHING
		`, ws)
		require.NoError(t, err)
	}

	svc := board.NewService(pool, board.EngineConfig{
		AttemptsCap:     3,
		LeaseDuration:   15 * time.Minute,
		ThrottleBackoff: time.Hour,
		DRRQuantum:      1,
		SweepInterval:   time.Second,
	}, board.WithWorkerSpawner(func(_ context.Context, c *board.Claim) (string, error) {
		return "sess-e2e-worker-" + c.ShortID, nil
	}))

	cs := k8sfake.NewSimpleClientset()
	cs.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		tr := action.(k8stesting.CreateAction).GetObject().(*authv1.TokenReview)
		tr.Status = authv1.TokenReviewStatus{
			Authenticated: true,
			User:          authv1.UserInfo{Username: "system:serviceaccount:" + expectedNS + ":workspace-" + wsPod},
		}
		return true, tr, nil
	})
	seam := &boardE2ESeam{cs: cs, svc: svc, expectedNS: expectedNS}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	b := r.Group("/internal/v1/automation/board")
	b.POST("/projects", seam.createProject)
	b.POST("/projects/:id/epics", seam.createEpic)
	b.POST("/epics/:id/tasks", seam.createTask)
	b.POST("/epics/:id/activate", seam.epicActivate)

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	client := opencode.NewLoopbackClient(srv.URL, "pw")

	// --- A: happy path — injection wins end to end ------------------------------
	resp, err := dispatchBoardManagerTool(ctx, client, "board_create_project", map[string]any{
		"lsp_injected_session": mgrSession,
		"sessionId":            "ses_MODEL_DECLARED",
		"name":                 "E2E Proj",
		"workerSlots":          2,
	}, "sa-token", wsPod)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.Status)
	var proj struct {
		ID      string `json:"id"`
		ShortID string `json:"short_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &proj))

	var n int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM board_events
		WHERE project_id = $1 AND actor->>'session_id' = $2`,
		proj.ID, mgrSession).Scan(&n))
	assert.Equal(t, 1, n)
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM board_events
		WHERE project_id = $1 AND actor->>'session_id' = 'ses_MODEL_DECLARED'`,
		proj.ID).Scan(&n))
	assert.Zero(t, n)

	resp, err = dispatchBoardManagerTool(ctx, client, "board_create_epic", map[string]any{
		"lsp_injected_session": mgrSession,
		"projectId":            proj.ShortID,
		"title":                "E2E Epic",
	}, "sa-token", wsPod)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.Status)
	var epic struct {
		ID      string `json:"id"`
		ShortID string `json:"short_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &epic))

	resp, err = dispatchBoardManagerTool(ctx, client, "board_create_task", map[string]any{
		"lsp_injected_session": mgrSession,
		"epicId":               epic.ShortID,
		"title":                "E2E Task",
		"description":          "charter",
	}, "sa-token", wsPod)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.Status)
	var task struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &task))

	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM board_events
		WHERE item_kind = 'epic' AND item_id = $1 AND actor->>'session_id' = $2`,
		epic.ID, mgrSession).Scan(&n))
	assert.Equal(t, 1, n)
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM board_events
		WHERE item_kind = 'task' AND item_id = $1 AND actor->>'session_id' = $2`,
		task.ID, mgrSession).Scan(&n))
	assert.Equal(t, 1, n)

	// --- B: identity-less dispatch fails closed pre-wire ------------------------
	before := boardE2EEventCount(t, pool)
	_, err = dispatchBoardManagerTool(ctx, client, "board_create_epic", map[string]any{
		"projectId": proj.ShortID,
		"title":     "x",
	}, "sa-token", wsPod)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "identity")
	assert.Equal(t, before, boardE2EEventCount(t, pool))

	// --- C: the engine belt refuses a claimed worker session --------------------
	resp, err = dispatchBoardManagerTool(ctx, client, "board_epic_activate", map[string]any{
		"lsp_injected_session": mgrSession,
		"epicId":               epic.ShortID,
	}, "sa-token", wsPod)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.Status)

	claim, err := svc.ClaimNext(ctx, "e2e-worker", wsPod)
	require.NoError(t, err)
	require.NotNil(t, claim)
	require.Equal(t, task.ID, claim.TaskID)

	_, err = dispatchBoardManagerTool(ctx, client, "board_create_task", map[string]any{
		"lsp_injected_session": claim.Assignee,
		"epicId":               epic.ShortID,
		"title":                "worker power grab",
	}, "sa-token", wsPod)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403")
	assert.Contains(t, err.Error(), board.GuardManagerSurface)

	// --- D: the cross-workspace binding -----------------------------------------
	foreign, err := svc.CreateProject(ctx,
		board.Actor{Kind: board.ActorUser, UserID: "user-7", Surface: board.SurfaceREST},
		board.ProjectInput{Name: "Foreign", TargetWorkspaceID: wsForeign}, "")
	require.NoError(t, err)
	_, err = dispatchBoardManagerTool(ctx, client, "board_create_epic", map[string]any{
		"lsp_injected_session": mgrSession,
		"projectId":            foreign.ShortID,
		"title":                "nope",
	}, "sa-token", wsPod)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403")
	assert.Contains(t, err.Error(), board.GuardWorkspaceBinding)
}
