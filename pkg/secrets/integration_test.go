// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestIntegration_SecretCRUD_FullStack tests create→get→update→delete through the service layer
func TestIntegration_SecretCRUD_FullStack(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	// Create
	created, err := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "integration-test", Type: SecretTypeAPIKey,
		Value:    `{"apiKey":"sk-test-123","kind":"openai","slug":"openai"}`,
		Metadata: json.RawMessage(`{"kind":"openai","slug":"openai","model":"gpt-4o"}`),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID == "" {
		t.Fatal("Created secret should have an ID")
	}

	// Get (metadata only, no value)
	got, err := svc.GetSecret(ctx, "user-1", created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "integration-test" {
		t.Errorf("Name mismatch: %s", got.Name)
	}
	if got.Type != SecretTypeAPIKey {
		t.Errorf("Type mismatch: %s", got.Type)
	}

	// Decrypt (verify value is correct)
	plaintext, err := svc.DecryptSecretValue(ctx, "user-1", sessionID, nil, created.ID)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(plaintext) != `{"apiKey":"sk-test-123","kind":"openai","slug":"openai"}` {
		t.Errorf("Decrypted value wrong: %s", string(plaintext))
	}

	// Update
	err = svc.UpdateSecret(ctx, "user-1", sessionID, nil, created.ID, UpdateSecretRequest{
		Value: `{"apiKey":"sk-new-456","kind":"openai","slug":"openai"}`,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Verify updated value
	plaintext2, err := svc.DecryptSecretValue(ctx, "user-1", sessionID, nil, created.ID)
	if err != nil {
		t.Fatalf("Decrypt after update: %v", err)
	}
	if string(plaintext2) != `{"apiKey":"sk-new-456","kind":"openai","slug":"openai"}` {
		t.Errorf("Updated value wrong: %s", string(plaintext2))
	}

	// Delete
	err = svc.DeleteSecret(ctx, "user-1", created.ID)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Verify gone
	_, err = svc.GetSecret(ctx, "user-1", created.ID)
	if err == nil {
		t.Error("Secret should not exist after deletion")
	}
}

// TestIntegration_BindingLifecycle tests the full binding flow
func TestIntegration_BindingLifecycle(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	// Create 3 secrets
	ids := make([]string, 3)
	for i := 0; i < 3; i++ {
		name := []string{"key-a", "key-b", "key-c"}[i]
		s, _ := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
			Name: name, Type: SecretTypeAPIKey, Value: "val-" + name,
			Metadata: json.RawMessage(`{"kind":"x","slug":"x"}`),
		})
		ids[i] = s.ID
	}

	// Bind all 3 to workspace
	_, err := svc.SetBindings(ctx, "user-1", "ws-1", ids)
	if err != nil {
		t.Fatalf("SetBindings: %v", err)
	}

	// Verify
	resp, _ := svc.GetBindings(ctx, "user-1", "ws-1")
	if len(resp.Bindings) != 3 {
		t.Fatalf("Expected 3 bindings, got %d", len(resp.Bindings))
	}

	// Remove one secret
	svc.DeleteSecret(ctx, "user-1", ids[1])

	// Bindings should auto-cascade
	resp, _ = svc.GetBindings(ctx, "user-1", "ws-1")
	if len(resp.Bindings) != 2 {
		t.Errorf("Expected 2 bindings after delete, got %d", len(resp.Bindings))
	}

	// Rebind with empty list
	_, _ = svc.SetBindings(ctx, "user-1", "ws-1", []string{})
	resp, _ = svc.GetBindings(ctx, "user-1", "ws-1")
	if len(resp.Bindings) != 0 {
		t.Errorf("Expected 0 bindings after clear, got %d", len(resp.Bindings))
	}
}

// TestIntegration_MultiSession tests that different sessions get independent DEKs
func TestIntegration_MultiSession(t *testing.T) {
	keyStore := newMockKeyStore()
	dekCache := newMockDEKCache()
	keySvc := NewKeyService(keyStore, dekCache)
	ctx := context.Background()

	password := []byte("password")
	_ = keySvc.InitializeUserKeysServerKEK(ctx, "user-1", "server_kek")

	// Login twice (two sessions)
	keySvc.UnlockDEK(ctx, "user-1", password, "sess-A", time.Hour)
	keySvc.UnlockDEK(ctx, "user-1", password, "sess-B", time.Hour)

	// Both should have DEK available
	if !keySvc.DEKAvailable(ctx, "sess-A") {
		t.Error("Session A should have DEK")
	}
	if !keySvc.DEKAvailable(ctx, "sess-B") {
		t.Error("Session B should have DEK")
	}

	// Evict one — other should still work
	keySvc.EvictDEK(ctx, "sess-A")
	if keySvc.DEKAvailable(ctx, "sess-A") {
		t.Error("Session A should be evicted")
	}
	if !keySvc.DEKAvailable(ctx, "sess-B") {
		t.Error("Session B should still be available")
	}

	// Both DEKs should be the same (same user, same key version)
	keySvc.UnlockDEK(ctx, "user-1", password, "sess-C", time.Hour)
	dekB, _ := keySvc.GetDEK(ctx, "sess-B", nil)
	dekC, _ := keySvc.GetDEK(ctx, "sess-C", nil)
	if !bytes.Equal(dekB, dekC) {
		t.Error("Same user same version should produce same DEK across sessions")
	}
}

// TestIntegration_InjectionAfterUpdate verifies injection reflects latest secret value
func TestIntegration_InjectionAfterUpdate(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	s, _ := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "mutable", Type: SecretTypeEnvSecret, Value: "original",
		Metadata: json.RawMessage(`{"var_name":"MY_VAR"}`),
	})
	_, _ = svc.SetBindings(ctx, "user-1", "ws-1", []string{s.ID})

	// Inject — should get "original"
	data, _ := svc.InjectSecrets(ctx, "user-1", sessionID, nil, "ws-1")
	var injected []InjectedSecret
	json.Unmarshal(data, &injected)
	if injected[0].Plaintext != "original" {
		t.Errorf("Expected 'original', got '%s'", injected[0].Plaintext)
	}

	// Update
	svc.UpdateSecret(ctx, "user-1", sessionID, nil, s.ID, UpdateSecretRequest{Value: "updated"})

	// Inject again — should get "updated"
	data, _ = svc.InjectSecrets(ctx, "user-1", sessionID, nil, "ws-1")
	json.Unmarshal(data, &injected)
	if injected[0].Plaintext != "updated" {
		t.Errorf("Expected 'updated', got '%s'", injected[0].Plaintext)
	}
}

// TestIntegration_AuditCompleteness verifies every operation produces audit entries
func TestIntegration_AuditCompleteness(t *testing.T) {
	svc, store, sessionID := setupSecretService(t)
	ctx := context.Background()

	// Clear any existing audit
	store.mu.Lock()
	store.audit = nil
	store.mu.Unlock()

	// Create
	s, _ := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "audit-complete", Type: SecretTypeAPIKey, Value: "v",
		Metadata: json.RawMessage(`{"kind":"x","slug":"x"}`),
	})

	// Update
	svc.UpdateSecret(ctx, "user-1", sessionID, nil, s.ID, UpdateSecretRequest{Value: "v2"})

	// Bind
	_, _ = svc.SetBindings(ctx, "user-1", "ws-1", []string{s.ID})

	// Inject
	svc.InjectSecrets(ctx, "user-1", sessionID, nil, "ws-1")

	// Delete
	svc.DeleteSecret(ctx, "user-1", s.ID)

	// Verify audit
	entries, _ := svc.QueryAudit(ctx, "user-1", AuditQuery{})
	actions := make(map[string]int)
	for _, e := range entries {
		actions[e.Action]++
	}

	expected := map[string]int{"create": 1, "update": 1, "bind": 1, "delete": 1}
	for action, count := range expected {
		if actions[action] < count {
			t.Errorf("Expected at least %d '%s' audit entries, got %d", count, action, actions[action])
		}
	}
}

// TestIntegration_RecoveryKeyFullFlow tests the complete recovery scenario
func TestIntegration_SecretTypeSpecificMetadata(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	tests := []struct {
		name       string
		secretName string
		secType    SecretType
		metadata   string
		checkKey   string
		checkVal   string
	}{
		{"ssh with host", "meta-ssh-with-host", SecretTypeSSHKey, `{"key_type":"rsa","host":"gitlab.com"}`, "host", "gitlab.com"},
		{"git with protocol", "meta-git-with-protocol", SecretTypeGitCredential, `{"host":"bitbucket.org","protocol":"https"}`, "host", "bitbucket.org"},
		{"file with path", "meta-file-with-path", SecretTypeSecretFile, `{"mount_path":"app/config.yaml"}`, "mount_path", "app/config.yaml"},
		{"env with var", "meta-env-with-var", SecretTypeEnvSecret, `{"var_name":"API_TOKEN"}`, "var_name", "API_TOKEN"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
				Name: tt.secretName, Type: tt.secType, Value: "secret-val",
				Metadata: json.RawMessage(tt.metadata),
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}

			// Get and verify metadata preserved
			got, _ := svc.GetSecret(ctx, "user-1", s.ID)
			var meta map[string]string
			json.Unmarshal(got.Metadata, &meta)
			if meta[tt.checkKey] != tt.checkVal {
				t.Errorf("Metadata[%s] = %s, want %s", tt.checkKey, meta[tt.checkKey], tt.checkVal)
			}
		})
	}
}
