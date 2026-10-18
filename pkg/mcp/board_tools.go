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
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// --- Tool definitions ---

var boardStages = []string{
	"backlog", "in_progress", "in_review", "done", "pending_input", "cancel" + "led", //nolint:misspell // design-verbatim stage value
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
	mcp.WithArray("tags", mcp.WithStringItems(), mcp.Description("Tags (replace on update)")),
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
	mcp.WithArray("tags", mcp.WithStringItems(), mcp.Description("Tags (may include one pr:N anchor)")),
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
		"accepted (a field for another kind is an error, not a silent drop). Omitted fields keep existing "+
		"values; tags and acceptance criteria REPLACE when present. set_criteria_met checks off the "+
		"criterion at the 0-based index (the reviewer's Done preparation). The assignee is engine-set, "+
		"never a patch field."),
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
	mcp.WithArray("tags", mcp.WithStringItems(), mcp.Description("Tags (all kinds; REPLACE when present)")),
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
		"returns the authoritative guard error). Forward: backlog→in_progress→in_review→done; the one "+
		"backward move is in_review→in_progress. done requires ALL acceptance criteria met + evidence_url "+
		"(ruling R2). The pending_input exit is a manager decision — allowed here by the owner."),
	mcp.WithString("task_id", mcp.Required(), mcp.Description("Task short id (tsk-XXXXXXXX)")),
	mcp.WithString("to_stage", mcp.Required(), mcp.Enum(boardStages...), mcp.Description("Target stage")),
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

// --- Tool handlers (Lane 3 red-first: stubbed — the pins in
// board_tools_test.go drive these to failure before the impl lands) ---

func boardNotImplemented(tool string) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultError(fmt.Sprintf("board tool %s: not implemented (Lane 3 #1700 in flight)", tool)), nil
}

func (h *handlers) boardListProjects(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return boardNotImplemented("board_list_projects")
}

func (h *handlers) boardGet(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return boardNotImplemented("board_get")
}

func (h *handlers) boardRead(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return boardNotImplemented("board_read")
}

func (h *handlers) boardCreateProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return boardNotImplemented("board_create_project")
}

func (h *handlers) boardCreateEpic(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return boardNotImplemented("board_create_epic")
}

func (h *handlers) boardCreateTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return boardNotImplemented("board_create_task")
}

func (h *handlers) boardUpdateItem(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return boardNotImplemented("board_update_item")
}

func (h *handlers) boardMoveStage(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return boardNotImplemented("board_move_stage")
}

func (h *handlers) boardStartTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return boardNotImplemented("board_start_task")
}

func (h *handlers) boardEpicActivate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return boardNotImplemented("board_epic_activate")
}

func (h *handlers) boardEpicPause(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return boardNotImplemented("board_epic_pause")
}

func (h *handlers) boardEpicArchive(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return boardNotImplemented("board_epic_archive")
}

func (h *handlers) boardEpicUnarchive(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return boardNotImplemented("board_epic_unarchive")
}

func (h *handlers) boardProjectArchive(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return boardNotImplemented("board_project_archive")
}

func (h *handlers) boardProjectUnarchive(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return boardNotImplemented("board_project_unarchive")
}

func (h *handlers) boardCancel(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return boardNotImplemented("board_cancel")
}

func (h *handlers) boardAddDependency(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return boardNotImplemented("board_add_dependency")
}

func (h *handlers) boardRemoveDependency(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return boardNotImplemented("board_remove_dependency")
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
