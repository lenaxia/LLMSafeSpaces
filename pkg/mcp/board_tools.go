// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package mcp

// board_tools.go — the Task Board's EXTERNAL platform MCP projection
// (design 0067 §4.3b, issue #1700): customer-facing board_* tools that
// ride the owner's REST projection (/api/v1/me/*, ruling R7) through
// the same handlers the REST column pins — identical guard outcomes by
// construction (the API's AuthMiddleware scopes every call to the
// authenticated owner; foreign rows render the same plain 404 a miss
// renders, never disclosed).
//
// Deliberate asymmetry (§4.3b, mirrored in the parity matrix):
// NO heartbeat, NO report_throttle, NO claim tool exists on this
// surface — claiming is engine-side (claim-then-spawn, R9) and the
// in-flight worker actions live on the agent registry only (§4.3c).
// External actors hold NO claim tokens: every board_* tool is a
// manager action under the owner's authority.
//
// Naming: design 0067 §4.3b names board_list_projects, board_get,
// board_update_item, board_move_stage, board_start_task, the
// epic/project lifecycle pairs, board_cancel, the dependency pair, and
// board_read. The design's board_create_* wildcard expands to
// board_create_project/epic/task (REST vocabulary); board_read is the
// tasks-listing reader (search/health join when the engine seams land,
// #1699 follow-ups).

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// --- Tool definitions ---

// --- Stage vocabularies (single source, r2) ---
//
// boardStageCancelled is the design-verbatim stage literal (ruling R2's
// binding set; the DB CHECK depends on it) — composed so misspell's
// fixer never rewrites it.
const boardStageCancelled = "cancel" + "led" //nolint:misspell // design-verbatim stage value

// boardStages is ruling R2's full six-stage binding set — the READ
// filter's vocabulary (board_read's stage parameter; every stage is
// listable, terminal ones included).
var boardStages = []string{
	"backlog", "in_progress", "in_review", "done", "pending_input", boardStageCancelled,
}

// boardMoveTargets is the stage machine's legal manager move_stage
// TARGET set (pkg/board stage.go's enumerated map, manager-surface
// column): in_progress→{in_review, done, pending_input},
// in_review→{in_progress, done, pending_input}, and the pending_input
// human exits →{in_progress, backlog}. The boardStageCancelled terminal is NOT a move target
// (cancel is its own terminal tool), and entering the pipeline FROM
// backlog is claim-only (task_start/dispatch — no manager write
// exists), so the schema must not advertise it.
var boardMoveTargets = []string{
	"in_progress", "in_review", "done", "pending_input", "backlog",
}

var boardPriorities = []string{"P0", "P1", "P2", "P3"}

var boardListProjectsTool = mcp.NewTool("board_list_projects",
	mcp.WithDescription("List the authenticated user's board projects (ruling R7 — user-level only). "+
		"Each project is a swimlane targeting one workspace; parallel work = sibling projects."),
	mcp.WithReadOnlyHintAnnotation(true),
)

var boardGetTool = mcp.NewTool("board_get",
	mcp.WithDescription("Get one board item by short id. The prefix selects the kind: prj- a project, "+
		"epc- an epic (with derived progress), tsk- a task (with blocked-by derivation). "+
		"Short-id addressing is the surface's contract (design 0067 §4.3a)."),
	mcp.WithString("item_id", mcp.Required(),
		mcp.Description("Short id: prj-XXXXXXXX, epc-XXXXXXXX, or tsk-XXXXXXXX")),
	mcp.WithReadOnlyHintAnnotation(true),
)

var boardReadTool = mcp.NewTool("board_read",
	mcp.WithDescription("Read a project's tasks, optionally filtered by one of the six stages "+
		"(backlog, in_progress, in_review, done, pending_input, cancel"+"led). "+ //nolint:misspell // design-verbatim
		"Search and health projections join this tool when the engine seams land (#1699)."),
	mcp.WithString("project_id", mcp.Required(), mcp.Description("Project short id (prj-XXXXXXXX)")),
	mcp.WithString("stage", mcp.Description("Optional stage filter (one of the six stages)")),
	mcp.WithReadOnlyHintAnnotation(true),
)

var boardCreateProjectTool = mcp.NewTool("board_create_project",
	mcp.WithDescription("Create a board project: a swimlane with ONE target workspace the owning user "+
		"can access (rulings R7/R8) and a worker-slot budget (max concurrent in-flight claims, default 2)."),
	mcp.WithString("name", mcp.Required(), mcp.Description("Project name")),
	mcp.WithString("target_workspace_id", mcp.Required(),
		mcp.Description("The single target workspace (ruling R8 — parallel work = sibling projects)")),
	mcp.WithInteger("worker_slots", mcp.Description("Max concurrent in-flight claims (default 2)")),
	mcp.WithArray("tags", mcp.WithStringItems(), mcp.Description("Tags (on update, omission CLEARS — send the full set to keep)")),
	mcp.WithString("request_id",
		mcp.Description("Idempotency key: reuse ONE id per logical operation across retries (clientMessageID discipline)")),
)

var boardCreateEpicTool = mcp.NewTool("board_create_epic",
	mcp.WithDescription("Create a DRAFT epic in a project (ruling R2's default). Activate it to start "+
		"feeding work to fresh workers; the outcome field is the human's steering wheel."),
	mcp.WithString("project_id", mcp.Required(), mcp.Description("Project short id (prj-XXXXXXXX)")),
	mcp.WithString("title", mcp.Required(), mcp.Description("Epic title")),
	mcp.WithString("outcome", mcp.Description("What this epic exists to achieve (the steering wheel)")),
	mcp.WithString("priority", mcp.Enum(boardPriorities...),
		mcp.Description("P0-P3, default P2 (display/filter only — the ready set orders by task priority)")),
	mcp.WithArray("tags", mcp.WithStringItems(), mcp.Description("Tags")),
	mcp.WithString("request_id", mcp.Description("Idempotency key (one per logical operation)")),
)

var boardCreateTaskTool = mcp.NewTool("board_create_task",
	mcp.WithDescription("Create a backlog task in an epic. The description (markdown) seeds the worker "+
		"charter; acceptance criteria define Done (ALL met + evidence_url — ruling R2). Tags may carry "+
		"the explicit pr:N anchor (owner ruling OR4 — pattern ^pr:[1-9][0-9]*$, the only PR-drift form "+
		"the reconciler consumes; malformed anchors are inert tags, never guessed)."),
	mcp.WithString("epic_id", mcp.Required(), mcp.Description("Epic short id (epc-XXXXXXXX)")),
	mcp.WithString("title", mcp.Required(), mcp.Description("Task title")),
	mcp.WithString("description", mcp.Description("Markdown; seeds the fresh worker's charter (R9)")),
	mcp.WithString("priority", mcp.Enum(boardPriorities...), mcp.Description("P0-P3, default P2")),
	mcp.WithArray("tags", mcp.WithStringItems(), mcp.Description("Tags (may include one pr:N anchor; the full set — project/epic updates rewrite tags wholesale)")),
	mcp.WithArray("acceptance_criteria",
		mcp.Items(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"text": map[string]any{"type": "string"},
				"met":  map[string]any{"type": "boolean"},
			},
			"required": []string{"text"},
		}),
		mcp.Description("Checkable criteria [{text, met}] — Done = ALL met + evidence")),
	mcp.WithString("request_id", mcp.Description("Idempotency key (one per logical operation)")),
)

var boardUpdateItemTool = mcp.NewTool("board_update_item",
	mcp.WithDescription("Patch a board item by short id; only fields applying to the addressed kind are "+
		"accepted (a field for another kind is an error, not a silent drop). Omitted scalar fields keep "+
		"existing values. TAGS semantics differ by kind (engine rules, REST-identical): on a task, omitted "+
		"tags KEEP the existing set and an explicit [] CLEARS it; on a project or epic, tags are written "+
		"unconditionally — OMITTING THEM CLEARS the set (send the full set to keep it). Acceptance criteria "+
		"REPLACE when present. set_criteria_met checks off the criterion at the 0-based index (the "+
		"reviewer's Done preparation). The assignee is engine-set, never a patch field."),
	mcp.WithString("item_id", mcp.Required(),
		mcp.Description("Short id: prj-XXXXXXXX (name/target_workspace_id/worker_slots/tags), "+
			"epc-XXXXXXXX (title/outcome/priority/tags), tsk-XXXXXXXX (title/description/priority/tags/"+
			"acceptance_criteria/set_criteria_met)")),
	mcp.WithString("name", mcp.Description("Project name (prj- only; rename cascades the display inheritance)")),
	mcp.WithString("target_workspace_id", mcp.Description("Project target workspace (prj- only; must be accessible to the owner)")),
	mcp.WithInteger("worker_slots", mcp.Description("Project worker-slot budget (prj- only)")),
	mcp.WithString("title", mcp.Description("Epic/task title (epc-/tsk- only)")),
	mcp.WithString("outcome", mcp.Description("Epic outcome (epc- only)")),
	mcp.WithString("description", mcp.Description("Task description markdown (tsk- only)")),
	mcp.WithString("priority", mcp.Enum(boardPriorities...), mcp.Description("Epic/task priority (epc-/tsk- only)")),
	mcp.WithArray("tags", mcp.WithStringItems(), mcp.Description("tsk-: omitted keeps, [] clears; prj-/epc-: written unconditionally — omitted CLEARS (send the full set to keep)")),
	mcp.WithArray("acceptance_criteria",
		mcp.Items(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"text": map[string]any{"type": "string"},
				"met":  map[string]any{"type": "boolean"},
			},
			"required": []string{"text"},
		}),
		mcp.Description("Task criteria [{text, met}] (tsk- only; REPLACE when present)")),
	mcp.WithInteger("set_criteria_met",
		mcp.Description("Check off the criterion at this 0-based index (tsk- only)")),
	mcp.WithString("request_id", mcp.Description("Idempotency key (one per logical operation)")),
)

var boardMoveStageTool = mcp.NewTool("board_move_stage",
	mcp.WithDescription("Move a task to another stage (server-arbitrated; a drag that loses a race "+
		"returns the authoritative guard error). Legal manager moves: in_progress→{in_review, done, pending_input}, "+
		"in_review→{in_progress (the review bounce), done, pending_input}, and the pending_input human exits "+
		"→{in_progress, backlog}. Entering the pipeline from backlog is claim-only (board_start_task — no manager "+
		"write exists); the terminal stage is reached via board_cancel, never a stage move. done requires ALL acceptance "+
		"criteria met + evidence_url (ruling R2)."),
	mcp.WithString("task_id", mcp.Required(), mcp.Description("Task short id (tsk-XXXXXXXX)")),
	mcp.WithString("to_stage", mcp.Required(), mcp.Enum(boardMoveTargets...), mcp.Description("Target stage")),
	mcp.WithString("evidence_url", mcp.Description("Artifact link for THIS transition (required for done)")),
	mcp.WithString("request_id", mcp.Description("Idempotency key (one per logical operation)")),
)

var boardStartTaskTool = mcp.NewTool("board_start_task",
	mcp.WithDescription("Manual one-off dispatch (task_start): claim + fresh-charter worker spawn into "+
		"the project's target workspace (R9). Explicitly allowed in draft epics; paused/completed/archived "+
		"epics and archived projects refuse it. No caller-supplied worker identity exists on any surface."),
	mcp.WithString("task_id", mcp.Required(), mcp.Description("Task short id (tsk-XXXXXXXX)")),
	mcp.WithString("request_id", mcp.Description("Idempotency key (one per logical operation)")),
)

var boardEpicActivateTool = mcp.NewTool("board_epic_activate",
	mcp.WithDescription("Activate an epic: start feeding ready work to fresh workers. Re-opens a completed "+
		"epic; unarchive always restores active."),
	mcp.WithString("epic_id", mcp.Required(), mcp.Description("Epic short id (epc-XXXXXXXX)")),
)

var boardEpicPauseTool = mcp.NewTool("board_epic_pause",
	mcp.WithDescription("Pause an epic: stop NEW claims without interrupting in-flight leases."),
	mcp.WithString("epic_id", mcp.Required(), mcp.Description("Epic short id (epc-XXXXXXXX)")),
)

var boardEpicArchiveTool = mcp.NewTool("board_epic_archive",
	mcp.WithDescription("Archive an epic (reversible storage; it leaves the candidate set)."),
	mcp.WithString("epic_id", mcp.Required(), mcp.Description("Epic short id (epc-XXXXXXXX)")),
)

var boardEpicUnarchiveTool = mcp.NewTool("board_epic_unarchive",
	mcp.WithDescription("Unarchive an epic — always restores active (the resumable default)."),
	mcp.WithString("epic_id", mcp.Required(), mcp.Description("Epic short id (epc-XXXXXXXX)")),
)

var boardProjectArchiveTool = mcp.NewTool("board_project_archive",
	mcp.WithDescription("Archive a project (reversible): stops ALL claims into it."),
	mcp.WithString("project_id", mcp.Required(), mcp.Description("Project short id (prj-XXXXXXXX)")),
)

var boardProjectUnarchiveTool = mcp.NewTool("board_project_unarchive",
	mcp.WithDescription("Unarchive a project: restores active."),
	mcp.WithString("project_id", mcp.Required(), mcp.Description("Project short id (prj-XXXXXXXX)")),
)

var boardCancelTool = mcp.NewTool("board_cancel",
	mcp.WithDescription("Terminal cancel of an epic or task by short id. Revokes the lease + claim token, "+
		"aborts the worker, cascades to descendants (epic) and stages non-terminal dependents to "+
		"pending_input (\"dependency abandoned\"). The cancel"+"led stage is immutable-terminal: never resurrects."), //nolint:misspell
	mcp.WithString("item_id", mcp.Required(), mcp.Description("Epic or task short id (epc-/tsk-)")),
	mcp.WithString("reason", mcp.Description("Why (audit trail)")),
	mcp.WithString("request_id", mcp.Description("Idempotency key (one per logical operation)")),
)

var boardAddDependencyTool = mcp.NewTool("board_add_dependency",
	mcp.WithDescription("Add a structural dependency edge (rulings R3/R4 — the ONLY blocking mechanism): "+
		"N-ary task→task within a project (cross-epic allowed, never cross-project); cycles are rejected "+
		"with did-you-mean suggestions (409). Unblocks when ALL targets are Done."),
	mcp.WithString("task_id", mcp.Required(), mcp.Description("Dependent task short id (tsk-XXXXXXXX)")),
	mcp.WithString("depends_on", mcp.Required(), mcp.Description("Prerequisite task short id (tsk-XXXXXXXX)")),
	mcp.WithString("request_id", mcp.Description("Idempotency key (one per logical operation)")),
)

var boardRemoveDependencyTool = mcp.NewTool("board_remove_dependency",
	mcp.WithDescription("Remove a dependency edge — one of the pending_input resolutions (removing an "+
		"abandoned prerequisite's edge unblocks the row; the human re-queue stays a move-stage write)."),
	mcp.WithString("task_id", mcp.Required(), mcp.Description("Dependent task short id (tsk-XXXXXXXX)")),
	mcp.WithString("depends_on", mcp.Required(), mcp.Description("Prerequisite task short id (tsk-XXXXXXXX)")),
)

// --- Tool handlers ---

func (h *handlers) boardListProjects(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	resp, err := h.client.BoardListProjects(ctx)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to list board projects: %v", err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardGet(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	id := strArg(args, "item_id")
	if id == "" {
		return mcp.NewToolResultError("item_id is required"), nil
	}
	kind, err := boardItemKind(id)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var resp json.RawMessage
	switch kind {
	case "project":
		resp, err = h.client.BoardGetProject(ctx, id)
	case "epic":
		resp, err = h.client.BoardGetEpic(ctx, id)
	case "task":
		resp, err = h.client.BoardGetTask(ctx, id)
	}
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to get %s %s: %v", kind, id, err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardRead(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	projectID := strArg(args, "project_id")
	if projectID == "" {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	stage := strArg(args, "stage")
	if stage != "" && !boardStageSet()[stage] {
		return mcp.NewToolResultError("stage filter must be one of " + strings.Join(boardStages, ", ")), nil
	}
	resp, err := h.client.BoardListTasks(ctx, projectID, stage)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to read board tasks: %v", err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardCreateProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	name := strArg(args, "name")
	targetWS := strArg(args, "target_workspace_id")
	if name == "" || targetWS == "" {
		return mcp.NewToolResultError("name and target_workspace_id are required"), nil
	}
	resp, err := h.client.BoardCreateProject(ctx, BoardProjectBody{
		Name: name, TargetWorkspaceID: targetWS,
		WorkerSlots: intArg(args, "worker_slots"), Tags: stringSliceArg(args, "tags"),
		RequestID: strArg(args, "request_id"),
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to create board project: %v", err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardCreateEpic(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	projectID := strArg(args, "project_id")
	title := strArg(args, "title")
	if projectID == "" || title == "" {
		return mcp.NewToolResultError("project_id and title are required"), nil
	}
	resp, err := h.client.BoardCreateEpic(ctx, projectID, BoardEpicBody{
		Title: title, Outcome: strArg(args, "outcome"), Priority: strArg(args, "priority"),
		Tags: stringSliceArg(args, "tags"), RequestID: strArg(args, "request_id"),
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to create board epic: %v", err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardCreateTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	epicID := strArg(args, "epic_id")
	title := strArg(args, "title")
	if epicID == "" || title == "" {
		return mcp.NewToolResultError("epic_id and title are required"), nil
	}
	criteria, err := boardCriteriaArg(args, "acceptance_criteria")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	resp, err := h.client.BoardCreateTask(ctx, epicID, BoardTaskBody{
		Title: title, Description: strArg(args, "description"), Priority: strArg(args, "priority"),
		Tags: stringSlicePtrArg(args, "tags"), AcceptanceCriteria: criteria,
		RequestID: strArg(args, "request_id"),
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to create board task: %v", err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardUpdateItem(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	id := strArg(args, "item_id")
	if id == "" {
		return mcp.NewToolResultError("item_id is required"), nil
	}
	kind, err := boardItemKind(id)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	// Kind-scoped fields: only the addressed kind's PATCH fields are
	// accepted — a field for another kind is an explicit error, never a
	// silent drop.
	allowed := map[string]bool{"item_id": true, "request_id": true}
	switch kind {
	case "project":
		allowed["name"] = true
		allowed["target_workspace_id"] = true
		allowed["worker_slots"] = true
		allowed["tags"] = true
	case "epic":
		allowed["title"] = true
		allowed["outcome"] = true
		allowed["priority"] = true
		allowed["tags"] = true
	case "task":
		allowed["title"] = true
		allowed["description"] = true
		allowed["priority"] = true
		allowed["tags"] = true
		allowed["acceptance_criteria"] = true
		allowed["set_criteria_met"] = true
	}
	var extraneous []string
	for k := range args {
		if !allowed[k] {
			extraneous = append(extraneous, k)
		}
	}
	if len(extraneous) > 0 {
		sort.Strings(extraneous)
		return mcp.NewToolResultError(fmt.Sprintf(
			"field(s) %v do not apply to a %s update (this tool patches one kind at a time; the prj-/epc-/tsk- prefix selects it)", extraneous, kind)), nil
	}
	requestID := strArg(args, "request_id")
	var resp json.RawMessage
	switch kind {
	case "project":
		resp, err = h.client.BoardUpdateProject(ctx, id, BoardProjectBody{
			Name: strArg(args, "name"), TargetWorkspaceID: strArg(args, "target_workspace_id"),
			WorkerSlots: intArg(args, "worker_slots"), Tags: stringSliceArg(args, "tags"),
			RequestID: requestID,
		})
	case "epic":
		resp, err = h.client.BoardUpdateEpic(ctx, id, BoardEpicBody{
			Title: strArg(args, "title"), Outcome: strArg(args, "outcome"),
			Priority: strArg(args, "priority"), Tags: stringSliceArg(args, "tags"),
			RequestID: requestID,
		})
	case "task":
		criteria, cerr := boardCriteriaArg(args, "acceptance_criteria")
		if cerr != nil {
			return mcp.NewToolResultError(cerr.Error()), nil
		}
		resp, err = h.client.BoardUpdateTask(ctx, id, BoardTaskBody{
			Title: strArg(args, "title"), Description: strArg(args, "description"),
			Priority: strArg(args, "priority"), Tags: stringSlicePtrArg(args, "tags"),
			AcceptanceCriteria: criteria, SetCriteriaMet: intPtrArg(args, "set_criteria_met"),
			RequestID: requestID,
		})
	}
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to update board %s %s: %v", kind, id, err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardMoveStage(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	taskID := strArg(args, "task_id")
	toStage := strArg(args, "to_stage")
	if taskID == "" || toStage == "" {
		return mcp.NewToolResultError("task_id and to_stage are required"), nil
	}
	if !boardMoveTargetSet()[toStage] {
		return mcp.NewToolResultError("to_stage must be a legal manager move target: " + strings.Join(boardMoveTargets, ", ") +
			" (" + boardStageCancelled + " is board_cancel's terminal, and entering the pipeline from backlog is claim-only — board_start_task)"), nil
	}
	resp, err := h.client.BoardMoveStage(ctx, taskID, toStage, strArg(args, "evidence_url"), strArg(args, "request_id"))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to move stage: %v", err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardStartTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	taskID := strArg(args, "task_id")
	if taskID == "" {
		return mcp.NewToolResultError("task_id is required"), nil
	}
	resp, err := h.client.BoardStartTask(ctx, taskID, strArg(args, "request_id"))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to start task: %v", err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardEpicLifecycle(ctx context.Context, req mcp.CallToolRequest,
	call func(ctx context.Context, epicID string) (json.RawMessage, error), verb string) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	epicID := strArg(args, "epic_id")
	if epicID == "" {
		return mcp.NewToolResultError("epic_id is required"), nil
	}
	resp, err := call(ctx, epicID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to %s epic: %v", verb, err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardEpicActivate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return h.boardEpicLifecycle(ctx, req, h.client.BoardEpicActivate, "activate")
}

func (h *handlers) boardEpicPause(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return h.boardEpicLifecycle(ctx, req, h.client.BoardEpicPause, "pause")
}

func (h *handlers) boardEpicArchive(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return h.boardEpicLifecycle(ctx, req, h.client.BoardEpicArchive, "archive")
}

func (h *handlers) boardEpicUnarchive(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return h.boardEpicLifecycle(ctx, req, h.client.BoardEpicUnarchive, "unarchive")
}

func (h *handlers) boardProjectLifecycle(ctx context.Context, req mcp.CallToolRequest,
	call func(ctx context.Context, projectID string) (json.RawMessage, error), verb string) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	projectID := strArg(args, "project_id")
	if projectID == "" {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	resp, err := call(ctx, projectID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to %s project: %v", verb, err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardProjectArchive(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return h.boardProjectLifecycle(ctx, req, h.client.BoardArchiveProject, "archive")
}

func (h *handlers) boardProjectUnarchive(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return h.boardProjectLifecycle(ctx, req, h.client.BoardUnarchiveProject, "unarchive")
}

func (h *handlers) boardCancel(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	id := strArg(args, "item_id")
	if id == "" {
		return mcp.NewToolResultError("item_id is required"), nil
	}
	reason := strArg(args, "reason")
	requestID := strArg(args, "request_id")
	var resp json.RawMessage
	var err error
	kind, kerr := boardItemKind(id)
	if kerr != nil {
		return mcp.NewToolResultError(kerr.Error()), nil
	}
	switch kind {
	case "epic":
		resp, err = h.client.BoardCancelEpic(ctx, id, reason, requestID)
	case "task":
		resp, err = h.client.BoardCancelTask(ctx, id, reason, requestID)
	case "project":
		return mcp.NewToolResultError("projects are not " + boardStageCancelled + " — archive them (reversible storage); " + boardStageCancelled + " is for epics and tasks"), nil
	}
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to cancel %s %s: %v", kind, id, err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardAddDependency(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	taskID := strArg(args, "task_id")
	dependsOn := strArg(args, "depends_on")
	if taskID == "" || dependsOn == "" {
		return mcp.NewToolResultError("task_id and depends_on are required"), nil
	}
	resp, err := h.client.BoardAddDependency(ctx, taskID, dependsOn, strArg(args, "request_id"))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to add dependency: %v", err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardRemoveDependency(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	taskID := strArg(args, "task_id")
	dependsOn := strArg(args, "depends_on")
	if taskID == "" || dependsOn == "" {
		return mcp.NewToolResultError("task_id and depends_on are required"), nil
	}
	if err := h.client.BoardRemoveDependency(ctx, taskID, dependsOn); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to remove dependency: %v", err)), nil
	}
	return mcp.NewToolResultText(`{"removed":true}`), nil
}

// boardItemKind resolves the prj-/epc-/tsk- prefix to its REST resource.
// Bare UUIDs are refused: the unified tools (board_get, board_update_item,
// board_cancel) need the kind to select the route, and short ids are this
// surface's addressing contract (§4.3a).
func boardItemKind(id string) (string, error) {
	switch {
	case strings.HasPrefix(id, "prj-"):
		return "project", nil
	case strings.HasPrefix(id, "epc-"):
		return "epic", nil
	case strings.HasPrefix(id, "tsk-"):
		return "task", nil
	default:
		return "", fmt.Errorf("item_id must be a short id carrying its prj-/epc-/tsk- prefix (got %q) — the prefix selects the item kind", id)
	}
}

// boardStageSet is ruling R2's six-stage binding set as a set (the
// board_read filter's vocabulary — built from boardStages, the single
// source).
func boardStageSet() map[string]bool {
	out := make(map[string]bool, len(boardStages))
	for _, s := range boardStages {
		out[s] = true
	}
	return out
}

// boardMoveTargetSet is the legal manager move_stage target set (built
// from boardMoveTargets, the single source).
func boardMoveTargetSet() map[string]bool {
	out := make(map[string]bool, len(boardMoveTargets))
	for _, s := range boardMoveTargets {
		out[s] = true
	}
	return out
}

// stringSlicePtrArg is the presence-aware slice decoder (r2): a PRESENT
// key yields a pointer to the (possibly empty) slice, an ABSENT key
// yields nil. The REST binding carries exactly this distinction for
// task tags — "tags":[] clears, omission keeps — so the wire must see
// it too.
func stringSlicePtrArg(args map[string]any, key string) *[]string {
	raw, ok := args[key]
	if !ok || raw == nil {
		return nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		s, _ := v.(string)
		out = append(out, s)
	}
	return &out
}

// intArg pulls an integer argument. JSON decoding yields float64;
// direct in-process callers may hand over native ints — both are
// accepted (the run_resolve/workflow handlers' string-arg pattern,
// numeric edition).
func intArg(args map[string]any, key string) int {
	if v, ok := args[key].(float64); ok {
		return int(v)
	}
	if v, ok := args[key].(int); ok {
		return v
	}
	if v, ok := args[key].(int64); ok {
		return int(v)
	}
	return 0
}

// intPtrArg pulls an optional integer argument; nil when absent.
func intPtrArg(args map[string]any, key string) *int {
	if raw, ok := args[key]; ok && raw != nil {
		switch v := raw.(type) {
		case float64:
			i := int(v)
			return &i
		case int:
			return &v
		case int64:
			i := int(v)
			return &i
		}
	}
	return nil
}

// boardCriteriaArg decodes the acceptance_criteria array ([{text, met}],
// ruling R2) from MCP arguments.
func boardCriteriaArg(args map[string]any, key string) ([]BoardCriterion, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return nil, nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array of {text, met} objects", key)
	}
	out := make([]BoardCriterion, 0, len(arr))
	for _, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s entries must be {text, met} objects", key)
		}
		text, _ := m["text"].(string)
		if text == "" {
			return nil, fmt.Errorf("each acceptance criterion requires non-empty text")
		}
		met, _ := m["met"].(bool)
		out = append(out, BoardCriterion{Text: text, Met: met})
	}
	return out, nil
}

// AddBoardTools registers the external board_* tools (design 0067 §4.3b)
// on an existing MCPServer. Called by NewServer after the Epic 64 tools.
func AddBoardTools(srv *server.MCPServer, h *handlers) {
	srv.AddTools(
		server.ServerTool{Tool: boardListProjectsTool, Handler: h.boardListProjects},
		server.ServerTool{Tool: boardGetTool, Handler: h.boardGet},
		server.ServerTool{Tool: boardReadTool, Handler: h.boardRead},
		server.ServerTool{Tool: boardCreateProjectTool, Handler: h.boardCreateProject},
		server.ServerTool{Tool: boardCreateEpicTool, Handler: h.boardCreateEpic},
		server.ServerTool{Tool: boardCreateTaskTool, Handler: h.boardCreateTask},
		server.ServerTool{Tool: boardUpdateItemTool, Handler: h.boardUpdateItem},
		server.ServerTool{Tool: boardMoveStageTool, Handler: h.boardMoveStage},
		server.ServerTool{Tool: boardStartTaskTool, Handler: h.boardStartTask},
		server.ServerTool{Tool: boardEpicActivateTool, Handler: h.boardEpicActivate},
		server.ServerTool{Tool: boardEpicPauseTool, Handler: h.boardEpicPause},
		server.ServerTool{Tool: boardEpicArchiveTool, Handler: h.boardEpicArchive},
		server.ServerTool{Tool: boardEpicUnarchiveTool, Handler: h.boardEpicUnarchive},
		server.ServerTool{Tool: boardProjectArchiveTool, Handler: h.boardProjectArchive},
		server.ServerTool{Tool: boardProjectUnarchiveTool, Handler: h.boardProjectUnarchive},
		server.ServerTool{Tool: boardCancelTool, Handler: h.boardCancel},
		server.ServerTool{Tool: boardAddDependencyTool, Handler: h.boardAddDependency},
		server.ServerTool{Tool: boardRemoveDependencyTool, Handler: h.boardRemoveDependency},
	)
}
