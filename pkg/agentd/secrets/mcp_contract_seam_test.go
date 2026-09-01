// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sec "github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// This file pins the Epic 53 wire contract at the seam that broke on
// 2026-08-15: the injection pipeline (pkg/secrets) writes mcp-server
// metadata with native JSON types (args array, timeoutMs number) per
// MATERIALIZE-CONTRACT.md, while this package's Secret.Metadata was
// map[string]string — one bound MCP server aborted the whole-file parse
// and crash-looped workspace boot. The per-side unit tests hardcode
// their own shapes; if either drifts independently they stay green and
// only this seam test fails, which is exactly the incident's failure
// mode. It lives in this package (not pkg/secrets) because this package
// already imports pkg/secrets — the reverse would be a cycle.

// xorRootProvider is a reversible sec.RootKeyProvider for the test:
// XOR is not cryptography, but loadMCPServers only needs
// Encrypt/Decrypt to round-trip the admin ciphertext.
type xorRootProvider struct{ mask byte }

func (x *xorRootProvider) Encrypt(_ context.Context, plaintext []byte) ([]byte, error) {
	out := make([]byte, len(plaintext))
	for i, b := range plaintext {
		out[i] = b ^ x.mask
	}
	return out, nil
}

func (x *xorRootProvider) Decrypt(ctx context.Context, ciphertext []byte) ([]byte, error) {
	return x.Encrypt(ctx, ciphertext)
}

// seamStore implements the SecretStore + CredentialStore surface with
// panic stubs (drift surfaces loudly); the two methods the sessionless
// injection path touches return the contract payload.
type seamStore struct {
	servers []sec.MCPServerBindingRow
	queried bool
}

func (s *seamStore) GetWorkspaceMCPServers(_ context.Context, _ string) ([]sec.MCPServerBindingRow, error) {
	s.queried = true
	return s.servers, nil
}
func (s *seamStore) GetWorkspaceCredentials(_ context.Context, _ string) ([]sec.CredentialBinding, error) {
	return nil, nil
}
func (s *seamStore) GetBindings(_ context.Context, _ string) ([]*sec.UserSecret, error) {
	return nil, nil
}
func (s *seamStore) GetBindingsForSecret(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}
func (s *seamStore) ListGlobalDefaultSecrets(_ context.Context, _ string) ([]*sec.UserSecret, error) {
	return nil, nil
}
func (s *seamStore) LogAudit(_ context.Context, _ *sec.AuditEntry) error { return nil }
func (s *seamStore) QueryAudit(_ context.Context, _ string, _ sec.AuditQuery) ([]*sec.AuditEntry, error) {
	return nil, nil
}
func (s *seamStore) UpsertFreeTierCredential(_ context.Context, _ []byte) error { return nil }
func (s *seamStore) SeedWorkspaceCredentials(_ context.Context, _, _ string, _ *string) error {
	return nil
}
func (s *seamStore) BindCredentialToAllUserWorkspaces(_ context.Context, _, _ string) error {
	return nil
}
func (s *seamStore) HasUserProviderCredential(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}
func (s *seamStore) CreateSecret(_ context.Context, _ *sec.UserSecret) error { panic("unused") }
func (s *seamStore) GetSecret(_ context.Context, _, _ string) (*sec.UserSecret, error) {
	panic("unused")
}
func (s *seamStore) GetSecretByName(_ context.Context, _, _ string) (*sec.UserSecret, error) {
	panic("unused")
}
func (s *seamStore) ListSecrets(_ context.Context, _ string) ([]*sec.UserSecret, error) {
	panic("unused")
}
func (s *seamStore) UpdateSecret(_ context.Context, _ *sec.UserSecret) error { panic("unused") }
func (s *seamStore) DeleteSecret(_ context.Context, _, _ string) error       { panic("unused") }
func (s *seamStore) ReEncryptUserSecrets(_ context.Context, _ string, _ int, _ func([]byte) ([]byte, error), _ func(context.Context) error) error {
	panic("unused")
}
func (s *seamStore) SetBindings(_ context.Context, _ string, _ []string) error { panic("unused") }
func (s *seamStore) AddBindings(_ context.Context, _ string, _ []string) error { panic("unused") }

func (s *seamStore) CurrentRevision(context.Context, string) (int64, string, bool, error) {
	return 0, "", false, nil
}
func (s *seamStore) EnsureRevision(context.Context, string, string) (int64, error) {
	return 1, nil
}

var _ sec.SecretStore = (*seamStore)(nil)
var _ sec.CredentialStore = (*seamStore)(nil)
var _ sec.RevisionStore = (*seamStore)(nil)

func TestMCPInjection_MaterializerSeam_ContractShape(t *testing.T) {
	secretPayload := `{"env":{"GITHUB_TOKEN":"ghp_x"},"headers":{"X-A":"1"}}`
	root := &xorRootProvider{mask: 0x5A}
	ciphertext, err := root.Encrypt(context.Background(), []byte(secretPayload))
	require.NoError(t, err)

	store := &seamStore{servers: []sec.MCPServerBindingRow{{
		ServerID:   "srv-1",
		OwnerType:  "admin",
		Name:       "github-tools",
		Transport:  "stdio",
		Command:    "npx",
		Args:       []string{"-y", "@modelcontextprotocol/server-github"},
		TimeoutMs:  intPtr(5000),
		Ciphertext: ciphertext,
		Version:    1,
		Enabled:    true,
	}}}

	svc := sec.NewSecretService(nil, store)
	svc.SetAdminProvider(root)

	batch, degrade, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	require.Nil(t, degrade)
	out := sec.LegacyBatchJSON(*batch)
	require.NoError(t, err)
	require.True(t, store.queried, "GetWorkspaceMCPServers must be reached sessionless")
	require.NotEmpty(t, out, "one admin-scope MCP server must be delivered sessionless")

	// Writer-side assertions: the injected entry carries the contract
	// shape natively (this is what MATERIALIZE-CONTRACT.md promises).
	var probe []map[string]any
	require.NoError(t, json.Unmarshal(out, &probe))
	found := false
	for _, e := range probe {
		if e["type"] == "mcp-server" && e["name"] == "github-tools" {
			found = true
			meta, ok := e["metadata"].(map[string]any)
			require.True(t, ok, "injection must emit metadata as a JSON object (contract)")
			assert.Equal(t, "stdio", meta["transport"])
			assert.Equal(t, "npx", meta["command"])
			assert.Equal(t, []any{"-y", "@modelcontextprotocol/server-github"}, meta["args"],
				"args must be a native JSON array per MATERIALIZE-CONTRACT.md")
			assert.Equal(t, float64(5000), meta["timeoutMs"],
				"timeoutMs must be a native JSON number per the contract")
			assert.Contains(t, e["plaintext"], "ghp_x", "decrypted secret payload travels in plaintext")
		}
	}
	assert.True(t, found, "mcp-server entry present in injected output")

	// Reader side: the real LoadSecretsFile the init container runs must
	// accept the injected bytes and stage the server with its config.
	dir := t.TempDir()
	secretsPath := filepath.Join(dir, "secrets.json")
	require.NoError(t, os.WriteFile(secretsPath, out, 0o600))

	parsed, err := LoadSecretsFile(secretsPath)
	require.NoError(t, err)
	staged := false
	for _, s := range parsed {
		if s.Type == "mcp-server" && s.Name == "github-tools" {
			staged = true
			var args []string
			require.NoError(t, json.Unmarshal([]byte(s.Metadata["args"]), &args))
			assert.Equal(t, []string{"-y", "@modelcontextprotocol/server-github"}, args)
			assert.Equal(t, "5000", s.Metadata["timeoutMs"])
			assert.Empty(t, s.MetadataInvalid)
		}
	}
	assert.True(t, staged, "materializer parsed and staged the contract-shaped server")
}

func intPtr(n int) *int { return &n }
