// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// #880: the tools/list contract. The stale signature (15 tools incl.
// session_question_reply) MUST fail; the current contract passes; a
// removed named tool fails; a below-floor registry fails.
package main

import (
	"testing"
)

func tools(names ...string) []map[string]any {
	out := make([]map[string]any, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]any{"name": n, "description": "d", "inputSchema": map[string]any{"type": "object"}})
	}
	return out
}

// currentRegistry builds the 24-tool registry shape (13 base + 11
// Epic-64) — mirrors pkg/mcp.NewServer; parity pinned cross-module by
// pkg/repolint TestCanary_MCPTools_Parity.
func currentRegistry() []map[string]any {
	base := []string{
		"workspace_create", "workspace_activate", "workspace_stop",
		"workspace_refresh_compute", "session_create", "session_message",
		"session_history", "run_resolve", "credential_create",
		"credential_list", "credential_delete", "model_list", "model_set",
	}
	epic64 := []string{
		"workflow_create", "workflow_get", "workflow_list", "workflow_run",
		"workflow_status", "workflow_update", "workflow_cancel",
		"trigger_create", "trigger_list", "trigger_update", "trigger_delete",
	}
	return tools(append(append([]string{}, base...), epic64...)...)
}

func TestCheckToolContract_CurrentRegistryPasses(t *testing.T) {
	if f := checkToolContract(currentRegistry()); len(f) != 0 {
		t.Fatalf("current 24-tool registry must pass, got failures: %v", f)
	}
}

func TestCheckToolContract_AdditiveChangePasses(t *testing.T) {
	reg := append(currentRegistry(), tools("shiny_new_tool")...)
	if f := checkToolContract(reg); len(f) != 0 {
		t.Fatalf("additive change must pass without canary edits (#880 fix direction), got: %v", f)
	}
}

func TestCheckToolContract_Stale880SignatureFails(t *testing.T) {
	stale := tools(
		"workspace_create", "workspace_activate", "workspace_stop",
		"workspace_refresh_compute", "session_create", "session_message",
		"session_history", "session_question_reply",
		"session_question_reject", "session_permission_reply",
		"credential_create", "credential_list", "credential_delete",
		"model_list", "model_set",
	)
	f := checkToolContract(stale)
	if len(f) == 0 {
		t.Fatal("the stale #880 signature (15 tools, collapsed session_*_reply) must fail")
	}
	// Both failure classes fire: missing run_resolve AND below floor.
	var missing, floor bool
	for _, x := range f {
		if x.name == "tool-present: run_resolve" {
			missing = true
		}
		if x.name == "tools: min count" {
			floor = true
		}
	}
	if !missing || !floor {
		t.Fatalf("expected missing-run_resolve and below-floor failures, got %v", f)
	}
}

func TestCheckToolContract_MissingNamedToolFails(t *testing.T) {
	reg := currentRegistry()
	filtered := reg[:0]
	for _, tt := range reg {
		if n, _ := tt["name"].(string); n != "model_set" {
			filtered = append(filtered, tt)
		}
	}
	f := checkToolContract(filtered)
	if len(f) == 0 {
		t.Fatal("a removed named tool must fail tool-present")
	}
}

func TestCheckToolContract_BelowFloorFails(t *testing.T) {
	// All named tools present but registry shrunk below the floor.
	f := checkToolContract(tools(canaryExpectedTools...))
	if len(f) == 0 {
		t.Fatal("below-floor registry must fail even with the named subset intact")
	}
}
