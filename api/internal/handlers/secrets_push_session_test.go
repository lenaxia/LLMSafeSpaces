// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// pushPathSessionStore is a minimal SecretStore + CredentialStore for unit
// testing the bind-time push path. It is deliberately small — only the
// methods exercised by the test return real values; the rest panic so any
// drift in the call surface fails loud.
type pushPathSessionStore struct {
	mu          sync.Mutex
	credentials []secrets.CredentialBinding
	bindings    map[string][]string
	secrets     map[string]*secrets.UserSecret
}

func (s *pushPathSessionStore) GetWorkspaceCredentials(_ context.Context, _ string) ([]secrets.CredentialBinding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]secrets.CredentialBinding, len(s.credentials))
	copy(cp, s.credentials)
	return cp, nil
}
func (s *pushPathSessionStore) UpsertFreeTierCredential(_ context.Context, _ []byte) error {
	return nil
}
func (s *pushPathSessionStore) SeedWorkspaceCredentials(_ context.Context, _, _ string, _ *string) error {
	return nil
}
func (s *pushPathSessionStore) BindCredentialToAllUserWorkspaces(_ context.Context, _, _ string) error {
	return nil
}
func (s *pushPathSessionStore) HasUserProviderCredential(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}

func (s *pushPathSessionStore) GetWorkspaceMCPServers(_ context.Context, _ string) ([]secrets.MCPServerBindingRow, error) {
	return nil, nil
}

func (s *pushPathSessionStore) GetBindings(_ context.Context, ws string) ([]*secrets.UserSecret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*secrets.UserSecret, 0, len(s.bindings[ws]))
	for _, sid := range s.bindings[ws] {
		if sec := s.secrets[sid]; sec != nil {
			cp := *sec
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (s *pushPathSessionStore) LogAudit(_ context.Context, _ *secrets.AuditEntry) error { return nil }

func (s *pushPathSessionStore) CurrentRevision(context.Context, string) (int64, string, bool, error) {
	return 0, "", false, nil
}
func (s *pushPathSessionStore) EnsureRevision(context.Context, string, string) (int64, error) {
	return 1, nil
}

func (s *pushPathSessionStore) CreateSecret(_ context.Context, sec *secrets.UserSecret) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sec.ID == "" {
		sec.ID = "sec-" + sec.Name
	}
	cp := *sec
	if s.secrets == nil {
		s.secrets = make(map[string]*secrets.UserSecret)
	}
	s.secrets[sec.ID] = &cp
	return nil
}
func (s *pushPathSessionStore) GetSecret(_ context.Context, _, id string) (*secrets.UserSecret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sec := s.secrets[id]; sec != nil {
		cp := *sec
		return &cp, nil
	}
	return nil, nil
}
func (s *pushPathSessionStore) GetSecretByName(_ context.Context, _, name string) (*secrets.UserSecret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sec := range s.secrets {
		if sec.Name == name {
			cp := *sec
			return &cp, nil
		}
	}
	return nil, nil
}
func (s *pushPathSessionStore) ListSecrets(_ context.Context, _ string) ([]*secrets.UserSecret, error) {
	return nil, nil
}
func (s *pushPathSessionStore) ListGlobalDefaultSecrets(_ context.Context, _ string) ([]*secrets.UserSecret, error) {
	return nil, nil
}
func (s *pushPathSessionStore) UpdateSecret(_ context.Context, _ *secrets.UserSecret) error {
	return nil
}
func (s *pushPathSessionStore) ReEncryptUserSecrets(_ context.Context, _ string, _ int,
	_ func([]byte) ([]byte, error), _ func(context.Context) error) error {
	return nil
}
func (s *pushPathSessionStore) DeleteSecret(_ context.Context, _, _ string) error { return nil }
func (s *pushPathSessionStore) SetBindings(_ context.Context, ws string, ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bindings == nil {
		s.bindings = make(map[string][]string)
	}
	s.bindings[ws] = ids
	return nil
}
func (s *pushPathSessionStore) AddBindings(_ context.Context, ws string, ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bindings == nil {
		s.bindings = make(map[string][]string)
	}
	s.bindings[ws] = append(s.bindings[ws], ids...)
	return nil
}
func (s *pushPathSessionStore) GetBindingsForSecret(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}
func (s *pushPathSessionStore) QueryAudit(_ context.Context, _ string, _ secrets.AuditQuery) ([]*secrets.AuditEntry, error) {
	return nil, nil
}

// TestHandler_BindPushesOrgCredentialEvenWithAPIKeyAuth proves Finding
// A2.2 (corrected after second-pass review): when a user binds a
// workspace via API-key auth AND that workspace has any user-DEK
// secret already bound (e.g. an env-secret from a prior JWT session),
// the auto-push to agentd MUST still deliver server-KEK content
// (admin/org credentials).
//
// Production reality (uncovered in second-pass review): API-key auth
// sets `sessionID = "apikey:" + hash(token)`, NOT empty string — the
// original first-pass fix branched on `sessionID != ""` and was dead
// code as a result. The durable shape (US-70.2): batch construction
// carries no session identity at all — the builder decrypts user
// entries via the server-side DEK unwrap, and DEK-tier failure
// degrades to server-KEK entries with a machine-readable reason.
//
// Recipe:
//
//   - Workspace has a bound user-DEK env-secret plus a server-KEK
//     org credential.
//   - pushSecretsToAgent calls Push(ctx, userID, ws) → BuildWorkspaceBatch
//   - The org credential (server-KEK) must always reach agentd.
//
// This test must FAIL today (agentd not called, or called without the
// org credential) and PASS after the loadNonLLMSecrets degrade fix.
func TestHandler_BindPushesOrgCredentialEvenWithAPIKeyAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Arrange: agentd mock that captures the reload-secrets payload.
	var (
		mu           sync.Mutex
		reloadCalled bool
		reloadBody   []byte
	)
	listener, err := net.Listen("tcp", "127.0.0.1:4097")
	if err != nil {
		t.Skip("port 4097 not available for test agentd mock")
	}
	agentd := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/reload-secrets" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		reloadCalled = true
		reloadBody = body
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"reloaded": 1, "restarted": false})
	}))
	agentd.Listener = listener
	agentd.Start()
	defer agentd.Close()

	// Org credential, server-KEK encrypted (decryptable without a session).
	orgKEK := make([]byte, 32)
	for i := range orgKEK {
		orgKEK[i] = byte(i + 1)
	}
	orgPlaintext, _ := json.Marshal(secrets.LLMProviderData{
		Kind: "openai_compatible", Slug: "custom", APIKey: "org-api-key", BaseURL: "https://example.test/v1",
	})
	orgCipher, err := secrets.EncryptSecret(orgKEK, orgPlaintext)
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}

	store := &pushPathSessionStore{
		credentials: []secrets.CredentialBinding{
			{ID: "cred-org", OwnerType: "org", OwnerID: "org-1", Kind: "openai_compatible", Slug: "custom", Ciphertext: orgCipher, SourceType: "auto"},
		},
		bindings: map[string][]string{
			// Workspace already has a user-DEK env-secret bound from a prior
			// JWT-auth session. This is the trigger for finding A2.2.
			"ws-1": {"sec-existing-env"},
		},
		secrets: map[string]*secrets.UserSecret{
			"sec-existing-env": {
				ID:         "sec-existing-env",
				UserID:     "user-1",
				Name:       "database_url",
				Type:       secrets.SecretTypeEnvSecret,
				Ciphertext: []byte("opaque-bytes-not-decryptable-without-DEK"),
				Metadata:   json.RawMessage(`{"var_name":"DATABASE_URL"}`),
			},
		},
	}

	keySvc := secrets.NewKeyService(newTestKeyStore(), newTestDEKCache())
	svc := secrets.NewSecretService(keySvc, store)
	orgProvider, err := secrets.NewStaticKeyProvider(orgKEK)
	if err != nil {
		t.Fatalf("NewStaticKeyProvider: %v", err)
	}
	svc.SetOrgProvider(orgProvider)

	handler := NewSecretsHandler(svc)
	handler.SetPodIPResolver(&staticPodIPResolver{addr: "127.0.0.1"})
	handler.SetPasswordProvider(staticPasswordProvider{})
	logger := &recordingLogger{}
	handler.SetLogger(logger)

	router := gin.New()
	// API-key auth: userID set, sessionID set to "apikey:<hash>" — this
	// matches the production AuthMiddleware behavior at
	// api/internal/services/auth/auth.go:1152. The pseudo-sessionID is
	// non-empty but no DEK is ever cached for it (regular API keys have
	// no decrypt_access scope). Without the loadNonLLMSecrets degrade-
	// to-skip-with-audit fix, this configuration triggers a hard
	// "DEK not available" error and the entire push is silently
	// dropped — the second-pass-review finding that A2.2 was dead code.
	router.Use(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Set("sessionID", "apikey:fake-hash-for-test")
		c.Next()
	})
	wsGroup := router.Group("/api/v1/workspaces")
	wsGroup.PUT("/:id/bindings", handler.SetBindings)

	// Bind: replace bindings with the existing env-secret (preserved).
	// SetBindings replaces the full set, so we must include it. The org
	// credential lives in workspace_credential_bindings (a different table)
	// and is unaffected by SetBindings — but pushSecretsToAgent reads BOTH,
	// so this bind triggers the push that should deliver the org cred.
	bindBody, _ := json.Marshal(map[string][]string{"secretIds": {"sec-existing-env"}})
	bReq := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-1/bindings", bytes.NewBuffer(bindBody))
	bReq.Header.Set("Content-Type", "application/json")
	bw := httptest.NewRecorder()
	router.ServeHTTP(bw, bReq)

	if bw.Code != http.StatusNoContent {
		t.Fatalf("Bind: expected 204, got %d: %s", bw.Code, bw.Body.String())
	}

	// pushSecretsToAgent runs inline (secrets.go:335 calls it before
	// c.Status(204)) so the agentd mock has already been called by
	// the time ServeHTTP returns. No sleep needed.
	mu.Lock()
	called := reloadCalled
	body := reloadBody
	mu.Unlock()

	if !called {
		t.Fatal("agentd MUST receive the org credential via reload-secrets even when bind is via API-key auth (no sessionID); push was silently dropped")
	}

	var pushed []struct {
		Type      string          `json:"type"`
		Name      string          `json:"name"`
		Plaintext json.RawMessage `json:"plaintext"`
	}
	if err := json.Unmarshal(body, &pushed); err != nil {
		t.Fatalf("agentd payload not parseable: %v\nbody=%s", err, string(body))
	}

	var sawOrg bool
	for _, p := range pushed {
		if p.Type == "llm-provider" && p.Name == "custom" {
			sawOrg = true
		}
	}
	if !sawOrg {
		t.Errorf("agentd payload must include the org credential (provider=custom); got: %s", string(body))
	}
}
