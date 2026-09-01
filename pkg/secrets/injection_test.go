// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

// injection_test.go — builder-adjacent unit tests that live naturally
// beside the old injector suite: the mixed-fleet render helper, stored-
// metadata preservation, and cross-tenant filtering. The sessionless/
// bootstrap behavioral matrix moved to builder_test.go and
// pod_bootstrap_injector_test.go; the precedence matrix lives in
// credential_precedence_test.go.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// injectJSONChecked is injectJSON for tests that handle the error
// themselves (kept distinct so the require-based helper stays
// assertion-clean).
func injectJSONChecked(svc *SecretService, ctx context.Context, userID, workspaceID string) ([]byte, error) {
	batch, _, err := svc.BuildWorkspaceBatch(ctx, userID, workspaceID)
	if err != nil {
		return nil, err
	}
	return LegacyBatchJSON(*batch), nil
}

// setupSecretServiceWithTwoUsers creates a service with two users for isolation tests
func setupSecretServiceWithTwoUsers(t *testing.T) (*SecretService, string, string) {
	t.Helper()
	keyStore := newMockKeyStore()
	dekCache := newMockDEKCache()
	keySvc := NewKeyService(keyStore, dekCache)
	keySvc.SetAPIKeyStore(nil, &recordingProvider{})
	secretStore := newMockSecretStore()
	svc := NewSecretService(keySvc, &builderTestStore{
		SecretStore:       secretStore,
		CredentialStore:   &mockCredentialStore{},
		fakeRevisionStore: &fakeRevisionStore{},
	})
	ctx := context.Background()

	_ = keySvc.InitializeUserKeysServerKEK(ctx, "user-1", "server_kek")
	_ = keySvc.UnlockDEK(ctx, "user-1", []byte("pw1"), "sess-1", time.Hour)

	_ = keySvc.InitializeUserKeysServerKEK(ctx, "user-2", "server_kek")
	_ = keySvc.UnlockDEK(ctx, "user-2", []byte("pw2"), "sess-2", time.Hour)

	return svc, "sess-1", "sess-2"
}

// TestBuildWorkspaceBatch_PreservesStoredMetadata pins that a
// secret-file's stored metadata (mount_path) survives the batch round
// trip untouched — stored content, never derived.
func TestBuildWorkspaceBatch_PreservesStoredMetadata(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	s1, err := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name:     "file-secret",
		Type:     SecretTypeSecretFile,
		Value:    "cert-content",
		Metadata: json.RawMessage(`{"mount_path":"cert.pem"}`),
	})
	require.NoError(t, err)
	_, err = svc.SetBindings(ctx, "user-1", "ws-1", []string{s1.ID})
	require.NoError(t, err)

	batch, degrade, err := svc.BuildWorkspaceBatch(ctx, "user-1", "ws-1")
	require.NoError(t, err)
	require.Nil(t, degrade)
	require.Len(t, batch.Entries, 1)

	var meta map[string]any
	require.NoError(t, json.Unmarshal(batch.Entries[0].Metadata, &meta))
	assert.Equal(t, "cert.pem", meta["mount_path"])
}

// TestBuildWorkspaceBatch_CrossTenantIsolation: the builder filters
// user_secrets rows to the workspace owner — a different owner building
// the same workspace gets none of the first owner's decrypted secrets.
func TestBuildWorkspaceBatch_CrossTenantIsolation(t *testing.T) {
	svc, sess1, _ := setupSecretServiceWithTwoUsers(t)
	ctx := context.Background()

	s1, err := svc.CreateSecret(ctx, "user-1", sess1, nil, CreateSecretRequest{
		Name:     "private",
		Type:     SecretTypeAPIKey,
		Value:    "user1-key",
		Metadata: json.RawMessage(`{"kind":"x","slug":"x"}`),
	})
	require.NoError(t, err)
	_, err = svc.SetBindings(ctx, "user-1", "ws-user1", []string{s1.ID})
	require.NoError(t, err)

	// user-2 builds the same workspace: the binding rows exist but the
	// secrets belong to user-1, so user-2's batch carries none of them.
	batch, _, err := svc.BuildWorkspaceBatch(ctx, "user-2", "ws-user1")
	require.NoError(t, err)
	for _, e := range batch.Entries {
		assert.NotEqual(t, "user1-key", e.Value, "user-2 must not see user-1's decrypted secrets")
	}
	assert.Empty(t, batch.Entries)
}
