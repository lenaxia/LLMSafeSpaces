// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// board_automation_test.go — the r1-S1 handler-level pins: the worker
// seam mounts on the pod-identity resolve (TokenReview → SA principal →
// workspace pin) and every board action carries the resolved owner AND
// workspace into the BoardService actor. The fake service records the
// actor — asserting the adapter stamps surface=agent, the workspace
// binding, and the claim token; the unscoped reader is never called.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// fakeBoardService records actors + calls; returns configured outcomes.
type fakeBoardService struct {
	lastActor     board.Actor
	lastWorkspace string
	lastTaskRef   string
	lastShortID   string
	lastToStage   string
	calls         map[string]int

	heartbeatErr error
	moveErr      error // board_parity_test.go: the shared-action guard-class arms
	getErr       error
	getTask      *board.Task
}

func newFakeBoardService() *fakeBoardService {
	return &fakeBoardService{calls: map[string]int{}}
}

func (f *fakeBoardService) Heartbeat(_ context.Context, actor board.Actor, taskID, shortIDOrID string, _ time.Time) (board.HeartbeatResult, error) {
	f.calls["heartbeat"]++
	f.lastActor, f.lastTaskRef, f.lastShortID = actor, taskID, shortIDOrID
	if f.heartbeatErr != nil {
		return board.HeartbeatResult{}, f.heartbeatErr
	}
	return board.HeartbeatResult{Extended: true}, nil
}

func (f *fakeBoardService) ReportThrottle(_ context.Context, actor board.Actor, taskID, shortIDOrID string, _ time.Duration, _ string) (*board.Task, error) {
	f.calls["throttle"]++
	f.lastActor, f.lastTaskRef, f.lastShortID = actor, taskID, shortIDOrID
	return &board.Task{ID: taskID, Stage: board.StageBacklog}, nil
}

func (f *fakeBoardService) MoveStage(_ context.Context, actor board.Actor, taskID, shortIDOrID, toStage, _, _ string) (*board.Task, error) {
	f.calls["move_stage"]++
	f.lastActor, f.lastTaskRef, f.lastShortID, f.lastToStage = actor, taskID, shortIDOrID, toStage
	if f.moveErr != nil {
		return nil, f.moveErr
	}
	return &board.Task{ID: taskID, Stage: toStage}, nil
}

func (f *fakeBoardService) GetTaskInWorkspace(_ context.Context, workspaceID, taskID, shortIDOrID string) (*board.Task, error) {
	f.calls["get"]++
	f.lastWorkspace, f.lastTaskRef, f.lastShortID = workspaceID, taskID, shortIDOrID
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.getTask, nil
}

// newBoardAutomationRouter mounts the board seam exactly as the router
// does, over the fake service (resolve() is REAL: fake TokenReview +
// workspace lookup — the auth chain is exercised, not stubbed).
func newBoardAutomationRouter(t *testing.T, svc *fakeBoardService) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := &PodAutomationHandler{
		tokenReviewer:     automationReviewer(),
		lookup:            automationLookup(),
		expectedNamespace: testRenameNamespace,
		board:             svc,
		boardEngine:       board.EngineConfig{ThrottleBackoff: 5 * time.Minute},
	}
	r := gin.New()
	b := r.Group("/internal/v1/automation/board")
	b.GET("/tasks/:id", h.BoardTaskGet)
	b.POST("/tasks/heartbeat", h.BoardHeartbeat)
	b.POST("/tasks/throttle", h.BoardReportThrottle)
	b.POST("/tasks/move-stage", h.BoardMoveStage)
	return r
}

func doBoard(t *testing.T, r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer whatever-the-reviewer-accepts")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestBoardAutomation_ActorStamping: every action stamps the delegation —
// the pod's resolved owner (ws-1 → user-7), the agent surface, the
// presented token, and the resolved workspace (the structural binding,
// r1-S1).
func TestBoardAutomation_ActorStamping(t *testing.T) {
	svc := newFakeBoardService()
	r := newBoardAutomationRouter(t, svc)

	w := doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/heartbeat",
		`{"workspaceID":"ws-1","shortId":"tsk-aaaa1111","claimToken":"tok-1","observedProgressAt":"2026-10-17T00:00:00Z"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 1, svc.calls["heartbeat"])
	assert.Equal(t, "user-7", svc.lastActor.UserID, "the pod's resolved owner delegates (R7)")
	assert.Equal(t, board.SurfaceAgent, svc.lastActor.Surface)
	assert.Equal(t, "tok-1", svc.lastActor.ClaimToken)
	assert.Equal(t, "ws-1", svc.lastActor.WorkspaceID, "the resolved workspace rides the actor (S1)")
	assert.Equal(t, "tsk-aaaa1111", svc.lastShortID)

	w = doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/throttle",
		`{"workspaceID":"ws-1","shortId":"tsk-aaaa1111","claimToken":"tok-1","retryAfterSeconds":60}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "ws-1", svc.lastActor.WorkspaceID)

	w = doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/move-stage",
		`{"workspaceID":"ws-1","shortId":"tsk-aaaa1111","claimToken":"tok-1","toStage":"in_review"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, board.StageInReview, svc.lastToStage)
	assert.Equal(t, "ws-1", svc.lastActor.WorkspaceID)
}

// TestBoardAutomation_ReadIsWorkspaceScoped: the reader resolves through
// the WORKSPACE-SCOPED service method with the pod's resolved workspace —
// the unscoped reader that leaked cross-tenant rows (r1-S1) is never on
// this path.
func TestBoardAutomation_ReadIsWorkspaceScoped(t *testing.T) {
	svc := newFakeBoardService()
	svc.getTask = &board.Task{ID: "t-1", ShortID: "tsk-aaaa1111"}
	r := newBoardAutomationRouter(t, svc)

	w := doBoard(t, r, http.MethodGet, "/internal/v1/automation/board/tasks/tsk-aaaa1111?workspaceID=ws-1", "")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 1, svc.calls["get"])
	assert.Equal(t, "ws-1", svc.lastWorkspace, "the read carries the pod's workspace — tenant-scoped by construction")

	// A foreign-task miss renders as 404 with the suggestions the SCOPED
	// service produced (cross-tenant suggestions are the service-level
	// pin; the handler passes them verbatim).
	svc2 := newFakeBoardService()
	svc2.getErr = &board.NotFoundError{Item: "task", Identifier: "tsk-zzz"}
	r2 := newBoardAutomationRouter(t, svc2)
	w = doBoard(t, r2, http.MethodGet, "/internal/v1/automation/board/tasks/tsk-zzz?workspaceID=ws-1", "")
	require.Equal(t, http.StatusNotFound, w.Code)
	var body struct {
		Suggestions []string `json:"suggestions"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Empty(t, body.Suggestions)
}

// TestBoardAutomation_ErrorClasses: the guard classes map to their HTTP
// outcomes on the seam (the parity matrix's pinned mapping).
func TestBoardAutomation_ErrorClasses(t *testing.T) {
	svc := newFakeBoardService()
	svc.heartbeatErr = &board.GuardError{Code: board.GuardClaimToken, Detail: "nope"}
	r := newBoardAutomationRouter(t, svc)
	w := doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/heartbeat",
		`{"workspaceID":"ws-1","shortId":"tsk-aaaa1111","claimToken":"stale"}`)
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), board.GuardClaimToken)

	svc.heartbeatErr = &board.GuardError{Code: board.GuardWorkspaceBinding, Detail: "foreign task"}
	w = doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/heartbeat",
		`{"workspaceID":"ws-1","shortId":"tsk-foreign","claimToken":"leaked"}`)
	require.Equal(t, http.StatusForbidden, w.Code, "the workspace binding renders 403")
}

// TestBoardAutomation_WorkspaceIdentityMismatch: the REAL resolve pins the
// pod to its own workspace — a pod presenting another workspaceID never
// reaches the service.
func TestBoardAutomation_WorkspaceIdentityMismatch(t *testing.T) {
	svc := newFakeBoardService()
	r := newBoardAutomationRouter(t, svc)
	// The SA principal is workspace-ws-1; asking for ws-OTHER's scope is a
	// 403 before any board call.
	req := httptest.NewRequest(http.MethodGet, "/internal/v1/automation/board/tasks/tsk-x?workspaceID=ws-OTHER", nil)
	req.Header.Set("Authorization", "Bearer x")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusForbidden, w.Code)
	assert.Zero(t, svc.calls["get"], "resolve rejects before the service sees anything")
}
