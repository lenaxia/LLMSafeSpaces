// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// setupTestRouter creates a Gin router with the secrets handler wired up,
// using in-memory mocks for the key and secret stores.
func setupTestRouter(t *testing.T) (*gin.Engine, *secrets.SecretService, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	keyStore := newTestKeyStore()
	dekCache := newTestDEKCache()
	keySvc := secrets.NewKeyService(keyStore, dekCache)
	testProv, _ := secrets.NewStaticKeyProvider(make([]byte, 32))
	keySvc.SetAPIKeyStore(nil, testProv)
	secretStore := newTestSecretStore()
	svc := secrets.NewSecretService(keySvc, secretStore)

	ctx := context.Background()
	userID := "test-user"
	password := []byte("test-password")
	sessionID := "test-session"

	err := keySvc.InitializeUserKeysServerKEK(ctx, userID, "server_kek")
	if err != nil {
		t.Fatalf("InitializeUserKeys: %v", err)
	}
	err = keySvc.UnlockDEK(ctx, userID, password, sessionID, time.Hour)
	if err != nil {
		t.Fatalf("UnlockDEK: %v", err)
	}

	handler := NewSecretsHandler(svc)

	router := gin.New()

	// Simulate auth middleware by injecting userID and sessionID
	router.Use(func(c *gin.Context) {
		c.Set("userID", userID)
		c.Set("sessionID", sessionID)
		c.Next()
	})

	// Register routes
	secretsGroup := router.Group("/api/v1/secrets")
	secretsGroup.POST("", handler.CreateSecret)
	secretsGroup.GET("", handler.ListSecrets)
	secretsGroup.GET("/:id", handler.GetSecret)
	secretsGroup.PUT("/:id", handler.UpdateSecret)
	secretsGroup.DELETE("/:id", handler.DeleteSecret)
	secretsGroup.GET("/audit", handler.GetAuditLog)

	wsGroup := router.Group("/api/v1/workspaces")
	wsGroup.PUT("/:id/bindings", handler.SetBindings)
	wsGroup.GET("/:id/bindings", handler.GetBindings)

	return router, svc, sessionID
}

func TestHandler_CreateSecret_Success(t *testing.T) {
	router, _, _ := setupTestRouter(t)

	body := `{"name":"my-key","type":"api-key","value":"sk-secret","metadata":{"kind":"openai","slug":"openai"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("Expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var resp secrets.SecretResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Name != "my-key" {
		t.Errorf("Expected name 'my-key', got '%s'", resp.Name)
	}
	if resp.Type != secrets.SecretTypeAPIKey {
		t.Errorf("Expected type api-key, got %s", resp.Type)
	}
}

func TestHandler_CreateSecret_InvalidType(t *testing.T) {
	router, _, _ := setupTestRouter(t)

	body := `{"name":"test","type":"bad-type","value":"val"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandler_CreateSecret_MissingMetadata(t *testing.T) {
	router, _, _ := setupTestRouter(t)

	body := `{"name":"ssh","type":"ssh-key","value":"key-data"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandler_CreateSecret_Duplicate(t *testing.T) {
	router, _, _ := setupTestRouter(t)

	body := `{"name":"dup","type":"api-key","value":"v1","metadata":{"kind":"x","slug":"x"}}`
	req1 := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewBufferString(body))
	req1.Header.Set("Content-Type", "application/json")
	w1 := httptest.NewRecorder()
	router.ServeHTTP(w1, req1)

	if w1.Code != http.StatusCreated {
		t.Fatalf("First create failed: %d", w1.Code)
	}

	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewBufferString(body))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, req2)

	if w2.Code != http.StatusConflict {
		t.Errorf("Expected 409 for duplicate, got %d: %s", w2.Code, w2.Body.String())
	}
}

func TestHandler_ListSecrets(t *testing.T) {
	router, _, _ := setupTestRouter(t)

	// Create two secrets
	for _, name := range []string{"key-1", "key-2"} {
		body := `{"name":"` + name + `","type":"api-key","value":"v","metadata":{"kind":"x","slug":"x"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/secrets", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	var resp struct {
		Secrets []secrets.SecretResponse `json:"secrets"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Secrets) != 2 {
		t.Errorf("Expected 2 secrets, got %d", len(resp.Secrets))
	}
}

func TestHandler_GetSecret_NotFound(t *testing.T) {
	router, _, _ := setupTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/secrets/nonexistent", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("Expected 404, got %d", w.Code)
	}
}

func TestHandler_UpdateSecret(t *testing.T) {
	router, _, _ := setupTestRouter(t)

	// Create
	createBody := `{"name":"updatable","type":"api-key","value":"old","metadata":{"kind":"x","slug":"x"}}`
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewBufferString(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	cw := httptest.NewRecorder()
	router.ServeHTTP(cw, createReq)

	var created secrets.SecretResponse
	json.Unmarshal(cw.Body.Bytes(), &created)

	// Update
	updateBody := `{"value":"new-value"}`
	updateReq := httptest.NewRequest(http.MethodPut, "/api/v1/secrets/"+created.ID, bytes.NewBufferString(updateBody))
	updateReq.Header.Set("Content-Type", "application/json")
	uw := httptest.NewRecorder()
	router.ServeHTTP(uw, updateReq)

	if uw.Code != http.StatusNoContent {
		t.Errorf("Expected 204, got %d: %s", uw.Code, uw.Body.String())
	}
}

func TestHandler_DeleteSecret(t *testing.T) {
	router, _, _ := setupTestRouter(t)

	// Create
	createBody := `{"name":"deletable","type":"api-key","value":"v","metadata":{"kind":"x","slug":"x"}}`
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewBufferString(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	cw := httptest.NewRecorder()
	router.ServeHTTP(cw, createReq)

	var created secrets.SecretResponse
	json.Unmarshal(cw.Body.Bytes(), &created)

	// Delete
	delReq := httptest.NewRequest(http.MethodDelete, "/api/v1/secrets/"+created.ID, nil)
	dw := httptest.NewRecorder()
	router.ServeHTTP(dw, delReq)

	if dw.Code != http.StatusNoContent {
		t.Errorf("Expected 204, got %d", dw.Code)
	}

	// Verify gone
	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/secrets/"+created.ID, nil)
	gw := httptest.NewRecorder()
	router.ServeHTTP(gw, getReq)

	if gw.Code != http.StatusNotFound {
		t.Errorf("Expected 404 after delete, got %d", gw.Code)
	}
}

func TestHandler_Bindings(t *testing.T) {
	router, _, _ := setupTestRouter(t)

	// Create a secret
	createBody := `{"name":"bindable","type":"api-key","value":"v","metadata":{"kind":"x","slug":"x"}}`
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewBufferString(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	cw := httptest.NewRecorder()
	router.ServeHTTP(cw, createReq)

	var created secrets.SecretResponse
	json.Unmarshal(cw.Body.Bytes(), &created)

	// Set bindings
	bindBody := `{"secretIds":["` + created.ID + `"]}`
	bindReq := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-1/bindings", bytes.NewBufferString(bindBody))
	bindReq.Header.Set("Content-Type", "application/json")
	bw := httptest.NewRecorder()
	router.ServeHTTP(bw, bindReq)

	if bw.Code != http.StatusNoContent {
		t.Errorf("Expected 204, got %d: %s", bw.Code, bw.Body.String())
	}

	// Get bindings
	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/bindings", nil)
	gw := httptest.NewRecorder()
	router.ServeHTTP(gw, getReq)

	if gw.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", gw.Code)
	}

	var resp secrets.BindingsResponse
	json.Unmarshal(gw.Body.Bytes(), &resp)
	if len(resp.Bindings) != 1 {
		t.Errorf("Expected 1 binding, got %d", len(resp.Bindings))
	}
}

func TestHandler_AuditLog(t *testing.T) {
	router, _, _ := setupTestRouter(t)

	// Create a secret (generates audit entry)
	body := `{"name":"audited","type":"api-key","value":"v","metadata":{"kind":"x","slug":"x"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Query audit
	auditReq := httptest.NewRequest(http.MethodGet, "/api/v1/secrets/audit", nil)
	aw := httptest.NewRecorder()
	router.ServeHTTP(aw, auditReq)

	if aw.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", aw.Code)
	}

	var resp struct {
		Entries []secrets.AuditEntry `json:"entries"`
	}
	json.Unmarshal(aw.Body.Bytes(), &resp)
	if len(resp.Entries) == 0 {
		t.Error("Expected at least 1 audit entry")
	}
}
