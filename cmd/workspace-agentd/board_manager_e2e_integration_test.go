// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

// board_manager_e2e_integration_test.go — #1747 review round 8 ("E2E
// minimal tier"), re-pinned by ruling 2026-10-27 /
// tsk-copper-refectory-generates: the two halves of the manager chain
// joined in ONE test. The REAL chain — the plugin-injection convention
// (lsp_injected_session over a conflicting model-declared copy) →
// agentd's real dispatchBoardManagerTool → the real loopback HTTP
// client → the pod seam over real HTTP → the real BoardService over
// Postgres (audit trail, workspace binding — engine arbitration).
// Pinned: injection wins into the audit actor; identity-less dispatch
// PROCEEDS with empty audit attribution (the #1747 §5 belt is
// repealed); a spawned worker's manager call PROCEEDS (the
// impersonation threat is a prompting concern, not RBAC); the
// cross-workspace binding refuses a foreign project (403
// workspace_binding).
//
// The seam's identity middleware (resolve/resolveManager: TokenReview →
// SA principal → workspace pin → owner → session attribution) is a
// CONTRACT MIRROR, not the real handler: Go's internal-package rule
// prevents cmd/workspace-agentd from importing api/internal/handlers
// (the same boundary documented in
// controller/internal/workspace/org_status_e2e_test.go). The real
// handler's own behavior is pinned in
// api/internal/handlers/board_manager_semantics_integration_test.go;
// this test proves the agentd arm of the contract against the same
// engine guards. Everything on the action path is real: dispatch, the
// loopback wire form, HTTP, the engine (Postgres), and the audit
// actor. The only doubles sit at the identity boundary (fake clientset
// TokenReview, workspace lookup) plus the recording spawner. Runs
// against TEST_DATABASE_URL (skips when unreachable); the schema is
// expected to exist (CI applies migrations).

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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

// managerActor mirrors the production manager seam (#1832 WS2): the
// MANAGER-GRADE agent surface — what the stage machine's pending_input
// guard reads.
func (s *boardE2ESeam) managerActor(ownerID, workspaceID, sessionID string) board.Actor {
	return board.Actor{Kind: board.ActorUser, UserID: ownerID, Surface: board.SurfaceAgentManager, WorkspaceID: workspaceID, SessionID: sessionID}
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
	// Attribution-only identity (ruling 2026-10-27 /
	// tsk-copper-refectory-generates): an absent or worker-typed
	// session NEVER refuses — the mirror of the production
	// resolveManager post-repeal.
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
		if e.Code == board.GuardInput || e.Code == board.GuardTextRejected { // #1837: text caps/corruption are 400s
			c.JSON(http.StatusBadRequest, gin.H{"error": e.Error(), "code": e.Code})
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
		Title              string                      `json:"title"`
		Description        string                      `json:"description"`
		AcceptanceCriteria []board.AcceptanceCriterion `json:"acceptanceCriteria"`
	}
	_ = json.Unmarshal(boardE2EFieldsJSON(fields), &req)
	t, err := s.svc.CreateTask(c.Request.Context(), s.actor(owner, ws, session), "", c.Param("id"),
		board.TaskInput{Title: req.Title, Description: req.Description, AcceptanceCriteria: req.AcceptanceCriteria}, "")
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

// managerMoveStage mirrors the production manager move route (#1832
// WS2): manager-grade actor, canonical MoveStage.
func (s *boardE2ESeam) managerMoveStage(c *gin.Context) {
	fields, ws, owner, session, ok := s.resolve(c)
	if !ok {
		return
	}
	var req struct {
		ToStage     string `json:"toStage"`
		EvidenceURL string `json:"evidenceUrl"`
	}
	_ = json.Unmarshal(boardE2EFieldsJSON(fields), &req)
	t, err := s.svc.MoveStage(c.Request.Context(), s.managerActor(owner, ws, session),
		"", c.Param("id"), req.ToStage, req.EvidenceURL, "")
	if err != nil {
		s.engineError(c, err)
		return
	}
	c.JSON(http.StatusOK, t)
}

// resolveQuery is resolve's GET twin (board_list's body-less pattern):
// the SA token proves the identity, the workspace binding rides the
// QUERY, and the session is audit attribution only (the #1747 §5
// repeal — no gate beyond the pod SA resolve on the config read).
func (s *boardE2ESeam) resolveQuery(c *gin.Context) (workspaceID, ownerID, sessionID string, ok bool) {
	token := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
	if token == "" {
		s.fail(c, http.StatusUnauthorized, "", "missing authorization")
		return "", "", "", false
	}
	tr, err := s.cs.AuthenticationV1().TokenReviews().Create(c.Request.Context(), &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{Token: token},
	}, metav1.CreateOptions{})
	if err != nil || !tr.Status.Authenticated {
		s.fail(c, http.StatusUnauthorized, "", "token not authenticated")
		return "", "", "", false
	}
	saNamespace, saWorkspaceID, principalOK := boardE2EParseSAPrincipal(tr.Status.User.Username)
	workspaceID = c.Query("workspaceID")
	if saNamespace != s.expectedNS || saWorkspaceID != workspaceID || !principalOK {
		s.fail(c, http.StatusForbidden, "", "workspace identity mismatch")
		return "", "", "", false
	}
	ws, lerr := boardE2ELookup{}.GetWorkspace(c.Request.Context(), workspaceID)
	if lerr != nil || ws == nil {
		s.fail(c, http.StatusInternalServerError, "", "workspace lookup failed")
		return "", "", "", false
	}
	return workspaceID, ws.UserID, c.Query("sessionId"), true
}

// getConfig mirrors the production GET /config seam handler
// (BoardGetConfig): the source-annotated view at either tier, no
// claim token, no gate beyond the pod SA resolve.
func (s *boardE2ESeam) getConfig(c *gin.Context) {
	ws, owner, session, ok := s.resolveQuery(c)
	if !ok {
		return
	}
	view, err := s.svc.GetBoardConfigView(c.Request.Context(),
		s.managerActor(owner, ws, session), "", c.Query("projectId"))
	if err != nil {
		s.engineError(c, err)
		return
	}
	c.JSON(http.StatusOK, view)
}

// setConfig mirrors the production PUT /config seam handler
// (BoardSetConfig): the tier selector rides the body, the routing
// keys are popped, and the remainder runs the SAME strict key-gate
// decode the platform runs (board.DecodeBoardConfigOverride) — the
// response is the annotated view of the written tier.
func (s *boardE2ESeam) setConfig(c *gin.Context) {
	fields, ws, owner, session, ok := s.resolve(c)
	if !ok {
		return
	}
	var req struct {
		Tier      string `json:"tier"`
		ProjectID string `json:"projectId"`
	}
	_ = json.Unmarshal(boardE2EFieldsJSON(fields), &req)
	for _, k := range []string{"tier", "projectId", "workspaceID", "sessionId"} {
		delete(fields, k)
	}
	in, err := board.DecodeBoardConfigOverride(fields)
	if err != nil {
		s.fail(c, http.StatusBadRequest, board.GuardInput, err.Error())
		return
	}
	actor := s.managerActor(owner, ws, session)
	var projectRef string
	switch req.Tier {
	case "user":
		if req.ProjectID != "" {
			s.fail(c, http.StatusBadRequest, board.GuardInput,
				"tier=user writes the board-wide defaults — projectId does not ride a user-tier set")
			return
		}
		if _, err := s.svc.SetUserBoardConfig(c.Request.Context(), actor, in); err != nil {
			s.engineError(c, err)
			return
		}
	case "project":
		if req.ProjectID == "" {
			s.fail(c, http.StatusBadRequest, board.GuardInput,
				"tier=project requires projectId — the project whose override map this set writes")
			return
		}
		if _, err := s.svc.SetProjectBoardConfig(c.Request.Context(), actor, req.ProjectID, "", in); err != nil {
			s.engineError(c, err)
			return
		}
		projectRef = req.ProjectID
	default:
		s.fail(c, http.StatusBadRequest, board.GuardInput,
			`tier must be "user" (the board-wide defaults) or "project" (one project's override map)`)
		return
	}
	view, err := s.svc.GetBoardConfigView(c.Request.Context(), actor, projectRef, "")
	if err != nil {
		s.engineError(c, err)
		return
	}
	c.JSON(http.StatusOK, view)
}

func boardE2EFieldsJSON(fields map[string]json.RawMessage) []byte {
	b, _ := json.Marshal(fields)
	return b
}

func boardE2EEventCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM board_events`).Scan(&n))
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
		TRUNCATE TABLE board_events, board_transitions, board_dependencies, board_sweeps,
			tasks, epics, projects, board_config_defaults
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
	b.POST("/tasks/:id/move-stage", seam.managerMoveStage)
	b.GET("/config", seam.getConfig)
	b.PUT("/config", seam.setConfig)

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	client := opencode.NewLoopbackClient(srv.URL, "pw")

	// --- A: happy path — injection wins end to end ------------------------------
	resp, err := dispatchBoardManagerTool(ctx, client, e2eMCPPassword, "board_create_project", map[string]any{
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

	resp, err = dispatchBoardManagerTool(ctx, client, e2eMCPPassword, "board_create_epic", map[string]any{
		"lsp_injected_session": mgrSession,
		"projectId":            proj.ShortID,
		"bodyFile":             e2ePayloadFile(t, `{"title":"E2E Epic"}`),
	}, "sa-token", wsPod)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.Status)
	var epic struct {
		ID      string `json:"id"`
		ShortID string `json:"short_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &epic))

	resp, err = dispatchBoardManagerTool(ctx, client, e2eMCPPassword, "board_create_task", map[string]any{
		"lsp_injected_session": mgrSession,
		"epicId":               epic.ShortID,
		"bodyFile": e2ePayloadFile(t, `{"title":"E2E Task","description":"charter",`+
			`"acceptanceCriteria":[{"text":"e2e criterion met","met":false}]}`),
	}, "sa-token", wsPod)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.Status)
	var task struct {
		ID      string `json:"id"`
		ShortID string `json:"short_id"`
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

	// --- B: identity-less dispatch PROCEEDS with empty attribution ----------------
	// (ruling 2026-10-27 / tsk-copper-refectory-generates: identity is
	// audit attribution, never a grant — the #1747 §5 fail-closed belt
	// is repealed; tonight's live failure was exactly this call shape.)
	before := boardE2EEventCount(t, ctx, pool)
	resp, err = dispatchBoardManagerTool(ctx, client, e2eMCPPassword, "board_create_epic", map[string]any{
		"projectId": proj.ShortID,
		"bodyFile":  e2ePayloadFile(t, `{"title":"identity-less"}`),
	}, "sa-token", wsPod)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.Status, resp.Body)
	require.Equal(t, before+1, boardE2EEventCount(t, ctx, pool))
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM board_events
		WHERE item_kind = 'epic' AND (actor->>'session_id' IS NULL OR actor->>'session_id' = '')`,
	).Scan(&n))
	assert.GreaterOrEqual(t, n, 1, "the identity-less create lands with an UNATTRIBUTED actor — not refused")

	// --- C: a spawned worker's manager call PROCEEDS (prompting concern,
	// not RBAC — the claim-token fence is the real in-flight protection) ----
	resp, err = dispatchBoardManagerTool(ctx, client, e2eMCPPassword, "board_epic_activate", map[string]any{
		"lsp_injected_session": mgrSession,
		"epicId":               epic.ShortID,
	}, "sa-token", wsPod)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.Status)

	claim, err := svc.ClaimNext(ctx, "e2e-worker", wsPod)
	require.NoError(t, err)
	require.NotNil(t, claim)
	require.Equal(t, task.ID, claim.TaskID)

	resp, err = dispatchBoardManagerTool(ctx, client, e2eMCPPassword, "board_create_task", map[string]any{
		"lsp_injected_session": claim.Assignee,
		"epicId":               epic.ShortID,
		"bodyFile": e2ePayloadFile(t, `{"title":"worker manager call","description":"seeded charter",`+
			`"acceptanceCriteria":[{"text":"c","met":false}]}`),
	}, "sa-token", wsPod)
	require.NoError(t, err, "a spawned worker holds the manager tools post-repeal")
	require.Equal(t, http.StatusCreated, resp.Status, resp.Body)
	var workerTask struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &workerTask))
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM board_events
		WHERE item_kind = 'task' AND item_id = $1 AND actor->>'session_id' = $2`,
		workerTask.ID, claim.Assignee).Scan(&n))
	assert.Equal(t, 1, n, "the worker-attributed manager create lands in the audit trail")

	// --- D: the cross-workspace binding -----------------------------------------
	foreign, err := svc.CreateProject(ctx,
		board.Actor{Kind: board.ActorUser, UserID: "user-7", Surface: board.SurfaceREST},
		board.ProjectInput{Name: "Foreign", TargetWorkspaceID: wsForeign}, "")
	require.NoError(t, err)
	_, err = dispatchBoardManagerTool(ctx, client, e2eMCPPassword, "board_create_epic", map[string]any{
		"lsp_injected_session": mgrSession,
		"projectId":            foreign.ShortID,
		"bodyFile":             e2ePayloadFile(t, `{"title":"nope"}`),
	}, "sa-token", wsPod)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403")
	assert.Contains(t, err.Error(), board.GuardWorkspaceBinding)

	// --- E: the manager-grade move exits pending_input end to end (#1832 WS2) ----
	// The tsk-3do6n458 freeze class: an escalated row (attempts at cap,
	// no assignee) that neither the worker surface NOR the manager
	// surface could exit (pre-#1832). The manager move lands it
	// backlog, attempts
	// reset, audit carrying the session identity. The escalation's own
	// engine path (attempts>=cap -> pending_input/escalated) is pinned
	// by pkg/board's suite; here we pin the EXIT through the real seam,
	// so the row is staged in the escalation's aftermath shape
	// directly (section C's claim left it in_progress — a backlog-only
	// candidate predicate is not this section's subject).
	_, err = pool.Exec(ctx, `
		UPDATE tasks SET stage='pending_input', stage_entered_at=now(),
			pending_origin='escalated', attempts=3, claim_token=NULL, lease_expires_at=NULL
		WHERE id=$1`, task.ID)
	require.NoError(t, err)

	resp, err = dispatchBoardManagerTool(ctx, client, e2eMCPPassword, "board_move_stage", map[string]any{
		"lsp_injected_session": mgrSession,
		"taskId":               task.ShortID,
		"toStage":              "backlog",
	}, "sa-token", wsPod)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.Status, resp.Body)
	var moved board.Task
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &moved))
	assert.Equal(t, board.StageBacklog, moved.Stage)
	assert.Zero(t, moved.Attempts, "re-pooled with the fresh-judgment ledger")

	// --- G+H (#1837, relabeled past main's section E): composed
	// file→store byte-identical + corruption refusal — the real
	// dispatch, the real wire, the real engine, real PG.
	e2eFileBackedCharterSections(t, ctx, client, svc, pool, &epic, mgrSession, wsPod)

	// --- I: the config tools end to end (board_get_config /
	// board_set_config) — the file payload through the real dispatch,
	// wire, seam, engine, and PG; the write proven by the follow-up
	// read; the tier refusals and the #1747 foreign-workspace binding
	// on the project set.
	e2eBoardConfigSections(t, ctx, client, svc, pool, &proj, mgrSession, wsPod)
}

// e2eFileBackedCharterSections (#1837 rounds 2/3, relabeled G+H past
// main's section E): the composed real-wiring arcs for P1 —
// file → wire → engine → PG byte-identical, and corruption → 400 with
// nothing stored.
func e2eFileBackedCharterSections(t *testing.T, ctx context.Context, client *opencode.Client, svc *board.Service, pool *pgxpool.Pool, epic *struct {
	ID      string `json:"id"`
	ShortID string `json:"short_id"`
}, mgrSession, wsPod string) {
	t.Helper()

	// --- G: composed file → wire → store, byte-identical -------------------------
	var charter strings.Builder
	for i := 0; i < 130; i++ {
		charter.WriteString("### Charter section number ")
		charter.WriteString(strconv.Itoa(i))
		charter.WriteString("\nDistinct numbered guidance payload ")
		charter.WriteString(strconv.Itoa(i))
		charter.WriteString(" for the worker.\n")
	}
	content := charter.String()
	require.Greater(t, len(content), 4096)
	charterJSON, err := json.Marshal(content)
	require.NoError(t, err)

	resp, err := dispatchBoardManagerTool(ctx, client, e2eMCPPassword, "board_create_task", map[string]any{
		"lsp_injected_session": mgrSession,
		"epicId":               epic.ShortID,
		"bodyFile": e2ePayloadFile(t, `{"title":"file-backed charter","description":`+string(charterJSON)+`,`+
			`"acceptanceCriteria":[{"text":"byte-identical storage","met":false}]}`),
	}, "sa-token", wsPod)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.Status)

	var fileTask struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &fileTask))
	var storedDesc string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT description FROM tasks WHERE id=$1`, fileTask.ID).Scan(&storedDesc))
	require.Equal(t, content, storedDesc, "file bytes → wire → PG row, byte-identical")

	// --- H0 (#1849 + design 0070): a seedless create through the real
	// dispatch chain now refuses LOCALLY at the CRUD-time validation
	// gate — the same findings board_validate_file returns; nothing
	// reaches the engine.
	_, err = dispatchBoardManagerTool(ctx, client, e2eMCPPassword, "board_create_task", map[string]any{
		"lsp_injected_session": mgrSession,
		"epicId":               epic.ShortID,
		"bodyFile":             e2ePayloadFile(t, `{"title":"seedless"}`),
	}, "sa-token", wsPod)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "local validation")
	assert.Contains(t, err.Error(), "description")
	assert.Contains(t, err.Error(), `"rule":"required"`)

	// --- F: composed corruption refusal — nothing stored ------------------------
	// --- H: composed corruption refusal — nothing stored ------------------------
	before := boardE2EEventCount(t, ctx, pool)
	corruptJSON, err := json.Marshal("prefix " + strings.Repeat("Retry the loop step and continue. ", 50) + " suffix")
	require.NoError(t, err)
	_, err = dispatchBoardManagerTool(ctx, client, e2eMCPPassword, "board_create_task", map[string]any{
		"lsp_injected_session": mgrSession,
		"epicId":               epic.ShortID,
		"bodyFile": e2ePayloadFile(t, `{"title":"corrupted","description":`+string(corruptJSON)+`,`+
			`"acceptanceCriteria":[{"text":"c","met":false}]}`),
	}, "sa-token", wsPod)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "400")
	assert.Contains(t, err.Error(), board.GuardTextRejected)
	assert.Equal(t, before, boardE2EEventCount(t, ctx, pool), "a refused write stores nothing")
}

// e2ePayloadFile writes one JSON payload file (the file-crud cutover's
// fixture helper — every payload-bearing dispatch rides a file now,
// design 0070).
func e2ePayloadFile(t *testing.T, payload string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "payload.json")
	require.NoError(t, os.WriteFile(path, []byte(payload), 0o600))
	return path
}

// e2eBoardConfigSections (section I): the config tools' composed
// real-wiring arc — board_get_config both tiers and board_set_config's
// configFile payload through the real dispatch → loopback → seam →
// BoardService → PG, the write proven by the follow-up read, and the
// guard shapes (tier routing refusals, the #1747 foreign-workspace
// binding on the project set) riding the same wire.
func e2eBoardConfigSections(t *testing.T, ctx context.Context, client *opencode.Client, svc *board.Service, pool *pgxpool.Pool, proj *struct {
	ID      string `json:"id"`
	ShortID string `json:"short_id"`
}, mgrSession, wsPod string) {
	t.Helper()

	// The read before any write: the user tier renders the code
	// defaults, every key sourced code_default.
	resp, err := dispatchBoardManagerTool(ctx, client, e2eMCPPassword, "board_get_config", map[string]any{
		"lsp_injected_session": mgrSession,
	}, "sa-token", wsPod)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.Status, resp.Body)
	var userView board.BoardConfigView
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &userView))
	assert.Equal(t, "user", userView.Scope)
	assert.Equal(t, board.ConfigSourceCodeDefault, userView.Sources["autoAssignReviewer"])

	// The user-tier write: the config map rides its FILE (design
	// 0070), the tier selector inline. The response is the annotated
	// view of the written tier.
	resp, err = dispatchBoardManagerTool(ctx, client, e2eMCPPassword, "board_set_config", map[string]any{
		"lsp_injected_session": mgrSession,
		"tier":                 "user",
		"configFile": e2ePayloadFile(t, `{"autoAssignReviewer":true,`+
			`"stagePersonas":{"in_review":{"name":"E2E Reviewer",`+
			`"instructions":"Read the acceptance criteria; refuse hollow evidence."}}}`),
	}, "sa-token", wsPod)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.Status, resp.Body)
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &userView))
	assert.Equal(t, "user", userView.Scope)
	assert.Equal(t, board.ConfigSourceUserDefault, userView.Sources["autoAssignReviewer"])
	assert.True(t, userView.Effective.AutoAssignReviewer)
	rev := userView.Personas["in_review"]
	assert.Equal(t, board.PersonaSourceUserDefault, rev.Source)
	require.NotNil(t, rev.Persona, "the stored persona renders in the view")
	assert.Equal(t, "E2E Reviewer", rev.Persona.Name)

	// The write landed in PG (the user defaults table carries the row).
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM board_config_defaults WHERE user_id='user-7' AND key='stagePersonas'`).Scan(&n))
	assert.Equal(t, 1, n, "the stagePersonas write reached the store")

	// The project tier's read: no override stored yet — the persona
	// inherits from the user tier.
	resp, err = dispatchBoardManagerTool(ctx, client, e2eMCPPassword, "board_get_config", map[string]any{
		"lsp_injected_session": mgrSession,
		"projectId":            proj.ShortID,
	}, "sa-token", wsPod)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.Status, resp.Body)
	var projView board.BoardConfigView
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &projView))
	assert.Equal(t, "project", projView.Scope)
	assert.Equal(t, board.ConfigSourceUserDefault, projView.Sources["autoAssignReviewer"],
		"an unset project key inherits the user tier's stored value")
	assert.Equal(t, board.PersonaSourceUserDefault, projView.Personas["in_review"].Source)

	// The project-tier write: the explicit {} stagePersonas opt-out —
	// the map OWNS every stage it does not list (the built-in
	// auto-reviewer included), rendered none_explicit.
	resp, err = dispatchBoardManagerTool(ctx, client, e2eMCPPassword, "board_set_config", map[string]any{
		"lsp_injected_session": mgrSession,
		"tier":                 "project",
		"projectId":            proj.ShortID,
		"configFile":           e2ePayloadFile(t, `{"stagePersonas":{}}`),
	}, "sa-token", wsPod)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.Status, resp.Body)
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &projView))
	assert.Equal(t, "project", projView.Scope)
	assert.Equal(t, board.PersonaSourceNoneExplicit, projView.Personas["in_review"].Source,
		"the explicit empty map owns every stage — the opt-out renders, never silently")
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM board_project_config_overrides WHERE project_id=$1 AND key='stagePersonas'`,
		proj.ID).Scan(&n))
	assert.Equal(t, 1, n, "the project override write reached the store")

	// The tier routing refusals ride the same wire: a bogus tier, and
	// projectId on the user tier.
	_, err = dispatchBoardManagerTool(ctx, client, e2eMCPPassword, "board_set_config", map[string]any{
		"lsp_injected_session": mgrSession,
		"tier":                 "board",
		"configFile":           e2ePayloadFile(t, `{}`),
	}, "sa-token", wsPod)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "400")

	_, err = dispatchBoardManagerTool(ctx, client, e2eMCPPassword, "board_set_config", map[string]any{
		"lsp_injected_session": mgrSession,
		"tier":                 "user",
		"projectId":            proj.ShortID,
		"configFile":           e2ePayloadFile(t, `{}`),
	}, "sa-token", wsPod)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "400")
	assert.Contains(t, err.Error(), "user-tier set")

	// The unknown-key key-gate refuses LOCALLY before any wire call
	// (design 0070's CRUD-time gate — the same findings
	// board_validate_file returns), and nothing lands.
	before := boardE2EEventCount(t, ctx, pool)
	_, err = dispatchBoardManagerTool(ctx, client, e2eMCPPassword, "board_set_config", map[string]any{
		"lsp_injected_session": mgrSession,
		"tier":                 "project",
		"projectId":            proj.ShortID,
		"configFile":           e2ePayloadFile(t, `{"reviewQueue":"off"}`),
	}, "sa-token", wsPod)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed local validation")
	assert.Contains(t, err.Error(), "reviewQueue")
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM board_project_config_overrides WHERE project_id=$1 AND key='reviewQueue'`,
		proj.ID).Scan(&n))
	assert.Zero(t, n, "the refused set writes nothing")
	assert.Equal(t, before, boardE2EEventCount(t, ctx, pool), "no event for a refused config set")

	// The #1747 foreign-workspace binding on the project set: a
	// project in ANOTHER workspace refuses at the binding guard —
	// never a write, never a disclosure.
	foreign, err := svc.CreateProject(ctx,
		board.Actor{Kind: board.ActorUser, UserID: "user-7", Surface: board.SurfaceREST},
		board.ProjectInput{Name: "ForeignCfg", TargetWorkspaceID: "22222222-2222-2222-2222-222222222202"}, "")
	require.NoError(t, err)
	_, err = dispatchBoardManagerTool(ctx, client, e2eMCPPassword, "board_set_config", map[string]any{
		"lsp_injected_session": mgrSession,
		"tier":                 "project",
		"projectId":            foreign.ShortID,
		"configFile":           e2ePayloadFile(t, `{"reviewColumn":"manual"}`),
	}, "sa-token", wsPod)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403")
	assert.Contains(t, err.Error(), board.GuardWorkspaceBinding)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM board_project_config_overrides WHERE project_id=$1`, foreign.ID).Scan(&n))
	assert.Zero(t, n, "the binding refusal precedes the write")
}
