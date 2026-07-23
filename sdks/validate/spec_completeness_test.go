// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestSpec_Completeness verifies the actual openapi.yaml has all expected endpoints.
// Derives the expected path set from the canonical expectedPaths list in main.go
// (single source of truth) — no duplicate list here.
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

	// Use the canonical expectedPaths from main.go — single source of truth.
	for _, expected := range expectedPaths {
		if _, ok := paths[expected]; !ok {
			t.Errorf("missing path: %s", expected)
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
