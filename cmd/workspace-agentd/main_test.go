// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// === ListSessions ===

func TestListSessions_HappyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		assert.True(t, ok)
		assert.Equal(t, "opencode", user)
		assert.Equal(t, "testpw", pass)
		switch r.URL.Path {
		case "/session":
			json.NewEncoder(w).Encode([]struct {
				ID string `json:"id"`
			}{
				{ID: "ses_1"},
				{ID: "ses_2"},
			})
		case "/session/ses_1":
			json.NewEncoder(w).Encode(map[string]string{"id": "ses_1", "title": "My Chat"})
		case "/session/ses_2":
			json.NewEncoder(w).Encode(map[string]string{"id": "ses_2", "title": ""})
		}
	}))
	defer server.Close()

	client := &OpenCodeClient{password: "testpw", client: &http.Client{Timeout: 5 * time.Second}}
	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(server.URL)

	sessions, err := client.ListSessions(context.Background())
	require.NoError(t, err)
	assert.Len(t, sessions, 2)
	assert.Equal(t, "ses_1", sessions[0].ID)
	assert.Equal(t, "My Chat", sessions[0].Title)
	assert.Equal(t, "idle", sessions[0].Status)
	assert.Equal(t, "ses_2", sessions[1].ID)
	assert.Equal(t, "", sessions[1].Title)
}

func TestListSessions_EmptyList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]struct{}{})
	}))
	defer server.Close()

	client := &OpenCodeClient{password: "pw", client: &http.Client{Timeout: 5 * time.Second}}
	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(server.URL)

	sessions, err := client.ListSessions(context.Background())
	require.NoError(t, err)
	assert.Len(t, sessions, 0)
}

func TestListSessions_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := &OpenCodeClient{password: "pw", client: &http.Client{Timeout: 5 * time.Second}}
	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(server.URL)

	// Server returns 500 but body is empty — decode will fail
	_, err := client.ListSessions(context.Background())
	assert.Error(t, err)
}

func TestListSessions_ConnectionRefused(t *testing.T) {
	client := &OpenCodeClient{password: "pw", client: &http.Client{Timeout: 1 * time.Second}}
	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr("http://127.0.0.1:1") // nothing listening

	_, err := client.ListSessions(context.Background())
	assert.Error(t, err)
}

// === cachedState ===

func TestCachedState_CachesWithinTTL(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		switch r.URL.Path {
		case "/provider":
			json.NewEncoder(w).Encode(map[string][]string{"connected": {"opencode"}})
		case "/config/providers":
			json.NewEncoder(w).Encode(map[string][]struct{}{"providers": {{}}})
		case "/session":
			json.NewEncoder(w).Encode([]struct {
				ID string `json:"id"`
			}{{ID: "ses_1"}})
		case "/session/ses_1":
			json.NewEncoder(w).Encode(map[string]string{"id": "ses_1", "title": "cached"})
		}
	}))
	defer server.Close()

	client := &OpenCodeClient{password: "pw", client: &http.Client{Timeout: 5 * time.Second}}
	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(server.URL)

	cache := &providerCache{}

	// First call populates cache
	connected1, configured1, sessions1 := cachedState(context.Background(), client, cache, newSessionStatusTracker())
	assert.Equal(t, []string{"opencode"}, connected1)
	assert.Equal(t, 1, configured1)
	assert.Len(t, sessions1, 1)
	firstCallCount := callCount

	// Second call within TTL should use cache
	connected2, configured2, sessions2 := cachedState(context.Background(), client, cache, newSessionStatusTracker())
	assert.Equal(t, connected1, connected2)
	assert.Equal(t, configured1, configured2)
	assert.Equal(t, sessions1, sessions2)
	assert.Equal(t, firstCallCount, callCount, "should not make additional HTTP calls within TTL")
}

// === statusz endpoint integration ===

func TestStatuszEndpoint_IncludesSessionsAndDisk(t *testing.T) {
	// Mock opencode server
	opencodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/global/health":
			json.NewEncoder(w).Encode(map[string]interface{}{"healthy": true, "version": "1.0.0"})
		case "/provider":
			json.NewEncoder(w).Encode(map[string][]string{"connected": {"opencode"}})
		case "/config/providers":
			json.NewEncoder(w).Encode(map[string][]struct{}{"providers": {{}}})
		case "/session":
			json.NewEncoder(w).Encode([]struct {
				ID string `json:"id"`
			}{
				{ID: "ses_1"},
			})
		case "/session/ses_1":
			json.NewEncoder(w).Encode(map[string]string{"id": "ses_1", "title": "Test Session"})
		}
	}))
	defer opencodeSrv.Close()

	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(opencodeSrv.URL)

	client := &OpenCodeClient{password: "pw", client: &http.Client{Timeout: 5 * time.Second}}
	cache := &providerCache{}
	tracker := newSessionStatusTracker()
	startedAt := time.Now()

	// Build the handler
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		healthy, version, _ := client.IsHealthy(r.Context())
		connected, configured, sessions := cachedState(r.Context(), client, cache, tracker)
		ready := healthy && len(connected) > 0

		activeCnt := 0
		for _, s := range sessions {
			if s.Status == "busy" {
				activeCnt++
			}
		}

		json.NewEncoder(w).Encode(agentd.StatuszResponse{
			Healthy:             healthy,
			Ready:               ready,
			Connected:           connected,
			ProvidersConfigured: configured,
			Sessions:            sessions,
			SessionsActive:      activeCnt,
			AgentType:           "opencode",
			AgentVersion:        version,
			UptimeSeconds:       int(time.Since(startedAt).Seconds()),
			Disk:                &agentd.DiskUsage{UsedBytes: 100, TotalBytes: 1000},
		})
	})

	req := httptest.NewRequest("GET", "/v1/statusz", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp agentd.StatuszResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.Healthy)
	assert.True(t, resp.Ready)
	assert.Len(t, resp.Sessions, 1)
	assert.Equal(t, "ses_1", resp.Sessions[0].ID)
	assert.Equal(t, "Test Session", resp.Sessions[0].Title)
	assert.Equal(t, "idle", resp.Sessions[0].Status)
	assert.NotNil(t, resp.Disk)
	assert.Equal(t, int64(100), resp.Disk.UsedBytes)
	assert.Equal(t, int64(1000), resp.Disk.TotalBytes)
}

// === statusz context usage integration (S36.3): exercises the real buildStatuszHandler ===

func TestStatuszEndpoint_ContextUsage_PerSessionContextUsed(t *testing.T) {
	opencodeSrv := newOpenCodeTestServer()
	defer opencodeSrv.Close()

	client, cache, tracker := newStatuszTestFixture(t, opencodeSrv)
	tracker.setPromptTokens("ses_1", 15000)
	tracker.setPromptTokens("ses_2", 80000)
	handler := buildStatuszHandler(client, cache, tracker, newMemoryPressureMonitor(), time.Now())

	req := httptest.NewRequest("GET", "/v1/statusz", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp agentd.StatuszResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Len(t, resp.Sessions, 2)
	assert.Equal(t, int64(15000), resp.Sessions[0].ContextUsed)
	assert.Equal(t, int64(80000), resp.Sessions[1].ContextUsed)
	assert.NotNil(t, resp.Context)
	assert.Equal(t, int64(0), resp.Context.UsedTokens, "top-level UsedTokens should be 0")
}

func TestStatuszEndpoint_ContextUsage_EmptySessions(t *testing.T) {
	opencodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"healthy": true, "version": "1.0.0",
			"connected": []string{"opencode"},
			"providers": []interface{}{},
			"sessions":  []interface{}{},
		})
	}))
	defer opencodeSrv.Close()

	client, cache, tracker := newStatuszTestFixture(t, opencodeSrv)
	handler := buildStatuszHandler(client, cache, tracker, newMemoryPressureMonitor(), time.Now())

	req := httptest.NewRequest("GET", "/v1/statusz", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp agentd.StatuszResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Empty(t, resp.Sessions)
	assert.NotNil(t, resp.Context, "context field always present")
	assert.Equal(t, int64(0), resp.Context.TotalTokens, "no sessions → context limit unknown")
	assert.Contains(t, w.Body.String(), `"context":`, "JSON wire format must include context object")
}

func TestStatuszEndpoint_ContextUsage_ColdStart(t *testing.T) {
	opencodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/global/health":
			json.NewEncoder(w).Encode(map[string]interface{}{"healthy": true, "version": "1.0.0"})
		case "/provider":
			json.NewEncoder(w).Encode(map[string][]string{"connected": {"opencode"}})
		case "/config/providers":
			json.NewEncoder(w).Encode(map[string][]struct{}{"providers": {{}}})
		case "/session":
			json.NewEncoder(w).Encode([]struct {
				ID    string `json:"id"`
				Model *struct {
					ID string `json:"id"`
				} `json:"model"`
			}{{ID: "ses_1", Model: &struct {
				ID string `json:"id"`
			}{ID: "glm-5.1"}}})
		case "/session/ses_1":
			json.NewEncoder(w).Encode(map[string]string{"title": ""})
		}
	}))
	defer opencodeSrv.Close()

	client, cache, tracker := newStatuszTestFixture(t, opencodeSrv)
	handler := buildStatuszHandler(client, cache, tracker, newMemoryPressureMonitor(), time.Now())

	req := httptest.NewRequest("GET", "/v1/statusz", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp agentd.StatuszResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Len(t, resp.Sessions, 1)
	assert.Equal(t, int64(0), resp.Sessions[0].ContextUsed, "cold-start session has no SSE data, ContextUsed=0")
}

func TestStatuszEndpoint_OldFieldsUnchanged(t *testing.T) {
	opencodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/global/health":
			json.NewEncoder(w).Encode(map[string]interface{}{"healthy": true, "version": "1.0.0"})
		case "/provider":
			json.NewEncoder(w).Encode(map[string][]string{"connected": {"opencode"}})
		case "/config/providers":
			json.NewEncoder(w).Encode(map[string][]struct{}{"providers": {{}}})
		case "/session":
			json.NewEncoder(w).Encode([]struct {
				ID string `json:"id"`
			}{{ID: "ses_1"}})
		case "/session/ses_1":
			json.NewEncoder(w).Encode(map[string]string{"id": "ses_1", "title": "Test"})
		}
	}))
	defer opencodeSrv.Close()

	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(opencodeSrv.URL)

	client := &OpenCodeClient{password: "pw", client: &http.Client{Timeout: 5 * time.Second}}
	cache := &providerCache{}
	tracker := newSessionStatusTracker()
	startedAt := time.Now()

	handler := buildStatuszHandler(client, cache, tracker, newMemoryPressureMonitor(), startedAt)

	req := httptest.NewRequest("GET", "/v1/statusz", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp agentd.StatuszResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.Healthy)
	assert.True(t, resp.Ready)
	assert.Len(t, resp.Sessions, 1)
	assert.Equal(t, "ses_1", resp.Sessions[0].ID)
	assert.Equal(t, "Test", resp.Sessions[0].Title)
	assert.Equal(t, "idle", resp.Sessions[0].Status)
	if resp.Disk != nil {
		assert.Greater(t, resp.Disk.UsedBytes, int64(0))
		assert.Greater(t, resp.Disk.TotalBytes, int64(0))
	}
}

// newOpenCodeTestServer creates a minimal opencode mock for statusz tests.
func newOpenCodeTestServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/global/health":
			json.NewEncoder(w).Encode(map[string]interface{}{"healthy": true, "version": "1.0.0"})
		case "/provider":
			json.NewEncoder(w).Encode(map[string][]string{"connected": {"opencode"}})
		case "/config/providers":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"providers": []map[string]interface{}{
					{
						"id": "opencode",
						"models": map[string]interface{}{
							"claude-sonnet-4-20250514": map[string]interface{}{
								"id":    "claude-sonnet-4-20250514",
								"limit": map[string]interface{}{"context": 200000},
							},
						},
					},
				},
			})
		case "/session":
			json.NewEncoder(w).Encode([]struct {
				ID    string `json:"id"`
				Model *struct {
					ID string `json:"id"`
				} `json:"model"`
			}{
				{ID: "ses_1", Model: &struct {
					ID string `json:"id"`
				}{ID: "claude-sonnet-4-20250514"}},
				{ID: "ses_2"},
			})
		case "/session/ses_1", "/session/ses_2":
			json.NewEncoder(w).Encode(map[string]string{"title": ""})
		}
	}))
}

// newStatuszTestFixture sets up OpenCodeClient, providerCache, tracker, and agentAddr
// pointing at the given opencode test server. Returns (client, cache, tracker).
func newStatuszTestFixture(t *testing.T, opencodeSrv *httptest.Server) (*OpenCodeClient, *providerCache, *sessionStatusTracker) {
	t.Helper()
	origAddr := getAgentAddr()
	setAgentAddr(opencodeSrv.URL)
	t.Cleanup(func() { setAgentAddr(origAddr) })
	return &OpenCodeClient{password: "pw", client: &http.Client{Timeout: 5 * time.Second}},
		&providerCache{},
		newSessionStatusTracker()
}

// === buildStatuszHandler integration: exercises the real production handler ===
// Unlike the hand-rolled tests above, these call buildStatuszHandler() directly
// so that any change to the production closure is automatically covered.

func TestBuildStatuszHandler_ContextUsed_PerSession(t *testing.T) {
	opencodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/global/health":
			json.NewEncoder(w).Encode(map[string]interface{}{"healthy": true, "version": "2.0.0"})
		case "/provider":
			json.NewEncoder(w).Encode(map[string][]string{"connected": {"opencode"}})
		case "/config/providers":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"providers": []map[string]interface{}{
					{
						"id": "opencode",
						"models": map[string]interface{}{
							"big-pickle": map[string]interface{}{
								"id":    "big-pickle",
								"limit": map[string]interface{}{"context": 128000},
							},
						},
					},
				},
			})
		case "/session":
			json.NewEncoder(w).Encode([]struct {
				ID    string `json:"id"`
				Model *struct {
					ID string `json:"id"`
				} `json:"model"`
			}{{ID: "ses_A", Model: &struct {
				ID string `json:"id"`
			}{ID: "big-pickle"}}})
		case "/session/ses_A":
			json.NewEncoder(w).Encode(map[string]string{"title": "integration test"})
		}
	}))
	defer opencodeSrv.Close()

	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(opencodeSrv.URL)

	client := &OpenCodeClient{password: "pw", client: &http.Client{Timeout: 5 * time.Second}}
	cache := &providerCache{}
	tracker := newSessionStatusTracker()
	tracker.setPromptTokens("ses_A", 55000)
	startedAt := time.Now()

	// Use the real buildStatuszHandler, not a hand-rolled copy.
	handler := buildStatuszHandler(client, cache, tracker, newMemoryPressureMonitor(), startedAt)

	req := httptest.NewRequest("GET", "/v1/statusz", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp agentd.StatuszResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.True(t, resp.Healthy)
	assert.Len(t, resp.Sessions, 1)
	assert.Equal(t, "ses_A", resp.Sessions[0].ID)
	assert.Equal(t, int64(55000), resp.Sessions[0].ContextUsed, "ContextUsed must be threaded from sseTracker into session")
	assert.NotNil(t, resp.Context)
	assert.Equal(t, int64(0), resp.Context.UsedTokens, "top-level UsedTokens must be 0")
	assert.Equal(t, int64(128000), resp.Context.TotalTokens, "TotalTokens from model context limit")
}

func TestBuildStatuszHandler_NoContextUsed_WhenTrackerEmpty(t *testing.T) {
	opencodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/global/health":
			json.NewEncoder(w).Encode(map[string]interface{}{"healthy": true, "version": "1.0.0"})
		case "/provider":
			json.NewEncoder(w).Encode(map[string][]string{"connected": {"opencode"}})
		case "/config/providers":
			json.NewEncoder(w).Encode(map[string]interface{}{"providers": []interface{}{}})
		case "/session":
			json.NewEncoder(w).Encode([]struct {
				ID string `json:"id"`
			}{{ID: "ses_cold"}})
		case "/session/ses_cold":
			json.NewEncoder(w).Encode(map[string]string{"title": "cold start"})
		}
	}))
	defer opencodeSrv.Close()

	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(opencodeSrv.URL)

	client := &OpenCodeClient{password: "pw", client: &http.Client{Timeout: 5 * time.Second}}
	cache := &providerCache{}
	tracker := newSessionStatusTracker() // empty — no SSE data yet
	startedAt := time.Now()

	handler := buildStatuszHandler(client, cache, tracker, newMemoryPressureMonitor(), startedAt)

	req := httptest.NewRequest("GET", "/v1/statusz", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp agentd.StatuszResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Len(t, resp.Sessions, 1)
	assert.Equal(t, int64(0), resp.Sessions[0].ContextUsed, "cold-start: no SSE data → ContextUsed must be 0")
	assert.Contains(t, w.Body.String(), `"contextUsed":0`, "JSON wire format must include contextUsed:0 (omitempty regression guard)")
}

// setAgentAddr is a test helper to override the package-level agentAddr.
func setAgentAddr(addr string) {
	agentAddrAtomic.Store(addr)
}

// === sessionStatusTracker ===

func TestSessionStatusTracker_SetAndGet(t *testing.T) {
	tracker := newSessionStatusTracker()

	assert.Equal(t, "idle", tracker.get("ses_1"), "unknown session defaults to idle")

	tracker.set("ses_1", "busy")
	assert.Equal(t, "busy", tracker.get("ses_1"))

	tracker.set("ses_1", "idle")
	assert.Equal(t, "idle", tracker.get("ses_1"))
}

func TestSessionStatusTracker_ProcessEvent_Flat(t *testing.T) {
	tracker := newSessionStatusTracker()

	// Flat format
	tracker.processEvent(`{"type":"session.status","properties":{"sessionID":"ses_abc","status":{"type":"busy"}}}`)
	assert.Equal(t, "busy", tracker.get("ses_abc"))

	tracker.processEvent(`{"type":"session.status","properties":{"sessionID":"ses_abc","status":{"type":"idle"}}}`)
	assert.Equal(t, "idle", tracker.get("ses_abc"))
}

func TestSessionStatusTracker_ProcessEvent_Nested(t *testing.T) {
	tracker := newSessionStatusTracker()

	// Nested format
	tracker.processEvent(`{"payload":{"type":"session.status","properties":{"sessionID":"ses_xyz","status":{"type":"busy"}}}}`)
	assert.Equal(t, "busy", tracker.get("ses_xyz"))
}

func TestSessionStatusTracker_ProcessEvent_IgnoresOtherTypes(t *testing.T) {
	tracker := newSessionStatusTracker()

	tracker.processEvent(`{"type":"message.created","properties":{"sessionID":"ses_1"}}`)
	assert.Equal(t, "idle", tracker.get("ses_1"), "non session.status events should not set status")
}

func TestSessionStatusTracker_ProcessEvent_StepEnded_CapturesPromptTokens(t *testing.T) {
	tracker := newSessionStatusTracker()

	tracker.processEvent(`{"type":"session.next.step.ended","properties":{"sessionID":"ses_abc","tokens":{"input":800,"output":400,"reasoning":100,"cache":{"read":200,"write":50}}}}`)
	assert.Equal(t, int64(1050), tracker.getPromptTokens("ses_abc"))
}

func TestSessionStatusTracker_ProcessEvent_StepEnded_MissingTokensIgnored(t *testing.T) {
	tracker := newSessionStatusTracker()

	tracker.processEvent(`{"type":"session.next.step.ended","properties":{"sessionID":"ses_abc"}}`)
	assert.Equal(t, int64(0), tracker.getPromptTokens("ses_abc"))
}

func TestSessionStatusTracker_ProcessEvent_StepEnded_EmptySessionIDIgnored(t *testing.T) {
	tracker := newSessionStatusTracker()

	tracker.processEvent(`{"type":"session.next.step.ended","properties":{"sessionID":"","tokens":{"input":100,"output":50,"reasoning":0,"cache":{"read":0,"write":0}}}}`)
	assert.Equal(t, int64(0), tracker.getPromptTokens(""))
}

func TestSessionStatusTracker_ProcessEvent_StepEnded_NestedFormat(t *testing.T) {
	tracker := newSessionStatusTracker()

	tracker.processEvent(`{"payload":{"type":"session.next.step.ended","properties":{"sessionID":"ses_nest","tokens":{"input":500,"output":200,"reasoning":50,"cache":{"read":100,"write":25}}}}}`)
	assert.Equal(t, int64(625), tracker.getPromptTokens("ses_nest"))
}

func TestSessionStatusTracker_GetPromptTokens_NoData_ReturnsZero(t *testing.T) {
	tracker := newSessionStatusTracker()
	assert.Equal(t, int64(0), tracker.getPromptTokens("nonexistent"))
}

func TestSessionStatusTracker_GetPromptTokens_ExistingData_ReturnsValue(t *testing.T) {
	tracker := newSessionStatusTracker()
	tracker.setPromptTokens("ses_1", 5000)
	assert.Equal(t, int64(5000), tracker.getPromptTokens("ses_1"))
}

func TestSessionStatusTracker_HasPromptTokens(t *testing.T) {
	tracker := newSessionStatusTracker()
	assert.False(t, tracker.hasPromptTokens("ses_1"))
	tracker.setPromptTokens("ses_1", 100)
	assert.True(t, tracker.hasPromptTokens("ses_1"))
}

func TestSessionStatusTracker_Prune_RemovesPromptTokens(t *testing.T) {
	tracker := newSessionStatusTracker()
	tracker.set("ses_1", "busy")
	tracker.setPromptTokens("ses_1", 5000)
	tracker.set("ses_2", "idle")
	tracker.setPromptTokens("ses_2", 3000)
	tracker.set("ses_old", "busy")
	tracker.setPromptTokens("ses_old", 90000)

	tracker.prune([]string{"ses_1", "ses_2"})

	assert.Equal(t, int64(5000), tracker.getPromptTokens("ses_1"))
	assert.Equal(t, int64(3000), tracker.getPromptTokens("ses_2"))
	assert.Equal(t, int64(0), tracker.getPromptTokens("ses_old"), "pruned session should return 0 prompt tokens")
	assert.False(t, tracker.hasPromptTokens("ses_old"))
}

func TestSessionStatusTracker_ProcessEvent_SessionStatus_UnchangedBehavior(t *testing.T) {
	tracker := newSessionStatusTracker()

	tracker.processEvent(`{"type":"session.status","properties":{"sessionID":"ses_abc","status":{"type":"busy"}}}`)
	assert.Equal(t, "busy", tracker.get("ses_abc"))

	tracker.processEvent(`{"type":"session.status","properties":{"sessionID":"ses_abc","status":{"type":"idle"}}}`)
	assert.Equal(t, "idle", tracker.get("ses_abc"))
}

func TestSessionStatusTracker_ProcessEvent_InvalidJSON(t *testing.T) {
	tracker := newSessionStatusTracker()
	tracker.processEvent(`not json at all`)
	assert.Equal(t, "idle", tracker.get("anything"))
}

func TestSessionStatusTracker_Prune(t *testing.T) {
	tracker := newSessionStatusTracker()
	tracker.set("ses_1", "busy")
	tracker.set("ses_2", "idle")
	tracker.set("ses_old", "busy")

	tracker.prune([]string{"ses_1", "ses_2"})

	assert.Equal(t, "busy", tracker.get("ses_1"))
	assert.Equal(t, "idle", tracker.get("ses_2"))
	assert.Equal(t, "idle", tracker.get("ses_old"), "pruned session should return default idle")

	// Verify map size
	tracker.mu.RLock()
	assert.Len(t, tracker.statuses, 2)
	tracker.mu.RUnlock()
}

func TestSessionStatusTracker_MergesIntoCachedState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/provider":
			json.NewEncoder(w).Encode(map[string][]string{"connected": {"opencode"}})
		case "/config/providers":
			json.NewEncoder(w).Encode(map[string][]struct{}{"providers": {{}}})
		case "/session":
			json.NewEncoder(w).Encode([]struct {
				ID string `json:"id"`
			}{{ID: "ses_1"}, {ID: "ses_2"}})
		case "/session/ses_1", "/session/ses_2":
			json.NewEncoder(w).Encode(map[string]string{"title": ""})
		}
	}))
	defer server.Close()

	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(server.URL)

	client := &OpenCodeClient{password: "pw", client: &http.Client{Timeout: 5 * time.Second}}
	cache := &providerCache{}
	tracker := newSessionStatusTracker()

	// Simulate SSE event marking ses_1 as busy
	tracker.set("ses_1", "busy")

	_, _, sessions := cachedState(context.Background(), client, cache, tracker)

	assert.Len(t, sessions, 2)
	assert.Equal(t, "busy", sessions[0].Status)
	assert.Equal(t, "idle", sessions[1].Status)
}

// === fetchSessionPromptTokens ===

func TestFetchSessionPromptTokens_AssistantWithTokens(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/session/ses_1/message", r.URL.Path)
		assert.Equal(t, "20", r.URL.Query().Get("limit"))
		json.NewEncoder(w).Encode([]struct {
			Info struct {
				Role   string `json:"role"`
				Tokens *struct {
					Input int64 `json:"input"`
					Cache struct {
						Read  int64 `json:"read"`
						Write int64 `json:"write"`
					} `json:"cache"`
				} `json:"tokens"`
			} `json:"info"`
		}{
			{Info: struct {
				Role   string `json:"role"`
				Tokens *struct {
					Input int64 `json:"input"`
					Cache struct {
						Read  int64 `json:"read"`
						Write int64 `json:"write"`
					} `json:"cache"`
				} `json:"tokens"`
			}{Role: "user"}},
			{Info: struct {
				Role   string `json:"role"`
				Tokens *struct {
					Input int64 `json:"input"`
					Cache struct {
						Read  int64 `json:"read"`
						Write int64 `json:"write"`
					} `json:"cache"`
				} `json:"tokens"`
			}{Role: "assistant", Tokens: &struct {
				Input int64 `json:"input"`
				Cache struct {
					Read  int64 `json:"read"`
					Write int64 `json:"write"`
				} `json:"cache"`
			}{Input: 1024, Cache: struct {
				Read  int64 `json:"read"`
				Write int64 `json:"write"`
			}{Read: 200, Write: 50}}}},
		})
	}))
	defer server.Close()

	client := &OpenCodeClient{password: "pw", client: &http.Client{Timeout: 5 * time.Second}}
	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(server.URL)

	tokens := client.fetchSessionPromptTokens(context.Background(), "ses_1")
	assert.Equal(t, int64(1274), tokens) // 1024 + 200 + 50
}

func TestFetchSessionPromptTokens_NoAssistant_ReturnsZero(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]struct {
			Info struct {
				Role string `json:"role"`
			} `json:"info"`
		}{
			{Info: struct {
				Role string `json:"role"`
			}{Role: "user"}},
		})
	}))
	defer server.Close()

	client := &OpenCodeClient{password: "pw", client: &http.Client{Timeout: 5 * time.Second}}
	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(server.URL)

	tokens := client.fetchSessionPromptTokens(context.Background(), "ses_1")
	assert.Equal(t, int64(0), tokens)
}

func TestFetchSessionPromptTokens_APIError_ReturnsZero(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := &OpenCodeClient{password: "pw", client: &http.Client{Timeout: 5 * time.Second}}
	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(server.URL)

	tokens := client.fetchSessionPromptTokens(context.Background(), "ses_1")
	assert.Equal(t, int64(0), tokens)
}

func TestFetchSessionPromptTokens_InvalidJSON_ReturnsZero(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer server.Close()

	client := &OpenCodeClient{password: "pw", client: &http.Client{Timeout: 5 * time.Second}}
	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(server.URL)

	tokens := client.fetchSessionPromptTokens(context.Background(), "ses_1")
	assert.Equal(t, int64(0), tokens)
}

func TestFillGaps_SkipsKnownSessions(t *testing.T) {
	tracker := newSessionStatusTracker()
	tracker.setPromptTokens("ses_1", 5000)

	var fetched []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetched = append(fetched, r.URL.Path)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode([]struct{}{})
	}))
	defer server.Close()

	client := &OpenCodeClient{password: "pw", client: &http.Client{Timeout: 5 * time.Second}}
	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(server.URL)

	state := &fillGapsState{}
	runFill(context.Background(), client, tracker, func() []agentd.SessionInfo {
		return []agentd.SessionInfo{{ID: "ses_1"}}
	}, state)

	assert.Empty(t, fetched, "should not fetch for sessions with known prompt tokens")
}

func TestFillGaps_FillsUnknownSessions(t *testing.T) {
	tracker := newSessionStatusTracker()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]struct {
			Info struct {
				Role   string `json:"role"`
				Tokens *struct {
					Input int64 `json:"input"`
					Cache struct {
						Read  int64 `json:"read"`
						Write int64 `json:"write"`
					} `json:"cache"`
				} `json:"tokens"`
			} `json:"info"`
		}{
			{Info: struct {
				Role   string `json:"role"`
				Tokens *struct {
					Input int64 `json:"input"`
					Cache struct {
						Read  int64 `json:"read"`
						Write int64 `json:"write"`
					} `json:"cache"`
				} `json:"tokens"`
			}{Role: "assistant", Tokens: &struct {
				Input int64 `json:"input"`
				Cache struct {
					Read  int64 `json:"read"`
					Write int64 `json:"write"`
				} `json:"cache"`
			}{Input: 3000, Cache: struct {
				Read  int64 `json:"read"`
				Write int64 `json:"write"`
			}{Read: 500, Write: 100}}}},
		})
	}))
	defer server.Close()

	client := &OpenCodeClient{password: "pw", client: &http.Client{Timeout: 5 * time.Second}}
	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(server.URL)

	state := &fillGapsState{}
	runFill(context.Background(), client, tracker, func() []agentd.SessionInfo {
		return []agentd.SessionInfo{{ID: "ses_new"}}
	}, state)

	assert.Equal(t, int64(3600), tracker.getPromptTokens("ses_new"))
}

func TestFillGaps_SkipsIfAlreadyRunning(t *testing.T) {
	tracker := newSessionStatusTracker()
	state := &fillGapsState{}
	state.running = true

	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode([]struct{}{})
	}))
	defer server.Close()

	client := &OpenCodeClient{password: "pw", client: &http.Client{Timeout: 5 * time.Second}}
	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(server.URL)

	runFill(context.Background(), client, tracker, func() []agentd.SessionInfo {
		return []agentd.SessionInfo{{ID: "ses_1"}}
	}, state)

	assert.Equal(t, 0, callCount, "should not fetch when already running")
}

func TestGracefulShutdown(t *testing.T) {
	srv := &http.Server{
		Handler:           http.NewServeMux(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		done <- srv.Serve(ln)
	}()

	resp, err := http.Get("http://" + ln.Addr().String() + "/")
	require.NoError(t, err)
	_ = resp.Body.Close()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	require.NoError(t, err)

	select {
	case err := <-done:
		assert.Equal(t, http.ErrServerClosed, err)
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down within 5s")
	}
}

func TestBackgroundContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	exited := make(chan struct{})
	go func() {
		<-ctx.Done()
		close(exited)
	}()

	cancel()

	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine did not exit after context cancellation")
	}
}

func TestConcurrentServerShutdown(t *testing.T) {
	blockDuration := 500 * time.Millisecond
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(blockDuration)
		w.WriteHeader(http.StatusOK)
	})

	srv1 := &http.Server{Handler: handler}
	srv2 := &http.Server{Handler: handler}

	ln1, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	go srv1.Serve(ln1)
	go srv2.Serve(ln2)
	defer srv1.Close()
	defer srv2.Close()

	time.Sleep(100 * time.Millisecond)

	go func() { _, _ = http.Get("http://" + ln1.Addr().String() + "/") }()
	go func() { _, _ = http.Get("http://" + ln2.Addr().String() + "/") }()

	time.Sleep(50 * time.Millisecond)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = srv1.Shutdown(shutdownCtx)
	}()
	go func() {
		defer wg.Done()
		_ = srv2.Shutdown(shutdownCtx)
	}()
	wg.Wait()

	elapsed := time.Since(start)
	assert.Less(t, elapsed, 2*blockDuration,
		"concurrent shutdown should complete in less than %v, got %v", 2*blockDuration, elapsed)
}

func TestFetchFreeModels_RespectsContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := fetchFreeModels(ctx, srv.URL, "pw")
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Less(t, elapsed, 1*time.Second,
		"fetchFreeModels should return promptly after context cancellation, got %v", elapsed)
}

func TestStartRelayInjector_ExitsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	killed := make(chan struct{}, 1)
	startRelayInjector(ctx, relayInjectorConfig{
		RelayURL:     "https://relay.example.com/s",
		HealthCheck:  func() bool { return false },
		KillOpenCode: func() { killed <- struct{}{} },
	})

	baseline := runtime.NumGoroutine()
	cancel()

	exited := false
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() < baseline {
			exited = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	assert.True(t, exited, "relay injector goroutine should exit within 1s after context cancellation")

	select {
	case <-killed:
		t.Fatal("KillOpenCode must not be called when health check never passes")
	default:
	}
}

// === G46: readAgentPassword ===

// TestReadAgentPasswordFromPath_HappyPath verifies the password is read
// and trimmed correctly. This exercises the real production code path
// (readAgentPasswordFromPath) rather than testing Go stdlib functions.
func TestReadAgentPasswordFromPath_HappyPath(t *testing.T) {
	dir := t.TempDir()
	pwPath := dir + "/password"
	const want = "super-secret-password-12345"
	require.NoError(t, os.WriteFile(pwPath, []byte(want+"\n"), 0o600))

	got, err := readAgentPasswordFromPath(pwPath)
	require.NoError(t, err)
	assert.Equal(t, want, got, "password should be read and trimmed (trailing newline stripped)")
}

// TestReadAgentPasswordFromPath_MissingFileReturnsError verifies the
// G46 behavioral contract: a missing password file returns an error
// (not an empty string with nil error). The caller (readAgentPassword)
// uses this error to trigger os.Exit(1) — pre-fix the function returned
// an empty string and logged only a Warn, leaving the workspace silently
// non-functional.
//
// This test does NOT exercise os.Exit (which would kill the test
// process). The error return is the meaningful contract; the caller's
// Error+exit behavior is the caller's responsibility.
func TestReadAgentPasswordFromPath_MissingFileReturnsError(t *testing.T) {
	dir := t.TempDir()
	pwPath := dir + "/nonexistent-password"

	got, err := readAgentPasswordFromPath(pwPath)
	require.Error(t, err, "G46: missing password file must return an error (pre-fix: returned empty string + nil error)")
	assert.Empty(t, got, "error path must not return a partial password")
}
