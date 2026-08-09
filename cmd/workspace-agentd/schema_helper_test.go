// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/require"
)

// schema_helper_test.go provides assertMatchesOpencodeSchema for agentd
// integration tests that write agent-config.json via paths other than the
// ConfigWriter (e.g. applyMCPServersToConfig in the materialize subcommand).
//
// The pinned schema and the authoritative validator live in
// pkg/agent/opencode/ (configwriter_test.go + testdata/). This helper
// references that testdata via a relative path so agentd tests validate
// against the SAME pinned schema without duplicating it. If the opencode
// package moves, update this path.

var (
	agentdOpencodeSchemaOnce sync.Once
	agentdOpencodeSchema     *jsonschema.Schema
	agentdOpencodeSchemaErr  error
)

func loadOpencodeSchemaForAgentd(t *testing.T) *jsonschema.Schema {
	t.Helper()
	agentdOpencodeSchemaOnce.Do(func() {
		path := filepath.Join("..", "..", "pkg", "agent", "opencode", "testdata", "opencode-config.schema.json")
		raw, err := os.ReadFile(path)
		if err != nil {
			agentdOpencodeSchemaErr = fmt.Errorf("read pinned schema (looked at %s): %w", path, err)
			return
		}
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			agentdOpencodeSchemaErr = err
			return
		}
		stripExternalRefsAgentd(doc)

		c := jsonschema.NewCompiler()
		if err := c.AddResource("mem://opencode-config.schema.json", doc); err != nil {
			agentdOpencodeSchemaErr = err
			return
		}
		agentdOpencodeSchema, agentdOpencodeSchemaErr = c.Compile("mem://opencode-config.schema.json")
	})
	require.NoError(t, agentdOpencodeSchemaErr, "load pinned opencode schema")
	require.NotNil(t, agentdOpencodeSchema)
	return agentdOpencodeSchema
}

func stripExternalRefsAgentd(node any) {
	switch v := node.(type) {
	case map[string]any:
		if ref, ok := v["$ref"].(string); ok && strings.HasPrefix(ref, "https://models.dev/") {
			for k := range v {
				delete(v, k)
			}
			v["type"] = "string"
			return
		}
		for _, child := range v {
			stripExternalRefsAgentd(child)
		}
	case []any:
		for _, child := range v {
			stripExternalRefsAgentd(child)
		}
	}
}

func assertMatchesOpencodeSchema(t *testing.T, path string) {
	t.Helper()
	sch := loadOpencodeSchemaForAgentd(t)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var doc any
	require.NoError(t, json.Unmarshal(raw, &doc))
	if err := sch.Validate(doc); err != nil {
		t.Fatalf("agent-config.json at %s does not match opencode's config schema:\n%s\n"+
			"Refresh the pinned schema per pkg/agent/opencode/testdata/REFRESH.md if opencode "+
			"changed the schema.", path, err.Error())
	}
}
