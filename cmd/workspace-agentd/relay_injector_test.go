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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	opencode "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
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
	writer := opencode.NewConfigWriter(filepath.Join(dir, "agent-config.json"))
	startRelayInjector(context.Background(), relayInjectorConfig{
		RelayURL:          "https://relay.example.test/path",
		AuthJSONPath:      authPath,
		AgentConfigWriter: writer,
		HealthCheck:       func() bool { return true },
		KillOpenCode:      func() { killed = true },
	})
	time.Sleep(100 * time.Millisecond)
	assert.False(t, killed, "KillOpenCode must not be called when user has personal key")
	assert.False(t, writer.HasRelay(), "writer must not have relay when skipped")
}

// TestStartRelayInjector_RetriesTransientFetchErrors verifies that a transient
// GET /provider failure (e.g. truncated response → decode EOF, observed in
// production pods) is retried within the fetch deadline instead of
// permanently skipping relay injection for the pod's lifetime. Before this
// fix, one transient error left free-tier sessions routing direct-to-Zen,
// which then failed with ModelUnavailableError.
func TestStartRelayInjector_RetriesTransientFetchErrors(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent-config.json")
	authPath := filepath.Join(dir, "auth.json")
	require.NoError(t, os.WriteFile(authPath,
		[]byte(`{"opencode":{"type":"api","key":"public"}}`), 0o600))

	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			// Truncated body — mimics the production "decode /provider:
			// unexpected EOF" failure.
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"connected":["open`))
			return
		}
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

	writer := opencode.NewConfigWriter(cfgPath)
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
		FetchRetryDelay:   10 * time.Millisecond,
		FetchDeadline:     2 * time.Second,
	})

	select {
	case <-killed:
	case <-time.After(3 * time.Second):
		t.Fatal("KillOpenCode was not called within 3s — transient fetch error was not retried")
	}
	assert.True(t, writer.HasRelay(), "writer must have relay after retry succeeds")
	assert.GreaterOrEqual(t, atomic.LoadInt32(&attempts), int32(2), "fetch must have been retried")
}

// TestStartRelayInjector_FetchErrorDeadlineExhausted_Skips verifies the
// give-up path: persistent fetch errors are retried until the deadline, then
// the injector skips (bounded, no infinite loop, no kill).
func TestStartRelayInjector_FetchErrorDeadlineExhausted_Skips(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent-config.json")
	authPath := filepath.Join(dir, "auth.json")
	require.NoError(t, os.WriteFile(authPath,
		[]byte(`{"opencode":{"type":"api","key":"public"}}`), 0o600))

	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	writer := opencode.NewConfigWriter(cfgPath)
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
		FetchRetryDelay:   10 * time.Millisecond,
		FetchDeadline:     200 * time.Millisecond,
	})

	// No kill, no relay — but the injector must have stopped (deadline).
	select {
	case <-killed:
		t.Fatal("KillOpenCode must not be called when fetch never succeeds")
	case <-time.After(500 * time.Millisecond):
	}
	assert.False(t, writer.HasRelay())
	assert.GreaterOrEqual(t, atomic.LoadInt32(&attempts), int32(2), "errors must be retried before giving up")
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

	writer := opencode.NewConfigWriter(cfgPath)
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
	assert.True(t, writer.HasRelay(), "writer must have relay after injection")

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

// TestStartRelayInjector_ConfigWriteFailure_DoesNotKill verifies the
// config_write_failed outcome path (relay_injector.go ~line 379-382):
// when Apply returns an error, the relay injector must log the error
// and RETURN without calling KillOpenCode. Killing opencode after a
// failed config write would boot it with the previous (relay-less)
// config — defeating the injection attempt.
//
// Review follow-up on PR #713: the reviewer flagged this outcome path
// as untested. Triggered by pointing the writer at an unwritable path.
func TestStartRelayInjector_ConfigWriteFailure_DoesNotKill(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root — chmod bits ineffective, cannot test write failure")
	}

	readonlyDir := t.TempDir()
	targetDir := filepath.Join(readonlyDir, "noread")
	require.NoError(t, os.Mkdir(targetDir, 0o555)) // r-x for all, no w

	cfgPath := filepath.Join(targetDir, "agent-config.json")
	authPath := filepath.Join(readonlyDir, "auth.json")
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

	writer := opencode.NewConfigWriter(cfgPath)
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

	// The injector must NOT call KillOpenCode when Apply fails. Give
	// it the same 2s window as the success-path test to be sure.
	select {
	case <-killed:
		t.Fatal("KillOpenCode called after Apply failed — would restart opencode with stale config")
	case <-time.After(2 * time.Second):
		// Expected: no kill.
	}

	assert.False(t, writer.HasRelay(), "writer must not report relay set when Apply failed")
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

	writer := opencode.NewConfigWriter(agentConfigPath)
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

	assert.True(t, writer.HasRelay(), "writer must have relay after successful retry")
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

	writer := opencode.NewConfigWriter(filepath.Join(dir, "agent-config.json"))
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
	assert.False(t, writer.HasRelay(),
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

	writer := opencode.NewConfigWriter(cfgPath)
	require.True(t, writer.HasRelay(),
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
