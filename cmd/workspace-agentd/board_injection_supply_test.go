// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// board_injection_supply_test.go — #1747 §5 IDENTITY SUPPLY freeze pin
// (review round 8, finding 1): the origin plugin's INJECTED_SESSION_TOOLS
// and the agentd registry's manager catalog must move in LOCKSTEP —
//
//   - every manager tool the registry serves is injected with the
//     calling session's identity by the plugin (a manager tool missing
//     from the plugin set loses its identity supply: dispatch falls to
//     the model-declared copy — exactly the §5 threat model), and
//   - nothing in the plugin set is unregistered (a stale entry injects
//     identity onto a tool that will never read it — dead weight that
//     silently rots).
//
// Source-scan, both directions, no fakes: the plugin file is parsed as
// text (the harness executes it as JS, not Go), the registry is the
// real mcpToolCatalog/boardManagerTools.

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestMCPBoard_ManagerInjectionSupply(t *testing.T) {
	pluginPath := filepath.Join("..", "..", "runtimes", "opencode", "plugins", "llmsafespaces-origin.js")
	raw, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("plugin source must exist at %s: %v", pluginPath, err)
	}
	block := regexp.MustCompile(`(?s)const INJECTED_SESSION_TOOLS = new Set\(\[(.*?)\]\)`).
		FindStringSubmatch(string(raw))
	if block == nil {
		t.Fatal("INJECTED_SESSION_TOOLS set not found in the plugin source")
	}
	pluginSet := map[string]bool{}
	for _, m := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(block[1], -1) {
		pluginSet[m[1]] = true
	}
	if len(pluginSet) == 0 {
		t.Fatal("INJECTED_SESSION_TOOLS parsed empty — the source-scan regex drifted from the plugin's shape")
	}

	registry := map[string]bool{}
	for _, tool := range mcpToolCatalog() {
		registry["llmsafespaces_"+tool.Name] = true
	}
	for _, tool := range boardManagerTools() {
		registry["llmsafespaces_"+tool.Name] = true
	}

	// Direction 1: no manager tool missing from the plugin.
	for _, tool := range boardManagerTools() {
		qualified := "llmsafespaces_" + tool.Name
		if !pluginSet[qualified] {
			t.Errorf("manager tool %s is registered but NOT in the plugin's INJECTED_SESSION_TOOLS — its identity supply is gone (ruling #1747 §5)", qualified)
		}
	}
	// Direction 2: nothing in the plugin that isn't registered.
	for name := range pluginSet {
		if !registry[name] {
			t.Errorf("plugin injects %s but the agentd registry serves no such tool — stale entry, remove it", name)
		}
	}
	// The platform trio stays injected (send_message attribution #1465,
	// session_metadata/delete_session self-scope #1627).
	for _, name := range []string{
		"llmsafespaces_send_message",
		"llmsafespaces_session_metadata",
		"llmsafespaces_delete_session",
	} {
		if !pluginSet[name] {
			t.Errorf("platform tool %s lost its injection — the pre-#1747 baseline must hold", name)
		}
	}
}
