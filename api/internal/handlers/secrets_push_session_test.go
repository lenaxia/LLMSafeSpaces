// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// pushPathSessionStore is a minimal SecretStore + CredentialStore for
// unit testing the bind-time notify path. It is deliberately small —
// only the methods exercised by the test return real values; the rest
// are quiet so any drift in the call surface is visible in assertions.
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

// TestHandler_BindNotifiesEvenWithAPIKeyAuthAndOrgCredential is the
// notify-era descendant of the A2.2 finding: when a user binds a
// workspace via API-key auth AND that workspace has user-DEK content
// bound, the live delivery MUST still fire. Under the notify model the
// API carries no content at all — the pod re-pulls server-KEK and
// user-DEK entries through the one session-independent builder — so the
// pin is that the bind dispatches the notify regardless of the caller's
// session shape.
func TestHandler_BindNotifiesEvenWithAPIKeyAuthAndOrgCredential(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var (
		mu           sync.Mutex
		notifyCalled bool
		notifyBody   []byte
		notifyPath   string
	)
	listener, err := net.Listen("tcp", "127.0.0.1:4097")
	if err != nil {
		t.Skip("port 4097 not available for test agentd mock")
	}
	agentd := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		mu.Lock()
		notifyCalled = true
		notifyBody = body
		notifyPath = r.URL.Path
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"applied","appliedRev":"2:a:b","restarted":false}`))
	}))
	agentd.Listener = listener
	agentd.Start()
	defer agentd.Close()

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
	router.Use(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Set("sessionID", "apikey:fake-hash-for-test")
		c.Next()
	})
	wsGroup := router.Group("/api/v1/workspaces")
	wsGroup.PUT("/:id/bindings", handler.SetBindings)

	bindBody, _ := json.Marshal(map[string][]string{"secretIds": {"sec-existing-env"}})
	bReq := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-1/bindings", strings.NewReader(string(bindBody)))
	bReq.Header.Set("Content-Type", "application/json")
	bw := httptest.NewRecorder()
	router.ServeHTTP(bw, bReq)

	if bw.Code != http.StatusNoContent {
		t.Fatalf("Bind: expected 204, got %d: %s", bw.Code, bw.Body.String())
	}

	mu.Lock()
	called := notifyCalled
	body := notifyBody
	path := notifyPath
	mu.Unlock()

	if !called {
		t.Fatal("agentd MUST be notified on bind even when the caller authenticated via API key (no session); delivery was silently dropped")
	}
	assert.Equal(t, "/v1/resync-secrets", path)
	assert.Empty(t, body, "the notify must carry no batch body — the pod re-pulls everything itself")
}
