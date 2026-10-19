// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// board_manager_tools.go — the #1747 manager board tools' dispatch:
// session gate (fail-closed on missing identity; worker-registry
// refusal), verbatim body passthrough with the resolver stamps, and
// one wire call per tool over the pod-identity seam. The platform
// validates; agentd re-models nothing (the mcpAutomation discipline).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
)

// boardManagerSession resolves and gates the calling session for a
// manager tool call (#1747 §5): the harness-attested injected identity
// (the send_message pattern — injection wins over any declared copy),
// fail-closed when absent, refused when the session was spawned as a
// board worker.
func boardManagerSession(name string, body map[string]any) (string, error) {
	injected, _ := body["lsp_injected_session"].(string)
	declared, _ := body["sessionId"].(string)
	sid := strings.TrimSpace(injected)
	if sid == "" {
		sid = strings.TrimSpace(declared)
	}
	if sid == "" {
		return "", fmt.Errorf("%s failed: manager board tools require the calling session's identity — "+
			"spawned workers and unidentified callers hold no manager grant (ruling #1747 §5)", name)
	}
	if boardWorkerSessions.isWorker(sid) {
		return "", fmt.Errorf("%s failed: this session was spawned as a board worker — manager board tools "+
			"attach to user-collaborator sessions only (ruling #1747 §5)", name)
	}
	return sid, nil
}

// boardArgs unwraps the documented flat argument form, tolerating the
// nested {"project"|"epic"|"task"|"plan": {...}} wrapper
// (passThroughBody's convention, boardToolArgs' generalized form).
func boardArgs(body map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		if nested, ok := body[k].(map[string]any); ok {
			return nested
		}
	}
	return body
}

// boardRefArg extracts a validated path reference argument.
func boardRefArg(args map[string]any, key string) string {
	ref, _ := args[key].(string)
	return strings.TrimSpace(ref)
}

// boardBoolArg reads an optional boolean argument.
func boardBoolArg(args map[string]any, key string) bool {
	v, _ := args[key].(bool)
	return v
}

// boardWireBody marshals the caller's argument set for verbatim wire
// submission (minus the routing/identity fields agentd owns).
func boardWireBody(args map[string]any) []byte {
	clean := make(map[string]any, len(args))
	for k, v := range args {
		if k == "lsp_injected_session" || k == "sessionId" {
			continue
		}
		clean[k] = v
	}
	b, err := json.Marshal(clean)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// dispatchBoardManagerTool routes one manager tool to its seam call.
func dispatchBoardManagerTool(ctx context.Context, client *opencode.Client, name string, body map[string]any, saToken, workspaceID string) (*opencode.AutomationResponse, error) {
	sessionID, err := boardManagerSession(name, body)
	if err != nil {
		return nil, err
	}
	args := boardArgs(body, "project", "epic", "task", "plan")
	wire := boardWireBody(args)

	switch name {
	case "board_create_project":
		return client.BoardCreateProject(ctx, saToken, workspaceID, sessionID, wire)
	case "board_update_project":
		return client.BoardUpdateProject(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "projectId"), wire)
	case "board_project_archive":
		return client.BoardManagerProjectAction(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "projectId"), "archive")
	case "board_project_unarchive":
		return client.BoardManagerProjectAction(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "projectId"), "unarchive")
	case "board_project_pause":
		return client.BoardManagerProjectAction(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "projectId"), "pause")
	case "board_project_resume":
		return client.BoardManagerProjectAction(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "projectId"), "resume")
	case "board_create_epic":
		return client.BoardCreateEpic(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "projectId"), wire)
	case "board_update_epic":
		return client.BoardUpdateEpic(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "epicId"), wire)
	case "board_epic_activate":
		return client.BoardManagerEpicAction(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "epicId"), "activate", wire)
	case "board_epic_pause":
		return client.BoardManagerEpicAction(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "epicId"), "pause", wire)
	case "board_epic_archive":
		return client.BoardManagerEpicAction(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "epicId"), "archive", wire)
	case "board_epic_unarchive":
		return client.BoardManagerEpicAction(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "epicId"), "unarchive", wire)
	case "board_delete_epic":
		return client.BoardDeleteEpic(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "epicId"), boardBoolArg(args, "force"))
	case "board_create_task":
		return client.BoardCreateTask(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "epicId"), wire)
	case "board_update_task":
		return client.BoardUpdateTask(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "taskId"), wire)
	case "board_add_dependency":
		return client.BoardAddDependency(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "taskId"), wire)
	case "board_remove_dependency":
		return client.BoardRemoveDependency(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "taskId"), boardRefArg(args, "dependsOn"))
	case "board_delete_task":
		return client.BoardDeleteTask(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "taskId"), boardBoolArg(args, "reconnect"))
	case "board_pause_task":
		return client.BoardManagerTaskAction(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "taskId"), "pause")
	case "board_resume_task":
		return client.BoardManagerTaskAction(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "taskId"), "resume")
	case "board_import_plan":
		return client.BoardImportPlan(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "projectId"), wire)
	default:
		return nil, fmt.Errorf("unknown board manager tool: %s", name)
	}
}
