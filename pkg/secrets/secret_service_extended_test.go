// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSecretService_CreateSecret_AllTypes(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	tests := []struct {
		name     string
		req      CreateSecretRequest
		wantErr  bool
		errMatch string
	}{
		{
			name: "api-key with metadata",
			req: CreateSecretRequest{
				Name: "openai", Type: SecretTypeAPIKey, Value: "sk-123",
				Metadata: json.RawMessage(`{"kind":"openai","slug":"openai","model":"gpt-4"}`),
			},
		},
		{
			name: "api-key without metadata (optional)",
			req: CreateSecretRequest{
				Name: "bare-llm", Type: SecretTypeAPIKey, Value: "sk-456",
			},
		},
		{
			name: "ssh-key valid",
			req: CreateSecretRequest{
				Name: "gh-ssh", Type: SecretTypeSSHKey, Value: "-----BEGIN OPENSSH PRIVATE KEY-----",
				Metadata: json.RawMessage(`{"key_type":"ed25519","host":"github.com"}`),
			},
		},
		{
			name: "ssh-key missing key_type",
			req: CreateSecretRequest{
				Name: "bad-ssh", Type: SecretTypeSSHKey, Value: "key",
				Metadata: json.RawMessage(`{"host":"github.com"}`),
			},
			wantErr: true, errMatch: "key_type",
		},
		{
			name: "git-credential valid",
			req: CreateSecretRequest{
				Name: "gh-pat", Type: SecretTypeGitCredential, Value: "ghp_xxxx",
				Metadata: json.RawMessage(`{"host":"github.com","protocol":"https"}`),
			},
		},
		{
			name: "git-credential no metadata (optional)",
			req: CreateSecretRequest{
				Name: "git-bare", Type: SecretTypeGitCredential, Value: "token",
			},
		},
		{
			name: "secret-file valid",
			req: CreateSecretRequest{
				Name: "cert", Type: SecretTypeSecretFile, Value: "cert-pem-data",
				Metadata: json.RawMessage(`{"mount_path":"cert.pem"}`),
			},
		},
		{
			name: "secret-file missing mount_path",
			req: CreateSecretRequest{
				Name: "bad-file", Type: SecretTypeSecretFile, Value: "data",
				Metadata: json.RawMessage(`{"description":"a file"}`),
			},
			wantErr: true, errMatch: "mount_path",
		},
		{
			name: "env-secret valid",
			req: CreateSecretRequest{
				Name: "db-url", Type: SecretTypeEnvSecret, Value: "postgres://...",
				Metadata: json.RawMessage(`{"var_name":"DATABASE_URL"}`),
			},
		},
		{
			name: "env-secret missing var_name",
			req: CreateSecretRequest{
				Name: "bad-env", Type: SecretTypeEnvSecret, Value: "val",
				Metadata: json.RawMessage(`{"description":"env"}`),
			},
			wantErr: true, errMatch: "var_name",
		},
		{
			name: "invalid metadata JSON",
			req: CreateSecretRequest{
				Name: "bad-json", Type: SecretTypeSSHKey, Value: "key",
				Metadata: json.RawMessage(`not valid json`),
			},
			wantErr: true, errMatch: "invalid metadata",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := svc.CreateSecret(ctx, "user-1", sessionID, nil, tt.req)
			if tt.wantErr {
				if err == nil {
					t.Error("Expected error, got nil")
				} else if tt.errMatch != "" && !strings.Contains(err.Error(), tt.errMatch) {
					t.Errorf("Error %q should contain %q", err.Error(), tt.errMatch)
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
				if resp == nil {
					t.Error("Expected non-nil response")
				}
			}
		})
	}
}

func TestSecretService_ConcurrentAccess(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	// Create 10 secrets concurrently
	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			name := "concurrent-" + string(rune('a'+idx))
			_, errs[idx] = svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
				Name: name, Type: SecretTypeAPIKey, Value: "val",
				Metadata: json.RawMessage(`{"kind":"x","slug":"x"}`),
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("Concurrent create %d failed: %v", i, err)
		}
	}

	// List should return all 10
	list, err := svc.ListSecrets(ctx, "user-1")
	if err != nil {
		t.Fatalf("ListSecrets failed: %v", err)
	}
	if len(list) != 10 {
		t.Errorf("Expected 10 secrets, got %d", len(list))
	}
}

func TestSecretService_EncryptionIsolation(t *testing.T) {
	// Two users should not be able to decrypt each other's secrets
	keyStore := newMockKeyStore()
	dekCache := newMockDEKCache()
	keySvc := NewKeyService(keyStore, dekCache)
	secretStore := newMockSecretStore()
	svc := NewSecretService(keySvc, secretStore)
	ctx := context.Background()

	// Setup user 1
	_ = keySvc.InitializeUserKeysServerKEK(ctx, "user-1", "server_kek")
	_ = keySvc.UnlockDEK(ctx, "user-1", []byte("pw1"), "sess-1", time.Hour)

	// Setup user 2
	_ = keySvc.InitializeUserKeysServerKEK(ctx, "user-2", "server_kek")
	_ = keySvc.UnlockDEK(ctx, "user-2", []byte("pw2"), "sess-2", time.Hour)

	// User 1 creates a secret
	created, err := svc.CreateSecret(ctx, "user-1", "sess-1", nil, CreateSecretRequest{
		Name: "private", Type: SecretTypeAPIKey, Value: "user1-secret-key",
		Metadata: json.RawMessage(`{"kind":"x","slug":"x"}`),
	})
	if err != nil {
		t.Fatalf("CreateSecret failed: %v", err)
	}

	// User 2 should not be able to get user 1's secret
	resp, err := svc.GetSecret(ctx, "user-2", created.ID)
	if resp != nil || err == nil {
		t.Errorf("User 2 should not be able to access user 1's secret (resp=%v, err=%v)", resp, err)
	}

	// User 2 should not be able to decrypt user 1's secret
	_, err = svc.DecryptSecretValue(ctx, "user-2", "sess-2", nil, created.ID)
	if err == nil {
		t.Error("User 2 should not be able to decrypt user 1's secret")
	}
}

func TestSecretService_UpdatePreservesEncryption(t *testing.T) {
	svc, store, sessionID := setupSecretService(t)
	ctx := context.Background()

	created, _ := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "rotate-test", Type: SecretTypeAPIKey, Value: "original",
		Metadata: json.RawMessage(`{"kind":"x","slug":"x"}`),
	})

	// Get ciphertext before update
	secretBefore, _ := store.GetSecret(ctx, "user-1", created.ID)
	ctBefore := make([]byte, len(secretBefore.Ciphertext))
	copy(ctBefore, secretBefore.Ciphertext)

	// Update
	svc.UpdateSecret(ctx, "user-1", sessionID, nil, created.ID, UpdateSecretRequest{Value: "updated"})

	// Ciphertext should be different
	secretAfter, _ := store.GetSecret(ctx, "user-1", created.ID)
	if bytesEqual(ctBefore, secretAfter.Ciphertext) {
		t.Error("Ciphertext should change after update")
	}

	// But decryption should yield new value
	plaintext, _ := svc.DecryptSecretValue(ctx, "user-1", sessionID, nil, created.ID)
	if string(plaintext) != "updated" {
		t.Errorf("Expected 'updated', got '%s'", string(plaintext))
	}
}

func TestSecretService_BindingMultipleWorkspaces(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	// Create one secret
	created, _ := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "shared-key", Type: SecretTypeAPIKey, Value: "v",
		Metadata: json.RawMessage(`{"kind":"x","slug":"x"}`),
	})

	// Bind to multiple workspaces
	_, _ = svc.SetBindings(ctx, "user-1", "ws-1", []string{created.ID})
	_, _ = svc.SetBindings(ctx, "user-1", "ws-2", []string{created.ID})
	_, _ = svc.SetBindings(ctx, "user-1", "ws-3", []string{created.ID})

	// Each workspace should see the binding
	for _, wsID := range []string{"ws-1", "ws-2", "ws-3"} {
		resp, err := svc.GetBindings(ctx, "user-1", wsID)
		if err != nil {
			t.Fatalf("GetBindings(%s) failed: %v", wsID, err)
		}
		if len(resp.Bindings) != 1 {
			t.Errorf("Workspace %s: expected 1 binding, got %d", wsID, len(resp.Bindings))
		}
	}
}

func TestSecretService_RebindReplacesExisting(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	s1, _ := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "key-1", Type: SecretTypeAPIKey, Value: "v1",
		Metadata: json.RawMessage(`{"kind":"a","slug":"a"}`),
	})
	s2, _ := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "key-2", Type: SecretTypeAPIKey, Value: "v2",
		Metadata: json.RawMessage(`{"kind":"b","slug":"b"}`),
	})

	// Bind both
	_, _ = svc.SetBindings(ctx, "user-1", "ws-1", []string{s1.ID, s2.ID})

	// Rebind with only s2
	_, _ = svc.SetBindings(ctx, "user-1", "ws-1", []string{s2.ID})

	resp, _ := svc.GetBindings(ctx, "user-1", "ws-1")
	if len(resp.Bindings) != 1 {
		t.Errorf("Expected 1 binding after rebind, got %d", len(resp.Bindings))
	}
	if resp.Bindings[0].SecretID != s2.ID {
		t.Errorf("Expected binding to s2, got %s", resp.Bindings[0].SecretID)
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
