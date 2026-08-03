package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	pkgutil "github.com/lenaxia/llmsafespaces/pkg/utilities"
)

func setupDEKRegressionRouter(t *testing.T) (
	router *gin.Engine,
	svc *Service,
	db *apiKeyAwareDB,
	dekCache *memDEKCache,
	keySvc *secrets.KeyService,
	secretSvc *secrets.SecretService,
	secretsHandler *handlers.SecretsHandler,
	masterKey []byte,
) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	cfg := testConfig()
	log := testLogger()
	db = &apiKeyAwareDB{
		users:   make(map[string]*types.User),
		apiKeys: make(map[string]*types.APIKey),
	}
	cache := &mockCache{}

	svc, err := New(cfg, log, db, cache)
	if err != nil {
		t.Fatalf("New auth: %v", err)
	}

	keyStore := &memKeyStore{records: make(map[string]*secrets.UserKeyRecord)}
	dekCache = &memDEKCache{store: make(map[string][]byte)}
	keySvc = secrets.NewKeyService(keyStore, dekCache)
	secretStore := &memSecretStore{
		secrets:  make(map[string]*secrets.UserSecret),
		bindings: make(map[string][]string),
	}
	secretSvc = secrets.NewSecretService(keySvc, secretStore)
	secretsHandler = handlers.NewSecretsHandler(secretSvc)

	masterKey = make([]byte, 32)
	_, _ = rand.Read(masterKey)
	svc.SetMasterKey(masterKey)
	svc.SetKeyService(keySvc)

	router = gin.New()

	router.POST("/api/v1/auth/register", func(c *gin.Context) {
		var req types.RegisterRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		resp, err := svc.Register(c.Request.Context(), req)
		if err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		c.JSON(201, resp)
	})
	router.POST("/api/v1/auth/login", func(c *gin.Context) {
		var req types.LoginRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		resp, err := svc.Login(c.Request.Context(), req)
		if err != nil {
			c.JSON(401, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, resp)
	})

	authed := router.Group("/api/v1")
	authed.Use(svc.AuthMiddleware())
	authed.POST("/api-keys", func(c *gin.Context) {
		var req types.CreateAPIKeyRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		sid, _ := c.Get("sessionID")
		sidStr, _ := sid.(string)
		apiKey, err := svc.CreateAPIKey(c.Request.Context(), svc.GetUserID(c), req, sidStr, nil)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(201, apiKey)
	})
	authed.POST("/secrets", secretsHandler.CreateSecret)
	authed.GET("/secrets", secretsHandler.ListSecrets)

	return
}

func startDEKTestServer(t *testing.T, router *gin.Engine) (*http.Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: router}
	go func() { _ = srv.Serve(ln) }()
	return srv, "http://" + ln.Addr().String()
}

func registerLoginCreateDEK(t *testing.T, client *http.Client, base, email, password string) (jwtToken string, rawAPIKey string) {
	t.Helper()
	resp := dekDoPost(t, client, base+"/api/v1/auth/register",
		`{"username":"`+email+`","email":"`+email+`","password":"`+password+`"}`, "")
	if resp.StatusCode != 201 {
		t.Fatalf("Register: %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = dekDoPost(t, client, base+"/api/v1/auth/login",
		`{"email":"`+email+`","password":"`+password+`"}`, "")
	if resp.StatusCode != 200 {
		t.Fatalf("Login: %d", resp.StatusCode)
	}
	var loginResp struct{ Token string }
	_ = json.NewDecoder(resp.Body).Decode(&loginResp)
	resp.Body.Close()
	jwtToken = loginResp.Token

	resp = dekDoPost(t, client, base+"/api/v1/api-keys",
		`{"name":"dek-key","decryptAccess":true}`, jwtToken)
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("Create API key with decryptAccess: %d: %s", resp.StatusCode, body)
	}
	var apiKeyResp struct {
		Key string `json:"key"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&apiKeyResp)
	resp.Body.Close()
	rawAPIKey = apiKeyResp.Key
	return
}

func dekDoPost(t *testing.T, c *http.Client, url, body, token string) *http.Response {
	t.Helper()
	var req *http.Request
	var err error
	if body != "" {
		req, err = http.NewRequest(http.MethodPost, url, bytes.NewBufferString(body))
	} else {
		req, err = http.NewRequest(http.MethodPost, url, nil)
	}
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func TestE2E_APIKey_WithoutDecryptAccess_SecretsOperation403(t *testing.T) {
	router, _, _, _, _, _, _, _ := setupDEKRegressionRouter(t)
	srv, base := startDEKTestServer(t, router)
	defer srv.Close()

	client := &http.Client{Timeout: 30 * time.Second}

	resp := dekDoPost(t, client, base+"/api/v1/auth/register",
		`{"username":"nodek","email":"nodek@test.com","password":"secure-password-123"}`, "")
	if resp.StatusCode != 201 {
		t.Fatalf("Register: %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = dekDoPost(t, client, base+"/api/v1/auth/login",
		`{"email":"nodek@test.com","password":"secure-password-123"}`, "")
	if resp.StatusCode != 200 {
		t.Fatalf("Login: %d", resp.StatusCode)
	}
	var loginResp struct{ Token string }
	_ = json.NewDecoder(resp.Body).Decode(&loginResp)
	resp.Body.Close()

	resp = dekDoPost(t, client, base+"/api/v1/api-keys",
		`{"name":"no-dek-key","decryptAccess":false}`, loginResp.Token)
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("Create API key: %d: %s", resp.StatusCode, body)
	}
	var apiKeyResp struct{ Key string }
	_ = json.NewDecoder(resp.Body).Decode(&apiKeyResp)
	resp.Body.Close()

	resp = dekDoPost(t, client, base+"/api/v1/secrets",
		`{"name":"should-fail","type":"api-key","value":"sk-test","metadata":{"kind":"test","slug":"test"}}`, apiKeyResp.Key)
	if resp.StatusCode != 403 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("Expected 403 for non-decrypt API key, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	t.Log("E2E API Key without decryptAccess → 403 on secrets: PASSED")
}

func TestE2E_APIKey_WithDecryptAccess_SessionIDConsistency(t *testing.T) {
	router, _, _, dekCache, _, _, _, _ := setupDEKRegressionRouter(t)
	srv, base := startDEKTestServer(t, router)
	defer srv.Close()

	client := &http.Client{Timeout: 30 * time.Second}
	_, rawAPIKey := registerLoginCreateDEK(t, client, base, "sid@test.com", "pw-123456")

	expectedSessionID := "apikey:" + pkgutil.HashString(rawAPIKey)
	if _, ok := dekCache.store[expectedSessionID]; ok {
		t.Fatal("DEK should NOT be cached under apikey: prefix before first API key request (it's under JWT jti)")
	}

	resp := dekDoPost(t, client, base+"/api/v1/secrets",
		`{"name":"sid-test","type":"api-key","value":"sk-val","metadata":{"kind":"test","slug":"test"}}`, rawAPIKey)
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("Create secret with DEK API key: %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	if _, ok := dekCache.store[expectedSessionID]; !ok {
		t.Fatalf("DEK should be cached under sessionID %q after API key auth + secrets request", expectedSessionID)
	}

	t.Log("E2E API Key sessionID consistency: PASSED")
}

func TestE2E_APIKey_DEKUnwrapCorrupt_GracefulDegradation(t *testing.T) {
	router, _, db, dekCache, _, _, _, _ := setupDEKRegressionRouter(t)
	srv, base := startDEKTestServer(t, router)
	defer srv.Close()

	client := &http.Client{Timeout: 30 * time.Second}
	_, rawAPIKey := registerLoginCreateDEK(t, client, base, "corrupt@test.com", "pw-123456")

	for _, k := range db.apiKeys {
		if k.DecryptAccess {
			k.WrappedDEK = []byte("corrupted-ciphertext-not-valid-gcm")
			break
		}
	}

	for key := range dekCache.store {
		delete(dekCache.store, key)
	}

	resp := dekDoPost(t, client, base+"/api/v1/secrets",
		`{"name":"should-fail","type":"api-key","value":"sk-val","metadata":{"kind":"test","slug":"test"}}`, rawAPIKey)
	if resp.StatusCode != 403 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("Expected 403 for corrupt DEK, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	t.Log("E2E API Key corrupt DEK unwrap → graceful 403: PASSED")
}

func TestE2E_APIKey_CreateWithoutDecryptAccess_NoDEKColumns(t *testing.T) {
	_, _, db, _, _, _, _, _ := setupDEKRegressionRouter(t)

	ctx := context.Background()
	for _, u := range db.users {
		svc, err := New(testConfig(), testLogger(), db, &mockCache{})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		apiKey, err := svc.CreateAPIKey(ctx, u.ID, types.CreateAPIKeyRequest{
			Name:          "no-dek",
			DecryptAccess: false,
		}, "", nil)
		if err != nil {
			t.Fatalf("CreateAPIKey: %v", err)
		}
		if apiKey.DecryptAccess {
			t.Error("DecryptAccess should be false")
		}
		var stored *types.APIKey
		for _, k := range db.apiKeys {
			if k.UserID == u.ID && k.Name == "no-dek" {
				stored = k
				break
			}
		}
		if stored == nil {
			t.Fatal("API key not found in store")
		}
		if stored.WrappedDEK != nil {
			t.Error("WrappedDEK should be nil for non-decrypt key")
		}
		if stored.KekSalt != nil {
			t.Error("KekSalt should be nil for non-decrypt key")
		}
		if stored.KeyCiphertext != nil {
			t.Error("KeyCiphertext should be nil for non-decrypt key")
		}
		if stored.DekSynced {
			t.Error("DekSynced should be false for non-decrypt key")
		}
		break
	}

	t.Log("E2E non-decrypt API key has no DEK columns: PASSED")
}

func TestE2E_APIKey_DEKTTLMatters(t *testing.T) {
	router, _, _, dekCache, _, _, _, _ := setupDEKRegressionRouter(t)
	srv, base := startDEKTestServer(t, router)
	defer srv.Close()

	client := &http.Client{Timeout: 30 * time.Second}
	_, rawAPIKey := registerLoginCreateDEK(t, client, base, "ttl@test.com", "pw-123456")

	resp := dekDoPost(t, client, base+"/api/v1/secrets",
		`{"name":"first-req","type":"api-key","value":"sk-val","metadata":{"kind":"test","slug":"test"}}`, rawAPIKey)
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("First secret create: expected 201, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	sid := "apikey:" + pkgutil.HashString(rawAPIKey)
	dek, ok := dekCache.store[sid]
	if !ok {
		t.Fatal("DEK should be cached under apikey: sessionID after first API key request")
	}

	delete(dekCache.store, sid)

	resp = dekDoPost(t, client, base+"/api/v1/secrets",
		`{"name":"ttl-test","type":"api-key","value":"sk-val2","metadata":{"kind":"test","slug":"test"}}`, rawAPIKey)
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("DEK re-cached on re-auth: expected 201, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	recached, ok := dekCache.store[sid]
	if !ok {
		t.Fatal("DEK should be re-cached after validateAPIKey re-authentication")
	}
	if len(recached) != len(dek) {
		t.Fatalf("Re-cached DEK length mismatch: %d vs %d", len(recached), len(dek))
	}

	t.Log("E2E API Key DEK TTL / re-cache on re-auth: PASSED")
}
