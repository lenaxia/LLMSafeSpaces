// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// board_rest_test.go — REST projection pins (design 0067 §4.3a, #1699
// Lane 2): actor stamping (surface=rest, JWT user, no claim token),
// error-class mapping identical to the internal seam's (the parity
// matrix's REST column), the read perimeter (foreign rows are plain
// 404s — never disclosed), requestId pass-through, and the request
// validation the adapter itself owns (binding, stage filter).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// fakeBoardManager records actor + call args; returns configured rows.
type fakeBoardManager struct {
	calls map[string]int

	lastActor     board.Actor
	lastRequestID string

	lastProjectID     string
	lastFailed        string
	lastSearchQuery   string
	lastSearchFilters board.SearchFilters
	lastHealthUser    string

	projects     []board.Project
	epics        []board.Epic
	tasks        []board.Task
	searchResult *board.SearchResult
	health       *board.Health

	errFor map[string]error

	// #1747 seam arms: the grant-belt answer (IsWorkerSession), the
	// create-project input capture, and the pressure sample sink.
	workerSession          string
	beltErr                error
	lastCreateProjectInput *board.ProjectInput
	lastPressureSample     *board.PressureSample
}

func (f *fakeBoardManager) IsWorkerSession(_ context.Context, sessionID string) (bool, error) {
	if f.beltErr != nil {
		return false, f.beltErr
	}
	return sessionID != "" && sessionID == f.workerSession, nil
}

func newFakeBoardManager() *fakeBoardManager {
	return &fakeBoardManager{
		calls:  map[string]int{},
		errFor: map[string]error{},
	}
}

func (f *fakeBoardManager) call(name string) { f.calls[name]++ }

func (f *fakeBoardManager) ListProjects(_ context.Context, userID string) ([]board.Project, error) {
	f.call("list_projects")
	if err := f.errFor["list_projects"]; err != nil {
		return nil, err
	}
	_ = userID
	return f.projects, nil
}

func (f *fakeBoardManager) GetProject(_ context.Context, _, _ string) (*board.Project, error) {
	f.call("get_project")
	if len(f.projects) > 0 {
		return &f.projects[0], nil
	}
	if err := f.errFor["get_project"]; err != nil {
		return nil, err
	}
	return nil, &board.NotFoundError{Item: "project", Identifier: "p1"}
}

func (f *fakeBoardManager) CreateProject(_ context.Context, actor board.Actor, in board.ProjectInput, requestID string) (*board.Project, error) {
	in2 := in
	f.lastCreateProjectInput = &in2
	f.call("create_project")
	f.lastActor, f.lastRequestID = actor, requestID
	if err := f.errFor["create_project"]; err != nil {
		return nil, err
	}
	return &board.Project{ID: "p1", ShortID: "prj-aaaa1111", UserID: actor.UserID, Name: in.Name, WorkerSlots: 2, Status: board.ProjectActive}, nil
}

func (f *fakeBoardManager) UpdateProject(_ context.Context, actor board.Actor, _, idOrShort string, in board.ProjectInput, requestID string) (*board.Project, error) {
	f.call("update_project")
	f.lastActor, f.lastRequestID = actor, requestID
	_ = idOrShort
	if err := f.errFor["update_project"]; err != nil {
		return nil, err
	}
	return &board.Project{ID: "p1", ShortID: "prj-aaaa1111", UserID: actor.UserID, Name: in.Name, WorkerSlots: 2, Status: board.ProjectActive}, nil
}

func (f *fakeBoardManager) ArchiveProject(_ context.Context, actor board.Actor, _, requestID string) (*board.Project, error) {
	f.call("archive_project")
	f.lastActor, f.lastRequestID = actor, requestID
	if err := f.errFor["archive_project"]; err != nil {
		return nil, err
	}
	return &board.Project{ID: "p1", ShortID: "prj-aaaa1111", UserID: actor.UserID, Status: board.ProjectArchived}, nil
}

func (f *fakeBoardManager) UnarchiveProject(_ context.Context, actor board.Actor, _, requestID string) (*board.Project, error) {
	f.call("unarchive_project")
	f.lastActor, f.lastRequestID = actor, requestID
	if err := f.errFor["unarchive_project"]; err != nil {
		return nil, err
	}
	return &board.Project{ID: "p1", ShortID: "prj-aaaa1111", UserID: actor.UserID, Status: board.ProjectActive}, nil
}

func (f *fakeBoardManager) GetEpic(_ context.Context, _, _ string) (*board.Epic, error) {
	f.call("get_epic")
	if len(f.epics) > 0 {
		return &f.epics[0], nil
	}
	if err := f.errFor["get_epic"]; err != nil {
		return nil, err
	}
	return nil, &board.NotFoundError{Item: "epic", Identifier: "e1"}
}

func (f *fakeBoardManager) CreateEpic(_ context.Context, actor board.Actor, _, idOrShort string, in board.EpicInput, requestID string) (*board.Epic, error) {
	f.call("create_epic")
	f.lastActor, f.lastRequestID = actor, requestID
	_ = idOrShort
	if err := f.errFor["create_epic"]; err != nil {
		return nil, err
	}
	return &board.Epic{ID: "e1", ShortID: "epc-bbbb2222", ProjectID: "p1", Title: in.Title, Status: board.EpicDraft}, nil
}

func (f *fakeBoardManager) UpdateEpic(_ context.Context, actor board.Actor, _, idOrShort string, in board.EpicInput, requestID string) (*board.Epic, error) {
	f.call("update_epic")
	f.lastActor, f.lastRequestID = actor, requestID
	_ = idOrShort
	if err := f.errFor["update_epic"]; err != nil {
		return nil, err
	}
	return &board.Epic{ID: "e1", ShortID: "epc-bbbb2222", ProjectID: "p1", Title: in.Title, Outcome: in.Outcome, Status: board.EpicActive}, nil
}

func (f *fakeBoardManager) epicLifecycle(name, to string) func(context.Context, board.Actor, string, string, string) (*board.Epic, error) {
	return func(_ context.Context, actor board.Actor, _, _, requestID string) (*board.Epic, error) {
		f.call(name)
		f.lastActor, f.lastRequestID = actor, requestID
		if err := f.errFor[name]; err != nil {
			return nil, err
		}
		return &board.Epic{ID: "e1", ShortID: "epc-bbbb2222", ProjectID: "p1", Status: to}, nil
	}
}

func (f *fakeBoardManager) EpicActivate(ctx context.Context, a board.Actor, id, s string, ack bool, r string) (*board.Epic, error) {
	_ = ack
	return f.epicLifecycle("epic_activate", board.EpicActive)(ctx, a, id, s, r)
}
func (f *fakeBoardManager) EpicPause(ctx context.Context, a board.Actor, id, s, r string) (*board.Epic, error) {
	return f.epicLifecycle("epic_pause", board.EpicPaused)(ctx, a, id, s, r)
}
func (f *fakeBoardManager) EpicArchive(ctx context.Context, a board.Actor, id, s, r string) (*board.Epic, error) {
	return f.epicLifecycle("epic_archive", board.EpicArchived)(ctx, a, id, s, r)
}
func (f *fakeBoardManager) EpicUnarchive(ctx context.Context, a board.Actor, id, s, r string) (*board.Epic, error) {
	return f.epicLifecycle("epic_unarchive", board.EpicActive)(ctx, a, id, s, r)
}

func (f *fakeBoardManager) PauseProject(_ context.Context, actor board.Actor, _, _, requestID string) (*board.Project, error) {
	f.call("pause_project")
	f.lastActor, f.lastRequestID = actor, requestID
	if err := f.errFor["pause_project"]; err != nil {
		return nil, err
	}
	p := board.Project{ID: "p1", ShortID: "prj-aaaa1111", UserID: actor.UserID, Status: board.ProjectActive, Paused: true}
	return &p, nil
}

func (f *fakeBoardManager) ResumeProject(_ context.Context, actor board.Actor, _, _, requestID string) (*board.Project, error) {
	f.call("resume_project")
	f.lastActor, f.lastRequestID = actor, requestID
	if err := f.errFor["resume_project"]; err != nil {
		return nil, err
	}
	p := board.Project{ID: "p1", ShortID: "prj-aaaa1111", UserID: actor.UserID, Status: board.ProjectActive}
	return &p, nil
}

func (f *fakeBoardManager) EpicInitialReadySet(_ context.Context, _ string) ([]string, error) {
	f.call("epic_ready_set")
	return []string{"tsk-cccc3333"}, nil
}

func (f *fakeBoardManager) DeleteEpic(_ context.Context, actor board.Actor, _, _ string, force bool, requestID string) (*board.DeleteResult, error) {
	f.call("delete_epic")
	f.lastActor, f.lastRequestID = actor, requestID
	_ = force
	if err := f.errFor["delete_epic"]; err != nil {
		return nil, err
	}
	return &board.DeleteResult{ItemKind: "epic", ID: "e1", ShortID: "epc-bbbb2222"}, nil
}

func (f *fakeBoardManager) DeleteTask(_ context.Context, actor board.Actor, _, _ string, reconnect bool, requestID string) (*board.DeleteResult, error) {
	f.call("delete_task")
	f.lastActor, f.lastRequestID = actor, requestID
	_ = reconnect
	if err := f.errFor["delete_task"]; err != nil {
		return nil, err
	}
	return &board.DeleteResult{ItemKind: "task", ID: "t1", ShortID: "tsk-cccc3333"}, nil
}

func (f *fakeBoardManager) PauseTask(_ context.Context, actor board.Actor, _, _, requestID string) (*board.Task, error) {
	f.call("pause_task")
	f.lastActor, f.lastRequestID = actor, requestID
	if err := f.errFor["pause_task"]; err != nil {
		return nil, err
	}
	if len(f.tasks) > 0 {
		t := f.tasks[0]
		t.Paused = true
		return &t, nil
	}
	return &board.Task{ID: "t1", ShortID: "tsk-cccc3333", Stage: board.StageBacklog, Paused: true, Tags: []string{}}, nil
}

func (f *fakeBoardManager) ResumeTask(_ context.Context, actor board.Actor, _, _, requestID string) (*board.Task, error) {
	f.call("resume_task")
	f.lastActor, f.lastRequestID = actor, requestID
	if err := f.errFor["resume_task"]; err != nil {
		return nil, err
	}
	return &board.Task{ID: "t1", ShortID: "tsk-cccc3333", Stage: board.StageBacklog, Tags: []string{}}, nil
}

func (f *fakeBoardManager) ImportPlan(_ context.Context, actor board.Actor, _, _ string, in board.PlanInput, dryRun bool, requestID string) (*board.ImportResult, error) {
	f.call("import_plan")
	f.lastActor, f.lastRequestID = actor, requestID
	_ = in
	if err := f.errFor["import_plan"]; err != nil {
		return nil, err
	}
	if dryRun {
		return &board.ImportResult{DryRun: true, ProjectID: "p1", Plan: &board.PlanValidation{Valid: true, InitialReadySet: []string{"a"}}}, nil
	}
	return &board.ImportResult{ProjectID: "p1", EpicID: "e1", EpicShortID: "epc-bbbb2222",
		TaskKeys: map[string]string{"a": "tsk-cccc3333"}, InitialReadySet: []string{"tsk-cccc3333"}}, nil
}

func (f *fakeBoardManager) GetTask(_ context.Context, _, _ string) (*board.Task, error) {
	f.call("get_task")
	if len(f.tasks) > 0 {
		return &f.tasks[0], nil
	}
	if err := f.errFor["get_task"]; err != nil {
		return nil, err
	}
	return nil, &board.NotFoundError{Item: "task", Identifier: "t1"}
}

func (f *fakeBoardManager) ListTasks(_ context.Context, projectID, stage, failed string) ([]board.Task, error) {
	f.call("list_tasks")
	f.lastFailed = failed
	_ = projectID
	if err := f.errFor["list_tasks"]; err != nil {
		return nil, err
	}
	out := f.tasks
	if stage != "" {
		out = filterTasks(out, func(t board.Task) bool { return t.Stage == stage })
	}
	if failed == "true" || failed == "false" {
		want := failed == "true"
		out = filterTasks(out, func(t board.Task) bool { return t.LastAttemptFailed == want })
	}
	return out, nil
}

func filterTasks(in []board.Task, keep func(board.Task) bool) []board.Task {
	var out []board.Task
	for _, t := range in {
		if keep(t) {
			out = append(out, t)
		}
	}
	return out
}

func (f *fakeBoardManager) ListEpics(_ context.Context, projectID string) ([]board.Epic, error) {
	f.call("list_epics")
	f.lastProjectID = projectID
	if err := f.errFor["list_epics"]; err != nil {
		return nil, err
	}
	return f.epics, nil
}

func (f *fakeBoardManager) SearchTasks(_ context.Context, userID, q string, filters board.SearchFilters) (*board.SearchResult, error) {
	f.call("search_tasks")
	f.lastSearchQuery, f.lastSearchFilters = q, filters
	_ = userID
	if err := f.errFor["search_tasks"]; err != nil {
		return nil, err
	}
	return f.searchResult, nil
}

func (f *fakeBoardManager) BoardHealth(_ context.Context, userID string) (*board.Health, error) {
	f.call("board_health")
	f.lastHealthUser = userID
	if err := f.errFor["board_health"]; err != nil {
		return nil, err
	}
	return f.health, nil
}

func (f *fakeBoardManager) CreateTask(_ context.Context, actor board.Actor, _, idOrShort string, in board.TaskInput, requestID string) (*board.Task, error) {
	f.call("create_task")
	f.lastActor, f.lastRequestID = actor, requestID
	_ = idOrShort
	if err := f.errFor["create_task"]; err != nil {
		return nil, err
	}
	return &board.Task{ID: "t1", ShortID: "tsk-cccc3333", ProjectID: "p1", EpicID: "e1", Title: in.Title, Stage: board.StageBacklog, Tags: []string{}}, nil
}

func (f *fakeBoardManager) UpdateTask(_ context.Context, actor board.Actor, _, idOrShort string, in board.TaskInput, requestID string) (*board.Task, error) {
	f.call("update_task")
	f.lastActor, f.lastRequestID = actor, requestID
	_ = idOrShort
	if err := f.errFor["update_task"]; err != nil {
		return nil, err
	}
	return &board.Task{ID: "t1", ShortID: "tsk-cccc3333", ProjectID: "p1", EpicID: "e1", Title: in.Title, Stage: board.StageBacklog, Tags: []string{}}, nil
}

func (f *fakeBoardManager) MoveStage(_ context.Context, actor board.Actor, _, idOrShort, toStage, evidenceURL, requestID string) (*board.Task, error) {
	f.call("move_stage")
	f.lastActor, f.lastRequestID = actor, requestID
	_ = idOrShort
	_ = evidenceURL
	if err := f.errFor["move_stage"]; err != nil {
		return nil, err
	}
	return &board.Task{ID: "t1", ShortID: "tsk-cccc3333", ProjectID: "p1", EpicID: "e1", Stage: toStage, Tags: []string{}}, nil
}

func (f *fakeBoardManager) TaskStart(_ context.Context, actor board.Actor, _, idOrShort, requestID string) (*board.Claim, error) {
	f.call("task_start")
	f.lastActor, f.lastRequestID = actor, requestID
	_ = idOrShort
	if err := f.errFor["task_start"]; err != nil {
		return nil, err
	}
	return &board.Claim{TaskID: "t1", ShortID: "tsk-cccc3333", ClaimToken: "tok-1", Escalated: false}, nil
}

func (f *fakeBoardManager) AddDependency(_ context.Context, actor board.Actor, _, idOrShort, dependsOn, requestID string) error {
	f.call("add_dependency")
	f.lastActor, f.lastRequestID = actor, requestID
	_ = idOrShort
	_ = dependsOn
	return f.errFor["add_dependency"]
}

func (f *fakeBoardManager) RemoveDependency(_ context.Context, actor board.Actor, _, idOrShort, dependsOn, requestID string) error {
	f.call("remove_dependency")
	f.lastActor, f.lastRequestID = actor, requestID
	_ = idOrShort
	_ = dependsOn
	return f.errFor["remove_dependency"]
}

// newBoardRESTRouter mounts the REST projection exactly as the router
// does: the /api/v1/me group with a userID-stamping AuthMiddleware
// stand-in (the context key contract the real middleware sets).
func newBoardRESTRouter(t *testing.T, svc *fakeBoardManager) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := NewBoardHandler(svc)
	r := gin.New()
	g := r.Group("/api/v1/me")
	g.Use(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Next()
	})
	p := g.Group("/projects")
	p.GET("", h.ListProjects)
	p.POST("", h.CreateProject)
	p.GET("/:id", h.GetProject)
	p.PATCH("/:id", h.UpdateProject)
	p.POST("/:id/archive", h.ArchiveProject)
	p.POST("/:id/unarchive", h.UnarchiveProject)
	p.POST("/:id/pause", h.PauseProject)
	p.POST("/:id/resume", h.ResumeProject)
	p.GET("/:id/tasks", h.ListProjectTasks)
	p.POST("/:id/epics", h.CreateEpic)
	p.POST("/:id/import-plan", h.ImportPlan)
	p.GET("/:id/epics", h.ListProjectEpics)
	bb := g.Group("/board")
	bb.GET("/search", h.BoardSearch)
	bb.GET("/health", h.BoardHealth)
	e := g.Group("/epics")
	e.GET("/:id", h.GetEpic)
	e.PATCH("/:id", h.UpdateEpic)
	e.POST("/:id/activate", h.ActivateEpic)
	e.POST("/:id/pause", h.PauseEpic)
	e.POST("/:id/archive", h.ArchiveEpic)
	e.POST("/:id/unarchive", h.UnarchiveEpic)
	e.DELETE("/:id", h.DeleteEpic)
	e.POST("/:id/tasks", h.CreateEpicTask)
	k := g.Group("/tasks")
	k.GET("/:id", h.GetTask)
	k.PATCH("/:id", h.UpdateTask)
	k.POST("/:id/move-stage", h.MoveStageTask)
	k.POST("/:id/start", h.StartTask)
	k.POST("/:id/pause", h.PauseTask)
	k.POST("/:id/resume", h.ResumeTask)
	k.DELETE("/:id", h.DeleteTask)
	k.POST("/:id/dependencies", h.AddDependency)
	k.DELETE("/:id/dependencies/:dependsOnId", h.RemoveDependency)
	return r
}

func doBoardREST(t *testing.T, r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestBoardREST_ActorStamping: every mutating action stamps the REST
// projection — the JWT user, user kind, surface=rest, NO claim token
// (manager surface; the §4.2 token guard is what makes optimistic drags
// on claimed rows snap back) — and threads requestId through.
func TestBoardREST_ActorStamping(t *testing.T) {
	cases := []struct {
		name string
		call string
		code int
		do   func(r *gin.Engine) *httptest.ResponseRecorder
	}{
		{"create_project", "create_project", http.StatusCreated, func(r *gin.Engine) *httptest.ResponseRecorder {
			return doBoardREST(t, r, http.MethodPost, "/api/v1/me/projects", `{"name":"Board","targetWorkspaceId":"ws-1","requestId":"req-1"}`)
		}},
		{"update_project", "update_project", http.StatusOK, func(r *gin.Engine) *httptest.ResponseRecorder {
			return doBoardREST(t, r, http.MethodPatch, "/api/v1/me/projects/prj-aaaa1111", `{"name":"Renamed","requestId":"req-2"}`)
		}},
		{"archive_project", "archive_project", http.StatusOK, func(r *gin.Engine) *httptest.ResponseRecorder {
			return doBoardREST(t, r, http.MethodPost, "/api/v1/me/projects/prj-aaaa1111/archive", `{}`)
		}},
		{"unarchive_project", "unarchive_project", http.StatusOK, func(r *gin.Engine) *httptest.ResponseRecorder {
			return doBoardREST(t, r, http.MethodPost, "/api/v1/me/projects/prj-aaaa1111/unarchive", `{}`)
		}},
		{"create_epic", "create_epic", http.StatusCreated, func(r *gin.Engine) *httptest.ResponseRecorder {
			return doBoardREST(t, r, http.MethodPost, "/api/v1/me/projects/prj-aaaa1111/epics", `{"title":"E1","requestId":"req-3"}`)
		}},
		{"update_epic", "update_epic", http.StatusOK, func(r *gin.Engine) *httptest.ResponseRecorder {
			return doBoardREST(t, r, http.MethodPatch, "/api/v1/me/epics/epc-bbbb2222", `{"outcome":"ship it","requestId":"req-4"}`)
		}},
		{"epic_activate", "epic_activate", http.StatusOK, func(r *gin.Engine) *httptest.ResponseRecorder {
			return doBoardREST(t, r, http.MethodPost, "/api/v1/me/epics/epc-bbbb2222/activate", `{}`)
		}},
		{"epic_pause", "epic_pause", http.StatusOK, func(r *gin.Engine) *httptest.ResponseRecorder {
			return doBoardREST(t, r, http.MethodPost, "/api/v1/me/epics/epc-bbbb2222/pause", `{}`)
		}},
		{"delete_epic", "delete_epic", http.StatusOK, func(r *gin.Engine) *httptest.ResponseRecorder {
			return doBoardREST(t, r, http.MethodDelete, "/api/v1/me/epics/epc-bbbb2222?force=true", ``)
		}},
		{"create_task", "create_task", http.StatusCreated, func(r *gin.Engine) *httptest.ResponseRecorder {
			return doBoardREST(t, r, http.MethodPost, "/api/v1/me/epics/epc-bbbb2222/tasks", `{"title":"T1","dependsOn":["tsk-dddd4444"],"requestId":"req-5"}`)
		}},
		{"update_task", "update_task", http.StatusOK, func(r *gin.Engine) *httptest.ResponseRecorder {
			return doBoardREST(t, r, http.MethodPatch, "/api/v1/me/tasks/tsk-cccc3333", `{"description":"redo","requestId":"req-6"}`)
		}},
		{"move_stage", "move_stage", http.StatusOK, func(r *gin.Engine) *httptest.ResponseRecorder {
			return doBoardREST(t, r, http.MethodPost, "/api/v1/me/tasks/tsk-cccc3333/move-stage", `{"toStage":"in_review","requestId":"req-7"}`)
		}},
		{"task_start", "task_start", http.StatusOK, func(r *gin.Engine) *httptest.ResponseRecorder {
			return doBoardREST(t, r, http.MethodPost, "/api/v1/me/tasks/tsk-cccc3333/start", `{"requestId":"req-8"}`)
		}},
		{"pause_task", "pause_task", http.StatusOK, func(r *gin.Engine) *httptest.ResponseRecorder {
			return doBoardREST(t, r, http.MethodPost, "/api/v1/me/tasks/tsk-cccc3333/pause", `{}`)
		}},
		{"resume_task", "resume_task", http.StatusOK, func(r *gin.Engine) *httptest.ResponseRecorder {
			return doBoardREST(t, r, http.MethodPost, "/api/v1/me/tasks/tsk-cccc3333/resume", `{}`)
		}},
		{"delete_task", "delete_task", http.StatusOK, func(r *gin.Engine) *httptest.ResponseRecorder {
			return doBoardREST(t, r, http.MethodDelete, "/api/v1/me/tasks/tsk-cccc3333", ``)
		}},
		{"add_dependency", "add_dependency", http.StatusOK, func(r *gin.Engine) *httptest.ResponseRecorder {
			return doBoardREST(t, r, http.MethodPost, "/api/v1/me/tasks/tsk-cccc3333/dependencies", `{"dependsOn":"tsk-dddd4444","requestId":"req-9"}`)
		}},
		{"remove_dependency", "remove_dependency", http.StatusOK, func(r *gin.Engine) *httptest.ResponseRecorder {
			return doBoardREST(t, r, http.MethodDelete, "/api/v1/me/tasks/tsk-cccc3333/dependencies/tsk-dddd4444", ``)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newFakeBoardManager()
			// The dependency renders re-read the task row; the epic/task
			// creates render their fresh rows — give the fake one of each.
			svc.tasks = []board.Task{{ID: "t1", ShortID: "tsk-cccc3333", ProjectID: "p1", EpicID: "e1", Stage: board.StageBacklog, Tags: []string{}}}
			r := newBoardRESTRouter(t, svc)
			w := tc.do(r)
			require.Equal(t, tc.code, w.Code, tc.name+": "+w.Body.String())
			assert.Equal(t, 1, svc.calls[tc.call], tc.name)
			assert.Equal(t, "user-1", svc.lastActor.UserID, tc.name)
			assert.Equal(t, board.ActorUser, svc.lastActor.Kind, tc.name)
			assert.Equal(t, board.SurfaceREST, svc.lastActor.Surface, tc.name)
			assert.Empty(t, svc.lastActor.ClaimToken, tc.name+" — manager surfaces hold no claim token")
			assert.Empty(t, svc.lastActor.WorkspaceID, tc.name+" — manager surfaces scope by ownership, not workspace")
		})
	}
}

// TestBoardREST_RequestIDThreadsThrough: the idempotency key a mutating
// SDK call supplies rides to the event writer (clientMessageID
// discipline — one per logical operation, reused on retry).
func TestBoardREST_RequestIDThreadsThrough(t *testing.T) {
	svc := newFakeBoardManager()
	r := newBoardRESTRouter(t, svc)
	w := doBoardREST(t, r, http.MethodPost, "/api/v1/me/tasks/tsk-cccc3333/move-stage",
		`{"toStage":"in_review","requestId":"11111111-1111-1111-1111-111111111111"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "11111111-1111-1111-1111-111111111111", svc.lastRequestID)
}

// TestBoardREST_ReadsAreOwnerScoped: the read perimeter never discloses
// foreign rows — a project/epic/task owned by another user renders the
// SAME plain 404 the not-found class renders (no row, no suggestions).
func TestBoardREST_ReadsAreOwnerScoped(t *testing.T) {
	svc := newFakeBoardManager()
	svc.projects = []board.Project{{ID: "p1", ShortID: "prj-aaaa1111", UserID: "someone-else", Status: board.ProjectActive}}
	svc.epics = []board.Epic{{ID: "e1", ShortID: "epc-bbbb2222", ProjectID: "p1", Status: board.EpicActive}}
	svc.tasks = []board.Task{{ID: "t1", ShortID: "tsk-cccc3333", ProjectID: "p1", EpicID: "e1", Stage: board.StageBacklog}}
	r := newBoardRESTRouter(t, svc)

	// NOTE: epic/task reads resolve ownership THROUGH the project row
	// (one extra get_project each), so per-path call counts are asserted
	// AFTER the loop — map iteration order must not matter (race-mode
	// flake caught on the first run).
	for _, path := range []string{
		"/api/v1/me/projects/prj-aaaa1111",
		"/api/v1/me/epics/epc-bbbb2222",
		"/api/v1/me/tasks/tsk-cccc3333",
	} {
		w := doBoardREST(t, r, http.MethodGet, path, "")
		assert.Equal(t, http.StatusNotFound, w.Code, path)
		var body map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), path)
		assert.NotContains(t, w.Body.String(), "someone-else", path)
		assert.Empty(t, body["suggestions"], path+" — foreign rows never carry did-you-mean suggestions")
	}
	assert.Equal(t, 1, svc.calls["get_epic"], "epic read")
	assert.Equal(t, 1, svc.calls["get_task"], "task read")
	assert.Equal(t, 3, svc.calls["get_project"], "project read directly + once per epic/task ownership check")

	// The task listing under a foreign project is equally closed.
	w := doBoardREST(t, r, http.MethodGet, "/api/v1/me/projects/prj-aaaa1111/tasks", "")
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, 0, svc.calls["list_tasks"], "foreign project never reaches the list seam")
}

// TestBoardREST_ErrorClassParity: the REST column of the parity matrix —
// every BoardService error class renders the same HTTP outcome the
// internal automation seam renders (§4.3): NotFoundError → 404 with
// suggestions, DependencyError → 409 with suggestions, GuardError
// authz/workspace_binding → 403, every other guard → 409, binding → 400.
func TestBoardREST_ErrorClassParity(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantBody string
	}{
		{"notfound", &board.NotFoundError{Item: "task", Identifier: "tsk-nope", Suggestions: []string{"tsk-cccc3333"}}, http.StatusNotFound, `"suggestions"`},
		{"guard_authz_403", &board.GuardError{Code: board.GuardAuthz, Detail: "board actions belong to the owning user (ruling R7)"}, http.StatusForbidden, `"authz"`},
		{"guard_workspace_binding_403", &board.GuardError{Code: board.GuardWorkspaceBinding, Detail: "target workspace must be accessible"}, http.StatusForbidden, `"workspace_binding"`},
		{"guard_stage_409", &board.GuardError{Code: board.GuardStage, Detail: "not an enumerated transition"}, http.StatusConflict, `"stage_transition"`},
		{"guard_done_409", &board.GuardError{Code: board.GuardDone, Detail: "criteria unmet or evidence missing"}, http.StatusConflict, `"done_guard"`},
		{"guard_claim_token_409", &board.GuardError{Code: board.GuardClaimToken, Detail: "stale claim"}, http.StatusConflict, `"claim_token"`},
		{"guard_epic_state_409", &board.GuardError{Code: board.GuardEpicState, Detail: "not a lifecycle transition"}, http.StatusConflict, `"epic_state"`},
		{"dependency_cycle_409", &board.DependencyError{Code: "cycle", Detail: "would create a cycle", Suggestions: []string{"tsk-cccc3333"}}, http.StatusConflict, `"suggestions"`},
		{"dependency_cross_project_409", &board.DependencyError{Code: "cross_project", Detail: "edges live within a project"}, http.StatusConflict, `"cross_project"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newFakeBoardManager()
			svc.errFor["update_task"] = tc.err
			r := newBoardRESTRouter(t, svc)
			w := doBoardREST(t, r, http.MethodPatch, "/api/v1/me/tasks/tsk-cccc3333", `{"title":"x"}`)
			assert.Equal(t, tc.wantCode, w.Code, tc.name+": "+w.Body.String())
			assert.Contains(t, w.Body.String(), tc.wantBody, tc.name)
		})
	}
}

// TestBoardREST_BindingValidation: the adapter-owned validation —
// required fields at create, toStage at move-stage, the enumerated stage
// filter on listings — answers 400 before the service is touched.
func TestBoardREST_BindingValidation(t *testing.T) {
	cases := []struct {
		name string
		do   func(r *gin.Engine) *httptest.ResponseRecorder
	}{
		{"create_project_missing_name", func(r *gin.Engine) *httptest.ResponseRecorder {
			return doBoardREST(t, r, http.MethodPost, "/api/v1/me/projects", `{"targetWorkspaceId":"ws-1"}`)
		}},
		{"create_project_missing_workspace", func(r *gin.Engine) *httptest.ResponseRecorder {
			return doBoardREST(t, r, http.MethodPost, "/api/v1/me/projects", `{"name":"Board"}`)
		}},
		{"create_epic_missing_title", func(r *gin.Engine) *httptest.ResponseRecorder {
			return doBoardREST(t, r, http.MethodPost, "/api/v1/me/projects/prj-aaaa1111/epics", `{}`)
		}},
		{"create_task_missing_title", func(r *gin.Engine) *httptest.ResponseRecorder {
			return doBoardREST(t, r, http.MethodPost, "/api/v1/me/epics/epc-bbbb2222/tasks", `{}`)
		}},
		{"move_stage_missing_toStage", func(r *gin.Engine) *httptest.ResponseRecorder {
			return doBoardREST(t, r, http.MethodPost, "/api/v1/me/tasks/tsk-cccc3333/move-stage", `{}`)
		}},
		{"add_dependency_missing_dependsOn", func(r *gin.Engine) *httptest.ResponseRecorder {
			return doBoardREST(t, r, http.MethodPost, "/api/v1/me/tasks/tsk-cccc3333/dependencies", `{}`)
		}},
		{"malformed_body", func(r *gin.Engine) *httptest.ResponseRecorder {
			return doBoardREST(t, r, http.MethodPost, "/api/v1/me/projects", `{not json`)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newFakeBoardManager()
			r := newBoardRESTRouter(t, svc)
			w := tc.do(r)
			assert.Equal(t, http.StatusBadRequest, w.Code, tc.name+": "+w.Body.String())
		})
	}

	// The stage filter accepts exactly the five stages (ruling R2's
	// binding set, #1747-amended: cancel is eliminated) and rejects
	// everything else.
	svc := newFakeBoardManager()
	svc.projects = []board.Project{{ID: "p1", ShortID: "prj-aaaa1111", UserID: "user-1", Status: board.ProjectActive}}
	r := newBoardRESTRouter(t, svc)
	for _, stage := range []string{"backlog", "in_progress", "in_review", "done", "pending_input"} {
		w := doBoardREST(t, r, http.MethodGet, "/api/v1/me/projects/prj-aaaa1111/tasks?stage="+stage, "")
		assert.Equal(t, http.StatusOK, w.Code, stage)
	}
	w := doBoardREST(t, r, http.MethodGet, "/api/v1/me/projects/prj-aaaa1111/tasks?stage=wip", "")
	assert.Equal(t, http.StatusBadRequest, w.Code, "unknown stage filter: "+w.Body.String())
	// #1747 (r2, stage-filter contract pin): the ELIMINATED terminal
	// stage is not a filter value anymore — same 400 class as a typo
	// (the openapi enum no longer advertises it either). The \u escape
	// spells the historical value as DATA without a bare source
	// occurrence the US-locale fixer would rewrite (r5 doctrine: one
	// named/escaped site, never concatenation).
	w = doBoardREST(t, r, http.MethodGet, "/api/v1/me/projects/prj-aaaa1111/tasks?stage=cancel\u006ced", "")
	assert.Equal(t, http.StatusBadRequest, w.Code, "the eliminated stage left the vocabulary: "+w.Body.String())

	// #1813: the failed-state overlay filter — tri-state boolean on the
	// SAME endpoint; anything but true/false is a 400. The filter is
	// SEMANTIC (the fake honors it): true keeps only failed rows,
	// false only clean rows, absent keeps all.
	svc2 := newFakeBoardManager()
	svc2.projects = []board.Project{{ID: "p1", ShortID: "prj-aaaa1111", UserID: "user-1", Status: board.ProjectActive}}
	svc2.tasks = []board.Task{
		{ID: "t-clean", ShortID: "tsk-aaaa0001", ProjectID: "p1", EpicID: "e1", Title: "clean", Stage: board.StageBacklog},
		{ID: "t-failed", ShortID: "tsk-bbbb0002", ProjectID: "p1", EpicID: "e1", Title: "flappy", Stage: board.StageBacklog, LastAttemptFailed: true, FailureCount: 2},
	}
	r2 := newBoardRESTRouter(t, svc2)
	w2 := doBoardREST(t, r2, http.MethodGet, "/api/v1/me/projects/prj-aaaa1111/tasks?failed=true", "")
	assert.Equal(t, http.StatusOK, w2.Code)
	assert.Contains(t, w2.Body.String(), "tsk-bbbb0002", "failed=true keeps the failed row")
	assert.NotContains(t, w2.Body.String(), "tsk-aaaa0001", "failed=true drops the clean row")
	assert.Contains(t, w2.Body.String(), `"failure_count":2`, "the overlay renders on the read")
	w2 = doBoardREST(t, r2, http.MethodGet, "/api/v1/me/projects/prj-aaaa1111/tasks?failed=false", "")
	assert.Equal(t, http.StatusOK, w2.Code)
	assert.Contains(t, w2.Body.String(), "tsk-aaaa0001", "failed=false keeps the clean row")
	assert.NotContains(t, w2.Body.String(), "tsk-bbbb0002", "failed=false drops the failed row")
	w2 = doBoardREST(t, r2, http.MethodGet, "/api/v1/me/projects/prj-aaaa1111/tasks", "")
	assert.Equal(t, http.StatusOK, w2.Code)
	assert.Contains(t, w2.Body.String(), "tsk-aaaa0001")
	assert.Contains(t, w2.Body.String(), "tsk-bbbb0002", "absent keeps all rows")
	w2 = doBoardREST(t, r2, http.MethodGet, "/api/v1/me/projects/prj-aaaa1111/tasks?failed=maybe", "")
	assert.Equal(t, http.StatusBadRequest, w2.Code, "failed filter is tri-state boolean: "+w2.Body.String())
}

// TestBoardREST_ListRenders: list endpoints render the owner's boards in
// the platform's envelope shape ({"projects": [...]}/{"tasks": [...]}).
func TestBoardREST_ListRenders(t *testing.T) {
	svc := newFakeBoardManager()
	svc.projects = []board.Project{{ID: "p1", ShortID: "prj-aaaa1111", UserID: "user-1", Name: "Board", Status: board.ProjectActive, Tags: []string{}}}
	svc.tasks = []board.Task{{ID: "t1", ShortID: "tsk-cccc3333", ProjectID: "p1", EpicID: "e1", Title: "T1", Stage: board.StageBacklog, Tags: []string{}}}
	r := newBoardRESTRouter(t, svc)

	w := doBoardREST(t, r, http.MethodGet, "/api/v1/me/projects", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var list struct {
		Projects []board.Project `json:"projects"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list))
	require.Len(t, list.Projects, 1)
	assert.Equal(t, "prj-aaaa1111", list.Projects[0].ShortID)

	w = doBoardREST(t, r, http.MethodGet, "/api/v1/me/projects/prj-aaaa1111/tasks", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var taskList struct {
		Tasks []board.Task `json:"tasks"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &taskList))
	require.Len(t, taskList.Tasks, 1)
	assert.Equal(t, "tsk-cccc3333", taskList.Tasks[0].ShortID)
}

// TestBoardREST_CreatedAndClaimed: creates answer 201; task_start
// renders the engine's Claim (token, lease, escalated flag — the
// dispatch outcome, not a task row).
func TestBoardREST_CreatedAndClaimed(t *testing.T) {
	svc := newFakeBoardManager()
	r := newBoardRESTRouter(t, svc)

	w := doBoardREST(t, r, http.MethodPost, "/api/v1/me/projects", `{"name":"Board","targetWorkspaceId":"ws-1"}`)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var proj board.Project
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &proj))
	assert.Equal(t, "prj-aaaa1111", proj.ShortID)

	w = doBoardREST(t, r, http.MethodPost, "/api/v1/me/tasks/tsk-cccc3333/start", `{}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var claim board.Claim
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &claim))
	assert.Equal(t, "tok-1", claim.ClaimToken)
	assert.False(t, claim.Escalated)
}

// TestBoardREST_DependencyRendersTask: dependency mutations render the
// re-derived task row (is_blocked converges in the same read).
func TestBoardREST_DependencyRendersTask(t *testing.T) {
	svc := newFakeBoardManager()
	svc.projects = []board.Project{{ID: "p1", ShortID: "prj-aaaa1111", UserID: "user-1", Status: board.ProjectActive}}
	svc.tasks = []board.Task{{ID: "t1", ShortID: "tsk-cccc3333", ProjectID: "p1", EpicID: "e1", Stage: board.StageBacklog, IsBlocked: true, Tags: []string{}}}
	r := newBoardRESTRouter(t, svc)

	w := doBoardREST(t, r, http.MethodPost, "/api/v1/me/tasks/tsk-cccc3333/dependencies", `{"dependsOn":"tsk-dddd4444"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 1, svc.calls["add_dependency"])

	w = doBoardREST(t, r, http.MethodDelete, "/api/v1/me/tasks/tsk-cccc3333/dependencies/tsk-dddd4444", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 1, svc.calls["remove_dependency"])
}

// TestBoardREST_StartTaskChunkedEmptyBody: the r2 binding fix's
// regression pin — a chunked empty body (ContentLength -1, zero bytes)
// is an ABSENT request, not a malformed one. The pre-fix code path
// (`ContentLength != 0` → bind → EOF → 400) fails this test; the fix
// accepts EOF-on-empty and still 400s genuinely malformed JSON.
func TestBoardREST_StartTaskChunkedEmptyBody(t *testing.T) {
	svc := newFakeBoardManager()
	r := newBoardRESTRouter(t, svc)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/me/tasks/tsk-cccc3333/start", nil)
	req.Header.Set("Content-Type", "application/json")
	// Chunked transfer-encoding: net/http reports ContentLength -1; the
	// body is zero bytes. The adapter must treat it as absent.
	req.ContentLength = -1
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "chunked empty body binds as an absent request: "+w.Body.String())
	assert.Equal(t, 1, svc.calls["task_start"])

	// Malformed JSON still 400s (the fix must not swallow real errors).
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/me/tasks/tsk-cccc3333/start", strings.NewReader("{not json"))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusBadRequest, w2.Code)
}

// ReportResourcePressure implements the seam's telemetry sink over the
// fake (the #1747 backpressure route's capture).
func (f *fakeBoardManager) ReportResourcePressure(_ context.Context, _ string, sample board.PressureSample) error {
	s := sample
	f.lastPressureSample = &s
	return nil
}
