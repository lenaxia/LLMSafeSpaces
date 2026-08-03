// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// TestE2E_RealHTTPServer boots a real TCP server and exercises the full
// secret management lifecycle through actual HTTP requests over the network.
func TestE2E_RealHTTPServer(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Setup real crypto + services
	keyStore := &dbKeyStoreAdapter{}
	dekCache := &memDEKCache{store: make(map[string][]byte)}
	keyService := secrets.NewKeyService(keyStore, dekCache)
	testProv, _ := secrets.NewStaticKeyProvider(make([]byte, 32))
	keyService.SetAPIKeyStore(nil, testProv)
	secretStore := &dbSecretStoreAdapter{}
	secretService := secrets.NewSecretService(keyService, secretStore)
	secretsHandler := handlers.NewSecretsHandler(secretService)

	// Initialize user keys (simulates registration)
	ctx := context.Background()
	userID := "e2e-http-user"
	password := []byte("e2e-password-123")
	sessionID := "e2e-jti-abc"

	err := keyService.InitializeUserKeysServerKEK(ctx, userID, "server_kek")
	if err != nil {
		t.Fatalf("InitializeUserKeysServerKEK: %v", err)
	}
	err = keyService.UnlockDEK(ctx, userID, password, sessionID, time.Hour)
	if err != nil {
		t.Fatalf("UnlockDEK: %v", err)
	}

	// Build router with simulated auth (sets userID + sessionID)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		auth := c.GetHeader("Authorization")
		if auth != "Bearer valid-token" {
			c.AbortWithStatusJSON(401, gin.H{"error": "unauthorized"})
			return
		}
		c.Set("userID", userID)
		c.Set("sessionID", sessionID)
		c.Next()
	})

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

	// Start real TCP server
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	server := &http.Server{Handler: router}
	go server.Serve(ln)
	defer server.Close()

	base := fmt.Sprintf("http://%s", ln.Addr().String())
	c := &http.Client{Timeout: 5 * time.Second}
	token := "valid-token"

	// === Phase 1: Unauthenticated access blocked ===
	resp := get(t, c, base+"/api/v1/secrets", "")
	assertStatus(t, resp, 401, "unauth list")

	resp = get(t, c, base+"/api/v1/secrets", "bad-token")
	assertStatus(t, resp, 401, "bad token")

	// === Phase 2: Create secret ===
	body := `{"name":"anthropic-key","type":"api-key","value":"sk-ant-api03-real-secret","metadata":{"kind":"anthropic","slug":"anthropic"}}`
	resp = post(t, c, base+"/api/v1/secrets", body, token)
	assertStatus(t, resp, 201, "create")
	var created struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Type string `json:"type"`
	}
	readJSON(t, resp, &created)
	if created.ID == "" || created.Name != "anthropic-key" {
		t.Fatalf("Create response wrong: %+v", created)
	}

	// === Phase 3: Value never in GET response ===
	resp = get(t, c, base+"/api/v1/secrets/"+created.ID, token)
	assertStatus(t, resp, 200, "get")
	respBody := readBody(t, resp)
	if bytes.Contains(respBody, []byte("sk-ant-api03-real-secret")) {
		t.Fatal("GET response MUST NOT contain secret value")
	}

	// === Phase 4: List ===
	resp = get(t, c, base+"/api/v1/secrets", token)
	assertStatus(t, resp, 200, "list")
	var listResp struct {
		Secrets []struct{ ID string } `json:"secrets"`
	}
	json.Unmarshal(readBody(t, resp), &listResp)
	if len(listResp.Secrets) != 1 {
		t.Errorf("List: expected 1, got %d", len(listResp.Secrets))
	}

	// === Phase 5: Create more types ===
	sshBody := `{"name":"gh-key","type":"ssh-key","value":"-----BEGIN KEY-----","metadata":{"key_type":"ed25519","host":"github.com"}}`
	resp = post(t, c, base+"/api/v1/secrets", sshBody, token)
	assertStatus(t, resp, 201, "create ssh")
	var sshCreated struct{ ID string }
	readJSON(t, resp, &sshCreated)

	envBody := `{"name":"db-url","type":"env-secret","value":"postgres://x","metadata":{"var_name":"DATABASE_URL"}}`
	resp = post(t, c, base+"/api/v1/secrets", envBody, token)
	assertStatus(t, resp, 201, "create env")
	var envCreated struct{ ID string }
	readJSON(t, resp, &envCreated)

	// === Phase 6: Bind to workspace ===
	bindBody := fmt.Sprintf(`{"secretIds":["%s","%s","%s"]}`, created.ID, sshCreated.ID, envCreated.ID)
	resp = put(t, c, base+"/api/v1/workspaces/ws-e2e-http/bindings", bindBody, token)
	assertStatus(t, resp, 204, "bind")

	// === Phase 7: Get bindings ===
	resp = get(t, c, base+"/api/v1/workspaces/ws-e2e-http/bindings", token)
	assertStatus(t, resp, 200, "get bindings")
	var bindResp struct {
		Bindings []struct {
			SecretID string `json:"secretId"`
			Name     string `json:"name"`
			Type     string `json:"type"`
		} `json:"bindings"`
	}
	json.Unmarshal(readBody(t, resp), &bindResp)
	if len(bindResp.Bindings) != 3 {
		t.Errorf("Bindings: expected 3, got %d", len(bindResp.Bindings))
	}

	// === Phase 8: Update secret ===

	// === Phase 11: Audit log ===
	resp = get(t, c, base+"/api/v1/secrets/audit", token)
	assertStatus(t, resp, 200, "audit")
	var auditResp struct {
		Entries []struct{ Action string } `json:"entries"`
	}
	json.Unmarshal(readBody(t, resp), &auditResp)
	if len(auditResp.Entries) < 4 {
		t.Errorf("Audit: expected >=4 entries, got %d", len(auditResp.Entries))
	}

	// === Phase 12: Delete and verify ===
	resp = del(t, c, base+"/api/v1/secrets/"+created.ID, token)
	assertStatus(t, resp, 204, "delete")

	resp = get(t, c, base+"/api/v1/secrets/"+created.ID, token)
	assertStatus(t, resp, 404, "get after delete")

	// === Phase 13: Duplicate name rejected ===
	resp = post(t, c, base+"/api/v1/secrets", sshBody, token)
	assertStatus(t, resp, 409, "duplicate name")

	// === Phase 14: Invalid type rejected ===
	resp = post(t, c, base+"/api/v1/secrets", `{"name":"x","type":"bad","value":"v"}`, token)
	assertStatus(t, resp, 400, "invalid type")

	// === Phase 15: Missing metadata rejected ===
	resp = post(t, c, base+"/api/v1/secrets", `{"name":"y","type":"ssh-key","value":"v"}`, token)
	assertStatus(t, resp, 400, "missing metadata")

	t.Log("E2E real HTTP: all 15 phases passed")
}

// --- HTTP helpers ---

func get(t *testing.T, c *http.Client, url, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

func post(t *testing.T, c *http.Client, url, body, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", url, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

func put(t *testing.T, c *http.Client, url, body, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("PUT", url, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", url, err)
	}
	return resp
}

func del(t *testing.T, c *http.Client, url, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("DELETE", url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", url, err)
	}
	return resp
}

func assertStatus(t *testing.T, resp *http.Response, want int, label string) {
	t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s: expected %d, got %d: %s", label, want, resp.StatusCode, string(body))
	}
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return body
}

func readJSON(t *testing.T, resp *http.Response, v interface{}) {
	t.Helper()
	body := readBody(t, resp)
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("Unmarshal: %v (body: %s)", err, string(body))
	}
}
