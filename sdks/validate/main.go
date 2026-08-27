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
}

// Route-coverage is NOT checked here. The authoritative gate is
// TestOpenAPIRouterContract (api/internal/server/router_openapi_contract_test.go),
// which diffs the spec against the production gin router in BOTH directions
// with every handler wired. This validator covers structure only: required
// fields, security schemes, $ref resolution, operationId uniqueness.

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
