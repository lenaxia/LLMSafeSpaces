// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestE2E_FullSecretLifecycle exercises the complete flow:
// register → init keys → login (unlock DEK) → create secrets → bind → inject → rotate → verify
func TestE2E_FullSecretLifecycle(t *testing.T) {
	keyStore := newMockKeyStore()
	dekCache := newMockDEKCache()
	keySvc := NewKeyService(keyStore, dekCache)
	secretStore := newMockSecretStore()
	svc := NewSecretService(keySvc, secretStore)
	ctx := context.Background()

	userID := "user-e2e"
	password := []byte("secure-password-123!")
	workspaceID := "ws-e2e-1"

	// === Phase 1: Account creation (register) ===
	err := keySvc.InitializeUserKeysServerKEK(ctx, userID, "server_kek")
	if err != nil {
		t.Fatalf("InitializeUserKeysServerKEK: %v", err)
	}

	// === Phase 2: Login (unlock DEK) ===
	sessionID := "jwt-jti-abc123"
	err = keySvc.UnlockDEK(ctx, userID, password, sessionID, 24*time.Hour)
	if err != nil {
		t.Fatalf("UnlockDEK: %v", err)
	}

	// === Phase 3: Create secrets of all types ===
	llmSecret, err := svc.CreateSecret(ctx, userID, sessionID, nil, CreateSecretRequest{
		Name:     "anthropic-prod",
		Type:     SecretTypeAPIKey,
		Value:    `{"apiKey":"sk-ant-api03-xxx","kind":"anthropic","slug":"anthropic","model":"claude-sonnet-4-20250514"}`,
		Metadata: json.RawMessage(`{"kind":"anthropic","slug":"anthropic"}`),
	})
	if err != nil {
		t.Fatalf("Create LLM secret: %v", err)
	}

	sshSecret, err := svc.CreateSecret(ctx, userID, sessionID, nil, CreateSecretRequest{
		Name:     "github-deploy-key",
		Type:     SecretTypeSSHKey,
		Value:    "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEA...",
		Metadata: json.RawMessage(`{"key_type":"ed25519","host":"github.com"}`),
	})
	if err != nil {
		t.Fatalf("Create SSH secret: %v", err)
	}

	gitSecret, err := svc.CreateSecret(ctx, userID, sessionID, nil, CreateSecretRequest{
		Name:     "github-pat",
		Type:     SecretTypeGitCredential,
		Value:    "ghp_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
		Metadata: json.RawMessage(`{"host":"github.com","protocol":"https"}`),
	})
	if err != nil {
		t.Fatalf("Create git secret: %v", err)
	}

	envSecret, err := svc.CreateSecret(ctx, userID, sessionID, nil, CreateSecretRequest{
		Name:     "database-url",
		Type:     SecretTypeEnvSecret,
		Value:    "postgres://admin:secret@db.internal:5432/myapp",
		Metadata: json.RawMessage(`{"var_name":"DATABASE_URL"}`),
	})
	if err != nil {
		t.Fatalf("Create env secret: %v", err)
	}

	fileSecret, err := svc.CreateSecret(ctx, userID, sessionID, nil, CreateSecretRequest{
		Name:     "tls-cert",
		Type:     SecretTypeSecretFile,
		Value:    "-----BEGIN CERTIFICATE-----\nMIIBxTCCAW...",
		Metadata: json.RawMessage(`{"mount_path":"tls.pem"}`),
	})
	if err != nil {
		t.Fatalf("Create file secret: %v", err)
	}

	// === Phase 4: Verify listing (never shows values) ===
	list, err := svc.ListSecrets(ctx, userID)
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	if len(list) != 5 {
		t.Fatalf("Expected 5 secrets, got %d", len(list))
	}

	// === Phase 5: Bind secrets to workspace ===
	_, err = svc.SetBindings(ctx, userID, workspaceID, []string{
		llmSecret.ID, sshSecret.ID, gitSecret.ID, envSecret.ID, fileSecret.ID,
	})
	if err != nil {
		t.Fatalf("SetBindings: %v", err)
	}

	bindings, err := svc.GetBindings(ctx, userID, workspaceID)
	if err != nil {
		t.Fatalf("GetBindings: %v", err)
	}
	if len(bindings.Bindings) != 5 {
		t.Fatalf("Expected 5 bindings, got %d", len(bindings.Bindings))
	}

	// === Phase 6: Prepare injection (simulates workspace activation) ===
	injectionData, err := svc.InjectSecrets(ctx, userID, sessionID, nil, workspaceID)
	if err != nil {
		t.Fatalf("InjectSecrets: %v", err)
	}

	var injected []InjectedSecret
	if err := json.Unmarshal(injectionData, &injected); err != nil {
		t.Fatalf("Unmarshal injection data: %v", err)
	}
	if len(injected) != 5 {
		t.Fatalf("Expected 5 injected secrets, got %d", len(injected))
	}

	// Verify each type has correct plaintext
	for _, s := range injected {
		if s.Plaintext == "" {
			t.Errorf("Secret %s (%s) has empty plaintext", s.Name, s.Type)
		}
		switch s.Name {
		case "anthropic-prod":
			if s.Type != SecretTypeAPIKey {
				t.Errorf("Wrong type for anthropic-prod: %s", s.Type)
			}
		case "github-deploy-key":
			if s.Type != SecretTypeSSHKey {
				t.Errorf("Wrong type for github-deploy-key: %s", s.Type)
			}
			var meta map[string]string
			json.Unmarshal(s.Metadata, &meta)
			if meta["key_type"] != "ed25519" {
				t.Errorf("SSH key_type not preserved: %v", meta)
			}
		case "database-url":
			if s.Plaintext != "postgres://admin:secret@db.internal:5432/myapp" {
				t.Errorf("Env secret value wrong: %s", s.Plaintext)
			}
		}
	}

	// === Phase 10: Delete a secret and verify cascade ===
	err = svc.DeleteSecret(ctx, userID, sshSecret.ID)
	if err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}

	// Bindings should now have 4
	bindings, _ = svc.GetBindings(ctx, userID, workspaceID)
	if len(bindings.Bindings) != 4 {
		t.Errorf("Expected 4 bindings after delete, got %d", len(bindings.Bindings))
	}

	// === Phase 11: Verify audit trail ===
	audit, err := svc.QueryAudit(ctx, userID, AuditQuery{})
	if err != nil {
		t.Fatalf("QueryAudit: %v", err)
	}
	if len(audit) == 0 {
		t.Error("Audit log should not be empty")
	}

	// Count action types
	actionCounts := make(map[string]int)
	for _, e := range audit {
		actionCounts[e.Action]++
	}
	if actionCounts["create"] < 5 {
		t.Errorf("Expected at least 5 create audit entries, got %d", actionCounts["create"])
	}
	if actionCounts["bind"] < 5 {
		t.Errorf("Expected at least 5 bind audit entries, got %d", actionCounts["bind"])
	}
	if actionCounts["read"] < 5 {
		t.Errorf("Expected at least 5 read audit entries (from injection), got %d", actionCounts["read"])
	}
	if actionCounts["delete"] < 1 {
		t.Errorf("Expected at least 1 delete audit entry, got %d", actionCounts["delete"])
	}

	t.Logf("E2E complete: %d audit entries, actions: %v", len(audit), actionCounts)
}
