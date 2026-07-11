// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- In-memory mock SecretStore ---

type mockSecretStore struct {
	mu                   sync.Mutex
	secrets              map[string]*UserSecret // keyed by ID
	bindings             map[string][]string    // workspace_id -> []secret_id
	audit                []*AuditEntry
	listGlobalDefaultErr error // optional: forces ListGlobalDefaultSecrets to fail
}

func newMockSecretStore() *mockSecretStore {
	return &mockSecretStore{
		secrets:  make(map[string]*UserSecret),
		bindings: make(map[string][]string),
	}
}

func (m *mockSecretStore) CreateSecret(_ context.Context, secret *UserSecret) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Check unique constraint
	for _, s := range m.secrets {
		if s.UserID == secret.UserID && s.Name == secret.Name {
			return &duplicateError{name: secret.Name}
		}
	}
	if secret.ID == "" {
		secret.ID = "sec-" + secret.Name // deterministic for tests
	}
	cp := *secret
	m.secrets[secret.ID] = &cp
	return nil
}

func (m *mockSecretStore) GetSecret(_ context.Context, userID, secretID string) (*UserSecret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.secrets[secretID]
	if !ok || s.UserID != userID {
		return nil, nil
	}
	cp := *s
	return &cp, nil
}

func (m *mockSecretStore) GetSecretByName(_ context.Context, userID, name string) (*UserSecret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.secrets {
		if s.UserID == userID && s.Name == name {
			cp := *s
			return &cp, nil
		}
	}
	return nil, nil
}

func (m *mockSecretStore) ListSecrets(_ context.Context, userID string) ([]*UserSecret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*UserSecret
	for _, s := range m.secrets {
		if s.UserID == userID {
			cp := *s
			result = append(result, &cp)
		}
	}
	return result, nil
}

func (m *mockSecretStore) ListGlobalDefaultSecrets(_ context.Context, userID string) ([]*UserSecret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listGlobalDefaultErr != nil {
		return nil, m.listGlobalDefaultErr
	}
	var result []*UserSecret
	for _, s := range m.secrets {
		if s.UserID == userID && s.GlobalDefault {
			cp := *s
			result = append(result, &cp)
		}
	}
	return result, nil
}

func (m *mockSecretStore) UpdateSecret(_ context.Context, secret *UserSecret) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.secrets[secret.ID]; !ok {
		return &notFoundError{id: secret.ID}
	}
	cp := *secret
	m.secrets[secret.ID] = &cp
	return nil
}

func (m *mockSecretStore) ReEncryptUserSecrets(ctx context.Context, userID string, newKeyVersion int, transform func([]byte) ([]byte, error), commit func(context.Context) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Two-pass: compute every new ciphertext first; if any transform
	// fails, abort without modifying state. This mirrors the
	// transactional contract the SecretStore interface promises.
	updates := make(map[string][]byte)
	for id, s := range m.secrets {
		if s.UserID != userID {
			continue
		}
		newCT, err := transform(s.Ciphertext)
		if err != nil {
			return err
		}
		updates[id] = newCT
	}
	// Run commit hook before any state change so failures roll back.
	if commit != nil {
		if err := commit(ctx); err != nil {
			return err
		}
	}
	for id, newCT := range updates {
		s := m.secrets[id]
		s.Ciphertext = newCT
		s.KeyVersion = newKeyVersion
	}
	return nil
}

func (m *mockSecretStore) DeleteSecret(_ context.Context, userID, secretID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.secrets[secretID]
	if !ok || s.UserID != userID {
		return &notFoundError{id: secretID}
	}
	delete(m.secrets, secretID)
	// Cascade bindings
	for wsID, sids := range m.bindings {
		var filtered []string
		for _, sid := range sids {
			if sid != secretID {
				filtered = append(filtered, sid)
			}
		}
		m.bindings[wsID] = filtered
	}
	return nil
}

func (m *mockSecretStore) SetBindings(_ context.Context, workspaceID string, secretIDs []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bindings[workspaceID] = secretIDs
	return nil
}

func (m *mockSecretStore) AddBindings(_ context.Context, workspaceID string, secretIDs []string) error {
	if len(secretIDs) == 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	existing := m.bindings[workspaceID]
	seen := make(map[string]struct{}, len(existing)+len(secretIDs))
	for _, id := range existing {
		seen[id] = struct{}{}
	}
	for _, id := range secretIDs {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		existing = append(existing, id)
	}
	m.bindings[workspaceID] = existing
	return nil
}

func (m *mockSecretStore) GetBindings(_ context.Context, workspaceID string) ([]*UserSecret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sids := m.bindings[workspaceID]
	var result []*UserSecret
	for _, sid := range sids {
		if s, ok := m.secrets[sid]; ok {
			cp := *s
			result = append(result, &cp)
		}
	}
	return result, nil
}

func (m *mockSecretStore) GetBindingsForSecret(_ context.Context, secretID string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var workspaces []string
	for wsID, sids := range m.bindings {
		for _, sid := range sids {
			if sid == secretID {
				workspaces = append(workspaces, wsID)
			}
		}
	}
	return workspaces, nil
}

func (m *mockSecretStore) LogAudit(_ context.Context, entry *AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audit = append(m.audit, entry)
	return nil
}

func (m *mockSecretStore) QueryAudit(_ context.Context, userID string, _ AuditQuery) ([]*AuditEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*AuditEntry
	for _, e := range m.audit {
		if e.UserID == userID {
			result = append(result, e)
		}
	}
	return result, nil
}

func (m *mockSecretStore) GetWorkspaceCredentials(_ context.Context, _ string) ([]CredentialBinding, error) {
	return nil, nil
}

func (m *mockSecretStore) UpsertFreeTierCredential(_ context.Context, _ []byte) error { return nil }

func (m *mockSecretStore) SeedWorkspaceCredentials(_ context.Context, _, _ string, _ *string) error {
	return nil
}

func (m *mockSecretStore) BindCredentialToAllUserWorkspaces(_ context.Context, _, _ string) error {
	return nil
}

func (m *mockSecretStore) HasUserProviderCredential(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}

// duplicateError wraps the package's ErrDuplicateSecret sentinel so
// errors.Is on the result of mockSecretStore.CreateSecret correctly
// classifies the error in handler tests. Without the Unwrap method,
// the handler's errors.Is(err, ErrDuplicateSecret) would not match.
type duplicateError struct{ name string }

func (e *duplicateError) Error() string { return "duplicate secret: " + e.name }
func (e *duplicateError) Unwrap() error { return ErrDuplicateSecret }

type notFoundError struct{ id string }

func (e *notFoundError) Error() string { return "not found: " + e.id }
func (e *notFoundError) Unwrap() error { return ErrSecretNotFound }

// --- Helper to set up a test SecretService with unlocked DEK ---

func setupSecretService(t *testing.T) (*SecretService, *mockSecretStore, string) {
	t.Helper()
	keyStore := newMockKeyStore()
	dekCache := newMockDEKCache()
	keySvc := NewKeyService(keyStore, dekCache)
	secretStore := newMockSecretStore()
	svc := NewSecretService(keySvc, secretStore)

	ctx := context.Background()
	userID := "user-1"
	password := []byte("test-password")
	sessionID := "session-1"

	_, err := keySvc.InitializeUserKeys(ctx, userID, password)
	if err != nil {
		t.Fatalf("InitializeUserKeys failed: %v", err)
	}
	err = keySvc.UnlockDEK(ctx, userID, password, sessionID, time.Hour)
	if err != nil {
		t.Fatalf("UnlockDEK failed: %v", err)
	}

	return svc, secretStore, sessionID
}

// --- Tests ---

// TestSecretService_CreateSecret_LLMProvider_Legacy documents the OLD (broken)
// behavior: storing an LLM key as api-key type with provider in metadata.
// This test exists to prove the old path still works for backward compat with
// any existing api-key secrets already in the database; new code should use
// SecretTypeLLMProvider instead (see TestSecretService_CreateSecret_LLMProvider_Correct).
func TestSecretService_CreateSecret_LLMProvider_Legacy(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	resp, err := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name:  "my-anthropic-key",
		Type:  SecretTypeAPIKey,
		Value: "sk-ant-api03-secret-key",
		Metadata: json.RawMessage(`{"kind": "anthropic",
		"slug": "anthropic"}`),
	})
	if err != nil {
		t.Fatalf("CreateSecret failed: %v", err)
	}

	if resp.Name != "my-anthropic-key" {
		t.Errorf("Expected name 'my-anthropic-key', got '%s'", resp.Name)
	}
	if resp.Type != SecretTypeAPIKey {
		t.Errorf("Expected type api-key, got %s", resp.Type)
	}
	if resp.ID == "" {
		t.Error("Expected non-empty ID")
	}
}

// TestSecretService_CreateSecret_LLMProvider_Correct verifies the CORRECT path:
// type=llm-provider, value=JSON-encoded LLMProviderData.
// The materializer only processes llm-provider type when building agent-config.json.
func TestSecretService_CreateSecret_LLMProvider_Correct(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	providerData, _ := json.Marshal(LLMProviderData{
		Kind:   "anthropic",
		Slug:   "anthropic",
		APIKey: "sk-ant-api03-test-key",
	})

	resp, err := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name:  "anthropic-prod",
		Type:  SecretTypeLLMProvider,
		Value: string(providerData),
	})
	if err != nil {
		t.Fatalf("CreateSecret(llm-provider) failed: %v", err)
	}
	if resp.Type != SecretTypeLLMProvider {
		t.Errorf("Expected type llm-provider, got %s", resp.Type)
	}
	if resp.Name != "anthropic-prod" {
		t.Errorf("Expected name anthropic-prod, got %s", resp.Name)
	}
}

// TestSecretService_CreateSecret_LLMProvider_InvalidJSON verifies that a
// non-JSON value for llm-provider type is rejected with ErrInvalidMetadata.
func TestSecretService_CreateSecret_LLMProvider_InvalidJSON(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	_, err := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name:  "bad-cred",
		Type:  SecretTypeLLMProvider,
		Value: "not-valid-json",
	})
	if err == nil {
		t.Fatal("Expected error for non-JSON llm-provider value, got nil")
	}
	if !errors.Is(err, ErrInvalidMetadata) {
		t.Errorf("Expected ErrInvalidMetadata, got %v", err)
	}
}

// TestSecretService_CreateSecret_LLMProvider_MissingProvider rejects a JSON
// value that omits the required provider field.
func TestSecretService_CreateSecret_LLMProvider_MissingProvider(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	val, _ := json.Marshal(map[string]string{"apiKey": "sk-ant-test"})
	_, err := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name:  "missing-provider",
		Type:  SecretTypeLLMProvider,
		Value: string(val),
	})
	if err == nil {
		t.Fatal("Expected error for missing provider field, got nil")
	}
	if !errors.Is(err, ErrInvalidMetadata) {
		t.Errorf("Expected ErrInvalidMetadata, got %v", err)
	}
}

// TestSecretService_CreateSecret_LLMProvider_MissingAPIKey rejects a JSON
// value that omits the required apiKey field.
func TestSecretService_CreateSecret_LLMProvider_MissingAPIKey(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	val, _ := json.Marshal(map[string]string{"kind": "anthropic",
		"slug": "anthropic"})
	_, err := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name:  "missing-apikey",
		Type:  SecretTypeLLMProvider,
		Value: string(val),
	})
	if err == nil {
		t.Fatal("Expected error for missing apiKey field, got nil")
	}
	if !errors.Is(err, ErrInvalidMetadata) {
		t.Errorf("Expected ErrInvalidMetadata, got %v", err)
	}
}

func TestSecretService_CreateSecret_SSHKey(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	resp, err := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name:     "github-ssh",
		Type:     SecretTypeSSHKey,
		Value:    "-----BEGIN OPENSSH PRIVATE KEY-----\n...",
		Metadata: json.RawMessage(`{"key_type": "ed25519", "host": "github.com"}`),
	})
	if err != nil {
		t.Fatalf("CreateSecret failed: %v", err)
	}
	if resp.Type != SecretTypeSSHKey {
		t.Errorf("Expected type ssh-key, got %s", resp.Type)
	}
}

func TestSecretService_CreateSecret_SSHKey_MissingMetadata(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	_, err := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name:  "github-ssh",
		Type:  SecretTypeSSHKey,
		Value: "key-data",
	})
	if err == nil {
		t.Error("SSH key without key_type metadata should fail")
	}
}

func TestSecretService_CreateSecret_SecretFile_MissingMountPath(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	_, err := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name:  "cert",
		Type:  SecretTypeSecretFile,
		Value: "cert-data",
	})
	if err == nil {
		t.Error("Secret file without mount_path metadata should fail")
	}
}

func TestSecretService_CreateSecret_EnvSecret_MissingVarName(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	_, err := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name:  "db-url",
		Type:  SecretTypeEnvSecret,
		Value: "postgres://...",
	})
	if err == nil {
		t.Error("Env secret without var_name metadata should fail")
	}
}

func TestSecretService_CreateSecret_InvalidType(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	_, err := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name:  "test",
		Type:  "invalid-type",
		Value: "data",
	})
	if err == nil {
		t.Error("Invalid secret type should fail")
	}
}

// TestSecretService_CreateSecret_InvalidType_ListsValidTypes is the
// regression test for Bug 7 in worklog 0085: the error message must
// enumerate the valid secret types so callers can fix the request
// without consulting external docs.
func TestSecretService_CreateSecret_InvalidType_ListsValidTypes(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	_, err := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "test", Type: "bogus", Value: "data",
	})
	if err == nil {
		t.Fatal("Invalid type must error")
	}
	msg := err.Error()
	for _, want := range []string{"api-key", "ssh-key", "git-credential", "secret-file", "env-secret"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q must list valid type %q", msg, want)
		}
	}
}

// TestSecretService_CreateSecret_InvalidMetadata_NamesField is the
// regression test for Bug 7: when metadata is missing a required key,
// the error names the exact field expected (e.g. "var_name") so callers
// don't have to reverse-engineer the schema.
func TestSecretService_CreateSecret_InvalidMetadata_NamesField(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	cases := []struct {
		secretType SecretType
		wantField  string
	}{
		{SecretTypeSSHKey, "key_type"},
		{SecretTypeSecretFile, "mount_path"},
		{SecretTypeEnvSecret, "var_name"},
	}
	for _, tc := range cases {
		_, err := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
			Name: "n-" + string(tc.secretType), Type: tc.secretType, Value: "v",
			Metadata: json.RawMessage(`{}`),
		})
		if err == nil {
			t.Errorf("%s: missing metadata must error", tc.secretType)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantField) {
			t.Errorf("%s: error %q must name field %q", tc.secretType, err.Error(), tc.wantField)
		}
	}
}

// TestSecretService_CreateSecret_RejectsAdversarialMountPath is the
// regression test for Bug 13 in worklog 0085: API-layer defense-in-depth
// against path-traversal in secret-file mount_path. The materializer's
// resolveMountPath catches these too, but accepting them at the API
// layer means adversarial input lives in the database long enough for a
// future bug or migration to mishandle it.
func TestSecretService_CreateSecret_RejectsAdversarialMountPath(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	bad := []string{
		"../../etc/passwd",
		"/etc/passwd",
		"/../../etc/shadow",
		"../escaped",
		".../traversal",
		"./valid/../../escape",
		"foo/../../bar",
		"", // empty
	}
	for _, mp := range bad {
		_, err := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
			Name: "f-" + mp, Type: SecretTypeSecretFile, Value: "x",
			Metadata: json.RawMessage(`{"mount_path":"` + mp + `"}`),
		})
		if err == nil {
			t.Errorf("mount_path %q must be rejected at the API layer", mp)
		}
	}

	// Sanity: a safe path is still accepted.
	_, err := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "safe-secret-file", Type: SecretTypeSecretFile, Value: "x",
		Metadata: json.RawMessage(`{"mount_path":"config/app.yaml"}`),
	})
	if err != nil {
		t.Errorf("safe mount_path was rejected: %v", err)
	}
}

// TestValidateMountPath is a direct unit test for the validateMountPath
// helper. It covers the cases that previously caused HTTP 400 when the
// frontend prepended "/home/sandbox/.secrets/" to the relative path, as
// well as path traversal patterns the function must block.
func TestValidateMountPath(t *testing.T) {
	rejected := []struct {
		name string
		mp   string
	}{
		// These are the cases from the bug: absolute paths (even valid ones
		// under the secrets base) must be rejected at the API layer so the
		// database always stores a relative path.
		{"absolute under secrets base", "/home/sandbox/.secrets/cert.pem"},
		{"absolute root", "/etc/passwd"},
		{"absolute with traversal", "/home/sandbox/.secrets/../../../etc/shadow"},
		// Classic traversal patterns.
		{"parent traversal", "../../etc/passwd"},
		{"single parent", "../escaped"},
		{"escape via valid prefix", "valid/../../escape"},
		{"sibling dir escape", "foo/../../bar"},
		// Degenerate inputs.
		{"empty string", ""},
		{"whitespace only", "   "},
		{"bare dot", "."},
	}
	for _, tc := range rejected {
		t.Run("reject_"+tc.name, func(t *testing.T) {
			if err := validateMountPath(tc.mp); err == nil {
				t.Errorf("validateMountPath(%q) returned nil; want an error", tc.mp)
			}
		})
	}

	accepted := []struct {
		name string
		mp   string
	}{
		// Simple filenames: the primary user-facing input after the bug fix.
		{"simple filename", "cert.pem"},
		{"filename with extension", "app.yaml"},
		// Paths within subdirectories stay under base.
		{"subdirectory path", "config/app.yaml"},
		{"deeply nested", "a/b/c/d.txt"},
		// Path components that clean to something safe.
		{"trailing slash cleaned", "certs/"},
		{"redundant dot", "certs/./file.pem"},
	}
	for _, tc := range accepted {
		t.Run("accept_"+tc.name, func(t *testing.T) {
			if err := validateMountPath(tc.mp); err != nil {
				t.Errorf("validateMountPath(%q) returned %v; want nil", tc.mp, err)
			}
		})
	}
}

// TestSecretService_CreateSecret_DuplicateName verifies that creating a secret
// with a name that already exists for the user returns ErrDuplicateSecret.
func TestSecretService_CreateSecret_DuplicateName(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	_, err := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name:  "my-key",
		Type:  SecretTypeAPIKey,
		Value: "value1",
		Metadata: json.RawMessage(`{"kind": "openai",
		"slug": "openai"}`),
	})
	if err != nil {
		t.Fatalf("First create failed: %v", err)
	}

	_, err = svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name:  "my-key",
		Type:  SecretTypeAPIKey,
		Value: "value2",
		Metadata: json.RawMessage(`{"kind": "openai",
		"slug": "openai"}`),
	})
	if err == nil {
		t.Error("Duplicate name should fail")
	}
}

func TestSecretService_CreateSecret_NoSession(t *testing.T) {
	keyStore := newMockKeyStore()
	dekCache := newMockDEKCache()
	keySvc := NewKeyService(keyStore, dekCache)
	secretStore := newMockSecretStore()
	svc := NewSecretService(keySvc, secretStore)
	ctx := context.Background()

	// Initialize keys but don't unlock
	_, _ = keySvc.InitializeUserKeys(ctx, "user-1", []byte("pw"))

	_, err := svc.CreateSecret(ctx, "user-1", "no-session", nil, CreateSecretRequest{
		Name:  "test",
		Type:  SecretTypeAPIKey,
		Value: "val",
	})
	if err == nil {
		t.Error("CreateSecret without active session should fail")
	}
}

func TestSecretService_GetSecret(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	created, _ := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name:  "test-secret",
		Type:  SecretTypeAPIKey,
		Value: "secret-value",
		Metadata: json.RawMessage(`{"kind": "openai",
		"slug": "openai"}`),
	})

	resp, err := svc.GetSecret(ctx, "user-1", created.ID)
	if err != nil {
		t.Fatalf("GetSecret failed: %v", err)
	}
	if resp.Name != "test-secret" {
		t.Errorf("Expected name 'test-secret', got '%s'", resp.Name)
	}
}

func TestSecretService_GetSecret_NotFound(t *testing.T) {
	svc, _, _ := setupSecretService(t)
	ctx := context.Background()

	_, err := svc.GetSecret(ctx, "user-1", "nonexistent")
	if err == nil {
		t.Error("GetSecret for nonexistent ID should fail")
	}
}

func TestSecretService_ListSecrets(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "key-1", Type: SecretTypeAPIKey, Value: "v1", Metadata: json.RawMessage(`{"kind":"a","slug":"a"}`),
	})
	svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "key-2", Type: SecretTypeEnvSecret, Value: "v2", Metadata: json.RawMessage(`{"var_name":"DB_URL"}`),
	})

	list, err := svc.ListSecrets(ctx, "user-1")
	if err != nil {
		t.Fatalf("ListSecrets failed: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("Expected 2 secrets, got %d", len(list))
	}
}

func TestSecretService_UpdateSecret(t *testing.T) {
	svc, store, sessionID := setupSecretService(t)
	ctx := context.Background()

	created, _ := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "updatable", Type: SecretTypeAPIKey, Value: "old-value", Metadata: json.RawMessage(`{"kind":"x","slug":"x"}`),
	})

	err := svc.UpdateSecret(ctx, "user-1", sessionID, nil, created.ID, UpdateSecretRequest{
		Value: "new-value",
	})
	if err != nil {
		t.Fatalf("UpdateSecret failed: %v", err)
	}

	// Verify ciphertext changed
	secret, _ := store.GetSecret(ctx, "user-1", created.ID)
	if secret == nil {
		t.Fatal("Secret should still exist")
	}
	// Decrypt and verify
	dek, _ := svc.keys.GetDEK(ctx, sessionID, nil)
	plaintext, _ := DecryptSecret(dek, secret.Ciphertext)
	if string(plaintext) != "new-value" {
		t.Errorf("Expected decrypted value 'new-value', got '%s'", string(plaintext))
	}
}

func TestSecretService_UpdateSecret_NotFound(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	err := svc.UpdateSecret(ctx, "user-1", sessionID, nil, "nonexistent", UpdateSecretRequest{Value: "x"})
	if err == nil {
		t.Error("UpdateSecret for nonexistent ID should fail")
	}
}

func TestSecretService_DeleteSecret(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	created, _ := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "deletable", Type: SecretTypeAPIKey, Value: "val", Metadata: json.RawMessage(`{"kind":"x","slug":"x"}`),
	})

	err := svc.DeleteSecret(ctx, "user-1", created.ID)
	if err != nil {
		t.Fatalf("DeleteSecret failed: %v", err)
	}

	_, err = svc.GetSecret(ctx, "user-1", created.ID)
	if err == nil {
		t.Error("Secret should not exist after deletion")
	}
}

func TestSecretService_DeleteSecret_NotFound(t *testing.T) {
	svc, _, _ := setupSecretService(t)
	ctx := context.Background()

	err := svc.DeleteSecret(ctx, "user-1", "nonexistent")
	if err == nil {
		t.Error("DeleteSecret for nonexistent ID should fail")
	}
}

func TestSecretService_DecryptSecretValue(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	originalValue := "sk-super-secret-key-12345"
	created, _ := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "decrypt-test", Type: SecretTypeAPIKey, Value: originalValue, Metadata: json.RawMessage(`{"kind":"x","slug":"x"}`),
	})

	plaintext, err := svc.DecryptSecretValue(ctx, "user-1", sessionID, nil, created.ID)
	if err != nil {
		t.Fatalf("DecryptSecretValue failed: %v", err)
	}
	if string(plaintext) != originalValue {
		t.Errorf("Expected '%s', got '%s'", originalValue, string(plaintext))
	}
}

func TestSecretService_SetAndGetBindings(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	s1, _ := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "key-1", Type: SecretTypeAPIKey, Value: "v1", Metadata: json.RawMessage(`{"kind":"a","slug":"a"}`),
	})
	s2, _ := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "key-2", Type: SecretTypeEnvSecret, Value: "v2", Metadata: json.RawMessage(`{"var_name":"X"}`),
	})

	_, err := svc.SetBindings(ctx, "user-1", "workspace-1", []string{s1.ID, s2.ID})
	if err != nil {
		t.Fatalf("SetBindings failed: %v", err)
	}

	resp, err := svc.GetBindings(ctx, "user-1", "workspace-1")
	if err != nil {
		t.Fatalf("GetBindings failed: %v", err)
	}
	if len(resp.Bindings) != 2 {
		t.Errorf("Expected 2 bindings, got %d", len(resp.Bindings))
	}
}

func TestSecretService_SetBindings_NonexistentSecret(t *testing.T) {
	svc, _, _ := setupSecretService(t)
	ctx := context.Background()

	_, err := svc.SetBindings(ctx, "user-1", "workspace-1", []string{"nonexistent-id"})
	if err == nil {
		t.Error("Binding nonexistent secret should fail")
	}
}

func TestSecretService_AuditLogging(t *testing.T) {
	svc, store, sessionID := setupSecretService(t)
	ctx := context.Background()

	// Create a secret (generates audit entry)
	created, _ := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "audited", Type: SecretTypeAPIKey, Value: "v", Metadata: json.RawMessage(`{"kind":"x","slug":"x"}`),
	})

	// Update (generates audit entry)
	svc.UpdateSecret(ctx, "user-1", sessionID, nil, created.ID, UpdateSecretRequest{Value: "v2"})

	// Delete (generates audit entry)
	svc.DeleteSecret(ctx, "user-1", created.ID)

	// Check audit entries
	entries, err := svc.QueryAudit(ctx, "user-1", AuditQuery{})
	if err != nil {
		t.Fatalf("QueryAudit failed: %v", err)
	}
	if len(entries) != 3 {
		t.Errorf("Expected 3 audit entries, got %d", len(entries))
	}

	// Verify actions
	actions := make([]string, len(store.audit))
	for i, e := range store.audit {
		actions[i] = e.Action
	}
	expected := []string{"create", "update", "delete"}
	for i, exp := range expected {
		if i >= len(actions) || actions[i] != exp {
			t.Errorf("Expected action[%d]='%s', got '%s'", i, exp, actions[i])
		}
	}
}

func TestSecretService_DeleteSecret_CascadesBindings(t *testing.T) {
	svc, secretStore, sessionID := setupSecretService(t)
	ctx := context.Background()

	created, _ := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "bound-secret", Type: SecretTypeAPIKey, Value: "v", Metadata: json.RawMessage(`{"kind":"x","slug":"x"}`),
	})

	// Bind to workspace
	_, _ = svc.SetBindings(ctx, "user-1", "ws-1", []string{created.ID})

	// Delete secret
	svc.DeleteSecret(ctx, "user-1", created.ID)

	// Bindings should be gone
	bindings, _ := secretStore.GetBindings(ctx, "ws-1")
	if len(bindings) != 0 {
		t.Errorf("Expected 0 bindings after delete, got %d", len(bindings))
	}
}

// TestSecretService_AddBindings_HappyPath_AppendsAndAudits is the
// regression test for the AddBindings primitive added during pass-2.
// SetWorkspaceEnv depends on AddBindings to merge new env-secrets
// into a workspace's binding set without clobbering pre-existing
// bindings; without coverage of the happy path, a regression that
// turns AddBindings into "no-op" or "replace-all" would silently
// break SetWorkspaceEnv for any workspace that already has bindings.
func TestSecretService_AddBindings_HappyPath_AppendsAndAudits(t *testing.T) {
	svc, secretStore, sessionID := setupSecretService(t)
	ctx := context.Background()

	pre, err := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "pre", Type: SecretTypeEnvSecret, Value: "p",
		Metadata: json.RawMessage(`{"var_name":"PRE"}`),
	})
	if err != nil {
		t.Fatalf("create pre: %v", err)
	}
	add1, _ := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "add1", Type: SecretTypeEnvSecret, Value: "a",
		Metadata: json.RawMessage(`{"var_name":"A"}`),
	})
	add2, _ := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "add2", Type: SecretTypeEnvSecret, Value: "b",
		Metadata: json.RawMessage(`{"var_name":"B"}`),
	})

	// Pre-existing binding (e.g. from an earlier SetBindings call).
	if _, err := svc.SetBindings(ctx, "user-1", "ws-1", []string{pre.ID}); err != nil {
		t.Fatalf("setbindings pre: %v", err)
	}

	// AddBindings must merge add1+add2 with the pre-existing binding.
	if _, err := svc.AddBindings(ctx, "user-1", "ws-1", []string{add1.ID, add2.ID}); err != nil {
		t.Fatalf("AddBindings: %v", err)
	}

	bindings, err := secretStore.GetBindings(ctx, "ws-1")
	if err != nil {
		t.Fatalf("GetBindings: %v", err)
	}
	gotIDs := map[string]bool{}
	for _, b := range bindings {
		gotIDs[b.ID] = true
	}
	for _, want := range []string{pre.ID, add1.ID, add2.ID} {
		if !gotIDs[want] {
			t.Errorf("AddBindings dropped expected secret %s; got %v", want, gotIDs)
		}
	}

	// Audit must record one "bind" entry per secret added (not for
	// the pre-existing one).
	q := AuditQuery{}
	auditEntries, err := svc.QueryAudit(ctx, "user-1", q)
	if err != nil {
		t.Fatalf("QueryAudit: %v", err)
	}
	bindCount := 0
	for _, e := range auditEntries {
		if e.Action == "bind" {
			bindCount++
		}
	}
	// 1 bind from SetBindings(pre) + 2 binds from AddBindings = 3.
	if bindCount != 3 {
		t.Errorf("expected 3 'bind' audit entries (1 pre + 2 add), got %d", bindCount)
	}
}

// TestSecretService_AddBindings_Idempotent verifies that re-adding
// already-bound secrets is a no-op rather than an error or duplicate
// binding row. The pg store relies on ON CONFLICT DO NOTHING; the
// in-memory mock relies on a 'seen' set. This test guards against a
// future regression in either.
func TestSecretService_AddBindings_Idempotent(t *testing.T) {
	svc, secretStore, sessionID := setupSecretService(t)
	ctx := context.Background()

	s, _ := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "s", Type: SecretTypeEnvSecret, Value: "v",
		Metadata: json.RawMessage(`{"var_name":"S"}`),
	})
	for i := 0; i < 3; i++ {
		if _, err := svc.AddBindings(ctx, "user-1", "ws-1", []string{s.ID}); err != nil {
			t.Fatalf("AddBindings call %d: %v", i, err)
		}
	}
	bindings, _ := secretStore.GetBindings(ctx, "ws-1")
	if len(bindings) != 1 {
		t.Errorf("expected 1 binding after 3 idempotent AddBindings, got %d", len(bindings))
	}
}

// TestSecretService_GetSecretByName_OwnerAndCrossUser verifies the
// service-layer name lookup used by SetWorkspaceEnv. Both
// "name doesn't exist" and "name exists but owned by another user"
// must collapse to (nil, nil) — the response shape (404 in the
// handler) is the same, but a distinguishing leak would let an
// attacker enumerate names cross-tenant.
func TestSecretService_GetSecretByName_OwnerAndCrossUser(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	created, err := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "shared-name", Type: SecretTypeEnvSecret, Value: "v",
		Metadata: json.RawMessage(`{"var_name":"X"}`),
	})
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}

	// Owner: returns the secret.
	got, err := svc.GetSecretByName(ctx, "user-1", "shared-name")
	if err != nil {
		t.Fatalf("GetSecretByName(owner): %v", err)
	}
	if got == nil || got.ID != created.ID {
		t.Errorf("owner: expected %s, got %v", created.ID, got)
	}

	// Cross-user: returns nil, nil (no leak).
	got, err = svc.GetSecretByName(ctx, "user-other", "shared-name")
	if err != nil {
		t.Errorf("cross-user: unexpected error %v (must be nil)", err)
	}
	if got != nil {
		t.Errorf("cross-user: must return nil; got %+v (cross-tenant name enumeration!)", got)
	}

	// Non-existent name: returns nil, nil.
	got, err = svc.GetSecretByName(ctx, "user-1", "no-such-name")
	if err != nil {
		t.Errorf("nonexistent: unexpected error %v", err)
	}
	if got != nil {
		t.Errorf("nonexistent: must return nil; got %+v", got)
	}
}

// TestSecretService_GetBindingsForSecret_OwnershipEnforced verifies
// that the secret-binding-reverse-lookup respects ownership. Pre-fix
// any DB error was conflated with "no such secret"; post-fix system
// errors propagate. Cross-user lookup must collapse to (nil, nil)
// without exposing the binding set.
func TestSecretService_GetBindingsForSecret_OwnershipEnforced(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	created, _ := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "lookup-me", Type: SecretTypeEnvSecret, Value: "v",
		Metadata: json.RawMessage(`{"var_name":"X"}`),
	})
	if _, err := svc.SetBindings(ctx, "user-1", "ws-1", []string{created.ID}); err != nil {
		t.Fatalf("SetBindings: %v", err)
	}

	// Owner sees the workspace.
	wsIDs, err := svc.GetBindingsForSecret(ctx, "user-1", created.ID)
	if err != nil {
		t.Fatalf("GetBindingsForSecret(owner): %v", err)
	}
	if len(wsIDs) != 1 || wsIDs[0] != "ws-1" {
		t.Errorf("owner: expected [ws-1], got %v", wsIDs)
	}

	// Cross-user: nil, nil — no leak of the binding set.
	wsIDs, err = svc.GetBindingsForSecret(ctx, "user-other", created.ID)
	if err != nil {
		t.Errorf("cross-user: unexpected error %v", err)
	}
	if wsIDs != nil {
		t.Errorf("cross-user: must return nil; got %v", wsIDs)
	}
}

// --- GlobalDefault / SeedGlobalDefaultSecrets tests ---

// TestSecretService_CreateSecret_GlobalDefault verifies that the GlobalDefault
// flag on CreateSecretRequest propagates to the stored UserSecret and the
// returned SecretResponse.
func TestSecretService_CreateSecret_GlobalDefault(t *testing.T) {
	svc, store, sessionID := setupSecretService(t)
	ctx := context.Background()

	resp, err := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name:          "global-key",
		Type:          SecretTypeEnvSecret,
		Value:         "v",
		Metadata:      json.RawMessage(`{"var_name":"X"}`),
		GlobalDefault: true,
	})
	if err != nil {
		t.Fatalf("CreateSecret failed: %v", err)
	}
	if !resp.GlobalDefault {
		t.Error("SecretResponse.GlobalDefault should be true")
	}

	stored, _ := store.GetSecret(ctx, "user-1", resp.ID)
	if stored == nil {
		t.Fatal("stored secret not found")
	}
	if !stored.GlobalDefault {
		t.Error("stored UserSecret.GlobalDefault should be true")
	}
}

// TestSecretService_CreateSecret_GlobalDefault_False verifies that the
// default is false when the field is omitted.
func TestSecretService_CreateSecret_GlobalDefault_False(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	resp, err := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name:     "non-global",
		Type:     SecretTypeEnvSecret,
		Value:    "v",
		Metadata: json.RawMessage(`{"var_name":"X"}`),
	})
	if err != nil {
		t.Fatalf("CreateSecret failed: %v", err)
	}
	if resp.GlobalDefault {
		t.Error("SecretResponse.GlobalDefault should default to false")
	}
}

// TestSecretService_ListGlobalDefaultSecrets_FilterCorrectness verifies that
// the store-level filter only returns secrets where global_default=true.
func TestSecretService_ListGlobalDefaultSecrets_FilterCorrectness(t *testing.T) {
	svc, store, sessionID := setupSecretService(t)
	ctx := context.Background()

	// Two global-default secrets, one non-global, belonging to user-1.
	svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "global-a", Type: SecretTypeEnvSecret, Value: "v",
		Metadata: json.RawMessage(`{"var_name":"A"}`), GlobalDefault: true,
	})
	svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "global-b", Type: SecretTypeEnvSecret, Value: "v",
		Metadata: json.RawMessage(`{"var_name":"B"}`), GlobalDefault: true,
	})
	svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "non-global", Type: SecretTypeEnvSecret, Value: "v",
		Metadata: json.RawMessage(`{"var_name":"C"}`),
	})

	defaults, err := store.ListGlobalDefaultSecrets(ctx, "user-1")
	if err != nil {
		t.Fatalf("ListGlobalDefaultSecrets failed: %v", err)
	}
	if len(defaults) != 2 {
		t.Fatalf("expected 2 global-default secrets, got %d", len(defaults))
	}
	for _, d := range defaults {
		if !d.GlobalDefault {
			t.Errorf("secret %s returned but GlobalDefault=false", d.Name)
		}
		if d.Name != "global-a" && d.Name != "global-b" {
			t.Errorf("unexpected secret returned: %s", d.Name)
		}
	}
}

// TestSecretService_SeedGlobalDefaultSecrets_HappyPath creates two global-
// default secrets, calls SeedGlobalDefaultSecrets, and verifies both are
// bound to the workspace and audit entries are recorded.
func TestSecretService_SeedGlobalDefaultSecrets_HappyPath(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	s1, _ := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "global-a", Type: SecretTypeEnvSecret, Value: "v",
		Metadata: json.RawMessage(`{"var_name":"A"}`), GlobalDefault: true,
	})
	s2, _ := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "global-b", Type: SecretTypeEnvSecret, Value: "v",
		Metadata: json.RawMessage(`{"var_name":"B"}`), GlobalDefault: true,
	})

	wsID := "ws-seed-1"
	if err := svc.SeedGlobalDefaultSecrets(ctx, wsID, "user-1"); err != nil {
		t.Fatalf("SeedGlobalDefaultSecrets failed: %v", err)
	}

	bindings, err := svc.GetBindings(ctx, "user-1", wsID)
	if err != nil {
		t.Fatalf("GetBindings failed: %v", err)
	}
	if len(bindings.Bindings) != 2 {
		t.Fatalf("expected 2 bindings, got %d", len(bindings.Bindings))
	}
	// Verify both secret IDs are present.
	seen := map[string]bool{}
	for _, b := range bindings.Bindings {
		seen[b.SecretID] = true
	}
	if !seen[s1.ID] || !seen[s2.ID] {
		t.Errorf("expected both %s and %s bound; got %v", s1.ID, s2.ID, seen)
	}

	// Audit entries: each auto-bind should record a "bind" action.
	entries, err := svc.QueryAudit(ctx, "user-1", AuditQuery{WorkspaceID: wsID})
	if err != nil {
		t.Fatalf("QueryAudit failed: %v", err)
	}
	bindCount := 0
	for _, e := range entries {
		if e.Action == "bind" {
			bindCount++
		}
	}
	if bindCount != 2 {
		t.Errorf("expected 2 bind audit entries, got %d", bindCount)
	}
}

// TestSecretService_SeedGlobalDefaultSecrets_EmptyPath verifies that a user
// with no global-default secrets results in a no-op (no bindings created,
// no error).
func TestSecretService_SeedGlobalDefaultSecrets_EmptyPath(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	// Create only a non-global secret.
	svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "non-global", Type: SecretTypeEnvSecret, Value: "v",
		Metadata: json.RawMessage(`{"var_name":"X"}`),
	})

	wsID := "ws-seed-empty"
	if err := svc.SeedGlobalDefaultSecrets(ctx, wsID, "user-1"); err != nil {
		t.Fatalf("SeedGlobalDefaultSecrets should not error on empty set: %v", err)
	}

	bindings, err := svc.GetBindings(ctx, "user-1", wsID)
	if err != nil {
		t.Fatalf("GetBindings failed: %v", err)
	}
	if len(bindings.Bindings) != 0 {
		t.Errorf("expected 0 bindings, got %d", len(bindings.Bindings))
	}
}

// TestSecretService_SeedGlobalDefaultSecrets_Idempotent verifies that calling
// SeedGlobalDefaultSecrets twice for the same workspace produces no duplicate
// bindings (AddBindings uses ON CONFLICT DO NOTHING at the store layer).
func TestSecretService_SeedGlobalDefaultSecrets_Idempotent(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "global-a", Type: SecretTypeEnvSecret, Value: "v",
		Metadata: json.RawMessage(`{"var_name":"A"}`), GlobalDefault: true,
	})

	wsID := "ws-seed-idem"
	if err := svc.SeedGlobalDefaultSecrets(ctx, wsID, "user-1"); err != nil {
		t.Fatalf("first SeedGlobalDefaultSecrets failed: %v", err)
	}
	if err := svc.SeedGlobalDefaultSecrets(ctx, wsID, "user-1"); err != nil {
		t.Fatalf("second SeedGlobalDefaultSecrets failed: %v", err)
	}

	bindings, err := svc.GetBindings(ctx, "user-1", wsID)
	if err != nil {
		t.Fatalf("GetBindings failed: %v", err)
	}
	if len(bindings.Bindings) != 1 {
		t.Errorf("expected 1 binding after double-seed, got %d", len(bindings.Bindings))
	}
}

// TestSecretService_SeedGlobalDefaultSecrets_StoreError verifies that a store
// error from ListGlobalDefaultSecrets propagates as an error from
// SeedGlobalDefaultSecrets.
func TestSecretService_SeedGlobalDefaultSecrets_StoreError(t *testing.T) {
	svc, store, sessionID := setupSecretService(t)
	ctx := context.Background()

	svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "global-a", Type: SecretTypeEnvSecret, Value: "v",
		Metadata: json.RawMessage(`{"var_name":"A"}`), GlobalDefault: true,
	})

	store.listGlobalDefaultErr = fmt.Errorf("simulated DB outage")
	defer func() { store.listGlobalDefaultErr = nil }()

	err := svc.SeedGlobalDefaultSecrets(ctx, "ws-seed-err", "user-1")
	if err == nil {
		t.Fatal("expected error from SeedGlobalDefaultSecrets on store failure")
	}
	if !strings.Contains(err.Error(), "simulated DB outage") {
		t.Errorf("expected wrapped DB error, got: %v", err)
	}
}

// TestSecretService_UpdateSecret_GlobalDefault_NilUnchanged verifies that a
// nil GlobalDefault pointer leaves the stored value unchanged.
func TestSecretService_UpdateSecret_GlobalDefault_NilUnchanged(t *testing.T) {
	svc, store, sessionID := setupSecretService(t)
	ctx := context.Background()

	created, _ := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name:          "global-key",
		Type:          SecretTypeEnvSecret,
		Value:         "v",
		Metadata:      json.RawMessage(`{"var_name":"X"}`),
		GlobalDefault: true,
	})

	// Update without specifying GlobalDefault (nil pointer).
	err := svc.UpdateSecret(ctx, "user-1", sessionID, nil, created.ID, UpdateSecretRequest{
		Value: "new-v",
	})
	if err != nil {
		t.Fatalf("UpdateSecret failed: %v", err)
	}

	stored, _ := store.GetSecret(ctx, "user-1", created.ID)
	if stored == nil {
		t.Fatal("secret not found")
	}
	if !stored.GlobalDefault {
		t.Error("GlobalDefault should remain true after update with nil pointer")
	}
}

// TestSecretService_UpdateSecret_GlobalDefault_ToggleOff verifies that
// GlobalDefault=&false flips the stored value from true to false and records
// the change in the audit metadata.
func TestSecretService_UpdateSecret_GlobalDefault_ToggleOff(t *testing.T) {
	svc, store, sessionID := setupSecretService(t)
	ctx := context.Background()

	created, _ := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name:          "global-key",
		Type:          SecretTypeEnvSecret,
		Value:         "v",
		Metadata:      json.RawMessage(`{"var_name":"X"}`),
		GlobalDefault: true,
	})

	newVal := false
	err := svc.UpdateSecret(ctx, "user-1", sessionID, nil, created.ID, UpdateSecretRequest{
		Value:         "new-v",
		GlobalDefault: &newVal,
	})
	if err != nil {
		t.Fatalf("UpdateSecret failed: %v", err)
	}

	stored, _ := store.GetSecret(ctx, "user-1", created.ID)
	if stored == nil {
		t.Fatal("secret not found")
	}
	if stored.GlobalDefault {
		t.Error("GlobalDefault should be false after toggle-off update")
	}

	// Audit entry should carry the globalDefault metadata key.
	entries, _ := svc.QueryAudit(ctx, "user-1", AuditQuery{Action: "update"})
	var found bool
	for _, e := range entries {
		if e.SecretID != nil && *e.SecretID == created.ID {
			if strings.Contains(string(e.Metadata), "globalDefault") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("expected update audit entry to record globalDefault change")
	}
}

// TestSecretService_UpdateSecret_GlobalDefault_ToggleOn verifies that
// GlobalDefault=&true flips the stored value from false to true.
func TestSecretService_UpdateSecret_GlobalDefault_ToggleOn(t *testing.T) {
	svc, store, sessionID := setupSecretService(t)
	ctx := context.Background()

	created, _ := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name:     "non-global",
		Type:     SecretTypeEnvSecret,
		Value:    "v",
		Metadata: json.RawMessage(`{"var_name":"X"}`),
	})

	newVal := true
	err := svc.UpdateSecret(ctx, "user-1", sessionID, nil, created.ID, UpdateSecretRequest{
		Value:         "new-v",
		GlobalDefault: &newVal,
	})
	if err != nil {
		t.Fatalf("UpdateSecret failed: %v", err)
	}

	stored, _ := store.GetSecret(ctx, "user-1", created.ID)
	if stored == nil {
		t.Fatal("secret not found")
	}
	if !stored.GlobalDefault {
		t.Error("GlobalDefault should be true after toggle-on update")
	}
}

// TestSecretService_UpdateSecret_GlobalDefault_SameValueNoAuditKey verifies
// that when GlobalDefault is set to the same value it already had, the audit
// metadata does NOT include the globalDefault key (no change = no record).
func TestSecretService_UpdateSecret_GlobalDefault_SameValueNoAuditKey(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	created, _ := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name:          "global-key",
		Type:          SecretTypeEnvSecret,
		Value:         "v",
		Metadata:      json.RawMessage(`{"var_name":"X"}`),
		GlobalDefault: true,
	})

	sameVal := true
	err := svc.UpdateSecret(ctx, "user-1", sessionID, nil, created.ID, UpdateSecretRequest{
		Value:         "new-v",
		GlobalDefault: &sameVal,
	})
	if err != nil {
		t.Fatalf("UpdateSecret failed: %v", err)
	}

	entries, _ := svc.QueryAudit(ctx, "user-1", AuditQuery{Action: "update"})
	for _, e := range entries {
		if e.SecretID != nil && *e.SecretID == created.ID {
			if strings.Contains(string(e.Metadata), "globalDefault") {
				t.Errorf("audit should not contain globalDefault key when value unchanged: %s", string(e.Metadata))
			}
		}
	}
}

// TestSecretService_G28_BindingsSurviveNoPodState is the G28 architectural
// invariant regression. The threat-model row originally flagged "PUT /workspaces/:id/bindings
// returns 204 but K8s Secret is never created" as a High gap. Epic 35
// (secretless injection) removed the durable K8s Secret path entirely;
// the architecture now persists bindings to PostgreSQL (the durable
// source of truth) and the init container fetches them via
// /internal/v1/pod-bootstrap at boot. The live HTTP push
// (agentpush.Service.Push) is best-effort; ErrNoRunningPod is an
// accepted, logged, transient state.
//
// This test locks the persistence invariant: after SetBindings commits,
// GetBindings returns the same set — proving that a binding created
// while no pod is running will be visible to the bootstrap endpoint at
// the next pod boot. If this test fails, the binding-persistence
// guarantee that G28's reclassification relies on is broken.
//
// G28 is classified as Accepted (not Fixed): the architecture
// intentionally defers first-time delivery to pod boot. This test
// ensures the deferral is safe.
func TestSecretService_G28_BindingsSurviveNoPodState(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()
	const userID = "user-1"
	const workspaceID = "ws-freshly-created"

	// Create two secrets — one LLM provider, one env-secret — covering
	// the two materialization paths the bootstrap endpoint must handle.
	s1, err := svc.CreateSecret(ctx, userID, sessionID, nil, CreateSecretRequest{
		Name:     "openai-key",
		Type:     SecretTypeAPIKey,
		Value:    "sk-test-g28",
		Metadata: json.RawMessage(`{"kind":"openai","slug":"openai"}`),
	})
	if err != nil {
		t.Fatalf("CreateSecret s1: %v", err)
	}
	s2, err := svc.CreateSecret(ctx, userID, sessionID, nil, CreateSecretRequest{
		Name:     "gh-token-env",
		Type:     SecretTypeEnvSecret,
		Value:    "ghp_test_token",
		Metadata: json.RawMessage(`{"var_name":"GH_TOKEN"}`),
	})
	if err != nil {
		t.Fatalf("CreateSecret s2: %v", err)
	}

	// Bind both secrets to a workspace that has never had a pod running
	// (the G28 scenario: "freshly-created workspace" — no pod yet).
	bindResult, err := svc.SetBindings(ctx, userID, workspaceID, []string{s1.ID, s2.ID})
	if err != nil {
		t.Fatalf("SetBindings: %v", err)
	}
	_ = bindResult // BindingsMutationResult describes the diff; success is err == nil

	// Now simulate "the pod eventually boots" — the bootstrap endpoint
	// calls GetBindings to resolve what to inject. The bindings MUST be
	// visible, proving the persistence survived the no-pod window.
	resp, err := svc.GetBindings(ctx, userID, workspaceID)
	if err != nil {
		t.Fatalf("GetBindings after no-pod window: %v", err)
	}
	if len(resp.Bindings) != 2 {
		t.Fatalf("G28 INVARIANT BROKEN: expected 2 bindings to survive no-pod state, got %d",
			len(resp.Bindings))
	}

	// Confirm both secret IDs are present (order-independent).
	boundIDs := make(map[string]struct{}, len(resp.Bindings))
	for _, b := range resp.Bindings {
		boundIDs[b.SecretID] = struct{}{}
	}
	for _, expected := range []string{s1.ID, s2.ID} {
		if _, ok := boundIDs[expected]; !ok {
			t.Errorf("G28 INVARIANT BROKEN: binding for secret %q not visible to GetBindings after no-pod state", expected)
		}
	}
}
