// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Canary scenario: S-BOARD-CRUD (#1699, design 0067 §4.3a)
// User-level board CRUD round-trip: project → epic → task, structural
// dependencies, the guard classes, and the OR4 pr:N anchor tag form.
package main

import (
	"context"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	ll "github.com/lenaxia/llmsafespaces/sdk/go"
	canary "github.com/lenaxia/llmsafespaces/sdks/canary/go"
)

var (
	prjShape = regexp.MustCompile(`^prj-[0-9a-z]{8}$`)
	epcShape = regexp.MustCompile(`^epc-[0-9a-z]{8}$`)
	tskShape = regexp.MustCompile(`^tsk-[0-9a-z]{8}$`)
	prAnchor = regexp.MustCompile(`^pr:[1-9][0-9]*$`)
)

func Handler(w http.ResponseWriter, r *http.Request) {
	run := canary.NewRunner("board-crud", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	runBoardCRUD(ctx, run, cfg)
	run.WriteHTTP(w)
}

func main() {
	run := canary.NewRunner("board-crud", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runBoardCRUD(ctx, run, cfg)
	res := run.Print()
	if res.Failed > 0 {
		os.Exit(1)
	}
}

func runBoardCRUD(ctx context.Context, run *canary.Runner, cfg canary.Config) {
	c := cfg.JWTClient()

	// Fixture: a workspace row for the project target (ruling R8).
	ws, err := c.Workspaces.Create(ctx, ll.CreateWorkspaceRequest{
		Name: "canary-go-board", Runtime: "base", StorageSize: "1Gi",
	})
	if !run.AssertNoError(err, "fixture-ws: no error") {
		return
	}
	wsID := ws.ID
	defer func() { _ = c.Workspaces.Delete(context.Background(), wsID) }()

	// P1: project create — short id shape prj-XXXXXXXX
	proj, err := c.Board.CreateProject(ctx, ll.BoardProjectInput{
		Name: "canary-go-board", TargetWorkspaceID: wsID, Tags: []string{"canary"},
	})
	if !run.AssertNoError(err, "create-project: no error") {
		return
	}
	run.Assert(prjShape.MatchString(proj.ShortID), "create-project: short_id shape", proj.ShortID)
	run.Assert(proj.Status == ll.BoardProjectActive, "create-project: active", proj.Status)
	projectSid := proj.ShortID

	// P2: list + get (case-insensitive short id)
	if _, err := c.Board.ListProjects(ctx); !run.AssertNoError(err, "list-projects: no error") {
		return
	}
	got, err := c.Board.GetProject(ctx, strings.ToUpper(projectSid))
	if run.AssertNoError(err, "get-project: no error") {
		run.Assert(got.ID == proj.ID, "get-project: id matches", got.ID)
	}

	// P3: patch (rename; tags replace)
	upd, err := c.Board.UpdateProject(ctx, projectSid, ll.BoardProjectInput{
		Name: "canary-go-board-2", Tags: []string{"canary", "board"},
	})
	if run.AssertNoError(err, "patch-project: no error") {
		run.Assert(upd.Name == "canary-go-board-2", "patch-project: renamed", upd.Name)
	}

	// P4: archive → unarchive
	arch, err := c.Board.ArchiveProject(ctx, projectSid)
	if run.AssertNoError(err, "archive-project: no error") {
		run.Assert(arch.Status == ll.BoardProjectArchived, "archive-project: archived", arch.Status)
	}
	unarch, err := c.Board.UnarchiveProject(ctx, projectSid)
	if run.AssertNoError(err, "unarchive-project: no error") {
		run.Assert(unarch.Status == ll.BoardProjectActive, "unarchive-project: active", unarch.Status)
	}

	// P5: epic create (draft default) → activate → double-pause guard
	epic, err := c.Board.CreateEpic(ctx, projectSid, ll.BoardEpicInput{
		Title: "E1", Outcome: "canary",
	})
	if !run.AssertNoError(err, "create-epic: no error") {
		return
	}
	run.Assert(epcShape.MatchString(epic.ShortID), "create-epic: short_id shape", epic.ShortID)
	run.Assert(epic.Status == ll.BoardEpicDraft, "create-epic: draft default", epic.Status)
	epicSid := epic.ShortID

	if act, err := c.Board.EpicActivate(ctx, epicSid); run.AssertNoError(err, "activate-epic: no error") {
		run.Assert(act.Status == ll.BoardEpicActive, "activate-epic: active", act.Status)
	}
	if _, err := c.Board.EpicPause(ctx, epicSid); run.AssertNoError(err, "pause-epic: no error") {
		if _, e := c.Board.EpicPause(ctx, epicSid); e == nil {
			run.Fail("pause-paused-epic: guard error", "expected guard error, got success")
		} else {
			run.OK("pause-paused-epic: guard error")
		}
	}

	// P6: task create with the OR4 pr:N anchor tag + criteria
	taskA, err := c.Board.CreateTask(ctx, epicSid, ll.BoardTaskInput{
		Title: "Task A",

		Tags: []string{"pr:1234"},
		AcceptanceCriteria: []ll.BoardAcceptanceCriterion{
			{Text: "c1", Met: true},
			{Text: "c2", Met: false},
		},
	})
	if !run.AssertNoError(err, "create-task: no error") {
		return
	}
	run.Assert(tskShape.MatchString(taskA.ShortID), "create-task: short_id shape", taskA.ShortID)
	run.Assert(len(taskA.Tags) == 1 && prAnchor.MatchString(taskA.Tags[0]), "create-task: OR4 pr:N anchor tag form", strings.Join(taskA.Tags, ","))
	taskASid := taskA.ShortID

	taskB, err := c.Board.CreateTask(ctx, epicSid, ll.BoardTaskInput{Title: "Task B"})
	if !run.AssertNoError(err, "create-task-b: no error") {
		return
	}
	taskBSid := taskB.ShortID

	// P7: structural dependency — blocked derivation, guards, unblock
	dep, err := c.Board.AddDependency(ctx, taskBSid, ll.BoardDependencyInput{DependsOn: taskASid})
	if run.AssertNoError(err, "add-dependency: no error") {
		run.Assert(dep.Task != nil && dep.Task.IsBlocked, "add-dependency: blocked", "")
	}
	if _, e := c.Board.AddDependency(ctx, taskBSid, ll.BoardDependencyInput{DependsOn: taskBSid}); e == nil {
		run.Fail("self-edge: guard error", "expected guard error, got success")
	} else {
		run.OK("self-edge: guard error")
	}
	if _, e := c.Board.AddDependency(ctx, taskASid, ll.BoardDependencyInput{DependsOn: taskBSid}); e == nil {
		run.Fail("cycle: guard error", "expected guard error, got success")
	} else {
		run.OK("cycle: guard error")
	}
	if rem, err := c.Board.RemoveDependency(ctx, taskBSid, taskASid); run.AssertNoError(err, "remove-dependency: no error") {
		run.Assert(rem.Task != nil && !rem.Task.IsBlocked, "remove-dependency: unblocked", "")
	}

	// P8: patch task — criteria check-off (Done preparation)
	met := 1
	if checked, err := c.Board.UpdateTask(ctx, taskASid, ll.BoardTaskInput{SetCriteriaMet: &met}); run.AssertNoError(err, "patch-task-criteria: no error") {
		run.Assert(len(checked.AcceptanceCriteria) == 2 && checked.AcceptanceCriteria[1].Met, "patch-task-criteria: met", "")
	}

	// P9: stage guard — backlog leaves by claim, not by drag
	if _, e := c.Board.MoveStage(ctx, taskASid, ll.BoardMoveStageInput{ToStage: "in_progress"}); e == nil {
		run.Fail("move-stage-from-backlog: guard error", "expected guard error, got success")
	} else {
		run.OK("move-stage-from-backlog: guard error")
	}

	// P10: listing with the enumerated stage filter; unknown stage 400s
	if tasks, err := c.Board.ListProjectTasks(ctx, projectSid, "backlog"); run.AssertNoError(err, "list-tasks: no error") {
		run.Assert(len(tasks) == 2, "list-tasks: two backlog", "")
	}
	if _, e := c.Board.ListProjectTasks(ctx, projectSid, "wip"); e == nil {
		run.Fail("list-tasks-bad-stage: error", "expected error, got success")
	} else {
		run.OK("list-tasks-bad-stage: error")
	}

	// P11: cancel task (terminal) + cancel epic (→ paused)
	if canceled, err := c.Board.CancelTask(ctx, taskBSid, ll.BoardCancelInput{Reason: "canary cleanup"}); run.AssertNoError(err, "cancel-task: no error") {
		run.Assert(canceled.Stage == ll.BoardStageCancelled, "cancel-task: terminal stage", canceled.Stage)
	}
	if ce, err := c.Board.CancelEpic(ctx, epicSid, ll.BoardCancelInput{Reason: "canary cleanup"}); run.AssertNoError(err, "cancel-task-epic: no error") {
		run.Assert(ce.Status == ll.BoardEpicPaused, "cancel-epic: paused", ce.Status)
	}

	// P12: read perimeter — a near-miss id is a plain 404
	if _, e := c.Board.GetProject(ctx, "prj-zzzzzzzz"); e == nil {
		run.Fail("get-project-nearmiss: 404", "expected 404, got success")
	} else {
		run.OK("get-project-nearmiss: 404")
	}
}
