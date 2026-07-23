// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestSpec_Completeness verifies the actual openapi.yaml has all expected endpoints.
func TestSpec_Completeness(t *testing.T) {
	data, err := os.ReadFile("../openapi.yaml")
	if err != nil {
		t.Skipf("openapi.yaml not found (run from sdks/validate/): %v", err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("failed to parse openapi.yaml: %v", err)
	}

	paths, _ := doc["paths"].(map[string]any)
	if paths == nil {
		t.Fatal("no paths in spec")
	}

	// All endpoints that must exist (from router.go)
	expectedPaths := []struct {
		path   string
		method string
	}{
		// Auth
		{"/auth/config", "get"},
		{"/auth/register", "post"},
		{"/auth/login", "post"},
		{"/auth/logout", "post"},
		{"/auth/me", "get"},
		{"/auth/api-keys", "post"},
		{"/auth/api-keys", "get"},
		{"/auth/api-keys/{id}", "delete"},
		{"/auth/password-reset/request", "post"},
		{"/auth/password-reset/confirm", "post"},
		{"/auth/verify-email", "post"},
		{"/auth/verify-email/resend", "post"},
		{"/auth/lookup", "post"},
		{"/auth/unlock-dek", "post"},
		// Workspaces
		{"/workspaces", "get"},
		{"/workspaces", "post"},
		{"/workspaces/{id}", "get"},
		{"/workspaces/{id}", "put"},
		{"/workspaces/{id}", "delete"},
		{"/workspaces/{id}/status", "get"},
		{"/workspaces/{id}/activate", "post"},
		{"/workspaces/{id}/suspend", "post"},
		{"/workspaces/{id}/restart", "post"},
		{"/workspaces/{id}/refresh-compute", "post"},
		{"/workspaces/{id}/agent/reload", "post"},
		{"/workspaces/{id}/models", "get"},
		{"/workspaces/{id}/model", "put"},
		{"/workspaces/{id}/prompt", "get"},
		{"/workspaces/{id}/prompt", "put"},
		{"/workspaces/{id}/agent-role", "get"},
		{"/workspaces/{id}/agent-role", "put"},
		{"/workspaces/{id}/agent-role", "delete"},
		{"/workspaces/{id}/effective-agent-role", "get"},
		// Sessions
		{"/workspaces/{id}/sessions", "get"},
		{"/workspaces/{id}/sessions/new", "post"},
		{"/workspaces/{id}/sessions/active", "get"},
		{"/workspaces/{id}/sessions/{sessionId}/title", "put"},
		{"/workspaces/{id}/sessions/{sessionId}/seen", "put"},
		// Proxy
		{"/workspaces/{id}/sessions/{sessionId}/message", "post"},
		{"/workspaces/{id}/sessions/{sessionId}/message", "get"},
		{"/workspaces/{id}/sessions/{sessionId}/prompt", "post"},
		{"/workspaces/{id}/sessions/{sessionId}", "get"},
		{"/workspaces/{id}/sessions/{sessionId}", "delete"},
		{"/workspaces/{id}/sessions/{sessionId}/abort", "post"},
		// Session queue
		{"/workspaces/{id}/sessions/{sessionId}/queue", "post"},
		{"/workspaces/{id}/sessions/{sessionId}/queue", "get"},
		{"/workspaces/{id}/sessions/{sessionId}/queue/{messageId}", "delete"},
		// Agent input requests
		{"/workspaces/{id}/question", "get"},
		{"/workspaces/{id}/question/{requestID}/reply", "post"},
		{"/workspaces/{id}/question/{requestID}/reject", "post"},
		{"/workspaces/{id}/permission", "get"},
		{"/workspaces/{id}/permission/{requestID}/reply", "post"},
		// Terminal
		{"/workspaces/{id}/terminal/ticket", "post"},
		// Secrets
		{"/secrets", "post"},
		{"/secrets", "get"},
		{"/secrets/audit", "get"},
		{"/secrets/{id}", "get"},
		{"/secrets/{id}", "put"},
		{"/secrets/{id}", "delete"},
		{"/secrets/{id}/reveal", "post"},
		{"/secrets/{id}/bindings", "get"},
		{"/workspaces/{id}/bindings", "put"},
		{"/workspaces/{id}/bindings", "get"},
		{"/workspaces/{id}/reload-secrets", "post"},
		{"/workspaces/{id}/env", "put"},
		{"/workspaces/{id}/env", "get"},
		{"/workspaces/{id}/env/{name}", "delete"},
		// Settings
		{"/admin/settings", "get"},
		{"/admin/settings/schema", "get"},
		{"/admin/settings/{key}", "put"},
		{"/users/me/settings", "get"},
		{"/users/me/settings/schema", "get"},
		{"/users/me/settings/{key}", "put"},
		// Provider credentials (user)
		{"/provider-credentials", "post"},
		{"/provider-credentials", "get"},
		{"/provider-credentials/{id}", "get"},
		{"/provider-credentials/{id}", "delete"},
		{"/provider-credentials/{id}/models", "get"},
		{"/provider-credentials/{id}/bindings", "get"},
		{"/provider-credentials/{id}/bind/{workspaceId}", "post"},
		{"/provider-credentials/{id}/bind/{workspaceId}", "delete"},
		// Provider credentials (admin)
		{"/admin/provider-credentials", "post"},
		{"/admin/provider-credentials", "get"},
		{"/admin/provider-credentials/{id}", "get"},
		{"/admin/provider-credentials/{id}", "put"},
		{"/admin/provider-credentials/{id}", "delete"},
		{"/admin/provider-credentials/{id}/models", "get"},
		{"/admin/provider-credentials/{id}/auto-apply", "post"},
		{"/admin/provider-credentials/{id}/auto-apply", "get"},
		{"/admin/provider-credentials/{id}/auto-apply/{targetType}/{targetId}", "delete"},
		// Agent roles (platform)
		{"/admin/agent-roles", "get"},
		{"/admin/agent-roles", "post"},
		{"/admin/agent-roles/{id}", "get"},
		{"/admin/agent-roles/{id}", "put"},
		{"/admin/agent-roles/{id}", "delete"},
		// Platform prompt
		{"/admin/prompt", "get"},
		{"/admin/prompt", "put"},
		// Usage
		{"/usage", "get"},
		{"/usage/workspaces/{id}", "get"},
		{"/usage/quota", "get"},
		// Probe
		{"/probe-models", "post"},
		// Account
		{"/account/rotate-key", "post"},
		{"/account/change-password", "post"},
		{"/account/recover", "post"},
		// Health
		{"/livez", "get"},
		{"/readyz", "get"},
		{"/health", "get"},
	}

	for _, ep := range expectedPaths {
		pathObj, ok := paths[ep.path]
		if !ok {
			t.Errorf("missing path: %s", ep.path)
			continue
		}
		methods, _ := pathObj.(map[string]any)
		if methods == nil {
			t.Errorf("path %s has no methods", ep.path)
			continue
		}
		if _, ok := methods[ep.method]; !ok {
			t.Errorf("path %s missing method %s", ep.path, ep.method)
		}
	}
}

// TestSpec_AllRefsResolve validates the actual spec has no broken references.
func TestSpec_AllRefsResolve(t *testing.T) {
	data, err := os.ReadFile("../openapi.yaml")
	if err != nil {
		t.Skipf("openapi.yaml not found: %v", err)
	}

	errors := validate(data)
	if len(errors) > 0 {
		for _, e := range errors {
			t.Errorf("validation error: %s", e)
		}
	}
}

// TestSpec_HasOperationIds verifies every endpoint has a unique operationId.
func TestSpec_HasOperationIds(t *testing.T) {
	data, err := os.ReadFile("../openapi.yaml")
	if err != nil {
		t.Skipf("openapi.yaml not found: %v", err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse error: %v", err)
	}

	paths, _ := doc["paths"].(map[string]any)
	seen := make(map[string]string) // operationId → "METHOD path"

	for path, pathObj := range paths {
		methods, _ := pathObj.(map[string]any)
		for method, opObj := range methods {
			if method == "parameters" {
				continue
			}
			op, _ := opObj.(map[string]any)
			if op == nil {
				continue
			}
			opID, _ := op["operationId"].(string)
			if opID == "" {
				t.Errorf("%s %s: missing operationId", method, path)
				continue
			}
			if prev, exists := seen[opID]; exists {
				t.Errorf("duplicate operationId %q: used by %s and %s %s", opID, prev, method, path)
			}
			seen[opID] = method + " " + path
		}
	}
}
