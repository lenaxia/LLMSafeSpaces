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
	"time"

	"github.com/gin-gonic/gin"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// TestHandler_E2E_CreateListGetDeleteRoundTrip tests the full CRUD cycle via HTTP
func TestHandler_E2E_CreateListGetDeleteRoundTrip(t *testing.T) {
	router, _, _ := setupTestRouter(t)

	// Create
	createBody := `{"name":"e2e-secret","type":"api-key","value":"sk-e2e-test-key","metadata":{"kind":"openai","slug":"openai","model":"gpt-4o"}}`
	cReq := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewBufferString(createBody))
	cReq.Header.Set("Content-Type", "application/json")
	cw := httptest.NewRecorder()
	router.ServeHTTP(cw, cReq)

	if cw.Code != http.StatusCreated {
		t.Fatalf("Create: expected 201, got %d: %s", cw.Code, cw.Body.String())
	}

	var created secrets.SecretResponse
	json.Unmarshal(cw.Body.Bytes(), &created)

	// List — should contain the secret
	lReq := httptest.NewRequest(http.MethodGet, "/api/v1/secrets", nil)
	lw := httptest.NewRecorder()
	router.ServeHTTP(lw, lReq)

	var listResp struct {
		Secrets []secrets.SecretResponse `json:"secrets"`
	}
	json.Unmarshal(lw.Body.Bytes(), &listResp)
	found := false
	for _, s := range listResp.Secrets {
		if s.ID == created.ID {
			found = true
			break
		}
	}
	if !found {
		t.Error("Created secret not found in list")
	}

	// Get by ID
	gReq := httptest.NewRequest(http.MethodGet, "/api/v1/secrets/"+created.ID, nil)
	gw := httptest.NewRecorder()
	router.ServeHTTP(gw, gReq)

	if gw.Code != http.StatusOK {
		t.Fatalf("Get: expected 200, got %d", gw.Code)
	}
	var got secrets.SecretResponse
	json.Unmarshal(gw.Body.Bytes(), &got)
	if got.Name != "e2e-secret" {
		t.Errorf("Get name mismatch: %s", got.Name)
	}

	// Delete
	dReq := httptest.NewRequest(http.MethodDelete, "/api/v1/secrets/"+created.ID, nil)
	dw := httptest.NewRecorder()
	router.ServeHTTP(dw, dReq)

	if dw.Code != http.StatusNoContent {
		t.Fatalf("Delete: expected 204, got %d", dw.Code)
	}

	// Verify gone
	gReq2 := httptest.NewRequest(http.MethodGet, "/api/v1/secrets/"+created.ID, nil)
	gw2 := httptest.NewRecorder()
	router.ServeHTTP(gw2, gReq2)
	if gw2.Code != http.StatusNotFound {
		t.Errorf("After delete: expected 404, got %d", gw2.Code)
	}
}

// TestHandler_E2E_UpdateAndVerify tests update changes the stored value
func TestHandler_E2E_UpdateAndVerify(t *testing.T) {
	router, _, _ := setupTestRouter(t)

	// Create
	body := `{"name":"updatable-e2e","type":"env-secret","value":"old-value","metadata":{"var_name":"TEST_VAR"}}`
	cReq := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewBufferString(body))
	cReq.Header.Set("Content-Type", "application/json")
	cw := httptest.NewRecorder()
	router.ServeHTTP(cw, cReq)

	var created secrets.SecretResponse
	json.Unmarshal(cw.Body.Bytes(), &created)

	// Update
	uBody := `{"value":"new-value"}`
	uReq := httptest.NewRequest(http.MethodPut, "/api/v1/secrets/"+created.ID, bytes.NewBufferString(uBody))
	uReq.Header.Set("Content-Type", "application/json")
	uw := httptest.NewRecorder()
	router.ServeHTTP(uw, uReq)

	if uw.Code != http.StatusNoContent {
		t.Fatalf("Update: expected 204, got %d: %s", uw.Code, uw.Body.String())
	}

	// Get — metadata should still be there
	gReq := httptest.NewRequest(http.MethodGet, "/api/v1/secrets/"+created.ID, nil)
	gw := httptest.NewRecorder()
	router.ServeHTTP(gw, gReq)

	var got secrets.SecretResponse
	json.Unmarshal(gw.Body.Bytes(), &got)
	var meta map[string]string
	json.Unmarshal(got.Metadata, &meta)
	if meta["var_name"] != "TEST_VAR" {
		t.Errorf("Metadata lost after update: %v", meta)
	}
}

// TestHandler_E2E_BindingFullCycle tests bind→get→rebind→unbind
func TestHandler_E2E_BindingFullCycle(t *testing.T) {
	router, _, _ := setupTestRouter(t)

	// Create 2 secrets
	ids := make([]string, 2)
	for i, name := range []string{"bind-a", "bind-b"} {
		body := `{"name":"` + name + `","type":"api-key","value":"v","metadata":{"kind":"x","slug":"x"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		var resp secrets.SecretResponse
		json.Unmarshal(w.Body.Bytes(), &resp)
		ids[i] = resp.ID
	}

	// Bind both
	bindBody, _ := json.Marshal(map[string][]string{"secretIds": ids})
	bReq := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-bind-test/bindings", bytes.NewBuffer(bindBody))
	bReq.Header.Set("Content-Type", "application/json")
	bw := httptest.NewRecorder()
	router.ServeHTTP(bw, bReq)
	if bw.Code != http.StatusNoContent {
		t.Fatalf("Bind: expected 204, got %d: %s", bw.Code, bw.Body.String())
	}

	// Get bindings
	gReq := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-bind-test/bindings", nil)
	gw := httptest.NewRecorder()
	router.ServeHTTP(gw, gReq)
	var resp secrets.BindingsResponse
	json.Unmarshal(gw.Body.Bytes(), &resp)
	if len(resp.Bindings) != 2 {
		t.Fatalf("Expected 2 bindings, got %d", len(resp.Bindings))
	}

	// Rebind with only first
	rebindBody, _ := json.Marshal(map[string][]string{"secretIds": {ids[0]}})
	rReq := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-bind-test/bindings", bytes.NewBuffer(rebindBody))
	rReq.Header.Set("Content-Type", "application/json")
	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, rReq)
	if rw.Code != http.StatusNoContent {
		t.Fatalf("Rebind: expected 204, got %d", rw.Code)
	}

	// Verify only 1 binding
	gReq2 := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-bind-test/bindings", nil)
	gw2 := httptest.NewRecorder()
	router.ServeHTTP(gw2, gReq2)
	json.Unmarshal(gw2.Body.Bytes(), &resp)
	if len(resp.Bindings) != 1 {
		t.Errorf("Expected 1 binding after rebind, got %d", len(resp.Bindings))
	}

	// Unbind all (empty array)
	emptyBind := `{"secretIds":[]}`
	eReq := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-bind-test/bindings", bytes.NewBufferString(emptyBind))
	eReq.Header.Set("Content-Type", "application/json")
	ew := httptest.NewRecorder()
	router.ServeHTTP(ew, eReq)
	if ew.Code != http.StatusNoContent {
		t.Fatalf("Unbind: expected 204, got %d", ew.Code)
	}

	// Verify empty
	gReq3 := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-bind-test/bindings", nil)
	gw3 := httptest.NewRecorder()
	router.ServeHTTP(gw3, gReq3)
	json.Unmarshal(gw3.Body.Bytes(), &resp)
	if len(resp.Bindings) != 0 {
		t.Errorf("Expected 0 bindings after unbind, got %d", len(resp.Bindings))
	}
}

// TestHandler_CreateSecret_AllTypesViaHTTP tests creating each secret type through HTTP
func TestHandler_CreateSecret_AllTypesViaHTTP(t *testing.T) {
	router, _, _ := setupTestRouter(t)

	tests := []struct {
		name     string
		body     string
		wantCode int
	}{
		{"api-key", `{"name":"llm","type":"api-key","value":"sk-123","metadata":{"kind":"anthropic","slug":"anthropic"}}`, 201},
		{"ssh-key", `{"name":"ssh","type":"ssh-key","value":"key-data","metadata":{"key_type":"ed25519","host":"github.com"}}`, 201},
		{"git-credential", `{"name":"git","type":"git-credential","value":"ghp_xxx","metadata":{"host":"github.com"}}`, 201},
		{"secret-file", `{"name":"file","type":"secret-file","value":"cert","metadata":{"mount_path":"app/cert.pem"}}`, 201},
		{"env-secret", `{"name":"env","type":"env-secret","value":"postgres://...","metadata":{"var_name":"DB_URL"}}`, 201},
		{"invalid-type", `{"name":"bad","type":"nope","value":"x"}`, 400},
		{"ssh-no-metadata", `{"name":"ssh2","type":"ssh-key","value":"x"}`, 400},
		{"file-no-path", `{"name":"file2","type":"secret-file","value":"x","metadata":{"other":"y"}}`, 400},
		{"env-no-var", `{"name":"env2","type":"env-secret","value":"x","metadata":{"other":"y"}}`, 400},
		{"empty-name", `{"name":"","type":"api-key","value":"x"}`, 400},
		{"missing-value", `{"name":"novalue","type":"api-key"}`, 400},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewBufferString(tt.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			if w.Code != tt.wantCode {
				t.Errorf("Expected %d, got %d: %s", tt.wantCode, w.Code, w.Body.String())
			}
		})
	}
}

// TestHandler_ConcurrentRequests tests concurrent HTTP requests don't race
func TestHandler_ConcurrentRequests(t *testing.T) {
	router, _, _ := setupTestRouter(t)

	done := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		go func(idx int) {
			name := string(rune('a'+idx)) + "-concurrent"
			body := `{"name":"` + name + `","type":"api-key","value":"v` + name + `","metadata":{"kind":"x","slug":"x"}}`
			req := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			if w.Code != http.StatusCreated {
				t.Errorf("Concurrent create %d: expected 201, got %d", idx, w.Code)
			}
			done <- true
		}(i)
	}

	for i := 0; i < 10; i++ {
		<-done
	}

	// List should have all 10
	req := httptest.NewRequest(http.MethodGet, "/api/v1/secrets", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var resp struct {
		Secrets []secrets.SecretResponse `json:"secrets"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Secrets) != 10 {
		t.Errorf("Expected 10 secrets after concurrent creates, got %d", len(resp.Secrets))
	}
}

// TestHandler_E2E_BindTriggersReloadSecrets verifies that SetBindings
// auto-pushes secrets to the running pod's agentd via reload-secrets.
func TestHandler_E2E_BindTriggersReloadSecrets(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var (
		mu           sync.Mutex
		reloadCalled bool
		reloadBody   []byte
	)

	agentdListener, err := net.Listen("tcp", "127.0.0.1:4097")
	if err != nil {
		t.Skip("port 4097 not available for test agentd mock")
	}
	agentd := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/reload-secrets" {
			t.Errorf("agentd received unexpected request: %s", r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		reloadCalled = true
		reloadBody = body
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"reloaded": 1, "restarted": false})
	}))
	agentd.Listener = agentdListener
	agentd.Start()
	defer agentd.Close()

	ctx := context.Background()
	userID := "test-user"
	password := []byte("test-password")
	sessionID := "test-session"

	keyStore := newTestKeyStore()
	dekCache := newTestDEKCache()
	keySvc := secrets.NewKeyService(keyStore, dekCache)
	secretStore := newTestSecretStore()
	svc := secrets.NewSecretService(keySvc, secretStore)

	err = keySvc.InitializeUserKeysServerKEK(ctx, userID, "server_kek")
	if err != nil {
		t.Fatalf("InitializeUserKeysServerKEK: %v", err)
	}
	err = keySvc.UnlockDEK(ctx, userID, password, sessionID, time.Hour)
	if err != nil {
		t.Fatalf("UnlockDEK: %v", err)
	}

	handler := NewSecretsHandler(svc)
	handler.SetPodIPResolver(&staticPodIPResolver{addr: "127.0.0.1"})

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userID", userID)
		c.Set("sessionID", sessionID)
		c.Next()
	})

	router.POST("/api/v1/secrets", handler.CreateSecret)
	wsGroup := router.Group("/api/v1/workspaces")
	wsGroup.PUT("/:id/bindings", handler.SetBindings)
	wsGroup.POST("/:id/reload-secrets", handler.ReloadSecrets)

	createBody := `{"name":"ssh-e2e","type":"ssh-key","value":"-----BEGIN KEY-----","metadata":{"key_type":"ed25519","host":"github.com"}}`
	cReq := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewBufferString(createBody))
	cReq.Header.Set("Content-Type", "application/json")
	cw := httptest.NewRecorder()
	router.ServeHTTP(cw, cReq)
	if cw.Code != http.StatusCreated {
		t.Fatalf("Create: expected 201, got %d: %s", cw.Code, cw.Body.String())
	}
	var created secrets.SecretResponse
	json.Unmarshal(cw.Body.Bytes(), &created)

	bindBody, _ := json.Marshal(map[string][]string{"secretIds": {created.ID}})
	bReq := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-e2e-test/bindings", bytes.NewBuffer(bindBody))
	bReq.Header.Set("Content-Type", "application/json")
	bw := httptest.NewRecorder()
	router.ServeHTTP(bw, bReq)
	if bw.Code != http.StatusNoContent {
		t.Fatalf("Bind: expected 204, got %d: %s", bw.Code, bw.Body.String())
	}

	mu.Lock()
	called := reloadCalled
	body := reloadBody
	mu.Unlock()

	if !called {
		t.Fatal("SetBindings did not trigger reload-secrets call to agentd")
	}

	var secrets []struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	json.Unmarshal(body, &secrets)
	if len(secrets) != 1 || secrets[0].Type != "ssh-key" || secrets[0].Name != "ssh-e2e" {
		t.Errorf("Unexpected secrets payload: %s", string(body))
	}
}

// TestHandler_E2E_BindNoReloadWhenNoPod verifies SetBindings succeeds
// even when the workspace has no running pod (agentd unreachable).
func TestHandler_E2E_BindNoReloadWhenNoPod(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx := context.Background()
	userID := "test-user"
	password := []byte("test-password")
	sessionID := "test-session"

	keyStore := newTestKeyStore()
	dekCache := newTestDEKCache()
	keySvc := secrets.NewKeyService(keyStore, dekCache)
	secretStore := newTestSecretStore()
	svc := secrets.NewSecretService(keySvc, secretStore)

	_, err := keySvc.InitializeUserKeysServerKEK(ctx, userID, "server_kek")
	if err != nil {
		t.Fatalf("InitializeUserKeysServerKEK: %v", err)
	}
	err = keySvc.UnlockDEK(ctx, userID, password, sessionID, time.Hour)
	if err != nil {
		t.Fatalf("UnlockDEK: %v", err)
	}

	handler := NewSecretsHandler(svc)
	handler.SetPodIPResolver(&staticPodIPResolver{addr: ""})

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userID", userID)
		c.Set("sessionID", sessionID)
		c.Next()
	})

	router.POST("/api/v1/secrets", handler.CreateSecret)
	wsGroup := router.Group("/api/v1/workspaces")
	wsGroup.PUT("/:id/bindings", handler.SetBindings)

	createBody := `{"name":"env-e2e","type":"env-secret","value":"secretval","metadata":{"var_name":"MY_VAR"}}`
	cReq := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewBufferString(createBody))
	cReq.Header.Set("Content-Type", "application/json")
	cw := httptest.NewRecorder()
	router.ServeHTTP(cw, cReq)
	if cw.Code != http.StatusCreated {
		t.Fatalf("Create: expected 201, got %d: %s", cw.Code, cw.Body.String())
	}
	var created secrets.SecretResponse
	json.Unmarshal(cw.Body.Bytes(), &created)

	bindBody, _ := json.Marshal(map[string][]string{"secretIds": {created.ID}})
	bReq := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-no-pod/bindings", bytes.NewBuffer(bindBody))
	bReq.Header.Set("Content-Type", "application/json")
	bw := httptest.NewRecorder()
	router.ServeHTTP(bw, bReq)

	if bw.Code != http.StatusNoContent {
		t.Fatalf("Bind should succeed even without a running pod, got %d: %s", bw.Code, bw.Body.String())
	}
}

type staticPodIPResolver struct {
	addr string
}

func (r *staticPodIPResolver) GetWorkspacePodIP(_ context.Context, _, _ string) (string, error) {
	return r.addr, nil
}

// recordingLogger captures Warn calls so tests can verify Bug 2 — that
// failures of the auto-push are no longer silently swallowed.
type recordingLogger struct {
	mu    sync.Mutex
	warns []recordingLoggerEntry
	infos []recordingLoggerEntry
	errs  []recordingLoggerEntry
}

type recordingLoggerEntry struct {
	Msg    string
	Fields []interface{}
}

func (r *recordingLogger) Debug(_ string, _ ...interface{})            {}
func (r *recordingLogger) Info(msg string, fields ...interface{})      { r.append(&r.infos, msg, fields) }
func (r *recordingLogger) Warn(msg string, fields ...interface{})      { r.append(&r.warns, msg, fields) }
func (r *recordingLogger) Error(msg string, _ error, _ ...interface{}) { r.append(&r.errs, msg, nil) }
func (r *recordingLogger) Fatal(_ string, _ error, _ ...interface{})   {}
func (r *recordingLogger) Sync() error                                 { return nil }
func (r *recordingLogger) With(_ ...interface{}) pkginterfaces.LoggerInterface {
	return r
}

func (r *recordingLogger) append(dst *[]recordingLoggerEntry, msg string, fields []interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	*dst = append(*dst, recordingLoggerEntry{Msg: msg, Fields: fields})
}

func (r *recordingLogger) warnCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.warns)
}

// TestHandler_BindLogsReloadFailure is the regression test for Bug 2 in
// worklog 0085: pushSecretsToAgent silently swallowed every error from
// doReload. We now require these to surface in the logger so operators
// can detect "live push" failures even when the bind itself succeeded.
func TestHandler_BindLogsReloadFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx := context.Background()
	userID := "test-user"
	password := []byte("test-password")
	sessionID := "test-session"

	keyStore := newTestKeyStore()
	dekCache := newTestDEKCache()
	keySvc := secrets.NewKeyService(keyStore, dekCache)
	secretStore := newTestSecretStore()
	svc := secrets.NewSecretService(keySvc, secretStore)

	if _, err := keySvc.InitializeUserKeysServerKEK(ctx, userID, "server_kek"); err != nil {
		t.Fatalf("InitializeUserKeysServerKEK: %v", err)
	}
	if err := keySvc.UnlockDEK(ctx, userID, password, sessionID, time.Hour); err != nil {
		t.Fatalf("UnlockDEK: %v", err)
	}

	logger := &recordingLogger{}
	handler := NewSecretsHandler(svc)
	handler.SetLogger(logger)
	// Resolver returns an unreachable address so HTTP push fails.
	handler.SetPodIPResolver(&staticPodIPResolver{addr: "127.0.0.1:1"})

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userID", userID)
		c.Set("sessionID", sessionID)
		c.Next()
	})
	router.POST("/api/v1/secrets", handler.CreateSecret)
	wsGroup := router.Group("/api/v1/workspaces")
	wsGroup.PUT("/:id/bindings", handler.SetBindings)

	createBody := `{"name":"warn-test","type":"env-secret","value":"v1","metadata":{"var_name":"WARN"}}`
	cReq := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewBufferString(createBody))
	cReq.Header.Set("Content-Type", "application/json")
	cw := httptest.NewRecorder()
	router.ServeHTTP(cw, cReq)
	if cw.Code != http.StatusCreated {
		t.Fatalf("Create: expected 201, got %d", cw.Code)
	}
	var created secrets.SecretResponse
	json.Unmarshal(cw.Body.Bytes(), &created)

	bindBody, _ := json.Marshal(map[string][]string{"secretIds": {created.ID}})
	bReq := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-warn/bindings", bytes.NewBuffer(bindBody))
	bReq.Header.Set("Content-Type", "application/json")
	bw := httptest.NewRecorder()
	router.ServeHTTP(bw, bReq)

	if bw.Code != http.StatusNoContent {
		t.Fatalf("Bind should still succeed even when push fails, got %d", bw.Code)
	}
	if logger.warnCount() == 0 {
		t.Fatal("Bug 2 regression: SetBindings push failure must surface in logs as Warn")
	}
}

// stubPasswordVerifier is a deterministic fake for RevealSecret tests.
type stubPasswordVerifier struct {
	expected []byte
	calls    int
}

func (v *stubPasswordVerifier) VerifyPassword(_ context.Context, _ string, password []byte) error {
	v.calls++
	if string(password) == string(v.expected) {
		return nil
	}
	return secrets.ErrInvalidPassword
}

// TestHandler_RevealSecret_RequiresPasswordVerification is the
// regression test for the validator's "RevealSecret is theater"
// finding. Pre-fix, the handler accepted any password (the field was
// declared but never checked). Post-fix:
//   - Without a configured verifier, reveal returns 503.
//   - With a verifier, the wrong password returns 403 and never
//     calls DecryptSecretValue.
//   - The right password returns 200 + plaintext.
func TestHandler_RevealSecret_RequiresPasswordVerification(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx := context.Background()
	userID := "test-user"
	password := []byte("test-password")
	sessionID := "test-session"

	keyStore := newTestKeyStore()
	dekCache := newTestDEKCache()
	keySvc := secrets.NewKeyService(keyStore, dekCache)
	secretStore := newTestSecretStore()
	svc := secrets.NewSecretService(keySvc, secretStore)

	if _, err := keySvc.InitializeUserKeysServerKEK(ctx, userID, "server_kek"); err != nil {
		t.Fatalf("InitializeUserKeysServerKEK: %v", err)
	}
	if err := keySvc.UnlockDEK(ctx, userID, password, sessionID, time.Hour); err != nil {
		t.Fatalf("UnlockDEK: %v", err)
	}

	created, err := svc.CreateSecret(ctx, userID, sessionID, nil, secrets.CreateSecretRequest{
		Name: "reveal-me", Type: secrets.SecretTypeEnvSecret, Value: "real-value",
		Metadata: []byte(`{"var_name":"X"}`),
	})
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}

	mkRouter := func(h *SecretsHandler) *gin.Engine {
		router := gin.New()
		router.Use(func(c *gin.Context) {
			c.Set("userID", userID)
			c.Set("sessionID", sessionID)
			c.Next()
		})
		router.POST("/api/v1/secrets/:id/reveal", h.RevealSecret)
		return router
	}

	t.Run("no verifier configured returns 503", func(t *testing.T) {
		h := NewSecretsHandler(svc)
		body, _ := json.Marshal(map[string]string{"password": "anything"})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/secrets/"+created.ID+"/reveal", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mkRouter(h).ServeHTTP(w, req)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("expected 503 without verifier, got %d", w.Code)
		}
	})

	t.Run("wrong password returns 403 and does not leak plaintext", func(t *testing.T) {
		verifier := &stubPasswordVerifier{expected: password}
		h := NewSecretsHandler(svc)
		h.SetPasswordVerifier(verifier)

		body, _ := json.Marshal(map[string]string{"password": "wrong"})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/secrets/"+created.ID+"/reveal", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mkRouter(h).ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Errorf("expected 403 for wrong password, got %d: %s", w.Code, w.Body.String())
		}
		if verifier.calls != 1 {
			t.Errorf("verifier must be invoked exactly once, got %d", verifier.calls)
		}
		if bytes.Contains(w.Body.Bytes(), []byte("real-value")) {
			t.Errorf("403 response leaked plaintext: %s", w.Body.String())
		}
	})

	t.Run("right password returns 200 and plaintext", func(t *testing.T) {
		verifier := &stubPasswordVerifier{expected: password}
		h := NewSecretsHandler(svc)
		h.SetPasswordVerifier(verifier)

		body, _ := json.Marshal(map[string]string{"password": string(password)})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/secrets/"+created.ID+"/reveal", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mkRouter(h).ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 for correct password, got %d: %s", w.Code, w.Body.String())
		}
		if !bytes.Contains(w.Body.Bytes(), []byte("real-value")) {
			t.Errorf("200 response missing plaintext: %s", w.Body.String())
		}
	})
}

// TestHandler_RevealSecret_CiphertextDecryptFailed_Returns409 verifies the
// failure path observed in production: the user's DEK was rotated (or
// user_keys row rewritten) without re-encrypting their existing user_secrets
// rows. The DEK unwraps fine, but the stored ciphertext is AEAD-incompatible.
//
// Before the fix this returned 500 "internal error" and emitted no audit log.
// After: 409 Conflict with an actionable user-facing message, and a
// secret_audit_log entry with reason=ciphertext_aead_failure.
func TestHandler_RevealSecret_CiphertextDecryptFailed_Returns409(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx := context.Background()
	userID := "test-user"
	password := []byte("test-password")
	sessionID := "test-session"

	keyStore := newTestKeyStore()
	dekCache := newTestDEKCache()
	keySvc := secrets.NewKeyService(keyStore, dekCache)
	secretStore := newTestSecretStore()
	svc := secrets.NewSecretService(keySvc, secretStore)

	if _, err := keySvc.InitializeUserKeysServerKEK(ctx, userID, "server_kek"); err != nil {
		t.Fatalf("InitializeUserKeysServerKEK: %v", err)
	}
	if err := keySvc.UnlockDEK(ctx, userID, password, sessionID, time.Hour); err != nil {
		t.Fatalf("UnlockDEK: %v", err)
	}

	created, err := svc.CreateSecret(ctx, userID, sessionID, nil, secrets.CreateSecretRequest{
		Name: "rotated-out", Type: secrets.SecretTypeEnvSecret, Value: "stale-value",
		Metadata: []byte(`{"var_name":"X"}`),
	})
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}

	// Corrupt the stored ciphertext: simulates DEK rotated without re-encrypting,
	// or storage tampering. The current DEK can no longer authenticate this blob.
	stored, _ := secretStore.GetSecret(ctx, userID, created.ID)
	stored.Ciphertext = []byte("\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d")
	if err := secretStore.UpdateSecret(ctx, stored); err != nil {
		t.Fatalf("UpdateSecret(corrupt): %v", err)
	}

	verifier := &stubPasswordVerifier{expected: password}
	h := NewSecretsHandler(svc)
	h.SetPasswordVerifier(verifier)

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userID", userID)
		c.Set("sessionID", sessionID)
		c.Next()
	})
	router.POST("/api/v1/secrets/:id/reveal", h.RevealSecret)

	body, _ := json.Marshal(map[string]string{"password": string(password)})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/secrets/"+created.ID+"/reveal", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// 409 Conflict — distinct from 500 (was: "internal error") and 403
	// (which means re-authenticate, which would NOT help here).
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for ciphertext mismatch, got %d: %s", w.Code, w.Body.String())
	}
	// User-facing message must be actionable, not "internal error".
	bodyStr := w.Body.String()
	if !bytes.Contains(w.Body.Bytes(), []byte("cannot be decrypted")) {
		t.Errorf("expected actionable error message mentioning decryption, got: %s", bodyStr)
	}
	if bytes.Contains(w.Body.Bytes(), []byte("internal error")) {
		t.Errorf("must not return generic 'internal error' — was the regression we are fixing: %s", bodyStr)
	}

	// Audit log must contain a structured entry with reason=ciphertext_aead_failure
	// so operators can detect this scenario from logs alone.
	found := false
	for _, e := range secretStore.audit {
		if e.Action != "secret_decrypt_failed" || len(e.Metadata) == 0 {
			continue
		}
		var meta map[string]string
		if err := json.Unmarshal(e.Metadata, &meta); err != nil {
			continue
		}
		if meta["reason"] != "ciphertext_aead_failure" {
			continue
		}
		found = true
		// Verify the entry is keyed to the right user and secret
		if e.UserID != userID {
			t.Errorf("audit entry user mismatch: got %q want %q", e.UserID, userID)
		}
		if e.SecretID == nil || *e.SecretID != created.ID {
			t.Errorf("audit entry secretID mismatch")
		}
		if meta["name"] != "rotated-out" {
			t.Errorf("audit metadata missing secret name: %v", meta)
		}
		break
	}
	if !found {
		t.Errorf("expected secret_audit_log entry with reason=ciphertext_aead_failure; got %d entries", len(secretStore.audit))
	}
}

// TestHandler_RevealSecret_DEKUnavailable_Returns403 verifies the existing
// distinct path: DEK is not in cache (session expired, user not logged in).
// This is correctly handled with 403 + "re-authenticate" — re-authenticating
// will fix it, unlike ErrCiphertextDecryptFailed.
func TestHandler_RevealSecret_DEKUnavailable_Returns403(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx := context.Background()
	userID := "test-user"
	password := []byte("test-password")
	sessionID := "test-session"

	keyStore := newTestKeyStore()
	dekCache := newTestDEKCache()
	keySvc := secrets.NewKeyService(keyStore, dekCache)
	secretStore := newTestSecretStore()
	svc := secrets.NewSecretService(keySvc, secretStore)

	if _, err := keySvc.InitializeUserKeysServerKEK(ctx, userID, "server_kek"); err != nil {
		t.Fatalf("InitializeUserKeysServerKEK: %v", err)
	}
	if err := keySvc.UnlockDEK(ctx, userID, password, sessionID, time.Hour); err != nil {
		t.Fatalf("UnlockDEK: %v", err)
	}
	created, err := svc.CreateSecret(ctx, userID, sessionID, nil, secrets.CreateSecretRequest{
		Name: "expired-session", Type: secrets.SecretTypeEnvSecret, Value: "v",
		Metadata: []byte(`{"var_name":"X"}`),
	})
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}

	// Evict the DEK to simulate session expiry.
	_ = dekCache.EvictDEK(ctx, sessionID)

	verifier := &stubPasswordVerifier{expected: password}
	h := NewSecretsHandler(svc)
	h.SetPasswordVerifier(verifier)

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userID", userID)
		c.Set("sessionID", sessionID)
		c.Next()
	})
	router.POST("/api/v1/secrets/:id/reveal", h.RevealSecret)

	body, _ := json.Marshal(map[string]string{"password": string(password)})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/secrets/"+created.ID+"/reveal", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for DEK unavailable, got %d: %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("re-authenticate")) {
		t.Errorf("expected message instructing re-authenticate, got: %s", w.Body.String())
	}

	// Audit log entry distinguishes this case via reason=dek_unavailable.
	found := false
	for _, e := range secretStore.audit {
		if e.Action != "secret_decrypt_failed" || len(e.Metadata) == 0 {
			continue
		}
		var meta map[string]string
		if err := json.Unmarshal(e.Metadata, &meta); err != nil {
			continue
		}
		if meta["reason"] == "dek_unavailable" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected secret_audit_log entry with reason=dek_unavailable")
	}
}
