// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestApplyMCPServer_RemoteHTTP stages a remote HTTP MCP server and asserts
// the staged entry has the correct opencode "remote" shape.
func TestApplyMCPServer_RemoteHTTP(t *testing.T) {
	m := &Materializer{FS: RealFS(), Paths: DefaultPaths(os.Getenv("HOME"))}
	s := Secret{
		Type: "mcp-server",
		Name: "wiki",
		Metadata: map[string]string{
			"transport": "http",
			"url":       "https://wiki.example.com/mcp",
		},
		Plaintext: `{"headers":{"Authorization":"Bearer tok"}}`,
	}

	require.NoError(t, m.applyMCPServer(s))

	staged := m.StagedMCPServers()
	require.Len(t, staged, 1)
	assert.Equal(t, "wiki", staged[0].Name)
	assert.Equal(t, "http", staged[0].Transport)
	assert.Equal(t, "https://wiki.example.com/mcp", staged[0].URL)
	assert.Equal(t, "Bearer tok", staged[0].Headers["Authorization"])
}

// TestApplyMCPServer_LocalStdio stages a stdio MCP server with env + args.
func TestApplyMCPServer_LocalStdio(t *testing.T) {
	m := &Materializer{FS: RealFS(), Paths: DefaultPaths(os.Getenv("HOME"))}
	s := Secret{
		Type: "mcp-server",
		Name: "github",
		Metadata: map[string]string{
			"transport": "stdio",
			"command":   "npx",
			"args":      `["-y","@modelcontextprotocol/server-github"]`,
		},
		Plaintext: `{"env":{"GITHUB_TOKEN":"ghp_xxx"}}`,
	}

	require.NoError(t, m.applyMCPServer(s))

	staged := m.StagedMCPServers()
	require.Len(t, staged, 1)
	assert.Equal(t, "github", staged[0].Name)
	assert.Equal(t, "stdio", staged[0].Transport)
	assert.Equal(t, "npx", staged[0].Command)
	assert.Equal(t, []string{"-y", "@modelcontextprotocol/server-github"}, staged[0].Args)
	assert.Equal(t, "ghp_xxx", staged[0].Env["GITHUB_TOKEN"])
}

// TestApplyMCPServer_SecretReference stages an MCP server whose env value is
// a {env:VAR} reference (not a literal secret). The reference passes through
// as a plain string — opencode resolves it at runtime.
func TestApplyMCPServer_SecretReference(t *testing.T) {
	m := &Materializer{FS: RealFS(), Paths: DefaultPaths(os.Getenv("HOME"))}
	s := Secret{
		Type: "mcp-server",
		Name: "github",
		Metadata: map[string]string{
			"transport": "stdio",
			"command":   "npx",
		},
		Plaintext: `{"env":{"GITHUB_TOKEN":"{env:GITHUB_TOKEN}"}}`,
	}

	require.NoError(t, m.applyMCPServer(s))

	staged := m.StagedMCPServers()
	require.Len(t, staged, 1)
	// The reference string passes through unchanged — it's not a secret value.
	assert.Equal(t, "{env:GITHUB_TOKEN}", staged[0].Env["GITHUB_TOKEN"])
}

// TestApplyMCPServer_NoSecrets stages an MCP server with an empty plaintext
// (no env, no headers). Must succeed — not every MCP server has secrets.
func TestApplyMCPServer_NoSecrets(t *testing.T) {
	m := &Materializer{FS: RealFS(), Paths: DefaultPaths(os.Getenv("HOME"))}
	s := Secret{
		Type: "mcp-server",
		Name: "public-api",
		Metadata: map[string]string{
			"transport": "sse",
			"url":       "https://public.example.com/sse",
		},
		Plaintext: "",
	}

	require.NoError(t, m.applyMCPServer(s))
	staged := m.StagedMCPServers()
	require.Len(t, staged, 1)
	assert.Nil(t, staged[0].Env)
	assert.Nil(t, staged[0].Headers)
}

// TestApplyMCPServer_InvalidTransport rejects a server with no transport.
func TestApplyMCPServer_InvalidTransport(t *testing.T) {
	m := &Materializer{FS: RealFS(), Paths: DefaultPaths(os.Getenv("HOME"))}
	s := Secret{
		Type:     "mcp-server",
		Name:     "bad",
		Metadata: map[string]string{}, // no transport
	}

	err := m.applyMCPServer(s)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "transport")
}

// TestMaterialize_IncludesMCP servers verifies the full Materialize loop
// handles mcp-server entries alongside other secret types without conflict.
func TestMaterialize_IncludesMCPServers(t *testing.T) {
	dir := t.TempDir()
	paths := Paths{
		Home:            dir,
		SecretsBaseDir:  filepath.Join(dir, "secrets"),
		SSHDir:          filepath.Join(dir, "ssh"),
		AgentConfigPath: filepath.Join(dir, "agent-config.json"),
		SecretsEnvPath:  filepath.Join(dir, "secrets-env"),
		GitCredsPath:    filepath.Join(dir, "git-credentials"),
	}
	m := &Materializer{FS: RealFS(), Paths: paths}

	secrets := []Secret{
		{
			Type:      "mcp-server",
			Name:      "wiki",
			Metadata:  map[string]string{"transport": "http", "url": "https://wiki.example.com/mcp"},
			Plaintext: `{"headers":{"Authorization":"Bearer tok"}}`,
		},
		{
			Type:      "mcp-server",
			Name:      "github",
			Metadata:  map[string]string{"transport": "stdio", "command": "npx", "args": `["server-github"]`},
			Plaintext: `{}`,
		},
	}

	result, err := m.Materialize(secrets)
	require.NoError(t, err)
	assert.False(t, result.HasFailures(), "materialize should not have failures")

	staged := m.StagedMCPServers()
	require.Len(t, staged, 2)
	assert.Equal(t, "wiki", staged[0].Name)
	assert.Equal(t, "github", staged[1].Name)
}

// TestMaterialize_MCPDisabledServerOmitted verifies that the applyOne dispatch
// handles the mcp-server type without crashing for the full MaterializeResult
// outcome tracking.
func TestMaterialize_MCPOutcomeTracked(t *testing.T) {
	dir := t.TempDir()
	paths := Paths{
		Home:            dir,
		SecretsBaseDir:  filepath.Join(dir, "secrets"),
		SSHDir:          filepath.Join(dir, "ssh"),
		AgentConfigPath: filepath.Join(dir, "agent-config.json"),
		SecretsEnvPath:  filepath.Join(dir, "secrets-env"),
		GitCredsPath:    filepath.Join(dir, "git-credentials"),
	}
	m := &Materializer{FS: RealFS(), Paths: paths}

	secrets := []Secret{
		{
			Type:      "mcp-server",
			Name:      "valid",
			Metadata:  map[string]string{"transport": "http", "url": "https://x.com"},
			Plaintext: `{}`,
		},
		{
			Type:      "mcp-server",
			Name:      "invalid",
			Metadata:  map[string]string{}, // missing transport → skipped
			Plaintext: `{}`,
		},
	}

	result, err := m.Materialize(secrets)
	require.NoError(t, err) // partial failure does not return error from Materialize

	mat, skip, fail := result.Counts()
	assert.Equal(t, 1, mat, "one materialized")
	assert.Equal(t, 1, skip, "one skipped (missing transport)")
	assert.Equal(t, 0, fail, "zero failed")
}
