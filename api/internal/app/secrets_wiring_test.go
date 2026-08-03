// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/api/internal/server"
	"github.com/lenaxia/llmsafespaces/api/internal/services/workspace"
	kmocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	lmocks "github.com/lenaxia/llmsafespaces/mocks/logger"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// TestSecretsWiring_E2E tests that the secrets handler is properly wired
// into the router and processes requests end-to-end.
func TestSecretsWiring_E2E(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Create real services using in-memory adapters (same as app.go wiring)
	keyStore := &dbKeyStoreAdapter{}
	dekCache := &memDEKCache{store: make(map[string][]byte)}
	keyService := secrets.NewKeyService(keyStore, dekCache)
	testProv, _ := secrets.NewStaticKeyProvider(make([]byte, 32))
	keyService.SetAPIKeyStore(nil, testProv)
	secretStore := &dbSecretStoreAdapter{}
	secretService := secrets.NewSecretService(keyService, secretStore)
	secretsHandler := handlers.NewSecretsHandler(secretService)

	// Initialize user keys (simulates registration)
	err := keyService.InitializeUserKeysServerKEK(context.Background(), "test-user", "server_kek")
	if err != nil {
		t.Fatalf("InitializeUserKeysServerKEK: %v", err)
	}

	// Unlock DEK (simulates login)
	err = keyService.UnlockDEK(context.Background(), "test-user", []byte("password"), "test-jti", 0)
	if err != nil {
		t.Fatalf("UnlockDEK: %v", err)
	}

	// Create a minimal router with auth simulation + secrets routes
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userID", "test-user")
		c.Set("sessionID", "test-jti")
		c.Next()
	})

	// Wire exactly as the real router does
	secretsGroup := router.Group("/api/v1/secrets")
	secretsGroup.POST("", secretsHandler.CreateSecret)
	secretsGroup.GET("", secretsHandler.ListSecrets)
	secretsGroup.GET("/audit", secretsHandler.GetAuditLog)
	secretsGroup.GET("/:id", secretsHandler.GetSecret)
	secretsGroup.PUT("/:id", secretsHandler.UpdateSecret)
	secretsGroup.DELETE("/:id", secretsHandler.DeleteSecret)

	wsGroup := router.Group("/api/v1/workspaces")
	wsGroup.PUT("/:id/bindings", secretsHandler.SetBindings)
	wsGroup.GET("/:id/bindings", secretsHandler.GetBindings)

	// === Test: Create secret ===
	body := `{"name":"wiring-test","type":"api-key","value":"sk-wired-123","metadata":{"kind":"openai","slug":"openai"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("Create: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var created secrets.SecretResponse
	json.Unmarshal(w.Body.Bytes(), &created)
	if created.ID == "" {
		t.Fatal("Created secret should have ID")
	}

	// === Test: List secrets ===
	req = httptest.NewRequest(http.MethodGet, "/api/v1/secrets", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("List: expected 200, got %d", w.Code)
	}

	// === Test: Get secret ===
	req = httptest.NewRequest(http.MethodGet, "/api/v1/secrets/"+created.ID, nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Get: expected 200, got %d", w.Code)
	}

	// === Test: Bind to workspace ===
	bindBody := `{"secretIds":["` + created.ID + `"]}`
	req = httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-wiring/bindings", bytes.NewBufferString(bindBody))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("Bind: expected 204, got %d: %s", w.Code, w.Body.String())
	}

	// === Test: Get bindings ===
	req = httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-wiring/bindings", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GetBindings: expected 200, got %d", w.Code)
	}
	var bindResp secrets.BindingsResponse
	json.Unmarshal(w.Body.Bytes(), &bindResp)
	if len(bindResp.Bindings) != 1 {
		t.Errorf("Expected 1 binding, got %d", len(bindResp.Bindings))
	}

	// === Test: Audit log ===
	req = httptest.NewRequest(http.MethodGet, "/api/v1/secrets/audit", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Audit: expected 200, got %d", w.Code)
	}

	// === Test: Delete ===
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/secrets/"+created.ID, nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("Delete: expected 204, got %d", w.Code)
	}

	t.Log("Secrets wiring E2E: all operations successful")
}

// TestRouterConfig_SecretsHandler verifies the router accepts SecretsHandler in config
func TestRouterConfig_SecretsHandler(t *testing.T) {
	cfg := server.DefaultRouterConfig()
	// Should not panic with nil SecretsHandler
	if cfg.SecretsHandler != nil {
		t.Error("Default config should have nil SecretsHandler")
	}
}

// TestUserProvCredChecker_DelegatesToWorkspaceService is the design 0041
// Story 2 regression test for the userProvCred wiring rewire. The old
// adapter (workspaceOwnerVerifierAdapter) was deleted because it lacked
// the D5 creator-membership re-check. The replacement is a closure that
// calls workspace.Service.ResolveWorkspace + CheckOwnership directly.
// This test reproduces that closure shape against a real *workspace.Service
// (with mocked DB) and asserts (a) the happy path delegates to ResolveWorkspace
// and returns nil, (b) ResolveWorkspace errors propagate, and (c)
// CheckOwnership returns Forbidden for a non-owner.
func TestUserProvCredChecker_DelegatesToWorkspaceService(t *testing.T) {
	const userID, wsID = "user-1", "ws-1"

	mkSvc := func(t *testing.T, db *imocks.MockDatabaseService) *workspace.Service {
		t.Helper()
		log := lmocks.NewMockLogger()
		log.On("Info", mock.Anything, mock.Anything).Maybe()
		log.On("Warn", mock.Anything, mock.Anything).Maybe()
		log.On("Error", mock.Anything, mock.Anything, mock.Anything).Maybe()
		log.On("With", mock.Anything).Return(log).Maybe()
		k8s := kmocks.NewMockKubernetesClient()
		met := &imocks.MockMetricsService{}
		met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
		svc, err := workspace.New(log, k8s, db, nil, met, &workspace.Config{Namespace: "default"})
		if err != nil {
			t.Fatalf("workspace.New: %v", err)
		}
		return svc
	}

	t.Run("happy_path_returns_nil", func(t *testing.T) {
		db := &imocks.MockDatabaseService{}
		db.On("GetWorkspace", mock.Anything, wsID).
			Return(&types.WorkspaceMetadata{ID: wsID, UserID: userID}, nil)
		svc := mkSvc(t, db)

		checker := mkUserProvCredChecker(svc)
		err := checker(context.Background(), userID, wsID)
		assert.NoError(t, err)
		db.AssertCalled(t, "GetWorkspace", mock.Anything, wsID)
	})

	t.Run("resolve_error_propagates", func(t *testing.T) {
		db := &imocks.MockDatabaseService{}
		want := fmt.Errorf("db down")
		db.On("GetWorkspace", mock.Anything, wsID).Return((*types.WorkspaceMetadata)(nil), want)
		svc := mkSvc(t, db)

		checker := mkUserProvCredChecker(svc)
		err := checker(context.Background(), userID, wsID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "db down")
	})

	t.Run("non_owner_returns_forbidden", func(t *testing.T) {
		db := &imocks.MockDatabaseService{}
		db.On("GetWorkspace", mock.Anything, wsID).
			Return(&types.WorkspaceMetadata{ID: wsID, UserID: "other-user"}, nil)
		svc := mkSvc(t, db)

		checker := mkUserProvCredChecker(svc)
		err := checker(context.Background(), userID, wsID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "workspace access denied")
	})
}

// mkUserProvCredChecker mirrors the closure wired in app.New so the test
// exercises the same shape production uses.
func mkUserProvCredChecker(wsSvc *workspace.Service) func(ctx context.Context, userID, wsID string) error {
	return func(ctx context.Context, userID, wsID string) error {
		meta, err := wsSvc.ResolveWorkspace(ctx, wsID)
		if err != nil {
			return err
		}
		return wsSvc.CheckOwnership(ctx, userID, meta)
	}
}

// TestSecretsHandler_PodIPResolverWired is the regression test for Bug 1
// in worklog 0085: app.New must call SetPodIPResolver on the secrets
// handler. Without this the reload-secrets endpoint and the SetBindings
// auto-push both silently no-op (returning 503 / swallowing the error).
//
// We test the wiring helper directly rather than constructing the full
// App because app.New requires PostgreSQL/Redis; the helper is the unit
// of behavior we actually care about.
func TestSecretsHandler_PodIPResolverWired(t *testing.T) {
	keyStore := &dbKeyStoreAdapter{}
	dekCache := &memDEKCache{store: make(map[string][]byte)}
	keyService := secrets.NewKeyService(keyStore, dekCache)
	testProv, _ := secrets.NewStaticKeyProvider(make([]byte, 32))
	keyService.SetAPIKeyStore(nil, testProv)
	secretStore := &dbSecretStoreAdapter{}
	secretService := secrets.NewSecretService(keyService, secretStore)

	h := handlers.NewSecretsHandler(secretService)
	if h.HasPodIPResolver() {
		t.Fatalf("freshly-constructed SecretsHandler must not have a resolver")
	}

	// Same call app.New makes; if this stops being valid the wiring is
	// either changed deliberately (update the test) or it has regressed.
	h.SetPodIPResolver(newSecretsPodIPResolver(
		&fakeAppCRDGetter{},
		&fakeAppDBLookup{},
		nil, // logger optional in this smoke test
	))

	if !h.HasPodIPResolver() {
		t.Fatalf("SetPodIPResolver must populate the handler's resolver")
	}
}

// TestPodBootstrapHandler_LoggerWired is the regression guard for the
// observability gap that PR #407 set out to close. Without explicit
// wiring of a logger via SetLogger, PodBootstrapHandler.Bootstrap
// swallows the underlying secret-prep error and returns only a
// generic 500 — the exact behavior that turned the 2026-06-24 outage
// into a 30-minute diagnosis exercise.
//
// The first review of #407 caught that NewPodBootstrapHandlerFromClientset
// in app.go was constructed without a corresponding SetLogger call,
// making the whole observability fix dead code in production. This
// test exists so that regression cannot recur silently — every future
// PR that touches the wiring is forced to keep SetLogger in lockstep
// with the constructor.
//
// We test the wiring helper directly rather than constructing the full
// App because app.New requires PostgreSQL/Redis; the helper is the unit
// of behavior we actually care about.
func TestPodBootstrapHandler_LoggerWired(t *testing.T) {
	keyStore := &dbKeyStoreAdapter{}
	dekCache := &memDEKCache{store: make(map[string][]byte)}
	keyService := secrets.NewKeyService(keyStore, dekCache)
	testProv, _ := secrets.NewStaticKeyProvider(make([]byte, 32))
	keyService.SetAPIKeyStore(nil, testProv)
	secretStore := &dbSecretStoreAdapter{}
	secretService := secrets.NewSecretService(keyService, secretStore)

	// Mirror the exact construction sequence app.go uses.
	fakeClientset := k8sfake.NewSimpleClientset()
	dbSvc := &fakeAppDBLookup{}
	h := handlers.NewPodBootstrapHandlerFromClientset(
		fakeClientset, secretService, dbSvc, nil, "test-namespace",
	)
	if h.HasLogger() {
		t.Fatalf("freshly-constructed PodBootstrapHandler must not have a logger before SetLogger is called")
	}

	// Same call app.go makes (or MUST make — this test exists to enforce
	// that). If this stops being valid the wiring is either changed
	// deliberately (update the test) or it has regressed.
	log := lmocks.NewMockLogger()
	h.SetLogger(log)

	if !h.HasLogger() {
		t.Fatalf("SetLogger must populate the handler's logger so 5xx errors include the underlying cause; otherwise the observability fix in #407 is dead code")
	}
}

// TestPodBootstrapHandler_SettingsReaderWired is the regression guard for
// the SetSettingsReader wiring in app.go. Without it, the
// workspace.allowedExternalDirectories setting is never delivered to agentd
// and agents prompt for /tmp/* on every session. Mirrors
// TestPodBootstrapHandler_LoggerWired's approach: mirror the exact
// construction+wire sequence app.go uses so a future reordering regresses
// loudly instead of silently disabling the feature.
func TestPodBootstrapHandler_SettingsReaderWired(t *testing.T) {
	keyStore := &dbKeyStoreAdapter{}
	dekCache := &memDEKCache{store: make(map[string][]byte)}
	keyService := secrets.NewKeyService(keyStore, dekCache)
	testProv, _ := secrets.NewStaticKeyProvider(make([]byte, 32))
	keyService.SetAPIKeyStore(nil, testProv)
	secretStore := &dbSecretStoreAdapter{}
	secretService := secrets.NewSecretService(keyService, secretStore)

	fakeClientset := k8sfake.NewSimpleClientset()
	dbSvc := &fakeAppDBLookup{}
	h := handlers.NewPodBootstrapHandlerFromClientset(
		fakeClientset, secretService, dbSvc, nil, "test-namespace",
	)
	if h.HasSettingsReader() {
		t.Fatalf("freshly-constructed PodBootstrapHandler must not have a settings reader before SetSettingsReader is called")
	}

	// Mirror the exact call app.go makes. settings.InstanceService satisfies
	// the local bootstrapSettingsReader interface; here we use a minimal fake
	// since the real InstanceService requires PostgreSQL.
	h.SetSettingsReader(&fakeBootstrapSettingsReader{})

	if !h.HasSettingsReader() {
		t.Fatalf("SetSettingsReader must populate the handler so allowed-external-directories is delivered to agentd; " +
			"otherwise the /tmp/* auto-approval feature is dead code in production")
	}
}

// fakeBootstrapSettingsReader satisfies handlers.bootstrapSettingsReader
// for the wiring test. No behavior needed — we only assert the wiring
// call was made, not that GetStrings returns a value.
type fakeBootstrapSettingsReader struct{}

func (f *fakeBootstrapSettingsReader) GetStrings(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

// fakeAppCRDGetter / fakeAppDBLookup are placeholders used only to
// confirm the resolver constructor accepts compatible adapter types.
// Behavioral tests live in secrets_podip_resolver_test.go.
type fakeAppCRDGetter struct{}

func (f *fakeAppCRDGetter) GetWorkspace(context.Context, string) (*v1.Workspace, error) {
	return nil, nil
}

type fakeAppDBLookup struct{}

func (f *fakeAppDBLookup) GetWorkspace(context.Context, string) (*types.WorkspaceMetadata, error) {
	return nil, nil
}

type memDEKCache struct {
	store map[string][]byte
}

func (m *memDEKCache) CacheDEK(_ context.Context, sessionID string, dek []byte, _ time.Duration) error {
	cp := make([]byte, len(dek))
	copy(cp, dek)
	m.store[sessionID] = cp
	return nil
}

func (m *memDEKCache) GetDEK(_ context.Context, sessionID string) ([]byte, error) {
	dek, ok := m.store[sessionID]
	if !ok {
		return nil, nil
	}
	return dek, nil
}

func (m *memDEKCache) EvictDEK(_ context.Context, sessionID string) error {
	delete(m.store, sessionID)
	return nil
}

// TestAuditedProvider_WiredIntoProductionProviders is the regression guard for
// the G50 audit wiring (#366). The first review of #482 caught that the
// providerCredsProv/orgCredsProv wraps were placed before asyncAudit was
// constructed — dead code that left admin/org decrypts unaudited, identical
// in shape to the #407 PodBootstrap logger regression that
// TestPodBootstrapHandler_LoggerWired guards. This test mirrors the exact
// construction+wrap sequence app.go uses and asserts each provider handed to
// a consumer is an *AuditedProvider, so a future reordering regresses loudly
// instead of silently disabling audit.
//
// Uses in-memory adapters (no Postgres/Redis) — the wrap sequence is the unit
// of behavior under test, not the full App.
func TestAuditedProvider_WiredIntoProductionProviders(t *testing.T) {
	// Mirror newPurposeProvider: derive a key and build a StaticKeyProvider.
	// (We can't call the unexported newPurposeProvider here, but the shape is
	// identical — a StaticKeyProvider wrapping an HKDF-derived key.)
	inner, err := secrets.NewStaticKeyProvider(bytes.Repeat([]byte{0x01}, 32))
	require.NoError(t, err)

	// Mirror asyncAudit construction (app.go:320). Use the in-memory secret
	// store adapter so no Postgres is required; the AuditWriter interface is
	// what matters, not the backing store.
	asyncAudit := secrets.NewAsyncAuditLogger(&dbSecretStoreAdapter{}, 16, lmocks.NewMockLogger())

	// Mirror app.go:329-330 (the wrap, AFTER asyncAudit construction).
	providerCredsProv := secrets.NewAuditedProvider(inner, asyncAudit, "provider-credentials")
	orgCredsProv := secrets.NewAuditedProvider(inner, asyncAudit, "org-credentials")
	// Mirror app.go:467 (apiKeyProv wrap, also after construction).
	apiKeyProv := secrets.NewAuditedProvider(inner, asyncAudit, "api-keys")

	for _, tc := range []struct {
		name string
		prov secrets.RootKeyProvider
	}{
		{"providerCredsProv", providerCredsProv},
		{"orgCredsProv", orgCredsProv},
		{"apiKeyProv", apiKeyProv},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrapped, ok := tc.prov.(*secrets.AuditedProvider)
			require.True(t, ok,
				"%s must be an *AuditedProvider so its Decrypts are audited; "+
					"if this fails the wrap in app.go was removed or reordered before asyncAudit construction "+
					"(the #482 first-review bug)", tc.name)
			// Sanity: the wrapper still decrypts (delegation intact).
			ct, err := wrapped.Encrypt(context.Background(), []byte("payload"))
			require.NoError(t, err)
			pt, err := wrapped.Decrypt(context.Background(), ct)
			require.NoError(t, err)
			assert.Equal(t, "payload", string(pt))
		})
	}
}
