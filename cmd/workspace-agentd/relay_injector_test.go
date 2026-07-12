// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// Tests for relay_injector.go — the Epic 26 post-boot relay config injection.
//
// Note: buildRelayConfig logic (merge relay provider into existing config) is
// now tested via agent_config_writer_test.go (TestAgentConfigWriter_Rebuild_*).
// activeRelayModels coordination is removed; relay state lives in
// AgentConfigWriter and is tested via TestAgentConfigWriter_HasRelay.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- shouldSkipRelay tests ---

func TestShouldSkipRelay_SkipsWhenPersonalKey(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	require.NoError(t, os.WriteFile(authPath,
		[]byte(`{"opencode":{"type":"api","key":"sk-personal-abc123"}}`), 0o600))

	skip, reason := shouldSkipRelay(authPath)
	assert.True(t, skip)
	assert.Contains(t, reason, "personal")
}

func TestShouldSkipRelay_DoesNotSkipWithPublicKey(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	require.NoError(t, os.WriteFile(authPath,
		[]byte(`{"opencode":{"type":"api","key":"public"}}`), 0o600))

	skip, _ := shouldSkipRelay(authPath)
	assert.False(t, skip)
}

func TestShouldSkipRelay_DoesNotSkipWithNoEntry(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	require.NoError(t, os.WriteFile(authPath, []byte(`{}`), 0o600))

	skip, _ := shouldSkipRelay(authPath)
	assert.False(t, skip)
}

func TestShouldSkipRelay_DoesNotSkipWithMissingFile(t *testing.T) {
	skip, _ := shouldSkipRelay("/nonexistent/auth.json")
	assert.False(t, skip)
}

// --- fetchFreeModels tests ---

func TestFetchFreeModels_FiltersCorrectly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"connected": []string{"opencode"},
			"all": []map[string]interface{}{
				{"id": "opencode", "models": map[string]interface{}{
					"free-model": map[string]interface{}{
						"id": "free-model", "name": "Free Model",
						"cost":  map[string]float64{"input": 0, "output": 0},
						"limit": map[string]int{"context": 100000, "output": 10000},
					},
					"paid-model": map[string]interface{}{
						"id": "paid-model", "name": "Paid Model",
						"cost":  map[string]float64{"input": 0.01, "output": 0.03},
						"limit": map[string]int{"context": 200000, "output": 20000},
					},
				}},
				{"id": "anthropic", "models": map[string]interface{}{
					"claude": map[string]interface{}{
						"id": "claude", "name": "Claude",
						"cost":  map[string]float64{"input": 0, "output": 0},
						"limit": map[string]int{"context": 200000, "output": 8000},
					},
				}},
			},
		})
	}))
	defer srv.Close()

	models, err := fetchFreeModels(context.Background(), srv.URL, "pw")
	require.NoError(t, err)
	require.Len(t, models, 1, "only free opencode models")
	assert.Equal(t, "free-model", models[0].ID)
	assert.Equal(t, 100000, models[0].ContextLimit)
}

func TestFetchFreeModels_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := fetchFreeModels(context.Background(), srv.URL, "pw")
	assert.Error(t, err)
}

// --- updateAuthJSONForRelay tests ---

func TestUpdateAuthJSONForRelay_AddsRelayEntry(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	require.NoError(t, os.WriteFile(authPath,
		[]byte(`{"opencode":{"type":"api","key":"public"}}`), 0o600))

	require.NoError(t, updateAuthJSONForRelay(authPath))

	data, _ := os.ReadFile(authPath)
	var auth map[string]map[string]string
	require.NoError(t, json.Unmarshal(data, &auth))
	assert.Equal(t, "public", auth["opencode-relay"]["key"])
	assert.Equal(t, "public", auth["opencode"]["key"], "existing entry preserved")
}

func TestUpdateAuthJSONForRelay_CreatesFileIfMissing(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")

	require.NoError(t, updateAuthJSONForRelay(authPath))

	data, err := os.ReadFile(authPath)
	require.NoError(t, err)
	var auth map[string]map[string]string
	require.NoError(t, json.Unmarshal(data, &auth))
	assert.Equal(t, "public", auth["opencode-relay"]["key"])
}

// --- startRelayInjector integration tests ---

func TestStartRelayInjector_SkipsWhenNoRelayURL(t *testing.T) {
	killed := false
	startRelayInjector(context.Background(), relayInjectorConfig{
		RelayURL:     "",
		KillOpenCode: func() { killed = true },
		HealthCheck:  func() bool { return true },
	})
	time.Sleep(50 * time.Millisecond)
	assert.False(t, killed, "KillOpenCode must not be called when RelayURL is empty")
}

func TestStartRelayInjector_SkipsWhenPersonalKey(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	require.NoError(t, os.WriteFile(authPath,
		[]byte(`{"opencode":{"type":"api","key":"sk-personal-abc123"}}`), 0o600))

	killed := false
	writer := newAgentConfigWriter(filepath.Join(dir, "agent-config.json"))
	startRelayInjector(context.Background(), relayInjectorConfig{
		RelayURL:          "https://relay.example.test/path",
		AuthJSONPath:      authPath,
		AgentConfigWriter: writer,
		HealthCheck:       func() bool { return true },
		KillOpenCode:      func() { killed = true },
	})
	time.Sleep(100 * time.Millisecond)
	assert.False(t, killed, "KillOpenCode must not be called when user has personal key")
	assert.False(t, writer.hasRelay(), "writer must not have relay when skipped")
}

// TestStartRelayInjector_WritesConfigAndKills verifies the full injection path:
// health check passes → models fetched → writer.SetRelay + Rebuild → auth.json
// updated → KillOpenCode called.
func TestStartRelayInjector_WritesConfigAndKills(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent-config.json")
	authPath := filepath.Join(dir, "auth.json")
	require.NoError(t, os.WriteFile(authPath,
		[]byte(`{"opencode":{"type":"api","key":"public"}}`), 0o600))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"connected": ["opencode"],
			"all": [
				{"id":"opencode","models":{
					"free-model": {"id":"free-model","name":"Free Model","cost":{"input":0,"output":0},"limit":{"context":100000,"output":10000}}
				}}
			]
		}`))
	}))
	defer srv.Close()

	writer := newAgentConfigWriter(cfgPath)
	killed := make(chan struct{}, 1)
	startRelayInjector(context.Background(), relayInjectorConfig{
		RelayURL:          "https://relay.example.test/path",
		OpenCodeBaseURL:   srv.URL,
		OpenCodePassword:  "testpw",
		AgentConfigPath:   cfgPath,
		AuthJSONPath:      authPath,
		AgentConfigWriter: writer,
		HealthCheck:       func() bool { return true },
		KillOpenCode:      func() { close(killed) },
	})

	select {
	case <-killed:
		time.Sleep(10 * time.Millisecond)
	case <-time.After(2 * time.Second):
		t.Fatal("KillOpenCode was not called within 2s")
	}

	// Verify writer has relay state
	assert.True(t, writer.hasRelay(), "writer must have relay after injection")

	// Verify config file was written by the writer
	data, err := os.ReadFile(cfgPath)
	require.NoError(t, err)

	var cfg map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &cfg))

	var disabled []string
	require.NoError(t, json.Unmarshal(cfg["disabled_providers"], &disabled))
	assert.Contains(t, disabled, "opencode")

	// Verify auth.json updated
	authData, _ := os.ReadFile(authPath)
	var auth map[string]map[string]string
	require.NoError(t, json.Unmarshal(authData, &auth))
	assert.Equal(t, "public", auth["opencode-relay"]["key"])
}

// TestStartRelayInjector_RetriesWhenZeroModels verifies the race-condition fix:
// when the first /provider call returns opencode connected but no free models
// (catalog not yet fully initialized), the relay injector retries rather than
// permanently skipping.
func TestStartRelayInjector_RetriesWhenZeroModels(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/global/health":
			_ = json.NewEncoder(w).Encode(map[string]bool{"healthy": true})
		case "/provider":
			callCount++
			w.Header().Set("Content-Type", "application/json")
			if callCount == 1 {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"connected": []string{"opencode"},
					"all": []map[string]interface{}{
						{"id": "opencode", "models": map[string]interface{}{}},
					},
				})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"connected": []string{"opencode"},
					"all": []map[string]interface{}{
						{"id": "opencode", "models": map[string]interface{}{
							"glm-5.1-free": map[string]interface{}{
								"id": "glm-5.1-free", "name": "GLM 5.1 Free",
								"cost":  map[string]float64{"input": 0, "output": 0},
								"limit": map[string]int{"context": 8192, "output": 2048},
							},
						}},
					},
				})
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	agentConfigPath := filepath.Join(dir, "agent-config.json")
	authPath := filepath.Join(dir, "auth.json")
	require.NoError(t, os.WriteFile(authPath,
		[]byte(`{"opencode":{"type":"api","key":"public"}}`), 0o600))

	writer := newAgentConfigWriter(agentConfigPath)
	killed := make(chan struct{})

	cfg := relayInjectorConfig{
		RelayURL:          "https://relay.test/secret",
		OpenCodeBaseURL:   srv.URL,
		OpenCodePassword:  "pw",
		AgentConfigPath:   agentConfigPath,
		AuthJSONPath:      authPath,
		AgentConfigWriter: writer,
		HealthCheck:       func() bool { return true },
		KillOpenCode:      func() { close(killed) },
	}

	startRelayInjector(context.Background(), cfg)

	select {
	case <-killed:
	case <-time.After(30 * time.Second):
		t.Fatal("relay injector did not retry after 0-model response within 30s")
	}

	assert.True(t, writer.hasRelay(), "writer must have relay after successful retry")
	assert.Equal(t, 2, callCount, "expected exactly 2 /provider calls (initial + retry)")
}

// TestStartRelayInjector_DoesNotSetRelayWhenSkipped verifies that when relay
// injection is skipped (personal key), the writer does not have relay state
// so the readyz handler reports RelayInjected=false.
func TestStartRelayInjector_DoesNotSetRelayWhenSkipped(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	require.NoError(t, os.WriteFile(authPath,
		[]byte(`{"opencode":{"type":"api","key":"sk-personal-key"}}`), 0o600))

	writer := newAgentConfigWriter(filepath.Join(dir, "agent-config.json"))
	killed := false
	startRelayInjector(context.Background(), relayInjectorConfig{
		RelayURL:          "https://relay.example.test/path",
		AuthJSONPath:      authPath,
		AgentConfigWriter: writer,
		HealthCheck:       func() bool { return true },
		KillOpenCode:      func() { killed = true },
	})
	time.Sleep(100 * time.Millisecond)

	assert.False(t, killed)
	assert.False(t, writer.hasRelay(),
		"writer must not have relay when injection is skipped for personal key")
}

// --- relayURLHost test ---

func TestRelayURLHost(t *testing.T) {
	tests := []struct {
		rawURL string
		want   string
	}{
		{"https://relay.example.test/path", "https://relay.example.test"},
		{"https://relay.example.test/path", "https://relay.example.test"},
		{"https://relay.example.test", "https://relay.example.test"},
		{"http://localhost:8080/secret", "http://localhost:8080"},
		{"not-a-url", "://"},
		{"", "://"},
	}
	for _, tt := range tests {
		t.Run(tt.rawURL, func(t *testing.T) {
			got := relayURLHost(tt.rawURL)
			assert.Equal(t, tt.want, got)
			assert.NotContains(t, got, "supersecrettoken")
			assert.NotContains(t, got, "/secret")
		})
	}
}

// TestStartRelayInjector_SkipsWhenPreBootApplied verifies the
// Phase D short-circuit: if the materialize subcommand has already
// pre-injected the relay block via the cluster-wide free-models
// ConfigMap, the in-pod injector goroutine MUST exit immediately
// without waiting for opencode health, fetching models, or killing
// opencode.
//
// This is the entire point of Phases A+B+C+D collectively — opencode
// boots ONCE with the final config.
//
// 2026-06-23 cold-start optimization, item #1a (Phase D).
func TestStartRelayInjector_SkipsWhenPreBootApplied(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent-config.json")

	// Seed agent-config.json with a pre-injected relay block (as
	// applyRelayConfigPreBoot would have written). The writer's
	// loadExisting will detect provider.opencode-relay and set
	// w.relay → hasRelay() returns true.
	require.NoError(t, os.WriteFile(cfgPath, []byte(`{
		"$schema": "https://opencode.ai/config.json",
		"provider": {
			"opencode-relay": {
				"name": "OpenCode Zen (Free)",
				"npm": "@ai-sdk/openai-compatible",
				"options": {"baseURL": "https://relay.test/", "apiKey": "public"},
				"models": {"free-a": {"name": "Free A", "limit": {"context": 100000, "output": 8000}}}
			}
		},
		"disabled_providers": ["opencode"]
	}`), 0o600))

	writer := newAgentConfigWriter(cfgPath)
	require.True(t, writer.hasRelay(),
		"writer must observe pre-injected relay block via loadExisting — "+
			"this is what enables the Phase D short-circuit")

	healthChecks := 0
	killed := false
	startRelayInjector(context.Background(), relayInjectorConfig{
		RelayURL:          "https://relay.test/",
		AgentConfigWriter: writer,
		HealthCheck: func() bool {
			healthChecks++
			return true
		},
		KillOpenCode: func() { killed = true },
	})
	// Give any goroutine a chance to fire.
	time.Sleep(100 * time.Millisecond)

	assert.Zero(t, healthChecks,
		"HealthCheck must not be called — the goroutine must short-circuit before starting")
	assert.False(t, killed,
		"KillOpenCode must not be called — opencode is already booting with the right config")
}

// TestLoadExisting_DetectsPreInjectedRelay covers the Phase D flag
// flip in agent_config_writer.go: a provider.opencode-relay entry
// in the on-disk file at agentd startup must populate w.relay so
// hasRelay() reports true.
func TestLoadExisting_DetectsPreInjectedRelay(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent-config.json")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`{
		"provider": {
			"openai": {"options": {"apiKey": "sk-test"}},
			"opencode-relay": {
				"options": {"baseURL": "https://relay.example/secret", "apiKey": "public"},
				"models": {
					"m1": {"name": "Model 1", "limit": {"context": 1000, "output": 500}},
					"m2": {"name": "Model 2", "limit": {"context": 2000, "output": 1000}}
				}
			}
		},
		"model": "openai/gpt-4"
	}`), 0o600))

	w := newAgentConfigWriter(cfgPath)

	require.True(t, w.hasRelay(),
		"a pre-injected opencode-relay entry must trigger hasRelay()=true")

	// The URL and models extracted should match what was on disk.
	require.NotNil(t, w.relay)
	assert.Equal(t, "https://relay.example/secret", w.relay.url)
	assert.Len(t, w.relay.models, 2)
}

// TestLoadExisting_NoRelayBlock_HasRelayFalse is the negative case:
// a config without provider.opencode-relay must leave hasRelay()=false
// so the in-pod injector still runs (legacy fallback path).
func TestLoadExisting_NoRelayBlock_HasRelayFalse(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent-config.json")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`{
		"provider": {"openai": {"options": {"apiKey": "sk-test"}}},
		"model": "openai/gpt-4"
	}`), 0o600))

	w := newAgentConfigWriter(cfgPath)
	assert.False(t, w.hasRelay(),
		"config with no opencode-relay block must leave hasRelay()=false — "+
			"this is what makes the legacy in-pod injection path still run when Phase C didn't apply")
}

// TestLoadExisting_MalformedRelayBlock_StillSetsRelay verifies the
// safety net: even an unparseable opencode-relay block produces a
// non-nil w.relay (sentinel) so hasRelay() doesn't lie.
//
// Rationale: if the relay block exists but we can't parse it, the
// safest behavior is to assume it's there (pessimistic from
// hasRelay()'s perspective) and let the writer's Rebuild regenerate
// it from defaults if anyone calls it. Worst case: in-pod injection
// is skipped redundantly and the user gets a dud relay until next
// reload — but we don't double-inject, which would race the file
// write.
func TestLoadExisting_MalformedRelayBlock_StillSetsRelay(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent-config.json")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`{
		"provider": {
			"opencode-relay": "this should be an object not a string"
		}
	}`), 0o600))

	w := newAgentConfigWriter(cfgPath)
	assert.True(t, w.hasRelay(),
		"unparseable but PRESENT opencode-relay block must still trigger hasRelay()=true — "+
			"non-nil sentinel prevents redundant in-pod injection")
}
