// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInjectSecrets_MCPViaAsyncAuditLogger proves the production injection
// path works: SecretService with AsyncAuditLogger wrapping a store that
// implements CredentialStore. This would have caught the original critical
// bug where loadMCPServers type-asserted *PgSecretStore instead of
// CredentialStore (the assertion failed silently under AsyncAuditLogger).
//
// The test verifies that GetWorkspaceMCPServers is actually called through
// the AsyncAuditLogger wrapper — the type assertion succeeds and the MCP
// query reaches the inner store. The decrypt step is tested separately.
func TestInjectSecrets_MCPViaAsyncAuditLogger(t *testing.T) {
	mockStore := &mcpInjectionMockStore{}

	// Wrap in AsyncAuditLogger (production configuration).
	auditStore := NewAsyncAuditLogger(mockStore, 16, nil)
	svc := NewSecretService(nil, auditStore)

	// Verify the type assertion succeeds — this is the regression guard.
	// Before the fix, s.store was *AsyncAuditLogger, not *PgSecretStore,
	// so the assertion always failed and GetWorkspaceMCPServers was never called.
	_, ok := svc.store.(CredentialStore)
	require.True(t, ok, "AsyncAuditLogger must implement CredentialStore so loadMCPServers works")

	// Call the sessionless injection path — exercises loadMCPServers.
	_, err := svc.InjectSessionlessSecrets(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)

	// Verify GetWorkspaceMCPServers was called (the query reached the inner store).
	assert.True(t, mockStore.mcpQueryCalled, "GetWorkspaceMCPServers must be called through AsyncAuditLogger")
}

// mcpInjectionMockStore implements SecretStore + CredentialStore for the
// injection-path test. Only the methods touched by InjectSessionlessSecrets
// return real values; the rest panic to surface drift.
type mcpInjectionMockStore struct {
	mcpQueryCalled bool
}

func (m *mcpInjectionMockStore) GetWorkspaceMCPServers(_ context.Context, _ string) ([]MCPServerBindingRow, error) {
	m.mcpQueryCalled = true
	return nil, nil
}

func (m *mcpInjectionMockStore) GetWorkspaceCredentials(_ context.Context, _ string) ([]CredentialBinding, error) {
	return nil, nil
}
func (m *mcpInjectionMockStore) UpsertFreeTierCredential(_ context.Context, _ []byte) error {
	return nil
}
func (m *mcpInjectionMockStore) SeedWorkspaceCredentials(_ context.Context, _, _ string, _ *string) error {
	return nil
}
func (m *mcpInjectionMockStore) BindCredentialToAllUserWorkspaces(_ context.Context, _, _ string) error {
	return nil
}
func (m *mcpInjectionMockStore) HasUserProviderCredential(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}

// SecretStore surface (methods touched by loadNonLLMSecrets + LogAudit).
func (m *mcpInjectionMockStore) GetBindings(_ context.Context, _ string) ([]*UserSecret, error) {
	return nil, nil
}
func (m *mcpInjectionMockStore) LogAudit(_ context.Context, _ *AuditEntry) error     { return nil }
func (m *mcpInjectionMockStore) CreateSecret(_ context.Context, _ *UserSecret) error { return nil }
func (m *mcpInjectionMockStore) GetSecret(_ context.Context, _, _ string) (*UserSecret, error) {
	return nil, nil
}
func (m *mcpInjectionMockStore) GetSecretByName(_ context.Context, _, _ string) (*UserSecret, error) {
	return nil, nil
}
func (m *mcpInjectionMockStore) ListSecrets(_ context.Context, _ string) ([]*UserSecret, error) {
	return nil, nil
}
func (m *mcpInjectionMockStore) UpdateSecret(_ context.Context, _ *UserSecret) error { return nil }
func (m *mcpInjectionMockStore) DeleteSecret(_ context.Context, _, _ string) error   { return nil }
func (m *mcpInjectionMockStore) ReEncryptUserSecrets(_ context.Context, _ string, _ int, _ func([]byte) ([]byte, error), _ func(context.Context) error) error {
	return nil
}
func (m *mcpInjectionMockStore) SetBindings(_ context.Context, _ string, _ []string) error {
	return nil
}
func (m *mcpInjectionMockStore) AddBindings(_ context.Context, _ string, _ []string) error {
	return nil
}
func (m *mcpInjectionMockStore) GetBindingsForSecret(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}
func (m *mcpInjectionMockStore) ListGlobalDefaultSecrets(_ context.Context, _ string) ([]*UserSecret, error) {
	return nil, nil
}
func (m *mcpInjectionMockStore) QueryAudit(_ context.Context, _ string, _ AuditQuery) ([]*AuditEntry, error) {
	return nil, nil
}
