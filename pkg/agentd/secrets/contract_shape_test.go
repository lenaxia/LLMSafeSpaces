// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression (2026-08-15, v0.15.6): the injection pipeline writes
// mcp-server metadata per MATERIALIZE-CONTRACT.md with NATIVE JSON types
// ("args": [...], "timeoutMs": 5000), but Secret.Metadata was
// map[string]string — the whole-file unmarshal aborted with
// "cannot unmarshal array into Go struct field Secret.metadata of type
// string", and since LoadSecretsFile fails the entire batch, ONE bound
// MCP server crash-looped workspace boot (Init:Error) and took every
// other secret down with it. The materializer must accept the contract
// shape: complex metadata values are carried JSON-encoded as strings.
func TestLoadSecretsFile_ContractShapedMCPServer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.json")
	doc := `[
		{"type":"llm-provider","name":"relay","metadata":{"api_base":"https://relay.example"},"plaintext":"k"},
		{"type":"mcp-server","name":"opengist","metadata":{"transport":"http","url":"https://mcp.example/abc","command":"","args":[],"timeoutMs":5000},"plaintext":"{\"env\":{},\"headers\":{}}"},
		{"type":"mcp-server","name":"github-tools","metadata":{"transport":"stdio","url":"","command":"npx","args":["-y","@modelcontextprotocol/server-github"],"timeoutMs":5000},"plaintext":"{\"env\":{},\"headers\":{}}"}
	]`
	require.NoError(t, os.WriteFile(path, []byte(doc), 0o600))

	got, err := LoadSecretsFile(path)
	require.NoError(t, err, "contract-shaped mcp-server metadata must parse")
	require.Len(t, got, 3)

	assert.Equal(t, "opengist", got[1].Name)
	assert.Equal(t, "https://mcp.example/abc", got[1].Metadata["url"])
	assert.Equal(t, "http", got[1].Metadata["transport"])
	assert.Equal(t, "5000", got[1].Metadata["timeoutMs"], "numeric metadata values stringify")
	assert.Equal(t, "[]", got[1].Metadata["args"], "array metadata values JSON-encode")

	var args []string
	require.NoError(t, json.Unmarshal([]byte(got[2].Metadata["args"]), &args))
	assert.Equal(t, []string{"-y", "@modelcontextprotocol/server-github"}, args,
		"the mcp staging branch's json.Unmarshal(argsStr) must receive valid JSON")
}

// One malformed entry must not take down the whole batch: LoadSecretsFile
// skips undecodable entries with the failure recorded on the entry
// (parse-tolerant, additive composition — same policy as the injection
// pipeline's per-server decrypt skip, D4).
func TestLoadSecretsFile_MalformedEntrySkippedNotFatal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.json")
	doc := `[
		{"type":"llm-provider","name":"good","metadata":{"api_base":"https://x"},"plaintext":"k"},
		{"type":"mcp-server","name":"bad","metadata":"not-an-object","plaintext":"{}"},
		{"type":"env-secret","name":"also-good","metadata":{"var_name":"FOO"},"plaintext":"v"}
	]`
	require.NoError(t, os.WriteFile(path, []byte(doc), 0o600))

	got, err := LoadSecretsFile(path)
	require.NoError(t, err, "a single malformed entry must not fail the file")
	require.Len(t, got, 3, "entry retained so Materialize can record the failure")

	// The malformed entry reports its reason through the materializer,
	// while the healthy entries materialize.
	m := &Materializer{FS: RealFS(), Paths: Paths{
		Home:            "/home/sandbox",
		SecretsBaseDir:  filepath.Join(dir, "rt", "secrets"),
		SSHDir:          filepath.Join(dir, "rt", "ssh"),
		AgentConfigPath: filepath.Join(dir, "agent-config.json"),
		SecretsEnvPath:  filepath.Join(dir, "secrets-env"),
		GitCredsPath:    filepath.Join(dir, "git-credentials"),
	}}
	res, err := m.Materialize(got)
	// A failed entry yields the partial-failure sentinel error; the
	// per-entry Results still say exactly which entry failed and which
	// succeeded (the caller logs results and continues boot).
	require.Error(t, err)
	assert.Contains(t, err.Error(), "partial failures")
	byName := map[string]SecretResult{}
	for _, r := range res.Results {
		byName[r.Name] = r
	}
	assert.Equal(t, OutcomeFailed, byName["bad"].Outcome)
	assert.Contains(t, byName["bad"].Reason, "not a JSON object")
	assert.Equal(t, OutcomeMaterialized, byName["also-good"].Outcome)
}

// Legacy shape (all-string metadata) must keep parsing byte-identically.
func TestLoadSecretsFile_LegacyStringMetadata(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.json")
	doc := `[{"type":"ssh-key","name":"deploy","metadata":{"host":"github.com"},"plaintext":"pk"}]`
	require.NoError(t, os.WriteFile(path, []byte(doc), 0o600))

	got, err := LoadSecretsFile(path)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "github.com", got[0].Metadata["host"])
}
