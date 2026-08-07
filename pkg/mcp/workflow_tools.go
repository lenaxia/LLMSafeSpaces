// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package mcp

// Epic 64: Workflow & trigger MCP tools for external agents.
//
// Adds workflow_list, workflow_get, workflow_create, workflow_update,
// workflow_run, workflow_status, workflow_cancel, trigger_list,
// trigger_create, trigger_update, trigger_delete to the platform MCP server.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// --- Tool definitions ---

var workflowListTool = mcp.NewTool("workflow_list",
	mcp.WithDescription("List workflows owned by the authenticated user"),
)

var workflowGetTool = mcp.NewTool("workflow_get",
	mcp.WithDescription("Get a workflow definition by ID"),
	mcp.WithString("workflow_id", mcp.Required(), mcp.Description("Workflow ID")),
)

var workflowCreateTool = mcp.NewTool("workflow_create",
	mcp.WithDescription("Create a new workflow definition"),
	mcp.WithString("name", mcp.Required(), mcp.Description("Workflow name")),
	mcp.WithString("spec_yaml", mcp.Required(), mcp.Description("Workflow DAG spec (JSON)")),
	mcp.WithString("status", mcp.Description("Workflow status: draft (default), active, archived")),
)

var workflowUpdateTool = mcp.NewTool("workflow_update",
	mcp.WithDescription("Update a workflow definition (partial update)"),
	mcp.WithString("workflow_id", mcp.Required(), mcp.Description("Workflow ID")),
	mcp.WithString("name", mcp.Description("New name")),
	mcp.WithString("status", mcp.Description("New status")),
	mcp.WithString("spec_yaml", mcp.Description("New spec (JSON)")),
)

var workflowRunTool = mcp.NewTool("workflow_run",
	mcp.WithDescription("Start a workflow run with the given input"),
	mcp.WithString("workflow_id", mcp.Required(), mcp.Description("Workflow ID")),
	mcp.WithString("input", mcp.Description("JSON input for the workflow")),
	mcp.WithString("workspace_id", mcp.Description("Override target workspace")),
)

var workflowStatusTool = mcp.NewTool("workflow_status",
	mcp.WithDescription("Get the status of a workflow run"),
	mcp.WithString("run_id", mcp.Required(), mcp.Description("Run ID")),
)

var workflowCancelTool = mcp.NewTool("workflow_cancel",
	mcp.WithDescription("Cancel a running workflow"),
	mcp.WithString("run_id", mcp.Required(), mcp.Description("Run ID")),
)

var triggerListTool = mcp.NewTool("trigger_list",
	mcp.WithDescription("List triggers owned by the authenticated user"),
)

var triggerCreateTool = mcp.NewTool("trigger_create",
	mcp.WithDescription("Create a new trigger"),
	mcp.WithString("name", mcp.Required(), mcp.Description("Trigger name")),
	mcp.WithString("source_type", mcp.Required(), mcp.Description("Source type: cron or webhook")),
	mcp.WithString("target_type", mcp.Required(), mcp.Description("Target type: run_workflow or run_script")),
	mcp.WithString("source_config", mcp.Required(), mcp.Description("Source config (JSON)")),
	mcp.WithString("target_config", mcp.Required(), mcp.Description("Target config (JSON)")),
)

var triggerUpdateTool = mcp.NewTool("trigger_update",
	mcp.WithDescription("Update a trigger (partial update)"),
	mcp.WithString("trigger_id", mcp.Required(), mcp.Description("Trigger ID")),
	mcp.WithString("enabled", mcp.Description("Enable/disable")),
)

var triggerDeleteTool = mcp.NewTool("trigger_delete",
	mcp.WithDescription("Delete a trigger"),
	mcp.WithString("trigger_id", mcp.Required(), mcp.Description("Trigger ID")),
)

// --- Tool handlers ---

func (h *handlers) workflowList(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	resp, err := h.client.ListWorkflows(ctx)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to list workflows: %v", err)), nil
	}
	out, _ := json.Marshal(resp)
	return mcp.NewToolResultText(string(out)), nil
}

func (h *handlers) workflowGet(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	workflowID, _ := args["workflow_id"].(string)
	if workflowID == "" {
		return mcp.NewToolResultError("workflow_id is required"), nil
	}
	resp, err := h.client.GetWorkflow(ctx, workflowID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to get workflow: %v", err)), nil
	}
	out, _ := json.Marshal(resp)
	return mcp.NewToolResultText(string(out)), nil
}

func (h *handlers) workflowCreate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	name, _ := args["name"].(string)
	specYAML, _ := args["spec_yaml"].(string)
	status, _ := args["status"].(string)
	if name == "" || specYAML == "" {
		return mcp.NewToolResultError("name and spec_yaml are required"), nil
	}
	resp, err := h.client.CreateWorkflow(ctx, name, specYAML, status)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to create workflow: %v", err)), nil
	}
	out, _ := json.Marshal(resp)
	return mcp.NewToolResultText(string(out)), nil
}

func (h *handlers) workflowUpdate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	workflowID, _ := args["workflow_id"].(string)
	if workflowID == "" {
		return mcp.NewToolResultError("workflow_id is required"), nil
	}
	name, _ := args["name"].(string)
	status, _ := args["status"].(string)
	specYAML, _ := args["spec_yaml"].(string)
	resp, err := h.client.UpdateWorkflow(ctx, workflowID, name, status, specYAML)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to update workflow: %v", err)), nil
	}
	out, _ := json.Marshal(resp)
	return mcp.NewToolResultText(string(out)), nil
}

func (h *handlers) workflowRun(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	workflowID, _ := args["workflow_id"].(string)
	if workflowID == "" {
		return mcp.NewToolResultError("workflow_id is required"), nil
	}
	input, _ := args["input"].(string)
	workspaceID, _ := args["workspace_id"].(string)
	resp, err := h.client.RunWorkflow(ctx, workflowID, input, workspaceID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to start workflow run: %v", err)), nil
	}
	out, _ := json.Marshal(resp)
	return mcp.NewToolResultText(string(out)), nil
}

func (h *handlers) workflowStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	runID, _ := args["run_id"].(string)
	if runID == "" {
		return mcp.NewToolResultError("run_id is required"), nil
	}
	resp, err := h.client.GetWorkflowRunStatus(ctx, runID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to get run status: %v", err)), nil
	}
	out, _ := json.Marshal(resp)
	return mcp.NewToolResultText(string(out)), nil
}

func (h *handlers) workflowCancel(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	runID, _ := args["run_id"].(string)
	if runID == "" {
		return mcp.NewToolResultError("run_id is required"), nil
	}
	if err := h.client.CancelWorkflowRun(ctx, runID); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to cancel run: %v", err)), nil
	}
	return mcp.NewToolResultText(`{"canceled":true}`), nil
}

func (h *handlers) triggerList(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	resp, err := h.client.ListTriggers(ctx)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to list triggers: %v", err)), nil
	}
	out, _ := json.Marshal(resp)
	return mcp.NewToolResultText(string(out)), nil
}

func (h *handlers) triggerCreate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	name, _ := args["name"].(string)
	sourceType, _ := args["source_type"].(string)
	targetType, _ := args["target_type"].(string)
	sourceConfig, _ := args["source_config"].(string)
	targetConfig, _ := args["target_config"].(string)
	if name == "" || sourceType == "" || targetType == "" {
		return mcp.NewToolResultError("name, source_type, and target_type are required"), nil
	}
	resp, err := h.client.CreateTrigger(ctx, name, sourceType, targetType, sourceConfig, targetConfig)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to create trigger: %v", err)), nil
	}
	out, _ := json.Marshal(resp)
	return mcp.NewToolResultText(string(out)), nil
}

func (h *handlers) triggerUpdate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	triggerID, _ := args["trigger_id"].(string)
	if triggerID == "" {
		return mcp.NewToolResultError("trigger_id is required"), nil
	}
	enabled, _ := args["enabled"].(string)
	resp, err := h.client.UpdateTrigger(ctx, triggerID, enabled)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to update trigger: %v", err)), nil
	}
	out, _ := json.Marshal(resp)
	return mcp.NewToolResultText(string(out)), nil
}

func (h *handlers) triggerDelete(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	triggerID, _ := args["trigger_id"].(string)
	if triggerID == "" {
		return mcp.NewToolResultError("trigger_id is required"), nil
	}
	if err := h.client.DeleteTrigger(ctx, triggerID); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to delete trigger: %v", err)), nil
	}
	return mcp.NewToolResultText(`{"deleted":true}`), nil
}

// AddWorkflowTools registers the Epic 64 workflow/trigger MCP tools on an
// existing MCPServer. Called by NewServer after registering the base tools.
func AddWorkflowTools(srv *server.MCPServer, h *handlers) {
	srv.AddTools(
		server.ServerTool{Tool: workflowListTool, Handler: h.workflowList},
		server.ServerTool{Tool: workflowGetTool, Handler: h.workflowGet},
		server.ServerTool{Tool: workflowCreateTool, Handler: h.workflowCreate},
		server.ServerTool{Tool: workflowUpdateTool, Handler: h.workflowUpdate},
		server.ServerTool{Tool: workflowRunTool, Handler: h.workflowRun},
		server.ServerTool{Tool: workflowStatusTool, Handler: h.workflowStatus},
		server.ServerTool{Tool: workflowCancelTool, Handler: h.workflowCancel},
		server.ServerTool{Tool: triggerListTool, Handler: h.triggerList},
		server.ServerTool{Tool: triggerCreateTool, Handler: h.triggerCreate},
		server.ServerTool{Tool: triggerUpdateTool, Handler: h.triggerUpdate},
		server.ServerTool{Tool: triggerDeleteTool, Handler: h.triggerDelete},
	)
}
