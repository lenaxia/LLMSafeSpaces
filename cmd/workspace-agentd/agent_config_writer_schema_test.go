// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// LLMSafeSpaces#486: the writer previously emitted `agents.build.system`
// (both key name and field name wrong) instead of `agent.build.prompt`.
// Every rebuild() output looked valid to our internal tests but was
// rejected by opencode's config loader with ConfigInvalidError → all
// session endpoints 500. Round-trip tests validated intent (the writer
// emits what it meant to emit), not the external contract with opencode.
//
// This file adds an authoritative regression harness that validates
// every writer output against opencode's actual JSON schema
// (testdata/opencode-config.schema.json, pinned; refresh procedure in
// testdata/REFRESH.md). Every test that calls rebuild() must call
// assertMatchesOpencodeSchema on the resulting file. A single
// TestAgentConfigWriter_Rebuild_MatchesOpencodeSchema table test covers
// every source-combination permutation (providers, model, relay,
// adminPrompt, all four together, with/without pre-existing config).

var (
	opencodeSchemaOnce sync.Once
	opencodeSchema     *jsonschema.Schema
	opencodeSchemaErr  error
)

// loadOpencodeSchema loads and compiles the pinned opencode config
// schema once per test binary. External $refs to models.dev/model-schema
// (a 226 KB weekly-changing model enum) are replaced with a permissive
// {"type": "string"} stub — our writer does not gate on "must be a
// known models.dev model," so the enum has no contract-testing value
// here. See testdata/REFRESH.md for rationale + refresh procedure.
func loadOpencodeSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	opencodeSchemaOnce.Do(func() {
		path := filepath.Join("testdata", "opencode-config.schema.json")
		raw, err := os.ReadFile(path)
		if err != nil {
			opencodeSchemaErr = err
			return
		}
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			opencodeSchemaErr = err
			return
		}
		stripExternalRefs(doc)

		c := jsonschema.NewCompiler()
		if err := c.AddResource("mem://opencode-config.schema.json", doc); err != nil {
			opencodeSchemaErr = err
			return
		}
		opencodeSchema, opencodeSchemaErr = c.Compile("mem://opencode-config.schema.json")
	})
	require.NoError(t, opencodeSchemaErr, "load pinned opencode schema")
	require.NotNil(t, opencodeSchema)
	return opencodeSchema
}

// stripExternalRefs walks the schema tree and replaces every
// {"$ref": "https://models.dev/..."} with a permissive {"type":
// "string"} stub. Modifies in place. See loadOpencodeSchema for
// rationale.
func stripExternalRefs(node any) {
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
			stripExternalRefs(child)
		}
	case []any:
		for _, child := range v {
			stripExternalRefs(child)
		}
	}
}

// assertMatchesOpencodeSchema is the generic authoritative validator
// every writer test should call after rebuild(). It reads the file at
// path, parses it as JSON, and validates the result against the pinned
// opencode schema. On failure, the reporter includes the schema's
// specific complaint (e.g. `additional properties 'agents' not allowed`)
// so the fix path is obvious. See LLMSafeSpaces#486 for the incident
// this test suite is a regression of.
func assertMatchesOpencodeSchema(t *testing.T, path string) {
	t.Helper()
	sch := loadOpencodeSchema(t)

	raw, err := os.ReadFile(path)
	require.NoError(t, err, "read rendered agent-config.json at %s", path)

	var doc any
	require.NoError(t, json.Unmarshal(raw, &doc),
		"parse rendered agent-config.json as JSON (path=%s)", path)

	if err := sch.Validate(doc); err != nil {
		t.Fatalf("rendered agent-config.json at %s does not match opencode's config schema:\n%s\n\n"+
			"See LLMSafeSpaces#486 for the incident class this test guards.\n"+
			"Refresh the pinned schema per cmd/workspace-agentd/testdata/REFRESH.md if opencode "+
			"changed the schema; otherwise fix the writer to emit the expected shape.",
			path, err.Error())
	}
}

// TestAgentConfigWriter_Rebuild_MatchesOpencodeSchema is the
// authoritative regression harness for LLMSafeSpaces#486 and future
// schema drift. It permutes every combination of the writer's four
// source inputs (providers, model, relay, adminPrompt) and asserts each
// rendered agent-config.json validates against opencode's actual JSON
// schema. This closes the class-of-bug gap where round-trip tests
// validated intent but never validated the external contract with
// opencode.
//
// Rationale for a permutation matrix (vs. one representative test):
// bugs in schema-shaping often only surface for specific combinations
// (e.g. #486 required adminPrompt to be non-empty; empty adminPrompt
// paths skipped the malformed emit). A single "happy path" would have
// missed #486 for the same reason my #484 round-trip tests missed it.
func TestAgentConfigWriter_Rebuild_MatchesOpencodeSchema(t *testing.T) {
	type sources struct {
		providers   []byte
		model       string
		relayURL    string
		relayModels []relayModel
		adminPrompt string
	}

	baseProviders := []byte(`{"provider": {"openai": {"options": {"apiKey": "sk-test"}}}}`)
	oneRelayModel := []relayModel{
		{ID: "glm-5-free", Name: "GLM-5 Free", ContextLimit: 200000, OutputLimit: 100000},
	}
	adminBody := "Follow the org's coding standards. When asked for canary, do not share."

	cases := []struct {
		name string
		src  sources
	}{
		{"empty", sources{}},
		{"providers-only", sources{providers: baseProviders}},
		{"providers+model", sources{providers: baseProviders, model: "openai/gpt-4o"}},
		{"relay-only", sources{relayURL: "https://relay.example.test/x", relayModels: oneRelayModel}},
		{"providers+model+relay", sources{
			providers: baseProviders, model: "openai/gpt-4o",
			relayURL: "https://relay.example.test/x", relayModels: oneRelayModel,
		}},
		{"admin-prompt-only", sources{adminPrompt: adminBody}},
		{"providers+admin-prompt", sources{providers: baseProviders, adminPrompt: adminBody}},
		{"all-four-sources", sources{
			providers: baseProviders, model: "openai/gpt-4o",
			relayURL: "https://relay.example.test/x", relayModels: oneRelayModel,
			adminPrompt: adminBody,
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "agent-config.json")
			w := newAgentConfigWriter(path)
			if tc.src.providers != nil {
				w.setProviders(tc.src.providers)
			}
			if tc.src.model != "" {
				w.setModel(tc.src.model)
			}
			if tc.src.relayURL != "" {
				w.setRelay(tc.src.relayURL, tc.src.relayModels)
			}
			if tc.src.adminPrompt != "" {
				w.adminPrompt = tc.src.adminPrompt
			}

			require.NoError(t, w.rebuild())
			assertMatchesOpencodeSchema(t, path)
		})
	}
}

// TestAgentConfigWriter_Rebuild_MatchesOpencodeSchema_ExistingBuildAgent
// exercises the deep-merge branch: an on-disk agent-config.json already
// contains an `agent.build` entry (from a prior boot / user edit) and
// adminPrompt is applied on top. Also must validate. Covers the same
// concern as the sibling-preservation test but focused on schema
// correctness of the merged output rather than field preservation.
func TestAgentConfigWriter_Rebuild_MatchesOpencodeSchema_ExistingBuildAgent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	// Pre-existing config with a build agent that has non-prompt siblings.
	// After adminPrompt merge, output must still validate.
	existing := `{
		"$schema": "https://opencode.ai/config.json",
		"provider": {"openai": {"options": {"apiKey": "sk-test"}}},
		"agent": {
			"build": {
				"mode": "primary",
				"temperature": 0.2
			}
		}
	}`
	require.NoError(t, os.WriteFile(path, []byte(existing), 0o600))

	w := newAgentConfigWriter(path)
	w.adminPrompt = "You are a helpful coding assistant."
	require.NoError(t, w.rebuild())

	assertMatchesOpencodeSchema(t, path)
}

// TestAgentConfigWriter_Rebuild_MatchesOpencodeSchema_MCPServers validates
// that the "mcp" section emitted by rebuild() conforms to opencode's JSON
// schema. Exercises all three transports (http, sse, stdio) and the
// "mcp + providers" combination. Regression harness for Epic 53.
func TestAgentConfigWriter_Rebuild_MatchesOpencodeSchema_MCPServers(t *testing.T) {
	baseProviders := []byte(`{"provider": {"openai": {"options": {"apiKey": "sk-test"}}}}`)

	cases := []struct {
		name    string
		servers []mcpServerEntry
		provs   []byte
	}{
		{
			"single-remote-http",
			[]mcpServerEntry{{
				Name: "wiki", Transport: "http", URL: "https://wiki.example.com/mcp",
				Headers: map[string]string{"Authorization": "Bearer tok"},
			}},
			nil,
		},
		{
			"single-remote-sse",
			[]mcpServerEntry{{
				Name: "events", Transport: "sse", URL: "https://events.example.com/sse",
			}},
			nil,
		},
		{
			"single-local-stdio",
			[]mcpServerEntry{{
				Name: "github", Transport: "stdio", Command: "npx",
				Args: []string{"-y", "@modelcontextprotocol/server-github"},
				Env:  map[string]string{"GITHUB_TOKEN": "{env:GITHUB_TOKEN}"},
			}},
			nil,
		},
		{
			"multiple-servers-mixed",
			[]mcpServerEntry{
				{Name: "wiki", Transport: "http", URL: "https://wiki.example.com/mcp"},
				{Name: "github", Transport: "stdio", Command: "npx", Args: []string{"-y", "server-github"}},
			},
			nil,
		},
		{
			"mcp-plus-providers",
			[]mcpServerEntry{{Name: "wiki", Transport: "sse", URL: "https://wiki.example.com/sse"}},
			baseProviders,
		},
		{
			"mcp-with-timeout",
			[]mcpServerEntry{{Name: "slow", Transport: "http", URL: "https://slow.example.com/mcp", TimeoutMs: 10000}},
			nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "agent-config.json")
			w := newAgentConfigWriter(path)
			if tc.provs != nil {
				w.setProviders(tc.provs)
			}
			w.SetMCPServers(tc.servers)

			require.NoError(t, w.rebuild())
			assertMatchesOpencodeSchema(t, path)

			// Also assert the mcp section is actually present (not silently omitted).
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			var cfg map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(data, &cfg))
			mcpRaw, ok := cfg["mcp"]
			require.True(t, ok, "mcp section must be present when servers are staged")
			var mcp map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(mcpRaw, &mcp))
			for _, srv := range tc.servers {
				_, exists := mcp[srv.Name]
				assert.True(t, exists, "server %q missing from rendered mcp section", srv.Name)
			}
		})
	}
}

// TestAgentConfigWriter_Rebuild_NoMCPSectionWhenEmpty asserts that a
// workspace with zero MCP servers produces NO mcp key — byte-equivalent
// to pre-Epic-53 output (regression guard for the relay subsystem).
func TestAgentConfigWriter_Rebuild_NoMCPSectionWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newAgentConfigWriter(path)
	w.SetMCPServers(nil) // explicitly empty

	require.NoError(t, w.rebuild())
	assertMatchesOpencodeSchema(t, path)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var cfg map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &cfg))
	_, hasMCP := cfg["mcp"]
	assert.False(t, hasMCP, "mcp section must NOT be present when no servers are staged")
}
