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
	lastCheck     *board.CriteriaCheck
	calls         map[string]int

	heartbeatErr  error
	moveErr       error // board_parity_test.go: the shared-action guard-class arms
	getErr        error
	getTask       *board.Task
	verdictErr    error               // #1947: the verdict channel's guard-class arm
	lastVerdict   *board.VerdictInput // #1947: the handler's payload pass-through
	lastRequestID string
	setCritErr    error // #1946: the criteria-met grant's guard-class arm
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

func (f *fakeBoardService) RecordVerdict(_ context.Context, actor board.Actor, taskID, shortIDOrID string, in board.VerdictInput, requestID string) (*board.Task, *board.Verdict, error) {
	f.calls["record_verdict"]++
	f.lastActor, f.lastTaskRef, f.lastShortID = actor, taskID, shortIDOrID
	vin := in
	f.lastVerdict, f.lastRequestID = &vin, requestID
	if f.verdictErr != nil {
		return nil, nil, f.verdictErr
	}
	return &board.Task{ID: taskID, Stage: board.StageInProgress}, &board.Verdict{Verdict: in.Verdict, Applied: true}, nil
}

func (f *fakeBoardService) SetCriteriaMet(_ context.Context, actor board.Actor, taskID, shortIDOrID string, check board.CriteriaCheck, _ string) (*board.Task, error) {
	f.calls["criteria_met"]++
	f.lastActor, f.lastTaskRef, f.lastShortID, f.lastCheck = actor, taskID, shortIDOrID, &check
	if f.setCritErr != nil {
		return nil, f.setCritErr
	}
	out := &board.Task{ID: taskID, Stage: board.StageInReview, ClaimPersona: "reviewer",
		AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "criterion 0"}, {Text: "criterion 1"}}}
	if check.Index >= 0 && check.Index < len(out.AcceptanceCriteria) {
		out.AcceptanceCriteria[check.Index].Met = check.Met
	}
	return out, nil
}

// newBoardAutomationRouter mounts the board seam exactly as the router
// does, over the fake service (resolve() is REAL: fake TokenReview +
// workspace lookup — the auth chain is exercised, not stubbed). #1747:
// the manager half rides the fakeBoardManager projection.
func newBoardAutomationRouter(t *testing.T, svc *fakeBoardService) *gin.Engine {
	return newBoardAutomationRouterWithManager(t, svc, newFakeBoardManager())
}

// newBoardAutomationRouterWithManager mounts the FULL seam (worker +
// manager routes) with independent fakes for the two projections.
func newBoardAutomationRouterWithManager(t *testing.T, svc *fakeBoardService, mgr *fakeBoardManager) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := &PodAutomationHandler{
		tokenReviewer:     automationReviewer(),
		lookup:            automationLookup(),
		expectedNamespace: testRenameNamespace,
		board:             svc,
		boardEngine:       board.EngineConfig{ThrottleBackoff: 5 * time.Minute},
		boardMgr:          mgr,
	}
	r := gin.New()
	b := r.Group("/internal/v1/automation/board")
	b.GET("/tasks/:id", h.BoardTaskGet)
	b.GET("/projects/:id/tasks", h.BoardManagerListTasks)
	b.GET("/health", h.BoardManagerHealth)
	b.POST("/tasks/heartbeat", h.BoardHeartbeat)
	b.POST("/tasks/throttle", h.BoardReportThrottle)
	b.POST("/tasks/verdict", h.BoardRecordVerdict) // #1947: the spawned reviewer's verdict channel
	b.POST("/tasks/move-stage", h.BoardMoveStage)
	b.POST("/tasks/set-criteria-met", h.BoardSetCriteriaMet)      // #1946 reviewer-persona scoped grant
	b.POST("/tasks/move-stage-bulk", h.BoardManagerMoveStageBulk) // #1836 bulk complete
	b.POST("/tasks/delete-bulk", h.BoardManagerDeleteTasksBulk)   // #1836 sibling: bulk delete
	b.POST("/projects", h.BoardCreateProject)
	b.PATCH("/projects/:id", h.BoardUpdateProject)
	b.POST("/projects/:id/archive", h.BoardArchiveProject)
	b.POST("/projects/:id/unarchive", h.BoardUnarchiveProject)
	b.POST("/projects/:id/pause", h.BoardPauseProject)
	b.POST("/projects/:id/resume", h.BoardResumeProject)
	b.POST("/projects/:id/epics", h.BoardCreateEpic)
	b.POST("/projects/:id/import-plan", h.BoardImportPlan)
	b.PATCH("/epics/:id", h.BoardUpdateEpic)
	b.POST("/epics/:id/activate", h.BoardEpicActivate)
	b.POST("/epics/:id/pause", h.BoardEpicPause)
	b.POST("/epics/:id/archive", h.BoardEpicArchive)
	b.POST("/epics/:id/unarchive", h.BoardEpicUnarchive)
	b.DELETE("/epics/:id", h.BoardDeleteEpic)
	b.POST("/epics/:id/tasks", h.BoardCreateTask)
	b.PATCH("/tasks/:id", h.BoardUpdateTask)
	b.POST("/tasks/:id/move-stage", h.BoardManagerMoveStage) // #1832 WS2
	b.POST("/tasks/:id/rearm", h.BoardRearmTask)             // #1832 WS3
	b.POST("/tasks/:id/pause", h.BoardPauseTask)
	b.POST("/tasks/:id/resume", h.BoardResumeTask)
	b.DELETE("/tasks/:id", h.BoardDeleteTask)
	b.POST("/tasks/:id/dependencies", h.BoardAddDependency)
	b.DELETE("/tasks/:id/dependencies/:dependsOnId", h.BoardRemoveDependency)
	b.POST("/resource-pressure", h.BoardResourcePressure)
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

// TestBoardAutomation_VerdictChannel (#1947): the spawned reviewer
// persona's route — actor stamping (pod-resolved owner, agent surface,
// presented claim token, workspace binding), payload pass-through, the
// guard-class HTTP mapping the tool description promises, and the
// explicit malformed-findings refusal.
func TestBoardAutomation_VerdictChannel(t *testing.T) {
	svc := newFakeBoardService()
	r := newBoardAutomationRouter(t, svc)

	// Actor stamping + payload pass-through (one full verdict call).
	w := doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/verdict",
		`{"workspaceID":"ws-1","shortId":"tsk-aaaa1111","claimToken":"tok-9","verdict":"fail","reviewer":"spawned-reviewer","evidenceUrl":"https://example.test/pr/9","findings":[{"severity":"major","pointer":"pkg/x.go:9","criterionIndex":0}],"requestId":"req-42"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 1, svc.calls["record_verdict"])
	assert.Equal(t, "user-7", svc.lastActor.UserID, "the pod's resolved owner delegates (R7)")
	assert.Equal(t, board.SurfaceAgent, svc.lastActor.Surface)
	assert.Equal(t, "tok-9", svc.lastActor.ClaimToken, "the reviewer's charter token rides the actor")
	assert.Equal(t, "ws-1", svc.lastActor.WorkspaceID, "the resolved workspace rides the actor (S1)")
	assert.Equal(t, "tsk-aaaa1111", svc.lastShortID)
	require.NotNil(t, svc.lastVerdict)
	assert.Equal(t, board.VerdictFail, svc.lastVerdict.Verdict)
	assert.Equal(t, "spawned-reviewer", svc.lastVerdict.Reviewer)
	assert.Equal(t, "https://example.test/pr/9", svc.lastVerdict.EvidenceURL)
	require.Len(t, svc.lastVerdict.Findings, 1)
	assert.Equal(t, board.SeverityMajor, svc.lastVerdict.Findings[0].Severity)
	assert.Equal(t, "pkg/x.go:9", svc.lastVerdict.Findings[0].Pointer)
	require.NotNil(t, svc.lastVerdict.Findings[0].CriterionIndex)
	assert.Equal(t, 0, *svc.lastVerdict.Findings[0].CriterionIndex)
	assert.Equal(t, "req-42", svc.lastRequestID)
	assert.Contains(t, w.Body.String(), `"applied":true`, "the verdict envelope renders")

	// The claim-token guard class maps to 409 — the exact contract the
	// agentd tool description promises ("A 409 with code claim_token").
	svc.verdictErr = &board.GuardError{Code: board.GuardClaimToken, Detail: "an agent-surface verdict requires the in_review row's live claim token"}
	w = doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/verdict",
		`{"workspaceID":"ws-1","shortId":"tsk-aaaa1111","claimToken":"stale","verdict":"fail","reviewer":"r"}`)
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), board.GuardClaimToken)

	// A malformed findings payload is an EXPLICIT 400 (the bind names
	// the field; nothing is silently dropped).
	w = doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/verdict",
		`{"workspaceID":"ws-1","shortId":"tsk-aaaa1111","claimToken":"tok-9","verdict":"fail","reviewer":"r","findings":"not-an-array"}`)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "invalid request body", "the malformed findings arm names its cause")
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

// --- #1747 manager seam: actor stamping, session attribution (the
// grant belt is REPEALED — ruling 2026-10-27 /
// tsk-copper-refectory-generates) ------------------------------

// TestBoardAutomation_ManagerActorStamping: a manager call stamps the
// full #1747 delegation — the pod's resolved owner (ws-1 → user-7),
// the manager-grade agent surface (#1832 WS2: a DISTINCT audit
// vocabulary from the worker projection), the structural workspace
// binding, AND the creating session's identity (audit attribution) —
// on the manager projection.
func TestBoardAutomation_ManagerActorStamping(t *testing.T) {
	mgr := newFakeBoardManager()
	r := newBoardAutomationRouterWithManager(t, newFakeBoardService(), mgr)

	w := doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/epics/epc-bbbb2222/tasks",
		`{"workspaceID":"ws-1","sessionId":"ses_author1","title":"T1","dependsOn":["tsk-aaaa1111"]}`)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Equal(t, 1, mgr.calls["create_task"], "the manager projection serves the route")
	require.NotNil(t, mgr.lastActor)
	assert.Equal(t, board.ActorUser, mgr.lastActor.Kind)
	assert.Equal(t, "user-7", mgr.lastActor.UserID, "the pod's resolved owner (R7 delegation)")
	assert.Equal(t, board.SurfaceAgentManager, mgr.lastActor.Surface,
		"#1832 WS2: the manager-grade surface — the audit distinction tonight's freeze diagnosis needed")
	assert.Equal(t, "ws-1", mgr.lastActor.WorkspaceID, "the structural caller-workspace binding")
	assert.Equal(t, "ses_author1", mgr.lastActor.SessionID, "audit attribution to the creating session (#1747)")
	assert.Empty(t, mgr.lastActor.ClaimToken, "manager actions hold no claim token")
}

// TestBoardAutomation_ManagerGrantRepealed (#1747 §5 repeal, ruling
// 2026-10-27 / tsk-copper-refectory-generates): the manager routes no
// longer refuse ANY in-workspace session — neither a session that
// currently holds a board worker role (the old live-assignee belt) nor
// a caller that omits its session identity entirely. The resolved
// sessionId rides the actor as audit attribution only.
func TestBoardAutomation_ManagerGrantRepealed(t *testing.T) {
	mgr := newFakeBoardManager()
	r := newBoardAutomationRouterWithManager(t, newFakeBoardService(), mgr)

	// A live worker session (the old belt's refusal class) → CREATED;
	// the attribution still records the worker's identity.
	w := doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/epics/epc-bbbb2222/tasks",
		`{"workspaceID":"ws-1","sessionId":"ses_liveworker","title":"T1"}`)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Equal(t, 1, mgr.calls["create_task"], "the manager projection serves the worker session")
	require.NotNil(t, mgr.lastActor)
	assert.Equal(t, "ses_liveworker", mgr.lastActor.SessionID, "the worker's identity still attributes the audit actor")

	// An identity-LESS call (the old omission 403) → CREATED; the actor
	// lands unattributed, never refused.
	w = doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/epics/epc-bbbb2222/tasks",
		`{"workspaceID":"ws-1","title":"T2"}`)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String(), "an omitted sessionId proceeds — attribution-only identity")
	assert.Equal(t, 2, mgr.calls["create_task"])
	assert.Empty(t, mgr.lastActor.SessionID, "no identity supplied — unattributed audit actor")

	// DELETE-class routes carried the session on the QUERY — the same
	// repeal holds (query-carried identity omission proceeds).
	w = doBoard(t, r, http.MethodDelete, "/internal/v1/automation/board/tasks/tsk-cccc3333?workspaceID=ws-1", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String(), "query-carried identity omission proceeds too")
	assert.Equal(t, 1, mgr.calls["delete_task"])
}

// TestBoardAutomation_ManagerCreateProjectScoping: the seam stamps the
// project's target workspace to the caller's OWN workspace — a caller
// spelling for another tenant's workspace is overwritten (structural
// scope, the scopeTriggerCreateBody discipline).
func TestBoardAutomation_ManagerCreateProjectScoping(t *testing.T) {
	mgr := newFakeBoardManager()
	r := newBoardAutomationRouterWithManager(t, newFakeBoardService(), mgr)

	w := doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/projects",
		`{"workspaceID":"ws-1","sessionId":"ses_author1","name":"B","targetWorkspaceId":"ws-someone-else"}`)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Equal(t, 1, mgr.calls["create_project"])
	require.NotNil(t, mgr.lastCreateProjectInput)
	assert.Equal(t, "ws-1", mgr.lastCreateProjectInput.TargetWorkspaceID,
		"the pod's own workspace wins over any caller spelling")
}

// TestBoardAutomation_ResourcePressureTelemetry: agentd's push route
// records the sample on the engine (the #1747 round-3 backpressure
// seam — the claim choke point consumes it).
func TestBoardAutomation_ResourcePressureTelemetry(t *testing.T) {
	mgr := newFakeBoardManager()
	r := newBoardAutomationRouterWithManager(t, newFakeBoardService(), mgr)

	w := doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/resource-pressure",
		`{"workspaceID":"ws-1","cpu":false,"memory":false,"disk":true,"disk_rate":0.92}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NotNil(t, mgr.lastPressureSample)
	assert.True(t, mgr.lastPressureSample.Disk)
	assert.InDelta(t, 0.92, mgr.lastPressureSample.DiskRate, 0.001)
}

// TestBoardAutomation_ManagerBeltRemoved (the r2 Security-1 fail-closed
// posture is REPEALED with the grant itself, ruling 2026-10-27 /
// tsk-copper-refectory-generates): the seam belt's omitted-session 403
// and belt-error 503 are gone — an omitted sessionId is empty
// attribution, not a refusal, and no belt query exists to fail.
func TestBoardAutomation_ManagerBeltRemoved(t *testing.T) {
	mgr := newFakeBoardManager()
	r := newBoardAutomationRouterWithManager(t, newFakeBoardService(), mgr)

	// No sessionId anywhere → the call PROCEEDS (previously 403).
	w := doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/epics/epc-bbbb2222/tasks",
		`{"workspaceID":"ws-1","title":"T1"}`)
	require.Equal(t, http.StatusCreated, w.Code, "an omitted sessionId proceeds — attribution-only identity")
	assert.Equal(t, 1, mgr.calls["create_task"])
}
