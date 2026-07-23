// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package main provides a standalone OpenAPI spec validator.
// It parses the YAML spec and validates it has the required structure:
// - Valid OpenAPI version
// - Info section with title and version
// - At least one path defined
// - All $ref targets resolve
// - Security schemes defined
package main

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: %s <openapi.yaml>\n", os.Args[0])
		os.Exit(1)
	}

	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading file: %v\n", err)
		os.Exit(1)
	}

	errors := validate(data)
	if len(errors) > 0 {
		fmt.Fprintf(os.Stderr, "Validation failed with %d error(s):\n", len(errors))
		for _, e := range errors {
			fmt.Fprintf(os.Stderr, "  ✗ %s\n", e)
		}
		os.Exit(1)
	}

	routeErrors := validateRouteCoverage(data)
	if len(routeErrors) > 0 {
		fmt.Fprintf(os.Stderr, "Route coverage check failed with %d error(s):\n", len(routeErrors))
		for _, e := range routeErrors {
			fmt.Fprintf(os.Stderr, "  ✗ %s\n", e)
		}
		os.Exit(1)
	}
}

// validate checks the OpenAPI spec for structural correctness.
func validate(data []byte) []string {
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return []string{fmt.Sprintf("YAML parse error: %v", err)}
	}

	var errors []string

	// Check OpenAPI version
	openapi, _ := doc["openapi"].(string)
	if openapi == "" {
		errors = append(errors, "missing 'openapi' version field")
	} else if !strings.HasPrefix(openapi, "3.0") && !strings.HasPrefix(openapi, "3.1") {
		errors = append(errors, fmt.Sprintf("unsupported openapi version: %s (expected 3.0.x or 3.1.x)", openapi))
	}

	// Check info section
	info, _ := doc["info"].(map[string]any)
	if info == nil {
		errors = append(errors, "missing 'info' section")
	} else {
		if info["title"] == nil {
			errors = append(errors, "missing 'info.title'")
		}
		if info["version"] == nil {
			errors = append(errors, "missing 'info.version'")
		}
	}

	// Check paths
	paths, _ := doc["paths"].(map[string]any)
	if paths == nil || len(paths) == 0 {
		errors = append(errors, "no paths defined")
	}

	// Check components/schemas exist
	components, _ := doc["components"].(map[string]any)
	if components == nil {
		errors = append(errors, "missing 'components' section")
	} else {
		schemas, _ := components["schemas"].(map[string]any)
		if schemas == nil || len(schemas) == 0 {
			errors = append(errors, "no schemas defined in components")
		}
		secSchemes, _ := components["securitySchemes"].(map[string]any)
		if secSchemes == nil || len(secSchemes) == 0 {
			errors = append(errors, "no securitySchemes defined in components")
		}
	}

	// Validate all $ref targets resolve
	refErrors := validateRefs(doc, doc)
	errors = append(errors, refErrors...)

	return errors
}

// validateRefs recursively finds all $ref values and checks they resolve.
func validateRefs(root map[string]any, node any) []string {
	var errors []string

	switch v := node.(type) {
	case map[string]any:
		if ref, ok := v["$ref"].(string); ok {
			if !resolveRef(root, ref) {
				errors = append(errors, fmt.Sprintf("unresolved $ref: %s", ref))
			}
		}
		for _, val := range v {
			errors = append(errors, validateRefs(root, val)...)
		}
	case []any:
		for _, item := range v {
			errors = append(errors, validateRefs(root, item)...)
		}
	}

	return errors
}

// resolveRef checks if a JSON pointer reference resolves within the document.
func resolveRef(root map[string]any, ref string) bool {
	if !strings.HasPrefix(ref, "#/") {
		return true // external refs not validated here
	}

	parts := strings.Split(strings.TrimPrefix(ref, "#/"), "/")
	var current any = root
	for _, part := range parts {
		m, ok := current.(map[string]any)
		if !ok {
			return false
		}
		current, ok = m[part]
		if !ok {
			return false
		}
	}
	return true
}

// expectedPaths is the explicit list of in-scope API routes that the OpenAPI
// spec MUST document. Derived from api/internal/server/router.go. When a new
// route is added to the router, add it here too — the CI check will fail if
// you forget.
//
// Routes NOT listed here (intentionally excluded — infrastructure/internal):
//   - POST /webhooks/stripe, POST /internal/v1/pod-bootstrap,
//     GET /internal/orgs/{orgID}/status (server-to-server)
//   - GET /metrics, GET /livez, GET /readyz, GET /health (infra probes)
//   - GET /events, GET /workspaces/{id}/session-events (SSE — excluded from SDKs)
//   - GET /workspaces/{id}/terminal (WebSocket — only ticket endpoint is SDK-facing)
//   - POST /users/me/agents/reload (bulk agent reload — deferred until consumer needs it)
//   - All /admin/usage/*, /admin/billing/*, /admin/orgs/*, /admin/users/*,
//     /admin/email/*, /admin/relay/*, /admin/audit (operator surfaces, not SDK-facing)
//   - All /orgs/* (operator surface — deferred per epic out-of-scope)
var expectedPaths = []string{
	// Auth
	"/auth/config",
	"/auth/register",
	"/auth/login",
	"/auth/logout",
	"/auth/me",
	"/auth/api-keys",
	"/auth/api-keys/{id}",
	"/auth/password-reset/request",
	"/auth/password-reset/confirm",
	"/auth/verify-email",
	"/auth/verify-email/resend",
	"/auth/lookup",
	"/auth/unlock-dek",

	// Workspaces
	"/workspaces",
	"/workspaces/{id}",
	"/workspaces/{id}/status",
	"/workspaces/{id}/activate",
	"/workspaces/{id}/suspend",
	"/workspaces/{id}/restart",
	"/workspaces/{id}/refresh-compute",
	"/workspaces/{id}/agent/reload",
	"/workspaces/{id}/models",
	"/workspaces/{id}/model",
	"/workspaces/{id}/prompt",
	"/workspaces/{id}/agent-role",
	"/workspaces/{id}/effective-agent-role",

	// Sessions
	"/workspaces/{id}/sessions",
	"/workspaces/{id}/sessions/new",
	"/workspaces/{id}/sessions/active",
	"/workspaces/{id}/sessions/{sessionId}",
	"/workspaces/{id}/sessions/{sessionId}/title",
	"/workspaces/{id}/sessions/{sessionId}/seen",
	"/workspaces/{id}/sessions/{sessionId}/message",
	"/workspaces/{id}/sessions/{sessionId}/prompt",
	"/workspaces/{id}/sessions/{sessionId}/abort",
	"/workspaces/{id}/sessions/{sessionId}/queue",
	"/workspaces/{id}/sessions/{sessionId}/queue/{messageId}",

	// Agent input requests
	"/workspaces/{id}/question",
	"/workspaces/{id}/question/{requestID}/reply",
	"/workspaces/{id}/question/{requestID}/reject",
	"/workspaces/{id}/permission",
	"/workspaces/{id}/permission/{requestID}/reply",

	// Terminal
	"/workspaces/{id}/terminal/ticket",

	// Secrets
	"/secrets",
	"/secrets/audit",
	"/secrets/{id}",
	"/secrets/{id}/reveal",
	"/secrets/{id}/bindings",
	"/workspaces/{id}/bindings",
	"/workspaces/{id}/reload-secrets",
	"/workspaces/{id}/env",
	"/workspaces/{id}/env/{name}",

	// Settings
	"/admin/settings",
	"/admin/settings/schema",
	"/admin/settings/{key}",
	"/users/me/settings",
	"/users/me/settings/schema",
	"/users/me/settings/{key}",

	// Account
	"/account/rotate-key",
	"/account/change-password",
	"/account/recover",

	// Provider credentials (user)
	"/provider-credentials",
	"/provider-credentials/{id}",
	"/provider-credentials/{id}/models",
	"/provider-credentials/{id}/bindings",
	"/provider-credentials/{id}/bind/{workspaceId}",

	// Provider credentials (admin)
	"/admin/provider-credentials",
	"/admin/provider-credentials/{id}",
	"/admin/provider-credentials/{id}/models",
	"/admin/provider-credentials/{id}/auto-apply",
	"/admin/provider-credentials/{id}/auto-apply/{targetType}/{targetId}",

	// Agent roles (platform)
	"/admin/agent-roles",
	"/admin/agent-roles/{id}",

	// Platform prompt
	"/admin/prompt",

	// Usage
	"/usage",
	"/usage/workspaces/{id}",
	"/usage/quota",

	// Probe
	"/probe-models",
}

// validateRouteCoverage checks that every expected in-scope API route has a
// corresponding path entry in the OpenAPI spec.
func validateRouteCoverage(data []byte) []string {
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return []string{"YAML parse error during route coverage check"}
	}

	paths, _ := doc["paths"].(map[string]any)

	var errors []string
	for _, expected := range expectedPaths {
		if _, ok := paths[expected]; !ok {
			errors = append(errors, fmt.Sprintf("expected path %q not found in spec (add it or update expectedPaths in validate/main.go)", expected))
		}
	}

	return errors
}
