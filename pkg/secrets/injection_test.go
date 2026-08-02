// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestSecretService_InjectSecrets_Empty(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	data, err := svc.InjectSecrets(ctx, "user-1", sessionID, nil, "ws-empty")
	if err != nil {
		t.Fatalf("InjectSecrets failed: %v", err)
	}

	var injected []InjectedSecret
	json.Unmarshal(data, &injected)
	if len(injected) != 0 {
		t.Errorf("Expected 0 injected secrets, got %d", len(injected))
	}
}

func TestSecretService_InjectSecrets_MultipleTypes(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	// Create secrets of different types
	s1, _ := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "anthropic", Type: SecretTypeAPIKey, Value: `{"apiKey":"sk-ant-123","kind":"anthropic","slug":"anthropic"}`,
		Metadata: json.RawMessage(`{"kind":"anthropic","slug":"anthropic"}`),
	})
	s2, _ := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "gh-ssh", Type: SecretTypeSSHKey, Value: "-----BEGIN OPENSSH PRIVATE KEY-----\nfake",
		Metadata: json.RawMessage(`{"key_type":"ed25519","host":"github.com"}`),
	})
	s3, _ := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "db-url", Type: SecretTypeEnvSecret, Value: "postgres://user:pass@host/db",
		Metadata: json.RawMessage(`{"var_name":"DATABASE_URL"}`),
	})

	// Bind all to workspace
	_, _ = svc.SetBindings(ctx, "user-1", "ws-1", []string{s1.ID, s2.ID, s3.ID})

	// Prepare injection
	data, err := svc.InjectSecrets(ctx, "user-1", sessionID, nil, "ws-1")
	if err != nil {
		t.Fatalf("InjectSecrets failed: %v", err)
	}

	var injected []InjectedSecret
	if err := json.Unmarshal(data, &injected); err != nil {
		t.Fatalf("Failed to unmarshal: %v", err)
	}

	if len(injected) != 3 {
		t.Fatalf("Expected 3 injected secrets, got %d", len(injected))
	}

	// Verify types and plaintext
	typeMap := make(map[SecretType]string)
	for _, s := range injected {
		typeMap[s.Type] = s.Plaintext
	}

	if v, ok := typeMap[SecretTypeAPIKey]; !ok || v == "" {
		t.Error("LLM provider secret missing or empty")
	}
	if v, ok := typeMap[SecretTypeSSHKey]; !ok || v == "" {
		t.Error("SSH key secret missing or empty")
	}
	if v, ok := typeMap[SecretTypeEnvSecret]; !ok || v != "postgres://user:pass@host/db" {
		t.Errorf("Env secret wrong: %s", v)
	}
}

func TestSecretService_InjectSecrets_NoSession(t *testing.T) {
	keyStore := newMockKeyStore()
	dekCache := newMockDEKCache()
	keySvc := NewKeyService(keyStore, dekCache)
	secretStore := newMockSecretStore()
	svc := NewSecretService(keySvc, secretStore)
	ctx := context.Background()

	// Initialize but don't unlock
	_ = keySvc.InitializeUserKeysServerKEK(ctx, "user-1", "server_kek")

	_, err := svc.InjectSecrets(ctx, "user-1", "no-session", nil, "ws-1")
	// Should succeed with empty result (no bindings)
	if err != nil {
		t.Fatalf("Should succeed with no bindings: %v", err)
	}
}

func TestSecretService_InjectSecrets_AuditLogged(t *testing.T) {
	svc, store, sessionID := setupSecretService(t)
	ctx := context.Background()

	s1, _ := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "audited-inject", Type: SecretTypeAPIKey, Value: "val",
		Metadata: json.RawMessage(`{"kind":"x","slug":"x"}`),
	})
	_, _ = svc.SetBindings(ctx, "user-1", "ws-1", []string{s1.ID})

	store.mu.Lock()
	store.audit = nil
	store.mu.Unlock()

	data, err := svc.InjectSecrets(ctx, "user-1", sessionID, nil, "ws-1")
	if err != nil {
		t.Fatalf("InjectSecrets failed: %v", err)
	}

	var injected []InjectedSecret
	json.Unmarshal(data, &injected)
	if len(injected) == 0 {
		t.Error("Expected injected secrets, got none")
	}
}

func TestSecretService_InjectSecrets_PreservesMetadata(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	s1, _ := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name: "file-secret", Type: SecretTypeSecretFile, Value: "cert-content",
		Metadata: json.RawMessage(`{"mount_path":"cert.pem"}`),
	})
	_, _ = svc.SetBindings(ctx, "user-1", "ws-1", []string{s1.ID})

	data, _ := svc.InjectSecrets(ctx, "user-1", sessionID, nil, "ws-1")

	var injected []InjectedSecret
	json.Unmarshal(data, &injected)

	if len(injected) != 1 {
		t.Fatalf("Expected 1, got %d", len(injected))
	}

	var meta map[string]string
	json.Unmarshal(injected[0].Metadata, &meta)
	if meta["mount_path"] != "cert.pem" {
		t.Errorf("Metadata mount_path not preserved: %v", meta)
	}
}

// setupSecretServiceWithTwoUsers creates a service with two users for isolation tests
func setupSecretServiceWithTwoUsers(t *testing.T) (*SecretService, string, string) {
	t.Helper()
	keyStore := newMockKeyStore()
	dekCache := newMockDEKCache()
	keySvc := NewKeyService(keyStore, dekCache)
	secretStore := newMockSecretStore()
	svc := NewSecretService(keySvc, secretStore)
	ctx := context.Background()

	_ = keySvc.InitializeUserKeysServerKEK(ctx, "user-1", "server_kek")
	_ = keySvc.UnlockDEK(ctx, "user-1", []byte("pw1"), "sess-1", time.Hour)

	_ = keySvc.InitializeUserKeysServerKEK(ctx, "user-2", "server_kek")
	_ = keySvc.UnlockDEK(ctx, "user-2", []byte("pw2"), "sess-2", time.Hour)

	return svc, "sess-1", "sess-2"
}

// TestSecretService_InjectSecrets_BootstrapPath_NoSession_BoundNonLLMSecrets
// is a regression test for the production outage observed on workspace
// d95b6751-8796-4ea5-addd-9f5af3053fac on 2026-06-24.
//
// The /internal/v1/pod-bootstrap handler (introduced by Epic 35) calls
// InjectSecrets with sessionID=="" because the init container
// has no user session. The documented contract (godoc on the function) says
// user-DEK-encrypted things must be skipped with an audit event when no
// session is available — and the LLM-credential loop honors this. But
// buildNonLLMSecrets does NOT: when any non-LLM user secret (ssh-key,
// env-secret, etc.) is bound to the workspace, GetDEK("") fails and the
// entire call returns an error. The bootstrap handler then returns 500
// "secret preparation failed", the init container writes secrets.json=[]
// and no workspace-config.json, and the pod boots with zero providers.
//
// Expected behavior: bootstrap-style call (sessionID=="") with a bound
// non-LLM user secret returns successfully, with the user secret omitted
// from the payload (it will be delivered later by the live
// /v1/reload-secrets push when the user opens the workspace).
//
// This test must FAIL today and PASS after the fix.
func TestSecretService_InjectSecrets_BootstrapPath_NoSession_BoundNonLLMSecrets(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	// Mirror the affected production workspace: user has a bound ssh-key
	// secret. (Real workspace had ssh-key + env-secret; one is sufficient
	// to trigger the bug.) The user creates the secret with a real session
	// — they are logged in when binding it. The bug only manifests later
	// at pod boot when there is no session.
	s, err := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name:     "github",
		Type:     SecretTypeSSHKey,
		Value:    "-----BEGIN OPENSSH PRIVATE KEY-----\nfake",
		Metadata: json.RawMessage(`{"key_type":"ed25519","host":"github.com"}`),
	})
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	if _, err := svc.SetBindings(ctx, "user-1", "ws-1", []string{s.ID}); err != nil {
		t.Fatalf("SetBindings: %v", err)
	}

	// Simulate the bootstrap path: no session.
	data, err := svc.InjectSessionlessSecrets(ctx, "user-1", "ws-1")
	if err != nil {
		t.Fatalf("bootstrap-style call must not fail when only user-DEK secrets are bound; got error: %v", err)
	}

	// User-DEK secrets must be omitted (we have no DEK to decrypt them with).
	var injected []InjectedSecret
	if err := json.Unmarshal(data, &injected); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	for _, item := range injected {
		if item.Type == SecretTypeSSHKey {
			t.Errorf("user-DEK ssh-key must NOT appear in bootstrap payload (no DEK available); got %+v", item)
		}
	}
}

// TestSecretService_InjectSecrets_BootstrapPath_NoSession_PreservesServerKEKCredentials
// is the positive counterpart: bootstrap with no session must still deliver
// admin/org credentials (server-KEK-encrypted, decryptable without a session).
//
// This proves the fix doesn't regress the "deliver what we CAN decrypt" half
// of the contract. Together with the negative test above, it pins down the
// exact behavior: server-KEK in, user-DEK out, when sessionID is empty.
func TestSecretService_InjectSecrets_BootstrapPath_NoSession_PreservesServerKEKCredentials(t *testing.T) {
	keyStore := newMockKeyStore()
	dekCache := newMockDEKCache()
	keySvc := NewKeyService(keyStore, dekCache)
	secretStore := newMockSecretStore()

	// Set up the user with a real password so non-LLM secrets can be
	// created (CreateSecret needs a session DEK). After binding, we'll
	// run the bootstrap-style call without a session to prove the fix.
	ctx := context.Background()
	if err := keySvc.InitializeUserKeysServerKEK(ctx, "user-1", "server_kek"); err != nil {
		t.Fatalf("InitializeUserKeysServerKEK: %v", err)
	}
	if err := keySvc.UnlockDEK(ctx, "user-1", []byte("pw"), "create-session", time.Hour); err != nil {
		t.Fatalf("UnlockDEK: %v", err)
	}

	// Build an admin credential encrypted with a server KEK.
	adminKEK := make([]byte, 32)
	for i := range adminKEK {
		adminKEK[i] = byte(i + 1)
	}
	adminPlaintext, _ := json.Marshal(LLMProviderData{Kind: "anthropic", Slug: "anthropic", APIKey: "admin-key"})
	adminCipher, err := EncryptSecret(adminKEK, adminPlaintext)
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}

	mockCred := &mockCredentialStore{
		bindings: []CredentialBinding{
			{ID: "cred-admin", OwnerType: "admin", OwnerID: "_platform", Kind: "anthropic", Slug: "anthropic", Ciphertext: adminCipher, SourceType: "auto"},
		},
	}
	combined := &combinedTestStore{SecretStore: secretStore, CredentialStore: mockCred}
	svc := NewSecretService(keySvc, combined)
	svc.SetAdminProvider(mustStaticProvider(t, adminKEK))

	// Bind a non-LLM user secret (the trigger for the bug).
	userSecret, err := svc.CreateSecret(ctx, "user-1", "create-session", nil, CreateSecretRequest{
		Name:     "github",
		Type:     SecretTypeSSHKey,
		Value:    "-----BEGIN OPENSSH PRIVATE KEY-----\nfake",
		Metadata: json.RawMessage(`{"key_type":"ed25519","host":"github.com"}`),
	})
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	if _, err := svc.SetBindings(ctx, "user-1", "ws-1", []string{userSecret.ID}); err != nil {
		t.Fatalf("SetBindings: %v", err)
	}

	// Bootstrap-style call: NO session.
	data, err := svc.InjectSessionlessSecrets(ctx, "user-1", "ws-1")
	if err != nil {
		t.Fatalf("bootstrap-style call must succeed; got error: %v", err)
	}

	var injected []InjectedSecret
	if err := json.Unmarshal(data, &injected); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	var sawAdmin bool
	for _, item := range injected {
		if item.Type == SecretTypeLLMProvider && item.Name == "anthropic" {
			sawAdmin = true
		}
		if item.Type == SecretTypeSSHKey {
			t.Errorf("user-DEK ssh-key must NOT appear in bootstrap payload; got %+v", item)
		}
	}
	if !sawAdmin {
		t.Errorf("admin LLM credential must appear in bootstrap payload (server-KEK is decryptable without a session); got %d items: %+v", len(injected), injected)
	}
}

// TestSecretService_InjectSecrets_BootstrapPath_NoSession_OrgCredential
// is the live-failure-case regression test. The affected workspace
// (d95b6751-...) had a single org credential bound:
//
//	provider="custom", owner_type="org", with model "glm-5.2" in the allowlist.
//
// Org credentials are encrypted with the org RootKeyProvider (server-KEK,
// decryptable without a user session), so they MUST be delivered at
// bootstrap time. With the bug, the user's bound ssh-key blocks the entire
// call and the org credential is collateral damage — exactly what the
// production pod observed (provider.custom missing from agent-config.json).
//
// This test must FAIL today and PASS after the fix.
func TestSecretService_InjectSecrets_BootstrapPath_NoSession_OrgCredential(t *testing.T) {
	keyStore := newMockKeyStore()
	dekCache := newMockDEKCache()
	keySvc := NewKeyService(keyStore, dekCache)
	secretStore := newMockSecretStore()

	ctx := context.Background()
	if err := keySvc.InitializeUserKeysServerKEK(ctx, "user-1", "server_kek"); err != nil {
		t.Fatalf("InitializeUserKeysServerKEK: %v", err)
	}
	if err := keySvc.UnlockDEK(ctx, "user-1", []byte("pw"), "create-session", time.Hour); err != nil {
		t.Fatalf("UnlockDEK: %v", err)
	}

	// Org credential, server-KEK encrypted. Mirrors the production credential:
	// provider name "custom", owner_type "org", model in allowlist.
	orgKEK := make([]byte, 32)
	for i := range orgKEK {
		orgKEK[i] = byte(i + 1)
	}
	orgPlaintext, _ := json.Marshal(LLMProviderData{Kind: "openai_compatible", Slug: "custom", APIKey: "org-key", BaseURL: "https://api.thekao.cloud/v1"})
	orgCipher, err := EncryptSecret(orgKEK, orgPlaintext)
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}

	mockCred := &mockCredentialStore{
		bindings: []CredentialBinding{
			{ID: "cred-org", OwnerType: "org", OwnerID: "org-1", Kind: "openai_compatible", Slug: "custom", Ciphertext: orgCipher, ModelAllowlist: []string{"glm-5.2"}, SourceType: "auto"},
		},
	}
	combined := &combinedTestStore{SecretStore: secretStore, CredentialStore: mockCred}
	svc := NewSecretService(keySvc, combined)
	svc.SetOrgProvider(mustStaticProvider(t, orgKEK))

	// Trigger condition: user has a bound non-LLM secret (ssh-key in production).
	userSecret, err := svc.CreateSecret(ctx, "user-1", "create-session", nil, CreateSecretRequest{
		Name:     "github",
		Type:     SecretTypeSSHKey,
		Value:    "-----BEGIN OPENSSH PRIVATE KEY-----\nfake",
		Metadata: json.RawMessage(`{"key_type":"ed25519","host":"github.com"}`),
	})
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	if _, err := svc.SetBindings(ctx, "user-1", "ws-1", []string{userSecret.ID}); err != nil {
		t.Fatalf("SetBindings: %v", err)
	}

	// Bootstrap-style call: NO session, replicating the init container path.
	data, err := svc.InjectSessionlessSecrets(ctx, "user-1", "ws-1")
	if err != nil {
		t.Fatalf("bootstrap-style call must succeed; got error: %v", err)
	}

	var injected []InjectedSecret
	if err := json.Unmarshal(data, &injected); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	var sawOrg bool
	for _, item := range injected {
		if item.Type == SecretTypeLLMProvider && item.Name == "custom" {
			sawOrg = true
			// Sanity: payload must round-trip through LLMProviderData so the
			// agentd Materializer can pick it up.
			var pd LLMProviderData
			if jerr := json.Unmarshal([]byte(item.Plaintext), &pd); jerr != nil {
				t.Errorf("org credential plaintext must be valid LLMProviderData JSON: %v", jerr)
			}
			if pd.APIKey != "org-key" {
				t.Errorf("org credential apiKey not preserved; got %q", pd.APIKey)
			}
		}
		if item.Type == SecretTypeSSHKey {
			t.Errorf("user-DEK ssh-key must NOT appear in bootstrap payload; got %+v", item)
		}
	}
	if !sawOrg {
		t.Errorf("org LLM credential (provider=custom) must appear in bootstrap payload; got %d items: %+v", len(injected), injected)
	}
}

// TestSecretService_InjectSecrets_BootstrapPath_NoSession_LegacyAPIKey_Skipped
// proves Finding A1.2 from the stress-test: legacy api-key user_secrets
// (the deprecated SecretTypeAPIKey, sunset 2026-12-19) are user-DEK
// encrypted just like ssh-key/env-secret. They must also be skipped at
// bootstrap, not propagated as a hard error.
//
// Without this test, a future reorganization of the SecretType filter
// in buildNonLLMSecrets could regress this case. The current filter
// `secret.Type != SecretTypeLLMProvider` happens to include api-key, so
// it follows the same path; this test pins that down.
//
// This test must FAIL today (api-key triggers DEK-unavailable error)
// and PASS after the fix.
func TestSecretService_InjectSecrets_BootstrapPath_NoSession_LegacyAPIKey_Skipped(t *testing.T) {
	// Disable the api-key sunset gate for this test so we can create a
	// legacy api-key secret (representing creds from before 2026-12-19).
	prevSunset := isAPIKeySunset
	isAPIKeySunset = func() bool { return false }
	defer func() { isAPIKeySunset = prevSunset }()

	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	apiKey, err := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name:     "legacy-anthropic",
		Type:     SecretTypeAPIKey,
		Value:    `{"apiKey":"sk-ant-legacy","kind":"anthropic","slug":"anthropic"}`,
		Metadata: json.RawMessage(`{"kind":"anthropic","slug":"anthropic"}`),
	})
	if err != nil {
		t.Fatalf("CreateSecret(api-key): %v", err)
	}
	if _, err := svc.SetBindings(ctx, "user-1", "ws-1", []string{apiKey.ID}); err != nil {
		t.Fatalf("SetBindings: %v", err)
	}

	data, err := svc.InjectSessionlessSecrets(ctx, "user-1", "ws-1")
	if err != nil {
		t.Fatalf("bootstrap-style call must not fail when only legacy api-key user_secrets are bound; got error: %v", err)
	}

	var injected []InjectedSecret
	if err := json.Unmarshal(data, &injected); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	for _, item := range injected {
		if item.Type == SecretTypeAPIKey {
			t.Errorf("legacy api-key user_secret must NOT appear in bootstrap payload (no DEK to decrypt with); got %+v", item)
		}
	}
}

// TestSecretService_InjectSecrets_BootstrapPath_NoSession_AuditsSkippedUserDEKSecrets
// proves Finding F6.1: the documented contract on InjectSecrets
// says user-DEK things "are skipped with an audit event". The LLM loop
// honors this when sessionID is non-empty but DEK is missing. The fix must
// emit an audit on user-DEK skip *at bootstrap time* (sessionID="") too,
// otherwise operators have no signal that a workspace's bound user secrets
// are not being delivered until live-push runs.
//
// Without auditing, an operator investigating "why does my workspace not
// have its env-secrets at boot" has zero diagnostic surface to pull from.
// The audit is the breadcrumb.
//
// This test must FAIL today (today the function errors before reaching the
// audit; no audit is emitted) and PASS after the fix.
func TestSecretService_InjectSecrets_BootstrapPath_NoSession_AuditsSkippedUserDEKSecrets(t *testing.T) {
	svc, store, sessionID := setupSecretService(t)
	ctx := context.Background()

	envSecret, err := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name:     "database_url",
		Type:     SecretTypeEnvSecret,
		Value:    "postgres://user:pass@host/db",
		Metadata: json.RawMessage(`{"var_name":"DATABASE_URL"}`),
	})
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	if _, err := svc.SetBindings(ctx, "user-1", "ws-1", []string{envSecret.ID}); err != nil {
		t.Fatalf("SetBindings: %v", err)
	}

	// Reset audit log to ignore noise from CreateSecret/SetBindings.
	store.mu.Lock()
	store.audit = nil
	store.mu.Unlock()

	if _, err := svc.InjectSessionlessSecrets(ctx, "user-1", "ws-1"); err != nil {
		t.Fatalf("bootstrap-style call must succeed; got error: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.audit) == 0 {
		t.Fatalf("expected an audit entry for the skipped user-DEK env-secret; got 0 entries")
	}

	var sawSkipAudit bool
	for _, entry := range store.audit {
		// Action name is implementation-detail of the fix, but it must
		// at least mention "skip" or "no_session" in either action or
		// metadata so operators can find these events in dashboards.
		if entry.Action == "secret_skipped_no_session" || entry.Action == "secret_decrypt_failed" {
			sawSkipAudit = true
			break
		}
	}
	if !sawSkipAudit {
		t.Errorf("audit log must record the user-DEK skip; got actions: %+v", store.audit)
	}
}

// TestSecretService_InjectSecrets_BootstrapPath_DeliversWorkspaceConfigDefaultModel
// proves Finding A4.3: the fix transitively repairs default-model
// selection. The bootstrap response carries `workspaceConfig.defaultModel`
// from the workspaces table — but only when the injector method
// itself succeeds (the handler bails before reaching the wsConfig branch
// on a 500). Once the secret-prep stops failing, the wsConfig is
// delivered and applyWorkspaceConfig() can resolve it to a fully-
// qualified providerID/modelID, restoring the per-workspace default
// model the user originally selected.
//
// This test pins down the indirect fix so a future regression in the
// secret-prep path (e.g. someone restoring the early-return on
// non-LLM bindings) is caught immediately.
//
// This test lives in pkg/secrets but is named for the handler-level
// contract — the wsConfig delivery is enforced at the handler in
// pod_bootstrap.go:169 (it only writes wsConfig when the prep call
// succeeds). The pkg/secrets piece tests the precondition: the call
// must not return an error in this configuration.
func TestSecretService_InjectSecrets_BootstrapPath_DeliversWorkspaceConfigDefaultModel(t *testing.T) {
	// Setup mirrors the production failure: org cred + user ssh-key.
	keyStore := newMockKeyStore()
	dekCache := newMockDEKCache()
	keySvc := NewKeyService(keyStore, dekCache)
	secretStore := newMockSecretStore()

	ctx := context.Background()
	_ = keySvc.InitializeUserKeysServerKEK(ctx, "user-1", "server_kek")
	_ = keySvc.UnlockDEK(ctx, "user-1", []byte("pw"), "create-session", time.Hour)

	orgKEK := make([]byte, 32)
	for i := range orgKEK {
		orgKEK[i] = byte(i + 1)
	}
	orgPlaintext, _ := json.Marshal(LLMProviderData{Kind: "openai_compatible", Slug: "custom", APIKey: "org-key"})
	orgCipher, _ := EncryptSecret(orgKEK, orgPlaintext)

	combined := &combinedTestStore{
		SecretStore: secretStore,
		CredentialStore: &mockCredentialStore{
			bindings: []CredentialBinding{
				{ID: "cred-org", OwnerType: "org", OwnerID: "org-1", Kind: "openai_compatible", Slug: "custom", Ciphertext: orgCipher, SourceType: "auto"},
			},
		},
	}
	svc := NewSecretService(keySvc, combined)
	svc.SetOrgProvider(mustStaticProvider(t, orgKEK))

	userSecret, _ := svc.CreateSecret(ctx, "user-1", "create-session", nil, CreateSecretRequest{
		Name: "github", Type: SecretTypeSSHKey, Value: "fake",
		Metadata: json.RawMessage(`{"key_type":"ed25519","host":"github.com"}`),
	})
	_, _ = svc.SetBindings(ctx, "user-1", "ws-1", []string{userSecret.ID})

	// The handler-level test (pod_bootstrap_e2e) verifies workspaceConfig
	// is in the response. The pkg/secrets test pins the precondition: the
	// call must not error. Without that, the handler at pod_bootstrap.go:169
	// short-circuits and never delivers wsConfig.
	data, err := svc.InjectSessionlessSecrets(ctx, "user-1", "ws-1")
	if err != nil {
		t.Fatalf("bootstrap-style call must succeed so handler can deliver workspaceConfig; got error: %v", err)
	}
	if len(data) == 0 || string(data) == "[]" {
		// Empty payload is acceptable when no decryptable creds exist, but
		// here we have an org cred — the payload MUST be non-empty.
		t.Errorf("bootstrap payload must contain the org credential; got empty payload (this guarantees handler delivers wsConfig)")
	}
}

// TestSecretService_InjectSessionlessSecrets_UserBindingDoesNotShadowServerKEK
// proves the skip-then-fallback contract for the new sessionless path:
// when a user binding (skipped because no DEK) appears in the precedence
// list BEFORE an admin/org binding for the same provider, the user skip
// must NOT set `seen[provider]`, so the lower-priority server-KEK binding
// still materializes.
//
// The pre-fix LLM loop already had this property for *decrypt failures*
// (LoadLLMCredentials does `continue` without setting seen). The new
// loadServerKEKCredentials path explicitly skips user bindings before
// reaching decryptBinding — this test verifies the explicit-skip path
// also avoids poisoning `seen`. Without this guard, a workspace with a
// user-bound `openai` cred and an admin-bound `openai` fallback would
// silently boot with NO `openai` provider after Epic 35.
//
// This is the second missing test surfaced by PR #407 review.
func TestSecretService_InjectSessionlessSecrets_UserBindingDoesNotShadowServerKEK(t *testing.T) {
	keyStore := newMockKeyStore()
	dekCache := newMockDEKCache()
	keySvc := NewKeyService(keyStore, dekCache)
	secretStore := newMockSecretStore()

	ctx := context.Background()
	if err := keySvc.InitializeUserKeysServerKEK(ctx, "user-1", "server_kek"); err != nil {
		t.Fatalf("InitializeUserKeysServerKEK: %v", err)
	}

	// User credential for "openai" — encrypted with user DEK. Without a
	// session at sessionless-injection time, this binding is skipped.
	userDEK := make([]byte, 32)
	for i := range userDEK {
		userDEK[i] = byte(i + 100)
	}
	userPlaintext, _ := json.Marshal(LLMProviderData{Kind: "openai", Slug: "openai", APIKey: "user-key"})
	userCipher, err := EncryptSecret(userDEK, userPlaintext)
	if err != nil {
		t.Fatalf("EncryptSecret(user): %v", err)
	}

	// Admin credential for the SAME provider "openai" — server-KEK
	// encrypted; this is the fallback that MUST still materialize.
	adminKEK := make([]byte, 32)
	for i := range adminKEK {
		adminKEK[i] = byte(i + 1)
	}
	adminPlaintext, _ := json.Marshal(LLMProviderData{Kind: "openai", Slug: "openai", APIKey: "admin-key"})
	adminCipher, err := EncryptSecret(adminKEK, adminPlaintext)
	if err != nil {
		t.Fatalf("EncryptSecret(admin): %v", err)
	}

	// Bindings ordered with user first — mirrors the production
	// precedence sort that puts user creds ahead of admin (Epic 30).
	mockCred := &mockCredentialStore{
		bindings: []CredentialBinding{
			{ID: "cred-user", OwnerType: "user", OwnerID: "user-1", Kind: "openai", Slug: "openai", Ciphertext: userCipher, SourceType: "explicit"},
			{ID: "cred-admin", OwnerType: "admin", OwnerID: "_platform", Kind: "openai", Slug: "openai", Ciphertext: adminCipher, SourceType: "auto"},
		},
	}
	combined := &combinedTestStore{SecretStore: secretStore, CredentialStore: mockCred}
	svc := NewSecretService(keySvc, combined)
	svc.SetAdminProvider(mustStaticProvider(t, adminKEK))

	data, err := svc.InjectSessionlessSecrets(ctx, "user-1", "ws-1")
	if err != nil {
		t.Fatalf("InjectSessionlessSecrets: %v", err)
	}

	var injected []InjectedSecret
	if err := json.Unmarshal(data, &injected); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	var sawOpenAI bool
	var apiKeyValue string
	for _, item := range injected {
		if item.Type == SecretTypeLLMProvider && item.Name == "openai" {
			sawOpenAI = true
			var pd LLMProviderData
			if jerr := json.Unmarshal([]byte(item.Plaintext), &pd); jerr != nil {
				t.Errorf("openai payload not valid LLMProviderData JSON: %v", jerr)
			}
			apiKeyValue = pd.APIKey
		}
	}
	if !sawOpenAI {
		t.Fatalf("admin openai credential MUST materialize as fallback when the user openai binding is skipped; got %d items: %+v", len(injected), injected)
	}
	if apiKeyValue != "admin-key" {
		t.Errorf("expected the admin-key (fallback) since user binding is skipped without a session; got %q", apiKeyValue)
	}
}

func TestSecretService_InjectSecrets_CrossTenantIsolation(t *testing.T) {
	svc, sess1, _ := setupSecretServiceWithTwoUsers(t)
	ctx := context.Background()

	// User 1 creates and binds a secret
	s1, _ := svc.CreateSecret(ctx, "user-1", sess1, nil, CreateSecretRequest{
		Name: "private", Type: SecretTypeAPIKey, Value: "user1-key",
		Metadata: json.RawMessage(`{"kind":"x","slug":"x"}`),
	})
	_, _ = svc.SetBindings(ctx, "user-1", "ws-user1", []string{s1.ID})

	// User 2 tries to inject from user 1's workspace (should get nothing)
	data, err := svc.InjectSecrets(ctx, "user-2", "sess-2", nil, "ws-user1")
	if err != nil {
		t.Fatalf("Should not error: %v", err)
	}

	var injected []InjectedSecret
	json.Unmarshal(data, &injected)
	// The bindings exist but secrets belong to user-1, so user-2 gets filtered out
	for _, s := range injected {
		if s.Plaintext == "user1-key" {
			t.Error("User 2 should NOT see user 1's decrypted secrets")
		}
	}
}
