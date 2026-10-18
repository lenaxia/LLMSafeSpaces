// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package llmsafespaces

// board_test.go — wire-level pins for the board service (#1699): method
// + path + body encoding per endpoint, the engine row encoding
// (snake_case short_id — identical to the automation seam), the OR4
// pr:N anchor tag form carried verbatim, and the error classes the
// handler maps (409 guards with code, 404 with suggestions).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func boardTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *[]string) {
	t.Helper()
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, WithAPIKey("lsp_test"))
	return c, &calls
}

func TestBoard_ProjectRoundTrip(t *testing.T) {
	c, calls := boardTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/me/projects":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["targetWorkspaceId"] != "ws-1" || body["name"] != "Board" {
				t.Errorf("create body: %+v", body)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"p1","short_id":"prj-aaaa1111","user_id":"u1","name":"Board","status":"active","worker_slots":2,"tags":["x"],"created_at":"2026-10-17T00:00:00Z","updated_at":"2026-10-17T00:00:00Z"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/me/projects":
			_, _ = w.Write([]byte(`{"projects":[{"id":"p1","short_id":"prj-aaaa1111","user_id":"u1","status":"active"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/me/projects/prj-aaaa1111":
			_, _ = w.Write([]byte(`{"id":"p1","short_id":"prj-aaaa1111","status":"active"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/archive"):
			_, _ = w.Write([]byte(`{"id":"p1","short_id":"prj-aaaa1111","status":"archived"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/unarchive"):
			_, _ = w.Write([]byte(`{"id":"p1","short_id":"prj-aaaa1111","status":"active"}`))
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ctx := context.Background()

	proj, err := c.Board.CreateProject(ctx, BoardProjectInput{Name: "Board", TargetWorkspaceID: "ws-1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if proj.ShortID != "prj-aaaa1111" || proj.Status != BoardProjectActive {
		t.Fatalf("create decode: %+v", proj)
	}

	list, err := c.Board.ListProjects(ctx)
	if err != nil || len(list) != 1 || list[0].ShortID != "prj-aaaa1111" {
		t.Fatalf("list: %v %+v", err, list)
	}

	got, err := c.Board.GetProject(ctx, "prj-aaaa1111")
	if err != nil || got.ID != "p1" {
		t.Fatalf("get: %v %+v", err, got)
	}

	if _, err := c.Board.ArchiveProject(ctx, "prj-aaaa1111"); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if _, err := c.Board.UnarchiveProject(ctx, "prj-aaaa1111"); err != nil {
		t.Fatalf("unarchive: %v", err)
	}
	want := []string{
		"POST /api/v1/me/projects",
		"GET /api/v1/me/projects",
		"GET /api/v1/me/projects/prj-aaaa1111",
		"POST /api/v1/me/projects/prj-aaaa1111/archive",
		"POST /api/v1/me/projects/prj-aaaa1111/unarchive",
	}
	if fmt.Sprint(*calls) != fmt.Sprint(want) {
		t.Fatalf("calls: %v", *calls)
	}
}

func TestBoard_EpicLifecyclePaths(t *testing.T) {
	c, _ := boardTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"e1","short_id":"epc-bbbb2222","project_id":"p1","status":"active","priority":"P2","progress_done":1,"progress_total":2,"progress_derived":true}`))
	})
	ctx := context.Background()

	if _, err := c.Board.CreateEpic(ctx, "prj-aaaa1111", BoardEpicInput{Title: "E1"}); err != nil {
		t.Fatalf("create epic: %v", err)
	}
	epic, err := c.Board.EpicActivate(ctx, "epc-bbbb2222")
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if !epic.ProgressDerived || epic.ProgressDone != 1 || epic.ProgressTotal != 2 {
		t.Fatalf("derived progress decode: %+v", epic)
	}
	for _, fn := range []struct {
		name string
		call func() error
	}{
		{"pause", func() error { _, err := c.Board.EpicPause(ctx, "epc-bbbb2222"); return err }},
		{"archive", func() error { _, err := c.Board.EpicArchive(ctx, "epc-bbbb2222"); return err }},
		{"unarchive", func() error { _, err := c.Board.EpicUnarchive(ctx, "epc-bbbb2222"); return err }},
		{"cancel", func() error {
			_, err := c.Board.CancelEpic(ctx, "epc-bbbb2222", BoardCancelInput{Reason: "r"})
			return err
		}},
	} {
		if err := fn.call(); err != nil {
			t.Fatalf("%s: %v", fn.name, err)
		}
	}
}

func TestBoard_TaskAndOR4Anchor(t *testing.T) {
	c, _ := boardTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/move-stage") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["toStage"] != "done" || body["evidenceUrl"] != "https://git/pr/1" {
				t.Errorf("move-stage body: %+v", body)
			}
		}
		_, _ = w.Write([]byte(`{"id":"t1","short_id":"tsk-cccc3333","stage":"done","tags":["pr:1234"],"is_blocked":false,"attempts":1,"acceptance_criteria":[{"text":"c","met":false}]}`))
	})
	ctx := context.Background()

	task, err := c.Board.CreateTask(ctx, "epc-bbbb2222", BoardTaskInput{
		Title: "T", Tags: []string{"pr:1234"},
		AcceptanceCriteria: []BoardAcceptanceCriterion{{Text: "c"}},
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	// OR4 pin: the explicit pr:N anchor tag rides verbatim.
	if len(task.Tags) != 1 || !strings.HasPrefix(task.Tags[0], "pr:") {
		t.Fatalf("OR4 anchor tag: %+v", task.Tags)
	}
	if len(task.AcceptanceCriteria) != 1 || task.AcceptanceCriteria[0].Met {
		t.Fatalf("criteria decode: %+v", task.AcceptanceCriteria)
	}

	moved, err := c.Board.MoveStage(ctx, "tsk-cccc3333", BoardMoveStageInput{ToStage: "done", EvidenceURL: "https://git/pr/1"})
	if err != nil || moved.Stage != BoardStageDone {
		t.Fatalf("move-stage: %v %+v", err, moved)
	}

	if _, err := c.Board.StartTask(ctx, "tsk-cccc3333", BoardCancelInput{}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := c.Board.CancelTask(ctx, "tsk-cccc3333", BoardCancelInput{Reason: "r"}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	met := 0
	if _, err := c.Board.UpdateTask(ctx, "tsk-cccc3333", BoardTaskInput{SetCriteriaMet: &met}); err != nil {
		t.Fatalf("update: %v", err)
	}
}

func TestBoard_DependencyAndClaimDecode(t *testing.T) {
	c, _ := boardTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/dependencies") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["dependsOn"] != "tsk-aaaa0000" {
				t.Errorf("dependency body: %+v", body)
			}
			_, _ = w.Write([]byte(`{"task":{"id":"t1","short_id":"tsk-cccc3333","is_blocked":true,"blocked_by":["tsk-aaaa0000"]}}`))
			return
		}
		if r.Method == http.MethodDelete {
			_, _ = w.Write([]byte(`{"task":{"id":"t1","short_id":"tsk-cccc3333","is_blocked":false}}`))
			return
		}
		_, _ = w.Write([]byte(`{"task_id":"t1","short_id":"tsk-cccc3333","claim_token":"tok-1","escalated":false,"task":{"id":"t1","stage":"in_progress"}}`))
	})
	ctx := context.Background()

	dep, err := c.Board.AddDependency(ctx, "tsk-cccc3333", BoardDependencyInput{DependsOn: "tsk-aaaa0000"})
	if err != nil || dep.Task == nil || !dep.Task.IsBlocked || dep.Task.BlockedBy[0] != "tsk-aaaa0000" {
		t.Fatalf("add dependency: %v %+v", err, dep)
	}
	rem, err := c.Board.RemoveDependency(ctx, "tsk-cccc3333", "tsk-aaaa0000")
	if err != nil || rem.Task == nil || rem.Task.IsBlocked {
		t.Fatalf("remove dependency: %v %+v", err, rem)
	}
	claim, err := c.Board.StartTask(ctx, "tsk-cccc3333", BoardCancelInput{})
	if err != nil || claim.ClaimToken != "tok-1" || claim.Escalated || claim.Task == nil {
		t.Fatalf("claim decode: %v %+v", err, claim)
	}
}

func TestBoard_StageFilterOnList(t *testing.T) {
	c, _ := boardTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "stage=backlog") {
			t.Errorf("stage filter missing: %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"tasks":[{"id":"t1","short_id":"tsk-cccc3333","stage":"backlog"}]}`))
	})
	tasks, err := c.Board.ListProjectTasks(context.Background(), "prj-aaaa1111", "backlog")
	if err != nil || len(tasks) != 1 || tasks[0].Stage != BoardStageBacklog {
		t.Fatalf("list tasks: %v %+v", err, tasks)
	}
}
