// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCredentialPrecedence_AdminFreeTierOverrideByUser verifies that a user
// credential for the same provider overrides an admin credential when both
// are bound to the same workspace.

// mustStaticProvider wraps a raw 32-byte key as a RootKeyProvider for tests.
// US-50.2: replaces the old AdminKeyDeriver callback with a per-purpose provider.
func mustStaticProvider(t *testing.T, key []byte) RootKeyProvider {
	t.Helper()
	p, err := NewStaticKeyProvider(key)
	require.NoError(t, err)
	return p
}

func TestCredentialPrecedence_AdminFreeTierOverrideByUser(t *testing.T) {
	keyStore := newMockKeyStore()
	dekCache := newTestDEKCache()
	keyService := NewKeyService(keyStore, dekCache)

	secretStore := newMockSecretStore()

	// Simulate admin key deriver returning a fixed key.
	adminKEK := make([]byte, 32)
	for i := range adminKEK {
		adminKEK[i] = byte(i + 1)
	}

	// Setup user DEK.
	userID := "user-1"
	sessionID := "sess-1"
	workspaceID := "ws-1"
	userDEK := make([]byte, 32)
	for i := range userDEK {
		userDEK[i] = byte(i + 100)
	}
	dekCache.CacheDEK(context.Background(), sessionID, userDEK, time.Hour) //nolint:errcheck

	// Create encrypted credentials.
	adminPlaintext, _ := json.Marshal(LLMProviderData{Kind: "anthropic", Slug: "anthropic", APIKey: "admin-key"})
	adminCipher, err := EncryptSecret(adminKEK, adminPlaintext)
	require.NoError(t, err)

	userPlaintext, _ := json.Marshal(LLMProviderData{Kind: "anthropic", Slug: "anthropic", APIKey: "user-key"}) //nolint:gosec
	userCipher, err := EncryptSecret(userDEK, userPlaintext)
	require.NoError(t, err)

	mockCredStore := &mockCredentialStore{
		bindings: []CredentialBinding{
			{ID: "cred-user", OwnerType: "user", OwnerID: userID, Kind: "anthropic", Slug: "anthropic", Ciphertext: userCipher, SourceType: "explicit"},
			{ID: "cred-admin", OwnerType: "admin", OwnerID: "_platform", Kind: "anthropic", Slug: "anthropic", Ciphertext: adminCipher, SourceType: "auto"},
		},
	}

	combinedStore := &combinedTestStore{SecretStore: secretStore, CredentialStore: mockCredStore}
	svc := NewSecretService(keyService, combinedStore)
	svc.SetAdminProvider(mustStaticProvider(t, adminKEK))
	svc.SetOrgProvider(mustStaticProvider(t, adminKEK))

	ctx := context.Background()
	result, err := svc.InjectSecrets(ctx, userID, sessionID, nil, workspaceID)
	require.NoError(t, err)

	var injected []InjectedSecret
	require.NoError(t, json.Unmarshal(result, &injected))

	llmSecrets := filterByType(injected, SecretTypeLLMProvider)
	require.Len(t, llmSecrets, 1, "expected exactly one anthropic credential (user wins)")

	var pd LLMProviderData
	require.NoError(t, json.Unmarshal([]byte(llmSecrets[0].Plaintext), &pd))
	assert.Equal(t, "user-key", pd.APIKey)
	assert.Equal(t, "anthropic", pd.Slug)
}

// TestCredentialPrecedence_ModelAllowlistFiltering verifies that model_allowlist
// restricts which models appear in the injected LLMProviderData.
func TestCredentialPrecedence_ModelAllowlistFiltering(t *testing.T) {
	keyStore := newMockKeyStore()
	dekCache := newTestDEKCache()
	keyService := NewKeyService(keyStore, dekCache)
	secretStore := newMockSecretStore()

	adminKEK := make([]byte, 32)
	for i := range adminKEK {
		adminKEK[i] = byte(i + 1)
	}

	adminPlaintext, _ := json.Marshal(LLMProviderData{
		Kind: "anthropic", Slug: "anthropic", APIKey: "key",
		Models: []LLMModelConfig{
			{ID: "claude-3", Label: "Claude 3"},
			{ID: "claude-2", Label: "Claude 2"},
			{ID: "claude-1", Label: "Claude 1"},
		},
	})
	adminCipher, err := EncryptSecret(adminKEK, adminPlaintext)
	require.NoError(t, err)

	mockCredStore := &mockCredentialStore{
		bindings: []CredentialBinding{{
			ID: "cred-filtered", OwnerType: "admin", OwnerID: "_platform",
			Kind: "anthropic", Slug: "anthropic", Ciphertext: adminCipher,
			SourceType: "auto", ModelAllowlist: []string{"claude-3", "claude-1"},
		}},
	}

	combinedStore := &combinedTestStore{SecretStore: secretStore, CredentialStore: mockCredStore}
	svc := NewSecretService(keyService, combinedStore)
	svc.SetAdminProvider(mustStaticProvider(t, adminKEK))
	svc.SetOrgProvider(mustStaticProvider(t, adminKEK))

	result, err := svc.InjectSecrets(context.Background(), "user-1", "no-session", nil, "ws-1")
	require.NoError(t, err)

	var injected []InjectedSecret
	require.NoError(t, json.Unmarshal(result, &injected))
	llm := filterByType(injected, SecretTypeLLMProvider)
	require.Len(t, llm, 1)

	var pd LLMProviderData
	require.NoError(t, json.Unmarshal([]byte(llm[0].Plaintext), &pd))
	assert.Len(t, pd.Models, 2)
	assert.Equal(t, "claude-3", pd.Models[0].ID)
	assert.Equal(t, "claude-1", pd.Models[1].ID)
}

// TestCredentialPrecedence_ModelAllowlistFallback verifies that when no models
// match the allowlist, synthetic stubs are created from the allowlist IDs.
func TestCredentialPrecedence_ModelAllowlistFallback(t *testing.T) {
	keyStore := newMockKeyStore()
	dekCache := newTestDEKCache()
	keyService := NewKeyService(keyStore, dekCache)
	secretStore := newMockSecretStore()

	adminKEK := make([]byte, 32)
	for i := range adminKEK {
		adminKEK[i] = byte(i + 1)
	}

	adminPlaintext, _ := json.Marshal(LLMProviderData{Kind: "openai", Slug: "openai", APIKey: "key"})
	adminCipher, err := EncryptSecret(adminKEK, adminPlaintext)
	require.NoError(t, err)

	mockCredStore := &mockCredentialStore{
		bindings: []CredentialBinding{{
			ID: "cred-stub", OwnerType: "admin", OwnerID: "_platform",
			Kind: "openai", Slug: "openai", Ciphertext: adminCipher,
			SourceType: "auto", ModelAllowlist: []string{"gpt-4o", "gpt-4o-mini"},
		}},
	}

	combinedStore := &combinedTestStore{SecretStore: secretStore, CredentialStore: mockCredStore}
	svc := NewSecretService(keyService, combinedStore)
	svc.SetAdminProvider(mustStaticProvider(t, adminKEK))
	svc.SetOrgProvider(mustStaticProvider(t, adminKEK))

	result, err := svc.InjectSecrets(context.Background(), "user-1", "no-session", nil, "ws-1")
	require.NoError(t, err)

	var injected []InjectedSecret
	require.NoError(t, json.Unmarshal(result, &injected))
	llm := filterByType(injected, SecretTypeLLMProvider)
	require.Len(t, llm, 1)

	var pd LLMProviderData
	require.NoError(t, json.Unmarshal([]byte(llm[0].Plaintext), &pd))
	assert.Len(t, pd.Models, 2)
	assert.Equal(t, "gpt-4o", pd.Models[0].ID)
	assert.Equal(t, "gpt-4o-mini", pd.Models[1].ID)
}

// TestCredentialPrecedence_DecryptionFailureFallback verifies that when a
// higher-priority credential fails to decrypt, the system falls through to
// a lower-priority one for the same provider.
func TestCredentialPrecedence_DecryptionFailureFallback(t *testing.T) {
	keyStore := newMockKeyStore()
	dekCache := newTestDEKCache()
	keyService := NewKeyService(keyStore, dekCache)
	secretStore := newMockSecretStore()

	adminKEK := make([]byte, 32)
	for i := range adminKEK {
		adminKEK[i] = byte(i + 1)
	}

	goodPlaintext, _ := json.Marshal(LLMProviderData{Kind: "anthropic", Slug: "anthropic", APIKey: "good-key"})
	goodCipher, err := EncryptSecret(adminKEK, goodPlaintext)
	require.NoError(t, err)

	mockCredStore := &mockCredentialStore{
		bindings: []CredentialBinding{
			{ID: "bad", OwnerType: "admin", OwnerID: "_platform", Kind: "anthropic", Slug: "anthropic",
				Ciphertext: []byte("corrupt"), SourceType: "explicit", WithinPriority: 10},
			{ID: "good", OwnerType: "admin", OwnerID: "_platform", Kind: "anthropic", Slug: "anthropic",
				Ciphertext: goodCipher, SourceType: "auto", WithinPriority: 0},
		},
	}

	combinedStore := &combinedTestStore{SecretStore: secretStore, CredentialStore: mockCredStore}
	svc := NewSecretService(keyService, combinedStore)
	svc.SetAdminProvider(mustStaticProvider(t, adminKEK))
	svc.SetOrgProvider(mustStaticProvider(t, adminKEK))

	result, err := svc.InjectSecrets(context.Background(), "user-1", "no-session", nil, "ws-1")
	require.NoError(t, err)

	var injected []InjectedSecret
	require.NoError(t, json.Unmarshal(result, &injected))
	llm := filterByType(injected, SecretTypeLLMProvider)
	require.Len(t, llm, 1, "should fall through to good credential")

	var pd LLMProviderData
	require.NoError(t, json.Unmarshal([]byte(llm[0].Plaintext), &pd))
	assert.Equal(t, "good-key", pd.APIKey)
}

// TestCredentialPrecedence_AdminOnlyNoSession verifies that admin credentials
// work without an active user session (no DEK needed).
func TestCredentialPrecedence_AdminOnlyNoSession(t *testing.T) {
	keyStore := newMockKeyStore()
	dekCache := newTestDEKCache()
	keyService := NewKeyService(keyStore, dekCache)
	secretStore := newMockSecretStore()

	adminKEK := make([]byte, 32)
	for i := range adminKEK {
		adminKEK[i] = byte(i + 1)
	}

	adminPlaintext, _ := json.Marshal(LLMProviderData{Kind: "opencode", Slug: "opencode", APIKey: "public"})
	adminCipher, err := EncryptSecret(adminKEK, adminPlaintext)
	require.NoError(t, err)

	mockCredStore := &mockCredentialStore{
		bindings: []CredentialBinding{
			{ID: "cred-free", OwnerType: "admin", OwnerID: "_platform", Kind: "opencode", Slug: "opencode", Ciphertext: adminCipher, SourceType: "auto"},
		},
	}

	combinedStore := &combinedTestStore{SecretStore: secretStore, CredentialStore: mockCredStore}
	svc := NewSecretService(keyService, combinedStore)
	svc.SetAdminProvider(mustStaticProvider(t, adminKEK))
	svc.SetOrgProvider(mustStaticProvider(t, adminKEK))

	ctx := context.Background()
	result, err := svc.InjectSecrets(ctx, "user-1", "no-session", nil, "ws-1")
	require.NoError(t, err)

	var injected []InjectedSecret
	require.NoError(t, json.Unmarshal(result, &injected))

	llmSecrets := filterByType(injected, SecretTypeLLMProvider)
	require.Len(t, llmSecrets, 1)

	var pd LLMProviderData
	require.NoError(t, json.Unmarshal([]byte(llmSecrets[0].Plaintext), &pd))
	assert.Equal(t, "public", pd.APIKey)
}

// --- Test helpers ---

func filterByType(secrets []InjectedSecret, typ SecretType) []InjectedSecret {
	var out []InjectedSecret
	for _, s := range secrets {
		if s.Type == typ {
			out = append(out, s)
		}
	}
	return out
}

type mockCredentialStore struct {
	bindings []CredentialBinding
}

func (m *mockCredentialStore) GetWorkspaceCredentials(_ context.Context, _ string) ([]CredentialBinding, error) {
	return m.bindings, nil
}

func (m *mockCredentialStore) UpsertFreeTierCredential(_ context.Context, _ []byte) error { return nil }
func (m *mockCredentialStore) SeedWorkspaceCredentials(_ context.Context, _, _ string, _ *string) error {
	return nil
}
func (m *mockCredentialStore) BindCredentialToAllUserWorkspaces(_ context.Context, _, _ string) error {
	return nil
}
func (m *mockCredentialStore) HasUserProviderCredential(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}

func (m *mockCredentialStore) GetWorkspaceMCPServers(_ context.Context, _ string) ([]MCPServerBindingRow, error) {
	return nil, nil
}

// combinedTestStore satisfies both SecretStore and CredentialStore via embedding.
type combinedTestStore struct {
	SecretStore
	CredentialStore
}

type testDEKCache struct {
	cache map[string][]byte
}

func newTestDEKCache() *testDEKCache {
	return &testDEKCache{cache: make(map[string][]byte)}
}

func (c *testDEKCache) Set(sessionID string, dek []byte) {
	c.cache[sessionID] = dek
}

func (c *testDEKCache) CacheDEK(_ context.Context, sessionID string, dek []byte, _ time.Duration) error {
	c.cache[sessionID] = dek
	return nil
}

func (c *testDEKCache) GetDEK(_ context.Context, sessionID string) ([]byte, error) {
	dek, ok := c.cache[sessionID]
	if !ok {
		return nil, ErrDEKUnavailable
	}
	return dek, nil
}

func (c *testDEKCache) EvictDEK(_ context.Context, _ string) error { return nil }

// TestAsyncAuditLogger_ImplementsCredentialStore verifies that the type assertion
// in the injector methods (InjectSecrets / InjectSessionlessSecrets) succeeds
// when store is *AsyncAuditLogger.
func TestAsyncAuditLogger_ImplementsCredentialStore(t *testing.T) {
	inner := newMockSecretStore()
	logger := &asyncAuditTestLogger{}
	audit := NewAsyncAuditLogger(inner, 16, logger)
	defer audit.Stop()

	// Type assertion must succeed.
	_, ok := interface{}(audit).(CredentialStore)
	require.True(t, ok, "AsyncAuditLogger must implement CredentialStore for injection path to activate")
}

// TestInjectSecrets_ViaAsyncAuditLogger ensures the new path
// activates when store is wrapped in AsyncAuditLogger (production configuration).
func TestInjectSecrets_ViaAsyncAuditLogger(t *testing.T) {
	// Inner store implements both SecretStore and CredentialStore.
	innerSecret := newMockSecretStore()
	adminKEK := make([]byte, 32)
	for i := range adminKEK {
		adminKEK[i] = byte(i + 1)
	}

	plaintext, _ := json.Marshal(LLMProviderData{Kind: "opencode", Slug: "opencode", APIKey: "public"})
	cipher, err := EncryptSecret(adminKEK, plaintext)
	require.NoError(t, err)

	// AsyncAuditLogger wraps the inner store (which is both SecretStore and doesn't implement CredentialStore).
	// We need to use a combined store that implements both.
	mockCred := &mockCredentialStore{
		bindings: []CredentialBinding{
			{ID: "c1", OwnerType: "admin", OwnerID: "_platform", Kind: "opencode", Slug: "opencode", Ciphertext: cipher, SourceType: "auto"},
		},
	}
	combined := &combinedTestStore{SecretStore: innerSecret, CredentialStore: mockCred}

	logger := &asyncAuditTestLogger{}
	audit := NewAsyncAuditLogger(combined, 16, logger)
	defer audit.Stop()

	dekCache := newTestDEKCache()
	keyService := NewKeyService(newMockKeyStore(), dekCache)

	svc := NewSecretService(keyService, audit)
	svc.SetAdminProvider(mustStaticProvider(t, adminKEK))
	svc.SetOrgProvider(mustStaticProvider(t, adminKEK))

	ctx := context.Background()
	result, err := svc.InjectSecrets(ctx, "user-1", "sess-1", nil, "ws-1")
	require.NoError(t, err)

	var injected []InjectedSecret
	require.NoError(t, json.Unmarshal(result, &injected))

	llm := filterByType(injected, SecretTypeLLMProvider)
	require.Len(t, llm, 1, "new path must activate through AsyncAuditLogger")
	assert.Contains(t, llm[0].Plaintext, `"public"`)
}

// TestCredentialPrecedence_AllowlistOnlyDefaultID verifies the regression fix:
// when the model allowlist contains only the invalid ID "default" (written by
// a mis-formed create request), the provider must still be injected (not
// silently dropped) and its pd.Models must be empty (no filtering applied).
// Prior to the fix, FormatOpenCodeConfig would write "models":{"default":{}}
// which made opencode return 0 providers.
func TestCredentialPrecedence_AllowlistOnlyDefaultID(t *testing.T) {
	keyStore := newMockKeyStore()
	dekCache := newTestDEKCache()
	keyService := NewKeyService(keyStore, dekCache)
	secretStore := newMockSecretStore()

	adminKEK := make([]byte, 32)
	for i := range adminKEK {
		adminKEK[i] = byte(i + 1)
	}

	adminPlaintext, _ := json.Marshal(LLMProviderData{Kind: "openai", Slug: "openai", APIKey: "sk-test"})
	adminCipher, err := EncryptSecret(adminKEK, adminPlaintext)
	require.NoError(t, err)

	mockCredStore := &mockCredentialStore{
		bindings: []CredentialBinding{{
			ID: "cred-bad-allowlist", OwnerType: "admin", OwnerID: "_platform",
			Kind: "openai", Slug: "openai", Ciphertext: adminCipher,
			SourceType:     "auto",
			ModelAllowlist: []string{"default"}, // invalid — should be skipped
		}},
	}

	combinedStore := &combinedTestStore{SecretStore: secretStore, CredentialStore: mockCredStore}
	svc := NewSecretService(keyService, combinedStore)
	svc.SetAdminProvider(mustStaticProvider(t, adminKEK))
	svc.SetOrgProvider(mustStaticProvider(t, adminKEK))

	result, err := svc.InjectSecrets(context.Background(), "user-1", "no-session", nil, "ws-1")
	require.NoError(t, err)

	var injected []InjectedSecret
	require.NoError(t, json.Unmarshal(result, &injected))
	llm := filterByType(injected, SecretTypeLLMProvider)

	// Provider must still be present (not dropped because of invalid allowlist).
	require.Len(t, llm, 1)

	var pd LLMProviderData
	require.NoError(t, json.Unmarshal([]byte(llm[0].Plaintext), &pd))
	// Models must be empty — no model filtering applied (safe fallback).
	assert.Empty(t, pd.Models, "Models must be empty when allowlist contains only invalid IDs")
}

// TestCredentialPrecedence_AllowlistMixedValidAndInvalid verifies that when the
// allowlist contains a mix of valid and invalid IDs, only the valid IDs survive.
func TestCredentialPrecedence_AllowlistMixedValidAndInvalid(t *testing.T) {
	keyStore := newMockKeyStore()
	dekCache := newTestDEKCache()
	keyService := NewKeyService(keyStore, dekCache)
	secretStore := newMockSecretStore()

	adminKEK := make([]byte, 32)
	for i := range adminKEK {
		adminKEK[i] = byte(i + 1)
	}

	adminPlaintext, _ := json.Marshal(LLMProviderData{Kind: "openai", Slug: "openai", APIKey: "sk-test"})
	adminCipher, err := EncryptSecret(adminKEK, adminPlaintext)
	require.NoError(t, err)

	mockCredStore := &mockCredentialStore{
		bindings: []CredentialBinding{{
			ID: "cred-mixed", OwnerType: "admin", OwnerID: "_platform",
			Kind: "openai", Slug: "openai", Ciphertext: adminCipher,
			SourceType:     "auto",
			ModelAllowlist: []string{"default", "", "glm-5.1", "gpt-4o"}, // 2 invalid + 2 valid
		}},
	}

	combinedStore := &combinedTestStore{SecretStore: secretStore, CredentialStore: mockCredStore}
	svc := NewSecretService(keyService, combinedStore)
	svc.SetAdminProvider(mustStaticProvider(t, adminKEK))
	svc.SetOrgProvider(mustStaticProvider(t, adminKEK))

	result, err := svc.InjectSecrets(context.Background(), "user-1", "no-session", nil, "ws-1")
	require.NoError(t, err)

	var injected []InjectedSecret
	require.NoError(t, json.Unmarshal(result, &injected))
	llm := filterByType(injected, SecretTypeLLMProvider)
	require.Len(t, llm, 1)

	var pd LLMProviderData
	require.NoError(t, json.Unmarshal([]byte(llm[0].Plaintext), &pd))
	// Only the 2 valid IDs must survive.
	require.Len(t, pd.Models, 2)
	ids := []string{pd.Models[0].ID, pd.Models[1].ID}
	assert.ElementsMatch(t, []string{"glm-5.1", "gpt-4o"}, ids)
}

type asyncAuditTestLogger struct{}

func (l *asyncAuditTestLogger) Info(_ string, _ ...interface{})           {}
func (l *asyncAuditTestLogger) Warn(_ string, _ ...interface{})           {}
func (l *asyncAuditTestLogger) Error(_ string, _ error, _ ...interface{}) {}
func (l *asyncAuditTestLogger) Debug(_ string, _ ...interface{})          {}
func (l *asyncAuditTestLogger) Fatal(_ string, _ error, _ ...interface{}) {}
func (l *asyncAuditTestLogger) Sync() error                               { return nil }
func (l *asyncAuditTestLogger) With(_ ...interface{}) pkginterfaces.LoggerInterface {
	return l
}

// TestCredentialPrecedence_ModelContextLimits_InjectedIntoLLMModelConfig verifies
// that ModelContextLimits from the credential binding are plumbed into the
// LLMModelConfig.ContextLimit field during injection.
//
// This is the critical path for the contextTotal fix (worklog 0272): ContextLimit
// flows LLMModelConfig → FormatOpenCodeConfig → agent-config.json → opencode
// /config/providers → agentd ModelContextLimit() → CRD contextTotal → frontend.
func TestCredentialPrecedence_ModelContextLimits_InjectedIntoLLMModelConfig(t *testing.T) {
	keyStore := newMockKeyStore()
	dekCache := newTestDEKCache()
	keyService := NewKeyService(keyStore, dekCache)
	secretStore := newMockSecretStore()

	adminKEK := make([]byte, 32)
	for i := range adminKEK {
		adminKEK[i] = byte(i + 10)
	}

	// Credential has no models in the blob — relies on ModelAllowlist + ModelContextLimits + ModelOutputLimits.
	adminPlaintext, _ := json.Marshal(LLMProviderData{
		Kind: "thekao cloud", Slug: "thekao cloud",
		APIKey:  "sk-test",
		BaseURL: "https://ai.thekao.cloud/v1",
	})
	adminCipher, err := EncryptSecret(adminKEK, adminPlaintext)
	require.NoError(t, err)

	mockCredStore := &mockCredentialStore{
		bindings: []CredentialBinding{{
			ID: "cred-thekao", OwnerType: "admin", OwnerID: "_platform",
			Kind: "thekao cloud", Slug: "thekao cloud", Ciphertext: adminCipher,
			SourceType:         "auto",
			ModelAllowlist:     []string{"glm-5.1", "glm-5.2", "classifier"},
			ModelContextLimits: map[string]int{"glm-5.1": 200000, "glm-5.2": 1000000},
			ModelOutputLimits:  map[string]int{"glm-5.1": 8192, "glm-5.2": 16384},
			// classifier has neither context nor output limit
		}},
	}

	combinedStore := &combinedTestStore{SecretStore: secretStore, CredentialStore: mockCredStore}
	svc := NewSecretService(keyService, combinedStore)
	svc.SetAdminProvider(mustStaticProvider(t, adminKEK))
	svc.SetOrgProvider(mustStaticProvider(t, adminKEK))

	result, err := svc.InjectSecrets(context.Background(), "user-1", "no-session", nil, "ws-1")
	require.NoError(t, err)

	var injected []InjectedSecret
	require.NoError(t, json.Unmarshal(result, &injected))
	llm := filterByType(injected, SecretTypeLLMProvider)
	require.Len(t, llm, 1)

	var pd LLMProviderData
	require.NoError(t, json.Unmarshal([]byte(llm[0].Plaintext), &pd))
	require.Len(t, pd.Models, 3, "all three allowlisted models must be present")

	byID := map[string]LLMModelConfig{}
	for _, m := range pd.Models {
		byID[m.ID] = m
	}

	assert.Equal(t, 200000, byID["glm-5.1"].ContextLimit,
		"glm-5.1 context limit must be 200000 from ModelContextLimits")
	assert.Equal(t, 1000000, byID["glm-5.2"].ContextLimit,
		"glm-5.2 context limit must be 1000000 from ModelContextLimits")
	assert.Equal(t, 0, byID["classifier"].ContextLimit,
		"classifier has no configured context limit — must remain 0")

	assert.Equal(t, 8192, byID["glm-5.1"].OutputLimit,
		"glm-5.1 output limit must be 8192 from ModelOutputLimits")
	assert.Equal(t, 16384, byID["glm-5.2"].OutputLimit,
		"glm-5.2 output limit must be 16384 from ModelOutputLimits")
	assert.Equal(t, 0, byID["classifier"].OutputLimit,
		"classifier has no configured output limit — must remain 0")
}

// TestCredentialPrecedence_ModelContextLimits_DoesNotOverrideExisting verifies
// that if a model in LLMProviderData.Models already has a ContextLimit or
// OutputLimit set (e.g. from the relay config), the binding-level
// ModelContextLimits / ModelOutputLimits do NOT overwrite them.
func TestCredentialPrecedence_ModelContextLimits_DoesNotOverrideExisting(t *testing.T) {
	keyStore := newMockKeyStore()
	dekCache := newTestDEKCache()
	keyService := NewKeyService(keyStore, dekCache)
	secretStore := newMockSecretStore()

	adminKEK := make([]byte, 32)
	for i := range adminKEK {
		adminKEK[i] = byte(i + 20)
	}

	// Model in the blob already has ContextLimit=128000 and OutputLimit=16384.
	adminPlaintext, _ := json.Marshal(LLMProviderData{
		Kind: "openai", Slug: "openai", APIKey: "sk-oai",
		Models: []LLMModelConfig{
			{ID: "gpt-4o", ContextLimit: 128000, OutputLimit: 16384},
		},
	})
	adminCipher, err := EncryptSecret(adminKEK, adminPlaintext)
	require.NoError(t, err)

	mockCredStore := &mockCredentialStore{
		bindings: []CredentialBinding{{
			ID: "cred-oai", OwnerType: "admin", OwnerID: "_platform",
			Kind: "openai", Slug: "openai", Ciphertext: adminCipher,
			SourceType:         "auto",
			ModelAllowlist:     []string{"gpt-4o"},
			ModelContextLimits: map[string]int{"gpt-4o": 999999}, // should NOT override
			ModelOutputLimits:  map[string]int{"gpt-4o": 999999}, // should NOT override
		}},
	}

	combinedStore := &combinedTestStore{SecretStore: secretStore, CredentialStore: mockCredStore}
	svc := NewSecretService(keyService, combinedStore)
	svc.SetAdminProvider(mustStaticProvider(t, adminKEK))
	svc.SetOrgProvider(mustStaticProvider(t, adminKEK))

	result, err := svc.InjectSecrets(context.Background(), "user-1", "no-session", nil, "ws-1")
	require.NoError(t, err)

	var injected []InjectedSecret
	require.NoError(t, json.Unmarshal(result, &injected))
	llm := filterByType(injected, SecretTypeLLMProvider)
	require.Len(t, llm, 1)

	var pd LLMProviderData
	require.NoError(t, json.Unmarshal([]byte(llm[0].Plaintext), &pd))
	require.Len(t, pd.Models, 1)

	assert.Equal(t, 128000, pd.Models[0].ContextLimit,
		"ContextLimit from blob (128000) must NOT be overwritten by ModelContextLimits (999999)")
	assert.Equal(t, 16384, pd.Models[0].OutputLimit,
		"OutputLimit from blob (16384) must NOT be overwritten by ModelOutputLimits (999999)")
}

// TestCredentialPrecedence_OrgCredentialViaServerKEK verifies that an
// OwnerType="org" credential decrypts using the server KEK derived from the
// "org-credentials" HKDF label and produces correct provider data.
//
// This is the keystone behavior of the org-DEK elimination (design 0031, Story 1):
// org credentials must inject via the server-side KEK with no per-org DEK and no
// active admin session. A regression that drops the "org" case from the
// OwnerType switch in injection.go, or that uses the wrong label, fails here.
func TestCredentialPrecedence_OrgCredentialViaServerKEK(t *testing.T) {
	keyStore := newMockKeyStore()
	dekCache := newTestDEKCache()
	keyService := NewKeyService(keyStore, dekCache)
	secretStore := newMockSecretStore()

	orgKEK := make([]byte, 32)
	for i := range orgKEK {
		orgKEK[i] = byte(i + 50)
	}

	orgPlaintext, _ := json.Marshal(LLMProviderData{Kind: "openai", Slug: "openai", APIKey: "org-key"}) //nolint:gosec
	orgCipher, err := EncryptSecret(orgKEK, orgPlaintext)
	require.NoError(t, err)

	mockCredStore := &mockCredentialStore{
		bindings: []CredentialBinding{{
			ID: "cred-org", OwnerType: "org", OwnerID: "org-1",
			Kind: "openai", Slug: "openai", Ciphertext: orgCipher, SourceType: "auto",
		}},
	}

	combinedStore := &combinedTestStore{SecretStore: secretStore, CredentialStore: mockCredStore}
	svc := NewSecretService(keyService, combinedStore)
	svc.SetOrgProvider(mustStaticProvider(t, orgKEK))

	result, err := svc.InjectSecrets(context.Background(), "user-1", "no-session", nil, "ws-1")
	require.NoError(t, err)

	var injected []InjectedSecret
	require.NoError(t, json.Unmarshal(result, &injected))
	llm := filterByType(injected, SecretTypeLLMProvider)
	require.Len(t, llm, 1, "org credential must inject via server KEK")

	var pd LLMProviderData
	require.NoError(t, json.Unmarshal([]byte(llm[0].Plaintext), &pd))
	assert.Equal(t, "org-key", pd.APIKey)
	assert.Equal(t, "openai", pd.Slug)
}

// TestCredentialPrecedence_DomainSeparation_AdminAndOrgDistinctKeys verifies
// HKDF domain separation between admin and org credentials: a deriver that
// returns cryptographically distinct keys for the "provider-credentials" and
// "org-credentials" labels must successfully decrypt BOTH an admin and an org
// credential in the same workspace.
//
// A regression that reuses the admin label for org credentials (or vice versa)
// causes exactly one decryption to fail here — the wrong-key ciphertext is
// unreadable. This is stronger than a label-string assertion: it proves the
// decryptBinding switch routes each OwnerType to its matching key end-to-end.
func TestCredentialPrecedence_DomainSeparation_AdminAndOrgDistinctKeys(t *testing.T) {
	keyStore := newMockKeyStore()
	dekCache := newTestDEKCache()
	keyService := NewKeyService(keyStore, dekCache)
	secretStore := newMockSecretStore()

	adminKEK := make([]byte, 32)
	for i := range adminKEK {
		adminKEK[i] = byte(i + 1)
	}
	orgKEK := make([]byte, 32)
	for i := range orgKEK {
		orgKEK[i] = byte(i + 200) // distinct from adminKEK
	}
	require.NotEqual(t, adminKEK, orgKEK, "test premise: keys must differ")

	// Admin credential for anthropic, encrypted with adminKEK.
	adminPlaintext, _ := json.Marshal(LLMProviderData{Kind: "anthropic", Slug: "anthropic", APIKey: "admin-key"})
	adminCipher, err := EncryptSecret(adminKEK, adminPlaintext)
	require.NoError(t, err)

	// Org credential for openai, encrypted with orgKEK.
	orgPlaintext, _ := json.Marshal(LLMProviderData{Kind: "openai", Slug: "openai", APIKey: "org-key"}) //nolint:gosec
	orgCipher, err := EncryptSecret(orgKEK, orgPlaintext)
	require.NoError(t, err)

	mockCredStore := &mockCredentialStore{
		bindings: []CredentialBinding{
			{ID: "cred-admin", OwnerType: "admin", OwnerID: "_platform", Kind: "anthropic", Slug: "anthropic", Ciphertext: adminCipher, SourceType: "auto"},
			{ID: "cred-org", OwnerType: "org", OwnerID: "org-1", Kind: "openai", Slug: "openai", Ciphertext: orgCipher, SourceType: "auto"},
		},
	}

	combinedStore := &combinedTestStore{SecretStore: secretStore, CredentialStore: mockCredStore}
	svc := NewSecretService(keyService, combinedStore)
	svc.SetAdminProvider(mustStaticProvider(t, adminKEK))
	svc.SetOrgProvider(mustStaticProvider(t, orgKEK))

	result, err := svc.InjectSecrets(context.Background(), "user-1", "no-session", nil, "ws-1")
	require.NoError(t, err)

	var injected []InjectedSecret
	require.NoError(t, json.Unmarshal(result, &injected))
	llm := filterByType(injected, SecretTypeLLMProvider)
	require.Len(t, llm, 2, "both admin and org credentials must decrypt with their respective KEKs")

	byProvider := map[string]string{}
	for _, s := range llm {
		var pd LLMProviderData
		require.NoError(t, json.Unmarshal([]byte(s.Plaintext), &pd))
		byProvider[pd.Slug] = pd.APIKey
	}
	assert.Equal(t, "admin-key", byProvider["anthropic"], "admin cred must decrypt with adminKEK")
	assert.Equal(t, "org-key", byProvider["openai"], "org cred must decrypt with orgKEK")
}

// TestCredentialPrecedence_OrgCredential_WrongKEK_FailsAndFallsBack verifies
// that an org credential encrypted with one key cannot be decrypted when the
// server derives a different key for the "org-credentials" label, and that the
// workspace boots without that provider rather than crashing or injecting
// corrupt data. This anchors the fail-soft contract in decryptBinding.
func TestCredentialPrecedence_OrgCredential_WrongKEK_FailsAndFallsBack(t *testing.T) {
	keyStore := newMockKeyStore()
	dekCache := newTestDEKCache()
	keyService := NewKeyService(keyStore, dekCache)
	secretStore := newMockSecretStore()

	encryptKEK := make([]byte, 32) // key used to encrypt the stored ciphertext
	for i := range encryptKEK {
		encryptKEK[i] = byte(i + 1)
	}
	deriveKEK := make([]byte, 32) // different key the deriver returns → decrypt fails
	for i := range deriveKEK {
		deriveKEK[i] = byte(i + 77)
	}

	orgPlaintext, _ := json.Marshal(LLMProviderData{Kind: "openai", Slug: "openai", APIKey: "org-key"}) //nolint:gosec
	orgCipher, err := EncryptSecret(encryptKEK, orgPlaintext)
	require.NoError(t, err)

	mockCredStore := &mockCredentialStore{
		bindings: []CredentialBinding{{
			ID: "cred-org", OwnerType: "org", OwnerID: "org-1",
			Kind: "openai", Slug: "openai", Ciphertext: orgCipher, SourceType: "auto",
		}},
	}

	combinedStore := &combinedTestStore{SecretStore: secretStore, CredentialStore: mockCredStore}
	svc := NewSecretService(keyService, combinedStore)
	svc.SetAdminProvider(mustStaticProvider(t, deriveKEK))
	svc.SetOrgProvider(mustStaticProvider(t, deriveKEK))

	result, err := svc.InjectSecrets(context.Background(), "user-1", "no-session", nil, "ws-1")
	require.NoError(t, err, "fail-soft: decrypt failure must not error the whole call")

	var injected []InjectedSecret
	require.NoError(t, json.Unmarshal(result, &injected))
	llm := filterByType(injected, SecretTypeLLMProvider)
	assert.Empty(t, llm, "undecryptable org credential must be skipped, not injected")
}

// TestCredentialPrecedence_SameKind_DifferentSlugs_BothMaterialize is the
// canonical Epic 55 regression guard. Two credentials of the SAME SDK kind
// (openai_compatible) with DIFFERENT slugs (litellm-prod, litellm-staging)
// must both materialize as separate entries in the injected payload — they
// are NOT collapsed by the dedup loop.
//
// Why this matters: pre-Epic-55 the `seen` dedup keyed on Provider (the SDK
// kind), so two openai_compatible credentials would collide and only one
// would reach opencode. The whole point of slug-as-identity is that two
// LiteLLM endpoints, two Bedrock accounts, or two OpenAI staging credentials
// can coexist. The dedup must key on Slug, not Kind.
//
// A regression that reverts the dedup key from `seen[b.Slug]` to
// `seen[b.Kind]` (or any of `b.Provider`-shaped renames) would silently
// collapse these two providers into one and the original incident class
// would reappear. This test fails loudly in that case.
func TestCredentialPrecedence_SameKind_DifferentSlugs_BothMaterialize(t *testing.T) {
	keyStore := newMockKeyStore()
	dekCache := newTestDEKCache()
	keyService := NewKeyService(keyStore, dekCache)

	secretStore := newMockSecretStore()

	orgKEK := make([]byte, 32)
	for i := range orgKEK {
		orgKEK[i] = byte(i + 1)
	}

	// Two org credentials, same Kind ("openai_compatible"), different Slugs.
	prodPlain, _ := json.Marshal(LLMProviderData{
		Kind:    "openai_compatible",
		Slug:    "litellm-prod",
		APIKey:  "sk-prod",
		BaseURL: "https://litellm-prod.example/v1",
	})
	prodCipher, err := EncryptSecret(orgKEK, prodPlain)
	require.NoError(t, err)

	stagingPlain, _ := json.Marshal(LLMProviderData{
		Kind:    "openai_compatible",
		Slug:    "litellm-staging",
		APIKey:  "sk-staging",
		BaseURL: "https://litellm-staging.example/v1",
	})
	stagingCipher, err := EncryptSecret(orgKEK, stagingPlain)
	require.NoError(t, err)

	mockCredStore := &mockCredentialStore{
		bindings: []CredentialBinding{
			{ID: "cred-prod", OwnerType: "org", OwnerID: "org-1",
				Kind: "openai_compatible", Slug: "litellm-prod",
				Ciphertext: prodCipher, SourceType: "auto"},
			{ID: "cred-staging", OwnerType: "org", OwnerID: "org-1",
				Kind: "openai_compatible", Slug: "litellm-staging",
				Ciphertext: stagingCipher, SourceType: "auto"},
		},
	}

	combinedStore := &combinedTestStore{SecretStore: secretStore, CredentialStore: mockCredStore}
	svc := NewSecretService(keyService, combinedStore)
	svc.SetAdminProvider(mustStaticProvider(t, orgKEK))
	svc.SetOrgProvider(mustStaticProvider(t, orgKEK))

	result, err := svc.InjectSecrets(context.Background(), "user-1", "no-session", nil, "ws-1")
	require.NoError(t, err)

	var injected []InjectedSecret
	require.NoError(t, json.Unmarshal(result, &injected))
	llm := filterByType(injected, SecretTypeLLMProvider)
	require.Len(t, llm, 2,
		"same-kind, different-slug credentials must BOTH materialize (Epic 55). "+
			"If this fails with len=1, dedup was reverted to key on Kind instead of Slug.")

	// Verify each slug appears exactly once as the InjectedSecret.Name (which
	// becomes the agent-config.json provider-map key).
	slugs := map[string]bool{}
	for _, s := range llm {
		slugs[s.Name] = true
		var pd LLMProviderData
		require.NoError(t, json.Unmarshal([]byte(s.Plaintext), &pd))
		require.Equal(t, "openai_compatible", pd.Kind,
			"both credentials share the same Kind")
		require.Equal(t, s.Name, pd.Slug,
			"InjectedSecret.Name must equal the credential's Slug (wire-format key)")
	}
	require.True(t, slugs["litellm-prod"], "litellm-prod must be present")
	require.True(t, slugs["litellm-staging"], "litellm-staging must be present")
}
