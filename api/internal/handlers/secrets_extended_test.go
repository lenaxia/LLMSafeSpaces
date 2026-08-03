// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

func setupUnauthRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	keyStore := newTestKeyStore()
	dekCache := newTestDEKCache()
	keySvc := secrets.NewKeyService(keyStore, dekCache)
	testProv, _ := secrets.NewStaticKeyProvider(make([]byte, 32))
	keySvc.SetAPIKeyStore(nil, testProv)
	secretStore := newTestSecretStore()
	svc := secrets.NewSecretService(keySvc, secretStore)
	handler := NewSecretsHandler(svc)

	router := gin.New()
	// NO auth middleware — simulates unauthenticated request
	secretsGroup := router.Group("/api/v1/secrets")
	secretsGroup.POST("", handler.CreateSecret)
	secretsGroup.GET("", handler.ListSecrets)
	secretsGroup.GET("/:id", handler.GetSecret)
	secretsGroup.PUT("/:id", handler.UpdateSecret)
	secretsGroup.DELETE("/:id", handler.DeleteSecret)

	return router
}

func TestHandler_CreateSecret_Unauthenticated(t *testing.T) {
	router := setupUnauthRouter(t)

	body := `{"name":"test","type":"api-key","value":"v","metadata":{"kind":"x","slug":"x"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401, got %d", w.Code)
	}
}

func TestHandler_ListSecrets_Unauthenticated(t *testing.T) {
	router := setupUnauthRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/secrets", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401, got %d", w.Code)
	}
}

func TestHandler_GetSecret_Unauthenticated(t *testing.T) {
	router := setupUnauthRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/secrets/some-id", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401, got %d", w.Code)
	}
}

func TestHandler_UpdateSecret_Unauthenticated(t *testing.T) {
	router := setupUnauthRouter(t)

	body := `{"value":"new"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/secrets/some-id", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401, got %d", w.Code)
	}
}

func TestHandler_DeleteSecret_Unauthenticated(t *testing.T) {
	router := setupUnauthRouter(t)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/secrets/some-id", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401, got %d", w.Code)
	}
}

func TestHandler_CreateSecret_InvalidJSON(t *testing.T) {
	router, _, _ := setupTestRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewBufferString("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected 400, got %d", w.Code)
	}
}

func TestHandler_CreateSecret_EmptyBody(t *testing.T) {
	router, _, _ := setupTestRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewBufferString("{}"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandler_UpdateSecret_InvalidJSON(t *testing.T) {
	router, _, _ := setupTestRouter(t)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/secrets/some-id", bytes.NewBufferString("bad"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected 400, got %d", w.Code)
	}
}

func TestHandler_UpdateSecret_NotFound(t *testing.T) {
	router, _, _ := setupTestRouter(t)

	body := `{"value":"new"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/secrets/nonexistent", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("Expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandler_DeleteSecret_NotFound(t *testing.T) {
	router, _, _ := setupTestRouter(t)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/secrets/nonexistent", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("Expected 404, got %d", w.Code)
	}
}

func TestHandler_ListSecrets_Empty(t *testing.T) {
	router, _, _ := setupTestRouter(t)

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
	if resp.Secrets == nil {
		t.Error("Secrets should be empty array, not null")
	}
	if len(resp.Secrets) != 0 {
		t.Errorf("Expected 0 secrets, got %d", len(resp.Secrets))
	}
}

func TestHandler_CreateSecret_ResponseNeverContainsValue(t *testing.T) {
	router, _, _ := setupTestRouter(t)

	body := `{"name":"secret-val","type":"api-key","value":"super-secret-key-12345","metadata":{"kind":"x","slug":"x"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("Expected 201, got %d", w.Code)
	}

	// Response body should NOT contain the secret value
	respBody := w.Body.String()
	if bytes.Contains([]byte(respBody), []byte("super-secret-key-12345")) {
		t.Error("Response should NEVER contain the secret value")
	}
}

func TestHandler_GetSecret_ResponseNeverContainsValue(t *testing.T) {
	router, _, _ := setupTestRouter(t)

	// Create
	createBody := `{"name":"no-leak","type":"api-key","value":"do-not-leak-this","metadata":{"kind":"x","slug":"x"}}`
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewBufferString(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	cw := httptest.NewRecorder()
	router.ServeHTTP(cw, createReq)

	var created secrets.SecretResponse
	json.Unmarshal(cw.Body.Bytes(), &created)

	// Get
	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/secrets/"+created.ID, nil)
	gw := httptest.NewRecorder()
	router.ServeHTTP(gw, getReq)

	respBody := gw.Body.String()
	if bytes.Contains([]byte(respBody), []byte("do-not-leak-this")) {
		t.Error("GET response should NEVER contain the secret value")
	}
}

func TestHandler_ListSecrets_ResponseNeverContainsValues(t *testing.T) {
	router, _, _ := setupTestRouter(t)

	// Create
	body := `{"name":"list-leak","type":"api-key","value":"hidden-value-xyz","metadata":{"kind":"x","slug":"x"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// List
	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/secrets", nil)
	lw := httptest.NewRecorder()
	router.ServeHTTP(lw, listReq)

	respBody := lw.Body.String()
	if bytes.Contains([]byte(respBody), []byte("hidden-value-xyz")) {
		t.Error("List response should NEVER contain secret values")
	}
}

func TestHandler_SetBindings_InvalidJSON(t *testing.T) {
	router, _, _ := setupTestRouter(t)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-1/bindings", bytes.NewBufferString("bad"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected 400, got %d", w.Code)
	}
}

func TestHandler_SetBindings_NonexistentSecret(t *testing.T) {
	router, _, _ := setupTestRouter(t)

	body := `{"secretIds":["nonexistent-id"]}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-1/bindings", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("Expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandler_GetBindings_Empty(t *testing.T) {
	router, _, _ := setupTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-empty/bindings", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	var resp secrets.BindingsResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Bindings == nil {
		t.Error("Bindings should be empty array, not null")
	}
}
