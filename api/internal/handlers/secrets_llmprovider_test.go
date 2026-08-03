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
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// TestHandler_E2E_LLMProvider_BindTriggersReloadWithFormattedConfig exercises
// the full llm-provider path: create secret → bind → reload pushes to agentd
// → agentd receives the correct secrets.json payload containing the provider
// data. This validates that the API layer correctly encrypts, stores, binds,
// decrypts, and pushes llm-provider secrets to the materializer.
func TestHandler_E2E_LLMProvider_BindTriggersReloadWithFormattedConfig(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var (
		mu           sync.Mutex
		reloadCalled bool
		reloadBody   []byte
	)

	// Mock agentd on port 4097 — receives the reload-secrets push.
	agentdListener, err := net.Listen("tcp", "127.0.0.1:4097")
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
	testProv, _ := secrets.NewStaticKeyProvider(make([]byte, 32))
	keySvc.SetAPIKeyStore(nil, testProv)
	secretStore := newTestSecretStore()
	svc := secrets.NewSecretService(keySvc, secretStore)

	err = keySvc.InitializeUserKeysServerKEK(ctx, userID, "server_kek")
	require.NoError(t, err)
	err = keySvc.UnlockDEK(ctx, userID, password, sessionID, time.Hour)
	require.NoError(t, err)

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

	// Create an llm-provider secret with structured provider data.
	providerData := map[string]any{
		"kind":    "anthropic",
		"slug":    "anthropic",
		"apiKey":  "sk-ant-api03-test-key",
		"baseURL": "",
		"default": "anthropic/claude-sonnet-4-5",
		"models": []map[string]any{
			{"id": "claude-sonnet-4-5", "label": "Claude Sonnet 4.5"},
			{"id": "claude-haiku-4-5"},
		},
	}
	providerJSON, _ := json.Marshal(providerData)
	createBody, _ := json.Marshal(map[string]any{
		"name":     "anthropic-prod",
		"type":     "llm-provider",
		"value":    string(providerJSON),
		"metadata": map[string]string{},
	})

	cReq := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewBuffer(createBody))
	cReq.Header.Set("Content-Type", "application/json")
	cw := httptest.NewRecorder()
	router.ServeHTTP(cw, cReq)
	require.Equal(t, http.StatusCreated, cw.Code, "Create response: %s", cw.Body.String())

	var created secrets.SecretResponse
	require.NoError(t, json.Unmarshal(cw.Body.Bytes(), &created))
	require.Equal(t, secrets.SecretTypeLLMProvider, created.Type)
	require.Equal(t, "anthropic-prod", created.Name)

	// Bind the secret to a workspace — this triggers pushSecretsToAgent.
	bindBody, _ := json.Marshal(map[string][]string{"secretIds": {created.ID}})
	bReq := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-llm-test/bindings", bytes.NewBuffer(bindBody))
	bReq.Header.Set("Content-Type", "application/json")
	bw := httptest.NewRecorder()
	router.ServeHTTP(bw, bReq)
	require.Equal(t, http.StatusNoContent, bw.Code, "Bind response: %s", bw.Body.String())

	// Verify agentd received the reload call with correct payload.
	mu.Lock()
	called := reloadCalled
	body := reloadBody
	mu.Unlock()

	require.True(t, called, "SetBindings must trigger reload-secrets call to agentd")

	// Parse the secrets.json payload that was pushed to the pod.
	var injected []struct {
		Type      string          `json:"type"`
		Name      string          `json:"name"`
		Metadata  json.RawMessage `json:"metadata"`
		Plaintext string          `json:"plaintext"`
	}
	require.NoError(t, json.Unmarshal(body, &injected))
	require.Len(t, injected, 1)
	require.Equal(t, "llm-provider", string(injected[0].Type))
	require.Equal(t, "anthropic", injected[0].Name)

	// Verify the plaintext is the original provider JSON (decrypted correctly).
	var decryptedProvider map[string]any
	require.NoError(t, json.Unmarshal([]byte(injected[0].Plaintext), &decryptedProvider))
	require.Equal(t, "anthropic", decryptedProvider["kind"])
	require.Equal(t, "anthropic", decryptedProvider["slug"])
	require.Equal(t, "sk-ant-api03-test-key", decryptedProvider["apiKey"])
	require.Equal(t, "anthropic/claude-sonnet-4-5", decryptedProvider["default"])
}

// TestHandler_E2E_LLMProvider_MultipleProviders_Bind verifies that binding
// multiple llm-provider secrets produces a payload with all providers.
func TestHandler_E2E_LLMProvider_MultipleProviders_Bind(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var (
		mu         sync.Mutex
		reloadBody []byte
	)

	agentdListener, err := net.Listen("tcp", "127.0.0.1:4097")
	if err != nil {
		t.Skip("port 4097 not available")
	}
	agentd := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		reloadBody = body
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"reloaded": 2, "restarted": false})
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
	testProv, _ := secrets.NewStaticKeyProvider(make([]byte, 32))
	keySvc.SetAPIKeyStore(nil, testProv)
	secretStore := newTestSecretStore()
	svc := secrets.NewSecretService(keySvc, secretStore)

	err = keySvc.InitializeUserKeysServerKEK(ctx, userID, "server_kek")
	require.NoError(t, err)
	err = keySvc.UnlockDEK(ctx, userID, password, sessionID, time.Hour)
	require.NoError(t, err)

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

	// Create two llm-provider secrets.
	providers := []map[string]any{
		{"kind": "anthropic",
			"slug": "anthropic", "apiKey": "sk-ant-123", "default": "anthropic/claude-sonnet-4-5"},
		{"kind": "openai",
			"slug": "openai", "apiKey": "sk-oai-456", "default": "openai/gpt-4o"},
	}

	var secretIDs []string
	for i, prov := range providers {
		provJSON, _ := json.Marshal(prov)
		body, _ := json.Marshal(map[string]any{
			"name":     prov["slug"],
			"type":     "llm-provider",
			"value":    string(provJSON),
			"metadata": map[string]string{},
		})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusCreated, w.Code, "Create provider %d: %s", i, w.Body.String())

		var resp secrets.SecretResponse
		json.Unmarshal(w.Body.Bytes(), &resp)
		secretIDs = append(secretIDs, resp.ID)
	}

	// Bind both to workspace.
	bindBody, _ := json.Marshal(map[string][]string{"secretIds": secretIDs})
	bReq := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-multi/bindings", bytes.NewBuffer(bindBody))
	bReq.Header.Set("Content-Type", "application/json")
	bw := httptest.NewRecorder()
	router.ServeHTTP(bw, bReq)
	require.Equal(t, http.StatusNoContent, bw.Code)

	// Verify payload has both providers.
	mu.Lock()
	body := reloadBody
	mu.Unlock()

	var injected []struct {
		Type      string `json:"type"`
		Plaintext string `json:"plaintext"`
	}
	require.NoError(t, json.Unmarshal(body, &injected))
	require.Len(t, injected, 2, "Both llm-provider secrets should be in payload")

	for _, s := range injected {
		require.Equal(t, "llm-provider", s.Type)
		var prov map[string]any
		require.NoError(t, json.Unmarshal([]byte(s.Plaintext), &prov))
		require.Contains(t, []any{"anthropic", "openai"}, prov["slug"])
	}
}
