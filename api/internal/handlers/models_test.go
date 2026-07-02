// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// newTestModelsHandler creates a ModelsHandler backed by a real
// WorkspaceClient that resolves to 127.0.0.1 with the given password.
// Used by tests that start a real listener on port 4096.
func newTestModelsHandler(password string) *ModelsHandler {
	ac := opencode.NewWorkspaceClient(
		mockPasswordGetter(password),
		&staticPodIPResolver{addr: "127.0.0.1"},
		zap.NewNop(),
	)
	return NewModelsHandler(ac)
}

// newTestModelsHandlerNoPod creates a ModelsHandler whose AgentClient
// will fail to resolve (empty pod IP). Tests error-handling paths.
func newTestModelsHandlerNoPod() *ModelsHandler {
	ac := opencode.NewWorkspaceClient(
		func(context.Context, string) (string, error) { return "", nil },
		&staticPodIPResolver{addr: ""},
		zap.NewNop(),
	)
	return NewModelsHandler(ac)
}

// --- Mocks ---

type mockWSUpdater struct {
	mu      sync.Mutex
	calls   []types.WorkspaceUpdates
	failErr error
	// WorkspaceOwnerChecker support
	ownerUserID string // if set, GetWorkspace returns a workspace owned by this user
}

func (m *mockWSUpdater) UpdateWorkspace(_ context.Context, _ string, updates types.WorkspaceUpdates) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, updates)
	return m.failErr
}

func (m *mockWSUpdater) GetWorkspace(_ context.Context, workspaceID string) (*types.WorkspaceMetadata, error) {
	if m.ownerUserID == "" {
		// Default: workspace owned by "user-1" (matches test auth middleware)
		return &types.WorkspaceMetadata{ID: workspaceID, UserID: "user-1"}, nil
	}
	return &types.WorkspaceMetadata{ID: workspaceID, UserID: m.ownerUserID}, nil
}

func (m *mockWSUpdater) GetDefaultModel(_ context.Context, _ string) (string, error) {
	return "", nil
}

// mockPasswordGetter returns a fixed password for any workspace.
func mockPasswordGetter(password string) func(context.Context, string) (string, error) {
	return func(_ context.Context, _ string) (string, error) {
		return password, nil
	}
}

// authEnforcingHandler returns an HTTP handler that rejects requests without valid Basic auth.
// This simulates real opencode behavior (Epic 27a A6).
func authEnforcingHandler(expectedPassword string, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "opencode" || pass != expectedPassword {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		handler(w, r)
	}
}

// --- ListModels Tests ---

func TestListModels_HappyPath(t *testing.T) {
	clearModelCache()
	gin.SetMode(gin.TestMode)

	// Mock opencode /provider endpoint on port 4096 — enforces Basic auth.
	const testPassword = "test-pw-456"
	listener, err := net.Listen("tcp", "127.0.0.1:4096")
	if err != nil {
		t.Skip("port 4096 not available")
	}
	providerResp := `{"connected":["anthropic"],"all":[{"id":"anthropic","models":{"claude-sonnet-4-5":{"id":"claude-sonnet-4-5","name":"Claude Sonnet 4.5","cost":{"input":3,"output":15}}}}]}`
	srv := httptest.NewUnstartedServer(authEnforcingHandler(testPassword, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/provider", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(providerResp))
	}))
	srv.Listener = listener
	srv.Start()
	defer srv.Close()

	handler := newTestModelsHandler(testPassword)

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Next()
	})
	router.GET("/api/v1/workspaces/:id/models", handler.ListModels)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/models", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), "claude-sonnet-4-5")
	require.Contains(t, w.Body.String(), `"tier"`)
	require.Contains(t, w.Body.String(), `"models"`)
}

func TestListModels_NoPodRunning(t *testing.T) {
	clearModelCache()
	gin.SetMode(gin.TestMode)

	handler := newTestModelsHandlerNoPod()

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Next()
	})
	router.GET("/api/v1/workspaces/:id/models", handler.ListModels)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/models", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)
	require.Contains(t, w.Body.String(), "pod not running")
}

func TestListModels_NoPodIPResolver(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := NewModelsHandler(nil)
	// No resolver set

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Next()
	})
	router.GET("/api/v1/workspaces/:id/models", handler.ListModels)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/models", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestListModels_AgentUnreachable(t *testing.T) {
	clearModelCache()
	gin.SetMode(gin.TestMode)

	// With nil AgentClient, ListModels returns 503 "model discovery unavailable".
	// (The old test tested the no-resolver path; with AgentClient, nil =
	// discovery unavailable.)
	handler := NewModelsHandler(nil)

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Next()
	})
	router.GET("/api/v1/workspaces/:id/models", handler.ListModels)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/models", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestListModels_Unauthenticated(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := NewModelsHandler(nil)
	router := gin.New()
	// No userID in context
	router.GET("/api/v1/workspaces/:id/models", handler.ListModels)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/models", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code)
}

// Ownership-denied coverage for ListModels / SetModel now lives at the
// router level (api/internal/server/router_workspace_access_test.go) — the
// WorkspaceAccessMiddleware gates both GET /:id/models and PUT /:id/model
// before the handler is reached, so a handler-level ownership test would
// either duplicate that coverage or assert behavior the handler no longer
// has. The handler trusts the middleware's decision per design 0041 D5.

// --- SetModel Tests ---

func TestSetModel_HappyPath(t *testing.T) {
	clearModelCache()
	gin.SetMode(gin.TestMode)

	updater := &mockWSUpdater{}
	handler := newTestModelsHandlerNoPod() // no pod needed — SetModel persists only
	handler.SetModelStore(updater)

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Next()
	})
	router.PUT("/api/v1/workspaces/:id/model", handler.SetModel)

	body, _ := json.Marshal(map[string]string{"model": "anthropic/claude-sonnet-4-5"})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-1/model", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	require.Equal(t, "anthropic/claude-sonnet-4-5", resp["model"])
	// applied is false because the AgentClient resolves to an empty pod IP
	// (no pod running), so the live PATCH to opencode fails.
	// The model IS persisted to workspace metadata (verified below).
	require.Equal(t, false, resp["applied"])

	// Verify workspace metadata was updated.
	updater.mu.Lock()
	require.Len(t, updater.calls, 1)
	require.Equal(t, "anthropic/claude-sonnet-4-5", *updater.calls[0].DefaultModel)
	updater.mu.Unlock()
}

func TestSetModel_MissingModelField(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := NewModelsHandler(nil)
	handler.SetModelStore(&mockWSUpdater{})

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Next()
	})
	router.PUT("/api/v1/workspaces/:id/model", handler.SetModel)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-1/model", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSetModel_NoUpdater(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := NewModelsHandler(nil)
	// No updater set

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Next()
	})
	router.PUT("/api/v1/workspaces/:id/model", handler.SetModel)

	body, _ := json.Marshal(map[string]string{"model": "test/model"})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-1/model", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
}

// TestSetModel_NilAgentClient_Returns503 is a regression test for the
// nil-agentClient panic found in review. When agentClient is nil (e.g.
// agentReloadHandler was nil so AgentClient was never wired), SetModel
// must return 503, not panic.
func TestSetModel_NilAgentClient_Returns503(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := NewModelsHandler(nil)
	handler.SetModelStore(&mockWSUpdater{}) // updater is set, but agentClient is nil

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Next()
	})
	router.PUT("/api/v1/workspaces/:id/model", handler.SetModel)

	body, _ := json.Marshal(map[string]string{"model": "test/model"})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-1/model", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.Contains(t, w.Body.String(), "agent client")
}

func TestSetModel_NoPod_AppliedFalse(t *testing.T) {
	gin.SetMode(gin.TestMode)

	updater := &mockWSUpdater{}
	handler := newTestModelsHandlerNoPod() // no pod
	handler.SetModelStore(updater)

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Next()
	})
	router.PUT("/api/v1/workspaces/:id/model", handler.SetModel)

	body, _ := json.Marshal(map[string]string{"model": "openai/gpt-4o"})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-1/model", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	require.Equal(t, "openai/gpt-4o", resp["model"])
	require.Equal(t, false, resp["applied"]) // saved but not applied to live pod

	// Workspace metadata still updated.
	updater.mu.Lock()
	require.Len(t, updater.calls, 1)
	updater.mu.Unlock()
}

// TestSetModel_LivePush_SendsBasicAuth verifies that when a pod is running,
// SetModel sends the workspace password as Basic auth to PATCH /global/config.
// Without Basic auth opencode returns 401 and applied stays false.
func TestSetModel_LivePush_SendsBasicAuth(t *testing.T) {
	clearModelCache()
	gin.SetMode(gin.TestMode)
	const testPassword = "ws-live-push-pw"

	// Mock opencode: only accepts authenticated PATCH /global/config (paid model).
	// For the model list (GET /api/model) return a paid model so relay baseURL is
	// not pushed (keeps the test focused on patchAgentModel auth).
	listener, err := net.Listen("tcp", "127.0.0.1:4096")
	if err != nil {
		t.Skip("port 4096 not available")
	}
	srv := httptest.NewUnstartedServer(authEnforcingHandler(testPassword, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPatch && r.URL.Path == "/global/config":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/provider":
			// Return a paid anthropic model so relay remap is not triggered
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"connected":["anthropic"],"all":[{"id":"anthropic","models":{"claude-paid":{"id":"claude-paid","name":"Claude Paid","cost":{"input":3,"output":15}}}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	srv.Listener = listener
	srv.Start()
	defer srv.Close()

	updater := &mockWSUpdater{ownerUserID: "user-1"}
	handler := newTestModelsHandler(testPassword)
	handler.SetModelStore(updater)

	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userID", "user-1"); c.Next() })
	router.PUT("/api/v1/workspaces/:id/model", handler.SetModel)

	body, _ := json.Marshal(map[string]string{"model": "claude-paid"})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-1/model", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, "claude-paid", resp["model"])
	require.Equal(t, true, resp["applied"], "applied must be true when PATCH /global/config succeeds with Basic auth")
}

func TestSetModel_Unauthenticated(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := NewModelsHandler(nil)
	handler.SetModelStore(&mockWSUpdater{})

	router := gin.New()
	// No userID
	router.PUT("/api/v1/workspaces/:id/model", handler.SetModel)

	body, _ := json.Marshal(map[string]string{"model": "test/m"})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-1/model", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestListModels_NoPasswordGetter_Returns503(t *testing.T) {
	clearModelCache()
	gin.SetMode(gin.TestMode)

	// With nil AgentClient (no password resolution path configured),
	// ListModels returns 503 "model discovery unavailable".
	handler := NewModelsHandler(nil)

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Next()
	})
	router.GET("/api/v1/workspaces/:id/models", handler.ListModels)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/models", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.Contains(t, w.Body.String(), "model discovery unavailable")
}

func TestListModels_WrongPassword_Returns502(t *testing.T) {
	clearModelCache()
	gin.SetMode(gin.TestMode)

	const correctPassword = "real-pw"
	listener, err := net.Listen("tcp", "127.0.0.1:4096")
	if err != nil {
		t.Skip("port 4096 not available")
	}
	// Mock opencode that enforces auth
	srv := httptest.NewUnstartedServer(authEnforcingHandler(correctPassword, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`))
	}))
	srv.Listener = listener
	srv.Start()
	defer srv.Close()

	handler := newTestModelsHandler("wrong-pw") // wrong password

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Next()
	})
	router.GET("/api/v1/workspaces/:id/models", handler.ListModels)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/models", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// opencode returns 401 (wrong password) → handler returns 502 Bad Gateway
	require.Equal(t, http.StatusBadGateway, w.Code)
}

// Ensure unused import doesn't break compilation.

// --- Availability Classification Tests ---

func TestClassifyAvailability_OpencodeZeroCost(t *testing.T) {
	avail := classifyAvailability("opencode", ProviderModelCost{Input: 0, Output: 0})
	require.Equal(t, ModelFreeTier, avail)
}

func TestClassifyAvailability_OpencodeNoCostEntries(t *testing.T) {
	// zero-value providerCost = input:0, output:0 → free tier
	avail := classifyAvailability("opencode", ProviderModelCost{})
	require.Equal(t, ModelFreeTier, avail)
}

func TestClassifyAvailability_OpencodeNilCost(t *testing.T) {
	// zero-value is indistinguishable from "no cost data" → still free tier
	avail := classifyAvailability("opencode", ProviderModelCost{})
	require.Equal(t, ModelFreeTier, avail)
}

func TestClassifyAvailability_OpencodePaidCost(t *testing.T) {
	avail := classifyAvailability("opencode", ProviderModelCost{Input: 3.0, Output: 15.0})
	require.Equal(t, ModelAvailable, avail)
}

// TestClassifyAvailability_OpencodeRelayZeroCost verifies that models with
// providerID="opencode-relay" and zero cost are classified as ModelFreeTier.
func TestClassifyAvailability_OpencodeRelayZeroCost(t *testing.T) {
	avail := classifyAvailability("opencode-relay", ProviderModelCost{Input: 0, Output: 0})
	require.Equal(t, ModelFreeTier, avail)
}

func TestClassifyAvailability_OpencodeRelayNoCostEntries(t *testing.T) {
	avail := classifyAvailability("opencode-relay", ProviderModelCost{})
	require.Equal(t, ModelFreeTier, avail)
}

func TestClassifyAvailability_OpencodeRelayPaidCost(t *testing.T) {
	// opencode-relay models with non-zero cost should be Available, not Free.
	avail := classifyAvailability("opencode-relay", ProviderModelCost{Input: 1.0, Output: 2.0})
	require.Equal(t, ModelAvailable, avail)
}

func TestClassifyAvailability_NonOpencodeLoaded(t *testing.T) {
	avail := classifyAvailability("anthropic", ProviderModelCost{Input: 0, Output: 0})
	require.Equal(t, ModelAvailable, avail)
}

func TestAnnotateModels_RelayActive_OnlyRemapsOpencode(t *testing.T) {
	// When relayActive=true and the catalog already has providerID="opencode-relay"
	// (Phase 2: relay injected), annotateModels must NOT double-remap to "opencode-relay".
	raw := `{
		"connected": ["opencode-relay","opencode"],
		"all": [
			{"id":"opencode-relay","models":{"nemotron-free":{"id":"nemotron-free","name":"Nemotron Free","cost":{"input":0,"output":0}}}},
			{"id":"opencode","models":{"glm-5.1-free":{"id":"glm-5.1-free","name":"GLM 5.1 Free","cost":{"input":0,"output":0}}}}
		]
	}`

	cat, err := NewOpencodeProviderParser().Parse([]byte(raw))
	require.NoError(t, err)
	result := annotateModels(cat, true, true)
	require.Len(t, result, 2)

	byID := make(map[string]annotatedModel)
	for _, m := range result {
		byID[m.ID] = m
	}
	// Phase 2 model: already opencode-relay, stays opencode-relay
	require.Equal(t, "opencode-relay", byID["nemotron-free"].ProviderID)
	require.Equal(t, ModelFreeTier, byID["nemotron-free"].Availability)
	// Phase 1 model: opencode remapped to opencode-relay when relayActive
	require.Equal(t, "opencode-relay", byID["glm-5.1-free"].ProviderID)
	require.Equal(t, ModelFreeTier, byID["glm-5.1-free"].Availability)
}

func TestAnnotateModels_FullResponse(t *testing.T) {
	raw := `{
		"connected": ["opencode","anthropic"],
		"all": [
			{"id":"opencode","models":{
				"free-model": {"id":"free-model","name":"Free Model","cost":{"input":0,"output":0}},
				"paid-model": {"id":"paid-model","name":"Paid OpenCode","cost":{"input":1,"output":2}}
			}},
			{"id":"anthropic","models":{
				"claude-sonnet-4-5": {"id":"claude-sonnet-4-5","name":"Claude Sonnet 4.5","cost":{"input":3,"output":15}}
			}}
		]
	}`

	cat, err := NewOpencodeProviderParser().Parse([]byte(raw))
	require.NoError(t, err)
	result := annotateModels(cat, false, false)
	require.Len(t, result, 3)

	byID := make(map[string]annotatedModel)
	for _, m := range result {
		byID[m.ID] = m
	}

	require.Equal(t, "free", byID["free-model"].Tier)
	require.True(t, byID["free-model"].FreeTier)
	require.True(t, byID["free-model"].ProxyRequired, "free-tier model must have proxyRequired=true")

	require.Equal(t, "paid", byID["claude-sonnet-4-5"].Tier)
	require.False(t, byID["claude-sonnet-4-5"].FreeTier)
	require.False(t, byID["claude-sonnet-4-5"].ProxyRequired, "paid model must have proxyRequired=false")

	require.Equal(t, "paid", byID["paid-model"].Tier) // opencode provider but has cost > 0
	require.False(t, byID["paid-model"].ProxyRequired, "paid model must have proxyRequired=false")
}

func TestAnnotateModels_NilCatalog(t *testing.T) {
	// annotateModels on a nil catalog returns nil (defensive — the parser
	// returns &Catalog{} for empty input, but nil guards the path).
	result := annotateModels(nil, false, false)
	require.Nil(t, result)
}

// parseAndAnnotate is a test helper that parses raw JSON via the opencode
// parser then annotates. Replaces the old annotateModels([]byte, …) signature.
func parseAndAnnotate(t *testing.T, raw string, relayActive, relayInjected bool) []annotatedModel {
	t.Helper()
	cat, err := NewOpencodeProviderParser().Parse([]byte(raw))
	require.NoError(t, err)
	return annotateModels(cat, relayActive, relayInjected)
}

func TestAnnotateModels_EmptyConnected(t *testing.T) {
	// connected=[] → no models accessible → empty result
	raw := `{"connected":[],"all":[{"id":"opencode","models":{"free":{"id":"free","cost":{"input":0,"output":0}}}}]}`
	result := parseAndAnnotate(t, raw, false, false)
	require.Len(t, result, 0)
}

func TestAnnotateModels_PreservesProviderID(t *testing.T) {
	raw := `{"connected":["anthropic"],"all":[{"id":"anthropic","models":{"claude":{"id":"claude","name":"Claude","cost":{"input":3,"output":15}}}}]}`
	result := parseAndAnnotate(t, raw, false, false)
	require.Len(t, result, 1)
	require.Equal(t, "anthropic", result[0].ProviderID)
	require.Equal(t, "claude", result[0].ID)
}

func TestListModels_ResponseAnnotated(t *testing.T) {
	clearModelCache()
	gin.SetMode(gin.TestMode)

	const testPassword = "annotated-pw"
	listener, err := net.Listen("tcp", "127.0.0.1:4096")
	if err != nil {
		t.Skip("port 4096 not available")
	}
	models := `{"connected":["opencode"],"all":[{"id":"opencode","models":{"test":{"id":"test","name":"Test","cost":{"input":0,"output":0}}}}]}`
	srv := httptest.NewUnstartedServer(authEnforcingHandler(testPassword, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(models))
	}))
	srv.Listener = listener
	srv.Start()
	defer srv.Close()

	handler := newTestModelsHandler(testPassword)

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Next()
	})
	router.GET("/api/v1/workspaces/:id/models", handler.ListModels)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/models", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Models       []annotatedModel `json:"models"`
		CurrentModel string           `json:"currentModel"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Models, 1)
	require.Equal(t, "free", resp.Models[0].Tier)
	require.True(t, resp.Models[0].FreeTier)
	require.True(t, resp.Models[0].ProxyRequired, "free-tier models must have proxyRequired=true (Epic 26)")
	require.Equal(t, "test", resp.Models[0].ID)
	require.Equal(t, "", resp.CurrentModel) // no updater set = empty
}

// TestAnnotateModels_RelayActive_RemapsProviderID verifies that when the relay
// is active, free-tier opencode models have ProviderID remapped to
// "opencode-relay" so clients use the relay provider for inference.
func TestAnnotateModels_RelayActive_RemapsProviderID(t *testing.T) {
	raw := `{
		"connected": ["opencode","anthropic"],
		"all": [
			{"id":"opencode","models":{
				"free-model": {"id":"free-model","name":"Free","cost":{"input":0,"output":0}},
				"paid-model": {"id":"paid-model","name":"Paid","cost":{"input":1,"output":2}}
			}},
			{"id":"anthropic","models":{
				"claude": {"id":"claude","name":"Claude","cost":{"input":3,"output":15}}
			}}
		]
	}`

	result := parseAndAnnotate(t, raw, true, true)
	require.Len(t, result, 3)

	byID := make(map[string]annotatedModel)
	for _, m := range result {
		byID[m.ID] = m
	}

	// Free opencode model: ProviderID must be remapped to opencode-relay
	assert.Equal(t, "opencode-relay", byID["free-model"].ProviderID,
		"free-tier opencode model must use opencode-relay providerID when relay is active")
	assert.True(t, byID["free-model"].ProxyRequired)
	assert.True(t, byID["free-model"].FreeTier)

	// Paid opencode model: ProviderID stays "opencode"
	assert.Equal(t, "opencode", byID["paid-model"].ProviderID,
		"paid opencode model must keep opencode providerID")
	assert.False(t, byID["paid-model"].ProxyRequired)

	// Non-opencode model: unaffected
	assert.Equal(t, "anthropic", byID["claude"].ProviderID)
}

// TestAnnotateModels_RelayInactive_DoesNotRemap verifies that when the relay
// is not active, providerIDs are not remapped.
func TestAnnotateModels_RelayInactive_DoesNotRemap(t *testing.T) {
	raw := `{
		"connected": ["opencode"],
		"all": [{"id":"opencode","models":{"free-model":{"id":"free-model","name":"Free","cost":{"input":0,"output":0}}}}]
	}`

	result := parseAndAnnotate(t, raw, false, false)
	require.Len(t, result, 1)

	assert.Equal(t, "opencode", result[0].ProviderID,
		"providerID must not be remapped when relay is inactive")
}

// TestAnnotateModels_PersonalKey_NoRemap verifies that when a user has a
// personal opencode key (relay was skipped, relayInjected=false), free-tier
// opencode models keep their original providerID="opencode" rather than being
// remapped to "opencode-relay". This is Bug 3 — before the fix, relayActive
// was a static global that caused remapping regardless of whether the relay
// injector ran.
func TestAnnotateModels_PersonalKey_NoRemap(t *testing.T) {
	// Personal-key scenario: opencode is connected, opencode-relay is NOT.
	// relayGloballyEnabled=true (env var is set), but relayInjected=false
	// (the injector was skipped because shouldSkipRelay returned true).
	raw := `{
		"connected": ["opencode"],
		"all": [{"id":"opencode","models":{
			"free-model":{"id":"free-model","name":"Free","cost":{"input":0,"output":0}}
		}}]
	}`

	result := parseAndAnnotate(t, raw, true, false)
	require.Len(t, result, 1)

	assert.Equal(t, "opencode", result[0].ProviderID,
		"free-tier model must NOT be remapped to opencode-relay when relay injector was skipped (personal key)")
	assert.True(t, result[0].FreeTier)
}

// TestAnnotateModels_Phase1_Remap verifies that in Phase 1 (relay globally
// configured but injector not yet complete), free models are NOT remapped
// (relayInjected=false). This means during the ~7s window before injection,
// free models show providerID="opencode" and will fail at inference — acceptable
// trade-off vs permanently breaking personal-key users.
func TestAnnotateModels_Phase1_NotRemapped(t *testing.T) {
	raw := `{
		"connected": ["opencode"],
		"all": [{"id":"opencode","models":{
			"free-model":{"id":"free-model","name":"Free","cost":{"input":0,"output":0}}
		}}]
	}`

	result := parseAndAnnotate(t, raw, true, false)
	require.Len(t, result, 1)

	// Phase 1: relayInjected=false → no remap. providerID stays "opencode".
	// After T+7s the injector completes and relayInjected becomes true.
	assert.Equal(t, "opencode", result[0].ProviderID,
		"Phase 1: free model must keep providerID='opencode' until relay injection completes")
}

// mockModelReader implements WorkspaceDefaultModelReader for testing.
type mockModelReader struct {
	model string
}

func (m *mockModelReader) UpdateWorkspace(_ context.Context, _ string, _ types.WorkspaceUpdates) error {
	return nil
}

func (m *mockModelReader) GetDefaultModel(_ context.Context, _ string) (string, error) {
	return m.model, nil
}

func (m *mockModelReader) GetWorkspace(_ context.Context, workspaceID string) (*types.WorkspaceMetadata, error) {
	return &types.WorkspaceMetadata{ID: workspaceID, UserID: "user-1"}, nil
}

func TestListModels_IncludesCurrentModel(t *testing.T) {
	clearModelCache()
	gin.SetMode(gin.TestMode)

	const testPassword = "currentmodel-pw"
	listener, err := net.Listen("tcp", "127.0.0.1:4096")
	if err != nil {
		t.Skip("port 4096 not available")
	}
	models := `{"connected":["anthropic"],"all":[{"id":"anthropic","models":{"claude-sonnet-4-5":{"id":"claude-sonnet-4-5","name":"Claude","cost":{"input":3,"output":15}}}}]}`
	srv := httptest.NewUnstartedServer(authEnforcingHandler(testPassword, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(models))
	}))
	srv.Listener = listener
	srv.Start()
	defer srv.Close()

	handler := newTestModelsHandler(testPassword)
	handler.SetModelStore(&mockModelReader{model: "claude-sonnet-4-5"})

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Next()
	})
	router.GET("/api/v1/workspaces/:id/models", handler.ListModels)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/models", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Models       []annotatedModel `json:"models"`
		CurrentModel string           `json:"currentModel"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, "claude-sonnet-4-5", resp.CurrentModel)
	require.Len(t, resp.Models, 1)
	require.Equal(t, "paid", resp.Models[0].Tier)
}

func TestListModels_FiltersPaidOpencodeModels(t *testing.T) {
	clearModelCache()
	gin.SetMode(gin.TestMode)

	const testPassword = "filter-pw"
	listener, err := net.Listen("tcp", "127.0.0.1:4096")
	if err != nil {
		t.Skip("port 4096 not available")
	}
	// Mix of connected providers: opencode (free+paid) and anthropic
	// openai is NOT in connected[] — should be excluded entirely
	models := `{
		"connected": ["opencode","anthropic"],
		"all": [
			{"id":"opencode","models":{
				"free-model": {"id":"free-model","name":"Free","cost":{"input":0,"output":0}},
				"paid-model": {"id":"paid-model","name":"Paid","cost":{"input":3,"output":15}}
			}},
			{"id":"anthropic","models":{
				"claude": {"id":"claude","name":"Claude","cost":{"input":3,"output":15}}
			}},
			{"id":"openai","models":{
				"gpt-5": {"id":"gpt-5","name":"GPT-5","cost":{"input":5,"output":15}}
			}}
		]
	}`
	srv := httptest.NewUnstartedServer(authEnforcingHandler(testPassword, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(models))
	}))
	srv.Listener = listener
	srv.Start()
	defer srv.Close()

	handler := newTestModelsHandler(testPassword)
	handler.SetModelStore(&mockWSUpdater{})

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Next()
	})
	router.GET("/api/v1/workspaces/:id/models", handler.ListModels)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/models", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Models []struct {
			ID       string `json:"id"`
			FreeTier bool   `json:"freeTier"`
		} `json:"models"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	// Should contain: free opencode + paid opencode + anthropic/claude
	// Should NOT contain: openai/gpt-5 (not in connected[])
	ids := make([]string, len(resp.Models))
	for i, m := range resp.Models {
		ids[i] = m.ID
	}
	require.Contains(t, ids, "free-model")
	require.Contains(t, ids, "paid-model")
	require.Contains(t, ids, "claude")
	require.NotContains(t, ids, "gpt-5")
	require.Len(t, resp.Models, 3)
}

// TestCatalog_ResolveModel_PrefixesProvider verifies that the model
// resolution method returns providerID/modelID format.
// Regression test for the opencode 1.15.x ProviderModelNotFoundError bug.
func TestCatalog_ResolveModel_PrefixesProvider(t *testing.T) {
	cat, err := NewOpencodeProviderParser().Parse([]byte(`{
		"connected": ["openai","anthropic"],
		"all": [
			{"id":"openai","models":{"gpt-5.5":{"id":"gpt-5.5","name":"GPT-5.5","cost":{"input":5,"output":15}}}},
			{"id":"anthropic","models":{"claude-3":{"id":"claude-3","name":"Claude 3","cost":{"input":3,"output":15}}}}
		]
	}`))
	require.NoError(t, err)

	tests := []struct {
		catalogID string
		want      string
	}{
		{"gpt-5.5", "openai/gpt-5.5"},
		{"claude-3", "anthropic/claude-3"},
		{"unknown-model", "unknown-model"},
	}
	for _, tt := range tests {
		t.Run(tt.catalogID, func(t *testing.T) {
			got := cat.resolveModel(tt.catalogID, false, false)
			require.Equal(t, tt.want, got)
		})
	}
}

// TestSetModel_LivePush_ResolvesProviderPrefix verifies that SetModel sends
// "providerID/modelID" to opencode, not just the flat catalog ID.
func TestSetModel_LivePush_ResolvesProviderPrefix(t *testing.T) {
	clearModelCache()
	gin.SetMode(gin.TestMode)
	const testPassword = "ws-resolve-pw"

	var receivedModel string
	listener, err := net.Listen("tcp", "127.0.0.1:4096")
	if err != nil {
		t.Skip("port 4096 not available")
	}
	srv := httptest.NewUnstartedServer(authEnforcingHandler(testPassword, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/provider":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"connected":["openai"],"all":[{"id":"openai","models":{"gpt-5.5":{"id":"gpt-5.5","name":"GPT-5.5","cost":{"input":5,"output":15}}}}]}`))
		case r.Method == http.MethodPatch && r.URL.Path == "/global/config":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			receivedModel = body["model"]
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	srv.Listener = listener
	srv.Start()
	defer srv.Close()

	updater := &mockWSUpdater{ownerUserID: "user-1"}
	handler := newTestModelsHandler(testPassword)
	handler.SetModelStore(updater)

	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userID", "user-1"); c.Next() })
	router.PUT("/api/v1/workspaces/:id/model", handler.SetModel)

	body, _ := json.Marshal(map[string]string{"model": "gpt-5.5"})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-1/model", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, true, resp["applied"], "applied must be true")
	require.Equal(t, "openai/gpt-5.5", receivedModel,
		"patchAgentModel must send providerID/modelID, not flat catalog ID")
}

// TestListModels_CurrentModelProviderID verifies that the currentModelProviderID
// field is populated with the resolved providerID for the selected model.
func TestListModels_CurrentModelProviderID(t *testing.T) {
	clearModelCache()
	gin.SetMode(gin.TestMode)

	const testPassword = "providerid-pw"
	listener, err := net.Listen("tcp", "127.0.0.1:4096")
	if err != nil {
		t.Skip("port 4096 not available")
	}
	models := `{"connected":["thekao"],"all":[{"id":"thekao","models":{
		"glm-5.1":{"id":"glm-5.1","name":"GLM 5.1","cost":{"input":1,"output":2}},
		"gpt-5.4":{"id":"gpt-5.4","name":"GPT 5.4","cost":{"input":1,"output":2}}
	}}]}`
	srv := httptest.NewUnstartedServer(authEnforcingHandler(testPassword, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(models))
	}))
	srv.Listener = listener
	srv.Start()
	defer srv.Close()

	handler := newTestModelsHandler(testPassword)
	handler.SetModelStore(&mockModelReader{model: "glm-5.1"})

	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userID", "user-1"); c.Next() })
	router.GET("/api/v1/workspaces/:id/models", handler.ListModels)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/models", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Models                 []annotatedModel `json:"models"`
		CurrentModel           string           `json:"currentModel"`
		CurrentModelProviderID string           `json:"currentModelProviderID"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, "glm-5.1", resp.CurrentModel)
	require.Equal(t, "thekao", resp.CurrentModelProviderID,
		"currentModelProviderID must be the provider that owns the selected model")
}

// TestListModels_CurrentModelProviderID_Collision verifies that when two
// connected providers expose the same model ID, currentModelProviderID is ""
// (signals ambiguity; client falls back to find()).
func TestListModels_CurrentModelProviderID_Collision(t *testing.T) {
	clearModelCache()
	gin.SetMode(gin.TestMode)

	const testPassword = "collision-pw"
	listener, err := net.Listen("tcp", "127.0.0.1:4096")
	if err != nil {
		t.Skip("port 4096 not available")
	}
	// Two connected providers both expose model ID "shared-model".
	models := `{"connected":["provider-a","provider-b"],"all":[
		{"id":"provider-a","models":{"shared-model":{"id":"shared-model","name":"Shared A","cost":{"input":1,"output":2}}}},
		{"id":"provider-b","models":{"shared-model":{"id":"shared-model","name":"Shared B","cost":{"input":1,"output":2}}}}
	]}`
	srv := httptest.NewUnstartedServer(authEnforcingHandler(testPassword, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(models))
	}))
	srv.Listener = listener
	srv.Start()
	defer srv.Close()

	handler := newTestModelsHandler(testPassword)
	handler.SetModelStore(&mockModelReader{model: "shared-model"})

	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userID", "user-1"); c.Next() })
	router.GET("/api/v1/workspaces/:id/models", handler.ListModels)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/models", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Models                 []annotatedModel `json:"models"`
		CurrentModel           string           `json:"currentModel"`
		CurrentModelProviderID string           `json:"currentModelProviderID"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, "shared-model", resp.CurrentModel)
	require.Equal(t, "", resp.CurrentModelProviderID,
		"collision must produce empty currentModelProviderID")
}

// TestListModels_CurrentModelProviderID_RelayActive verifies that when
// relayActive=true AND the relay injector has run (relayInjected=true from
// statusz), a free-tier opencode model is correctly remapped to "opencode-relay"
// in currentModelProviderID. This catches regressions if the remap logic or
// the annotated-model iteration order changes.
func TestListModels_CurrentModelProviderID_RelayActive(t *testing.T) {
	clearModelCache()
	gin.SetMode(gin.TestMode)

	const testPassword = "relay-providerid-pw"
	listener, err := net.Listen("tcp", "127.0.0.1:4096")
	if err != nil {
		t.Skip("port 4096 not available")
	}
	// Also mock agentd admin port (4098) to serve readyz with RelayInjected=true.
	adminListener, err := net.Listen("tcp", "127.0.0.1:4098")
	if err != nil {
		t.Skip("port 4098 not available")
	}

	// Free-tier opencode model: cost.input==0, providerID=="opencode" →
	// annotateModels remaps providerID to "opencode-relay" when relayActive=true
	// AND relayInjected=true (from readyz).
	models := `{"connected":["opencode"],"all":[{"id":"opencode","models":{
		"glm-5.1-free":{"id":"glm-5.1-free","name":"GLM 5.1 Free","cost":{"input":0,"output":0}}
	}}]}`

	// opencode server (port 4096) — serves /provider
	srv := httptest.NewUnstartedServer(authEnforcingHandler(testPassword, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(models))
	}))
	srv.Listener = listener
	srv.Start()
	defer srv.Close()

	// agentd admin server (port 4098) — serves /v1/readyz with RelayInjected=true
	statuszBody, _ := json.Marshal(agentd.ReadyzResponse{RelayInjected: true})
	adminSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(statuszBody)
	}))
	adminSrv.Listener = adminListener
	adminSrv.Start()
	defer adminSrv.Close()

	handler := newTestModelsHandler(testPassword)
	handler.SetModelStore(&mockModelReader{model: "glm-5.1-free"})
	handler.SetRelayActive(true)
	// Wire a relayChecker that hits the agentd admin server (port 4098)
	// started above. Without this, h.relayChecker is nil and the handler
	// skips the readyz check, leaving relayInjected=false (Bug: assertion
	// would see "opencode" instead of "opencode-relay").
	handler.SetRelayChecker(func(_ context.Context, _, _ string) bool {
		req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:4098/v1/readyz", nil)
		if err != nil {
			return false
		}
		req.Header.Set("Authorization", "Bearer "+testPassword)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return false
		}
		var rz struct {
			RelayInjected bool `json:"relay_injected"`
		}
		if json.NewDecoder(resp.Body).Decode(&rz) != nil {
			return false
		}
		return rz.RelayInjected
	})

	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userID", "user-1"); c.Next() })
	router.GET("/api/v1/workspaces/:id/models", handler.ListModels)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/models", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Models                 []annotatedModel `json:"models"`
		CurrentModel           string           `json:"currentModel"`
		CurrentModelProviderID string           `json:"currentModelProviderID"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, "glm-5.1-free", resp.CurrentModel)
	require.Equal(t, "opencode-relay", resp.CurrentModelProviderID,
		"relay remap must be reflected in currentModelProviderID when relayInjected=true")
}

// TestListModels_RelayActive_PersonalKey_NoRemap verifies that when
// relayActive=true but the relay injector was skipped (RelayInjected=false in
// readyz — personal opencode key), free models are NOT remapped to
// opencode-relay. This is Bug 3.
func TestListModels_RelayActive_PersonalKey_NoRemap(t *testing.T) {
	clearModelCache()
	gin.SetMode(gin.TestMode)

	const testPassword = "personal-key-pw"
	listener, err := net.Listen("tcp", "127.0.0.1:4096")
	if err != nil {
		t.Skip("port 4096 not available")
	}
	adminListener, err := net.Listen("tcp", "127.0.0.1:4098")
	if err != nil {
		t.Skip("port 4098 not available")
	}

	models := `{"connected":["opencode"],"all":[{"id":"opencode","models":{
		"free-model":{"id":"free-model","name":"Free","cost":{"input":0,"output":0}}
	}}]}`

	srv := httptest.NewUnstartedServer(authEnforcingHandler(testPassword, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(models))
	}))
	srv.Listener = listener
	srv.Start()
	defer srv.Close()

	// readyz returns RelayInjected=false — relay was skipped (personal key)
	statuszBody, _ := json.Marshal(agentd.ReadyzResponse{RelayInjected: false})
	adminSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(statuszBody)
	}))
	adminSrv.Listener = adminListener
	adminSrv.Start()
	defer adminSrv.Close()

	handler := newTestModelsHandler(testPassword)
	handler.SetRelayActive(true)
	// Wire a relayChecker that hits the agentd admin server (port 4098)
	// started above. Without this, h.relayChecker is nil and the handler
	// skips the readyz check — the test would pass for the wrong reason
	// (nil checker → no remap, instead of readyz=false → no remap).
	handler.SetRelayChecker(func(_ context.Context, _, _ string) bool {
		req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:4098/v1/readyz", nil)
		if err != nil {
			return false
		}
		req.Header.Set("Authorization", "Bearer "+testPassword)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return false
		}
		var rz struct {
			RelayInjected bool `json:"relay_injected"`
		}
		if json.NewDecoder(resp.Body).Decode(&rz) != nil {
			return false
		}
		return rz.RelayInjected
	})

	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userID", "user-1"); c.Next() })
	router.GET("/api/v1/workspaces/:id/models", handler.ListModels)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/models", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Models []annotatedModel `json:"models"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Models, 1)
	require.Equal(t, "opencode", resp.Models[0].ProviderID,
		"free model must NOT be remapped to opencode-relay when relay injector was skipped (personal key)")
}

// TestListModels_CacheInvalidatedOnRelayInjectedTransition is a regression
// test for LLMSafeSpaces#467. The model cache stores a snapshot of annotated
// models keyed only by workspaceID. If the cache was populated while
// relayInjected=false (pre-injection state — free models providerID="opencode")
// and a subsequent request lands during the cache TTL (5s) after the relay
// injector has completed (relayInjected=true now), the cached pre-injection
// snapshot is served even though the workspace's free models are now resolved
// through the relay (providerID="opencode-relay") at every other layer
// (agent-config.json, opencode auth, etc.).
//
// Observed in production: a fresh workspace's first /models call landed
// during the ~20s post-injection cache window (5s API cache + 15s agentd
// providerCache), returned currentModelProviderID="opencode" alongside a
// fully pre-injection models[] array. The frontend faithfully forwarded
// providerID="opencode" in the next /prompt POST. opencode rejected the
// request because "opencode" is in disabled_providers post-injection and
// the requested free model lives under "opencode-relay". User sees silent
// send failure on every first-message-after-workspace-creation attempt.
//
// Fix: when serving from cache, re-check the current relayInjected state
// via relayChecker. If it differs from the cached payload's RelayInjected,
// evict the cache and re-fetch. The relayChecker call is cheap (hits
// agentd's own 15s cache in the steady state) compared to letting the
// stale window serve broken responses for ~20s.
//
// This test: pre-populates the cache with relayInjected=false payload,
// then issues a request with the live relayChecker returning true. The
// served response MUST reflect relayInjected=true (free model remapped
// to "opencode-relay"), proving the cache was invalidated and refreshed.
func TestListModels_CacheInvalidatedOnRelayInjectedTransition(t *testing.T) {
	clearModelCache()
	gin.SetMode(gin.TestMode)

	const testPassword = "cache-invalidate-pw"
	listener, err := net.Listen("tcp", "127.0.0.1:4096")
	if err != nil {
		t.Skip("port 4096 not available")
	}
	adminListener, err := net.Listen("tcp", "127.0.0.1:4098")
	if err != nil {
		t.Skip("port 4098 not available")
	}

	// Free-tier opencode model — eligible for the relay remap when
	// relayInjected=true.
	models := `{"connected":["opencode"],"all":[{"id":"opencode","models":{
		"big-pickle":{"id":"big-pickle","name":"Big Pickle","cost":{"input":0,"output":0}}
	}}]}`

	srv := httptest.NewUnstartedServer(authEnforcingHandler(testPassword, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(models))
	}))
	srv.Listener = listener
	srv.Start()
	defer srv.Close()

	// Live agentd readyz returns relayInjected=true — the injector has run.
	statuszBody, _ := json.Marshal(agentd.ReadyzResponse{RelayInjected: true})
	adminSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(statuszBody)
	}))
	adminSrv.Listener = adminListener
	adminSrv.Start()
	defer adminSrv.Close()

	handler := newTestModelsHandler(testPassword)
	handler.SetModelStore(&mockModelReader{model: "big-pickle"})
	handler.SetRelayActive(true)
	handler.SetRelayChecker(func(_ context.Context, _, _ string) bool {
		req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:4098/v1/readyz", nil)
		if err != nil {
			return false
		}
		req.Header.Set("Authorization", "Bearer "+testPassword)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		var rz struct {
			RelayInjected bool `json:"relay_injected"`
		}
		if json.NewDecoder(resp.Body).Decode(&rz) != nil {
			return false
		}
		return rz.RelayInjected
	})

	// Pre-populate the cache with a pre-injection snapshot — RelayInjected:false,
	// big-pickle.providerID="opencode". Reproduces the moment the API server
	// cached a fetch made just before the injector completed.
	stalePayload := modelCachePayload{
		Models: []annotatedModel{{
			ID:            "big-pickle",
			ProviderID:    "opencode",
			Name:          "Big Pickle",
			Enabled:       true,
			Availability:  ModelFreeTier,
			Tier:          "free",
			FreeTier:      true,
			ProxyRequired: true,
		}},
		RelayInjected: false,
	}
	serialized, err := json.Marshal(stalePayload)
	require.NoError(t, err)
	cache := NewInMemoryModelCache()
	cache.Set("ws-1", serialized)
	handler.SetModelCache(cache)

	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userID", "user-1"); c.Next() })
	router.GET("/api/v1/workspaces/:id/models", handler.ListModels)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/models", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Models                 []annotatedModel `json:"models"`
		CurrentModel           string           `json:"currentModel"`
		CurrentModelProviderID string           `json:"currentModelProviderID"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	require.Equal(t, "big-pickle", resp.CurrentModel)
	require.Equal(t, "opencode-relay", resp.CurrentModelProviderID,
		"cache must be invalidated when relayInjected transitions false→true, "+
			"so the served response reflects the post-injection relay remap. "+
			"Issue #467.")

	require.Len(t, resp.Models, 1, "should still return the one model")
	require.Equal(t, "opencode-relay", resp.Models[0].ProviderID,
		"models[] entries must also reflect the post-injection remap (same "+
			"snapshot as currentModelProviderID — they come from the same "+
			"annotated slice). Issue #467.")
}

// TestListModels_CacheSteadyState_NoRefetchWhenInjectedMatches asserts the
// steady-state path through the new cache-invalidation guard: when the cache
// holds a payload with the SAME RelayInjected value as the live relayChecker
// reports, the handler MUST serve the cached snapshot WITHOUT calling
// agentClient.ListModels again. Without this assertion, an always-evict
// regression (e.g. flipping `!=` to `==`, or making the inner body
// unconditional) would pass every existing test while completely defeating
// the cache under the production relay-default. Issue #467 review feedback.
func TestListModels_CacheSteadyState_NoRefetchWhenInjectedMatches(t *testing.T) {
	clearModelCache()
	gin.SetMode(gin.TestMode)

	const testPassword = "steady-state-pw"
	listener, err := net.Listen("tcp", "127.0.0.1:4096")
	if err != nil {
		t.Skip("port 4096 not available")
	}
	adminListener, err := net.Listen("tcp", "127.0.0.1:4098")
	if err != nil {
		t.Skip("port 4098 not available")
	}

	// Count agentClient.ListModels invocations by counting requests to the
	// real opencode test server.
	var listModelsCalls atomic.Int64
	models := `{"connected":["opencode"],"all":[{"id":"opencode","models":{
		"big-pickle":{"id":"big-pickle","name":"Big Pickle","cost":{"input":0,"output":0}}
	}}]}`
	srv := httptest.NewUnstartedServer(authEnforcingHandler(testPassword, func(w http.ResponseWriter, r *http.Request) {
		listModelsCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(models))
	}))
	srv.Listener = listener
	srv.Start()
	defer srv.Close()

	// Live readyz: relayInjected=true — matches the cached payload below.
	statuszBody, _ := json.Marshal(agentd.ReadyzResponse{RelayInjected: true})
	adminSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(statuszBody)
	}))
	adminSrv.Listener = adminListener
	adminSrv.Start()
	defer adminSrv.Close()

	handler := newTestModelsHandler(testPassword)
	handler.SetModelStore(&mockModelReader{model: "big-pickle"})
	handler.SetRelayActive(true)
	handler.SetRelayChecker(func(_ context.Context, _, _ string) bool {
		req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:4098/v1/readyz", nil)
		if err != nil {
			return false
		}
		req.Header.Set("Authorization", "Bearer "+testPassword)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		var rz struct {
			RelayInjected bool `json:"relay_injected"`
		}
		if json.NewDecoder(resp.Body).Decode(&rz) != nil {
			return false
		}
		return rz.RelayInjected
	})

	// Seed the cache with a post-injection (relayInjected=true) snapshot
	// matching what a fresh fetch would have produced. The steady-state
	// guard MUST recognize live==cached and serve this without refetching.
	cachedPayload := modelCachePayload{
		Models: []annotatedModel{{
			ID:            "big-pickle",
			ProviderID:    "opencode-relay",
			Name:          "Big Pickle",
			Enabled:       true,
			Availability:  ModelFreeTier,
			Tier:          "free",
			FreeTier:      true,
			ProxyRequired: true,
		}},
		RelayInjected: true,
	}
	serialized, err := json.Marshal(cachedPayload)
	require.NoError(t, err)
	cache := NewInMemoryModelCache()
	cache.Set("ws-1", serialized)
	handler.SetModelCache(cache)

	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userID", "user-1"); c.Next() })
	router.GET("/api/v1/workspaces/:id/models", handler.ListModels)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/models", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Models                 []annotatedModel `json:"models"`
		CurrentModel           string           `json:"currentModel"`
		CurrentModelProviderID string           `json:"currentModelProviderID"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	require.Equal(t, "opencode-relay", resp.CurrentModelProviderID,
		"cached relayInjected=true snapshot must be served when live state matches")
	require.Equal(t, int64(0), listModelsCalls.Load(),
		"agentClient.ListModels MUST NOT be called when the cached payload's "+
			"RelayInjected matches the live relayChecker result. This guards against "+
			"an always-evict regression that would defeat the cache entirely under "+
			"the production relay-default. Issue #467 review feedback.")
}

// TestListModels_CacheInvalidatedOnReverseTransition asserts the symmetric
// case: cache holds relayInjected=true (post-injection state), but the live
// relayChecker now returns false (e.g. admin disabled the relay, or the
// relay-router peers ConfigMap was cleared, causing agentd's readyz to flip
// back). The guard must evict and re-annotate as the non-relayed state.
// The PR body documents this direction is supported; this test pins it.
// Without this, a future directional rewrite (e.g. `live && !cached`) would
// silently break the reverse path. Issue #467 review feedback.
func TestListModels_CacheInvalidatedOnReverseTransition(t *testing.T) {
	clearModelCache()
	gin.SetMode(gin.TestMode)

	const testPassword = "reverse-transition-pw"
	listener, err := net.Listen("tcp", "127.0.0.1:4096")
	if err != nil {
		t.Skip("port 4096 not available")
	}
	adminListener, err := net.Listen("tcp", "127.0.0.1:4098")
	if err != nil {
		t.Skip("port 4098 not available")
	}

	models := `{"connected":["opencode"],"all":[{"id":"opencode","models":{
		"big-pickle":{"id":"big-pickle","name":"Big Pickle","cost":{"input":0,"output":0}}
	}}]}`
	srv := httptest.NewUnstartedServer(authEnforcingHandler(testPassword, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(models))
	}))
	srv.Listener = listener
	srv.Start()
	defer srv.Close()

	// Live readyz: relayInjected=false — relay is no longer injected
	// (admin disabled it, or the controller cleared the peers).
	statuszBody, _ := json.Marshal(agentd.ReadyzResponse{RelayInjected: false})
	adminSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(statuszBody)
	}))
	adminSrv.Listener = adminListener
	adminSrv.Start()
	defer adminSrv.Close()

	handler := newTestModelsHandler(testPassword)
	handler.SetModelStore(&mockModelReader{model: "big-pickle"})
	handler.SetRelayActive(true)
	handler.SetRelayChecker(func(_ context.Context, _, _ string) bool {
		req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:4098/v1/readyz", nil)
		if err != nil {
			return false
		}
		req.Header.Set("Authorization", "Bearer "+testPassword)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		var rz struct {
			RelayInjected bool `json:"relay_injected"`
		}
		if json.NewDecoder(resp.Body).Decode(&rz) != nil {
			return false
		}
		return rz.RelayInjected
	})

	// Seed cache with a post-injection (relayInjected=true) snapshot —
	// the model was remapped to opencode-relay at that time. The live
	// state has since reverted to false; the guard MUST evict and
	// re-annotate as non-relayed (providerID="opencode").
	stalePayload := modelCachePayload{
		Models: []annotatedModel{{
			ID:            "big-pickle",
			ProviderID:    "opencode-relay",
			Name:          "Big Pickle",
			Enabled:       true,
			Availability:  ModelFreeTier,
			Tier:          "free",
			FreeTier:      true,
			ProxyRequired: true,
		}},
		RelayInjected: true,
	}
	serialized, err := json.Marshal(stalePayload)
	require.NoError(t, err)
	cache := NewInMemoryModelCache()
	cache.Set("ws-1", serialized)
	handler.SetModelCache(cache)

	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userID", "user-1"); c.Next() })
	router.GET("/api/v1/workspaces/:id/models", handler.ListModels)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/models", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Models                 []annotatedModel `json:"models"`
		CurrentModel           string           `json:"currentModel"`
		CurrentModelProviderID string           `json:"currentModelProviderID"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	require.Equal(t, "big-pickle", resp.CurrentModel)
	require.Equal(t, "opencode", resp.CurrentModelProviderID,
		"reverse transition true→false must evict the cache and re-annotate as "+
			"the non-relayed state. The PR body claims this direction is supported; "+
			"this test pins it. Issue #467 review feedback.")
	require.Len(t, resp.Models, 1)
	require.Equal(t, "opencode", resp.Models[0].ProviderID,
		"models[] entries must also reflect the post-eviction non-relayed state")
}

// (TestDoReload_EvictsModelCache removed in PR #494: doReload was
// deleted as part of the agentpush.Service extraction. The equivalent
// invariant — "successful push evicts the workspace's model cache" —
// is now covered by TestPush_SuccessEvictsModelCache in
// api/internal/services/agentpush/agentpush_test.go, at the level of
// the shared implementation both this handler and the workspace
// service consume.)
